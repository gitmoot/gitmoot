package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// settlePartReview stores decision on a dispatched part-review job and moves it
// to state, the way a finished worker does.
func settlePartReview(t *testing.T, store *db.Store, jobID, state, decision string) {
	t.Helper()
	ctx := context.Background()
	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, jobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: "part verdict " + decision, TestsRun: []string{"go test ./..."}, Evidence: "executed"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, jobID, string(workflow.JobQueued), state, string(encoded), db.JobEvent{JobID: jobID, Kind: state, Message: "settled"}); err != nil {
		t.Fatal(err)
	}
}

func partSubjectKey(t *testing.T, head, part string) string {
	t.Helper()
	key, err := db.ReviewRequestSubjectKey("owner/repo", 12, head, db.ReviewRequestPurpose("code", false, part))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// #2405: a settled BLOCKING part verdict at a head could not be re-reviewed at
// that head. A plain request attached to the old verdict; --supersede moves the
// claim to a new job on the SAME reviewer, records the linkage on both jobs,
// and leaves the old job and its verdict untouched.
func TestReviewRequestSupersedeReReviewsASettledBlockingPart(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json", "--role", "joltra", "--part", "mm-1-mm-9"}

	first, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	settlePartReview(t, store, first.JobID, string(workflow.JobSucceeded), "changes_requested")

	// Without --supersede the part stays answered by the old verdict.
	again, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if again.JobID != first.JobID || again.State == reviewRequestDispatched {
		t.Fatalf("plain re-request = %+v, want attachment to %s", again, first.JobID)
	}

	reason := "first prompt was not scoped to this part"
	second, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--supersede", first.JobID, "--reason", reason)...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.State != reviewRequestDispatched || second.JobID == first.JobID || second.Supersedes != first.JobID {
		t.Fatalf("supersede request = %+v, want a new dispatched job superseding %s", second, first.JobID)
	}
	claim, err := store.GetReviewRequest(ctx, partSubjectKey(t, head, "mm-1-mm-9"))
	if err != nil || claim.JobID != second.JobID {
		t.Fatalf("claim after supersede = %+v (%v), want it on %s", claim, err, second.JobID)
	}
	newJob := mustGetJob(t, store, second.JobID)
	oldJob := mustGetJob(t, store, first.JobID)
	if newJob.Agent != oldJob.Agent {
		t.Fatalf("re-review agent %q, want the superseded reviewer %q (the gate retires a part objection only with the same reviewer)", newJob.Agent, oldJob.Agent)
	}
	newPayload, err := workflow.ParseJobPayload(newJob.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if newPayload.ReviewSupersedes != first.JobID || newPayload.ReviewSupersedeReason != reason || newPayload.ReviewPart != "mm-1-mm-9" || newPayload.HeadSHA != head {
		t.Fatalf("re-review payload linkage = supersedes %q reason %q part %q head %q", newPayload.ReviewSupersedes, newPayload.ReviewSupersedeReason, newPayload.ReviewPart, newPayload.HeadSHA)
	}
	oldPayload, err := workflow.ParseJobPayload(oldJob.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if oldJob.State != string(workflow.JobSucceeded) || oldPayload.Result == nil || oldPayload.Result.Decision != "changes_requested" {
		t.Fatalf("superseded job changed: state %s result %+v; its objection must stay until the re-review lands", oldJob.State, oldPayload.Result)
	}
	events, err := store.ListJobEvents(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == reviewPartSupersededEventKind && strings.Contains(event.Message, second.JobID) && strings.Contains(event.Message, reason) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s event naming %s and the reason on the superseded job", reviewPartSupersededEventKind, second.JobID)
	}

	// One winner: a second superseder naming the same old job finds the claim moved.
	var out, errOut strings.Builder
	if code := runReview(append([]string{"request"}, append(append([]string{}, base...), "--supersede", first.JobID, "--reason", "again")...), &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "held by job "+second.JobID) {
		t.Fatalf("competing supersede: exit %d stderr %q, want refusal naming the new holder", code, errOut.String())
	}
}

// #2405: --supersede refuses everything outside its contract and moves no claim.
func TestReviewRequestSupersedeRefusesOutsideItsContract(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "other-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	part := func(name string) []string {
		return []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json", "--role", "joltra", "--part", name, "--reviewer", "opus-reviewer"}
	}
	refused := func(args []string, want string) {
		t.Helper()
		var out, errOut strings.Builder
		code := runReview(append([]string{"request"}, args...), &out, &errOut)
		if code == 0 || !strings.Contains(errOut.String(), want) {
			t.Fatalf("args %v: exit %d stderr %q, want refusal containing %q", args, code, errOut.String(), want)
		}
	}

	live, failure := runReviewRequestJSON(t, part("live")...)
	if failure != "" {
		t.Fatal(failure)
	}
	refused(append(part("live"), "--supersede", live.JobID, "--reason", "r"), "only a settled review can be superseded")

	approved, failure := runReviewRequestJSON(t, part("clean")...)
	if failure != "" {
		t.Fatal(failure)
	}
	settlePartReview(t, store, approved.JobID, string(workflow.JobSucceeded), "approved")
	refused(append(part("clean"), "--supersede", approved.JobID, "--reason", "r"), "nothing to supersede")

	blocked, failure := runReviewRequestJSON(t, part("blocked")...)
	if failure != "" {
		t.Fatal(failure)
	}
	settlePartReview(t, store, blocked.JobID, string(workflow.JobBlocked), "blocked")
	// A different part's job, a different reviewer, and missing flags are all refused.
	refused(append(part("clean"), "--supersede", blocked.JobID, "--reason", "r"), "is held by job "+approved.JobID)
	other := append(part("blocked"), "--supersede", blocked.JobID, "--reason", "r")
	other[len(other)-5] = "other-reviewer" // the value after --reviewer
	refused(other, "differs from opus-reviewer")
	refused(append(part("blocked"), "--supersede", blocked.JobID), "requires --part and --reason")
	refused(append(part("blocked"), "--reason", "r"), "only meaningful with --supersede")

	for name, job := range map[string]string{"live": live.JobID, "clean": approved.JobID, "blocked": blocked.JobID} {
		claim, err := store.GetReviewRequest(ctx, partSubjectKey(t, head, name))
		if err != nil || claim.JobID != job {
			t.Fatalf("claim for part %s = %+v (%v), want it unchanged on %s", name, claim, err, job)
		}
	}
}

// #2405: a re-review that fails before its job exists gives the claim BACK to
// the superseded job, so the part is never left unclaimed and its verdict
// never stops answering.
func TestReviewRequestSupersedeRestoresTheClaimWhenDispatchFails(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json", "--role", "joltra", "--part", "mm-1-mm-9"}
	first, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	settlePartReview(t, store, first.JobID, string(workflow.JobSucceeded), "changes_requested")
	// The reviewer loses its review capability: selection fails after the claim moved.
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	var out, errOut strings.Builder
	if code := runReview(append([]string{"request"}, append(append([]string{}, base...), "--supersede", first.JobID, "--reason", "r")...), &out, &errOut); code == 0 {
		t.Fatalf("supersede with an unusable reviewer succeeded: %q", out.String())
	}
	claim, err := store.GetReviewRequest(ctx, partSubjectKey(t, head, "mm-1-mm-9"))
	if err != nil || claim.JobID != first.JobID {
		t.Fatalf("claim after failed supersede = %+v (%v), want it back on %s", claim, err, first.JobID)
	}
}
