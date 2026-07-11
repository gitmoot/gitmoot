package db

import (
	"context"
	"testing"
)

func TestGroomLLMVerdictCacheRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)

	if _, ok, err := store.GetGroomLLMVerdict(ctx, "missing"); err != nil || ok {
		t.Fatalf("missing verdict: ok=%v err=%v", ok, err)
	}
	if err := store.StoreGroomLLMVerdict(ctx, GroomLLMVerdict{
		ContentHash: "hash-keep", Verdict: "no_split", Model: "codex/default",
	}); err != nil {
		t.Fatalf("store no_split verdict: %v", err)
	}
	keep, ok, err := store.GetGroomLLMVerdict(ctx, "hash-keep")
	if err != nil || !ok {
		t.Fatalf("get no_split verdict: ok=%v err=%v", ok, err)
	}
	if keep.Verdict != "no_split" || keep.CutsJSON != "" || keep.Model != "codex/default" || keep.CreatedAt == "" {
		t.Fatalf("no_split round trip = %+v", keep)
	}

	const cuts = `[{"id":"c001","text":"Second story"}]`
	if err := store.StoreGroomLLMVerdict(ctx, GroomLLMVerdict{
		ContentHash: "hash-split", Verdict: "split", CutsJSON: cuts, Model: "claude/sonnet",
	}); err != nil {
		t.Fatalf("store split verdict: %v", err)
	}
	split, ok, err := store.GetGroomLLMVerdict(ctx, "hash-split")
	if err != nil || !ok || split.Verdict != "split" || split.CutsJSON != cuts || split.Model != "claude/sonnet" {
		t.Fatalf("split round trip = %+v ok=%v err=%v", split, ok, err)
	}

	if err := store.StoreGroomLLMVerdict(ctx, GroomLLMVerdict{
		ContentHash: "hash-keep", Verdict: "split", CutsJSON: cuts, Model: "changed",
	}); err != nil {
		t.Fatalf("repeat store: %v", err)
	}
	unchanged, _, err := store.GetGroomLLMVerdict(ctx, "hash-keep")
	if err != nil || unchanged.Verdict != "no_split" || unchanged.Model != "codex/default" {
		t.Fatalf("first verdict was replaced: %+v err=%v", unchanged, err)
	}
}

func TestStoreGroomLLMVerdictValidation(t *testing.T) {
	store := openMemTestStore(t)
	for _, verdict := range []GroomLLMVerdict{
		{Verdict: "no_split"},
		{ContentHash: "h", Verdict: "unknown"},
		{ContentHash: "h", Verdict: "split"},
	} {
		if err := store.StoreGroomLLMVerdict(context.Background(), verdict); err == nil {
			t.Fatalf("expected invalid verdict %+v to fail", verdict)
		}
	}
}
