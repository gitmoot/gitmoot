package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func reviewRouterHome(t *testing.T) (string, *db.Store, string) {
	t.Helper()
	home, store, _, _, head := reviewRouterHomeWithHistory(t)
	return home, store, head
}

// reviewRouterHomeWithHistory exposes the fixture's TWO commits and its
// checkout so a delta-review test can request at an ancestor head and then at
// its descendant (#2177). reviewRouterHome keeps returning only the second.
func reviewRouterHomeWithHistory(t *testing.T) (string, *db.Store, string, string, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`
[org]
enforce = "warn"
[org.roles."owner"]
scope = ["*"]
[org.roles."joltra"]
parent = "owner"
scope = ["owner/repo"]
[review_router]
code = ["devin/swe-2", "openai-codex/gpt-5.6-sol"]
`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, home)
	t.Cleanup(func() { store.Close() })
	checkout, firstHead, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	previousPreflight := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		return runtime.RuntimeContractResult{Runtime: runtime.OmpRuntime, Version: "unknown", State: runtime.RuntimeContractUnknown, Instrument: "test"}
	}
	t.Cleanup(func() {
		newAgentDispatchGitHubClient = previousGitHubFactory
		localRuntimeContractPreflight = previousPreflight
	})
	return home, store, checkout, firstHead, head
}

