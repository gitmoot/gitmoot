package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// E2E for the #532 first slice (operational-blocker classification + deferred
// auto-redispatch), deterministic and LLM-free: a REAL shell-runtime agent whose
// script fails the FIRST delivery with a fabricated 429-with-reset and succeeds
// on the second, driven through the REAL daemon dispatch entry
// (runQueuedJobsForRepo → listPendingQueuedJobs → jobWorker.run → engine.RunJob
// → ShellAdapter → real subprocess → Mailbox.Run's pre-terminal BlockerDeferrer).
//
// MUTATION PROOF: disable the classification match (make
// classifyOperationalBlocker return false, or drop the "throttled" case) and
// the first dispatch terminally fails the job — the `queued` assertion and the
// second-attempt success assertion below flip RED.

// blockerE2EHome initializes an isolated gitmoot home (no GITMOOT_HOME / live
// home reads) and opens its store.
func blockerE2EHome(t *testing.T) (*db.Store, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store, home
}

// blockerE2EWorker builds a jobWorker on the isolated home with only the
// checkout resolution stubbed (checkout state is not under test); the adapter is
// the REAL ShellAdapter built by the default factory from the agent's runtime.
func blockerE2EWorker(store *db.Store, home string, checkout string) jobWorker {
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	return worker
}

func blockerE2EDeliveryCount(t *testing.T, countFile string) int {
	t.Helper()
	data, err := os.ReadFile(countFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read count file: %v", err)
	}
	return strings.Count(string(data), "x")
}

func blockerE2EJobPayload(t *testing.T, store *db.Store, jobID string) (db.Job, workflow.JobPayload) {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	return job, payload
}

