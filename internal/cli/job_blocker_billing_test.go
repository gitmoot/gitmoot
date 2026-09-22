package cli

import "testing"

// The live outage this is written against, 2026-09-21: two review jobs on
// jerryfane/adspower-agent#58 and adspower-fleet#46 died as ordinary failures
// while the review pool held three working fallback models, because xAI reports
// an exhausted subscription with a 403 and the words "run out of credits" —
// matching none of the quota signatures. An account that cannot pay is the same
// operational fact as a throttle: this provider cannot serve the job, another can.
func TestBillingExhaustionClassifiesAsThrottled(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{
			name: "xai subscription exhausted, the verbatim live text",
			text: "delivery failed: omp turn failed (stopReason error, errorStatus 403): 403 You have run out of credits or need a Grok subscription. Add credits at https://grok.com/?_s=usage or upgrade at https://grok.com/supergrok.",
		},
		{
			name: "openai-style insufficient credits",
			text: "provider error: insufficient credits for this request",
		},
		{
			name: "payment required",
			text: "HTTP 402 payment required",
		},
		{
			name: "upgrade your plan",
			text: "403 this model requires a pro subscription; upgrade your plan to continue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAuthQuotaStrict(tc.text); got != "throttled" {
				t.Fatalf("classifyAuthQuotaStrict = %q, want \"throttled\" so the pool falls through to the next model", got)
			}
		})
	}
}

// 403 is the ordinary "you may not do that" status. A permission refusal
// classified as quota would be deferred and retried against a wall that never
// opens, and the operator would be told to wait for a reset that does not exist.
// So 403 counts ONLY when a billing word sits on the same line.
func TestPlain403IsNotQuota(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "github permission refusal",
			text: "delivery failed: HTTP 403: Resource not accessible by integration",
			want: "",
		},
		{
			name: "forbidden path",
			text: "delivery failed: 403 Forbidden: api.anthropic.com is not allowlisted for this seat",
			want: "",
		},
		{
			name: "401 keeps its more specific auth class",
			text: "delivery failed: HTTP 401 unauthorized",
			want: "auth failing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAuthQuotaStrict(tc.text); got != tc.want {
				t.Fatalf("classifyAuthQuotaStrict = %q, want %q", got, tc.want)
			}
		})
	}
}

// The billing matcher must not fire on a reviewer's prose. Review results are
// agent-authored and routinely discuss credits, billing and subscriptions as
// subject matter; a finding that says "the billing module is untested" must not
// park the job as an account outage.
func TestBillingMatcherIgnoresOrdinaryProse(t *testing.T) {
	for _, text := range []string{
		"the review found that credits are never refunded on cancel",
		"P2: subscription renewal path lacks a test",
		"summary: upgrade the dependency to v2",
		"exit status 1",
	} {
		t.Run(text, func(t *testing.T) {
			if got := classifyAuthQuotaStrict(text); got != "" {
				t.Fatalf("classifyAuthQuotaStrict(%q) = %q, want \"\"", text, got)
			}
		})
	}
}

// Second live outage, same day, different provider: every claude-runtime review
// in the fleet now fails with an entitlement revocation rather than a balance.
// Verbatim from job local-review-joltra-claude-review-18d7a22b230e09f3-1.
// An entitlement the account no longer holds is the same operational fact as an
// empty balance: this provider cannot serve the job, another one can.
func TestEntitlementRevocationClassifiesAsThrottled(t *testing.T) {
	text := "delivery failed: claude: Your organization has disabled Claude subscription access for Claude Code \u00b7 Use an Anthropic API key instead, or ask your admin to enable access (api_error_status 403): exit status 1"
	if got := classifyAuthQuotaStrict(text); got != "throttled" {
		t.Fatalf("classifyAuthQuotaStrict = %q, want \"throttled\" so the pool falls through to the next model", got)
	}
}