func runReviewRequestJSON(t *testing.T, args ...string) (reviewRequestOutput, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runReview(append([]string{"request"}, args...), &stdout, &stderr); code != 0 {
		return reviewRequestOutput{}, fmt.Sprintf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var output reviewRequestOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	return output, ""
}

// One head, three requesters: exactly one review is dispatched, later callers
// attach, a saved verdict is reused, and a request that ends without a verdict
// does not block the next one.
func TestReviewRequestDeduplicatesOnExactHead(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "builder", runtime.ShellRuntime, "true", []string{"review", "implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--role", "joltra", "--home", home, "--json"}
	first, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if first.State != reviewRequestDispatched || first.Reviewer != "opus-reviewer" || first.Model != "devin/swe-2" {
		t.Fatalf("first request = %+v, want a dispatched review on the review-only agent with the pool head", first)
	}
	if first.AwaitedFactID == 0 {
		t.Fatal("first request did not subscribe the requester to the verdict")
	}
	job, err := store.GetJob(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.RuntimeOverride != runtime.OmpRuntime || payload.Model != "devin/swe-2" ||
		strings.Join(payload.ReviewModelPool, ",") != "devin/swe-2,openai-codex/gpt-5.6-sol" ||
		payload.ReviewRequester != "joltra" || !payload.NoFixTarget || payload.HeadSHA != head {
		t.Fatalf("routed payload = %+v, want omp override, pool, requester, no fix target and exact head", payload)
	}

	second, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.State != reviewRequestAttached || second.JobID != first.JobID {
		t.Fatalf("second request = %+v, want attachment to job %s", second, first.JobID)
	}
	jobs, err := store.ListReviewJobsForPullRequest(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("review jobs after a duplicate request = %d, want 1", len(jobs))
	}

	// A different purpose is a different question and may run in parallel.
	security, failure := runReviewRequestJSON(t, append(base, "--purpose", "security")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if security.State != reviewRequestDispatched || security.JobID == first.JobID {
		t.Fatalf("security request = %+v, want its own dispatched job", security)
	}

	// The first review dies the way the daemon's dead-runtime recovery records
	// it: terminal failed WITH a synthetic `failed` result. That is not a
	// verdict, so the claim must yield to the next requester.
	deadPayload, err := workflow.ParseJobPayload(mustGetJob(t, store, first.JobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	deadPayload.Result = &workflow.AgentResult{Decision: "failed", Summary: "daemon recovery: runtime pid is dead"}
	deadEncoded, err := json.Marshal(deadPayload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, first.JobID, string(workflow.JobQueued), string(workflow.JobFailed), string(deadEncoded), db.JobEvent{JobID: first.JobID, Kind: "failed", Message: "runtime crashed"}); err != nil {
		t.Fatal(err)
	}
	third, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if third.State != reviewRequestDispatched || third.JobID == first.JobID {
		t.Fatalf("request after a resultless failure = %+v, want a fresh dispatch", third)
	}

	// A saved verdict is the terminal answer: attach, report it, spend nothing.
	verdictPayload, err := workflow.ParseJobPayload(mustGetJob(t, store, third.JobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	verdictPayload.Result = &workflow.AgentResult{Decision: "approved", Summary: "clean", TestsRun: []string{"go test ./..."}, Evidence: "executed"}
	encoded, err := json.Marshal(verdictPayload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, third.JobID, string(workflow.JobQueued), string(workflow.JobSucceeded), string(encoded), db.JobEvent{JobID: third.JobID, Kind: "succeeded", Message: "job succeeded"}); err != nil {
		t.Fatal(err)
	}
	fourth, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if fourth.State != reviewRequestVerdictExists || fourth.JobID != third.JobID || fourth.Verdict != "approved" {
		t.Fatalf("request after a saved verdict = %+v, want verdict_exists on job %s", fourth, third.JobID)
	}
	fact, err := store.GetAwaitedFact(ctx, fourth.AwaitedFactID)
	if err != nil {
		t.Fatal(err)
	}
	if fact.State != db.AwaitedFactStateSatisfied || !strings.Contains(fact.ResolutionDetail, "executed_checks=1") || !strings.Contains(fact.ResolutionDetail, "gitmoot job show "+third.JobID) {
		t.Fatalf("requester fact = %+v, want it satisfied by the saved verdict with executed-check evidence", fact)
	}
}

func mustGetJob(t *testing.T, store *db.Store, id string) db.Job {
	t.Helper()
	job, err := store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// The claim, not the jobs row, is the mutual exclusion. Dispatch does seconds of
// work before Mailbox.Enqueue, so a loser that reads "job row absent" must NOT
// conclude the holder is dead and steal the claim: that dispatches a second
// reviewer for one subject, the duplicate the table exists to prevent. Reverting
// resolveLostReviewClaim to a bare GetJob check fails this test.
func TestReviewRequestClaimSurvivesTheDispatchWindow(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	subject := mustSubjectKey(t, head)

	// Requester A has won the claim and is mid-dispatch: its job row does not
	// exist yet, exactly as during worktree allocation and runtime preflight.
	claim, won, err := store.ClaimReviewRequest(ctx, subject, "job-a-dispatching", "code", "joltra", db.ReviewRequestOwner{})
	if err != nil || !won {
		t.Fatalf("seed claim = %+v won=%v err=%v", claim, won, err)
	}
	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--role", "joltra", "--home", home, "--json"}
	loser, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if loser.State != reviewRequestAttached || loser.JobID != "job-a-dispatching" {
		t.Fatalf("request during another requester's dispatch window = %+v, want attachment to job-a-dispatching", loser)
	}
	if loser.JobState != "dispatching" {
		t.Fatalf("job state = %q, want the in-flight dispatch reported honestly", loser.JobState)
	}
	jobs, err := store.ListReviewJobsForPullRequest(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("the losing requester dispatched %d review job(s); the claim holder owns this subject", len(jobs))
	}
	if current, err := store.GetReviewRequest(ctx, subject); err != nil || current.JobID != "job-a-dispatching" {
		t.Fatalf("claim after the losing request = %+v (%v), want it still held by job-a-dispatching", current, err)
	}

	// A claim whose holder never enqueued and has aged past the dispatch window
	// IS dead, and must be taken over rather than stranding the subject forever.
	stale := db.ReviewRequest{JobID: "job-a-dispatching", UpdatedAt: time.Now().UTC().Add(-2 * reviewRequestDispatchWindow).Format(time.RFC3339Nano)}
	if _, takeover, err := resolveLostReviewClaim(ctx, store, stale, time.Now().UTC()); err != nil || !takeover {
		t.Fatalf("aged claim with no job row: takeover=%v err=%v, want takeover", takeover, err)
	}
}

// A code verdict must not terminally satisfy a security requester at the same
// head: the router runs different purposes as separate reviews and the docs
// promise exactly that. Reverting the subscription to the bare verdict key, or
// the producer to a single-key resolve, fails this test.
func TestReviewRequestVerdictDoesNotSatisfyAnotherPurpose(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json"}

	code, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	security, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "owner", "--purpose", "security")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if security.AwaitedFactID == code.AwaitedFactID {
		t.Fatal("both purposes subscribed to the same fact; a code verdict would answer a security request")
	}

	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, code.JobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.Result = &workflow.AgentResult{Decision: "approved", Summary: "code review clean", TestsRun: []string{"go test ./..."}, Evidence: "executed"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, code.JobID, string(workflow.JobQueued), string(workflow.JobSucceeded), string(encoded), db.JobEvent{JobID: code.JobID, Kind: "succeeded", Message: "job succeeded"}); err != nil {
		t.Fatal(err)
	}

	codeFact, err := store.GetAwaitedFact(ctx, code.AwaitedFactID)
	if err != nil {
		t.Fatal(err)
	}
	if codeFact.State != db.AwaitedFactStateSatisfied {
		t.Fatalf("code requester fact = %s, want satisfied by its own verdict", codeFact.State)
	}
	securityFact, err := store.GetAwaitedFact(ctx, security.AwaitedFactID)
	if err != nil {
		t.Fatal(err)
	}
	if securityFact.State != db.AwaitedFactStateWaiting {
		t.Fatalf("security requester fact = %s (%q), want it still waiting for a security verdict", securityFact.State, securityFact.ResolutionDetail)
	}
}

// The parallel-purposes contract must hold where agent substitution CANNOT
// help: one eligible reviewer, and an explicit --reviewer naming the agent that
// already answered. DetectReviewLoop keys on agent and head, so unless it also
// keys on PURPOSE the second request is refused as a loop — the documented
// contract broken by the guard that protects a different property.
func TestReviewRequestSecondPurposeSurvivesTheLoopGuardWithOneReviewer(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "only-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json", "--reviewer", "only-reviewer"}

	code, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, code.JobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.Result = &workflow.AgentResult{Decision: "approved", Summary: "code clean", TestsRun: []string{"go test ./..."}, Evidence: "executed"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, code.JobID, string(workflow.JobQueued), string(workflow.JobSucceeded), string(encoded), db.JobEvent{JobID: code.JobID, Kind: "succeeded", Message: "job succeeded"}); err != nil {
		t.Fatal(err)
	}

	security, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "owner", "--purpose", "security")...)
	if failure != "" {
		t.Fatalf("a security request after a code verdict by the SAME agent was refused: %s", failure)
	}
	if security.State != reviewRequestDispatched || security.JobID == code.JobID {
		t.Fatalf("security request = %+v, want its own dispatched review", security)
	}

	// The guard must still bite for a REPEAT of the same purpose by the same
	// agent at the same head; purpose-awareness must not disable it.
	var stdout, stderr bytes.Buffer
	if exit := runReview(append([]string{"request"}, append(append([]string{}, base...), "--role", "joltra")...), &stdout, &stderr); exit == 0 {
		if out := stdout.String(); !strings.Contains(out, reviewRequestVerdictExists) {
			t.Fatalf("a repeated CODE request = %q, want the existing verdict rather than a fresh review", out)
		}
	}
}

// releaseUnenqueuedReviewClaim frees a claim ONLY when its job was never
// enqueued. Two ways to get that wrong, both tested here because both hand the
// subject to a second reviewer:
//
//  1. dispatch fails AFTER Mailbox.Enqueue commits — the job is live, so
//     releasing would leave it running with no claim;
//  2. the claim has since moved to another requester's job — the DELETE is
//     CAS'd on job_id, so it must match nothing rather than delete a live claim.
func TestReviewClaimReleaseOnlyFreesAnUnenqueuedJob(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	subject := mustSubjectKey(t, head)

	if _, won, err := store.ClaimReviewRequest(ctx, subject, "job-enqueued", "code", "joltra", db.ReviewRequestOwner{}); err != nil || !won {
		t.Fatalf("seed claim: won=%v err=%v", won, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-enqueued", Agent: "reviewer", Action: "review", Repo: "owner/repo",
		Branch: "feature/review", PullRequest: 12, HeadSHA: head, NoFixTarget: true,
	})
	releaseUnenqueuedReviewClaim(ctx, store, subject, "job-enqueued")
	if current, err := store.GetReviewRequest(ctx, subject); err != nil || current.JobID != "job-enqueued" {
		t.Fatalf("claim after a post-enqueue failure = %+v (%v), want it held: the job is live", current, err)
	}

	// The interleaving that actually reaches the DELETE: this requester never
	// enqueued (so the job-exists guard passes), and by the time its dispatch
	// error unwinds the claim has already moved to another requester's job. The
	// DELETE is CAS'd on job_id so it must match nothing; without that it frees
	// a live claim and the subject is dispatched twice.
	if _, won, err := store.ClaimReviewRequest(ctx, subject+"|second", "job-never-enqueued", "code", "joltra", db.ReviewRequestOwner{}); err != nil || !won {
		t.Fatalf("seed second claim: won=%v err=%v", won, err)
	}
	moved, err := store.ReplaceReviewRequestJob(ctx, subject+"|second", "job-never-enqueued", "job-successor", "owner", db.ReviewRequestOwner{})
	if err != nil || !moved {
		t.Fatalf("move claim: moved=%v err=%v", moved, err)
	}
	releaseUnenqueuedReviewClaim(ctx, store, subject+"|second", "job-never-enqueued")
	current, err := store.GetReviewRequest(ctx, subject+"|second")
	if err != nil || current.JobID != "job-successor" {
		t.Fatalf("claim after a stale release = %+v (%v), want it still held by job-successor", current, err)
	}
}

// A STALLED dispatch is not a dead one. The window bound used to decide
// takeover on elapsed time alone, so a requester hung on a cold PR-ref fetch or
// stopped by a signal had its claim stolen and the subject was dispatched
// twice — the original P1 with a clock in front of it. Takeover now asks
// whether the holding PROCESS is alive.
func TestReviewClaimTakeoverAsksLivenessNotAge(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	subject := mustSubjectKey(t, head)
	ancient := time.Now().UTC().Add(-6 * reviewRequestDispatchWindow)

	// This process is alive and holds the claim; its dispatch has simply taken
	// longer than the window. Its claim must survive.
	live := reviewRequestOwner()
	if _, won, err := store.ClaimReviewRequest(ctx, subject, "job-stalled", "code", "joltra", live); err != nil || !won {
		t.Fatalf("seed live claim: won=%v err=%v", won, err)
	}
	stalled := db.ReviewRequest{JobID: "job-stalled", OwnerPID: live.PID, OwnerPIDStartTime: live.PIDStartTime, OwnerBootID: live.BootID, UpdatedAt: ancient.Format(time.RFC3339Nano)}
	if _, takeover, err := resolveLostReviewClaim(ctx, store, stalled, time.Now().UTC()); err != nil || takeover {
		t.Fatalf("stalled-but-live holder: takeover=%v err=%v, want the claim held", takeover, err)
	}

	// A holder whose process is provably gone releases immediately, without
	// waiting out any window: pid 0 with a recorded identity cannot be alive.
	dead := db.ReviewRequest{JobID: "job-dead", OwnerPID: 2147483646, OwnerPIDStartTime: "1", OwnerBootID: live.BootID, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if _, takeover, err := resolveLostReviewClaim(ctx, store, dead, time.Now().UTC()); err != nil || !takeover {
		t.Fatalf("dead holder: takeover=%v err=%v, want takeover without waiting for the window", takeover, err)
	}

	// A claim recorded on a different boot cannot still be dispatching.
	rebooted := db.ReviewRequest{JobID: "job-prior-boot", OwnerPID: live.PID, OwnerPIDStartTime: live.PIDStartTime, OwnerBootID: "0000-boot-that-is-not-this-one", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if _, takeover, err := resolveLostReviewClaim(ctx, store, rebooted, time.Now().UTC()); err != nil || !takeover {
		t.Fatalf("claim from a prior boot: takeover=%v err=%v, want takeover", takeover, err)
	}
}

// The producer-side purpose fix left the SUBSCRIBE door open: the canonical
// recheck matched head only, so a purposed waiter subscribing AFTER a
// different-purpose verdict was satisfied by it on insert.
func TestReviewSubscribeRecheckRespectsPurpose(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json"}

	code, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, code.JobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.Result = &workflow.AgentResult{Decision: "approved", Summary: "code clean", TestsRun: []string{"go test ./..."}, Evidence: "executed"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, code.JobID, string(workflow.JobQueued), string(workflow.JobSucceeded), string(encoded), db.JobEvent{JobID: code.JobID, Kind: "succeeded", Message: "job succeeded"}); err != nil {
		t.Fatal(err)
	}

	// The security request subscribes AFTER the code verdict already exists.
	security, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "owner", "--purpose", "security")...)
	if failure != "" {
		t.Fatal(failure)
	}
	fact, err := store.GetAwaitedFact(ctx, security.AwaitedFactID)
	if err != nil {
		t.Fatal(err)
	}
	if fact.State != db.AwaitedFactStateWaiting {
		t.Fatalf("security wait = %s (%q), want waiting: a code verdict is not a security answer", fact.State, fact.ResolutionDetail)
	}
	if security.State == reviewRequestVerdictExists {
		t.Fatalf("security request = %+v, want its own review rather than the code verdict", security)
	}
}

// A failed or cancelled job can carry a stored verdict. Honouring it pinned the
// claim and reported verdict_exists while the awaited fact — satisfied only
// from a SUCCEEDED transition — left the requester waiting out its TTL.
func TestReviewVerdictRequiresASucceededJob(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	subject := mustSubjectKey(t, head)
	if _, won, err := store.ClaimReviewRequest(ctx, subject, "job-failed-with-verdict", "code", "joltra", reviewRequestOwner()); err != nil || !won {
		t.Fatalf("seed claim: won=%v err=%v", won, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-failed-with-verdict", Agent: "reviewer", Action: "review", Repo: "owner/repo",
		Branch: "feature/review", PullRequest: 12, HeadSHA: head, NoFixTarget: true,
	})
	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, "job-failed-with-verdict").Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.Result = &workflow.AgentResult{Decision: "approved", Summary: "verdict stored on a job that then failed"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, "job-failed-with-verdict", string(workflow.JobQueued), string(workflow.JobFailed), string(encoded), db.JobEvent{JobID: "job-failed-with-verdict", Kind: "failed", Message: "failed after storing a result"}); err != nil {
		t.Fatal(err)
	}
	job := mustGetJob(t, store, "job-failed-with-verdict")
	stored, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if decision := reviewVerdictDecision(job, stored); decision != "" {
		t.Fatalf("verdict from a failed job = %q, want none: the awaited fact would never be satisfied", decision)
	}
	if reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a failed job with a stored verdict still pins the claim; a later requester can never dispatch")
	}
}

func TestReviewRequestReleasesClaimWhenReviewerRefused(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	var stdout, stderr bytes.Buffer
	code := runReview([]string{"request", "--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--role", "joltra", "--home", home, "--reviewer", "nobody"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), `reviewer "nobody"`) {
		t.Fatalf("exit=%d stderr=%q, want a refusal naming the unknown reviewer", code, stderr.String())
	}
	if _, err := store.GetReviewRequest(context.Background(), mustSubjectKey(t, head)); err == nil {
		t.Fatal("a refused request left its claim behind")
	}
}

func mustSubjectKey(t *testing.T, head string) string {
	t.Helper()
	key, err := db.ReviewRequestSubjectKey("owner/repo", 12, head, "code")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A provider quota failure on the first pool model re-queues the review on the
// next model immediately; the same failure with the pool exhausted takes the
// ordinary timed hold. Both run through the daemon's real delivery path.
func TestReviewModelPoolFallsBackBeforeTimedHold(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	state := t.TempDir()
	countFile := filepath.Join(state, "count")
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	// First delivery: the measured omp provider-stall rendering (no HTTP status).
	// Second delivery: a provider 429. Both are operational, so the first moves
	// the job to the next pool entry and the second, with the pool exhausted,
	// takes the ordinary timed hold.
	script := fmt.Sprintf(`printf x >> %q
if [ "$(wc -c < %q)" = "1" ]; then
  echo "omp turn failed (stopReason error): Provider stream stalled while waiting for the next event" 1>&2
  exit 1
fi
echo "HTTP 429 Too Many Requests: rate limit reached; try again in 3 seconds" 1>&2
exit 1`, countFile, countFile)
	seedDaemonWorkerAgent(t, store, "router-reviewer", runtime.ShellRuntime, script, []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-pool", Agent: "router-reviewer", Action: "review", Repo: "owner/repo",
		Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40), NoFixTarget: true,
		Model: "devin/swe-2", ReviewModelPool: []string{"devin/swe-2", "openai-codex/gpt-5.6-sol"}, ReviewPurpose: "code",
	})
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-pool")
	if job.State != string(workflow.JobQueued) || payload.Model != "openai-codex/gpt-5.6-sol" {
		events, _ := store.ListJobEvents(ctx, "job-pool")
		t.Fatalf("after first quota failure state=%s model=%q pool=%v events=%+v, want queued on the second pool model", job.State, payload.Model, payload.ReviewModelPool, events)
	}
	if !blockerE2EHasEventKind(t, store, "job-pool", reviewModelFallbackEventKind) {
		t.Fatalf("missing %s event", reviewModelFallbackEventKind)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil || retryAt.After(time.Now().UTC()) {
		t.Fatalf("fallback retry_at = %q (%v), want an immediate re-queue", payload.BlockerRetryAt, err)
	}
	pending, err := listPendingQueuedJobs(ctx, worker, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "job-pool" {
		t.Fatalf("pending after fallback = %+v, want the re-queued review dispatchable now", pending)
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if data, err := os.ReadFile(countFile); err != nil || strings.Count(string(data), "x") != 2 {
		t.Fatalf("deliveries = %q (%v), want two", data, err)
	}
	job, payload = blockerE2EJobPayload(t, store, "job-pool")
	if job.State != string(workflow.JobQueued) || payload.Model != "openai-codex/gpt-5.6-sol" || payload.BlockerAttempts != 2 {
		t.Fatalf("after exhausted pool state=%s model=%q attempts=%d, want the last model held for retry", job.State, payload.Model, payload.BlockerAttempts)
	}
	retryAt, err = time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil || !retryAt.After(time.Now().UTC()) {
		t.Fatalf("exhausted-pool retry_at = %q (%v), want the provider's timed hold", payload.BlockerRetryAt, err)
	}
}

// stagedPreflightClaim seeds the shape #2172 round 2 was built for and round 3
// found untested: a routed review job whose own result is a FAN-OUT
// announcement (not a verdict), plus one child of the given type and state.
// Judged on its own row the parent looks finished-without-a-verdict, so the
// claim it holds looks stealable while the tree is still working.
func stagedPreflightClaim(t *testing.T, store *db.Store, head string, childType string, childState string, childDecision string) db.Job {
	t.Helper()
	ctx := context.Background()
	parentID := "local-review-staged-parent"
	announcement := workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 12, HeadSHA: head, ReviewPurpose: "code",
		Result: &workflow.AgentResult{Decision: "approved", FanOut: true},
	}
	parentPayload, err := json.Marshal(announcement)
	if err != nil {
		t.Fatal(err)
	}
	parent := db.Job{ID: parentID, Agent: "opus-reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: string(parentPayload), Repo: "owner/repo"}
	if err := store.CreateJob(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := workflow.JobPayload{Repo: "owner/repo", PullRequest: 12, HeadSHA: head}
	if childDecision != "" {
		child.Result = &workflow.AgentResult{Decision: childDecision, Evidence: "executed"}
	}
	childPayload, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: parentID + "/delegation/leg", Agent: "opus-reviewer", Type: childType,
		State: childState, Payload: string(childPayload), Repo: "owner/repo", ParentJobID: parentID,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetJob(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// THE ROUND-2 STAGED-REVIEW FIX, both halves, which round 3 caught me claiming
// as mutation-proven when it had no coverage at all. Deleting the tree walk and
// deleting the review-scoped inheritance both passed the entire suite.
//
// Half one: a preflight whose verdict child is still RUNNING must keep its
// claim. Without the walk the parent's fan-out row reads as finished, the claim
// becomes takeover-eligible, and a second reviewer is dispatched at the same
// head while the first is still producing the answer.
func TestStagedPreflightKeepsItsClaimWhileTheVerdictChildRuns(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	job := stagedPreflightClaim(t, store, head, "review", string(workflow.JobRunning), "")
	if !reviewJobStillAnswers(context.Background(), store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a staged preflight with a running review child does not answer, so its claim is stealable; the fan-out tree walk is gone")
	}
}

// Half two: the verdict child must be reachable as a REVIEW, which is what the
// scoped inheritance preserves through the delegation. A finished review child
// carrying the decision answers the question its parent only announced.
func TestStagedPreflightIsAnsweredByItsVerdictChild(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	job := stagedPreflightClaim(t, store, head, "review", string(workflow.JobSucceeded), "approved")
	if !reviewJobStillAnswers(context.Background(), store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a staged preflight whose review child saved a verdict does not answer; the tree is not being consulted")
	}
}

// ROUND-3 P2: the walk was type-blind. A staged preflight may also delegate a
// NON-review leg (implement, ask) whose result legitimately carries
// decision="approved". Counting it pinned the claim forever - no takeover, ever
// - while the awaited fact, which only a review verdict can satisfy, stayed
// unsatisfiable. A non-review child is not evidence in either direction.
func TestStagedPreflightNonReviewChildCannotPinTheClaim(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	job := stagedPreflightClaim(t, store, head, "implement", string(workflow.JobRunning), "approved")
	if reviewJobStillAnswers(context.Background(), store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a running implement child is answering a REVIEW question: the claim is pinned forever behind a leg that can never satisfy the verdict wait")
	}
}

// ROUND-3 P3, and #1685's rule that no consumer may render an announcement as a
// verdict. `review status` printed payload.Result.Decision verbatim, so a
// staged preflight - whose fan-out announcement carries decision="approved" -
// was shown to the requester as verdict=approved while the verdict child was
// still running. The row must show the job's state, not a decision it never made.
func TestReviewStatusDoesNotRenderAFanOutAnnouncementAsAVerdict(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	stagedPreflightClaim(t, store, head, "review", string(workflow.JobRunning), "")

	var stdout, stderr bytes.Buffer
	if code := runReview([]string{"status", "--repo", "owner/repo", "--pr", "12", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("review status exit=%d stderr=%q", code, stderr.String())
	}
	var output reviewStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	for _, entry := range output.Requests {
		if entry.JobID == "local-review-staged-parent" && entry.Verdict != "" {
			t.Fatalf("status rendered the fan-out announcement as verdict=%q: the requester reads an approval the reviewer never gave", entry.Verdict)
		}
	}
}

// ROUND-4 P2, and the reason my round-3 test did not catch it: that test passed
// --reviewer, which BYPASSES selection entirely, so the fix was proven on a path
// that avoids the broken one. This one goes through selectReviewRouterAgent with
// ONE eligible review-only agent and no --reviewer, which is the exact shape the
// docs promise and the shape the reviewer reproduced live.
func TestReviewRequestSecondPurposeSurvivesSelectionWithOneReviewer(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "only-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	requireSingleEligibleReviewer(t, store, "only-reviewer")
	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json"}

	code, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, code.JobID)

	security, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "owner", "--purpose", "security")...)
	if failure != "" {
		t.Fatalf("a security request was refused although only a CODE verdict exists at this head, and selection is the path that refused it: %s", failure)
	}
	if security.JobID == code.JobID {
		t.Fatalf("security request reused the code job %s: a different question must get its own review", code.JobID)
	}
	if security.State == reviewRequestVerdictExists {
		t.Fatalf("security request was answered by the code verdict: state=%s", security.State)
	}
}

// The must-refuse control for the same path: a REPEAT of the same purpose by the
// only eligible reviewer is still a loop, and selection must say so rather than
// dispatching a duplicate.
func TestReviewRequestRepeatedPurposeIsStillRefusedBySelection(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "only-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	requireSingleEligibleReviewer(t, store, "only-reviewer")
	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json"}

	first, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	repeat, failure := runReviewRequestJSON(t, append(append([]string{}, base...), "--role", "owner")...)
	if failure == "" && repeat.State != reviewRequestVerdictExists {
		t.Fatalf("a repeated CODE request dispatched a duplicate instead of reusing or refusing: state=%s job=%s", repeat.State, repeat.JobID)
	}
}

// ROUND-4 P3: the walk is acyclic by construction, but a corrupted
// parent_job_id cycle exhausted the stack rather than failing closed.
func TestStagedPreflightWalkSurvivesACorruptedParentCycle(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "review", string(workflow.JobSucceeded), "")
	// The child must ALSO announce a fan-out, or the walk stops at it and never
	// follows the corrupted edge back to the parent.
	makeJobAFanOutAnnouncement(t, store, job.ID+"/delegation/leg")
	forgeParentCycle(t, store, job.ID, job.ID+"/delegation/leg")
	done := make(chan bool, 1)
	go func() { done <- reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the claim walk did not terminate on a corrupted parent cycle")
	}
}

// saveReviewRouterVerdict marks a dispatched router job succeeded with a real
// verdict, preserving the payload's recorded purpose.
func saveReviewRouterVerdict(t *testing.T, store *db.Store, jobID string) {
	t.Helper()
	saveReviewRouterVerdictWith(t, store, jobID, "approved", nil)
}

// saveReviewRouterVerdictWith saves a terminal verdict with named findings, the
// shape a delta review inherits as its carried-forward brief (#2177).
func saveReviewRouterVerdictWith(t *testing.T, store *db.Store, jobID, decision string, findings []string) {
	t.Helper()
	ctx := context.Background()
	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, jobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	// Findings are OBJECTS, which is what real reviewers emit and what
	// db.SucceededReviewVerdicts can decode: its payload struct types findings
	// as []struct{Severity string}, so a verdict carrying bare JSON strings
	// fails to decode and vanishes from verdict history entirely.
	raw := make([]json.RawMessage, 0, len(findings))
	for _, finding := range findings {
		encoded, err := json.Marshal(map[string]string{"severity": "P2", "title": finding})
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, encoded)
	}
	// Severity is REQUIRED for a review that requests changes; without it the
	// result is not a terminal verdict and never reaches the baseline history.
	severity := ""
	if decision == "changes_requested" {
		severity = "P2"
	}
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: "clean", TestsRun: []string{"go test ./..."}, Evidence: "executed", Findings: raw, Severity: severity}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, jobID, string(workflow.JobQueued), string(workflow.JobSucceeded), string(encoded), db.JobEvent{JobID: jobID, Kind: "succeeded", Message: "job succeeded"}); err != nil {
		t.Fatal(err)
	}
}

// forgeParentCycle writes the corruption the walk must survive: parent_job_id
// is insert-only in production, so this reaches past the store to make the
// parent a child of its own child.
func forgeParentCycle(t *testing.T, store *db.Store, parentID, childID string) {
	t.Helper()
	if err := store.ExecForTest(context.Background(), "UPDATE jobs SET parent_job_id = ? WHERE id = ?", childID, parentID); err != nil {
		t.Fatalf("forge cycle: %v", err)
	}
}

// requireSingleEligibleReviewer removes every OTHER review-only candidate, which
// is the premise these tests depend on: with two eligible reviewers the router
// substitutes one and a purpose-blind refusal never fires, so the test passes
// while the defect is live. Round 4 caught exactly that shape.
func requireSingleEligibleReviewer(t *testing.T, store *db.Store, keep string) {
	t.Helper()
	ctx := context.Background()
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eligible := 0
	for _, agent := range agents {
		if !agentHasCapability(agent.Capabilities, "review") || agentHasCapability(agent.Capabilities, "implement") {
			continue
		}
		if agent.Name == keep {
			eligible++
			continue
		}
		if err := store.ExecForTest(ctx, "DELETE FROM agents WHERE name = ?", agent.Name); err != nil {
			t.Fatalf("drop competing reviewer %s: %v", agent.Name, err)
		}
	}
	if eligible != 1 {
		t.Fatalf("kept reviewer %q is not eligible; the single-candidate premise does not hold", keep)
	}
}

// makeJobAFanOutAnnouncement rewrites a job's result into a coordinator
// announcement, the shape whose children the claim walk follows.
func makeJobAFanOutAnnouncement(t *testing.T, store *db.Store, jobID string) {
	t.Helper()
	ctx := context.Background()
	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, jobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.Result = &workflow.AgentResult{Decision: "approved", FanOut: true}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", string(encoded), jobID); err != nil {
		t.Fatal(err)
	}
}

