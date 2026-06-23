package workflow

import (
	"context"
	"strings"
)

// ModelPrice is a per-model price row in USD per single token (input and
// output). The figures are derived from the published per-1M-token list prices
// divided by 1e6, so the math below is a direct tokens × price multiply.
//
// We deliberately track only input and output here (not the cache-write /
// cache-read classes ruflo's _prices.mjs carries) because gitmoot persists only
// per-job InputTokens / OutputTokens (see db.Job and UpdateJobUsage); cached
// token classes are not captured by any runtime adapter today, so a four-class
// table would add columns that are always zero. When usage capture grows cache
// classes, extend ModelPrice and priceForModel together.
type ModelPrice struct {
	// InputPerToken is the USD charged per input (prompt) token.
	InputPerToken float64
	// OutputPerToken is the USD charged per output (completion) token.
	OutputPerToken float64
}

// modelPriceRow names a default price-table entry: a case-insensitive substring
// that is matched against a job's model id (Tier, e.g. "haiku"/"sonnet"/"opus",
// mirroring ruflo's modelTier()) and the price for that tier. The list is
// ordered most-specific-first; priceForModel returns the first row whose Tier is
// a substring of the model id.
type modelPriceRow struct {
	Tier  string
	Price ModelPrice
}

// defaultModelPrices is the built-in price table: per-1M-token list prices
// (Haiku 0.25/1.25, Sonnet 3/15, Opus 15/75 — input/output USD) expressed as USD
// per single token. A model id that matches none of these tiers falls back to
// defaultUnknownModelPrice. The table is intentionally tiny and hardcoded
// (mirroring ruflo's _prices.mjs); it drifts from real provider pricing unless
// maintained, so the cost budget is a coarse runaway-cost backstop, not a
// precise spend meter.
var defaultModelPrices = []modelPriceRow{
	{Tier: "opus", Price: ModelPrice{InputPerToken: 15.0 / 1e6, OutputPerToken: 75.0 / 1e6}},
	{Tier: "sonnet", Price: ModelPrice{InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6}},
	{Tier: "haiku", Price: ModelPrice{InputPerToken: 0.25 / 1e6, OutputPerToken: 1.25 / 1e6}},
}

// defaultUnknownModelPrice is the fallback applied to a job whose model id is
// empty or matches no tier in defaultModelPrices. It uses the Sonnet row — a
// mid-tier, non-zero price — so an unknown model still contributes a sensible
// (rather than free) cost to the budget. Picking a non-zero default keeps the
// budget conservative: an unpriced model cannot silently spend past the cap.
var defaultUnknownModelPrice = ModelPrice{InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6}

// priceForModel maps a model id to a price row by case-insensitive substring,
// most-specific-tier-first (opus → sonnet → haiku), mirroring ruflo's
// modelTier(). An empty or unrecognized model id falls back to
// defaultUnknownModelPrice rather than zero, so the budget never under-prices an
// unknown model to free.
func priceForModel(model string) ModelPrice {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return defaultUnknownModelPrice
	}
	for _, row := range defaultModelPrices {
		if strings.Contains(id, row.Tier) {
			return row.Price
		}
	}
	return defaultUnknownModelPrice
}

// costFromTokens derives the USD cost of a single job's measured token usage:
// input × input-price + output × output-price, using the price row for model.
// It is post-call accounting over real token counts (mirroring ruflo's
// costUsd() / costFromTokens()), never a pre-call estimate. Negative token
// counts are clamped to 0 so a bad usage write cannot "refund" cost past the cap.
func costFromTokens(model string, inputTokens, outputTokens int) float64 {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	p := priceForModel(model)
	return float64(inputTokens)*p.InputPerToken + float64(outputTokens)*p.OutputPerToken
}

// sumRootDelegationCost sums the USD cost across an entire coordination tree —
// the originating coordinator (job.ID == rootID) plus every child or
// continuation whose payload RootJobID points back at it — by pricing each job's
// measured token usage through priceForModel(payload.Model). It is the cost
// analogue of sumRootDelegationTokens (#338 Part B): there is no store query
// keyed on root, so it lists all jobs and filters by RootJobID.
//
// Cost is measured accounting derived from the same per-job token counts the
// token budget already sums (db.Job.InputTokens / OutputTokens) — a job whose
// runtime did not report usage contributes 0, so the sum under-counts rather
// than over-counts. A job whose model id is empty/unknown is priced at the
// mid-tier fallback (defaultUnknownModelPrice) so it is never free.
func (e Engine) sumRootDelegationCost(ctx context.Context, rootID string) (float64, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, job := range jobs {
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return 0, err
		}
		if job.ID == rootID {
			total += costFromTokens(payload.Model, job.InputTokens, job.OutputTokens)
			continue
		}
		if payload.RootJobID == rootID {
			total += costFromTokens(payload.Model, job.InputTokens, job.OutputTokens)
		}
	}
	return total, nil
}
