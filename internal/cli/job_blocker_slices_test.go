package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// ---- Slice C: checkout-contention classifier + child exclusion.

func TestClassifyCheckoutContention(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantKind   checkoutContentionKind
		wantAction bool
	}{
		{"branch lock self-heals", errors.New("branch main is locked by other-worker, not me"), checkoutContentionLock, false},
		{"dirty checkout needs a human", errors.New("checkout /x has uncommitted changes"), checkoutContentionDirty, true},
		{"wrong head needs a human", errors.New("checkout head is abc, not review job head def"), checkoutContentionDirty, true},
		{"wrong branch needs a human", errors.New("checkout branch is main, not job branch feat/x"), checkoutContentionDirty, true},
		{"unrelated error is not contention", errors.New("adapter factory failed"), checkoutContentionNone, false},
		{"nil error", nil, checkoutContentionNone, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, action := classifyCheckoutContention(tc.err)
			if kind != tc.wantKind {
				t.Fatalf("kind = %d, want %d", kind, tc.wantKind)
			}
			if (action != "") != tc.wantAction {
				t.Fatalf("action = %q, wantNonEmpty=%v", action, tc.wantAction)
			}
		})
	}
}

func TestCheckoutLockBackoffIsShortExponential(t *testing.T) {
	if got := checkoutLockBackoff(1); got != checkoutLockBaseBackoff {
		t.Fatalf("attempt 1 backoff = %s, want %s", got, checkoutLockBaseBackoff)
	}
	if got := checkoutLockBackoff(2); got != 2*checkoutLockBaseBackoff {
		t.Fatalf("attempt 2 backoff = %s, want %s", got, 2*checkoutLockBaseBackoff)
	}
	if got := checkoutLockBackoff(3); got != 4*checkoutLockBaseBackoff {
		t.Fatalf("attempt 3 backoff = %s, want %s", got, 4*checkoutLockBaseBackoff)
	}
	// Always short — never near the dirty (minutes) backoff.
	if checkoutLockBackoff(3) >= checkoutDirtyBackoff {
		t.Fatal("lock backoff grew into the dirty-checkout range")
	}
	if got := checkoutLockBackoff(20); got != checkoutLockMaxBackoff {
		t.Fatalf("large attempt backoff = %s, want clamp %s", got, checkoutLockMaxBackoff)
	}
}

// A delegation child must never be diverted by slice C: deferCheckoutContention
// returns false so the caller's existing DAG routing (finishQueuedJob) runs.
func TestDeferCheckoutContentionExcludesDelegationChild(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "childbot", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "child-job", Agent: "childbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
		ParentJobID: "parent-1", DelegationID: "deleg-1",
	})
	job, err := store.GetJob(ctx, "child-job")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard, t.TempDir())
	deferred, err := worker.deferCheckoutContention(ctx, job, payload, errors.New("checkout /x has uncommitted changes"))
	if err != nil {
		t.Fatalf("deferCheckoutContention: %v", err)
	}
	if deferred {
		t.Fatal("slice C deferred a delegation child; it must keep DAG routing")
	}
	reloaded, _ := store.GetJob(ctx, "child-job")
	p, _ := daemonJobPayload(reloaded)
	if p.BlockerClass != "" {
		t.Fatalf("delegation child acquired a blocker class: %+v", p)
	}
}

