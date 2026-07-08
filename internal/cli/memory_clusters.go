package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

// memory clusters (#763 Track A) — emergent community detection over the fact
// similarity graph, surfaced as an explicit list + a human-gated propose→apply
// recompute round-trip, mirroring the `memory groom` staleness-anchored pattern.
//
// The clustering is DETERMINISTIC: the graph is the same bm25+id-tiebreak signal
// the vault [[links]] use, and the community detection (internal/memory) is a
// pure function with fixed iteration order and tie-breaks — the same store always
// yields byte-identical clusters, labels, medoids, and ids.

// clusterPlanSchemaVersion is the on-disk recompute-plan schema version.
const clusterPlanSchemaVersion = 1

// clusterLinkK is the k-NN degree per fact — the same neighbor count the vault
// links render (vaultLinkK), so clusters emerge from the exact same signal.
const clusterLinkK = vaultLinkK

// clusterPlanCluster is one proposed community in the plan (the apply path writes
// these verbatim). Members are ascending fact ids.
type clusterPlanCluster struct {
	ClusterID int64   `json:"cluster_id"`
	Label     string  `json:"label"`
	MedoidID  int64   `json:"medoid_id"`
	Members   []int64 `json:"members"`
}

// clusterPlanMove is one fact that lands in a different cluster than it currently
// occupies (a review-only diff; apply writes the whole assignment, not moves).
type clusterPlanMove struct {
	MemoryID    int64 `json:"memory_id"`
	FromCluster int64 `json:"from_cluster"`
	ToCluster   int64 `json:"to_cluster"`
}

// clusterPlan is the reviewable artifact `recompute --propose` writes and
// `recompute --apply --plan` reads. Anchor is the staleness guard over the active
// facts' (id, updated_at); a mismatch at apply aborts as stale.
type clusterPlan struct {
	SchemaVersion int                  `json:"schema_version"`
	Anchor        string               `json:"anchor"`
	Clusters      []clusterPlanCluster `json:"clusters"`
	Moves         []clusterPlanMove    `json:"moves"`
	NewFacts      []int64              `json:"new_facts,omitempty"`     // facts not previously in any cluster
	DroppedFacts  []int64              `json:"dropped_facts,omitempty"` // previously-clustered facts now gone/retired
	Stats         clusterPlanStats     `json:"stats"`
}

type clusterPlanStats struct {
	Facts       int `json:"facts"`
	Clusters    int `json:"clusters"`    // real communities (excludes the unclustered bucket)
	Unclustered int `json:"unclustered"` // facts in the reserved bucket
	Moves       int `json:"moves"`
}

// runMemoryClusters is the `gitmoot memory clusters ...` entry point (list +
// recompute).
func runMemoryClusters(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "recompute" {
		return runMemoryClustersRecompute(args[1:], stdout, stderr)
	}
	return runMemoryClustersList(args, stdout, stderr)
}

// ---- memory clusters (list) -----------------------------------------------

type clusterListEntry struct {
	ClusterID int64  `json:"cluster_id"`
	Label     string `json:"label"`
	Override  string `json:"label_override,omitempty"`
	MedoidID  int64  `json:"medoid_id"`
	Count     int    `json:"count"`
}

func runMemoryClustersList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory clusters", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}

	var entries []clusterListEntry
	err := withReadOnlyStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		clusters, err := store.ListMemoryClusters(ctx)
		if err != nil {
			return err
		}
		counts, err := store.MemoryClusterCounts(ctx)
		if err != nil {
			return err
		}
		entries = make([]clusterListEntry, 0, len(clusters))
		for _, c := range clusters {
			entries = append(entries, clusterListEntry{
				ClusterID: c.ClusterID, Label: c.DisplayLabel(), Override: c.LabelOverride,
				MedoidID: c.MedoidID, Count: counts[c.ClusterID],
			})
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory clusters: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := writeJSON(stdout, entries); err != nil {
			fmt.Fprintf(stderr, "memory clusters: %v\n", err)
			return 1
		}
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "no clusters (run `gitmoot memory clusters recompute --apply` to build them)")
		return 0
	}
	for _, e := range entries {
		fmt.Fprintf(stdout, "%-4d %-32s %5d facts (medoid %d)\n", e.ClusterID, e.Label, e.Count, e.MedoidID)
	}
	return 0
}

// ---- memory clusters recompute (propose/apply) ----------------------------

