package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
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
		{id: "terminal-old", state: workflow.JobFailed, age: 73 * time.Hour, path: t.TempDir()},
		{id: "terminal-fresh", state: workflow.JobSucceeded, age: time.Hour, path: t.TempDir()},
		{id: "blocked-old", state: workflow.JobBlocked, age: 30 * 24 * time.Hour, path: t.TempDir()},
	}
	sharedPath := t.TempDir()
	rows = append(rows,
		seeded{id: "shared-old", state: workflow.JobFailed, age: 10 * 24 * time.Hour, path: sharedPath},
		seeded{id: "shared-fresh", state: workflow.JobSucceeded, age: time.Hour, path: sharedPath},
	)
	for _, row := range rows {
		payload, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", DelegationID: row.id, WorktreePath: row.path})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: row.id, Agent: "reader", Type: "ask", State: string(row.state),
			ParentJobID: "parent", DelegationID: row.id, Payload: string(payload),
		}, db.JobEvent{Kind: string(row.state), Message: "seed"}); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	const layout = "2006-01-02 15:04:05"
	for _, row := range rows {
		at := now.Add(-row.age).Format(layout)
		setJobTimes(t, home, row.id, at, at)
	}
	store = openCLIJobStore(t, home)
	defer store.Close()

	manager := &fakeReclaimWorktreeManager{branches: map[string]bool{}}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, DelegationCheckout: t.TempDir(), DelegationWorktrees: manager}
	}
	if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), now, 72*time.Hour); err != nil {
		t.Fatalf("reclaimAgedTerminalDelegationWorktrees: %v", err)
	}
	if len(manager.removed) != 1 || manager.removed[0] != rows[0].path {
		t.Fatalf("removed = %v, want only aged final worktree %s", manager.removed, rows[0].path)
	}
	if manager.pruned != 1 {
		t.Fatalf("prune calls = %d, want 1", manager.pruned)
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
	if err := engine.ReclaimAgedTerminalDelegationWorktree(ctx, "blocked-old", now.Add(-72*time.Hour)); err == nil || !strings.Contains(err.Error(), "non-final") {
		t.Fatalf("blocked force reclaim error = %v, want non-final refusal", err)
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
