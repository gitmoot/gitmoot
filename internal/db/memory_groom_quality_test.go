package db

import (
	"context"
	"math"
	"testing"
)

func TestGroomQualityVerdictCacheRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	if _, ok, err := store.GetGroomQualityVerdict(ctx, "missing"); err != nil || ok {
		t.Fatalf("missing verdict: ok=%v err=%v", ok, err)
	}
	if err := store.StoreGroomQualityVerdict(ctx, GroomQualityVerdict{
		ContentHash: "hash-useless", Verdict: "useless", Confidence: 0.93, Model: "codex/default",
	}); err != nil {
		t.Fatalf("store useless verdict: %v", err)
	}
	got, ok, err := store.GetGroomQualityVerdict(ctx, "hash-useless")
	if err != nil || !ok || got.Verdict != "useless" || got.Confidence != 0.93 || got.Model != "codex/default" || got.CreatedAt == "" {
		t.Fatalf("useless round trip = %+v ok=%v err=%v", got, ok, err)
	}
	const residue = "the original misparse error now explains itself"
	if err := store.StoreGroomQualityVerdict(ctx, GroomQualityVerdict{
		ContentHash: "hash-residue", Verdict: "contains_durable_residue", Confidence: 0.81, Residue: residue,
	}); err != nil {
		t.Fatalf("store residue verdict: %v", err)
	}
	residueGot, ok, err := store.GetGroomQualityVerdict(ctx, "hash-residue")
	if err != nil || !ok || residueGot.Residue != residue {
		t.Fatalf("residue round trip = %+v ok=%v err=%v", residueGot, ok, err)
	}
	if err := store.StoreGroomQualityVerdict(ctx, GroomQualityVerdict{
		ContentHash: "hash-useless", Verdict: "useful", Confidence: 0.5, Model: "changed",
	}); err != nil {
		t.Fatalf("repeat store: %v", err)
	}
	unchanged, _, _ := store.GetGroomQualityVerdict(ctx, "hash-useless")
	if unchanged.Verdict != "useless" || unchanged.Model != "codex/default" {
		t.Fatalf("first verdict was replaced: %+v", unchanged)
	}
}

func TestStoreGroomQualityVerdictValidation(t *testing.T) {
	store := openMemTestStore(t)
	for _, verdict := range []GroomQualityVerdict{
		{Verdict: "useless", Confidence: 0.5},
		{ContentHash: "h", Verdict: "unknown", Confidence: 0.5},
		{ContentHash: "h", Verdict: "contains_durable_residue", Confidence: 0.5},
		{ContentHash: "h", Verdict: "useful", Confidence: 0.5, Residue: "unexpected"},
		{ContentHash: "h", Verdict: "useless", Confidence: -0.1},
		{ContentHash: "h", Verdict: "useless", Confidence: 1.1},
		{ContentHash: "h", Verdict: "useless", Confidence: math.NaN()},
	} {
		if err := store.StoreGroomQualityVerdict(context.Background(), verdict); err == nil {
			t.Fatalf("expected invalid verdict %+v to fail", verdict)
		}
	}
}
