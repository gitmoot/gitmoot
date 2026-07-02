package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// E2Es for #532 slices B–F, deterministic and LLM-free (the shell runtime), each
// driving the REAL daemon dispatch entry (runQueuedJobsForRepo /
// listPendingQueuedJobs → jobWorker.run → engine.RunJob → mailbox seam).

func blockerSliceContainsJob(jobs []db.Job, id string) bool {
	for _, j := range jobs {
		if j.ID == id {
			return true
		}
	}
	return false
}

// ---- Slice E: the deferral is PRE-TERMINAL (no job.failed precedes job.deferred).

// TestE2E532SliceEDeferralHasNoPrecedingJobFailed proves the mailbox seam defers a
// classified blocker BEFORE the terminal transition: the [events] sink sees a
// job.deferred and NEVER a job.failed for the run.
//
// MUTATION PROOF: revert slice E (let Mailbox.Run call m.fail then defer
// post-terminally, or drop the deferBlocker call) and a job.failed is emitted for
// this job — the "0 job.failed" assertion below flips RED.
func TestE2E532SliceEDeferralHasNoPrecedingJobFailed(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	countFile := filepath.Join(t.TempDir(), "count")
	script := fmt.Sprintf(`printf x >> %q
echo "HTTP 429 Too Many Requests: rate limit reached; try again in 30 seconds" 1>&2
exit 1`, countFile)
	seedDaemonWorkerAgent(t, store, "flapbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-flap", Agent: "flapbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	worker := blockerE2EWorker(store, home, checkout)
	sink := &recordingSink{}
	worker.EventSinkOverride = sink

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-flap")
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want %q (classification/pre-terminal regressed)", job.State, workflow.JobQueued)
	}
	if payload.BlockerClass != string(blockerClassRuntimeQuota) {
		t.Fatalf("blocker_class = %q, want %q", payload.BlockerClass, blockerClassRuntimeQuota)
	}
	// The whole point of slice E: NO job.failed for this job, ever.
	if failed := sink.byType(events.EventJobFailed); blockerSliceHasJob(failed, "job-flap") {
		t.Fatalf("job.failed was emitted for job-flap before the deferral — the failed→deferred flap is back")
	}
	// And the ordered stream for this job carries exactly one first-class deferral.
	ordered := sink.orderedForJob("job-flap")
	if len(ordered) != 1 || ordered[0] != events.EventJobDeferred {
		t.Fatalf("ordered events for job-flap = %v, want exactly [job.deferred]", ordered)
	}
}

func blockerSliceHasJob(evs []events.Event, id string) bool {
	for _, e := range evs {
		if e.JobID == id {
			return true
		}
	}
	return false
}

// ---- Slice C: checkout_contention (lock self-heals with short backoff; dirty
// surfaces a suggested_action).

// TestE2E532SliceCLockContentionDefersThenSucceeds proves a branch-lock
// pre-flight failure defers with a SHORT backoff (pre-terminally, no job.failed)
// and auto-succeeds once the lock clears.
//
// MUTATION PROOF: make classifyCheckoutContention return checkoutContentionNone
// for "is locked by" and the first dispatch terminally FAILS the job — the queued
// assertion flips RED.
func TestE2E532SliceCLockContentionDefersThenSucceeds(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	script := fmt.Sprintf("printf '%%s' '%s'", blockerE2EApprovedResult)
	seedDaemonWorkerAgent(t, store, "lockbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-lock", Agent: "lockbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	worker := blockerE2EWorker(store, home, checkout)
	sink := &recordingSink{}
	worker.EventSinkOverride = sink
	// The checkout is "locked by another worker" on the first pre-flight, then clears.
	var checkoutCalls int
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		checkoutCalls++
		if checkoutCalls == 1 {
			return "", fmt.Errorf("branch %s is locked by other-worker, not lockbot", "main")
		}
		return checkout, nil
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("first dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-lock")
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("state after lock contention = %q, want %q (terminal fail means slice C is broken)", job.State, workflow.JobQueued)
	}
	if payload.BlockerClass != string(blockerClassCheckoutContention) {
		t.Fatalf("blocker_class = %q, want %q", payload.BlockerClass, blockerClassCheckoutContention)
	}
	if payload.BlockerSuggestedAction != "" {
		t.Fatalf("lock contention set a suggested_action %q, want none (it self-heals)", payload.BlockerSuggestedAction)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil {
		t.Fatalf("parse retry-at: %v", err)
	}
	// SHORT exponential backoff: attempt 1 == checkoutLockBaseBackoff (2s), never minutes.
	if until := time.Until(retryAt); until <= 0 || until > checkoutLockBaseBackoff+time.Second {
		t.Fatalf("lock backoff %s not short (until=%s)", payload.BlockerRetryAt, until)
	}
	// Pre-terminal: no job.failed for this job.
	if blockerSliceHasJob(sink.byType(events.EventJobFailed), "job-lock") {
		t.Fatal("job.failed emitted for a lock-contention deferral (flap)")
	}
	if !blockerSliceHasJob(sink.byType(events.EventJobDeferred), "job-lock") {
		t.Fatal("no job.deferred emitted for the lock-contention deferral")
	}

	// After the short hold elapses, re-dispatch auto-succeeds.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
			t.Fatalf("re-dispatch returned error: %v", err)
		}
		job, payload = blockerE2EJobPayload(t, store, "job-lock")
		if job.State == string(workflow.JobSucceeded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never succeeded after the lock cleared; state=%q", job.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if payload.Result == nil || payload.Result.Decision != "approved" {
		t.Fatalf("stored result = %+v, want approved", payload.Result)
	}
}

