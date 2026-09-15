package cli

import (
	"context"
	"github.com/gitmoot/gitmoot/internal/db"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2191. A review's duration is a property of the PROMPT CLASS, not of the
// agent that happens to answer it. Before this, the deadline came from the
// agent - 10m, 30m, 45m and 2h all existed on this box - so rotating reviewers
// rotated the deadline. The first router-rotated review drew a 10-minute agent
// for a class whose observed maximum is 164 minutes and aborted after nine.
func TestReviewClassDeadlineFloorsAShortAgentTimeout(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute, // gm-review-devin-omp's real value
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	review := workflow.JobPayload{ReviewPurpose: "code"}

	got := resolveEffectiveJobTimeout(review, managed, "review")
	if got.Timeout != config.DefaultReviewJobTimeout {
		t.Fatalf("review timeout = %s, want the class floor %s: a 10-minute agent still truncates a 164-minute class",
			got.Timeout, config.DefaultReviewJobTimeout)
	}
	if got.Source != "review class" {
		t.Fatalf("source = %q, want %q so the reason is legible in the clamp event", got.Source, "review class")
	}
}

// A FLOOR, not an assignment: an agent configured longer keeps its own value.
func TestReviewClassDeadlineDoesNotLowerALongerAgentTimeout(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        4 * time.Hour, // longer than the class floor
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     8 * time.Hour,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewPurpose: "code"}, managed, "review")
	if got.Timeout != 4*time.Hour {
		t.Fatalf("timeout = %s, want the agent's longer %s", got.Timeout, 4*time.Hour)
	}
}

// NON-REVIEW jobs are untouched. The floor is scoped to the class it measures;
// applying it everywhere would turn every hung job into a three-hour slot lease.
func TestReviewClassDeadlineLeavesNonReviewJobsAlone(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{}, managed, "ask")
	if got.Timeout != 10*time.Minute {
		t.Fatalf("non-review timeout = %s, want the agent's own %s", got.Timeout, 10*time.Minute)
	}
}

// The operator's ceiling still wins: the floor cannot raise a review above
// [daemon].job_timeout_max, and the clamp stays reported.
func TestReviewClassDeadlineStillObeysTheDaemonMaximum(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     30 * time.Minute,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewPurpose: "code"}, managed, "review")
	if got.Timeout != 30*time.Minute || !got.Clamped {
		t.Fatalf("timeout = %s clamped=%v, want the daemon maximum enforced", got.Timeout, got.Clamped)
	}
}

// A pool-bearing review with no purpose is still a review: `agent review`
// dispatches carry the pool from the chokepoint and may leave purpose empty.
func TestReviewClassDeadlineRecognisesAPoolBearingReview(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewModelPool: []string{"devin/swe-2"}}, managed, "review")
	if got.Timeout != config.DefaultReviewJobTimeout {
		t.Fatalf("pool-bearing review timeout = %s, want the class floor", got.Timeout)
	}
}

// Q6: the floor must be OPERATOR-SETTABLE, not a constant with a config knob
// that nothing reads. A mutant discarding the parsed value survived every test
// above, because they all construct managedJobRuntimeConfig by hand.
func TestReviewClassDeadlineComesFromTheOperatorsConfig(t *testing.T) {
	store, home := blockerE2EHome(t)
	paths := config.PathsForHome(home)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[review_router]\njob_timeout = \"90m\"\ncode = [\"sentinel/router-a\"]\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	worker := defaultJobWorker(store, io.Discard, home)
	managed, err := worker.managedJobConfig(context.Background(), "opus-reviewer")
	if err != nil {
		t.Fatalf("managedJobConfig: %v", err)
	}
	if managed.ReviewJobTimeout != 90*time.Minute {
		t.Fatalf("review class timeout = %s, want the configured 90m: the knob is documented but unread",
			managed.ReviewJobTimeout)
	}
}

// And an absent knob keeps the built-in floor rather than removing it.
func TestReviewClassDeadlineDefaultsWhenUnconfigured(t *testing.T) {
	store, home := blockerE2EHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	worker := defaultJobWorker(store, io.Discard, home)
	managed, err := worker.managedJobConfig(context.Background(), "opus-reviewer")
	if err != nil {
		t.Fatalf("managedJobConfig: %v", err)
	}
	if managed.ReviewJobTimeout != config.DefaultReviewJobTimeout {
		t.Fatalf("review class timeout = %s, want the built-in %s", managed.ReviewJobTimeout, config.DefaultReviewJobTimeout)
	}
}