// THE THIRD GUARD IN THIS CHANGE FOUND WITHOUT COVERAGE, caught by my own
// inventory rather than by a reviewer. A succeeded review row that recorded NO
// head cannot satisfy an exact-head wait, so the head-keyed resolver passes over
// it. A requester never told about that skip waits its full TTL believing a
// review is coming (#2130 makes headless rows common). Deleting the surfacing
// loop passed all 53 review tests.
func TestReviewRequestSurfacesAHeadlessReviewSkipAsAHold(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "only-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	// A succeeded review for this PR that recorded no head at all.
	payload, err := json.Marshal(map[string]any{
		"repo": "owner/repo", "pull_request": 12, "head_sha": "",
		"review_purpose": "code",
		"result":         map[string]any{"decision": "approved", "evidence": "executed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "headless-review", Agent: "only-reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: string(payload), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}

	output, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--role", "joltra", "--home", home, "--json")
	if failure != "" {
		t.Fatal(failure)
	}
	for _, hold := range output.Holds {
		if strings.Contains(hold, "headless-review") && strings.Contains(hold, "recorded no head") {
			return
		}
	}
	t.Fatalf("holds = %v, want the headless review named: the requester waits its whole TTL for a review that can never satisfy this exact-head wait", output.Holds)
}

// #2176, and the third time on this PR that a fix created its own defect: the
// type filter that stopped a non-review child ANSWERING also stopped the walk
// DESCENDING, so a review grandchild under a non-review leg was invisible and
// its claim was stealable while it worked.
func TestStagedPreflightSeesAReviewGrandchildUnderANonReviewChild(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "implement", string(workflow.JobSucceeded), "approved")
	middle := job.ID + "/delegation/leg"
	makeJobAFanOutAnnouncement(t, store, middle)
	payload, err := json.Marshal(map[string]any{"repo": "owner/repo", "pull_request": 12, "head_sha": head})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: middle + "/delegation/verdict", Agent: "opus-reviewer", Type: "review",
		State: string(workflow.JobRunning), Payload: string(payload), Repo: "owner/repo", ParentJobID: middle,
	}); err != nil {
		t.Fatal(err)
	}
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a RUNNING review grandchild under a non-review leg is invisible: the claim is stealable while the real reviewer works")
	}
}

