package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// This file holds the integrated, full-chain, no-LLM / no-network E2Es for the
// #682/#683/#684 reliability batch (PRs #691-#693). Each chain drives the REAL
// production code paths end to end on an isolated t.TempDir home — the actual CLI
// commands, the real daemon worker tick, the real resume/recovery helpers, and the
// real GitHub client run seam behind a stubbed transport — so the chains prove
// integration, not just units. They mirror the established full-chain patterns in
// session_job_e2e_test.go, review_head_resync_test.go and canary_e2e_test.go.

const reliabilityBlockedNeedsResult = `{"gitmoot_result":{"decision":"blocked","summary":"needs credentials","findings":[],"changes_made":[],"tests_run":[],"needs":["API key","R2 token"],"delegations":[]}}`

const reliabilityApprovedReviewResult = `{"gitmoot_result":{"decision":"approved","summary":"looks good at the new head","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

func reliabilityDeliveredCount(delivered []string, id string) int {
	n := 0
	for _, d := range delivered {
		if d == id {
			n++
		}
	}
	return n
}

// -----------------------------------------------------------------------------
// CHAIN A — #682 resumable blocked/needs gates with auto-resume.
//
// Enqueue a job through the production Mailbox path, drive it to a `blocked`
// result carrying needs[] through a REAL worker tick (a fake DeliveryAdapter
// returns the blocked gitmoot_result), then prove the whole resumable-gate
// contract through REAL code: gates persisted at the delivery chokepoint, the
// `job gates` CLI lists them, clearing ONE leaves the job blocked, clearing ALL
// auto-requeues through the REAL resume path (RetryJob), and a second REAL worker
// tick re-delivers the resumed stage — closing the loop end to end.
// -----------------------------------------------------------------------------
func TestResumableGatesFullChainE2E(t *testing.T) {
	ctx := context.Background()

	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("db.Open returned error: %v", err)
	}
	defer store.Close()

	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")

	// Enqueue through the production Mailbox path.
	const jobID = "gate-ask-1"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: jobID, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})

	// --- drive to blocked+needs through a REAL worker tick -------------------
	adapter := &cliWorkerFakeAdapter{output: reliabilityBlockedNeedsResult}
	worker := poolSchedulerWorker(t, store, adapter, false)
	if err := runDaemonWorkerTick(ctx, store, worker, 1, false, "owner/repo", "", io.Discard, time.Now().UTC()); err != nil {
		t.Fatalf("runDaemonWorkerTick(block) returned error: %v", err)
	}
	if got := reliabilityDeliveredCount(adapter.deliveredIDs(), jobID); got != 1 {
		t.Fatalf("delivered count after first tick = %d, want 1", got)
	}
	blocked, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if blocked.State != string(workflow.JobBlocked) {
		t.Fatalf("job state after blocked result = %q, want blocked", blocked.State)
	}

	// Gates were persisted at the single result-bearing terminal chokepoint.
	gates, err := store.ListJobGates(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobGates returned error: %v", err)
	}
	if len(gates) != 2 || gates[0].Need != "API key" || gates[1].Need != "R2 token" || gates[0].Satisfied || gates[1].Satisfied {
		t.Fatalf("gates = %+v, want two open gates from needs", gates)
	}
	if evs, err := store.ListJobEvents(ctx, jobID); err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	} else if !anyEventKind(evs, "gates_recorded") {
		t.Fatalf("events = %+v, want a gates_recorded event", evs)
	}

	// --- `job gates <id>` lists them (real CLI dispatcher) -------------------
	var listOut, listErr bytes.Buffer
	if code := Run([]string{"job", "gates", jobID, "--home", home}, &listOut, &listErr); code != 0 {
		t.Fatalf("job gates list exit = %d, stderr=%s", code, listErr.String())
	}
	if !strings.Contains(listOut.String(), "open\tAPI key") || !strings.Contains(listOut.String(), "2 gate(s), 2 open") {
		t.Fatalf("job gates list output = %q", listOut.String())
	}

	// --- clear ONE need -> still blocked, not resumed (real CLI) -------------
	var clearOut, clearErr bytes.Buffer
	if code := Run([]string{"job", "gates", "clear", jobID, "--need", "API key", "--home", home}, &clearOut, &clearErr); code != 0 {
		t.Fatalf("clear one exit = %d, stderr=%s", code, clearErr.String())
	}
	if !strings.Contains(clearOut.String(), "not resumed") {
		t.Fatalf("clear-one output = %q, want not resumed", clearOut.String())
	}
	if job, _ := store.GetJob(ctx, jobID); job.State != string(workflow.JobBlocked) {
		t.Fatalf("state after clearing one gate = %q, want still blocked", job.State)
	}
	if _, open, err := store.CountJobGates(ctx, jobID); err != nil {
		t.Fatalf("CountJobGates returned error: %v", err)
	} else if open != 1 {
		t.Fatalf("open gates after clearing one = %d, want 1", open)
	}

	// --- clear ALL -> auto-requeued through the REAL resume path -------------
	clearOut.Reset()
	clearErr.Reset()
	if code := Run([]string{"job", "gates", "clear", jobID, "--all", "--home", home}, &clearOut, &clearErr); code != 0 {
		t.Fatalf("clear all exit = %d, stderr=%s", code, clearErr.String())
	}
	if !strings.Contains(clearOut.String(), "resumed:") {
		t.Fatalf("clear-all output = %q, want resumed", clearOut.String())
	}
	requeued, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if requeued.State != string(workflow.JobQueued) {
		t.Fatalf("state after clearing all gates = %q, want queued (RetryJob re-queued the blocked stage)", requeued.State)
	}
	if evs, err := store.ListJobEvents(ctx, jobID); err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	} else if !anyEventKind(evs, "gates_cleared_resume") {
		t.Fatalf("events = %+v, want a gates_cleared_resume event", evs)
	}

	// --- a second REAL worker tick re-delivers the resumed stage -------------
	if err := runDaemonWorkerTick(ctx, store, worker, 1, false, "owner/repo", "", io.Discard, time.Now().UTC()); err != nil {
		t.Fatalf("runDaemonWorkerTick(resume) returned error: %v", err)
	}
	if got := reliabilityDeliveredCount(adapter.deliveredIDs(), jobID); got != 2 {
		t.Fatalf("delivered count after resume tick = %d, want 2 (the resumed stage re-delivered)", got)
	}
}

// TestResumableGatesDoNotResumeSessionJobE2E proves the first safety exclusion
// through the REAL CLI resume path: a session job (ExternallyDriven, #657) is
// NEVER auto-resumed even when every gate is cleared, so a resource gate can never
// make the daemon Deliver an empty session payload.
func TestResumableGatesDoNotResumeSessionJobE2E(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	if err := store.CreateExternallyDrivenJobWithEvent(ctx, db.Job{
		ID:      "sess-blocked",
		Agent:   "lead",
		Type:    "implement",
		State:   string(workflow.JobBlocked),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, db.JobEvent{Kind: string(workflow.JobBlocked), Message: "blocked"}); err != nil {
		t.Fatalf("CreateExternallyDrivenJobWithEvent returned error: %v", err)
	}
	if _, err := store.RecordJobGates(ctx, "sess-blocked", []string{"API key"}); err != nil {
		t.Fatalf("RecordJobGates returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "gates", "clear", "sess-blocked", "--all", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("clear all exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not resumed") || !strings.Contains(stdout.String(), "session job") {
		t.Fatalf("clear-all output = %q, want a session-job not-resumed reason", stdout.String())
	}
	if job, _ := store.GetJob(ctx, "sess-blocked"); job.State != string(workflow.JobBlocked) {
		t.Fatalf("session job state = %q, want still blocked (never re-queued)", job.State)
	}
}

// TestResumableGatesDoNotBypassAwaitingHumanE2E proves the second safety exclusion
// through the REAL CLI resume path: a blocked stage whose tree is paused awaiting a
// human (#305/#340/#445) is NOT auto-resumed by clearing a resource gate — the
// human's `gitmoot resume` decision must not be bypassed.
func TestResumableGatesDoNotBypassAwaitingHumanE2E(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	if err := store.UpsertTask(ctx, db.Task{ID: "task-h", RepoFullName: "owner/repo", Title: "t", State: string(workflow.TaskAwaitingHuman)}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seedCLIJob(t, store, db.Job{
		ID:      "human-blocked",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobBlocked),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", TaskID: "task-h"}),
	}, "blocked")
	if _, err := store.RecordJobGates(ctx, "human-blocked", []string{"human approval"}); err != nil {
		t.Fatalf("RecordJobGates returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "gates", "clear", "human-blocked", "--all", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("clear all exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not resumed") || !strings.Contains(stdout.String(), "awaiting a human") {
		t.Fatalf("clear-all output = %q, want an awaiting-human not-resumed reason", stdout.String())
	}
	if job, _ := store.GetJob(ctx, "human-blocked"); job.State != string(workflow.JobBlocked) {
		t.Fatalf("state = %q, want still blocked (human gate must not be bypassed)", job.State)
	}
}

// -----------------------------------------------------------------------------
// CHAIN B — #684 review re-sync when the PR branch advances.
//
// The existing unit tests (review_head_resync_test.go) call defaultCheckout
// directly and cover open-resync / closed-fail / wrong-branch-decline. This chain
// extends coverage to the FULL worker tick: a real local git repo whose branch
// advanced past the pinned head, driven through the REAL worker checkout path and
// on to DELIVERY, proving the re-sync feeds through to an actual review delivery
// (fake adapter captures it) instead of failing. It also adds the missing counter-
// case the review flagged as untested: NO local PR record ⇒ decline re-sync.
// -----------------------------------------------------------------------------
func TestReviewResyncFullChainDeliversOnOpenPRE2E(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "feat/x")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")

	// The head the review was pinned to at enqueue.
	staleHead := daemonWorkerHeadSHA(t, checkout)

	// The branch advances (a newer commit is pushed before the review runs).
	if err := os.WriteFile(checkout+"/feature.txt", []byte("more work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "feature.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "advance the branch")
	newHead := daemonWorkerHeadSHA(t, checkout)
	if newHead == staleHead {
		t.Fatal("test setup: branch head did not advance")
	}

	// The PR is still OPEN.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       23,
		HeadBranch:   "feat/x",
		HeadSHA:      staleHead,
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	const jobID = "review-resync-e2e-1"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:          jobID,
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "feat/x",
		PullRequest: 23,
		HeadSHA:     staleHead,
		TaskID:      "review-task-e2e-1", // no task row -> resolves the shared checkout
	})

	// Build a worker that runs the REAL defaultCheckout (so the re-sync actually
	// happens) but captures delivery with a fake adapter and drops the merge gate so
	// the approved review terminates locally with no GitHub call.
	worker := defaultJobWorker(store, io.Discard)
	adapter := &cliWorkerFakeAdapter{output: reliabilityApprovedReviewResult}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return adapter, nil }
	baseWorkflow := worker.defaultWorkflow
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		engine := baseWorkflow(checkout)
		// A nil MergeGate makes runMergeGate resolve an approved review to
		// ready-to-merge with no network (see engine.runMergeGate).
		engine.MergeGate = nil
		return engine
	}

	if err := runDaemonWorkerTick(ctx, store, worker, 1, false, "owner/repo", "", io.Discard, time.Now().UTC()); err != nil {
		t.Fatalf("runDaemonWorkerTick(review) returned error: %v", err)
	}

	// The review was DELIVERED (not failed at the head-mismatch pre-flight).
	if got := reliabilityDeliveredCount(adapter.deliveredIDs(), jobID); got != 1 {
		t.Fatalf("delivered count = %d, want 1 (review re-synced and delivered, not failed)", got)
	}

	reloaded, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	// Re-synced to the checkout's current head, persisted on the payload.
	payload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.HeadSHA != newHead {
		t.Fatalf("re-synced HeadSHA = %q, want current head %q (was %q)", payload.HeadSHA, newHead, staleHead)
	}
	// Delivered instead of failing.
	if reloaded.State != string(workflow.JobSucceeded) {
		t.Fatalf("review job state = %q, want succeeded (delivered, not failed)", reloaded.State)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasResyncEvent(events, "review_head_resynced") {
		t.Fatalf("events = %+v, want a review_head_resynced event", events)
	}
}

// TestDefaultCheckoutDeclinesResyncWhenNoLocalPRRecordE2E is the counter-case the
// #691 review flagged as untested: when the store has NO local pull_requests
// record for the review's PR, resyncReviewHead cannot confirm the PR is open, so
// it must DECLINE the re-sync and keep the existing head-mismatch failure — the
// payload head is left unchanged and no re-sync event is recorded. (Open / closed
// / wrong-branch are already covered by review_head_resync_test.go.)
func TestDefaultCheckoutDeclinesResyncWhenNoLocalPRRecordE2E(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "feat/x")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	staleHead := daemonWorkerHeadSHA(t, checkout)
	if err := os.WriteFile(checkout+"/feature.txt", []byte("more work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "feature.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "advance the branch")

	// Deliberately DO NOT UpsertPullRequest: the store has no evidence the PR is live.
	if _, err := (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
		ID:          "review-no-pr-1",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "feat/x",
		PullRequest: 99,
		HeadSHA:     staleHead,
		TaskID:      "review-task-no-pr",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "review-no-pr-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	_, err = worker.defaultCheckout(ctx, job, payload, runtime.Agent{Name: "reviewer"})
	if err == nil {
		t.Fatal("defaultCheckout re-synced a review with no local PR record; want a clean head-mismatch failure")
	}
	if !strings.Contains(err.Error(), "not review job head") {
		t.Fatalf("expected the review head-mismatch error, got: %v", err)
	}

	reloaded, err := store.GetJob(ctx, "review-no-pr-1")
	if err != nil {
		t.Fatalf("GetJob (reload) returned error: %v", err)
	}
	reloadedPayload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload (reload) returned error: %v", err)
	}
	if reloadedPayload.HeadSHA != staleHead {
		t.Fatalf("no-PR-record HeadSHA = %q, want it left unchanged at %q", reloadedPayload.HeadSHA, staleHead)
	}
	events, err := store.ListJobEvents(ctx, "review-no-pr-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasResyncEvent(events, "review_head_resynced") {
		t.Fatalf("events = %+v, want NO review_head_resynced event when there is no local PR record", events)
	}
}

// -----------------------------------------------------------------------------
// CHAIN C — #683 GitHub rate-limit-aware scheduling, wired through the daemon.
//
// The review's MEDIUM was that non-daemon paths bypass the limiter wiring; this
// chain asserts the DAEMON path IS wired: the real configureGitHubLimiter reads
// the isolated home's [github] config and installs it as the process-global
// default, and a github.Client built via NewClient (which carries no Limiter of
// its own) actually schedules through that shared limiter — proven by both the
// concurrency cap bounding in-flight calls and a secondary-limit 403 with a
// Retry-After engaging the clamped process-wide backoff.
// -----------------------------------------------------------------------------

// limiterProbeRunner is a stubbed subprocess.Runner that never touches the network.
// It records the peak number of concurrently in-flight gh calls (to observe the
// concurrency cap) and can be told to return a secondary-rate-limit failure.
type limiterProbeRunner struct {
	mu        sync.Mutex
	inFlight  int32
	peak      int32
	hold      time.Duration
	secondary bool
}

func (r *limiterProbeRunner) Run(ctx context.Context, _ string, _ string, _ ...string) (subprocess.Result, error) {
	if r.secondary {
		return subprocess.Result{Stderr: "HTTP 403: secondary rate limit\nRetry-After: 30"}, errors.New("exit status 1")
	}
	cur := atomic.AddInt32(&r.inFlight, 1)
	for {
		old := atomic.LoadInt32(&r.peak)
		if cur <= old || atomic.CompareAndSwapInt32(&r.peak, old, cur) {
			break
		}
	}
	if r.hold > 0 {
		timer := time.NewTimer(r.hold)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	atomic.AddInt32(&r.inFlight, -1)
	return subprocess.Result{Stdout: `{"nameWithOwner":"owner/repo"}`}, nil
}

func (r *limiterProbeRunner) LookPath(file string) (string, error) { return file, nil }

func TestGitHubLimiterDaemonWiringFullChainE2E(t *testing.T) {
	// The default limiter is process-global to the github package; reset it to the
	// inert default after this test so no later cli test inherits our caps/backoff.
	t.Cleanup(func() { github.ConfigureDefault(github.RateLimiterConfig{}) })

	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	// Configure a tiny call budget via [github] in the isolated home.
	cfg := config.DefaultConfig(paths) + "\n[github]\nmax_concurrent = 2\nmin_interval = 0\nsecondary_backoff = true\nbackoff_base = 60\nbackoff_max = 300\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	// The REAL daemon wiring: load [github] and install the process-global limiter.
	configureGitHubLimiter(paths, io.Discard)

	// The config -> limiter wiring is live: the default limiter reflects the policy.
	snap := github.DefaultLimiterSnapshot()
	if snap.MaxConcurrent != 2 {
		t.Fatalf("DefaultLimiterSnapshot.MaxConcurrent = %d, want 2 (config not wired)", snap.MaxConcurrent)
	}
	if !snap.BackoffEnabled {
		t.Fatalf("DefaultLimiterSnapshot.BackoffEnabled = false, want true (config not wired)")
	}

	// --- concurrency cap bounds in-flight calls through the NewClient seam ----
	// github.NewClient carries no Limiter of its own, so it schedules through the
	// process-global default we just configured. Firing more concurrent calls than
	// the cap, each holding a slot for a real window, must never exceed the cap in
	// flight; a broken wiring (inert default) would let all N overlap.
	runner := &limiterProbeRunner{hold: 40 * time.Millisecond}
	client := github.NewClient(t.TempDir())
	client.Runner = runner

	const fan = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < fan; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := client.Ping(context.Background()); err != nil {
				t.Errorf("Ping returned error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	peak := atomic.LoadInt32(&runner.peak)
	if peak > 2 {
		t.Fatalf("peak in-flight gh calls = %d, want <= 2 (the shared concurrency cap was not enforced)", peak)
	}
	if peak < 2 {
		t.Fatalf("peak in-flight gh calls = %d, want the calls to actually overlap up to the cap (>= 2)", peak)
	}

	// --- a secondary-limit 403 with Retry-After engages the clamped backoff ---
	// The same shared limiter must record a process-wide pause when a call routed
	// through the client path ultimately fails secondary — the reactive protection
	// #683 needed. This proves the client run seam feeds the daemon-configured
	// limiter's backoff, not just its static caps.
	secClient := github.NewClient(t.TempDir())
	secClient.Runner = &limiterProbeRunner{secondary: true}
	secClient.MaxRetries = 1
	secClient.Sleep = func(context.Context, time.Duration) error { return nil } // no real inter-retry sleep
	if err := secClient.Ping(context.Background()); err == nil {
		t.Fatal("Ping through a persistent secondary limit returned nil error, want failure")
	}
	backoff := github.DefaultLimiterSnapshot()
	if !backoff.InBackoff {
		t.Fatalf("limiter not in backoff after a final secondary failure: %+v", backoff)
	}
	if backoff.SecondaryHits != 1 {
		t.Fatalf("SecondaryHits = %d, want 1 (one final failure, not one per attempt)", backoff.SecondaryHits)
	}
	// Retry-After: 30 honored (clamped well under the 30m ceiling); allow slack for
	// the real clock ticking between NoteSecondaryLimit and the Snapshot read.
	if backoff.BackoffRemaining <= 25*time.Second || backoff.BackoffRemaining > 30*time.Second {
		t.Fatalf("BackoffRemaining = %s, want ~30s (Retry-After honored)", backoff.BackoffRemaining)
	}
}
