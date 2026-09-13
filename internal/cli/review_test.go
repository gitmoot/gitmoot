package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func reviewRouterHome(t *testing.T) (string, *db.Store, string) {
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
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
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
	return home, store, head
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
	if reviewJobStillAnswers(ctx, store, job) {
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
