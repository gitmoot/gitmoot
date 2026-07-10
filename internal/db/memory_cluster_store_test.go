package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
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

func TestMemoryClusterHierarchyMigrationOnPopulatedStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pre-hierarchy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	raw.SetMaxOpenConns(1)
	raw.SetMaxIdleConns(1)
	if err := configureWritableSQLite(ctx, raw); err != nil {
		t.Fatalf("configure sqlite: %v", err)
	}
	store := &Store{db: raw}
	t.Cleanup(func() { _ = store.Close() })

	hierarchyMigration := -1
	for i, migration := range migrations {
		if strings.Contains(migration, "ADD COLUMN parent_id") {
			hierarchyMigration = i
			break
		}
	}
	if hierarchyMigration < 0 {
		t.Fatal("parent_id migration not found")
	}
	for i := 0; i < hierarchyMigration; i++ {
		if err := store.applyMigration(ctx, i+1, migrations[i]); err != nil {
			t.Fatalf("apply pre-hierarchy migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `
INSERT INTO memory_clusters (cluster_id, label, label_override, medoid_id)
VALUES (7, 'legacy', 'owner-label', 42)`); err != nil {
		t.Fatalf("seed legacy cluster: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate latest: %v", err)
	}
	clusters, err := store.ListMemoryClusters(ctx)
	if err != nil {
		t.Fatalf("list migrated clusters: %v", err)
	}
	if len(clusters) != 1 || clusters[0].ParentID != 0 || clusters[0].LabelOverride != "owner-label" {
		t.Fatalf("migrated legacy cluster = %+v, want top-level with preserved data", clusters)
	}
}

func TestMemoryClusterHierarchyOverrideIdentity(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	const (
		parentID = int64(1)
		childAID = int64(1<<52 + 101)
		childBID = int64(1<<52 + 202)
	)
	split := MemoryClusterAssignment{Clusters: []MemoryCluster{
		{ClusterID: parentID, Label: "parent", MedoidID: 10},
		{ClusterID: childAID, ParentID: parentID, Label: "alpha", MedoidID: 10},
		{ClusterID: childBID, ParentID: parentID, Label: "beta", MedoidID: 20},
	}}
	flat := MemoryClusterAssignment{Clusters: []MemoryCluster{{ClusterID: parentID, Label: "parent", MedoidID: 10}}}
	if err := store.RecomputeMemoryClusters(ctx, flat); err != nil {
		t.Fatalf("seed flat parent: %v", err)
	}
	if err := store.RenameMemoryCluster(ctx, parentID, "owner-parent"); err != nil {
		t.Fatalf("rename parent: %v", err)
	}
	if err := store.RecomputeMemoryClusters(ctx, split); err != nil {
		t.Fatalf("split renamed parent: %v", err)
	}
	if err := store.RenameMemoryCluster(ctx, childAID, "owner-child"); err != nil {
		t.Fatalf("rename child: %v", err)
	}
	if err := store.RecomputeMemoryClusters(ctx, split); err != nil {
		t.Fatalf("recompute split: %v", err)
	}
	clusters, err := store.ListMemoryClusters(ctx)
	if err != nil {
		t.Fatalf("list recomputed split: %v", err)
	}
	byID := map[int64]MemoryCluster{}
	for _, c := range clusters {
		byID[c.ClusterID] = c
	}
	if byID[parentID].LabelOverride != "owner-parent" || byID[childAID].LabelOverride != "owner-child" {
		t.Fatalf("overrides collided or were lost: %+v", clusters)
	}

	if err := store.RecomputeMemoryClusters(ctx, flat); err != nil {
		t.Fatalf("dissolve split: %v", err)
	}
	clusters, err = store.ListMemoryClusters(ctx)
	if err != nil {
		t.Fatalf("list dissolved split: %v", err)
	}
	if len(clusters) != 1 || clusters[0].LabelOverride != "owner-parent" {
		t.Fatalf("dissolve did not preserve only parent override: %+v", clusters)
	}
}
