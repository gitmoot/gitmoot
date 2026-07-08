package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrClusterPlanStale is returned by RecomputeMemoryClustersFresh when the
// active-fact anchor re-read inside the rewrite transaction no longer matches the
// anchor the reviewed plan was built against — a fact was confirmed/edited/retired
// in the window. The caller re-proposes rather than applying a stale plan.
var ErrClusterPlanStale = errors.New("cluster plan is stale")

// This file is the store layer for emergent memory clusters (#763 Track A). It
// persists the deterministic community detection computed in internal/memory:
// memory_clusters (one row per community + the reserved cluster 0 'unclustered'
// bucket) and memory_cluster_members (fact -> cluster). The clustering itself is
// a pure function elsewhere; this layer is only SQL. Every read orders by id so
// callers see a stable traversal.

// MemoryCluster is one persisted community. Label is the computed
// distinctive-term label; LabelOverride is the owner's `memory cluster rename`
// (it wins when non-empty). MedoidID anchors the label for stability.
type MemoryCluster struct {
	ClusterID     int64
	Label         string
	LabelOverride string
	MedoidID      int64
}

// DisplayLabel is the label the bridge/CLI should show: the override when the
// owner set one, else the computed label.
func (c MemoryCluster) DisplayLabel() string {
	if c.LabelOverride != "" {
		return c.LabelOverride
	}
	return c.Label
}

// MemoryClusterMember maps one active confirmed fact to its cluster.
type MemoryClusterMember struct {
	MemoryID  int64
	ClusterID int64
}

// MemoryClusterAssignment is the full input the recompute writes in one tx: the
// cluster rows and their membership. The caller (CLI) builds these from the pure
// internal/memory clustering.
type MemoryClusterAssignment struct {
	Clusters []MemoryCluster
	Members  []MemoryClusterMember
}

