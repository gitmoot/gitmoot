package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRoleMissedWakeRoundTripAndReset(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var table string
	if err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='org_role_missed_wakes'`).Scan(&table); err != nil {
		t.Fatalf("org_role_missed_wakes migration missing: %v", err)
	}
	first := time.Date(2026, 7, 22, 8, 1, 2, 345678901, time.UTC)
	if err := store.IncrementRoleMissedWake(ctx, " Review ", first); err != nil {
		t.Fatalf("IncrementRoleMissedWake(insert) error = %v", err)
	}
	if err := store.IncrementRoleMissedWake(ctx, "REVIEW", first.Add(time.Minute)); err != nil {
		t.Fatalf("IncrementRoleMissedWake(update) error = %v", err)
	}
	if err := store.IncrementRoleMissedWake(ctx, "Owner", first.Add(2*time.Minute)); err != nil {
		t.Fatalf("IncrementRoleMissedWake(second role) error = %v", err)
	}

	misses, err := store.ListRoleMissedWakes(ctx)
	if err != nil {
		t.Fatalf("ListRoleMissedWakes() error = %v", err)
	}
	if len(misses) != 2 || misses[0].Role != "owner" || misses[0].Consecutive != 1 || misses[1].Role != "review" || misses[1].Consecutive != 2 {
		t.Fatalf("misses = %+v, want role-sorted normalized counters", misses)
	}
	if got, want := misses[1].UpdatedAt, first.Add(time.Minute).Format(BlockedEpisodeTimeLayout); got != want {
		t.Fatalf("UpdatedAt = %q, want %q", got, want)
	}

	if err := store.ResetRoleMissedWake(ctx, " REVIEW "); err != nil {
		t.Fatalf("ResetRoleMissedWake() error = %v", err)
	}
	if err := store.ResetRoleMissedWake(ctx, "review"); err != nil {
		t.Fatalf("ResetRoleMissedWake(idempotent) error = %v", err)
	}
	misses, err = store.ListRoleMissedWakes(ctx)
	if err != nil || len(misses) != 1 || misses[0].Role != "owner" {
		t.Fatalf("misses after reset = %+v, err=%v", misses, err)
	}
}

func TestRoleMissedWakeRejectsEmptyRole(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.IncrementRoleMissedWake(ctx, " ", time.Now()); err == nil {
		t.Fatal("IncrementRoleMissedWake() error = nil, want validation error")
	}
	if err := store.ResetRoleMissedWake(ctx, " "); err == nil {
		t.Fatal("ResetRoleMissedWake() error = nil, want validation error")
	}
}