func blockerE2EHasEventKind(t *testing.T, store *db.Store, jobID string, kind string) bool {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

const blockerE2EApprovedResult = `{"gitmoot_result":{"decision":"approved","summary":"second attempt succeeded","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

func TestE2E532QuotaBlockerDeferredAndAutoRedispatched(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	state := t.TempDir()
	marker := filepath.Join(state, "delivered")
	countFile := filepath.Join(state, "count")
	promptFile := filepath.Join(state, "retry-prompt")
	// Fail the first delivery with a fabricated 429 carrying a relative reset;
	// succeed on the second, capturing the retried prompt ($1) so the
	// at-least-once reconciliation notice is provable end to end.
	script := fmt.Sprintf(`printf x >> %q
if [ ! -f %q ]; then
  : > %q
  echo "HTTP 429 Too Many Requests: rate limit reached; try again in 3 seconds" 1>&2
  exit 1
fi
printf '%%s' "$1" > %q
printf '%%s' '%s'`, countFile, marker, marker, promptFile, blockerE2EApprovedResult)
	seedDaemonWorkerAgent(t, store, "opsbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-quota", Agent: "opsbot", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 1,
	})
	worker := blockerE2EWorker(store, home, checkout)
	sink := &recordingSink{}
	worker.EventSinkOverride = sink

	// First dispatch: the delivery fails 429 → the job must be DEFERRED
	// (blocked-operational), not terminally failed.
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("first dispatch returned error: %v", err)
	}
	if got := blockerE2EDeliveryCount(t, countFile); got != 1 {
		t.Fatalf("delivery count after first dispatch = %d, want 1", got)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-quota")
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state after classified 429 = %q, want %q (terminal fail means classification is broken)", job.State, workflow.JobQueued)
	}
	if payload.BlockerClass != string(blockerClassRuntimeQuota) {
		t.Fatalf("blocker_class = %q, want %q", payload.BlockerClass, blockerClassRuntimeQuota)
	}
	if payload.BlockerAttempts != 1 {
		t.Fatalf("blocker_attempts = %d, want 1", payload.BlockerAttempts)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil {
		t.Fatalf("parse blocker_retry_at %q: %v", payload.BlockerRetryAt, err)
	}
	// The fabricated 3s reset is floored to quotaBlockerMinParsedDelay (5s), plus
	// up to 10% jitter.
	if until := time.Until(retryAt); until <= 0 || until > quotaBlockerMinParsedDelay+time.Second {
		t.Fatalf("blocker_retry_at %s not within the floored reset window (until=%s)", payload.BlockerRetryAt, until)
	}
	if !blockerE2EHasEventKind(t, store, "job-quota", blockerDeferredEventKind) {
		t.Fatalf("missing %s job event", blockerDeferredEventKind)
	}
	// The [events] stream must carry the additive job.deferred so external
	// consumers acting on the already-emitted job.failed can suppress terminal
	// handling for a job the daemon is about to retry.
	deferredEvents := sink.byType(events.EventJobDeferred)
	if len(deferredEvents) != 1 {
		t.Fatalf("job.deferred emissions = %d, want 1", len(deferredEvents))
	}
	if ev := deferredEvents[0]; ev.JobID != "job-quota" || ev.Repo != "owner/repo" ||
		!strings.Contains(ev.Detail, string(blockerClassRuntimeQuota)) ||
		!strings.Contains(ev.Detail, "attempt 1/") ||
		!strings.Contains(ev.Detail, "retry at ") {
		t.Fatalf("job.deferred event = %+v, want class/attempt/retry_at in detail", ev)
	}

	// The #552 stuck-reason surface (job list/show) must explain the hold.
	reason := loadStuckReason(store, job)
	if !strings.HasPrefix(reason.Reason, "blocked-operational: "+string(blockerClassRuntimeQuota)) {
		t.Fatalf("stuck reason = %q, want blocked-operational %s prefix", reason.Reason, blockerClassRuntimeQuota)
	}
	if reason.NextRetryAt != payload.BlockerRetryAt {
		t.Fatalf("stuck next_retry_at = %q, want %q", reason.NextRetryAt, payload.BlockerRetryAt)
	}

	// While inside the hold window, the queue gate must keep the job out of the
	// pending listing and re-dispatch attempts must not deliver.
	if time.Now().UTC().Before(retryAt) {
		pending, err := listPendingQueuedJobs(ctx, worker, "", "", true)
		if err != nil {
			t.Fatalf("listPendingQueuedJobs returned error: %v", err)
		}
		for _, p := range pending {
			if p.ID == "job-quota" {
				t.Fatal("held job is listed as pending inside its hold window")
			}
		}
	}
	for {
		if !time.Now().UTC().Before(retryAt) {
			break
		}
		if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
			t.Fatalf("held-window dispatch returned error: %v", err)
		}
		// Only judge deliveries observed strictly inside the window; a dispatch
		// that legitimately started at/after retryAt is not a gate violation.
		if time.Now().UTC().Before(retryAt) {
			if got := blockerE2EDeliveryCount(t, countFile); got != 1 {
				t.Fatalf("job was re-dispatched before earliest-retry-at (deliveries=%d)", got)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// After the reset elapses the daemon must re-dispatch automatically and the
	// job must SUCCEED on the second attempt.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
			t.Fatalf("post-window dispatch returned error: %v", err)
		}
		job, payload = blockerE2EJobPayload(t, store, "job-quota")
		if job.State == string(workflow.JobSucceeded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never succeeded after the reset window; state=%q", job.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := blockerE2EDeliveryCount(t, countFile); got != 2 {
		t.Fatalf("delivery count after success = %d, want exactly 2", got)
	}
	if payload.Result == nil || payload.Result.Decision != "approved" {
		t.Fatalf("stored result = %+v, want approved decision", payload.Result)
	}
	if payload.BlockerAttempts != 1 {
		t.Fatalf("blocker_attempts after success = %d, want 1 (single deferral)", payload.BlockerAttempts)
	}
	// The retried delivery is at-least-once for side effects, so its prompt must
	// carry the reconciliation notice telling the agent to verify prior work.
	retryPrompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read captured retry prompt: %v", err)
	}
	if !strings.Contains(string(retryPrompt), "operational blocker (runtime_quota)") ||
		!strings.Contains(string(retryPrompt), "reconcile") {
		t.Fatalf("retried prompt is missing the at-least-once reconciliation notice:\n%s", retryPrompt)
	}
}

func TestE2E532AuthBlockerDeferredAndHeld(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	countFile := filepath.Join(t.TempDir(), "count")
	script := fmt.Sprintf(`printf x >> %q
echo "Failed to authenticate. API Error: 401 authentication_error: invalid x-api-key" 1>&2
exit 1`, countFile)
	seedDaemonWorkerAgent(t, store, "authbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-auth", Agent: "authbot", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 2,
	})
	worker := blockerE2EWorker(store, home, checkout)

	before := time.Now().UTC()
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-auth")
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state after classified auth failure = %q, want %q", job.State, workflow.JobQueued)
	}
	if payload.BlockerClass != string(blockerClassRuntimeAuth) {
		t.Fatalf("blocker_class = %q, want %q", payload.BlockerClass, blockerClassRuntimeAuth)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil {
		t.Fatalf("parse blocker_retry_at %q: %v", payload.BlockerRetryAt, err)
	}
	if min, max := before.Add(authBlockerRetryDelay-time.Minute), before.Add(authBlockerRetryDelay+time.Minute); retryAt.Before(min) || retryAt.After(max) {
		t.Fatalf("auth blocker_retry_at %s outside expected ~%s window", payload.BlockerRetryAt, authBlockerRetryDelay)
	}
	// Held: an immediate re-dispatch must not deliver again.
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("held dispatch returned error: %v", err)
	}
	if got := blockerE2EDeliveryCount(t, countFile); got != 1 {
		t.Fatalf("delivery count while held = %d, want 1", got)
	}
	reason := loadStuckReason(store, job)
	if !strings.HasPrefix(reason.Reason, "blocked-operational: "+string(blockerClassRuntimeAuth)) {
		t.Fatalf("stuck reason = %q, want blocked-operational %s prefix", reason.Reason, blockerClassRuntimeAuth)
	}
}

// A PRODUCT failure — the agent answered with a parseable gitmoot_result whose
// decision is "failed" — must NEVER be classified or auto-retried, even though
// the summary text mentions a rate limit.
func TestE2E532ProductFailureIsNeverRetried(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	countFile := filepath.Join(t.TempDir(), "count")
	result := `{"gitmoot_result":{"decision":"failed","summary":"could not finish: upstream rate limit made the approach infeasible","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	script := fmt.Sprintf("printf x >> %q\nprintf '%%s' '%s'", countFile, result)
	seedDaemonWorkerAgent(t, store, "prodbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-product", Agent: "prodbot", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 3,
	})
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-product")
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("product failure state = %q, want %q", job.State, workflow.JobFailed)
	}
	if payload.Result == nil || payload.Result.Decision != "failed" {
		t.Fatalf("stored result = %+v, want failed decision", payload.Result)
	}
	if payload.BlockerClass != "" || payload.BlockerAttempts != 0 || payload.BlockerRetryAt != "" {
		t.Fatalf("product failure acquired blocker fields: %+v", payload)
	}
	if blockerE2EHasEventKind(t, store, "job-product", blockerDeferredEventKind) {
		t.Fatal("product failure recorded a blocker_deferred event")
	}
	// A later dispatch pass must not resurrect it.
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("second dispatch returned error: %v", err)
	}
	if got := blockerE2EDeliveryCount(t, countFile); got != 1 {
		t.Fatalf("product failure was re-delivered (count=%d)", got)
	}
	job, _ = blockerE2EJobPayload(t, store, "job-product")
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("product failure left terminal state: %q", job.State)
	}
}