// #2176: dispatch writes the parent's fan-out announcement before the children
// rows exist. An empty child list means too early to tell, not finished - and
// reading it as finished let takeover dispatch a duplicate reviewer.
func TestStagedPreflightWithNoChildrenYetIsNotFinished(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	announcement, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 12, HeadSHA: head, ReviewPurpose: "code",
		Result: &workflow.AgentResult{Decision: "approved", FanOut: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "childless-preflight", Agent: "opus-reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: string(announcement), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, "childless-preflight")
	if err != nil {
		t.Fatal(err)
	}
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a fan-out whose children are not enqueued yet reads as finished, so a duplicate reviewer is dispatched into live review capacity")
	}
}

// testClaimSubject is the question the staged-preflight fixtures are minted
// for: owner/repo#12 at this head, default purpose.
func testClaimSubject(t *testing.T, head string) reviewClaimSubject {
	t.Helper()
	key, err := db.ReviewRequestSubjectKey("owner/repo", 12, head, db.DefaultReviewPurpose)
	if err != nil {
		t.Fatal(err)
	}
	subject, ok := reviewSubjectFromClaim(db.ReviewRequest{SubjectKey: key, Purpose: db.DefaultReviewPurpose})
	if !ok {
		t.Fatalf("subject from claim key %q failed to parse", key)
	}
	return subject
}

// #2176 F2 (P2). The walk was subject-blind: a non-review leg may legitimately
// delegate a review of a DIFFERENT pull request or head, and that verdict
// counted as answering THIS claim. A stored verdict never stops answering, so
// the claim pinned forever while its awaited fact - keyed to this exact head
// and purpose - could never be satisfied by it.
func TestClaimWalkIgnoresAForeignSubjectVerdictUnderANonReviewLeg(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "implement", string(workflow.JobSucceeded), "approved")
	middle := job.ID + "/delegation/leg"
	makeJobAFanOutAnnouncement(t, store, middle)
	foreign, err := json.Marshal(map[string]any{
		"repo": "owner/repo", "pull_request": 99, "head_sha": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"review_purpose": "code",
		"result":         map[string]any{"decision": "approved", "evidence": "executed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: middle + "/delegation/foreign", Agent: "opus-reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(foreign), Repo: "owner/repo", ParentJobID: middle,
	}); err != nil {
		t.Fatal(err)
	}
	if reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a verdict for a DIFFERENT pull request and head is answering this claim: it pins forever while the awaited fact can never be satisfied")
	}
}

// #2176 F1 (P2). The not-yet-enqueued grace had no release. A refused staged
// preflight is rewritten to FAILED with its fan-out result kept verbatim, and
// no child is ever enqueued - so the grace pinned the claim permanently. A
// terminal-but-not-succeeded fan-out gets no grace at all.
func TestChildlessFanOutOnAFailedJobGetsNoGrace(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	announcement, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 12, HeadSHA: head, ReviewPurpose: "code",
		Result: &workflow.AgentResult{Decision: "approved", FanOut: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "refused-preflight", Agent: "opus-reviewer", Type: "review", State: string(workflow.JobFailed), Payload: string(announcement), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, "refused-preflight")
	if err != nil {
		t.Fatal(err)
	}
	if reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a FAILED fan-out whose children can never arrive is holding the claim: nothing releases it, so every future review of this head is blocked")
	}
}

// And the same grace must expire even on a succeeded job, or a permanently
// refused advance pins the claim just as hard, one state over.
func TestChildlessFanOutGraceExpiresWithTheDispatchWindow(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	announcement, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 12, HeadSHA: head, ReviewPurpose: "code",
		Result: &workflow.AgentResult{Decision: "approved", FanOut: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "stalled-preflight", Agent: "opus-reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: string(announcement), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, "stalled-preflight")
	if err != nil {
		t.Fatal(err)
	}
	subject := testClaimSubject(t, head)
	if !reviewJobStillAnswers(ctx, store, job, subject, time.Now().UTC()) {
		t.Fatal("a fresh childless fan-out lost its grace: a duplicate reviewer is dispatched while children are seconds away")
	}
	late := time.Now().UTC().Add(2 * reviewRequestDispatchWindow)
	if reviewJobStillAnswers(ctx, store, job, subject, late) {
		t.Fatal("the grace never expires: a fan-out whose children never arrive holds the claim forever")
	}
}

// #2176 F3 (P3). The not-yet-enqueued grace existed at depth 1 but not deeper,
// so a nested fan-out under a non-review leg lost its review grandchild in the
// window before dispatch inserts it - the exact defect F1 closed, one level down.
func TestNestedFanOutUnderANonReviewLegKeepsTheNotYetEnqueuedGrace(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "implement", string(workflow.JobSucceeded), "approved")
	middle := job.ID + "/delegation/leg"
	makeJobAFanOutAnnouncement(t, store, middle)
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a nested fan-out with no children yet reads as finished: takeover dispatches a duplicate while the review grandchild is seconds away")
	}
}

// #2176 f6 (P2). My subject check pruned a foreign REVIEW leg's whole subtree
// before ever listing its children - applying "answering and traversing are
// different questions" to non-review legs while denying it to review legs. A
// foreign review job (another PR, another head) can fan out a child that
// reviews exactly THIS head; pruning released the claim and dispatched a
// duplicate while that child worked.
func TestClaimWalkSeesAMatchingReviewUnderAForeignReviewLeg(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "review", string(workflow.JobSucceeded), "")
	foreignLeg := job.ID + "/delegation/leg"
	foreign, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 99, HeadSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		ReviewPurpose: "code", Result: &workflow.AgentResult{Decision: "approved", FanOut: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET payload = ?, type = 'review' WHERE id = ?", string(foreign), foreignLeg); err != nil {
		t.Fatal(err)
	}
	matching, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", PullRequest: 12, HeadSHA: head, ReviewPurpose: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: foreignLeg + "/delegation/matching", Agent: "opus-reviewer", Type: "review",
		State: string(workflow.JobRunning), Payload: string(matching), Repo: "owner/repo", ParentJobID: foreignLeg,
	}); err != nil {
		t.Fatal(err)
	}
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a RUNNING review of this exact head is invisible because its parent reviews another PR: the claim releases and a duplicate reviewer is dispatched")
	}
}

// #2176 f7 (P3), first half. The unknown-subject direction was the entire point
// of the previous commit and shipped with no failing test: an unparseable claim
// key must let NOTHING but the holder keep the claim, because "matches
// everything" is the permanent wedge the subject check exists to remove.
func TestUnparseableClaimSubjectAnswersNothing(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "review", string(workflow.JobSucceeded), "approved")
	unknown, ok := reviewSubjectFromClaim(db.ReviewRequest{SubjectKey: "not-a-subject-key", Purpose: "code"})
	if ok {
		t.Fatal("a malformed key parsed; this test no longer exercises the unknown-subject direction")
	}
	if reviewJobStillAnswers(ctx, store, job, unknown, time.Now().UTC()) {
		t.Fatal("an unknown subject is matching every descendant verdict: the claim pins forever exactly as it did before the subject check")
	}
}

// #2176 f7, second half. The holder exemption is what keeps a claim with a
// drifted or unreadable payload from releasing under its own owner's feet.
func TestClaimHolderKeepsItsOwnClaimDespiteAForeignPayload(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	foreign, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 99, HeadSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", ReviewPurpose: "code",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "drifted-holder", Agent: "opus-reviewer", Type: "review", State: string(workflow.JobRunning), Payload: string(foreign), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, "drifted-holder")
	if err != nil {
		t.Fatal(err)
	}
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("the claim holder lost its own live claim because its payload names another subject: a running reviewer is displaced by a duplicate")
	}
}

// #2176 round 3 P2. Traversal was gated on ResultIsFanOut, which requires a
// terminal REVIEW decision - but dispatchDelegations runs for any decision that
// is not blocked or failed, so an implement leg that succeeded with decision
// "implemented" has real children the walk never listed, and a live matching
// review grandchild under it was invisible.
func TestClaimWalkSeesChildrenOfALegWhoseDecisionIsNotAReviewVerdict(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "implement", string(workflow.JobSucceeded), "")
	leg := job.ID + "/delegation/leg"
	implemented, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 12, HeadSHA: head,
		Result: &workflow.AgentResult{Decision: "implemented", Delegations: []workflow.Delegation{{ID: "review", Agent: "opus-reviewer", Action: "review"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", string(implemented), leg); err != nil {
		t.Fatal(err)
	}
	matching, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", PullRequest: 12, HeadSHA: head, ReviewPurpose: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: leg + "/delegation/review", Agent: "opus-reviewer", Type: "review",
		State: string(workflow.JobRunning), Payload: string(matching), Repo: "owner/repo", ParentJobID: leg,
	}); err != nil {
		t.Fatal(err)
	}
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a RUNNING review of this head is invisible because its parent's decision was not a review verdict: the claim releases and a duplicate dispatches")
	}
}

// #2176 round 3 P3. The childless grace is subject-blind by design - unborn
// children have no subject - but a node that DECLARES a foreign subject is a
// different matter: a continuously active foreign subtree renewed the hold
// indefinitely, so the bound was foreign activity rather than this claim's clock.
func TestChildlessFanOutOnAForeignSubjectGetsNoGrace(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "review", string(workflow.JobSucceeded), "")
	leg := job.ID + "/delegation/leg"
	foreign, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 99, HeadSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		ReviewPurpose: "code", Result: &workflow.AgentResult{Decision: "approved", FanOut: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET payload = ?, state = 'succeeded' WHERE id = ?", string(foreign), leg); err != nil {
		t.Fatal(err)
	}
	if reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a freshly-active FOREIGN fan-out is holding this claim on children it has not created: an unrelated orchestra run renews the hold indefinitely")
	}
}