func runMemoryClustersRecompute(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory clusters recompute", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	propose := fs.Bool("propose", false, "compute clusters and write a reviewable plan (writes nothing to the store)")
	apply := fs.Bool("apply", false, "apply a plan (with --plan), or on first run compute+write directly")
	plan := fs.String("plan", "", "path to a plan produced by --propose (required with --apply once clusters exist)")
	out := fs.String("out", "", "where --propose writes the plan (default: <home>/evals/clusters/clusters-<anchor>.json)")
	jsonOut := fs.Bool("json", false, "print the summary as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	if *propose == *apply {
		fmt.Fprintln(stderr, "memory clusters recompute: pass exactly one of --propose or --apply")
		return 2
	}
	if *propose {
		return runMemoryClustersPropose(*home, *out, *jsonOut, stdout, stderr)
	}
	return runMemoryClustersApply(*home, *plan, *jsonOut, stdout, stderr)
}

func runMemoryClustersPropose(home, out string, jsonOut bool, stdout, stderr io.Writer) int {
	var plan clusterPlan
	err := withReadOnlyStore(home, func(store *db.Store) error {
		ctx := context.Background()
		built, anchor, err := buildClusterAssignment(ctx, store)
		if err != nil {
			return err
		}
		current, err := currentMembership(ctx, store)
		if err != nil {
			return err
		}
		plan = makeClusterPlan(built, anchor, current)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory clusters recompute: %v\n", err)
		return 1
	}

	outPath := out
	if outPath == "" {
		paths, perr := pathsFromFlag(home)
		if perr != nil {
			fmt.Fprintf(stderr, "memory clusters recompute: %v\n", perr)
			return 1
		}
		short := plan.Anchor
		if len(short) > 12 {
			short = short[:12]
		}
		outPath = filepath.Join(paths.Evals, "clusters", "clusters-"+short+".json")
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "memory clusters recompute: create plan dir: %v\n", err)
		return 1
	}
	if err := writeJSONFile(outPath, plan); err != nil {
		fmt.Fprintf(stderr, "memory clusters recompute: write plan: %v\n", err)
		return 1
	}

	if jsonOut {
		summary := struct {
			clusterPlan
			Out string `json:"out"`
		}{clusterPlan: plan, Out: outPath}
		if err := writeJSON(stdout, summary); err != nil {
			fmt.Fprintf(stderr, "memory clusters recompute: %v\n", err)
			return 1
		}
		return 0
	}
	printClusterProposal(stdout, plan, outPath)
	return 0
}

func runMemoryClustersApply(home, planPath string, jsonOut bool, stdout, stderr io.Writer) int {
	type applyResult struct {
		Anchor      string `json:"anchor"`
		Applied     bool   `json:"applied"`
		FirstRun    bool   `json:"first_run"`
		Clusters    int    `json:"clusters"`
		Unclustered int    `json:"unclustered"`
		Facts       int    `json:"facts"`
	}
	var result applyResult

	err := withStore(home, func(store *db.Store) error {
		ctx := context.Background()

		// First-run shortcut: with no clusters yet there is nothing to protect, so
		// `--apply` without a plan may compute + write directly.
		if planPath == "" {
			n, err := store.CountMemoryClusters(ctx)
			if err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("clusters already exist: `--apply` requires a reviewed `--plan` (run `--propose` first); a bare `--apply` is only allowed on first run")
			}
			built, anchor, err := buildClusterAssignment(ctx, store)
			if err != nil {
				return err
			}
			if err := store.RecomputeMemoryClusters(ctx, built.assignment); err != nil {
				return err
			}
			result = applyResult{Anchor: anchor, Applied: true, FirstRun: true,
				Clusters: built.realClusters, Unclustered: built.unclustered, Facts: built.facts}
			return nil
		}

		plan, err := readClusterPlan(planPath)
		if err != nil {
			return err
		}
		// Verify the CURRENT anchor and perform the destructive rewrite ATOMICALLY:
		// RecomputeMemoryClustersFresh re-reads the active-fact anchor inside the same
		// transaction as the delete/insert, so a fact confirmed/edited/retired in the
		// window between propose and apply cannot slip through — it either changes the
		// anchor (stale) or invalidates the write snapshot. A prior split (anchor read
		// then a separate rewrite tx) left a TOCTOU window where a concurrent
		// attachConfirmedFactToCluster member row was silently dropped by the DELETE.
		assignment := planAssignment(plan)
		if err := store.RecomputeMemoryClustersFresh(ctx, assignment, plan.Anchor, clusterAnchor); err != nil {
			if errors.Is(err, db.ErrClusterPlanStale) {
				return fmt.Errorf("plan is stale: the memory store changed since it was proposed (%v); re-run `gitmoot memory clusters recompute --propose` and re-review", err)
			}
			return err
		}
		real, unc := countPlanBuckets(plan)
		result = applyResult{Anchor: plan.Anchor, Applied: true, Clusters: real,
			Unclustered: unc, Facts: len(plan.members())}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory clusters recompute: %v\n", err)
		return 1
	}

	if jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory clusters recompute: %v\n", err)
			return 1
		}
		return 0
	}
	kind := "applied"
	if result.FirstRun {
		kind = "applied (first run)"
	}
	fmt.Fprintf(stdout, "%s: %d cluster(s), %d unclustered fact(s) across %d fact(s) (anchor %s)\n",
		kind, result.Clusters, result.Unclustered, result.Facts, result.Anchor)
	return 0
}

