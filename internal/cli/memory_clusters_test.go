package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

// seedClusterCorpus seeds two clearly separable communities (database facts vs
// network facts) sharing an owner/repo so they are mutually FTS-visible, returning
// the two id sets.
func seedClusterCorpus(t *testing.T, store *db.Store) (dbIDs, netIDs []int64) {
	t.Helper()
	owner := db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: "researcher"}
	dbIDs = []int64{
		seedConfirmed(t, store, owner, "acme/widget", "repo", "db-index", "database index speeds up query lookups on the table"),
		seedConfirmed(t, store, owner, "acme/widget", "repo", "db-cache", "database query cache reduces index scan latency"),
		seedConfirmed(t, store, owner, "acme/widget", "repo", "db-vacuum", "database vacuum reclaims index bloat after query churn"),
	}
	netIDs = []int64{
		seedConfirmed(t, store, owner, "acme/widget", "repo", "net-retry", "network retry backoff handles socket timeout gracefully"),
		seedConfirmed(t, store, owner, "acme/widget", "repo", "net-pool", "network socket pool reuses connections to cut retry cost"),
		seedConfirmed(t, store, owner, "acme/widget", "repo", "net-tls", "network tls handshake adds socket setup before retry"),
	}
	return dbIDs, netIDs
}

func clustersJSON(t *testing.T, home string) []clusterListEntry {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"clusters", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory clusters exit %d: %s", code, stderr.String())
	}
	var entries []clusterListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("parse clusters json: %v (%s)", err, stdout.String())
	}
	return entries
}

// TestClustersFirstRunApplyAndList: on first run `recompute --apply` (no plan) is
// allowed; it builds the two communities and the list surfaces them.
func TestClustersFirstRunApplyAndList(t *testing.T) {
	home, store := memoryTestHome(t)
	dbIDs, netIDs := seedClusterCorpus(t, store)

	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"clusters", "recompute", "--apply", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("first-run apply exit %d: %s", code, stderr.String())
	}

	entries := clustersJSON(t, home)
	real := 0
	for _, e := range entries {
		if e.ClusterID != memory.UnclusteredID {
			real++
		}
	}
	if real != 2 {
		t.Fatalf("real clusters = %d, want 2: %+v", real, entries)
	}

	// The three db facts share one cluster; the three net facts share another; the
	// two clusters differ.
	ctx := context.Background()
	dbc := clusterOf(t, store, ctx, dbIDs[0])
	for _, id := range dbIDs {
		if got := clusterOf(t, store, ctx, id); got != dbc {
			t.Fatalf("db fact %d in cluster %d, want %d (same as sibling)", id, got, dbc)
		}
	}
	netc := clusterOf(t, store, ctx, netIDs[0])
	for _, id := range netIDs {
		if got := clusterOf(t, store, ctx, id); got != netc {
			t.Fatalf("net fact %d in cluster %d, want %d", id, got, netc)
		}
	}
	if dbc == netc {
		t.Fatalf("db and net facts must be in different clusters (both %d)", dbc)
	}
}

// TestClustersApplyWithoutPlanRejectedWhenExist: once clusters exist, a bare
// `--apply` (no plan) must be rejected — recompute has to go through propose.
func TestClustersApplyWithoutPlanRejectedWhenExist(t *testing.T) {
	home, store := memoryTestHome(t)
	seedClusterCorpus(t, store)

	var b bytes.Buffer
	if code := runMemory([]string{"clusters", "recompute", "--apply", "--home", home}, &b, &b); code != 0 {
		t.Fatalf("first apply exit %d: %s", code, b.String())
	}
	b.Reset()
	code := runMemory([]string{"clusters", "recompute", "--apply", "--home", home}, &b, &b)
	if code == 0 {
		t.Fatalf("bare --apply with existing clusters must fail")
	}
	if !bytes.Contains(b.Bytes(), []byte("first run")) {
		t.Fatalf("error should explain first-run-only; got %s", b.String())
	}
}

// TestClustersProposeApplyStalenessAbort: a plan proposed then invalidated by a
// new fact must abort as stale at apply.
func TestClustersProposeApplyStalenessAbort(t *testing.T) {
	home, store := memoryTestHome(t)
	seedClusterCorpus(t, store)
	ctx := context.Background()

	// Build an initial clustering so recompute goes through the propose path.
	var b bytes.Buffer
	if code := runMemory([]string{"clusters", "recompute", "--apply", "--home", home}, &b, &b); code != 0 {
		t.Fatalf("seed apply exit %d: %s", code, b.String())
	}

	planPath := filepath.Join(t.TempDir(), "plan.json")
	b.Reset()
	if code := runMemory([]string{"clusters", "recompute", "--propose", "--home", home, "--out", planPath}, &b, &b); code != 0 {
		t.Fatalf("propose exit %d: %s", code, b.String())
	}

	// Mutate the store AFTER proposing: the anchor over (id, updated_at) changes.
	if _, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: "researcher"},
		Repo:  "acme/widget", Scope: "repo", Key: "db-migrate", Content: "database migration rewrites the index and query plan",
	}); err != nil {
		t.Fatalf("mutate store: %v", err)
	}

	b.Reset()
	code := runMemory([]string{"clusters", "recompute", "--apply", "--plan", planPath, "--home", home}, &b, &b)
	if code == 0 {
		t.Fatalf("stale apply must fail")
	}
	if !bytes.Contains(b.Bytes(), []byte("stale")) {
		t.Fatalf("error should mention staleness; got %s", b.String())
	}
}

