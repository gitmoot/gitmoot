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

// unknownModelFailure is omp's measured refusal when a pool model is missing
// from the remote catalog (herdrup#589, data-core#29).
const unknownModelFailure = "omp turn failed (stopReason error): Unknown model: openai-codex/gpt-6.1-sol: remote command exited with code 1: exit status 1"

// quotedRefusalCodeBeforeColon is the round-2 reviewer's probe: a Codex error
// whose message quotes the refusal code right before a colon, while the
// provider's real code is a different one.
const quotedRefusalCodeBeforeColon = "omp turn failed (stopReason error): Codex error event: The submitted fixture contains (code=cyber_policy): the marker is not valid in this field. (code=invalid_request_error): exit status 1"

// The round-3 reviewer's probes: omp keeps newlines inside one provider
// message, so a newline after a quoted code is not the end of the error, and a
// genuine refusal may carry a multi-line message.
const (
	quotedRefusalCodeBeforeNewline = "omp turn failed (stopReason error): Codex error event: The submitted fixture contains (code=cyber_policy)\nthe marker is not valid in this field. (code=invalid_request_error): exit status 1"
	multilineRefusal               = "omp turn failed (stopReason error): Codex error event: This content was flagged for possible cybersecurity risk.\nIf this seems wrong, try rephrasing your request. (code=cyber_policy): remote command exited with code 1: exit status 1"
)

// codexModelNotFound is verbatim from job
// local-review-review-router-18eb712586553bda-1 (2026-11-26), one of 13 routed
// reviews in 8 days that failed on it with fallback models behind them.
const codexModelNotFound = "omp turn failed (stopReason error): Codex error event: The 'gpt-6.2-sol' model is not supported when using Codex with a ChatGPT account. (code=model_not_found): exit status 1"

func TestProviderModelNotFoundClassifiesAsModelUnavailable(t *testing.T) {
	const notFound = "omp turn failed (stopReason error): Codex error event: The 'gpt-6-sol' model is not supported when using Codex with a ChatGPT account. (code=model_not_found)"
	// The live suffix shapes across all 13 live failures.
	for _, text := range []string{
		codexModelNotFound,
		notFound,
		notFound + ": remote command exited with code 1: exit status 1",
	} {
		got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, time.Now())
		if !ok || got.Class != blockerClassModelUnavailable || !got.RetryAt.IsZero() {
			t.Fatalf("classifyOperationalBlocker(%q) = %+v, %v; want class %s with zero RetryAt", text, got, ok, blockerClassModelUnavailable)
		}
	}
	// A quoted code is not the provider's, and the two codes keep their classes.
	quoted := "omp turn failed (stopReason error): Codex error event: the fixture names (code=model_not_found)\nwhile the provider says (code=invalid_request_error): exit status 1"
	if got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(quoted)}, time.Now()); ok && got.Class == blockerClassModelUnavailable {
		t.Fatalf("classifyOperationalBlocker(%q) = %s, want no model_unavailable", quoted, got.Class)
	}
	if got, _ := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(codexCyberPolicyRefusal)}, time.Now()); got.Class != blockerClassModelRefused {
		t.Fatalf("cyber_policy classified %s, want %s", got.Class, blockerClassModelRefused)
	}
}

func TestProviderContentRefusalClassifiesAsModelRefused(t *testing.T) {
	const flagged = "omp turn failed (stopReason error): Codex error event: This content was flagged for possible cybersecurity risk. (code=cyber_policy)"
	// The live suffix shapes (all 20 live failures), plus a multi-line message.
	for _, text := range []string{
		codexCyberPolicyRefusal,
		flagged,
		flagged + ": remote command exited with code 1: exit status 1",
		flagged + "\n",
		multilineRefusal,
	} {
		got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, time.Now())
		if !ok || got.Class != blockerClassModelRefused {
			t.Fatalf("classifyOperationalBlocker(%q) = %+v, %v; want class %s", text, got, ok, blockerClassModelRefused)
		}
		if !got.RetryAt.IsZero() {
			t.Fatalf("RetryAt = %v, want zero: a classifier verdict does not expire on a timer", got.RetryAt)
		}
	}
	// Quoting the code is not the provider refusing the content.
	for _, text := range []string{
		"delivery failed: the reviewer flagged cyber_policy handling as untested",
		`delivery failed: {"error":{"message":"The submitted fixture contains (code=cyber_policy)","code":"invalid_request_error"}}`,
		"omp turn failed (stopReason error): Codex error event: the fixture quotes (code=cyber_policy) mid-sentence and goes on",
		"omp turn failed (stopReason error): Codex error event: flagged (diagnostic-code=cyber_policy)",
		"omp turn failed (stopReason error): Codex error event: flagged (code=cyber_policy-preview-v2)",
		quotedRefusalCodeBeforeColon,
		quotedRefusalCodeBeforeNewline,
	} {
		if got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, time.Now()); ok && got.Class == blockerClassModelRefused {
			t.Fatalf("classifyOperationalBlocker(%q) = %s, want no model_refused", text, got.Class)
		}
	}
}