// TestE2E532SliceCDirtyCheckoutSurfacesSuggestedAction proves a dirty checkout
// defers with a fixed (non-short) backoff AND a human-facing suggested_action that
// the #552 stuck surface renders.
func TestE2E532SliceCDirtyCheckoutSurfacesSuggestedAction(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "dirtybot", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-dirty", Agent: "dirtybot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	worker := blockerE2EWorker(store, home, checkout)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return "", fmt.Errorf("checkout %s has uncommitted changes", checkout)
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-dirty")
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("dirty checkout state = %q, want %q", job.State, workflow.JobQueued)
	}
	if payload.BlockerClass != string(blockerClassCheckoutContention) {
		t.Fatalf("blocker_class = %q, want %q", payload.BlockerClass, blockerClassCheckoutContention)
	}
	if strings.TrimSpace(payload.BlockerSuggestedAction) == "" {
		t.Fatal("dirty checkout did not persist a suggested_action")
	}
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil {
		t.Fatalf("parse retry-at: %v", err)
	}
	// Fixed (human-needing) backoff — minutes, not the short lock backoff.
	if until := time.Until(retryAt); until <= checkoutLockMaxBackoff {
		t.Fatalf("dirty backoff %s is too short (until=%s), want ~%s", payload.BlockerRetryAt, until, checkoutDirtyBackoff)
	}
	// The #552 stuck surface must carry the suggested_action.
	reason := loadStuckReason(store, job)
	if !strings.HasPrefix(reason.Reason, "blocked-operational: "+string(blockerClassCheckoutContention)) {
		t.Fatalf("stuck reason = %q, want checkout_contention prefix", reason.Reason)
	}
	if reason.SuggestedAction != payload.BlockerSuggestedAction {
		t.Fatalf("stuck suggested_action = %q, want %q", reason.SuggestedAction, payload.BlockerSuggestedAction)
	}
}

// ---- Slice D: a typed network / GitHub outage surfaced through the delivery seam
// defers as network_outage with a short backoff.

// MUTATION PROOF: remove the github.IsTransientMessage arm from
// classifyOperationalBlocker and the outage terminally FAILS the job.
func TestE2E532SliceDNetworkOutageDefers(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	countFile := filepath.Join(t.TempDir(), "count")
	script := fmt.Sprintf(`printf x >> %q
echo "fatal: unable to access 'https://github.com/owner/repo/': Could not resolve host: github.com" 1>&2
exit 1`, countFile)
	seedDaemonWorkerAgent(t, store, "netbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-net", Agent: "netbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch returned error: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-net")
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("network outage state = %q, want %q (terminal fail means slice D is broken)", job.State, workflow.JobQueued)
	}
	if payload.BlockerClass != string(blockerClassNetworkOutage) {
		t.Fatalf("blocker_class = %q, want %q", payload.BlockerClass, blockerClassNetworkOutage)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil {
		t.Fatalf("parse retry-at: %v", err)
	}
	// Short backoff for a self-clearing outage.
	if until := time.Until(retryAt); until <= 0 || until > networkBlockerRetryDelay+30*time.Second+time.Second {
		t.Fatalf("network backoff %s not short (until=%s)", payload.BlockerRetryAt, until)
	}
}

