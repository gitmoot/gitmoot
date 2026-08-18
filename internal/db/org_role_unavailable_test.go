package db

import (
	"context"
	"testing"
	"time"
)

func TestOrgRoleUnavailableLifecycle(t *testing.T) {
	store, err := openCachedTestStore(t, t.TempDir()+"/gitmoot.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if err := store.UpsertOrgRoleUnavailable(ctx, " Review ", "QUOTA", now.Add(time.Hour), now); err != nil {
		t.Fatalf("UpsertOrgRoleUnavailable: %v", err)
	}
	got, found, err := store.GetActiveOrgRoleUnavailable(ctx, "REVIEW", now)
	if err != nil || !found || got.Role != "review" || got.Reason != "quota" || got.EscalatedAt != "" {
		t.Fatalf("GetActiveOrgRoleUnavailable = %+v, %v, %v", got, found, err)
	}
	claimed, err := store.MarkOrgRoleUnavailableEscalated(ctx, "review", now.Add(time.Minute))
	if err != nil || !claimed {
		t.Fatalf("first escalation claim = %v, %v", claimed, err)
	}
	claimed, err = store.MarkOrgRoleUnavailableEscalated(ctx, "review", now.Add(2*time.Minute))
	if err != nil || claimed {
		t.Fatalf("second escalation claim = %v, %v", claimed, err)
	}

	// Refreshing the same live incident neither shortens it nor resets escalation.
	if err := store.UpsertOrgRoleUnavailable(ctx, "review", "quota", now.Add(30*time.Minute), now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, found, err = store.GetActiveOrgRoleUnavailable(ctx, "review", now.Add(4*time.Minute))
	if err != nil || !found || got.Until != now.Add(time.Hour).Format(BlockedEpisodeTimeLayout) || got.EscalatedAt == "" {
		t.Fatalf("refreshed incident = %+v, %v, %v", got, found, err)
	}

	cleared, err := store.ClearExpiredOrgRolesUnavailable(ctx, now.Add(time.Hour))
	if err != nil || cleared != 1 {
		t.Fatalf("ClearExpiredOrgRolesUnavailable = %d, %v", cleared, err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(ctx, "review", now.Add(time.Hour)); err != nil || found {
		t.Fatalf("expired incident found=%v err=%v", found, err)
	}

	// A new incident after expiry receives a fresh escalation claim.
	if err := store.UpsertOrgRoleUnavailable(ctx, "review", "quota", now.Add(2*time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.MarkOrgRoleUnavailableEscalated(ctx, "review", now.Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("new incident escalation claim = %v, %v", claimed, err)
	}
	if err := store.ClearOrgRoleUnavailable(ctx, "review"); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListActiveOrgRolesUnavailable(ctx, now); err != nil || len(rows) != 0 {
		t.Fatalf("rows after clear = %+v, %v", rows, err)
	}
}

func TestOrgRoleUnavailableRejectsInvalidInput(t *testing.T) {
	store, err := openCachedTestStore(t, t.TempDir()+"/gitmoot.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "", "quota", now.Add(time.Hour), now); err == nil {
		t.Fatal("empty role accepted")
	}
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "owner", "", now.Add(time.Hour), now); err == nil {
		t.Fatal("empty reason accepted")
	}
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "owner", "quota", time.Time{}, now); err == nil {
		t.Fatal("zero until accepted")
	}
	if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "owner", "", "quota", now.Add(time.Hour), now); err == nil {
		t.Fatal("empty runtime accepted")
	}
}

func TestOrgRoleUnavailableRuntimeScopedClear(t *testing.T) {
	store, err := openCachedTestStore(t, t.TempDir()+"/gitmoot.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if err := store.UpsertOrgRoleUnavailableForRuntime(ctx, "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetActiveOrgRoleUnavailable(ctx, "review", now)
	if err != nil || !found || got.Runtime != "claude" {
		t.Fatalf("runtime-attributed incident = %+v found=%v err=%v", got, found, err)
	}
	if err := store.ClearOrgRoleUnavailableForRuntime(ctx, "review", "codex"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(ctx, "review", now); err != nil || !found {
		t.Fatalf("cross-runtime success cleared incident: found=%v err=%v", found, err)
	}
	if err := store.ClearOrgRoleUnavailableForRuntime(ctx, "review", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(ctx, "review", now); err != nil || found {
		t.Fatalf("same-runtime success left incident: found=%v err=%v", found, err)
	}
}

func TestOrgRoleUnavailableMalformedUntilFailsClosed(t *testing.T) {
	store, err := openCachedTestStore(t, t.TempDir()+"/gitmoot.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if err := store.UpsertOrgRoleUnavailableForRuntime(ctx, "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE org_role_unavailable SET unavailable_until = 'not-a-time' WHERE role = 'review'`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(ctx, "review", now); err == nil || found {
		t.Fatalf("malformed incident = found=%v err=%v, want indeterminate error", found, err)
	}
	if rows, err := store.ListActiveOrgRolesUnavailable(ctx, now); err == nil || rows != nil {
		t.Fatalf("malformed incident list = %+v err=%v, want indeterminate error", rows, err)
	}
	if cleared, err := store.ClearExpiredOrgRolesUnavailable(ctx, now); err == nil || cleared != 0 {
		t.Fatalf("malformed incident sweep = %d err=%v, want retained fail-closed evidence", cleared, err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM org_role_unavailable WHERE role = 'review'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("malformed incident rows = %d, want retained fail-closed evidence", count)
	}
}