// TestClusterRenameOverrideWinsAndSurvivesRecompute: rename sets an override that
// wins in the list AND is carried forward across a recompute (medoid-anchored).
func TestClusterRenameOverrideWinsAndSurvivesRecompute(t *testing.T) {
	home, store := memoryTestHome(t)
	seedClusterCorpus(t, store)

	var b bytes.Buffer
	if code := runMemory([]string{"clusters", "recompute", "--apply", "--home", home}, &b, &b); code != 0 {
		t.Fatalf("apply exit %d: %s", code, b.String())
	}

	// Pick a real cluster to rename.
	var target int64 = -1
	for _, e := range clustersJSON(t, home) {
		if e.ClusterID != memory.UnclusteredID {
			target = e.ClusterID
			break
		}
	}
	if target < 0 {
		t.Fatalf("no real cluster to rename")
	}

	b.Reset()
	if code := runMemory([]string{"cluster", "rename", "--home", home}, &b, &b); code == 0 {
		t.Fatalf("rename with no args must fail usage")
	}
	b.Reset()
	if code := runMemory([]string{"cluster", "rename", itoaTest(target), "storage-layer", "--home", home}, &b, &b); code != 0 {
		t.Fatalf("rename exit %d: %s", code, b.String())
	}

	assertOverride := func(where string) {
		for _, e := range clustersJSON(t, home) {
			if e.ClusterID == target {
				if e.Override != "storage-layer" || e.Label != "storage-layer" {
					t.Fatalf("%s: cluster %d label=%q override=%q, want display+override 'storage-layer'", where, target, e.Label, e.Override)
				}
				return
			}
		}
		t.Fatalf("%s: cluster %d missing", where, target)
	}
	assertOverride("after rename")

	// Recompute (propose+apply, no store change) must preserve the override.
	planPath := filepath.Join(t.TempDir(), "plan.json")
	b.Reset()
	if code := runMemory([]string{"clusters", "recompute", "--propose", "--home", home, "--out", planPath}, &b, &b); code != 0 {
		t.Fatalf("propose exit %d: %s", code, b.String())
	}
	b.Reset()
	if code := runMemory([]string{"clusters", "recompute", "--apply", "--plan", planPath, "--home", home}, &b, &b); code != 0 {
		t.Fatalf("apply exit %d: %s", code, b.String())
	}
	assertOverride("after recompute")
}

// TestClustersProposeDeterministic: two proposes over an unchanged store produce
// the same anchor and the same cluster assignment.
func TestClustersProposeDeterministic(t *testing.T) {
	home, store := memoryTestHome(t)
	seedClusterCorpus(t, store)

	propose := func() clusterPlan {
		planPath := filepath.Join(t.TempDir(), "plan.json")
		var b bytes.Buffer
		if code := runMemory([]string{"clusters", "recompute", "--propose", "--home", home, "--out", planPath}, &b, &b); code != 0 {
			t.Fatalf("propose exit %d: %s", code, b.String())
		}
		p, err := readClusterPlan(planPath)
		if err != nil {
			t.Fatalf("read plan: %v", err)
		}
		return p
	}
	a := propose()
	c := propose()
	if a.Anchor != c.Anchor {
		t.Fatalf("anchors differ: %s vs %s", a.Anchor, c.Anchor)
	}
	if got, want := toJSON(t, a.Clusters), toJSON(t, c.Clusters); got != want {
		t.Fatalf("cluster assignment not deterministic:\n%s\n%s", got, want)
	}
}

// TestClustersIncrementalAttach: a newly confirmed fact attaches to the cluster of
// its nearest neighbor.
func TestClustersIncrementalAttach(t *testing.T) {
	home, store := memoryTestHome(t)
	dbIDs, _ := seedClusterCorpus(t, store)
	ctx := context.Background()

	var b bytes.Buffer
	if code := runMemory([]string{"clusters", "recompute", "--apply", "--home", home}, &b, &b); code != 0 {
		t.Fatalf("apply exit %d: %s", code, b.String())
	}
	dbc := clusterOf(t, store, ctx, dbIDs[0])

	// Stage a new DB-flavored observation and confirm it.
	obsID, err := store.InsertMemoryObservation(ctx, db.MemoryObservation{
		Owner: db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: "researcher"},
		Repo:  "acme/widget", Scope: "repo", Key: "db-shard",
		Content: "database sharding splits the index across query nodes", TrustMark: "normal",
	})
	if err != nil {
		t.Fatalf("insert obs: %v", err)
	}

	b.Reset()
	if code := runMemory([]string{"confirm", "--yes", "--home", home, itoaTest(obsID)}, &b, &b); code != 0 {
		t.Fatalf("confirm exit %d: %s", code, b.String())
	}

	// Resolve the confirmed row id for the new key and assert it joined the db cluster.
	newID := confirmedIDForKey(t, store, ctx, "db-shard")
	got := clusterOf(t, store, ctx, newID)
	if got != dbc {
		t.Fatalf("new fact attached to cluster %d, want %d (nearest-neighbor db cluster)", got, dbc)
	}
}

// ---- small test helpers ----

func clusterOf(t *testing.T, store *db.Store, ctx context.Context, id int64) int64 {
	t.Helper()
	cid, ok, err := store.ClusterOfMemory(ctx, id)
	if err != nil {
		t.Fatalf("cluster of %d: %v", id, err)
	}
	if !ok {
		t.Fatalf("fact %d has no cluster", id)
	}
	return cid
}

func confirmedIDForKey(t *testing.T, store *db.Store, ctx context.Context, key string) int64 {
	t.Helper()
	rows, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		t.Fatalf("list confirmed: %v", err)
	}
	for _, r := range rows {
		if r.Key == key {
			return r.ID
		}
	}
	t.Fatalf("no confirmed fact with key %q", key)
	return 0
}

func itoaTest(n int64) string {
	return strconv.FormatInt(n, 10)
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
