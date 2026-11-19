package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceLedgerEnforcesBothCapsAndOneOpenPerSubject(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 12, 0, 0, 123456789, time.UTC)
	workspace := func(id string, issue int64) Workspace {
		return Workspace{ID: id, Seat: "intern", Repo: "acme/widget", Issue: issue, Provider: "e2b", Root: "/home/user/work",
			CreatedAt: now, LastActiveAt: now, RateUSDPerHour: 4, CostReservedUSD: 4, PaidUntil: now.Add(time.Hour)}
	}
	if err := store.CreateWorkspace(ctx, workspace("ws-a", 1), 10); err != nil {
		t.Fatal(err)
	}
	var already *WorkspaceAlreadyOpenError
	if err := store.CreateWorkspace(ctx, workspace("ws-dup", 1), 10); !errors.As(err, &already) || already.ID != "ws-a" {
		t.Fatalf("duplicate = %v", err)
	}
	if err := store.CreateWorkspace(ctx, workspace("ws-b", 2), 10); err != nil {
		t.Fatal(err)
	}
	// $8 committed: a third $4 reservation would pass $10.
	var capErr *WorkspaceCostCapError
	if err := store.CreateWorkspace(ctx, workspace("ws-c", 3), 10); !errors.As(err, &capErr) || capErr.Clause != "total" || capErr.CommittedUSD != 8 {
		t.Fatalf("over the cap = %v", err)
	}
	// Exactly at the cap is admitted.
	if err := store.CreateWorkspace(ctx, workspace("ws-c", 3), 12); err != nil {
		t.Fatalf("at the cap = %v", err)
	}
	if err := store.MarkWorkspaceReady(ctx, "ws-a", "ws/intern/1", "abc", "main", now); err != nil {
		t.Fatal(err)
	}
	if err := store.ExtendWorkspaceReservation(ctx, "ws-a", 4, now.Add(2*time.Hour), 1, 100, 6); !errors.As(err, &capErr) || capErr.Clause != "workspace" {
		t.Fatalf("per-workspace cap = %v", err)
	}
	if err := store.ExtendWorkspaceReservation(ctx, "ws-a", 4, now.Add(2*time.Hour), 1, 100, 8); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetWorkspace(ctx, "ws-a")
	if err != nil || got.CostReservedUSD != 8 || got.CostAccruedUSD != 1 || !got.PaidUntil.Equal(now.Add(2*time.Hour)) || !got.CreatedAt.Equal(now) {
		t.Fatalf("after extend = %+v %v", got, err)
	}
	// Touch never moves last_active_at backwards.
	if err := store.TouchWorkspace(ctx, "ws-a", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetWorkspace(ctx, "ws-a"); !got.LastActiveAt.Equal(now) {
		t.Fatalf("last_active_at moved back to %s", got.LastActiveAt)
	}
	// Closing settles to the accrued cost and frees the subject.
	if began, err := store.BeginWorkspaceClose(ctx, "ws-a", "manual"); !began || err != nil {
		t.Fatalf("begin close = %v %v", began, err)
	}
	if began, _ := store.BeginWorkspaceClose(ctx, "ws-a", "other"); began {
		t.Fatal("a second close reason replaced the first")
	}
	if err := store.FinishWorkspaceClose(ctx, "ws-a", WorkspaceStateClosed, 1.5, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if committed, _ := store.WorkspaceCommittedUSD(ctx); committed != 1.5+4+4 {
		t.Fatalf("committed = %v", committed)
	}
	if err := store.CreateWorkspace(ctx, workspace("ws-a2", 1), 100); err != nil {
		t.Fatalf("reopen after close = %v", err)
	}
	if active, _ := store.ListWorkspaces(ctx, false); len(active) != 3 {
		t.Fatalf("active = %d", len(active))
	}
	if all, _ := store.ListWorkspaces(ctx, true); len(all) != 4 {
		t.Fatalf("all = %d", len(all))
	}
	if _, err := store.GetWorkspace(ctx, "ws-none"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("missing = %v", err)
	}
}
