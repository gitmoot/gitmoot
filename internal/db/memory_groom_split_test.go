package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestApplyGroomSplitsLosslessInertClusteredAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	owner := agentOwner("lead")
	content := "**Waveform speaker split**\nThe waveform path shipped with stable frame timing.\n\n**Goal-set workflow**\nThe goal-set path shipped with explicit owner review."
	parentID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, AuthorRef: "author", Repo: "acme/widget", Scope: "repo",
		Key: "editor-session", Content: content, Provenance: "ingest:session.md", SourceJob: "job-source",
	})
	if err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	otherID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "other",
		Content: "The waveform path has stable frame timing and owner review.", Provenance: "test",
	})
	if err != nil {
		t.Fatalf("seed linked fact: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO memory_clusters (cluster_id, label) VALUES (7, 'shipping')`); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	if err := store.AssignMemoryToCluster(ctx, parentID, 7); err != nil {
		t.Fatalf("assign parent cluster: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT OR REPLACE INTO memory_links (src_id, dst_id, score, origin, created_at) VALUES (?, ?, 2, 'test', 'now')`, otherID, parentID); err != nil {
		t.Fatalf("seed parent target link: %v", err)
	}
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM confirmed_memories WHERE id = ?`, parentID).Scan(&updatedAt); err != nil {
		t.Fatalf("read parent revision: %v", err)
	}
	children := []GroomSplitChild{
		{Key: "editor-session-waveform-speaker-split", Content: "**Waveform speaker split**\nThe waveform path shipped with stable frame timing.\n\n"},
		{Key: "editor-session-goal-set-workflow", Content: "**Goal-set workflow**\nThe goal-set path shipped with explicit owner review."},
	}
	item := GroomSplitItem{ParentID: parentID, ExpectedUpdatedAt: updatedAt, Children: children}
	result, err := store.ApplyGroomSplits(ctx, []GroomSplitItem{item})
	if err != nil {
		t.Fatalf("apply split: %v", err)
	}
	if len(result.Applied) != 1 || len(result.Applied[0].ChildIDs) != 2 || len(result.Skipped) != 0 {
		t.Fatalf("split result = %+v", result)
	}
	childIDs := result.Applied[0].ChildIDs

	var supersededBy int64
	if err := store.db.QueryRowContext(ctx, `SELECT superseded_by FROM confirmed_memories WHERE id = ?`, parentID).Scan(&supersededBy); err != nil {
		t.Fatalf("read superseded parent: %v", err)
	}
	if supersededBy != childIDs[0] {
		t.Fatalf("parent superseded_by = %d, want first child %d", supersededBy, childIDs[0])
	}
	if n := ftsRowCount(t, store, parentID); n != 0 {
		t.Fatalf("parent must leave FTS, found %d rows", n)
	}
	for _, id := range childIDs {
		if n := ftsRowCount(t, store, id); n != 1 {
			t.Fatalf("child %d FTS rows = %d, want 1", id, n)
		}
	}

	var joined string
	for _, id := range childIDs {
		var ownerKind, ownerRef, authorRef, repo, scope, contextValue, provenance, sourceJob, childContent string
		if err := store.db.QueryRowContext(ctx, `
SELECT owner_kind, owner_ref, author_ref, repo, scope, context, provenance, source_job, content
FROM confirmed_memories WHERE id = ?`, id).Scan(
			&ownerKind, &ownerRef, &authorRef, &repo, &scope, &contextValue, &provenance, &sourceJob, &childContent); err != nil {
			t.Fatalf("read child %d: %v", id, err)
		}
		if ownerKind != "agent" || ownerRef != "lead" || authorRef != "author" || repo != "acme/widget" || scope != "repo" || contextValue != "editor-session" || provenance != "groom-split:"+itoa(parentID) || sourceJob != "job-source" {
			t.Fatalf("child %d did not inherit metadata: owner=%s/%s author=%q repo=%q scope=%q context=%q provenance=%q source=%q", id, ownerKind, ownerRef, authorRef, repo, scope, contextValue, provenance, sourceJob)
		}
		joined += childContent
	}
	if joined != content {
		t.Fatalf("child coverage = %q, want %q", joined, content)
	}

	members, err := store.ListMemoryClusterMembers(ctx)
	if err != nil {
		t.Fatalf("list cluster members: %v", err)
	}
	seenChildren := map[int64]bool{}
	for _, member := range members {
		if member.MemoryID == parentID {
			t.Fatal("superseded parent leaked through cluster read path")
		}
		if member.ClusterID == 7 {
			seenChildren[member.MemoryID] = true
		}
	}
	if !seenChildren[childIDs[0]] || !seenChildren[childIDs[1]] {
		t.Fatalf("children did not inherit parent cluster: %+v", members)
	}

	vault, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("list vault: %v", err)
	}
	for _, row := range vault {
		if row.ID == parentID {
			t.Fatal("superseded parent leaked through active confirmed read path")
		}
	}
	links, err := store.ListMemoryLinks(ctx, otherID)
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	for _, link := range links {
		if link.DstID == parentID {
			t.Fatal("superseded parent leaked through link target read path")
		}
	}
	matches, err := store.QueryConfirmedMemories(ctx, owner, "acme/widget", `"waveform" OR "goal"`, 10)
	if err != nil {
		t.Fatalf("query confirmed: %v", err)
	}
	for _, match := range matches {
		if match.ID == parentID {
			t.Fatal("superseded parent leaked through FTS query")
		}
	}

	second, err := store.ApplyGroomSplits(ctx, []GroomSplitItem{item})
	if err != nil {
		t.Fatalf("re-run split: %v", err)
	}
	if len(second.Applied) != 0 || len(second.Skipped) != 1 || second.Skipped[0] != parentID {
		t.Fatalf("re-run must be a no-op: %+v", second)
	}
	var childCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM confirmed_memories WHERE provenance = ?`, "groom-split:"+itoa(parentID)).Scan(&childCount); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if childCount != 2 {
		t.Fatalf("re-run created children: count=%d", childCount)
	}

	dedup, err := store.ObservationDedupKeys(ctx, "lead")
	if err != nil {
		t.Fatalf("dedup keys: %v", err)
	}
	if _, ok := dedup[MemoryDedupKey("repo", "acme/widget", sha256HexOf(content))]; !ok {
		t.Fatal("retained superseded parent must dedup re-ingest of its source content")
	}
}