// #2192 review (P3): an EXPLICIT per-job timeout must win over the class floor.
// Without this a coordinator that deliberately caps a review at 15m silently
// got 3h, so a delegation timeout could not bound a review at all - the router
// overruling a stated choice, which this campaign already rejected for runtimes.
func TestExplicitPayloadTimeoutBeatsTheClassFloor(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewPurpose: "code", JobTimeout: "15m"}, managed, "review")
	if got.Timeout != 15*time.Minute {
		t.Fatalf("timeout = %s, want the operator's explicit 15m: the floor overruled a stated choice", got.Timeout)
	}
	if got.Source != "payload" {
		t.Fatalf("source = %q, want payload", got.Source)
	}
}

// And an UNSTATED deadline is still floored - the case the floor exists for.
func TestUnstatedReviewTimeoutIsStillFloored(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	if got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewPurpose: "code"}, managed, "review"); got.Timeout != config.DefaultReviewJobTimeout {
		t.Fatalf("timeout = %s, want the class floor", got.Timeout)
	}
}

// #2192 review (P3): an unreadable [review_router] must not silently swap the
// operator's class deadline for the built-in one.
func TestUnreadableRouterConfigIsReportedNotSwallowed(t *testing.T) {
	store, home := blockerE2EHome(t)
	paths := config.PathsForHome(home)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[review_router]\njob_timeout = \"banana\"\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	worker := defaultJobWorker(store, io.Discard, home)
	managed, err := worker.managedJobConfig(context.Background(), "opus-reviewer")
	if err != nil {
		t.Fatalf("managedJobConfig: %v", err)
	}
	if managed.ReviewClassFallbackReason == "" {
		t.Fatal("an unreadable [review_router] produced no fallback reason: the operator's shorter deadline silently becomes the built-in")
	}
	if managed.ReviewJobTimeout != config.DefaultReviewJobTimeout {
		t.Fatalf("fallback timeout = %s, want the built-in %s", managed.ReviewJobTimeout, config.DefaultReviewJobTimeout)
	}
}

// The advisory must reach the JOB, not just the config struct. Asserting the
// reason field alone would be the "correct but undefended" shape this campaign
// keeps finding: the operator learns nothing from a field no consumer reads.
func TestUnreadableRouterConfigAdvisoryLandsOnTheReviewJob(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	paths := config.PathsForHome(home)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[review_router]\njob_timeout = \"banana\"\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgent(t, store, "script-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-class-advisory", Agent: "script-reviewer", Action: "review", Repo: "owner/repo",
		Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40), NoFixTarget: true,
		ReviewPurpose: "code",
	})

	worker := blockerE2EWorker(store, home, checkout)
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !blockerE2EHasEventKind(t, store, "job-class-advisory", "review_class_deadline_default") {
		t.Fatal("no review_class_deadline_default event: the operator's unreadable config is invisible on the job it changed")
	}
}

// #2192 review (P3): a MARKERLESS review - no purpose, no pool - used to escape
// the class deadline entirely, which is the defect the floor exists to prevent
// reached through a different door. The JOB TYPE is what the store says the job
// IS; markers are what a producer happened to fill in.
func TestMarkerlessReviewStillGetsTheClassDeadline(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{}, managed, "review")
	if got.Timeout != config.DefaultReviewJobTimeout {
		t.Fatalf("markerless review timeout = %s, want the class floor", got.Timeout)
	}
}

// #2192 review (P3): an unparseable payload timeout was SILENTLY ignored at the
// level just promoted to highest priority.
func TestUnparseablePayloadTimeoutIsReported(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	for _, raw := range []string{"banana", "-5m", "0s"} {
		got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewPurpose: "code", JobTimeout: raw}, managed, "review")
		if got.InvalidPayload != raw {
			t.Fatalf("payload job_timeout %q was ignored silently: InvalidPayload=%q", raw, got.InvalidPayload)
		}
		if got.Source == "payload" {
			t.Fatalf("payload job_timeout %q was USED despite being invalid", raw)
		}
		// It still falls through to the class floor rather than refusing.
		if got.Timeout != config.DefaultReviewJobTimeout {
			t.Fatalf("timeout = %s after an invalid payload value, want the class floor", got.Timeout)
		}
	}
}

// The positivity check on the operator knob was unguarded by any test: a zero
// or negative [review_router] job_timeout must be REFUSED, not silently used.
func TestNonPositiveClassDeadlineIsRefusedByTheLoader(t *testing.T) {
	for _, raw := range []string{"0s", "-10m"} {
		home := t.TempDir()
		paths := config.PathsForHome(home)
		if err := config.Initialize(paths); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("\n[review_router]\njob_timeout = \"" + raw + "\"\n"); err != nil {
			file.Close()
			t.Fatal(err)
		}
		file.Close()
		_, err = config.LoadReviewRouterSettings(paths)
		if err == nil {
			t.Fatalf("[review_router] job_timeout %q was accepted; a non-positive deadline must be refused", raw)
		}
		if !strings.Contains(err.Error(), "must be positive") {
			t.Fatalf("job_timeout %q refused for the wrong reason: %v", raw, err)
		}
	}
}