// ListMemoryClusters returns every persisted cluster ordered by cluster_id, so
// the reserved unclustered bucket (id 0) sorts first and real communities follow
// in their stable numbering.
func (s *Store) ListMemoryClusters(ctx context.Context) ([]MemoryCluster, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT cluster_id, label, label_override, medoid_id
FROM memory_clusters
ORDER BY cluster_id`)
	if err != nil {
		return nil, fmt.Errorf("list memory clusters: %w", err)
	}
	defer rows.Close()
	var out []MemoryCluster
	for rows.Next() {
		var c MemoryCluster
		if err := rows.Scan(&c.ClusterID, &c.Label, &c.LabelOverride, &c.MedoidID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListMemoryClusterMembers returns every fact->cluster row ordered by memory_id,
// for the bridge and the CLI count roll-up.
func (s *Store) ListMemoryClusterMembers(ctx context.Context) ([]MemoryClusterMember, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT memory_id, cluster_id
FROM memory_cluster_members
ORDER BY memory_id`)
	if err != nil {
		return nil, fmt.Errorf("list memory cluster members: %w", err)
	}
	defer rows.Close()
	var out []MemoryClusterMember
	for rows.Next() {
		var m MemoryClusterMember
		if err := rows.Scan(&m.MemoryID, &m.ClusterID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountMemoryClusters returns how many cluster rows exist. Used by the CLI to
// detect the first-run case (no clusters yet ⇒ `recompute --apply` is allowed
// without a proposal to protect).
func (s *Store) CountMemoryClusters(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_clusters`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count memory clusters: %w", err)
	}
	return n, nil
}

// RecomputeMemoryClusters replaces the ENTIRE clustering in one transaction:
// every memory_clusters and memory_cluster_members row is deleted and the new
// assignment inserted. Owner label overrides are carried forward by MEDOID
// identity — a new cluster whose medoid matches a prior cluster that carried an
// override keeps that override (the medoid anchors owner intent across a
// recompute, since cluster_id numbering itself is not stable across membership
// changes). The whole swap is atomic: a reader never sees a half-written
// clustering.
func (s *Store) RecomputeMemoryClusters(ctx context.Context, a MemoryClusterAssignment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := recomputeMemoryClustersTx(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit()
}

// RecomputeMemoryClustersFresh performs the destructive clustering rewrite ONLY if
// the active-fact anchor — re-read from the SAME transaction that performs the
// delete/insert — still equals expected. This closes the TOCTOU window between the
// CLI's staleness pre-check and the rewrite: a concurrent confirm/attach in that
// window would otherwise be silently dropped by the DELETE. anchorFn computes the
// anchor from the tx-scoped active vault rows using the SAME algorithm that
// produced expected. Because the anchor read and the rewrite share one transaction,
// a fact confirmed after the snapshot either changes the anchor (→ ErrClusterPlanStale)
// or invalidates the write snapshot (→ SQLITE_BUSY_SNAPSHOT); the stale row can no
// longer be dropped unnoticed. Returns ErrClusterPlanStale (wrapped) if the anchor
// moved; the whole swap is atomic.
func (s *Store) RecomputeMemoryClustersFresh(ctx context.Context, a MemoryClusterAssignment, expected string, anchorFn func([]ConfirmedMemory) string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := listConfirmedMemoriesForVault(ctx, tx, "")
	if err != nil {
		return err
	}
	if got := anchorFn(rows); got != expected {
		return fmt.Errorf("%w (plan anchor %s, current %s)", ErrClusterPlanStale, expected, got)
	}
	if err := recomputeMemoryClustersTx(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit()
}

// recomputeMemoryClustersTx is the delete-all + reinsert core of a full clustering
// rewrite, run inside the caller's transaction so it can be paired atomically with
// a same-tx anchor re-read (RecomputeMemoryClustersFresh) or run standalone
// (RecomputeMemoryClusters). Owner label overrides are carried forward by medoid
// identity.
func recomputeMemoryClustersTx(ctx context.Context, tx *sql.Tx, a MemoryClusterAssignment) error {
	// Preserve overrides keyed by medoid id before wiping.
	overrideByMedoid, err := overridesByMedoidTx(ctx, tx)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_cluster_members`); err != nil {
		return fmt.Errorf("clear cluster members: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_clusters`); err != nil {
		return fmt.Errorf("clear clusters: %w", err)
	}

	for _, c := range a.Clusters {
		override := c.LabelOverride
		if override == "" {
			if prev, ok := overrideByMedoid[c.MedoidID]; ok && c.MedoidID != 0 {
				override = prev
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_clusters (cluster_id, label, label_override, medoid_id)
VALUES (?, ?, ?, ?)`, c.ClusterID, c.Label, override, c.MedoidID); err != nil {
			return fmt.Errorf("insert cluster %d: %w", c.ClusterID, err)
		}
	}
	for _, m := range a.Members {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_cluster_members (memory_id, cluster_id) VALUES (?, ?)`,
			m.MemoryID, m.ClusterID); err != nil {
			return fmt.Errorf("insert cluster member %d: %w", m.MemoryID, err)
		}
	}
	return nil
}

// overridesByMedoidTx reads the current (medoid_id -> non-empty override) map
// inside a transaction. A zero medoid (the unclustered bucket) is skipped — its
// label is fixed and never carries an owner override.
func overridesByMedoidTx(ctx context.Context, tx *sql.Tx) (map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT medoid_id, label_override FROM memory_clusters
WHERE label_override <> '' AND medoid_id <> 0`)
	if err != nil {
		return nil, fmt.Errorf("read prior overrides: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var medoid int64
		var override string
		if err := rows.Scan(&medoid, &override); err != nil {
			return nil, err
		}
		out[medoid] = override
	}
	return out, rows.Err()
}

// AssignMemoryToCluster incrementally attaches a single fact to a cluster
// (INSERT OR REPLACE on the memory_id PK), used when a newly confirmed fact joins
// the cluster of its nearest neighbor without a full recompute. Fail-safe by
// design: the caller wraps it best-effort so a failed attach never blocks a
// confirm.
func (s *Store) AssignMemoryToCluster(ctx context.Context, memoryID, clusterID int64) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO memory_cluster_members (memory_id, cluster_id) VALUES (?, ?)
ON CONFLICT(memory_id) DO UPDATE SET cluster_id = excluded.cluster_id`,
		memoryID, clusterID); err != nil {
		return fmt.Errorf("assign memory %d to cluster %d: %w", memoryID, clusterID, err)
	}
	return nil
}

// RenameMemoryCluster sets a cluster's owner label override (the display label
// then wins over the computed label). A blank label clears the override, falling
// back to the computed label. Renaming the reserved unclustered bucket (id 0) is
// rejected — its grouping is structural, not a named community.
func (s *Store) RenameMemoryCluster(ctx context.Context, clusterID int64, label string) error {
	if clusterID == 0 {
		return fmt.Errorf("cannot rename the reserved unclustered bucket")
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE memory_clusters SET label_override = ? WHERE cluster_id = ?`, label, clusterID)
	if err != nil {
		return fmt.Errorf("rename cluster %d: %w", clusterID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no cluster with id %d", clusterID)
	}
	return nil
}

// ClusterOfMemory returns the cluster id a fact currently belongs to, and whether
// it has an assignment. Used by incremental attach to find a neighbor's cluster.
func (s *Store) ClusterOfMemory(ctx context.Context, memoryID int64) (int64, bool, error) {
	var cid int64
	err := s.db.QueryRowContext(ctx, `
SELECT cluster_id FROM memory_cluster_members WHERE memory_id = ?`, memoryID).Scan(&cid)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("cluster of memory %d: %w", memoryID, err)
	}
	return cid, true, nil
}

// MemoryClusterCounts returns member counts keyed by cluster id, deterministically
// (a plain read the caller can pair with ListMemoryClusters). Sorted keys are the
// caller's job; this returns the map.
func (s *Store) MemoryClusterCounts(ctx context.Context) (map[int64]int, error) {
	members, err := s.ListMemoryClusterMembers(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]int{}
	for _, m := range members {
		out[m.ClusterID]++
	}
	return out, nil
}