func TestApplyGroomSplitsCoverageViolationRollsBack(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	parentID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: agentOwner("lead"), Key: "parent", Content: "alpha beta gamma delta", Provenance: "test",
	})
	if err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM confirmed_memories WHERE id = ?`, parentID).Scan(&updatedAt); err != nil {
		t.Fatalf("read revision: %v", err)
	}
	_, err = store.ApplyGroomSplits(ctx, []GroomSplitItem{{
		ParentID: parentID, ExpectedUpdatedAt: updatedAt,
		Children: []GroomSplitChild{{Key: "parent-a", Content: "alpha beta"}, {Key: "parent-b", Content: "delta"}},
	}})
	if err == nil {
		t.Fatal("expected coverage violation")
	}
	var rows, fts int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM confirmed_memories`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM confirmed_memories_fts`).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || fts != 1 {
		t.Fatalf("coverage failure must roll back: rows=%d fts=%d", rows, fts)
	}
}

func TestRevertGroomSplitsHappyDryRunAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	parentID, childIDs, _ := seedAppliedGroomSplit(t, store)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO memory_clusters(cluster_id, label) VALUES (9, 'current-child-cluster')`); err != nil {
		t.Fatalf("seed moved cluster: %v", err)
	}
	if err := store.AssignMemoryToCluster(ctx, childIDs[0], 9); err != nil {
		t.Fatalf("move lowest child: %v", err)
	}

	dry, err := store.RevertGroomSplits(ctx, GroomSplitRevertOptions{ParentIDs: []int64{parentID}, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run revert: %v", err)
	}
	if len(dry.Reverted) != 1 || len(dry.Skipped) != 0 || len(dry.Reverted[0].ChildIDs) != 2 {
		t.Fatalf("dry-run result = %+v", dry)
	}
	if n := ftsRowCount(t, store, parentID); n != 0 {
		t.Fatalf("dry run restored parent FTS: %d", n)
	}

	result, err := store.RevertGroomSplits(ctx, GroomSplitRevertOptions{ParentIDs: []int64{parentID}})
	if err != nil {
		t.Fatalf("revert split: %v", err)
	}
	if len(result.Reverted) != 1 || result.Reverted[0].ParentID != parentID || len(result.Skipped) != 0 {
		t.Fatalf("revert result = %+v", result)
	}
	var superseded sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `SELECT superseded_by FROM confirmed_memories WHERE id = ?`, parentID).Scan(&superseded); err != nil {
		t.Fatalf("read restored parent: %v", err)
	}
	if superseded.Valid || ftsRowCount(t, store, parentID) != 1 {
		t.Fatalf("parent not restored: superseded=%+v fts=%d", superseded, ftsRowCount(t, store, parentID))
	}
	var clusterID int64
	if err := store.db.QueryRowContext(ctx, `SELECT cluster_id FROM memory_cluster_members WHERE memory_id = ?`, parentID).Scan(&clusterID); err != nil {
		t.Fatalf("read restored parent cluster: %v", err)
	}
	if clusterID != 9 {
		t.Fatalf("restored parent cluster = %d, want lowest child's current cluster 9", clusterID)
	}
	for _, childID := range childIDs {
		var retiredAt, reason string
		if err := store.db.QueryRowContext(ctx, `SELECT retired_at, retired_reason FROM confirmed_memories WHERE id = ?`, childID).Scan(&retiredAt, &reason); err != nil {
			t.Fatalf("read retired child %d: %v", childID, err)
		}
		if retiredAt == "" || reason != "groom-split-revert:"+itoa(parentID) || ftsRowCount(t, store, childID) != 0 {
			t.Fatalf("child %d not retired correctly: retired=%q reason=%q fts=%d", childID, retiredAt, reason, ftsRowCount(t, store, childID))
		}
		var memberships int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_cluster_members WHERE memory_id = ?`, childID).Scan(&memberships); err != nil || memberships != 0 {
			t.Fatalf("child %d cluster memberships = %d, err=%v", childID, memberships, err)
		}
	}

	second, err := store.RevertGroomSplits(ctx, GroomSplitRevertOptions{})
	if err != nil {
		t.Fatalf("idempotent revert: %v", err)
	}
	if len(second.Reverted) != 0 || len(second.Skipped) != 0 {
		t.Fatalf("second revert = %+v, want no-op", second)
	}
}

func TestRevertGroomSplitsEditedChildFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	parentID, childIDs, _ := seedAppliedGroomSplit(t, store)
	if _, err := store.db.ExecContext(ctx, `UPDATE confirmed_memories SET content = content || ' edited' WHERE id = ?`, childIDs[1]); err != nil {
		t.Fatalf("edit child: %v", err)
	}

	result, err := store.RevertGroomSplits(ctx, GroomSplitRevertOptions{})
	if err != nil {
		t.Fatalf("revert edited split: %v", err)
	}
	if len(result.Reverted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].ParentID != parentID {
		t.Fatalf("edited-child result = %+v", result)
	}
	var supersededBy int64
	if err := store.db.QueryRowContext(ctx, `SELECT superseded_by FROM confirmed_memories WHERE id = ?`, parentID).Scan(&supersededBy); err != nil {
		t.Fatal(err)
	}
	if supersededBy != childIDs[0] || ftsRowCount(t, store, parentID) != 0 {
		t.Fatalf("failed-closed parent changed: superseded=%d fts=%d", supersededBy, ftsRowCount(t, store, parentID))
	}
	for _, childID := range childIDs {
		var retiredAt string
		if err := store.db.QueryRowContext(ctx, `SELECT retired_at FROM confirmed_memories WHERE id = ?`, childID).Scan(&retiredAt); err != nil || retiredAt != "" {
			t.Fatalf("child %d retired on skipped revert: retired=%q err=%v", childID, retiredAt, err)
		}
	}
}

func TestRevertGroomSplitsAllowsIdenticalKeyResplit(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	parentID, firstChildIDs, item := seedAppliedGroomSplit(t, store)
	if _, err := store.RevertGroomSplits(ctx, GroomSplitRevertOptions{}); err != nil {
		t.Fatalf("revert first split: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM confirmed_memories WHERE id = ?`, parentID).Scan(&item.ExpectedUpdatedAt); err != nil {
		t.Fatalf("read restored revision: %v", err)
	}
	second, err := store.ApplyGroomSplits(ctx, []GroomSplitItem{item})
	if err != nil {
		t.Fatalf("re-split: %v", err)
	}
	if len(second.Applied) != 1 {
		t.Fatalf("re-split result = %+v", second)
	}
	for i, childID := range second.Applied[0].ChildIDs {
		if childID == firstChildIDs[i] {
			t.Fatalf("re-split reused retired row id %d", childID)
		}
		var key string
		if err := store.db.QueryRowContext(ctx, `SELECT key FROM confirmed_memories WHERE id = ?`, childID).Scan(&key); err != nil {
			t.Fatal(err)
		}
		if key != item.Children[i].Key {
			t.Fatalf("re-split child key = %q, want %q", key, item.Children[i].Key)
		}
	}
}

