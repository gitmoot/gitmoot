package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Verbatim from six routed reviews that failed between 2026-11-17 and
// 2026-11-24 with fallback models behind them (e.g. job
// local-review-review-router-18eadb40299aaab5-1).
const codexAtCapacity = "delivery failed: omp turn failed (stopReason error): Codex error event: Codex is at capacity, please try again later (code=turn_rate_limit_exceeded) retry-after-ms=417000: exit status 1"

// A provider at capacity is a throttle: the routed review must fall through to
// its next model instead of ending review_failed.
func TestProviderCapacityClassifiesAsRuntimeQuota(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"codex at capacity, verbatim live text", codexAtCapacity},
		{"bare openai error code", `{"error":{"code":"rate_limit_exceeded","message":"Rate exceeded"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAuthQuotaStrict(tc.text); got != "throttled" {
				t.Fatalf("classifyAuthQuotaStrict = %q, want \"throttled\" so the pool falls through", got)
			}
			if got := classifyAuthQuota(tc.text); got != "throttled" {
				t.Fatalf("classifyAuthQuota (stuck-reason label) = %q, want \"throttled\": the label and the routing decision must agree", got)
			}
		})
	}
	got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(codexAtCapacity)}, time.Now())
	if !ok || got.Class != blockerClassRuntimeQuota {
		t.Fatalf("classifyOperationalBlocker = %+v, %v; want class %s", got, ok, blockerClassRuntimeQuota)
	}
}

// Reviews discuss capacity and rate limiting as subject matter; none of this
// may park a review as a provider outage.
func TestProviderCapacityPhrasesDoNotMatchReviewProse(t *testing.T) {
	for _, text := range []string{
		"the worker pool is at capacity under load",
		"P2: the queue is at capacity and drops jobs",
		"the handler returns RateLimitExceeded without a retry hint",
		"rename rate_limit_exceeded_total to follow the metric naming convention",
		"please try again later is shown to users without a retry button",
		// #2749 review probes: a mention of the code, or the provider's sentence
		// quoted, is not the provider's error-code field.
		"P2: the handler drops the rate_limit_exceeded response",
		"delivery failed: plan validation: step 3 quotes \"at capacity, please try again later\"",
		"delivery failed: retry when the upstream returns rate_limit_exceeded",
	} {
		t.Run(text, func(t *testing.T) {
			if got := classifyAuthQuotaStrict(text); got != "" {
				t.Fatalf("classifyAuthQuotaStrict(%q) = %q, want \"\"", text, got)
			}
			if got := classifyAuthQuota(text); got != "" {
				t.Fatalf("classifyAuthQuota(%q) = %q, want \"\"", text, got)
			}
		})
	}
}

// An unknown model whose name ends in the code is model_unavailable, not a
// throttle: the first cut of #2749 let the capacity match claim it first.
func TestUnknownModelNamedLikeTheCapacityCodeStaysModelUnavailable(t *testing.T) {
	text := "delivery failed: omp turn failed: unknown model: acme/turn_rate_limit_exceeded: exit status 1"
	got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, time.Now())
	if !ok || got.Class != blockerClassModelUnavailable {
		t.Fatalf("classifyOperationalBlocker = %+v, %v; want class %s", got, ok, blockerClassModelUnavailable)
	}
}