// #2469 and #2750. An unknown model and a content refusal are facts about one
// pool entry: a routed review moves to the next entry immediately; with
// nothing left, or no pool at all, it stays failed, because waiting changes
// neither omp's catalog nor the provider's verdict.
func TestPerModelFailureAdvancesReviewPoolOrStaysTerminal(t *testing.T) {
	type outcome struct {
		deferred         bool
		state, model     string
		class            string
		fallbacks, holds int
	}
	run := func(t *testing.T, failure, current string, pool []string) outcome {
		t.Helper()
		ctx := context.Background()
		store, home := blockerE2EHome(t)
		checkout := t.TempDir()
		seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
		seedDaemonWorkerAgent(t, store, "omp-reviewer", runtime.OmpRuntime, "true", []string{"review"}, "owner/repo")
		request := poolReviewRequest("job-per-model", "omp-reviewer")
		request.ReviewModelPool = pool
		request.Model = current
		remote := "remote"
		request.ExecBackend = &remote
		enqueueDaemonWorkerJob(t, store, request)
		if _, err := store.TransitionJobState(ctx, request.ID, string(workflow.JobQueued), string(workflow.JobRunning)); err != nil {
			t.Fatalf("TransitionJobState: %v", err)
		}
		worker := blockerE2EWorker(store, home, checkout)
		deferred, err := worker.deferOperationalBlockerPreTerminal(ctx, request.ID, workflow.DeliveryError{Err: errors.New(failure)})
		if err != nil {
			t.Fatalf("deferOperationalBlockerPreTerminal: %v", err)
		}
		job, payload := blockerE2EJobPayload(t, store, request.ID)
		events, err := store.ListJobEvents(ctx, request.ID)
		if err != nil {
			t.Fatal(err)
		}
		return outcome{deferred, job.State, payload.Model, payload.BlockerClass,
			len(eventsOfKind(events, reviewModelFallbackEventKind)), len(eventsOfKind(events, blockerDeferredEventKind))}
	}
	for _, tc := range []struct {
		name, failure string
		class         blockerClass
	}{
		{"unknown model", unknownModelFailure, blockerClassModelUnavailable},
		{"content refusal", codexCyberPolicyRefusal, blockerClassModelRefused},
		{"provider model not found", codexModelNotFound, blockerClassModelUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("next pool entry", func(t *testing.T) {
				got := run(t, tc.failure, "openai-codex/gpt-6.1-sol", []string{"openai-codex/gpt-6.1-sol", "kimi-code/k3"})
				if !got.deferred || got.state != string(workflow.JobQueued) || got.model != "kimi-code/k3" || got.class != string(tc.class) || got.fallbacks != 1 {
					t.Fatalf("got %+v, want queued on kimi-code/k3 as %s with one fallback event", got, tc.class)
				}
			})
			for name, pool := range map[string][]string{
				"exhausted pool": {"openai-codex/gpt-6.1-sol"},
				"no pool":        nil,
			} {
				t.Run(name+" stays terminal", func(t *testing.T) {
					got := run(t, tc.failure, "openai-codex/gpt-6.1-sol", pool)
					if got.deferred || got.state != string(workflow.JobRunning) || got.class != "" || got.holds != 0 {
						t.Fatalf("got %+v, want the failure left to the terminal path with no timed hold", got)
					}
				})
			}
		})
	}
	// The reviewers' probes: an unrelated provider error that quotes the code
	// must not move the review to another model.
	for name, failure := range map[string]string{
		"quoted in JSON":        `delivery failed: {"error":{"message":"The submitted fixture contains (code=cyber_policy)","code":"invalid_request_error"}}`,
		"quoted before colon":   quotedRefusalCodeBeforeColon,
		"quoted before newline": quotedRefusalCodeBeforeNewline,
	} {
		t.Run(name+" does not fall back", func(t *testing.T) {
			got := run(t, failure, "openai-codex/gpt-6.1-sol", []string{"openai-codex/gpt-6.1-sol", "kimi-code/k3"})
			if got.model != "openai-codex/gpt-6.1-sol" || got.fallbacks != 0 {
				t.Fatalf("got %+v, want no pool advance", got)
			}
		})
	}
}
