package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRecycleOverdueEpisodeRoundTripKeepsFirstOverdueSince(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var table string
	if err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='org_recycle_overdue_episodes'`).Scan(&table); err != nil {
		t.Fatalf("org_recycle_overdue_episodes migration missing: %v", err)
	}
	first := time.Date(2026, 7, 23, 8, 1, 2, 345678901, time.UTC)
	if err := store.UpsertRecycleOverdueEpisode(ctx, "review", first, first); err != nil {
		t.Fatalf("UpsertRecycleOverdueEpisode(insert) error = %v", err)
	}
	if err := store.UpsertRecycleOverdueEpisode(ctx, "review", first.Add(3*time.Hour), first.Add(3*time.Hour)); err != nil {
		t.Fatalf("UpsertRecycleOverdueEpisode(update) error = %v", err)
	}
	if err := store.UpsertRecycleOverdueEpisode(ctx, "owner", first.Add(time.Minute), first.Add(time.Minute)); err != nil {
		t.Fatalf("UpsertRecycleOverdueEpisode(second subject) error = %v", err)
	}

	episodes, err := store.ListRecycleOverdueEpisodes(ctx)
	if err != nil {
		t.Fatalf("ListRecycleOverdueEpisodes() error = %v", err)
	}
	if len(episodes) != 2 || episodes[0].Subject != "owner" || episodes[1].Subject != "review" {
		t.Fatalf("episodes = %+v, want subject-sorted rows", episodes)
	}
	if got, want := episodes[1].OverdueSince, first.Format(BlockedEpisodeTimeLayout); got != want {
		t.Fatalf("OverdueSince = %q, want first-seen %q", got, want)
	}
	if got, want := episodes[1].UpdatedAt, first.Add(3*time.Hour).Format(BlockedEpisodeTimeLayout); got != want {
		t.Fatalf("UpdatedAt = %q, want refreshed %q", got, want)
	}
	if episodes[1].EmittedAt != "" {
		t.Fatalf("EmittedAt = %q, want empty", episodes[1].EmittedAt)
	}

	markedAt := first.Add(4 * time.Hour)
	if err := store.MarkRecycleOverdueEpisodeEmitted(ctx, episodes[1].Subject, markedAt); err != nil {
		t.Fatalf("MarkRecycleOverdueEpisodeEmitted() error = %v", err)
	}
	if err := store.MarkRecycleOverdueEpisodeEmitted(ctx, "missing", markedAt); err != nil {
		t.Fatalf("MarkRecycleOverdueEpisodeEmitted(missing) error = %v", err)
	}
	episodes, err = store.ListRecycleOverdueEpisodes(ctx)
	if err != nil {
		t.Fatalf("ListRecycleOverdueEpisodes(after mark) error = %v", err)
	}
	if got, want := episodes[1].EmittedAt, markedAt.Format(BlockedEpisodeTimeLayout); got != want {
		t.Fatalf("EmittedAt = %q, want %q", got, want)
	}

	if err := store.ClearRecycleOverdueEpisode(ctx, episodes[1].Subject); err != nil {
		t.Fatalf("ClearRecycleOverdueEpisode() error = %v", err)
	}
	if err := store.ClearRecycleOverdueEpisode(ctx, episodes[1].Subject); err != nil {
		t.Fatalf("ClearRecycleOverdueEpisode(idempotent) error = %v", err)
	}
	episodes, err = store.ListRecycleOverdueEpisodes(ctx)
	if err != nil || len(episodes) != 1 || episodes[0].Subject != "owner" {
		t.Fatalf("episodes after clear = %+v, err=%v", episodes, err)
	}
}

func TestUpsertRecycleOverdueEpisodeRejectsEmptySubject(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertRecycleOverdueEpisode(context.Background(), " ", time.Now(), time.Now()); err == nil {
		t.Fatal("UpsertRecycleOverdueEpisode() error = nil, want validation error")
	}
}