// #2176 round 4 P2, first arm. The grace still asked a VERDICT classifier
// whether children were coming: a leg that succeeded with decision
// "implemented" and announced a review delegation was indistinguishable from a
// leaf while that child was unborn, so the claim released and a duplicate
// dispatched.
func TestGraceCoversAnnouncedChildrenOnANonVerdictDecision(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "implement", string(workflow.JobSucceeded), "")
	leg := job.ID + "/delegation/leg"
	announced, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 12, HeadSHA: head,
		Result: &workflow.AgentResult{Decision: "implemented", Delegations: []workflow.Delegation{{ID: "review", Agent: "opus-reviewer", Action: "review"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", string(announced), leg); err != nil {
		t.Fatal(err)
	}
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("an announced-but-unborn review child on a non-verdict decision reads as a leaf: the claim releases and a duplicate dispatches")
	}
}

// Second arm: a DEPS-DEFERRED delegation creates no row until its deps succeed,
// so a sibling that already landed must not cancel the grace. The old
// len(children)==0 precondition gave that shape no grace at all.
func TestGraceSurvivesASiblingWhenADeferredChildIsStillUnborn(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "implement", string(workflow.JobSucceeded), "")
	leg := job.ID + "/delegation/leg"
	announced, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 12, HeadSHA: head,
		Result: &workflow.AgentResult{Decision: "implemented", Delegations: []workflow.Delegation{
			{ID: "build", Agent: "builder", Action: "implement"},
			{ID: "review", Agent: "opus-reviewer", Action: "review", Deps: []string{"build"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", string(announced), leg); err != nil {
		t.Fatal(err)
	}
	built, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", PullRequest: 12, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: leg + "/delegation/build", Agent: "builder", Type: "implement",
		State: string(workflow.JobSucceeded), Payload: string(built), Repo: "owner/repo", ParentJobID: leg,
	}); err != nil {
		t.Fatal(err)
	}
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a landed sibling cancelled the grace while the deferred review child is still unborn: the claim releases before the review can start")
	}
}

// #2176 round 4 P3. The reviewer proved both polarities in subjectIsForeign
// were unpinned: flipping unset-is-not-foreign, and removing the unknown-subject
// early return, each passed the whole suite. Unreachable in production per its
// enumeration - but only deleting a guard tells you which way it points.
func TestSubjectIsForeignTreatsSilenceAsNotForeign(t *testing.T) {
	subject := testClaimSubject(t, "15ebf4cf5cc70f90a6521d3e9d4960721665c2b7")
	if subjectIsForeign(subject, workflow.JobPayload{}) {
		t.Fatal("a payload that declares no subject reads as foreign: coordinator legs that name nothing would lose their grace and prune the traversal")
	}
	if subjectIsForeign(subject, workflow.JobPayload{Repo: "owner/repo"}) {
		t.Fatal("a partial payload naming only the matching repo reads as foreign")
	}
	if !subjectIsForeign(subject, workflow.JobPayload{Repo: "owner/repo", PullRequest: 99}) {
		t.Fatal("a payload naming another pull request is not foreign: an unrelated subtree can renew this claim indefinitely")
	}
	// An UNKNOWN claim subject cannot judge anything foreign - the walk's
	// unknown-subject direction is handled by answers(), not here, and having
	// both return "foreign" would deny grace to every node under a corrupt key.
	unknown, ok := reviewSubjectFromClaim(db.ReviewRequest{SubjectKey: "not-a-key", Purpose: "code"})
	if ok {
		t.Fatal("a malformed key parsed; this no longer exercises the unknown-subject early return")
	}
	if subjectIsForeign(unknown, workflow.JobPayload{Repo: "owner/repo", PullRequest: 99}) {
		t.Fatal("an unknown subject is judging other nodes foreign")
	}
}

// #2176 round 5, and I caught this one on my own fixture while measuring cost
// rather than from a review. The tempting optimisation - a node with no stored
// result never dispatched delegations, so skip listing its children - prunes
// subtrees that DO have children: the staged fixture's own leg carries a nil
// result and real child rows. Whether a node has children is a question about
// the store, and every cheaper proxy for it has been wrong.
func TestClaimWalkListsChildrenOfANodeWithNoStoredResult(t *testing.T) {
	_, store, head := reviewRouterHome(t)
	ctx := context.Background()
	job := stagedPreflightClaim(t, store, head, "implement", string(workflow.JobSucceeded), "")
	leg := job.ID + "/delegation/leg"
	if err := store.ExecForTest(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", `{"repo":"owner/repo","pull_request":12,"head_sha":"`+head+`"}`, leg); err != nil {
		t.Fatal(err)
	}
	matching, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", PullRequest: 12, HeadSHA: head, ReviewPurpose: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: leg + "/delegation/review", Agent: "opus-reviewer", Type: "review",
		State: string(workflow.JobRunning), Payload: string(matching), Repo: "owner/repo", ParentJobID: leg,
	}); err != nil {
		t.Fatal(err)
	}
	if !reviewJobStillAnswers(ctx, store, job, testClaimSubject(t, head), time.Now().UTC()) {
		t.Fatal("a RUNNING review is invisible because its parent stored no result: the claim releases and a duplicate dispatches")
	}
}

// deltaReviewFixture seeds a single eligible reviewer and returns the fixture's
// ancestor head, its descendant, and the checkout backing both.
func deltaReviewFixture(t *testing.T) (home string, store *db.Store, checkout, firstHead, secondHead string) {
	t.Helper()
	home, store, checkout, firstHead, secondHead = reviewRouterHomeWithHistory(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "only-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	requireSingleEligibleReviewer(t, store, "only-reviewer")
	return home, store, checkout, firstHead, secondHead
}

func deltaReviewBase(home, head string) []string {
	return []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--home", home, "--json"}
}

func dispatchedReviewPayload(t *testing.T, store *db.Store, jobID string) workflow.JobPayload {
	t.Helper()
	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, jobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// #2177. A prior terminal verdict at an ancestor head bounds the next review to
// that range and carries its findings, instead of paying to re-read the whole
// pull request. Measured motivation: the same reviewer cost 1,192,188 input
// tokens on the full diff and 342,583 on the delta.
func TestReviewRequestDeltaFromPriorVerdictAtAncestorHead(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdictWith(t, store, first.JobID, "changes_requested", []string{"F1: guard has no test"})

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.State != reviewRequestDispatched {
		t.Fatalf("state = %s, want a dispatched review", second.State)
	}
	if second.Baseline != firstHead {
		t.Fatalf("baseline = %q, want the prior verdict's head %q: the review re-reads the whole PR", second.Baseline, firstHead)
	}
	payload := dispatchedReviewPayload(t, store, second.JobID)
	if payload.ReviewScope == nil || payload.ReviewScope.PreviousHeadSHA != firstHead {
		t.Fatalf("payload scope = %+v, want it bounded to %s", payload.ReviewScope, firstHead)
	}
	if got := payload.ReviewScope.ChangedFiles; len(got) != 1 || got[0] != "review.txt" {
		t.Fatalf("changed files = %v, want [review.txt]", got)
	}
	if got := payload.ReviewScope.Findings; len(got) != 1 || !strings.Contains(got[0], "F1: guard has no test") {
		t.Fatalf("carried findings = %v, want the baseline's finding: the reviewer is not told what it must answer", got)
	}
	if !strings.Contains(payload.Instructions, "Diff from that prior head, never from the PR base") {
		t.Fatalf("instructions do not bound the diff: %q", payload.Instructions)
	}
	if !strings.Contains(payload.Instructions, "F1: guard has no test") {
		t.Fatalf("instructions omit the carried finding: %q", payload.Instructions)
	}
	if strings.Contains(payload.Instructions, "Read the full diff against its base") {
		t.Fatalf("instructions still order a full re-read: %q", payload.Instructions)
	}
}

// An APPROVED ancestor is a baseline too, not only changes_requested: every
// round after the first should benefit, which is the whole saving.
func TestReviewRequestDeltaUsesApprovedBaselineToo(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != firstHead {
		t.Fatalf("baseline = %q, want %q: an approved ancestor is a valid baseline", second.Baseline, firstHead)
	}
	payload := dispatchedReviewPayload(t, store, second.JobID)
	if !strings.Contains(payload.Instructions, "Named findings still in scope:\n- none") {
		t.Fatalf("instructions do not state the empty finding list: %q", payload.Instructions)
	}
}

func TestReviewRequestFullWhenNoPriorVerdict(t *testing.T) {
	home, store, _, _, secondHead := deltaReviewFixture(t)

	only, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if only.Baseline != "" {
		t.Fatalf("baseline = %q, want none: nothing has been reviewed yet", only.Baseline)
	}
	if only.BaselineSkipped != "no prior verdict for this purpose" {
		t.Fatalf("skip reason = %q, want the no-prior-verdict reason", only.BaselineSkipped)
	}
	payload := dispatchedReviewPayload(t, store, only.JobID)
	if payload.ReviewScope != nil {
		t.Fatalf("scope = %+v, want nil for a first review", payload.ReviewScope)
	}
	if !strings.Contains(payload.Instructions, "Read the full diff against its base") {
		t.Fatalf("first review is not told to read the full diff: %q", payload.Instructions)
	}
}

// A rebase, force-push or squash orphans the baseline. Falling back to a full
// review costs one re-read; refusing would block the commonest shape there is.
func TestReviewRequestFullWhenBaselineIsNotAnAncestor(t *testing.T) {
	home, store, checkout, firstHead, _ := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	runGit(t, checkout, "switch", "main")
	writeFile(t, filepath.Join(checkout, "unrelated.txt"), "off the branch\n")
	runGit(t, checkout, "add", "unrelated.txt")
	runGit(t, checkout, "commit", "-m", "unrelated main commit")
	orphanHead := readonlyWorktreeHead(t, checkout)

	second, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", orphanHead,
		"--branch", "main", "--home", home, "--json", "--role", "owner")
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != "" {
		t.Fatalf("baseline = %q, want none: the prior head is not an ancestor of this one", second.Baseline)
	}
	if !strings.HasPrefix(second.BaselineSkipped, "not a direct follow-up: ") {
		t.Fatalf("skip reason = %q, want the not-a-follow-up reason", second.BaselineSkipped)
	}
	payload := dispatchedReviewPayload(t, store, second.JobID)
	if !strings.Contains(payload.Instructions, "Read the full diff against its base") {
		t.Fatalf("a rebased head is not reviewed in full: %q", payload.Instructions)
	}
}

func TestReviewRequestFullFlagForcesFullReview(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdictWith(t, store, first.JobID, "changes_requested", []string{"F1: guard has no test"})

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner", "--full")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.BaselineSkipped != "flag --full" {
		t.Fatalf("skip reason = %q, want the operator flag: --full is the escape hatch from a silent fallback", second.BaselineSkipped)
	}
	if payload := dispatchedReviewPayload(t, store, second.JobID); payload.ReviewScope != nil {
		t.Fatalf("scope = %+v, want nil under --full", payload.ReviewScope)
	}
}

// A code verdict does not bound a security review: the purpose is part of the
// question, so it is part of the baseline.
func TestReviewRequestDeltaIgnoresOtherPurposeVerdict(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner", "--purpose", "security")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.BaselineSkipped != "no prior verdict for this purpose" {
		t.Fatalf("skip reason = %q, want no-prior-verdict: a code verdict must not bound a security review", second.BaselineSkipped)
	}
}