// ---- memory cluster rename ------------------------------------------------

func runMemoryClusterRename(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory cluster rename", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	// Documented form is `rename <cluster-id> <label> [flags]`, but Go's flag parser
	// stops at the first positional, so trailing --home would be silently dropped.
	// Peel the leading positionals (everything before the first flag) off the front,
	// then flag-parse the remainder — the `memory ingest` pattern, and it handles the
	// `--home <value>` form correctly.
	var positionals, flagArgs []string
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		positionals = append(positionals, args[i])
		i++
	}
	flagArgs = args[i:]
	if err := parseMemoryFlags(fs, flagArgs); err != nil {
		return memoryFlagExit(err)
	}
	if len(positionals) < 2 {
		fmt.Fprintln(stderr, "memory cluster rename: usage: gitmoot memory cluster rename <cluster-id> <label>")
		return 2
	}
	clusterID, err := strconv.ParseInt(strings.TrimSpace(positionals[0]), 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "memory cluster rename: invalid cluster id %q\n", positionals[0])
		return 2
	}
	label := strings.TrimSpace(strings.Join(positionals[1:], " "))
	if label == "" {
		fmt.Fprintln(stderr, "memory cluster rename: label cannot be empty")
		return 2
	}
	err = withStore(*home, func(store *db.Store) error {
		return store.RenameMemoryCluster(context.Background(), clusterID, label)
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory cluster rename: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "renamed cluster %d to %q\n", clusterID, label)
	return 0
}

// ---- graph construction + plan helpers ------------------------------------

// builtClustering bundles the pure clustering result mapped into a store
// assignment plus a few counts for reporting.
type builtClustering struct {
	assignment   db.MemoryClusterAssignment
	realClusters int
	unclustered  int
	facts        int
}

// buildClusterAssignment reads every active confirmed fact, builds the undirected
// k-NN similarity graph from the SAME bm25+id-tiebreak neighbor ranking the vault
// links use, runs the deterministic community detection, and maps the result into
// a store assignment. It also returns the staleness anchor over the active facts'
// (id, updated_at). Read-only.
func buildClusterAssignment(ctx context.Context, store *db.Store) (builtClustering, string, error) {
	rows, err := store.ListConfirmedMemoriesForVault(ctx, "")
	if err != nil {
		return builtClustering{}, "", err
	}

	nodes := make([]memory.ClusterNode, 0, len(rows))
	var edges []memory.ClusterEdge
	for _, r := range rows {
		// The similarity GRAPH reuses the exact vault-link signal (key+content, via
		// vaultLinksFor below); the label is recomputed from CONTENT only, so a fact's
		// mechanical key slug/hash (e.g. an ingest "untitled-<hash>" stem) never
		// pollutes a cluster label.
		nodes = append(nodes, memory.ClusterNode{ID: r.ID, Text: r.Content})
		links, lerr := vaultLinksFor(ctx, store, r)
		if lerr != nil {
			return builtClustering{}, "", lerr
		}
		// Rank-based directed weight: the closest neighbor (rank 0) contributes the
		// most. BuildClusters symmetrizes and sums both directions, so a mutually
		// top-ranked pair gets the heaviest undirected edge.
		for rank, l := range links {
			w := clusterLinkK - rank
			if w <= 0 {
				continue
			}
			edges = append(edges, memory.ClusterEdge{A: r.ID, B: l.TargetID, Weight: w})
		}
	}

	res := memory.BuildClusters(nodes, edges)
	built := builtClustering{facts: len(rows)}
	for _, c := range res.Clusters {
		built.assignment.Clusters = append(built.assignment.Clusters, db.MemoryCluster{
			ClusterID: c.ID, Label: c.Label, MedoidID: c.MedoidID,
		})
		for _, m := range c.Members {
			built.assignment.Members = append(built.assignment.Members, db.MemoryClusterMember{
				MemoryID: m, ClusterID: c.ID,
			})
		}
		if c.ID == memory.UnclusteredID {
			built.unclustered = len(c.Members)
		} else {
			built.realClusters++
		}
	}
	return built, clusterAnchor(rows), nil
}

