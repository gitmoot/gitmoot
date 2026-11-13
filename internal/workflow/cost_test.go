package workflow

import (
	"math"
	"testing"
)

// floatNear reports whether got is within eps of want; the price math is in
// floating dollars so equality checks must tolerate representation error.
func floatNear(got, want, eps float64) bool {
	return math.Abs(got-want) <= eps
}

// TestPriceForModelTiers pins the substring tiering (mirroring ruflo's
// modelTier()): a model id is matched case-insensitively against opus → sonnet →
// haiku (most-specific first), and anything unrecognized — including empty —
// falls back to the mid-tier (Sonnet) default rather than zero.
func TestPriceForModelTiers(t *testing.T) {
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{"claude-opus-4-8", ModelPrice{InputPerToken: 15.0 / 1e6, OutputPerToken: 75.0 / 1e6}},
		{"claude-sonnet-4-5", ModelPrice{InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6}},
		{"claude-3-5-HAIKU", ModelPrice{InputPerToken: 0.25 / 1e6, OutputPerToken: 1.25 / 1e6}},
		{"", defaultUnknownModelPrice},
		{"gpt-5.5", defaultUnknownModelPrice},
		{"  Opus  ", ModelPrice{InputPerToken: 15.0 / 1e6, OutputPerToken: 75.0 / 1e6}},
	}
	for _, tc := range cases {
		got := priceForModel(tc.model)
		if !floatNear(got.InputPerToken, tc.want.InputPerToken, 1e-12) ||
			!floatNear(got.OutputPerToken, tc.want.OutputPerToken, 1e-12) {
			t.Fatalf("priceForModel(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

// TestCostFromTokens pins the post-call accounting formula
// (input × input_price + output × output_price) and the negative-spend clamp:
// negative token counts contribute 0, so a bad usage write cannot refund cost.
func TestCostFromTokens(t *testing.T) {
	// 1,000,000 input + 1,000,000 output on Opus => $15 + $75 = $90.
	if got := costFromTokens("claude-opus-4-8", 1_000_000, 1_000_000); !floatNear(got, 90.0, 1e-9) {
		t.Fatalf("costFromTokens(opus, 1e6, 1e6) = %v, want 90.0", got)
	}
	// Sonnet: 1,000,000 input + 1,000,000 output => $3 + $15 = $18.
	if got := costFromTokens("claude-sonnet-4-5", 1_000_000, 1_000_000); !floatNear(got, 18.0, 1e-9) {
		t.Fatalf("costFromTokens(sonnet, 1e6, 1e6) = %v, want 18.0", got)
	}
	// Negative counts clamp to 0 — no refund past the cap.
	if got := costFromTokens("claude-opus-4-8", -5, -7); got != 0 {
		t.Fatalf("costFromTokens(opus, -5, -7) = %v, want 0 (clamped)", got)
	}
	// Unknown model is priced at the mid-tier fallback, never free.
	if got := costFromTokens("some-unknown-model", 1_000_000, 0); !floatNear(got, 3.0, 1e-9) {
		t.Fatalf("costFromTokens(unknown, 1e6, 0) = %v, want 3.0 (Sonnet fallback)", got)
	}
}