// ---- Slice B: runtime_auth re-dispatch is gated on a live probe.

// TestE2E532SliceBAuthReDispatchGatedOnProbe seeds a runtime_auth deferral whose
// coarse hold has already elapsed and drives listPendingQueuedJobs with a fake
// probe: an Invalid verdict keeps the job held (extending the hold, NOT burning an
// attempt); a Valid verdict releases it.
//
// MUTATION PROOF: drop the authProbeAllowsRedispatch gate from
// listPendingQueuedJobs and the job is eligible on the FIRST (Invalid) pass — the
// "not pending" assertion flips RED.
func TestE2E532SliceBAuthReDispatchGatedOnProbe(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "probebot", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-probe", Agent: "probebot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	// Seed as a runtime_auth deferral whose coarse hold already elapsed.
	seedElapsedAuthHold := func() {
		job, payload := blockerE2EJobPayload(t, store, "job-probe")
		payload.BlockerClass = string(blockerClassRuntimeAuth)
		payload.BlockerAttempts = 1
		payload.BlockerRetryAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
			t.Fatalf("UpdateJobPayload: %v", err)
		}
	}
	seedElapsedAuthHold()

	worker := blockerE2EWorker(store, home, checkout)
	verdict := authProbeInvalid
	probeCalls := 0
	worker.AuthProbe = func(context.Context, db.Job, workflow.JobPayload) authProbeVerdict {
		probeCalls++
		return verdict
	}

	// Phase 1: probe Invalid -> held, hold extended, attempt NOT burned.
	pending, err := listPendingQueuedJobs(ctx, worker, "", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs: %v", err)
	}
	if blockerSliceContainsJob(pending, "job-probe") {
		t.Fatal("auth job was dispatched while the live probe reports the credential Invalid")
	}
	if probeCalls == 0 {
		t.Fatal("the auth probe was never consulted")
	}
	_, p2 := blockerE2EJobPayload(t, store, "job-probe")
	if p2.BlockerAttempts != 1 {
		t.Fatalf("blocker_attempts = %d after a probe failure, want 1 (probe fail must not burn an attempt)", p2.BlockerAttempts)
	}
	if extended, err := time.Parse(time.RFC3339Nano, p2.BlockerRetryAt); err != nil || !extended.After(time.Now().UTC()) {
		t.Fatalf("hold was not extended after the probe failure: %q", p2.BlockerRetryAt)
	}

	// Phase 2: probe now Valid AND the hold re-elapsed -> the job is eligible.
	verdict = authProbeValid
	seedElapsedAuthHold()
	pending, err = listPendingQueuedJobs(ctx, worker, "", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs: %v", err)
	}
	if !blockerSliceContainsJob(pending, "job-probe") {
		t.Fatal("auth job was NOT released after the live probe reported the credential Valid")
	}
}

// ---- Slice F: the retry prompt carries a WARN-level prior-blocker line.

// TestE2E532SliceFRetryPromptCarriesWarnContext proves a re-dispatched job's
// prompt tells the agent the previous attempt died OPERATIONALLY (not on its own
// output). Driven end to end: a 429 defers the first delivery, the second captures
// the prompt.
//
// MUTATION PROOF: stop prepending blockerRetryReconciliationNotice in Mailbox.Run
// and the "did NOT fail on the merits" assertion flips RED.
func TestE2E532SliceFRetryPromptCarriesWarnContext(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	state := t.TempDir()
	marker := filepath.Join(state, "delivered")
	promptFile := filepath.Join(state, "retry-prompt")
	script := fmt.Sprintf(`if [ ! -f %q ]; then
  : > %q
  echo "HTTP 429 Too Many Requests: rate limit reached; try again in 3 seconds" 1>&2
  exit 1
fi
printf '%%s' "$1" > %q
printf '%%s' '%s'`, marker, marker, promptFile, blockerE2EApprovedResult)
	seedDaemonWorkerAgent(t, store, "warnbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-warn", Agent: "warnbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	worker := blockerE2EWorker(store, home, checkout)

	// Defer, then wait out the (floored 5s) hold and succeed.
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	_, payload := blockerE2EJobPayload(t, store, "job-warn")
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil {
		t.Fatalf("parse retry-at: %v", err)
	}
	for time.Now().UTC().Before(retryAt) {
		time.Sleep(50 * time.Millisecond)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
			t.Fatalf("re-dispatch: %v", err)
		}
		job, _ := blockerE2EJobPayload(t, store, "job-warn")
		if job.State == string(workflow.JobSucceeded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never succeeded; state=%q", job.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read retry prompt: %v", err)
	}
	text := string(prompt)
	// WARN level (slice F): died operationally, not on its own output.
	if !strings.Contains(text, "did NOT fail on the merits") ||
		!strings.Contains(text, "operational blocker (runtime_quota)") {
		t.Fatalf("retry prompt is missing the slice F warn-level prior-blocker context:\n%s", text)
	}
	// Composed with the at-least-once reconciliation notice.
	if !strings.Contains(text, "reconcile") {
		t.Fatalf("retry prompt lost the reconciliation notice:\n%s", text)
	}
}