func TestReviewStatusShowsBaseline(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)
	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}

	var stdout, stderr bytes.Buffer
	if code := runReview([]string{"status", "--repo", "owner/repo", "--pr", "12", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("review status exit=%d stderr=%q", code, stderr.String())
	}
	var output reviewStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	seen := map[string]string{}
	for _, entry := range output.Requests {
		seen[entry.JobID] = entry.Baseline
	}
	if got := seen[second.JobID]; got != firstHead {
		t.Fatalf("delta job baseline = %q, want %q: a reader cannot tell a bounded review from a full one", got, firstHead)
	}
	if got := seen[first.JobID]; got != "" {
		t.Fatalf("first job baseline = %q, want empty", got)
	}
}

// Mutant (a) from the round-1 battery: `continue` -> `break` on the
// not-a-follow-up arm. Needs TWO candidates where the NEWER is a non-ancestor
// and the OLDER is an ancestor, which no earlier test exercised.
func TestDeltaSkipsANonAncestorCandidateAndUsesTheOlderAncestor(t *testing.T) {
	home, store, checkout, firstHead, secondHead := deltaReviewFixture(t)
	ctx := context.Background()

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	// A NEWER terminal verdict at a head on another branch: not an ancestor of
	// secondHead, so the loop must step over it to the older ancestor above.
	runGit(t, checkout, "switch", "main")
	writeFile(t, filepath.Join(checkout, "orphan.txt"), "off the branch\n")
	runGit(t, checkout, "add", "orphan.txt")
	runGit(t, checkout, "commit", "-m", "orphan commit")
	orphanHead := readonlyWorktreeHead(t, checkout)
	runGit(t, checkout, "switch", "feature/review")

	payload, err := json.Marshal(map[string]any{
		"repo": "owner/repo", "pull_request": 12, "head_sha": orphanHead, "review_purpose": "code",
		"result": map[string]any{"decision": "approved", "evidence": "executed", "findings": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "newer-orphan-verdict", Agent: "only-reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET pull_request = 12, updated_at = '2099-01-01 00:00:00' WHERE id = 'newer-orphan-verdict'"); err != nil {
		t.Fatal(err)
	}

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != firstHead {
		t.Fatalf("baseline = %q, want the older ANCESTOR %q: a newer non-ancestor candidate must be stepped over, not treated as the end of the search", second.Baseline, firstHead)
	}
}

// Mutant (c): a verdict predating the router records no purpose and must
// default to code, or a legacy verdict stops bounding a code request.
func TestDeltaTreatsALegacyVerdictWithNoPurposeAsCode(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)
	ctx := context.Background()

	payload, err := json.Marshal(map[string]any{
		"repo": "owner/repo", "pull_request": 12, "head_sha": firstHead,
		"result": map[string]any{"decision": "approved", "evidence": "executed", "findings": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "legacy-verdict", Agent: "only-reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET pull_request = 12 WHERE id = 'legacy-verdict'"); err != nil {
		t.Fatal(err)
	}

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != firstHead {
		t.Fatalf("baseline = %q, want %q: a pre-router verdict carries no purpose and must read as the default", second.Baseline, firstHead)
	}
}

// Mutant (b): the `baseline == head` skip. The CLI path attaches before
// dispatch when a verdict exists at the requested head, so this is only
// reachable by calling the resolver directly - which is exactly why it had no
// test. Without it a request would be bounded against ITSELF: an empty range,
// and an instruction telling the reviewer to approve without reading anything.
func TestResolveDeltaScopeNeverBoundsAHeadAgainstItself(t *testing.T) {
	home, store, checkout, head, descendant := deltaReviewFixture(t)
	ctx := context.Background()

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, head), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	// THE CONTROL MUST DIFFER FROM THE ASSERTION, or it controls nothing. My
	// first version called the resolver twice with identical arguments and
	// passed even with a nil client, because the same-head guard short-circuits
	// before any git call (#2178 round 2). The control therefore resolves at a
	// DESCENDANT, where a scope must be built - proving the resolver, the store
	// and the git client all work before the real assertion claims a nil.
	git := gitutil.NewHostClient(checkout)
	control, controlReason := resolveDeltaReviewScope(ctx, store, git, "owner/repo", 12, descendant, "code")
	if control == nil {
		t.Fatalf("control: no scope built at a descendant head (%s), so a nil below would prove nothing", controlReason)
	}
	if control.PreviousHeadSHA != head {
		t.Fatalf("control: baseline = %q, want the verdict head %q", control.PreviousHeadSHA, head)
	}
	scope, reason := resolveDeltaReviewScope(ctx, store, git, "owner/repo", 12, head, "code")
	if scope != nil {
		t.Fatalf("scope = %+v, want nil: a head bounded against itself yields an empty range and an instruction to approve without reading", scope)
	}
	if reason == "" {
		t.Fatal("no skip reason recorded for a self-baseline")
	}
}

// #2178 round 1 P3. A carried finding may legitimately cite a commit outside
// this pull request ("regression introduced in <sha>"). The delta brief embeds
// finding text verbatim, so the citation classifier scans it and refuses the
// dispatch - naming an escape flag that `review request` did not register.
// The refusal is correct; naming an escape that does not exist is not.
func TestCarriedFindingCitingAForeignCommitHasAWorkingEscape(t *testing.T) {
	home, store, checkout, firstHead, secondHead := deltaReviewFixture(t)

	runGit(t, checkout, "switch", "main")
	writeFile(t, filepath.Join(checkout, "elsewhere.txt"), "another history\n")
	runGit(t, checkout, "add", "elsewhere.txt")
	runGit(t, checkout, "commit", "-m", "foreign commit")
	foreignSHA := readonlyWorktreeHead(t, checkout)
	runGit(t, checkout, "switch", "feature/review")

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdictWith(t, store, first.JobID, "changes_requested",
		[]string{"F1: regression introduced in " + foreignSHA})

	// WITHOUT the flag the guard must still refuse: the escape is escapable,
	// not off. This is the arm that fails if the flag is ever forced on.
	var stdout, stderr bytes.Buffer
	if exit := runReview(append([]string{"request"}, append(deltaReviewBase(home, secondHead), "--role", "owner")...), &stdout, &stderr); exit == 0 {
		t.Fatalf("a foreign citation dispatched WITHOUT the escape flag: the head-binding guard is off, not escapable (stdout %q)", stdout.String())
	} else if !strings.Contains(stderr.String(), "allow-prompt-head-mismatch") {
		t.Fatalf("refusal does not name the escape: %q", stderr.String())
	}

	escaped, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner", "--allow-prompt-head-mismatch")...)
	if failure != "" {
		t.Fatalf("the escape flag named by the refusal does not work: %s", failure)
	}
	if escaped.Baseline != firstHead {
		t.Fatalf("baseline = %q, want %q", escaped.Baseline, firstHead)
	}
	payload := dispatchedReviewPayload(t, store, escaped.JobID)
	if !strings.Contains(payload.Instructions, foreignSHA) {
		t.Fatalf("carried finding lost its citation: %q", payload.Instructions)
	}
}

// #2178 round 2 r2-f1. Refusing on ANY undecodable row in the PR over-refuses:
// a row orphaned by a rebase could never have been selected as this baseline,
// so it cannot have been silently skipped. The delta must survive it.
func TestUndecodableVerdictOffThisHistoryDoesNotBlockTheDelta(t *testing.T) {
	home, store, checkout, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	runGit(t, checkout, "switch", "main")
	writeFile(t, filepath.Join(checkout, "orphan2.txt"), "rebase orphan\n")
	runGit(t, checkout, "add", "orphan2.txt")
	runGit(t, checkout, "commit", "-m", "orphaned by rebase")
	orphanHead := readonlyWorktreeHead(t, checkout)
	runGit(t, checkout, "switch", "feature/review")

	seedUndecodableVerdict(t, store, "orphan-undecodable", orphanHead)

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != firstHead {
		t.Fatalf("baseline = %q, want %q: an undecodable verdict off this line of history could never have been selected, so it must not disable the delta (skip reason %q)", second.Baseline, firstHead, second.BaselineSkipped)
	}
}

// The other half: an undecodable verdict sitting BETWEEN the baseline and the
// dispatch head is exactly the one that would have been selected, so the brief
// would claim a head the reviewer never saw. That must still refuse.
func TestUndecodableVerdictBetweenBaselineAndHeadBlocksTheDelta(t *testing.T) {
	home, store, checkout, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	// A third commit ON THE REVIEW BRANCH - the fixture leaves the checkout on
	// main, so committing without switching lands off this line of history.
	runGit(t, checkout, "switch", "feature/review")
	writeFile(t, filepath.Join(checkout, "review.txt"), "round three\n")
	runGit(t, checkout, "add", "review.txt")
	runGit(t, checkout, "commit", "-m", "review round three")
	thirdHead := readonlyWorktreeHead(t, checkout)
	seedUndecodableVerdict(t, store, "middle-undecodable", secondHead)

	third, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", thirdHead,
		"--branch", "feature/review", "--home", home, "--json", "--role", "owner")
	if failure != "" {
		t.Fatal(failure)
	}
	if third.Baseline != "" {
		t.Fatalf("baseline = %q, want none: an invisible verdict sits between it and this head, so the brief would assert a head the reviewer never saw", third.Baseline)
	}
	if !strings.Contains(third.BaselineSkipped, "would have been selected") {
		t.Fatalf("skip reason = %q, want it to name the verdict selection would have chosen", third.BaselineSkipped)
	}
}

// r2-f2 mutant M4: `continue` -> `break` on the same-head arm. A same-head
// verdict must be STEPPED OVER, not treated as the end of the search, or an
// older ancestor that could have bounded the review is missed.
func TestDeltaStepsOverASameHeadVerdictToAnOlderAncestor(t *testing.T) {
	home, store, checkout, firstHead, secondHead := deltaReviewFixture(t)
	ctx := context.Background()

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	// A NEWER verdict at the dispatch head itself. Reached directly, the
	// resolver must skip it and keep walking to firstHead.
	payload, err := json.Marshal(map[string]any{
		"repo": "owner/repo", "pull_request": 12, "head_sha": secondHead, "review_purpose": "code",
		"result": map[string]any{"decision": "approved", "evidence": "executed", "findings": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "same-head-verdict", Agent: "only-reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET pull_request = 12, updated_at = '2099-01-01 00:00:00' WHERE id = 'same-head-verdict'"); err != nil {
		t.Fatal(err)
	}

	scope, reason := resolveDeltaReviewScope(ctx, store, gitutil.NewHostClient(checkout), "owner/repo", 12, secondHead, "code")
	if scope == nil || scope.PreviousHeadSHA != firstHead {
		t.Fatalf("scope = %+v (%s), want the older ancestor %s: a same-head verdict must be stepped over, not end the search", scope, reason, firstHead)
	}
}

// r2-f2 mutant M5: `continue` -> `break` on the purpose arm. A NEWER
// wrong-purpose verdict must not suppress a delta an older right-purpose
// verdict would have bounded.
func TestDeltaStepsOverAWrongPurposeVerdictToAnOlderMatch(t *testing.T) {
	home, store, checkout, firstHead, secondHead := deltaReviewFixture(t)
	ctx := context.Background()

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	payload, err := json.Marshal(map[string]any{
		"repo": "owner/repo", "pull_request": 12, "head_sha": firstHead, "review_purpose": "security",
		"result": map[string]any{"decision": "approved", "evidence": "executed", "findings": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "newer-security-verdict", Agent: "only-reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET pull_request = 12, updated_at = '2099-01-01 00:00:00' WHERE id = 'newer-security-verdict'"); err != nil {
		t.Fatal(err)
	}

	scope, reason := resolveDeltaReviewScope(ctx, store, gitutil.NewHostClient(checkout), "owner/repo", 12, secondHead, "code")
	if scope == nil || scope.PreviousHeadSHA != firstHead {
		t.Fatalf("scope = %+v (%s), want %s: a newer wrong-purpose verdict must be stepped over, not end the search", scope, reason, firstHead)
	}
}

// seedUndecodableVerdict writes a succeeded review whose findings are a shape
// verdict history cannot decode. Bare STRINGS stopped qualifying when #2179
// widened the decoder; a number still does.
func seedUndecodableVerdict(t *testing.T, store *db.Store, id, head string) {
	t.Helper()
	seedUndecodableVerdictInState(t, store, id, head, string(workflow.JobSucceeded))
}

// seedUndecodableVerdictInState creates the row already IN the state under
// test. A raw UPDATE of jobs.state bypasses the lifecycle-generation seam
// (#1407) and is refused by its guard test, correctly.
func seedUndecodableVerdictInState(t *testing.T, store *db.Store, id, head, state string) {
	t.Helper()
	ctx := context.Background()
	payload, err := json.Marshal(map[string]any{
		"repo": "owner/repo", "pull_request": 12, "head_sha": head, "review_purpose": "code",
		// A NUMBER finding is the shape that stays undecodable after #2179 widened
		// the decoder to accept strings as well as objects.
		"result": map[string]any{"decision": "changes_requested", "severity": "P2", "evidence": "executed",
			"findings": []any{42}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: id, Agent: "only-reviewer", Type: "review",
		State: state, Payload: string(payload), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET pull_request = 12 WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
}

// M8: UndecodableReviewVerdicts must report only SUCCEEDED reviews. A failed
// job's payload is not verdict history, and treating one as an invisible
// verdict would disable delta review on every PR that ever had a failed review.
func TestUndecodableCountIgnoresNonSucceededReviews(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)
	ctx := context.Background()

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	seedUndecodableVerdictInState(t, store, "failed-undecodable", secondHead, string(workflow.JobFailed))

	heads, err := store.UndecodableReviewVerdicts(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != 0 {
		t.Fatalf("undecodable heads = %v, want none: a FAILED review is not verdict history", heads)
	}
	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != firstHead {
		t.Fatalf("baseline = %q, want %q: a failed review disabled the delta (%s)", second.Baseline, firstHead, second.BaselineSkipped)
	}
}

// seedUndecodableVerdictFull writes an undecodable succeeded review with an
// explicit purpose, head and updated_at, so a test can place it exactly where
// selection would have looked.
func seedUndecodableVerdictFull(t *testing.T, store *db.Store, id, head, purpose, updatedAt string) {
	t.Helper()
	ctx := context.Background()
	body := map[string]any{
		"repo": "owner/repo", "pull_request": 12, "review_purpose": purpose,
		"result": map[string]any{"decision": "changes_requested", "severity": "P2", "evidence": "executed",
			"findings": []any{42}},
	}
	if head != "" {
		body["head_sha"] = head
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: id, Agent: "only-reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecForTest(ctx, "UPDATE jobs SET pull_request = 12, updated_at = ? WHERE id = ?", updatedAt, id); err != nil {
		t.Fatal(err)
	}
}

// #2178 round 3 R3-F1. "The reviewer last saw head X" is a RECENCY claim.
// Selection orders by updated_at, so a NEWER invisible verdict at a head that
// is an ancestor OF the baseline would have sorted first - and a purely
// positional check sees nothing "between" the baseline and the head.
func TestDeltaRefusesWhenANewerInvisibleVerdictWouldHaveBeenSelected(t *testing.T) {
	home, store, checkout, firstHead, secondHead := deltaReviewFixture(t)

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, second.JobID)

	// Invisible verdict at the OLDER commit but with the NEWEST timestamp.
	seedUndecodableVerdictFull(t, store, "newer-invisible", firstHead, "code", "2099-01-01 00:00:00")

	runGit(t, checkout, "switch", "feature/review")
	writeFile(t, filepath.Join(checkout, "review.txt"), "round three\n")
	runGit(t, checkout, "add", "review.txt")
	runGit(t, checkout, "commit", "-m", "review round three")
	thirdHead := readonlyWorktreeHead(t, checkout)

	third, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", thirdHead,
		"--branch", "feature/review", "--home", home, "--json", "--role", "owner")
	if failure != "" {
		t.Fatal(failure)
	}
	if third.Baseline != "" {
		t.Fatalf("baseline = %q, want none: a NEWER invisible verdict would have been selected first, so the brief would name the wrong head", third.Baseline)
	}
	if !strings.Contains(third.BaselineSkipped, "would have been selected") {
		t.Fatalf("skip reason = %q, want it to name the newer invisible verdict", third.BaselineSkipped)
	}
}

// R3-F2. Mailbox.OpenExternalJob writes review payloads with NO head_sha. Such
// a row can never be a baseline - selection skips an empty head - so one
// undecodable session verdict must not disable delta review for the whole PR.
func TestHeadlessUndecodableVerdictDoesNotBlockTheDelta(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)
	seedUndecodableVerdictFull(t, store, "headless-session-verdict", "", "code", "2099-01-01 00:00:00")

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != firstHead {
		t.Fatalf("baseline = %q, want %q: a head-less session verdict is unselectable and must not disable delta review (%s)", second.Baseline, firstHead, second.BaselineSkipped)
	}
}

// A payload that is not valid JSON at all is the one shape whose position
// cannot be established, and it must still refuse unconditionally (mutant M4).
func TestUnreadableVerdictPayloadBlocksTheDelta(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)
	ctx := context.Background()

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	if err := store.CreateJob(ctx, db.Job{ID: "unreadable-verdict", Agent: "only-reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(`{"repo":"owner/repo",`), Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	// An unreadable payload projects no repo and no pull request, so the
	// columns must be set explicitly; in production such a row is therefore
	// never even associated with a pull request. This arm is defensive.
	if err := store.ExecForTest(ctx, "UPDATE jobs SET pull_request = 12, repo = 'owner/repo' WHERE id = 'unreadable-verdict'"); err != nil {
		t.Fatal(err)
	}

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != "" {
		t.Fatalf("baseline = %q, want none: a payload that is not valid JSON cannot be placed", second.Baseline)
	}
	if !strings.Contains(second.BaselineSkipped, "unreadable") {
		t.Fatalf("skip reason = %q, want it to name the unreadable row", second.BaselineSkipped)
	}
}

// R3-F3, residual over-refusal: an invisible row for ANOTHER purpose could
// never have bounded this request, and an OLDER one had already been passed
// over by selection. Neither may refuse. Also kills mutant M3 (the
// baseline/head skip) via the same-head row.
func TestUnselectableInvisibleVerdictsDoNotBlockTheDelta(t *testing.T) {
	home, store, _, firstHead, secondHead := deltaReviewFixture(t)

	first, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, firstHead), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, first.JobID)

	seedUndecodableVerdictFull(t, store, "wrong-purpose-invisible", secondHead, "security", "2099-01-01 00:00:00")
	seedUndecodableVerdictFull(t, store, "older-invisible", secondHead, "code", "2000-01-01 00:00:00")
	seedUndecodableVerdictFull(t, store, "same-head-invisible", secondHead, "code", "2099-01-01 00:00:01")
	seedUndecodableVerdictFull(t, store, "at-baseline-invisible", firstHead, "code", "2099-01-01 00:00:02")

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, secondHead), "--role", "owner")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.Baseline != firstHead {
		t.Fatalf("baseline = %q, want %q: wrong-purpose, older, at-baseline and at-head invisible rows are all unselectable and must not refuse (%s)", second.Baseline, firstHead, second.BaselineSkipped)
	}
}

// threeCommitDeltaFixture gives h1 < h2 < h3 on feature/review with a decodable
// baseline verdict at h2, so an invisible row can be placed at h1 - an ancestor
// of the dispatch head that is NEITHER the baseline nor the head, which is the
// only position where the recency and purpose arms decide anything.
func threeCommitDeltaFixture(t *testing.T) (home string, store *db.Store, checkout, h1, h2, h3 string) {
	t.Helper()
	home, store, checkout, h1, h2 = deltaReviewFixture(t)
	runGit(t, checkout, "switch", "feature/review")
	writeFile(t, filepath.Join(checkout, "review.txt"), "round three\n")
	runGit(t, checkout, "add", "review.txt")
	runGit(t, checkout, "commit", "-m", "review round three")
	h3 = readonlyWorktreeHead(t, checkout)

	second, failure := runReviewRequestJSON(t, append(deltaReviewBase(home, h2), "--role", "joltra")...)
	if failure != "" {
		t.Fatal(failure)
	}
	saveReviewRouterVerdict(t, store, second.JobID)
	return home, store, checkout, h1, h2, h3
}

func requestAtHead(t *testing.T, home, head string) reviewRequestOutput {
	t.Helper()
	out, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--home", home, "--json", "--role", "owner")
	if failure != "" {
		t.Fatal(failure)
	}
	return out
}

// Recency arm in isolation: an invisible row at h1 is an ancestor of h3 and is
// neither the baseline (h2) nor the head, but it is OLDER than the baseline, so
// selection had already passed it. It must not refuse.
func TestOlderInterposedInvisibleVerdictDoesNotBlockTheDelta(t *testing.T) {
	home, store, _, h1, h2, h3 := threeCommitDeltaFixture(t)
	seedUndecodableVerdictFull(t, store, "older-interposed", h1, "code", "2000-01-01 00:00:00")

	third := requestAtHead(t, home, h3)
	if third.Baseline != h2 {
		t.Fatalf("baseline = %q, want %q: an invisible row OLDER than the baseline was already passed over by selection and must not refuse (%s)", third.Baseline, h2, third.BaselineSkipped)
	}
}

// Purpose arm in isolation: same position, NEWER than the baseline, but for
// another purpose - a different question that could never have bounded this one.
func TestWrongPurposeInterposedInvisibleVerdictDoesNotBlockTheDelta(t *testing.T) {
	home, store, _, h1, h2, h3 := threeCommitDeltaFixture(t)
	seedUndecodableVerdictFull(t, store, "security-interposed", h1, "security", "2099-01-01 00:00:00")

	third := requestAtHead(t, home, h3)
	if third.Baseline != h2 {
		t.Fatalf("baseline = %q, want %q: an invisible SECURITY verdict cannot bound a code review and must not refuse (%s)", third.Baseline, h2, third.BaselineSkipped)
	}
}

// Off-history arm must CONTINUE, not end the scan: an unselectable row listed
// BEFORE a genuinely selectable one must not hide it.
func TestOffHistoryInvisibleVerdictDoesNotHideALaterBlockingOne(t *testing.T) {
	home, store, checkout, h1, _, h3 := threeCommitDeltaFixture(t)

	runGit(t, checkout, "switch", "main")
	writeFile(t, filepath.Join(checkout, "orphan3.txt"), "off history\n")
	runGit(t, checkout, "add", "orphan3.txt")
	runGit(t, checkout, "commit", "-m", "off-history commit")
	orphanHead := readonlyWorktreeHead(t, checkout)
	runGit(t, checkout, "switch", "feature/review")

	// Inserted FIRST so the scan meets the unselectable row before the one that
	// must refuse; both are newer than the baseline.
	seedUndecodableVerdictFull(t, store, "a-off-history", orphanHead, "code", "2099-01-01 00:00:00")
	seedUndecodableVerdictFull(t, store, "b-interposed", h1, "code", "2099-01-01 00:00:01")

	third := requestAtHead(t, home, h3)
	if third.Baseline != "" {
		t.Fatalf("baseline = %q, want none: an off-history row listed first hid a newer interposed verdict that would have been selected", third.Baseline)
	}
	if !strings.Contains(third.BaselineSkipped, "would have been selected") {
		t.Fatalf("skip reason = %q, want the interposed verdict named", third.BaselineSkipped)
	}
}

// #2180. The defect this pins: `gitmoot review request` armed the review model
// pool and the omp runtime at its own construction site, and `gitmoot agent
// review` armed neither, so every agent-review job carried an EMPTY pool - a
// state indistinguishable from a pool with no alternatives, which made the
// provider fallback structurally unreachable for those dispatches.
//
// The assertions are on the PERSISTED PAYLOAD, because that is the artifact the
// daemon worker and the blocker actually read. Asserting the request struct
// would pass while the field was dropped between dispatch and insert.
// #2180 observable: a BACKGROUND job deliberately leaves EffectiveRuntime empty
// on the payload (the daemon records it when execution starts), so the runtime a
// dispatch actually selected - the value the unavailability gate is tested
// against at dispatch time - is read from the runtime event the dispatcher
// writes with the override already applied.
func dispatchedRuntime(t *testing.T, store *db.Store, jobID string, agentDefault string) string {
	t.Helper()
	if override := strings.TrimSpace(dispatchedReviewPayload(t, store, jobID).RuntimeOverride); override != "" {
		return override
	}
	return agentDefault
}

func TestBothReviewVerbsResolveTheSamePoolAndKeepTheirRuntimes(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	// Path A: the router. Path B: agent review, which arms nothing - and whose
	// registered runtime is deliberately NOT omp, so an unresolved runtime is
	// visible rather than coincidentally correct.
	routed, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home, "--json")
	if failure != "" {
		t.Fatal(failure)
	}
	routedPayload := dispatchedReviewPayload(t, store, routed.JobID)

	direct, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "opus-reviewer", Action: "review",
		Instructions: "review this", Background: true, Home: home,
		PullRequest: 12, HeadSHA: head, Branch: "feature/review",
		ActingOrgRole: "joltra", NoFixTarget: true,
	})
	if err != nil {
		t.Fatalf("agent review dispatch: %v", err)
	}
	directPayload := dispatchedReviewPayload(t, store, direct.JobID)

	if len(directPayload.ReviewModelPool) == 0 {
		t.Fatal("agent review dispatched with an EMPTY pool: the provider fallback is unreachable for this job (#2180)")
	}
	if !slices.Equal(directPayload.ReviewModelPool, routedPayload.ReviewModelPool) {
		t.Fatalf("pools diverge for the same purpose: agent review=%v review request=%v",
			directPayload.ReviewModelPool, routedPayload.ReviewModelPool)
	}
	if want := []string{"devin/swe-2", "openai-codex/gpt-5.6-sol"}; !slices.Equal(directPayload.ReviewModelPool, want) {
		t.Fatalf("resolved pool = %v, want the configured code pool %v", directPayload.ReviewModelPool, want)
	}
	// THE POOL IS ATTACHED, THE MODEL IS NOT CHOSEN (#2186 review, F1). An
	// earlier version of this fix also set Model to the pool head on every
	// review, which handed an OMP-qualified model to a registered claude
	// reviewer - the model half of exactly the damage the runtime reversal
	// above exists to prevent. The pool is what the fallback needs; the model
	// belongs to the agent the operator named, until a blocker moves the job to
	// omp and the fallback picks the first pool entry deliberately.
	if directPayload.Model != "" {
		t.Fatalf("agent review model = %q, want the agent's own: resolving a pool must not choose a model for a named reviewer", directPayload.Model)
	}
	// RUNTIME IS THE DELIBERATE EXCEPTION, pinned here so a later reading of
	// #2180's "both paths make the same decisions" cannot quietly extend to it.
	// The router selects its own reviewer and pins omp; `agent review` is handed
	// a registered reviewer whose runtime is part of its identity, alongside its
	// auth profile and session. Forcing them to agree overrides the operator's
	// chosen agent - the first version of this fix did exactly that and broke
	// three existing tests. What must be equal is the POOL; what must exist for
	// runtime is an escape, covered by TestExplicitRuntimeOverridesThePinOnBothVerbs.
	if got := dispatchedRuntime(t, store, routed.JobID, runtime.ShellRuntime); got != runtime.OmpRuntime {
		t.Fatalf("review request runtime = %q, want the router pin %q", got, runtime.OmpRuntime)
	}
	if got := dispatchedRuntime(t, store, direct.JobID, runtime.ShellRuntime); got != runtime.ShellRuntime {
		t.Fatalf("agent review runtime = %q, want the reviewer's registered %q - the pin overrode a named agent", got, runtime.ShellRuntime)
	}
}

