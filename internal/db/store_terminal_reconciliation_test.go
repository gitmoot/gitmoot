package db

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPullRequestTerminalReconciliationPinsOwnerAndScopesDebtToHead(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := store.BeginPullRequestTerminalReconciliation(ctx, PullRequestTerminalReconciliation{
		RepoFullName: "owner/repo", PullRequest: 17, HeadSHA: "head-one", OwnerTaskID: "task-a",
	}, []string{"task-a", "task-b"})
	if err != nil {
		t.Fatal(err)
	}
	if first.OwnerTaskID != "task-a" || first.EffectsCompleted {
		t.Fatalf("first reconciliation = %+v, want incomplete task-a owner", first)
	}
	if err := store.CompletePullRequestTerminalEffects(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolvePullRequestTerminalSettlement(ctx, first, "task-b"); err != nil {
		t.Fatal(err)
	}

	retry, err := store.BeginPullRequestTerminalReconciliation(ctx, PullRequestTerminalReconciliation{
		RepoFullName: "OWNER/REPO", PullRequest: 17, HeadSHA: "head-one", OwnerTaskID: "task-b",
	}, []string{"task-b", "task-c"})
	if err != nil {
		t.Fatal(err)
	}
	if retry.OwnerTaskID != "task-a" || !retry.EffectsCompleted {
		t.Fatalf("retry reconciliation = %+v, want completed original task-a owner", retry)
	}
	settlements, err := store.ListPullRequestTerminalSettlements(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(settlements, []string{"task-b", "task-c"}) {
		t.Fatalf("retry settlements = %v, want newly observed task-b and task-c debt", settlements)
	}

	newHead, err := store.BeginPullRequestTerminalReconciliation(ctx, PullRequestTerminalReconciliation{
		RepoFullName: "owner/repo", PullRequest: 17, HeadSHA: "head-two", OwnerTaskID: "task-b",
	}, []string{"task-b"})
	if err != nil {
		t.Fatal(err)
	}
	if newHead.OwnerTaskID != "task-b" || newHead.EffectsCompleted {
		t.Fatalf("new-head reconciliation = %+v, want fresh incomplete task-b owner", newHead)
	}
	settlements, err = store.ListPullRequestTerminalSettlements(ctx, newHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(settlements) != 0 {
		t.Fatalf("new-head settlements = %v, want no inherited debt", settlements)
	}
}
