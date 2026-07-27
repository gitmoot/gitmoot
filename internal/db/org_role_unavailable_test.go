package db

import (
	"context"
	"testing"
	"time"
)

func TestOrgRoleUnavailableLifecycle(t *testing.T) {
	store, err := Open(t.TempDir() + "/gitmoot.db")
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
	store, err := Open(t.TempDir() + "/gitmoot.db")
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
}
