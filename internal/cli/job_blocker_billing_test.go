package cli

import "testing"

// The live outages this is written against. Each string is verbatim provider
// output captured from a failed job, except where noted as provider wording
// reported by independent review of #2254.
var (
	xaiCreditsExhausted = "delivery failed: omp turn failed (stopReason error, errorStatus 403): 403 You have run out of credits or need a Grok subscription. Add credits at https://grok.com/?_s=usage or upgrade at https://grok.com/supergrok."
	claudeEntitlement   = "delivery failed: claude: Your organization has disabled Claude subscription access for Claude Code \u00b7 Use an Anthropic API key instead, or ask your admin to enable access (api_error_status 403): exit status 1"
)

// An account that cannot pay is the same operational fact as a throttle: this
// provider cannot serve the job, another can. Two review jobs died on the xAI
// wording with a healthy pool behind them because it matched nothing.
func TestBillingExhaustionClassifiesAsThrottled(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"xai subscription exhausted, verbatim live text", xaiCreditsExhausted},
		{"anthropic credit balance", "400 Your credit balance is too low to access the Anthropic API."},
		{"deepseek-style insufficient balance", "HTTP 402 Insufficient Balance"},
		{"payment required", "HTTP 402 payment required"},
		{"insufficient credits", "provider error: insufficient credits for this request"},
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
}

// A revoked entitlement is not a balance: nothing resets it on a timer, only an
// admin action does. The first cut classified it throttled, which gave it the
// quota hold and told the operator to wait for a reset that does not exist. It
// is an auth failure. The pool advances on runtime_auth exactly as on
// runtime_quota, so the job still falls through.
func TestEntitlementRevocationClassifiesAsAuth(t *testing.T) {
	if got := classifyAuthQuotaStrict(claudeEntitlement); got != "auth failing" {
		t.Fatalf("classifyAuthQuotaStrict = %q, want \"auth failing\"", got)
	}
	if got := classifyAuthQuota(claudeEntitlement); got != "auth failing" {
		t.Fatalf("classifyAuthQuota = %q, want \"auth failing\"", got)
	}
}

// This classifier reads agent-authored text, and reviews discuss billing,
// credits, subscriptions and upgrades as SUBJECT MATTER. The first cut of this
// change matched bare `billing` and `upgrade to`; independent review executed it
// and both of the first two strings below classified as an account outage,
// which would have parked a review as a provider failure because it criticised
// a billing module. These are the regression.
func TestProviderPhrasesDoNotMatchReviewProse(t *testing.T) {
	for _, text := range []string{
		"the billing module is untested",
		"upgrade to the new API before merging",
		"P2: the billing webhook drops retries",
		"the review found that credits are never refunded on cancel",
		"P2: subscription renewal path lacks a test",
		"summary: upgrade the dependency to v2",
		"consider asking your admin about this design",
		"exit status 1",
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

// 403 is the ordinary "you may not do that" status. A permission refusal
// classified as quota or auth would be deferred against a wall that never opens.
func TestPlain403IsNotQuota(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"github permission refusal", "delivery failed: HTTP 403: Resource not accessible by integration", ""},
		{"gateway allowlist refusal", `delivery failed: model gateway upstream host "api.anthropic.com" is not allowlisted`, ""},
		{"401 keeps its auth class", "delivery failed: HTTP 401 unauthorized", "auth failing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAuthQuotaStrict(tc.text); got != tc.want {
				t.Fatalf("classifyAuthQuotaStrict = %q, want %q", got, tc.want)
			}
		})
	}
}