// The invariant the pool defect's own fix must not break, and the reason the
// resolver is gated on the action rather than applied to every request.
func TestNonReviewDispatchAcquiresNoReviewPool(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "builder", runtime.ShellRuntime, "true", []string{"review", "implement", "ask"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "builder", Action: "ask",
		Instructions: "what is the state of this repo", Background: true, Home: home,
		PullRequest: 12, HeadSHA: head, Branch: "feature/review", ActingOrgRole: "joltra",
	})
	if err != nil {
		t.Fatalf("ask dispatch: %v", err)
	}
	payload := dispatchedReviewPayload(t, store, out.JobID)
	if len(payload.ReviewModelPool) != 0 {
		t.Fatalf("non-review job acquired a reviewer's pool %v (#2180)", payload.ReviewModelPool)
	}
	if got := dispatchedRuntime(t, store, out.JobID, runtime.ShellRuntime); got != runtime.ShellRuntime {
		t.Fatalf("non-review job runtime = %q, want the agent's own %q", got, runtime.ShellRuntime)
	}
}

// Acceptance criterion 3: no review path may be a dead end. A pinned runtime
// with no operator escape is refused outright whenever an unavailability hold
// is written for the pinned runtime - the shape #2181 hit from the other side.
func TestExplicitRuntimeOverridesThePinOnBothVerbs(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	routed, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.ClaudeRuntime, "--json")
	if failure != "" {
		t.Fatal(failure)
	}
	if got := dispatchedRuntime(t, store, routed.JobID, runtime.ShellRuntime); got != runtime.ClaudeRuntime {
		t.Fatalf("review request --runtime ignored: runtime = %q, want %q", got, runtime.ClaudeRuntime)
	}

	direct, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "opus-reviewer", Action: "review",
		Instructions: "review this", Background: true, Home: home,
		PullRequest: 13, HeadSHA: head, Branch: "feature/review",
		ActingOrgRole: "joltra", NoFixTarget: true, Runtime: runtime.ClaudeRuntime,
	})
	if err != nil {
		t.Fatalf("agent review dispatch: %v", err)
	}
	if got := dispatchedRuntime(t, store, direct.JobID, runtime.ShellRuntime); got != runtime.ClaudeRuntime {
		t.Fatalf("agent review --runtime ignored: runtime = %q, want %q", got, runtime.ClaudeRuntime)
	}
}