// #2192 review (P3): the event-error path was fail-closed by construction and
// untested. A trigger fails EXACTLY the new advisory insert and nothing else,
// so the assertion is about this branch rather than about a broken database:
// when the daemon cannot record that it ignored an operator's timeout, it must
// refuse the job instead of running it under a deadline nobody was told about.
func TestInvalidPayloadTimeoutEventFailureStopsTheJob(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "plain", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	if err := store.ExecForTest(ctx, `CREATE TRIGGER fail_invalid_timeout_event
BEFORE INSERT ON job_events
WHEN NEW.kind = 'job_timeout_payload_invalid'
BEGIN
	SELECT RAISE(ABORT, 'event write refused');
END;`); err != nil {
		t.Fatal(err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-invalid-timeout", Agent: "plain", Action: "ask", Repo: "owner/repo", Branch: "main", JobTimeout: "banana"})
	job, err := store.GetJob(ctx, "job-invalid-timeout")
	if err != nil {
		t.Fatal(err)
	}
	capture := &timeoutCaptureAdapter{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return capture, nil }
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("run returned %v, want the job finished as failed", err)
	}
	// DELIVERY is dispatch. The adapter FACTORY runs earlier in the job
	// lifecycle, so factory invocation would pass even with the guard removed.
	if capture.hasDeadline {
		t.Fatal("the agent ran after the advisory write failed: the substituted deadline went unrecorded")
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", after.State)
	}
}

// #2192 review (P3, seventh instance of the family): the advisory dedupped on
// (job_id, kind), so a retry carrying a DIFFERENT invalid timeout emitted
// nothing - first bad value disclosed, every later distinct bad value silent.
// The fix for a silent-failure mode had acquired one of its own at the
// disclosure layer.
//
// Driven through worker.run TWICE rather than by calling the store helper: a
// store-level test would pass with the production call reverted to
// AddJobEventIfAbsent, which is the defect under test.
func TestEachDistinctInvalidPayloadTimeoutIsDisclosed(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "plain", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-distinct-invalid", Agent: "plain", Action: "ask", Repo: "owner/repo", Branch: "main", JobTimeout: "banana"})

	run := func(raw string) {
		t.Helper()
		// The payload write touches ONLY the payload: assigning jobs.state in raw
		// SQL bypasses the lifecycle-generation seam, and the repo guard
		// TestEveryJobStateWriteBumpsTheLifecycleGeneration caught this fixture
		// doing exactly that. Every entry into queued must advance the
		// generation, so the state change goes through the store method.
		if err := store.ExecForTest(ctx, `UPDATE jobs SET payload=json_set(payload,'$.job_timeout',?) WHERE id=?`, raw, "job-distinct-invalid"); err != nil {
			t.Fatal(err)
		}
		current, err := store.GetJob(ctx, "job-distinct-invalid")
		if err != nil {
			t.Fatal(err)
		}
		if current.State != string(workflow.JobQueued) {
			moved, err := store.TransitionJobState(ctx, "job-distinct-invalid", current.State, string(workflow.JobQueued))
			if err != nil {
				t.Fatal(err)
			}
			if !moved {
				t.Fatalf("could not requeue the job from state %q", current.State)
			}
		}
		job, err := store.GetJob(ctx, "job-distinct-invalid")
		if err != nil {
			t.Fatal(err)
		}
		worker := defaultJobWorker(store, io.Discard, home)
		worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
			return t.TempDir(), nil
		}
		worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return &timeoutCaptureAdapter{}, nil }
		if err := worker.run(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	run("banana")
	run("-5m")
	run("banana") // an identical repeat must still collapse

	events, err := store.ListJobEvents(ctx, "job-distinct-invalid")
	if err != nil {
		t.Fatal(err)
	}
	disclosed := map[string]int{}
	for _, event := range events {
		if event.Kind == "job_timeout_payload_invalid" {
			disclosed[event.Message]++
		}
	}
	if len(disclosed) != 2 {
		t.Fatalf("distinct invalid values disclosed = %d, want 2 (banana and -5m): %v", len(disclosed), disclosed)
	}
	for message, count := range disclosed {
		if count != 1 {
			t.Fatalf("message %q recorded %d times, want the identical repeat collapsed", message, count)
		}
	}
}