func seedAppliedGroomSplit(t *testing.T, store *Store) (int64, []int64, GroomSplitItem) {
	t.Helper()
	ctx := context.Background()
	content := "**First story**\nThe first story has exact durable content.\n\n**Second story**\nThe second story has exact durable content."
	parentID, err := store.UpsertConfirmedMemory(ctx, ConfirmedMemory{
		Owner: agentOwner("lead"), Repo: "acme/widget", Scope: "repo", Key: "parent-subject", Content: content, Provenance: "test",
	})
	if err != nil {
		t.Fatalf("seed split parent: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO memory_clusters(cluster_id, label) VALUES (7, 'original')`); err != nil {
		t.Fatalf("seed original cluster: %v", err)
	}
	if err := store.AssignMemoryToCluster(ctx, parentID, 7); err != nil {
		t.Fatalf("assign original cluster: %v", err)
	}
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM confirmed_memories WHERE id = ?`, parentID).Scan(&updatedAt); err != nil {
		t.Fatalf("read split parent revision: %v", err)
	}
	item := GroomSplitItem{
		ParentID: parentID, ExpectedUpdatedAt: updatedAt,
		Children: []GroomSplitChild{
			{Key: "parent-subject-first-story", Content: "**First story**\nThe first story has exact durable content.\n\n"},
			{Key: "parent-subject-second-story", Content: "**Second story**\nThe second story has exact durable content."},
		},
	}
	result, err := store.ApplyGroomSplits(ctx, []GroomSplitItem{item})
	if err != nil {
		t.Fatalf("apply seeded split: %v", err)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("seed split result = %+v", result)
	}
	return parentID, result.Applied[0].ChildIDs, item
}

func itoa(id int64) string {
	return fmt.Sprintf("%d", id)
}
