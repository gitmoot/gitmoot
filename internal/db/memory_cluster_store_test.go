package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// clusterTestAnchor mirrors the CLI's clusterAnchor shape (deterministic over the
// active facts' (id, updated_at)) closely enough for the store guard test: it only
// needs to move when a fact is added/edited/retired.
func clusterTestAnchor(rows []ConfirmedMemory) string {
	var sb strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&sb, "%d:%s;", r.ID, r.UpdatedAt)
	}
	return sb.String()
}

// TestRecomputeMemoryClustersFreshAnchorGuard proves the atomic apply:
// RecomputeMemoryClustersFresh rewrites the clustering when the in-tx anchor still
// matches, and — critically for the TOCTOU fix — leaves the membership UNCHANGED
// (ErrClusterPlanStale, full rollback, no partial write) when a fact confirmed
// after the plan was built moves the anchor. This is the store-side guarantee the
// CLI relies on so a concurrent confirm/attach is never silently dropped by the
// destructive rewrite.
func TestRecomputeMemoryClustersFreshAnchorGuard(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("builder")

	idA, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "a", Content: "the storage layer flushes the write-ahead log on commit",
	})
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	idB, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "b", Content: "the storage layer checkpoints the write-ahead log periodically",
	})
	if err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	rows, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("list vault: %v", err)
	}
	expected := clusterTestAnchor(rows)

	asg1 := MemoryClusterAssignment{
		Clusters: []MemoryCluster{
			{ClusterID: 0, Label: "unclustered"},
			{ClusterID: 1, Label: "storage", MedoidID: idA},
		},
		Members: []MemoryClusterMember{
			{MemoryID: idA, ClusterID: 1},
			{MemoryID: idB, ClusterID: 1},
		},
	}
	if err := store.RecomputeMemoryClustersFresh(ctx, asg1, expected, clusterTestAnchor); err != nil {
		t.Fatalf("fresh apply with matching anchor: %v", err)
	}
	members, err := store.ListMemoryClusterMembers(ctx)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("want 2 members after apply, got %d", len(members))
	}

	// A fact confirmed after the plan was built moves the anchor. Attempting the
	// rewrite with the now-stale expected anchor must abort and roll back entirely.
	if _, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo",
		Key: "c", Content: "a concurrently confirmed fact that must not be dropped",
	}); err != nil {
		t.Fatalf("upsert C: %v", err)
	}
	asg2 := MemoryClusterAssignment{
		Clusters: []MemoryCluster{{ClusterID: 0, Label: "unclustered"}},
		Members:  []MemoryClusterMember{{MemoryID: idA, ClusterID: 0}},
	}
	err = store.RecomputeMemoryClustersFresh(ctx, asg2, expected, clusterTestAnchor)
	if !errors.Is(err, ErrClusterPlanStale) {
		t.Fatalf("want ErrClusterPlanStale on moved anchor, got %v", err)
	}

	// Rollback: membership is exactly asg1's — the stale plan wrote nothing.
	members, err = store.ListMemoryClusterMembers(ctx)
	if err != nil {
		t.Fatalf("list members after stale: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("stale apply must not modify membership; got %d members", len(members))
	}
	for _, m := range members {
		if m.ClusterID != 1 {
			t.Fatalf("stale apply changed membership: memory %d now in cluster %d", m.MemoryID, m.ClusterID)
		}
	}
}
