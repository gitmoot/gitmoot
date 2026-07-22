package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestBlockedEpisodeRoundTripKeepsFirstBlockedSince(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var table string
	if err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='org_blocked_episodes'`).Scan(&table); err != nil {
		t.Fatalf("org_blocked_episodes migration missing: %v", err)
	}
	first := time.Date(2026, 7, 22, 8, 1, 2, 345678901, time.UTC)
	if err := store.UpsertBlockedEpisode(ctx, "task:owner/repo:task-1", first); err != nil {
		t.Fatalf("UpsertBlockedEpisode(insert) error = %v", err)
	}
	if err := store.UpsertBlockedEpisode(ctx, "task:owner/repo:task-1", first.Add(3*time.Hour)); err != nil {
		t.Fatalf("UpsertBlockedEpisode(update) error = %v", err)
	}
	if err := store.UpsertBlockedEpisode(ctx, "role:review", first.Add(time.Minute)); err != nil {
		t.Fatalf("UpsertBlockedEpisode(second subject) error = %v", err)
	}

	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		t.Fatalf("ListBlockedEpisodes() error = %v", err)
	}
	if len(episodes) != 2 || episodes[0].Subject != "role:review" || episodes[1].Subject != "task:owner/repo:task-1" {
		t.Fatalf("episodes = %+v, want subject-sorted rows", episodes)
	}
	if got, want := episodes[1].BlockedSince, first.Format(blockedEpisodeTimeLayout); got != want {
		t.Fatalf("BlockedSince = %q, want first-seen %q", got, want)
	}
	if episodes[1].EmittedAt != "" {
		t.Fatalf("EmittedAt = %q, want empty", episodes[1].EmittedAt)
	}

	if err := store.MarkBlockedEpisodeEmitted(ctx, episodes[1].Subject); err != nil {
		t.Fatalf("MarkBlockedEpisodeEmitted() error = %v", err)
	}
	episodes, err = store.ListBlockedEpisodes(ctx)
	if err != nil {
		t.Fatalf("ListBlockedEpisodes(after mark) error = %v", err)
	}
	if episodes[1].EmittedAt == "" {
		t.Fatal("EmittedAt is empty after mark")
	}
	if _, err := time.Parse(blockedEpisodeTimeLayout, episodes[1].EmittedAt); err != nil {
		t.Fatalf("EmittedAt = %q, want fixed-width UTC timestamp: %v", episodes[1].EmittedAt, err)
	}

	if err := store.ClearBlockedEpisode(ctx, episodes[1].Subject); err != nil {
		t.Fatalf("ClearBlockedEpisode() error = %v", err)
	}
	if err := store.ClearBlockedEpisode(ctx, episodes[1].Subject); err != nil {
		t.Fatalf("ClearBlockedEpisode(idempotent) error = %v", err)
	}
	episodes, err = store.ListBlockedEpisodes(ctx)
	if err != nil || len(episodes) != 1 || episodes[0].Subject != "role:review" {
		t.Fatalf("episodes after clear = %+v, err=%v", episodes, err)
	}
}

func TestUpsertBlockedEpisodeRejectsEmptySubject(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertBlockedEpisode(context.Background(), " ", time.Now()); err == nil {
		t.Fatal("UpsertBlockedEpisode() error = nil, want validation error")
	}
}
