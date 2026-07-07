package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openBinaryTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestBinaryVerdictRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openBinaryTestStore(t)

	verdicts := []BinaryVerdict{
		{RunID: "run-1", QuestionID: "q2", Dimension: "style", Verdict: "no", Explanation: "has TODO"},
		{RunID: "run-1", QuestionID: "q1", Dimension: "correctness", Verdict: "yes", Explanation: "test present"},
		{RunID: "run-2", QuestionID: "q1", Dimension: "correctness", Verdict: "no"},
	}
	for _, v := range verdicts {
		if err := store.UpsertBinaryVerdict(ctx, v); err != nil {
			t.Fatalf("upsert %s/%s: %v", v.RunID, v.QuestionID, err)
		}
	}

	got, err := store.ListBinaryVerdicts(ctx, "run-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("run-1 verdicts = %d, want 2", len(got))
	}
	// Ordered by (dimension, question_id): correctness/q1 before style/q2.
	if got[0].QuestionID != "q1" || got[0].Dimension != "correctness" {
		t.Fatalf("first row = %+v, want correctness/q1", got[0])
	}
	if got[0].Verdict != "yes" || got[0].Explanation != "test present" {
		t.Fatalf("first row payload = %+v", got[0])
	}
	if got[0].CreatedAt == "" {
		t.Fatal("created_at not defaulted")
	}
	if got[1].QuestionID != "q2" {
		t.Fatalf("second row = %+v, want q2", got[1])
	}

	// run-2 is isolated.
	got2, err := store.ListBinaryVerdicts(ctx, "run-2")
	if err != nil {
		t.Fatalf("list run-2: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("run-2 verdicts = %d, want 1", len(got2))
	}
}

func TestBinaryVerdictUpsertOverwrites(t *testing.T) {
	ctx := context.Background()
	store := openBinaryTestStore(t)

	if err := store.UpsertBinaryVerdict(ctx, BinaryVerdict{RunID: "r", QuestionID: "q", Dimension: "d", Verdict: "no", Explanation: "first"}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if err := store.UpsertBinaryVerdict(ctx, BinaryVerdict{RunID: "r", QuestionID: "q", Dimension: "d", Verdict: "yes", Explanation: "second"}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, err := store.ListBinaryVerdicts(ctx, "r")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("verdicts = %d, want 1 (upsert in place)", len(got))
	}
	if got[0].Verdict != "yes" || got[0].Explanation != "second" {
		t.Fatalf("row = %+v, want overwrite to yes/second", got[0])
	}
}

func TestBinaryVerdictValidation(t *testing.T) {
	ctx := context.Background()
	store := openBinaryTestStore(t)

	if err := store.UpsertBinaryVerdict(ctx, BinaryVerdict{QuestionID: "q"}); err == nil {
		t.Fatal("expected error for missing run id")
	}
	if err := store.UpsertBinaryVerdict(ctx, BinaryVerdict{RunID: "r"}); err == nil {
		t.Fatal("expected error for missing question id")
	}
	// Verdict defaults to "no" when blank.
	if err := store.UpsertBinaryVerdict(ctx, BinaryVerdict{RunID: "r", QuestionID: "q"}); err != nil {
		t.Fatalf("upsert with default verdict: %v", err)
	}
	got, err := store.ListBinaryVerdicts(ctx, "r")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Verdict != "no" {
		t.Fatalf("default verdict row = %+v, want verdict=no", got)
	}
}

func TestListBinaryVerdictsEmpty(t *testing.T) {
	ctx := context.Background()
	store := openBinaryTestStore(t)
	got, err := store.ListBinaryVerdicts(ctx, "nope")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty run verdicts = %d, want 0", len(got))
	}
}
