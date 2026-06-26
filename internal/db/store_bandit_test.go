package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openBanditStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestBanditArmMissingIsPrior proves an unrecorded arm reads back as "not found"
// (the caller treats that as the Beta(1,1) prior), keeping the table off-by-
// default: no rows exist until an A/B records a pick.
func TestBanditArmMissingIsPrior(t *testing.T) {
	store := openBanditStore(t)
	ctx := context.Background()
	arm, found, err := store.GetBanditArm(ctx, "planner", "planner@v1")
	if err != nil {
		t.Fatalf("GetBanditArm: %v", err)
	}
	if found {
		t.Fatalf("expected no row for a fresh arm, got %+v", arm)
	}
}

// TestIncrementBanditArm proves the atomic win/loss counters: a first win seeds
// Beta(2,1) pulls=1 from the prior, a subsequent loss yields Beta(2,2) pulls=2,
// and the returned arm matches the persisted row.
func TestIncrementBanditArm(t *testing.T) {
	store := openBanditStore(t)
	ctx := context.Background()

	won, err := store.IncrementBanditArm(ctx, "planner", "planner@v2", true)
	if err != nil {
		t.Fatalf("IncrementBanditArm win: %v", err)
	}
	if won.Alpha != 2 || won.Beta != 1 || won.Pulls != 1 {
		t.Fatalf("after win = Beta(%.0f,%.0f) pulls=%d, want Beta(2,1) pulls=1", won.Alpha, won.Beta, won.Pulls)
	}

	lost, err := store.IncrementBanditArm(ctx, "planner", "planner@v2", false)
	if err != nil {
		t.Fatalf("IncrementBanditArm loss: %v", err)
	}
	if lost.Alpha != 2 || lost.Beta != 2 || lost.Pulls != 2 {
		t.Fatalf("after loss = Beta(%.0f,%.0f) pulls=%d, want Beta(2,2) pulls=2", lost.Alpha, lost.Beta, lost.Pulls)
	}

	got, found, err := store.GetBanditArm(ctx, "planner", "planner@v2")
	if err != nil || !found {
		t.Fatalf("GetBanditArm after increments: found=%v err=%v", found, err)
	}
	if got.Alpha != 2 || got.Beta != 2 || got.Pulls != 2 {
		t.Fatalf("persisted arm = Beta(%.0f,%.0f) pulls=%d, want Beta(2,2) pulls=2", got.Alpha, got.Beta, got.Pulls)
	}
}

// TestUpsertBanditArmReplaces proves UpsertBanditArm stores the full posterior
// verbatim (it does not increment) and clamps degenerate inputs to the prior.
func TestUpsertBanditArmReplaces(t *testing.T) {
	store := openBanditStore(t)
	ctx := context.Background()
	if err := store.UpsertBanditArm(ctx, BanditArm{TemplateID: "planner", TemplateVersionID: "planner@v3", Alpha: 7, Beta: 3, Pulls: 8}); err != nil {
		t.Fatalf("UpsertBanditArm: %v", err)
	}
	got, found, err := store.GetBanditArm(ctx, "planner", "planner@v3")
	if err != nil || !found {
		t.Fatalf("GetBanditArm: found=%v err=%v", found, err)
	}
	if got.Alpha != 7 || got.Beta != 3 || got.Pulls != 8 {
		t.Fatalf("arm = Beta(%.0f,%.0f) pulls=%d, want Beta(7,3) pulls=8", got.Alpha, got.Beta, got.Pulls)
	}
	// Degenerate alpha/beta clamp to the prior, not below.
	if err := store.UpsertBanditArm(ctx, BanditArm{TemplateVersionID: "planner@v4", Alpha: 0, Beta: -1, Pulls: -5}); err != nil {
		t.Fatalf("UpsertBanditArm degenerate: %v", err)
	}
	clamped, _, err := store.GetBanditArm(ctx, "", "planner@v4")
	if err != nil {
		t.Fatalf("GetBanditArm clamped: %v", err)
	}
	if clamped.Alpha != 1 || clamped.Beta != 1 || clamped.Pulls != 0 {
		t.Fatalf("clamped arm = Beta(%.0f,%.0f) pulls=%d, want Beta(1,1) pulls=0", clamped.Alpha, clamped.Beta, clamped.Pulls)
	}
}
