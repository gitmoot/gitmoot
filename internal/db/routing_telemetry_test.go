package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestInsertAndListRoutingTelemetry pins the #530 capture round-trip: inserted
// rows read back with every field intact, newest first, and the repo/action/since
// filters narrow correctly.
func TestInsertAndListRoutingTelemetry(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	rows := []RoutingTelemetry{
		{JobID: "j1", Repo: "o/r", Action: "implement", Phase: "build", Runtime: "codex", Model: "gpt-5.5", Agent: "impl", TemplateID: "t1", TemplateCommit: "abc", JobState: "succeeded", Decision: "implemented", Approved: true, TestsRun: 2, DurationMS: 1000, InputTokens: 100, OutputTokens: 20},
		{JobID: "j2", Repo: "o/r", Action: "review", Runtime: "claude", Model: "opus", Agent: "rev", JobState: "succeeded", Decision: "approved", Approved: true, DurationMS: 500},
		{JobID: "j3", Repo: "other/r", Action: "implement", Runtime: "codex", JobState: "failed", Decision: "failed", DurationMS: 2000},
	}
	for _, r := range rows {
		if err := store.InsertRoutingTelemetry(ctx, r); err != nil {
			t.Fatalf("InsertRoutingTelemetry(%s) error: %v", r.JobID, err)
		}
	}

	all, err := store.ListRoutingTelemetry(ctx, RoutingTelemetryFilter{})
	if err != nil {
		t.Fatalf("ListRoutingTelemetry error: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d rows, want 3", len(all))
	}
	// Newest first (id DESC).
	if all[0].JobID != "j3" || all[2].JobID != "j1" {
		t.Fatalf("unexpected order: %s..%s", all[0].JobID, all[2].JobID)
	}
	// Field integrity on the richest row.
	got := all[2]
	if got.Repo != "o/r" || got.Action != "implement" || got.Phase != "build" || got.Runtime != "codex" ||
		got.Model != "gpt-5.5" || got.Agent != "impl" || got.TemplateID != "t1" || got.TemplateCommit != "abc" ||
		got.JobState != "succeeded" || got.Decision != "implemented" || !got.Approved || got.TestsRun != 2 ||
		got.DurationMS != 1000 || got.InputTokens != 100 || got.OutputTokens != 20 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.CreatedAt == "" {
		t.Fatalf("expected a created_at default, got empty")
	}

	// repo filter.
	byRepo, err := store.ListRoutingTelemetry(ctx, RoutingTelemetryFilter{Repo: "o/r"})
	if err != nil {
		t.Fatalf("ListRoutingTelemetry repo error: %v", err)
	}
	if len(byRepo) != 2 {
		t.Fatalf("repo filter got %d, want 2", len(byRepo))
	}

	// action filter.
	byAction, err := store.ListRoutingTelemetry(ctx, RoutingTelemetryFilter{Action: "implement"})
	if err != nil {
		t.Fatalf("ListRoutingTelemetry action error: %v", err)
	}
	if len(byAction) != 2 {
		t.Fatalf("action filter got %d, want 2", len(byAction))
	}

	// since filter in the future excludes everything.
	future, err := store.ListRoutingTelemetry(ctx, RoutingTelemetryFilter{Since: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("ListRoutingTelemetry since error: %v", err)
	}
	if len(future) != 0 {
		t.Fatalf("future since got %d, want 0", len(future))
	}
}

// TestAggregateRoutingTelemetry pins the pure aggregation: grouping by
// (action, runtime, model, template), success/approval rates, blocked/failed
// counts, median duration, summed tokens, and deterministic ordering.
func TestAggregateRoutingTelemetry(t *testing.T) {
	rows := []RoutingTelemetry{
		// Group A: codex/gpt-5.5/implement/t1 — 3 rows: 2 succeeded (1 approved via
		// implemented), 1 failed.
		{Action: "implement", Runtime: "codex", Model: "gpt-5.5", TemplateID: "t1", JobState: "succeeded", Approved: true, DurationMS: 100, InputTokens: 10, OutputTokens: 1},
		{Action: "implement", Runtime: "codex", Model: "gpt-5.5", TemplateID: "t1", JobState: "succeeded", Approved: true, DurationMS: 300, InputTokens: 10, OutputTokens: 1},
		{Action: "implement", Runtime: "codex", Model: "gpt-5.5", TemplateID: "t1", JobState: "failed", Approved: false, DurationMS: 200, InputTokens: 10, OutputTokens: 1},
		// Group B: claude/opus/review/t2 — 1 row, blocked.
		{Action: "review", Runtime: "claude", Model: "opus", TemplateID: "t2", JobState: "blocked", Approved: false, DurationMS: 999},
	}
	groups := AggregateRoutingTelemetry(rows)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	// Most observations first => group A leads.
	a := groups[0]
	if a.Action != "implement" || a.Runtime != "codex" || a.Model != "gpt-5.5" || a.TemplateID != "t1" {
		t.Fatalf("group A key mismatch: %+v", a)
	}
	if a.Count != 3 || a.SuccessCount != 2 || a.FailedCount != 1 || a.ApprovedCount != 2 {
		t.Fatalf("group A counts mismatch: %+v", a)
	}
	if a.SuccessRate < 0.66 || a.SuccessRate > 0.67 {
		t.Fatalf("group A success rate = %v, want ~0.667", a.SuccessRate)
	}
	if a.MedianDurationMS != 200 {
		t.Fatalf("group A median = %d, want 200", a.MedianDurationMS)
	}
	if a.InputTokens != 30 || a.OutputTokens != 3 {
		t.Fatalf("group A tokens = %d/%d, want 30/3", a.InputTokens, a.OutputTokens)
	}
	b := groups[1]
	if b.Count != 1 || b.BlockedCount != 1 || b.SuccessCount != 0 {
		t.Fatalf("group B counts mismatch: %+v", b)
	}
	if b.MedianDurationMS != 999 {
		t.Fatalf("group B median = %d, want 999", b.MedianDurationMS)
	}

	// Empty input is safe.
	if got := AggregateRoutingTelemetry(nil); len(got) != 0 {
		t.Fatalf("nil input got %d groups, want 0", len(got))
	}
}

// TestListRoutingTelemetryMissingTable proves a store that predates the #530
// migration reports "no observations" rather than erroring, so a read-only
// summary on an un-migrated DB degrades gracefully.
func TestListRoutingTelemetryMissingTable(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(ctx, `DROP TABLE routing_telemetry`); err != nil {
		t.Fatalf("drop table error: %v", err)
	}
	rows, err := store.ListRoutingTelemetry(ctx, RoutingTelemetryFilter{})
	if err != nil {
		t.Fatalf("expected nil error on missing table, got %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}