// clusterAnchor is the deterministic staleness anchor: sha256 over the active
// facts' (id, updated_at) in id order. A new/edited/retired fact changes it,
// which aborts a stale apply.
func clusterAnchor(rows []db.ConfirmedMemory) string {
	ordered := append([]db.ConfirmedMemory(nil), rows...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	h := sha256.New()
	for _, r := range ordered {
		fmt.Fprintf(h, "%d:%s\n", r.ID, r.UpdatedAt)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// currentMembership reads the persisted fact->cluster map for the propose diff.
func currentMembership(ctx context.Context, store *db.Store) (map[int64]int64, error) {
	members, err := store.ListMemoryClusterMembers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int64, len(members))
	for _, m := range members {
		out[m.MemoryID] = m.ClusterID
	}
	return out, nil
}

// makeClusterPlan builds the reviewable plan from the freshly built clustering
// and the current persisted membership (for the moves/new/dropped diff).
func makeClusterPlan(built builtClustering, anchor string, current map[int64]int64) clusterPlan {
	plan := clusterPlan{SchemaVersion: clusterPlanSchemaVersion, Anchor: anchor}
	byCluster := map[int64][]int64{}
	for _, m := range built.assignment.Members {
		byCluster[m.ClusterID] = append(byCluster[m.ClusterID], m.MemoryID)
	}
	for _, c := range built.assignment.Clusters {
		members := append([]int64(nil), byCluster[c.ClusterID]...)
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
		plan.Clusters = append(plan.Clusters, clusterPlanCluster{
			ClusterID: c.ClusterID, Label: c.Label, MedoidID: c.MedoidID, Members: members,
		})
	}
	// Diff: moves (fact changed cluster), new facts (not previously clustered),
	// dropped facts (previously clustered, now absent).
	proposed := map[int64]int64{}
	for _, m := range built.assignment.Members {
		proposed[m.MemoryID] = m.ClusterID
	}
	for _, fid := range sortedInt64Keys(proposed) {
		to := proposed[fid]
		from, ok := current[fid]
		if !ok {
			plan.NewFacts = append(plan.NewFacts, fid)
			continue
		}
		if from != to {
			plan.Moves = append(plan.Moves, clusterPlanMove{MemoryID: fid, FromCluster: from, ToCluster: to})
		}
	}
	for _, fid := range sortedInt64Keys(current) {
		if _, ok := proposed[fid]; !ok {
			plan.DroppedFacts = append(plan.DroppedFacts, fid)
		}
	}
	plan.Stats = clusterPlanStats{
		Facts: built.facts, Clusters: built.realClusters, Unclustered: built.unclustered,
		Moves: len(plan.Moves),
	}
	return plan
}

// members returns every fact id referenced by the plan's clusters.
func (p clusterPlan) members() []int64 {
	var out []int64
	for _, c := range p.Clusters {
		out = append(out, c.Members...)
	}
	return out
}

// planAssignment maps a reviewed plan back into a store assignment for apply.
func planAssignment(p clusterPlan) db.MemoryClusterAssignment {
	var a db.MemoryClusterAssignment
	for _, c := range p.Clusters {
		a.Clusters = append(a.Clusters, db.MemoryCluster{
			ClusterID: c.ClusterID, Label: c.Label, MedoidID: c.MedoidID,
		})
		for _, m := range c.Members {
			a.Members = append(a.Members, db.MemoryClusterMember{MemoryID: m, ClusterID: c.ClusterID})
		}
	}
	return a
}

func countPlanBuckets(p clusterPlan) (real, unclustered int) {
	for _, c := range p.Clusters {
		if c.ClusterID == memory.UnclusteredID {
			unclustered = len(c.Members)
		} else {
			real++
		}
	}
	return real, unclustered
}

func readClusterPlan(path string) (clusterPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return clusterPlan{}, fmt.Errorf("read plan %s: %w", path, err)
	}
	var plan clusterPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return clusterPlan{}, fmt.Errorf("%s is not a valid cluster plan: %w", path, err)
	}
	if plan.SchemaVersion != clusterPlanSchemaVersion {
		return clusterPlan{}, fmt.Errorf("cluster plan schema v%d is not supported by this build (expected v%d); re-run `gitmoot memory clusters recompute --propose`", plan.SchemaVersion, clusterPlanSchemaVersion)
	}
	if plan.Anchor == "" {
		return clusterPlan{}, fmt.Errorf("%s has no anchor; re-run `gitmoot memory clusters recompute --propose`", path)
	}
	return plan, nil
}

