package db

import (
	"context"
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
		var ownerKind, ownerRef, authorRef, repo, scope, provenance, sourceJob, childContent string
		if err := store.db.QueryRowContext(ctx, `
SELECT owner_kind, owner_ref, author_ref, repo, scope, provenance, source_job, content
FROM confirmed_memories WHERE id = ?`, id).Scan(
			&ownerKind, &ownerRef, &authorRef, &repo, &scope, &provenance, &sourceJob, &childContent); err != nil {
			t.Fatalf("read child %d: %v", id, err)
		}
		if ownerKind != "agent" || ownerRef != "lead" || authorRef != "author" || repo != "acme/widget" || scope != "repo" || provenance != "groom-split:"+itoa(parentID) || sourceJob != "job-source" {
			t.Fatalf("child %d did not inherit metadata: owner=%s/%s author=%q repo=%q scope=%q provenance=%q source=%q", id, ownerKind, ownerRef, authorRef, repo, scope, provenance, sourceJob)
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

func itoa(id int64) string {
	return fmt.Sprintf("%d", id)
}