// The property that makes this seam safe to add: it RESOLVES a missing pool, it
// does not decide one. A producer that already chose a pool - a future router,
// a pipeline stage, an operator - must reach the daemon with exactly that pool,
// or the shared layer becomes a second authority competing with the first,
// which is the failure mode #2180 exists to end rather than relocate.
func TestSharedResolverPreservesACallerSuppliedPool(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	// Deliberately NOT the configured code pool, so an overwrite is visible.
	chosen := []string{"anthropic/claude-opus-5", "devin/swe-2"}
	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "opus-reviewer", Action: "review",
		Instructions: "review this", Background: true, Home: home,
		PullRequest: 12, HeadSHA: head, Branch: "feature/review",
		ActingOrgRole: "joltra", NoFixTarget: true,
		ReviewPurpose: "code", ReviewModelPool: chosen, Model: chosen[0],
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload := dispatchedReviewPayload(t, store, out.JobID)
	if !slices.Equal(payload.ReviewModelPool, chosen) {
		t.Fatalf("caller pool overwritten: got %v, want %v", payload.ReviewModelPool, chosen)
	}
	if payload.Model != chosen[0] {
		t.Fatalf("caller model overwritten: got %q, want %q", payload.Model, chosen[0])
	}
}

// The scoping the owner's runtime switch needs, proved where a live provider
// is not required: a shell agent's command is what executes, so a pool fallback
// advances its model but must NOT move it to omp - that would silently replace
// the operator's script with a model. Found by an existing test failing, then
// pinned here.
func TestReviewFallbackKeepsAScriptAgentOnItsRuntime(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	seedDaemonWorkerAgent(t, store, "script-reviewer", runtime.ShellRuntime,
		`echo "HTTP 429 Too Many Requests: rate limit reached; try again in 3 seconds" 1>&2
exit 1`, []string{"review"}, "owner/repo")
	// No Model: the `agent review` shape, whose pool #2180 now resolves.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-script", Agent: "script-reviewer", Action: "review", Repo: "owner/repo",
		Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("b", 40), NoFixTarget: true,
		ReviewModelPool: []string{"devin/swe-2", "openai-codex/gpt-5.6-sol"}, ReviewPurpose: "code",
	})
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	_, payload := blockerE2EJobPayload(t, store, "job-script")
	// A model that was never a pool entry now advances to the head of the pool:
	// before #2180 armed these jobs, this branch could not be reached at all.
	if payload.Model != "devin/swe-2" {
		t.Fatalf("model = %q, want the first untried pool entry", payload.Model)
	}
	if payload.RuntimeOverride != "" {
		t.Fatalf("runtime_override = %q, want none: switching a script agent to omp discards the script it runs", payload.RuntimeOverride)
	}
}

// The owner's fallback decision itself (2026-09-14): a quota-blocked review on
// a model-driven runtime retries on omp with the next pool model, rather than
// on the runtime that just refused it. A pool entry is an OMP provider/model
// name, so retrying a claude or codex job with it swaps one guaranteed failure
// for another.
//
// Asserted on the predicate because the alternative needs a live claude/codex
// delivery; the e2e above covers the excluded case on a real worker.
func TestReviewFallbackRuntimeSwitchScope(t *testing.T) {
	for _, tc := range []struct {
		effectiveRuntime string
		want             bool
	}{
		{runtime.ClaudeRuntime, true},
		{"codex", true},
		{"kimi", true},
		{runtime.OmpRuntime, false},
		{runtime.ShellRuntime, false},
		{"", false},
	} {
		if got := reviewFallbackSwitchesToOmp(tc.effectiveRuntime); got != tc.want {
			t.Fatalf("reviewFallbackSwitchesToOmp(%q) = %v, want %v", tc.effectiveRuntime, got, tc.want)
		}
	}
}