func printClusterProposal(w io.Writer, plan clusterPlan, outPath string) {
	fmt.Fprintf(w, "cluster proposal (anchor %s)\n", plan.Anchor)
	fmt.Fprintf(w, "  %d fact(s) -> %d cluster(s), %d unclustered; %d move(s), %d new, %d dropped\n",
		plan.Stats.Facts, plan.Stats.Clusters, plan.Stats.Unclustered,
		len(plan.Moves), len(plan.NewFacts), len(plan.DroppedFacts))
	for _, c := range plan.Clusters {
		if c.ClusterID == memory.UnclusteredID {
			continue
		}
		fmt.Fprintf(w, "  cluster %d [%s] %d fact(s) (medoid %d)\n", c.ClusterID, c.Label, len(c.Members), c.MedoidID)
	}
	if len(plan.Moves) > 0 {
		fmt.Fprintln(w, "\nMoves:")
		for _, m := range plan.Moves {
			fmt.Fprintf(w, "  - fact %d: cluster %d -> %d\n", m.MemoryID, m.FromCluster, m.ToCluster)
		}
	}
	fmt.Fprintf(w, "\nplan written to %s\n", outPath)
	fmt.Fprintf(w, "review it, then apply with: gitmoot memory clusters recompute --apply --plan %s\n", outPath)
}

// attachConfirmedFactToCluster incrementally attaches a freshly confirmed fact to
// the cluster of its nearest similarity neighbor, using the SAME bm25+id-tiebreak
// ranking the vault links and the full recompute use. It is best-effort and
// fail-safe: any error (no clusters yet, no neighbor, a store hiccup) is swallowed
// so a confirmation is never blocked. A fact already assigned to a cluster is left
// alone (a content re-confirm never re-shelves it); a fact with no clustered
// neighbor falls into the reserved 'unclustered' bucket when it exists.
func attachConfirmedFactToCluster(ctx context.Context, store *db.Store, cm db.ConfirmedMemory) {
	// Nothing to attach to until a clustering has been built at least once.
	if n, err := store.CountMemoryClusters(ctx); err != nil || n == 0 {
		return
	}
	// Don't re-shelve a fact that already has an assignment.
	if _, ok, err := store.ClusterOfMemory(ctx, cm.ID); err != nil || ok {
		return
	}
	links, err := vaultLinksFor(ctx, store, cm)
	if err != nil {
		return
	}
	for _, l := range links {
		if l.TargetID == cm.ID {
			continue
		}
		if cid, ok, err := store.ClusterOfMemory(ctx, l.TargetID); err == nil && ok {
			_ = store.AssignMemoryToCluster(ctx, cm.ID, cid)
			return
		}
	}
	// No clustered neighbor: fall into the unclustered bucket if it exists.
	if bucketExists(ctx, store) {
		_ = store.AssignMemoryToCluster(ctx, cm.ID, memory.UnclusteredID)
	}
}

// bucketExists reports whether the reserved unclustered cluster row is present.
func bucketExists(ctx context.Context, store *db.Store) bool {
	clusters, err := store.ListMemoryClusters(ctx)
	if err != nil {
		return false
	}
	for _, c := range clusters {
		if c.ClusterID == memory.UnclusteredID {
			return true
		}
	}
	return false
}

// sortedInt64Keys returns the ascending keys of an int64-keyed map (deterministic
// iteration for the plan diff).
func sortedInt64Keys[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