// TestE2E532CheckoutContentionRetrySuppressesReconciliation proves the slice-F
// reconciliation notice is NOT prepended when the prior deferral was PRE-DELIVERY —
// a checkout_contention hold trips at the daemon pre-flight, BEFORE the agent is
// ever delivered a prompt, so the re-dispatch is a FIRST run with no prior side
// effects to reconcile. Telling the agent otherwise would send it hunting for
// non-existent artifacts (or mis-reconciling its own PR branch).
//
// MUTATION PROOF: drop `&& !payload.BlockerPreDelivery` from the Mailbox.Run gate
// (or the BlockerPreDelivery=true in deferCheckoutContention) and the "must NOT
// contain" assertions flip RED.
func TestE2E532CheckoutContentionRetrySuppressesReconciliation(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	state := t.TempDir()
	promptFile := filepath.Join(state, "delivered-prompt")
	// Capture the delivered prompt and succeed on the first (and only) delivery.
	script := fmt.Sprintf(`printf '%%s' "$1" > %q
printf '%%s' '%s'`, promptFile, blockerE2EApprovedResult)
	seedDaemonWorkerAgent(t, store, "checkoutbot", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-checkout", Agent: "checkoutbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})

	// Seed a PRE-DELIVERY checkout_contention deferral whose hold has already elapsed
	// (the daemon pre-flight would have set exactly these fields; the agent never ran).
	job, payload := blockerE2EJobPayload(t, store, "job-checkout")
	payload.BlockerClass = string(blockerClassCheckoutContention)
	payload.BlockerAttempts = 1
	payload.BlockerRetryAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	payload.BlockerPreDelivery = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}

	worker := blockerE2EWorker(store, home, checkout)
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, p := blockerE2EJobPayload(t, store, "job-checkout"); p.Result == nil || p.Result.Decision != "approved" {
		t.Fatalf("job did not run to an approved result: %+v", p.Result)
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read delivered prompt: %v", err)
	}
	text := string(prompt)
	// The pre-delivery retry must NOT carry any at-least-once reconciliation framing:
	// there was no prior attempt and no side effects to reconcile.
	if strings.Contains(text, "did NOT fail on the merits") ||
		strings.Contains(text, "operational blocker (checkout_contention)") ||
		strings.Contains(text, "pushed branches") {
		t.Fatalf("a pre-delivery checkout_contention retry wrongly carried the slice-F reconciliation notice:\n%s", text)
	}
}