// The shared 3-attempt budget bounds checkout contention too: a job already at the
// cap is not deferred again (it records the exhausted event and falls through).
func TestDeferCheckoutContentionRespectsBudget(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "capbot", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "cap-job", Agent: "capbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	job, _ := store.GetJob(ctx, "cap-job")
	payload, _ := daemonJobPayload(job)
	payload.BlockerAttempts = maxOperationalBlockerRetries
	encoded, _ := json.Marshal(payload)
	if err := store.UpdateJobPayload(ctx, "cap-job", string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
	job, _ = store.GetJob(ctx, "cap-job")
	payload, _ = daemonJobPayload(job)
	worker := defaultJobWorker(store, io.Discard, t.TempDir())
	deferred, err := worker.deferCheckoutContention(ctx, job, payload, errors.New("branch main is locked by other"))
	if err != nil {
		t.Fatalf("deferCheckoutContention: %v", err)
	}
	if deferred {
		t.Fatal("checkout contention deferred past the retry budget")
	}
	if !blockerE2EHasEventKind(t, store, "cap-job", blockerExhaustedEventKind) {
		t.Fatal("budget-exhausted checkout contention did not record the exhausted event")
	}
}

// ---- Slice D: network_outage classification (typed marker + delivery signature).

func TestClassifyOperationalBlockerNetworkOutage(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	t.Run("typed github TransientError classifies without the delivery marker", func(t *testing.T) {
		cause := github.TransientError{Err: errors.New("connection refused")}
		got, ok := classifyOperationalBlocker(cause, now)
		if !ok || got.Class != blockerClassNetworkOutage {
			t.Fatalf("classify = (%+v, %v), want network_outage", got, ok)
		}
	})
	t.Run("delivery-seam outage signature classifies", func(t *testing.T) {
		cause := workflow.DeliveryError{Err: errors.New("fatal: unable to access: Could not resolve host: github.com: exit status 128")}
		got, ok := classifyOperationalBlocker(cause, now)
		if !ok || got.Class != blockerClassNetworkOutage {
			t.Fatalf("classify = (%+v, %v), want network_outage", got, ok)
		}
	})
	t.Run("bare outage text WITHOUT the delivery marker never classifies", func(t *testing.T) {
		// Agent-authored text mentioning a network problem is a product failure.
		if _, ok := classifyOperationalBlocker(errors.New("summary: the service was unavailable"), now); ok {
			t.Fatal("agent-authored network text classified without the DeliveryError marker")
		}
	})
	t.Run("auth/quota still win over network on the same text", func(t *testing.T) {
		cause := workflow.DeliveryError{Err: errors.New("HTTP 503 while checking usage limit; rate limit reached")}
		got, ok := classifyOperationalBlocker(cause, now)
		if !ok || got.Class != blockerClassRuntimeQuota {
			t.Fatalf("classify = (%+v, %v), want runtime_quota (more specific than network)", got, ok)
		}
	})
}

// ---- Slice B: probe verdict mapping.

func TestClassifyClaudeAuthProbe(t *testing.T) {
	if got := classifyClaudeAuthProbe(nil); got != authProbeValid {
		t.Fatalf("nil probe err = %d, want valid", got)
	}
	if got := classifyClaudeAuthProbe(errors.New("some network blip")); got != authProbeUnknown {
		t.Fatalf("transient probe err = %d, want unknown", got)
	}
	authErr := errors.Join(errors.New("rejected"), runtime.ErrClaudeAuthFailed)
	if got := classifyClaudeAuthProbe(authErr); got != authProbeInvalid {
		t.Fatalf("auth-failed probe err = %d, want invalid", got)
	}
}

// A non-auth deferral is never probe-gated (the gate is only for runtime_auth).
func TestAuthProbeAllowsRedispatchOnlyGatesAuth(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "qbot", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "quota-job", Agent: "qbot", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	job, _ := store.GetJob(ctx, "quota-job")
	payload, _ := daemonJobPayload(job)
	payload.BlockerClass = string(blockerClassRuntimeQuota)
	encoded, _ := json.Marshal(payload)
	_ = store.UpdateJobPayload(ctx, "quota-job", string(encoded))
	job, _ = store.GetJob(ctx, "quota-job")

	worker := defaultJobWorker(store, io.Discard, t.TempDir())
	probed := false
	worker.AuthProbe = func(context.Context, db.Job, workflow.JobPayload) authProbeVerdict {
		probed = true
		return authProbeInvalid
	}
	if !authProbeAllowsRedispatch(ctx, worker, job, time.Now().UTC()) {
		t.Fatal("a runtime_quota deferral was probe-gated; only runtime_auth should be")
	}
	if probed {
		t.Fatal("the auth probe ran for a non-auth deferral")
	}
}
