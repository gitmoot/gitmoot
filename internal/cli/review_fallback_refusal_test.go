package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Verbatim from job local-review-review-router-18eae3570cba6257-1 (2026-11-24),
// one of 18 routed reviews in 30 days that failed on it with fallback models
// behind them.
const codexCyberPolicyRefusal = "delivery failed: omp turn failed (stopReason error): Codex error event: This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. If you’re doing authorized security work that requires more cyber permissive safeguards, apply for Daybreak access via https://platform.openai.com/settings/organization/status-and-access before retrying. (code=cyber_policy): exit status 1"

func TestProviderContentRefusalClassifiesAsModelRefused(t *testing.T) {
	got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(codexCyberPolicyRefusal)}, time.Now())
	if !ok || got.Class != blockerClassModelRefused {
		t.Fatalf("classifyOperationalBlocker = %+v, %v; want class %s", got, ok, blockerClassModelRefused)
	}
	if !got.RetryAt.IsZero() {
		t.Fatalf("RetryAt = %v, want zero: a classifier verdict does not expire on a timer", got.RetryAt)
	}
	// Naming the code is not the provider's error-code field.
	for _, text := range []string{
		"delivery failed: the reviewer flagged cyber_policy handling as untested",
		"delivery failed: plan validation: step 2 mentions (cyber_policy) in prose",
	} {
		if got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, time.Now()); ok && got.Class == blockerClassModelRefused {
			t.Fatalf("classifyOperationalBlocker(%q) = %s, want no model_refused", text, got.Class)
		}
	}
}

// A content refusal is a fact about one pool entry: a routed review moves to
// the next entry immediately; with nothing left it stays failed, because
// waiting does not change the classifier's verdict.
func TestContentRefusalAdvancesReviewPoolOrStaysTerminal(t *testing.T) {
	run := func(t *testing.T, pool []string) (bool, string, string, string, int, int) {
		t.Helper()
		ctx := context.Background()
		store, home := blockerE2EHome(t)
		checkout := t.TempDir()
		seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
		seedDaemonWorkerAgent(t, store, "omp-reviewer", runtime.OmpRuntime, "true", []string{"review"}, "owner/repo")
		request := poolReviewRequest("job-refusal", "omp-reviewer")
		request.ReviewModelPool = pool
		request.Model = pool[0]
		enqueueDaemonWorkerJob(t, store, request)
		if _, err := store.TransitionJobState(ctx, request.ID, string(workflow.JobQueued), string(workflow.JobRunning)); err != nil {
			t.Fatalf("TransitionJobState: %v", err)
		}
		worker := blockerE2EWorker(store, home, checkout)
		deferred, err := worker.deferOperationalBlockerPreTerminal(ctx, request.ID, workflow.DeliveryError{Err: errors.New(codexCyberPolicyRefusal)})
		if err != nil {
			t.Fatalf("deferOperationalBlockerPreTerminal: %v", err)
		}
		job, payload := blockerE2EJobPayload(t, store, request.ID)
		events, err := store.ListJobEvents(ctx, request.ID)
		if err != nil {
			t.Fatal(err)
		}
		return deferred, job.State, payload.Model, payload.BlockerClass,
			len(eventsOfKind(events, reviewModelFallbackEventKind)), len(eventsOfKind(events, blockerDeferredEventKind))
	}
	t.Run("next pool entry", func(t *testing.T) {
		deferred, state, model, class, fallbacks, _ := run(t, []string{"openai-codex/gpt-6.2-sol", "anthropic/claude-opus-5-5"})
		if !deferred || state != string(workflow.JobQueued) || model != "anthropic/claude-opus-5-5" || class != string(blockerClassModelRefused) || fallbacks != 1 {
			t.Fatalf("deferred=%v state=%s model=%q class=%q fallbacks=%d, want queued on anthropic/claude-opus-5-5 as model_refused with one fallback event",
				deferred, state, model, class, fallbacks)
		}
	})
	t.Run("exhausted pool stays terminal", func(t *testing.T) {
		deferred, state, _, class, _, holds := run(t, []string{"openai-codex/gpt-6.2-sol"})
		if deferred || state != string(workflow.JobRunning) || class != "" || holds != 0 {
			t.Fatalf("deferred=%v state=%s class=%q holds=%d, want the failure left to the terminal path with no timed hold", deferred, state, class, holds)
		}
	})
}
