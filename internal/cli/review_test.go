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
	ctx := context.Background()
	payload, err := workflow.ParseJobPayload(mustGetJob(t, store, jobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.Result = &workflow.AgentResult{Decision: "approved", Summary: "clean", TestsRun: []string{"go test ./..."}, Evidence: "executed"}
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