// TestAuthProbeDedupedAndCadenceMarkedPerPass proves two defects from the slice-B
// review are fixed at once: (1) the live credential probe is computed AT MOST ONCE
// per listPendingQueuedJobs pass and shared across every auth-held job of the same
// effective runtime (the ambient-token verdict is identical for all of them), and
// (2) a probe-cadence marker is written on EVERY verdict — here a VALID one — so a
// job that probed this cadence is re-held and NOT re-probed on the next pass.
//
// MUTATION PROOF: remove the authProbeCache dedup and probeCalls jumps to 2; drop
// the extendAuthBlockerHold call on the Valid branch and BlockerRetryAt stays in the
// past so the "re-held into the future" assertion flips RED.
func TestAuthProbeDedupedAndCadenceMarkedPerPass(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	// Two DISTINCT agents that share one runtime: the ambient-token probe is identical
	// for both, so they must collapse to a single live probe.
	seedDaemonWorkerAgent(t, store, "authbot-a", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "authbot-b", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	for _, spec := range []struct{ id, agent string }{{"job-auth-a", "authbot-a"}, {"job-auth-b", "authbot-b"}} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: spec.id, Agent: spec.agent, Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
		})
		job, payload := blockerE2EJobPayload(t, store, spec.id)
		payload.BlockerClass = string(blockerClassRuntimeAuth)
		payload.BlockerAttempts = 1
		payload.BlockerRetryAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
			t.Fatalf("UpdateJobPayload: %v", err)
		}
	}

	worker := blockerE2EWorker(store, home, checkout)
	probeCalls := 0
	worker.AuthProbe = func(context.Context, db.Job, workflow.JobPayload) authProbeVerdict {
		probeCalls++
		return authProbeValid
	}

	pending, err := listPendingQueuedJobs(ctx, worker, "", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs: %v", err)
	}
	if probeCalls != 1 {
		t.Fatalf("probe ran %d times for two auth-held jobs of one runtime; want 1 (deduped)", probeCalls)
	}
	if !blockerSliceContainsJob(pending, "job-auth-a") || !blockerSliceContainsJob(pending, "job-auth-b") {
		t.Fatalf("both auth jobs should be pending after a Valid probe; got %d", len(pending))
	}
	// Cadence marker: both jobs' holds were pushed into the future, so a subsequent
	// pass finds them held and does NOT re-probe.
	for _, id := range []string{"job-auth-a", "job-auth-b"} {
		_, p := blockerE2EJobPayload(t, store, id)
		at, err := time.Parse(time.RFC3339Nano, p.BlockerRetryAt)
		if err != nil || !at.After(time.Now().UTC()) {
			t.Fatalf("%s hold was not re-armed after the probe: %q", id, p.BlockerRetryAt)
		}
	}
	probeCalls = 0
	pending2, err := listPendingQueuedJobs(ctx, worker, "", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs (second pass): %v", err)
	}
	if probeCalls != 0 {
		t.Fatalf("re-probed on the next pass despite an in-window cadence marker (probeCalls=%d)", probeCalls)
	}
	if blockerSliceContainsJob(pending2, "job-auth-a") || blockerSliceContainsJob(pending2, "job-auth-b") {
		t.Fatal("a job whose cadence marker was just re-armed should be held on the next pass")
	}
}

// TestAuthProbeSkippedOnWarningCountPath proves finding #2: the live credential
// probe must NOT run — and the job payload must NOT be mutated — when the queued
// listing is consumed by the preflight serialization-warning count (forDispatch
// false), only when a caller is truly about to dispatch. Otherwise a claude-runtime
// outage would spawn a `claude -p` subprocess and rewrite BlockerRetryAt on every
// scheduler tick from a pure read path.
//
// MUTATION PROOF: make listPendingQueuedJobs always probe (ignore forDispatch) and
// both the "probe never ran" and "payload untouched" assertions flip RED.
func TestAuthProbeSkippedOnWarningCountPath(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "warnauthbot", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-warnauth", Agent: "warnauthbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	job, payload := blockerE2EJobPayload(t, store, "job-warnauth")
	payload.BlockerClass = string(blockerClassRuntimeAuth)
	payload.BlockerAttempts = 1
	elapsed := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	payload.BlockerRetryAt = elapsed
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}

	worker := blockerE2EWorker(store, home, checkout)
	probed := false
	worker.AuthProbe = func(context.Context, db.Job, workflow.JobPayload) authProbeVerdict {
		probed = true
		return authProbeInvalid
	}

	// The warning-count path (forDispatch=false) must never probe or mutate.
	if _, err := listPendingQueuedJobs(ctx, worker, "", "", false); err != nil {
		t.Fatalf("listPendingQueuedJobs: %v", err)
	}
	if probed {
		t.Fatal("the live auth probe ran on the warning-count (non-dispatch) path")
	}
	if _, p := blockerE2EJobPayload(t, store, "job-warnauth"); p.BlockerRetryAt != elapsed {
		t.Fatalf("the warning-count path mutated BlockerRetryAt: %q -> %q", elapsed, p.BlockerRetryAt)
	}
}
