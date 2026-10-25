package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestReclaimAgedTerminalDelegationWorktreesTTLAndStateGate(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	type seeded struct {
		id    string
		state workflow.JobState
		age   time.Duration
		path  string
	}
	rows := []seeded{
		{id: "terminal-old", state: workflow.JobFailed, age: 73 * time.Hour, path: managedReclaimTestPath(t, home, "terminal-old")},
		{id: "terminal-fresh", state: workflow.JobSucceeded, age: time.Hour, path: managedReclaimTestPath(t, home, "terminal-fresh")},
		{id: "blocked-old", state: workflow.JobBlocked, age: 30 * 24 * time.Hour, path: managedReclaimTestPath(t, home, "blocked-old")},
	}
	sharedPath := managedReclaimTestPath(t, home, "shared-old")
	rows = append(rows,
		seeded{id: "shared-old", state: workflow.JobFailed, age: 10 * 24 * time.Hour, path: sharedPath},
		seeded{id: "shared-fresh", state: workflow.JobSucceeded, age: time.Hour, path: sharedPath},
	)
	manager, checkout := newSelectiveReclaimManager(t, rows[0].path, rows[1].path, rows[2].path, sharedPath)
	head, err := manager.RevParse(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		decision := "approved"
		if row.state == workflow.JobFailed {
			decision = "failed"
		}
		payload, err := json.Marshal(workflow.JobPayload{
			Repo: "owner/repo", WorktreePath: row.path, ReadOnlyWorktree: true, ReadOnlySeat: true,
			PullRequest: 7, HeadSHA: head,
			Result: &workflow.AgentResult{Decision: decision, Summary: "terminal fixture evidence"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: row.id, Agent: "reader", Type: "review", State: string(row.state), Payload: string(payload),
		}, db.JobEvent{Kind: string(row.state), Message: "seed"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range rows {
		backdateJobUpdatedAt(t, store.DatabasePath(), row.id, now.Add(-row.age))
	}
	defer store.Close()

	worker := defaultJobWorker(store, io.Discard, home)
	worker.WorkflowFactory = func(string) workflow.Engine {
		return reclaimFixtureEngine(worker, checkout, manager)
	}
	if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), now, 72*time.Hour); err != nil {
		t.Fatalf("reclaimAgedTerminalDelegationWorktrees: %v", err)
	}
	if len(manager.removed) != 1 || manager.removed[0] != rows[0].path {
		t.Fatalf("removed = %v, want only aged final worktree %s", manager.removed, rows[0].path)
	}
	for i, row := range rows {
		_, err := os.Stat(row.path)
		if i == 0 && !os.IsNotExist(err) {
			t.Fatalf("eligible terminal resource remains: %v", err)
		}
		if i != 0 && err != nil {
			t.Fatalf("protected resource %s changed: %v", row.id, err)
		}
	}
	outcome, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID(rows[0].id, rows[0].path))
	if err != nil || outcome.State != db.CleanupObligationRemoved {
		t.Fatalf("terminal disposal was not recorded durably: %+v %v", outcome, err)
	}
	events, err := store.ListJobEvents(ctx, "terminal-old")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "delegation_worktree_reclaimed_ttl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events = %+v, want delegation_worktree_reclaimed_ttl", events)
	}

	engine := worker.WorkflowFactory("")
	if removed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, "blocked-old", now.Add(-72*time.Hour)); err == nil || removed {
		t.Fatalf("blocked owner was not refused: removed=%v err=%v", removed, err)
	}
}

func TestReclaimAgedTerminalDelegationWorktreesDisabled(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	manager := &fakeReclaimWorktreeManager{branches: map[string]bool{}}
	worker := defaultJobWorker(store, io.Discard, t.TempDir())
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, DelegationCheckout: t.TempDir(), DelegationWorktrees: manager}
	}
	if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if len(manager.removed) != 0 {
		t.Fatalf("TTL=0 removed %v", manager.removed)
	}
}