// A repair-exhausted gitmoot_result CONTRACT failure — the agent delivered
// output every time but never a valid envelope, and the final error text is
// agent-authored (here an unsupported decision that mentions "rate limit") —
// is a product failure: it must never classify as an operational blocker, no
// matter what quota/auth words the agent's text contains.
func TestE2E532ContractValidationFailureIsNeverRetried(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	countFile := filepath.Join(t.TempDir(), "count")
	malformed := `{"gitmoot_result":{"decision":"blocked: rate limit","summary":"quota","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	script := fmt.Sprintf("printf x >> %q\nprintf '%%s' '%s'", countFile, malformed)
	seedDaemonWorkerAgent(t, store, "contractbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-contract", Agent: "contractbot", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 6,
	})
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-contract")
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("contract failure state = %q, want %q (a deferral means agent-authored text classified)", job.State, workflow.JobFailed)
	}
	if payload.BlockerClass != "" || payload.BlockerRetryAt != "" {
		t.Fatalf("contract failure acquired blocker fields: %+v", payload)
	}
	if blockerE2EHasEventKind(t, store, "job-contract", blockerDeferredEventKind) {
		t.Fatal("contract failure recorded a blocker_deferred event")
	}
	// First delivery + the repair re-asks — but never a #532 auto-retry.
	deliveries := blockerE2EDeliveryCount(t, countFile)
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("second dispatch returned error: %v", err)
	}
	if got := blockerE2EDeliveryCount(t, countFile); got != deliveries {
		t.Fatalf("contract failure was re-delivered (count %d -> %d)", deliveries, got)
	}
}

// A 429 that hits the REPAIR delivery — i.e. after a first delivery that
// completed a full, potentially side-effectful agent turn — must not defer:
// the persisted raw output proves execution, and an auto-retry would re-run
// the entire prompt (duplicate pushes/PRs). The job keeps today's terminal
// path.
func TestE2E532RepairDeliveryQuotaFailureIsNotRetried(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	state := t.TempDir()
	marker := filepath.Join(state, "delivered")
	countFile := filepath.Join(state, "count")
	// First delivery SUCCEEDS but with no gitmoot_result envelope (a completed
	// turn); every repair delivery then fails with a classified 429.
	script := fmt.Sprintf(`printf x >> %q
if [ ! -f %q ]; then
  : > %q
  printf '%%s' "did the work, pushed the branch, forgot the envelope"
  exit 0
fi
echo "HTTP 429 Too Many Requests: rate limit reached; try again in 3 seconds" 1>&2
exit 1`, countFile, marker, marker)
	seedDaemonWorkerAgent(t, store, "repairbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-repair", Agent: "repairbot", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 7,
	})
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-repair")
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("repair-delivery 429 state = %q, want %q (deferring would re-run an already-executed job)", job.State, workflow.JobFailed)
	}
	if len(payload.RawOutputs) == 0 {
		t.Fatal("first delivery's raw output was not persisted; the side-effect guard has nothing to key on")
	}
	if payload.BlockerClass != "" || payload.BlockerRetryAt != "" {
		t.Fatalf("already-executed job acquired blocker fields: %+v", payload)
	}
	if blockerE2EHasEventKind(t, store, "job-repair", blockerDeferredEventKind) {
		t.Fatal("already-executed job recorded a blocker_deferred event")
	}
}

// An unclassified operational error keeps today's byte-identical terminal path.
func TestE2E532UnclassifiedFailureStaysTerminal(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	script := `echo "boom: something unrelated broke" 1>&2
exit 2`
	seedDaemonWorkerAgent(t, store, "plainbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-plain", Agent: "plainbot", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 4,
	})
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-plain")
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("unclassified failure state = %q, want %q", job.State, workflow.JobFailed)
	}
	if payload.BlockerClass != "" || payload.BlockerRetryAt != "" {
		t.Fatalf("unclassified failure acquired blocker fields: %+v", payload)
	}
	if blockerE2EHasEventKind(t, store, "job-plain", blockerDeferredEventKind) {
		t.Fatal("unclassified failure recorded a blocker_deferred event")
	}
}

// The auto-retry budget is a hard bound: a classified blocker recurring after
// maxOperationalBlockerRetries deferrals leaves the job terminally failed with a
// blocker_retries_exhausted event.
func TestE2E532BlockerRetryBudgetExhausted(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	script := `echo "HTTP 429 Too Many Requests: rate limit reached; try again in 1 seconds" 1>&2
exit 1`
	seedDaemonWorkerAgent(t, store, "capbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-cap", Agent: "capbot", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 5,
	})
	// Simulate a job that already spent its full auto-retry budget.
	job, payload := blockerE2EJobPayload(t, store, "job-cap")
	payload.BlockerAttempts = maxOperationalBlockerRetries
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload returned error: %v", err)
	}
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, _ = blockerE2EJobPayload(t, store, "job-cap")
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("exhausted-budget state = %q, want %q", job.State, workflow.JobFailed)
	}
	if !blockerE2EHasEventKind(t, store, "job-cap", blockerExhaustedEventKind) {
		t.Fatalf("missing %s job event", blockerExhaustedEventKind)
	}
	if blockerE2EHasEventKind(t, store, "job-cap", blockerDeferredEventKind) {
		t.Fatal("exhausted-budget job was deferred again")
	}
}
