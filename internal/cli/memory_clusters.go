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

	"github.com/jerryfane/gitmoot/internal/config"
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
const clusterPlanSchemaVersion = 2

// clusterLinkK is the k-NN degree per fact — the same neighbor count the vault
// links render (vaultLinkK), so clusters emerge from the exact same signal.
const clusterLinkK = vaultLinkK

// clusterPlanCluster is one proposed community in the plan (the apply path writes
// these verbatim). Members are ascending fact ids.
type clusterPlanCluster struct {
	ClusterID int64   `json:"cluster_id"`
	ParentID  int64   `json:"parent_id,omitempty"`
	Label     string  `json:"label"`
	MedoidID  int64   `json:"medoid_id"`
	Members   []int64 `json:"members"`
}

// clusterPlanHierarchyChange makes automatic hierarchy changes explicit in a
// recompute proposal. ChildIDs are sorted for deterministic plans.
type clusterPlanHierarchyChange struct {
	ParentID int64   `json:"parent_id"`
	ChildIDs []int64 `json:"child_ids"`
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
	SchemaVersion int                          `json:"schema_version"`
	Anchor        string                       `json:"anchor"`
	Clusters      []clusterPlanCluster         `json:"clusters"`
	Moves         []clusterPlanMove            `json:"moves"`
	NewFacts      []int64                      `json:"new_facts,omitempty"`     // facts not previously in any cluster
	DroppedFacts  []int64                      `json:"dropped_facts,omitempty"` // previously-clustered facts now gone/retired
	Splits        []clusterPlanHierarchyChange `json:"splits"`
	Dissolves     []clusterPlanHierarchyChange `json:"dissolves"`
	Stats         clusterPlanStats             `json:"stats"`
}

type clusterPlanStats struct {
	Facts       int `json:"facts"`
	Clusters    int `json:"clusters"` // top-level real communities (excludes the unclustered bucket)
	Subclusters int `json:"subclusters"`
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
	ParentID  int64  `json:"parent_id,omitempty"`
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
		members, err := store.ListMemoryClusterMembers(ctx)
		if err != nil {
			return err
		}
		counts := hierarchyClusterCounts(clusters, members)
		entries = make([]clusterListEntry, 0, len(clusters))
		for _, c := range hierarchyOrderedClusters(clusters) {
			entries = append(entries, clusterListEntry{
				ClusterID: c.ClusterID, ParentID: c.ParentID, Label: c.DisplayLabel(), Override: c.LabelOverride,
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
	depths := clusterListDepths(entries)
	for _, e := range entries {
		indent := strings.Repeat("  ", depths[e.ClusterID]-1)
		fmt.Fprintf(stdout, "%s%-4d %-32s %5d facts (medoid %d)\n", indent, e.ClusterID, e.Label, e.Count, e.MedoidID)
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
	options, err := clusterHierarchyOptions(home)
	if err != nil {
		fmt.Fprintf(stderr, "memory clusters recompute: %v\n", err)
		return 1
	}
	var plan clusterPlan
	err = withReadOnlyStore(home, func(store *db.Store) error {
		ctx := context.Background()
		built, anchor, err := buildClusterAssignment(ctx, store, options)
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
		Subclusters int    `json:"subclusters"`
		Unclustered int    `json:"unclustered"`
		Facts       int    `json:"facts"`
	}
	var result applyResult

	options, err := clusterHierarchyOptions(home)
	if err != nil {
		fmt.Fprintf(stderr, "memory clusters recompute: %v\n", err)
		return 1
	}
	err = withStore(home, func(store *db.Store) error {
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
			built, anchor, err := buildClusterAssignment(ctx, store, options)
			if err != nil {
				return err
			}
			if err := store.RecomputeMemoryClusters(ctx, built.assignment); err != nil {
				return err
			}
			result = applyResult{Anchor: anchor, Applied: true, FirstRun: true,
				Clusters: built.realClusters, Subclusters: built.subclusters,
				Unclustered: built.unclustered, Facts: built.facts}
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
		real, subclusters, unc := countPlanBuckets(plan)
		result = applyResult{Anchor: plan.Anchor, Applied: true, Clusters: real,
			Subclusters: subclusters, Unclustered: unc, Facts: len(plan.members())}
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
	fmt.Fprintf(stdout, "%s: %d top-level cluster(s), %d subcluster(s), %d unclustered fact(s) across %d fact(s) (anchor %s)\n",
		kind, result.Clusters, result.Subclusters, result.Unclustered, result.Facts, result.Anchor)
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
	assignment       db.MemoryClusterAssignment
	previousClusters []db.MemoryCluster
	realClusters     int
	subclusters      int
	unclustered      int
	facts            int
}

// buildClusterAssignment reads every active confirmed fact, builds the undirected
// k-NN similarity graph from the SAME bm25+id-tiebreak neighbor ranking the vault
// links use, runs the deterministic community detection, and maps the result into
// a store assignment. It also returns the staleness anchor over the active facts'
// (id, updated_at). Read-only.
func buildClusterAssignment(ctx context.Context, store *db.Store, options memory.ClusterHierarchyOptions) (builtClustering, string, error) {
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
		nodes = append(nodes, memory.ClusterNode{ID: r.ID, Text: r.Content, Repo: strings.TrimSpace(r.Repo)})
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

	previous, err := store.ListMemoryClusters(ctx)
	if err != nil {
		return builtClustering{}, "", err
	}
	previousMembers, err := store.ListMemoryClusterMembers(ctx)
	if err != nil {
		return builtClustering{}, "", err
	}
	options.Existing = existingClusterHierarchyState(previous, previousMembers, rows)
	res := memory.BuildClusterHierarchyWithOptions(nodes, edges, options)
	built := builtClustering{facts: len(rows), previousClusters: previous}
	previousOverrideByID, rootOverrideByMedoid := previousClusterOverrides(previous)
	childrenByParent := map[int64]bool{}
	for _, c := range res.Clusters {
		if c.ParentID != 0 {
			childrenByParent[c.ParentID] = true
		}
	}
	for _, c := range res.Clusters {
		override := preservedClusterOverride(c, previousOverrideByID, rootOverrideByMedoid)
		built.assignment.Clusters = append(built.assignment.Clusters, db.MemoryCluster{
			ClusterID: c.ID, ParentID: c.ParentID, Label: c.Label, LabelOverride: override, MedoidID: c.MedoidID,
		})
		if !childrenByParent[c.ID] {
			for _, m := range c.Members {
				built.assignment.Members = append(built.assignment.Members, db.MemoryClusterMember{
					MemoryID: m, ClusterID: c.ID,
				})
			}
		}
		if c.ID == memory.UnclusteredID {
			built.unclustered = len(c.Members)
		} else if c.ParentID != 0 {
			built.subclusters++
		} else {
			built.realClusters++
		}
	}
	return built, clusterAnchor(rows), nil
}

func previousClusterOverrides(previous []db.MemoryCluster) (map[int64]string, map[int64]string) {
	byID := map[int64]string{}
	rootsByMedoid := map[int64]string{}
	for _, cluster := range previous {
		if cluster.LabelOverride == "" {
			continue
		}
		byID[cluster.ClusterID] = cluster.LabelOverride
		if cluster.ParentID == 0 && cluster.MedoidID != 0 {
			rootsByMedoid[cluster.MedoidID] = cluster.LabelOverride
		}
	}
	return byID, rootsByMedoid
}

func preservedClusterOverride(cluster memory.Cluster, byID, rootsByMedoid map[int64]string) string {
	if cluster.BaseCommunity {
		if override := rootsByMedoid[cluster.MedoidID]; override != "" {
			return override
		}
	}
	return byID[cluster.ID]
}

func clusterHierarchyOptions(home string) (memory.ClusterHierarchyOptions, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return memory.ClusterHierarchyOptions{}, err
	}
	settings, err := config.LoadMemorySettings(paths)
	if err != nil {
		return memory.ClusterHierarchyOptions{}, err
	}
	return memory.ClusterHierarchyOptions{
		Fanout: settings.ClusterFanout, FanoutKeep: settings.ClusterFanoutKeep, DepthCap: settings.ClusterDepthCap,
	}, nil
}

// existingClusterHierarchyState reconstructs medoid-path identity and fan-out
// hysteresis from existing rows. No extra persistence column is needed: parent
// links, ordered child medoids, direct leaf memberships, and fact repo scopes
// contain the complete state.
func existingClusterHierarchyState(clusters []db.MemoryCluster, members []db.MemoryClusterMember, rows []db.ConfirmedMemory) []memory.ClusterHierarchyState {
	children := map[int64][]db.MemoryCluster{}
	for _, c := range clusters {
		if c.ParentID != 0 {
			children[c.ParentID] = append(children[c.ParentID], c)
		}
	}
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			if children[parentID][i].MedoidID != children[parentID][j].MedoidID {
				return children[parentID][i].MedoidID < children[parentID][j].MedoidID
			}
			return children[parentID][i].ClusterID < children[parentID][j].ClusterID
		})
	}
	repoByMemory := map[int64]string{}
	for _, row := range rows {
		repoByMemory[row.ID] = strings.TrimSpace(row.Repo)
	}
	directRepos := map[int64]map[string]int{}
	for _, member := range members {
		if directRepos[member.ClusterID] == nil {
			directRepos[member.ClusterID] = map[string]int{}
		}
		directRepos[member.ClusterID][repoByMemory[member.MemoryID]]++
	}
	repoMemo := map[int64]map[string]int{}
	var reposFor func(int64, map[int64]bool) map[string]int
	reposFor = func(id int64, visiting map[int64]bool) map[string]int {
		if cached := repoMemo[id]; cached != nil {
			return cached
		}
		if visiting[id] {
			return map[string]int{}
		}
		visiting[id] = true
		counts := map[string]int{}
		for repo, count := range directRepos[id] {
			counts[repo] += count
		}
		for _, child := range children[id] {
			for repo, count := range reposFor(child.ClusterID, visiting) {
				counts[repo] += count
			}
		}
		delete(visiting, id)
		repoMemo[id] = counts
		return counts
	}
	var out []memory.ClusterHierarchyState
	var walk func(db.MemoryCluster, int, []int64)
	walk = func(cluster db.MemoryCluster, level int, parentPath []int64) {
		path := append(append([]int64(nil), parentPath...), cluster.MedoidID)
		kids := children[cluster.ClusterID]
		if len(kids) > 0 {
			state := memory.ClusterHierarchyState{
				Level: level, MedoidPath: path, ClusterID: cluster.ClusterID,
				Scope:     dominantClusterRepo(reposFor(cluster.ClusterID, map[int64]bool{})),
				Synthetic: memory.IsSyntheticClusterID(cluster.ClusterID),
			}
			for _, child := range kids {
				state.ChildMedoids = append(state.ChildMedoids, child.MedoidID)
				state.ChildIDs = append(state.ChildIDs, child.ClusterID)
			}
			out = append(out, state)
		}
		for _, child := range kids {
			walk(child, level+1, path)
		}
	}
	var roots []db.MemoryCluster
	for _, c := range clusters {
		if c.ParentID == 0 && c.ClusterID != memory.UnclusteredID {
			roots = append(roots, c)
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].MedoidID != roots[j].MedoidID {
			return roots[i].MedoidID < roots[j].MedoidID
		}
		return roots[i].ClusterID < roots[j].ClusterID
	})
	for _, root := range roots {
		walk(root, 1, nil)
	}
	return out
}

func dominantClusterRepo(counts map[string]int) string {
	best, bestCount := "", -1
	for repo, count := range counts {
		if count > bestCount || (count == bestCount && repo < best) {
			best, bestCount = repo, count
		}
	}
	return best
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
	plan := clusterPlan{
		SchemaVersion: clusterPlanSchemaVersion,
		Anchor:        anchor,
		Splits:        []clusterPlanHierarchyChange{},
		Dissolves:     []clusterPlanHierarchyChange{},
	}
	byCluster := map[int64][]int64{}
	for _, m := range built.assignment.Members {
		byCluster[m.ClusterID] = append(byCluster[m.ClusterID], m.MemoryID)
	}
	for _, c := range built.assignment.Clusters {
		members := append([]int64(nil), byCluster[c.ClusterID]...)
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
		plan.Clusters = append(plan.Clusters, clusterPlanCluster{
			ClusterID: c.ClusterID, ParentID: c.ParentID, Label: c.Label, MedoidID: c.MedoidID, Members: members,
		})
	}
	plan.Splits, plan.Dissolves = hierarchyPlanChanges(built.previousClusters, built.assignment.Clusters)
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
		Facts: built.facts, Clusters: built.realClusters, Subclusters: built.subclusters, Unclustered: built.unclustered,
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
			ClusterID: c.ClusterID, ParentID: c.ParentID, Label: c.Label, MedoidID: c.MedoidID,
		})
		for _, m := range c.Members {
			a.Members = append(a.Members, db.MemoryClusterMember{MemoryID: m, ClusterID: c.ClusterID})
		}
	}
	return a
}

func countPlanBuckets(p clusterPlan) (real, subclusters, unclustered int) {
	for _, c := range p.Clusters {
		if c.ClusterID == memory.UnclusteredID {
			unclustered = len(c.Members)
		} else if c.ParentID != 0 {
			subclusters++
		} else {
			real++
		}
	}
	return real, subclusters, unclustered
}

func hierarchyClusterCounts(clusters []db.MemoryCluster, members []db.MemoryClusterMember) map[int64]int {
	parent := map[int64]int64{}
	for _, cluster := range clusters {
		parent[cluster.ClusterID] = cluster.ParentID
	}
	counts := map[int64]int{}
	for _, member := range members {
		current := member.ClusterID
		seen := map[int64]bool{}
		for {
			if seen[current] {
				break
			}
			seen[current] = true
			counts[current]++
			next, ok := parent[current]
			if !ok || next == 0 {
				break
			}
			current = next
		}
	}
	return counts
}

func clusterListDepths(entries []clusterListEntry) map[int64]int {
	parent := map[int64]int64{}
	for _, entry := range entries {
		parent[entry.ClusterID] = entry.ParentID
	}
	return hierarchyDepths(parent)
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
	fmt.Fprintf(w, "  %d fact(s) -> %d top-level cluster(s), %d subcluster(s), %d unclustered; %d move(s), %d new, %d dropped\n",
		plan.Stats.Facts, plan.Stats.Clusters, plan.Stats.Subclusters, plan.Stats.Unclustered,
		len(plan.Moves), len(plan.NewFacts), len(plan.DroppedFacts))
	ordered := hierarchyOrderedPlanClusters(plan.Clusters)
	depths := planClusterDepths(plan.Clusters)
	for _, c := range ordered {
		if c.ClusterID == memory.UnclusteredID {
			continue
		}
		indent := strings.Repeat("  ", depths[c.ClusterID]-1)
		count := len(c.Members)
		if len(c.Members) == 0 {
			count = planClusterCount(plan.Clusters, c.ClusterID)
		}
		fmt.Fprintf(w, "  %scluster %d [%s] %d fact(s) (medoid %d)\n", indent, c.ClusterID, c.Label, count, c.MedoidID)
	}
	if len(plan.Splits) > 0 {
		fmt.Fprintln(w, "\nPlanned splits:")
		for _, change := range plan.Splits {
			fmt.Fprintf(w, "  - cluster %d -> children %v\n", change.ParentID, change.ChildIDs)
		}
	}
	if len(plan.Dissolves) > 0 {
		fmt.Fprintln(w, "\nPlanned dissolves:")
		for _, change := range plan.Dissolves {
			fmt.Fprintf(w, "  - cluster %d removes children %v\n", change.ParentID, change.ChildIDs)
		}
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

func hierarchyOrderedClusters(clusters []db.MemoryCluster) []db.MemoryCluster {
	children := map[int64][]db.MemoryCluster{}
	var roots []db.MemoryCluster
	for _, c := range clusters {
		if c.ParentID == 0 {
			roots = append(roots, c)
		} else {
			children[c.ParentID] = append(children[c.ParentID], c)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ClusterID < roots[j].ClusterID })
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			if children[parentID][i].MedoidID != children[parentID][j].MedoidID {
				return children[parentID][i].MedoidID < children[parentID][j].MedoidID
			}
			return children[parentID][i].ClusterID < children[parentID][j].ClusterID
		})
	}
	out := make([]db.MemoryCluster, 0, len(clusters))
	visited := map[int64]bool{}
	var walk func(db.MemoryCluster)
	walk = func(cluster db.MemoryCluster) {
		if visited[cluster.ClusterID] {
			return
		}
		visited[cluster.ClusterID] = true
		out = append(out, cluster)
		for _, child := range children[cluster.ClusterID] {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	for _, cluster := range clusters {
		walk(cluster)
	}
	return out
}

func hierarchyOrderedPlanClusters(clusters []clusterPlanCluster) []clusterPlanCluster {
	children := map[int64][]clusterPlanCluster{}
	var roots []clusterPlanCluster
	for _, c := range clusters {
		if c.ParentID == 0 {
			roots = append(roots, c)
		} else {
			children[c.ParentID] = append(children[c.ParentID], c)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ClusterID < roots[j].ClusterID })
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			if children[parentID][i].MedoidID != children[parentID][j].MedoidID {
				return children[parentID][i].MedoidID < children[parentID][j].MedoidID
			}
			return children[parentID][i].ClusterID < children[parentID][j].ClusterID
		})
	}
	out := make([]clusterPlanCluster, 0, len(clusters))
	visited := map[int64]bool{}
	var walk func(clusterPlanCluster)
	walk = func(cluster clusterPlanCluster) {
		if visited[cluster.ClusterID] {
			return
		}
		visited[cluster.ClusterID] = true
		out = append(out, cluster)
		for _, child := range children[cluster.ClusterID] {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	for _, cluster := range clusters {
		walk(cluster)
	}
	return out
}

func planClusterCount(clusters []clusterPlanCluster, clusterID int64) int {
	byParent := map[int64][]clusterPlanCluster{}
	byID := map[int64]clusterPlanCluster{}
	for _, cluster := range clusters {
		byID[cluster.ClusterID] = cluster
		byParent[cluster.ParentID] = append(byParent[cluster.ParentID], cluster)
	}
	visited := map[int64]bool{}
	var count func(int64) int
	count = func(id int64) int {
		if visited[id] {
			return 0
		}
		visited[id] = true
		total := len(byID[id].Members)
		for _, child := range byParent[id] {
			total += count(child.ClusterID)
		}
		return total
	}
	return count(clusterID)
}

func planClusterDepths(clusters []clusterPlanCluster) map[int64]int {
	parent := map[int64]int64{}
	for _, cluster := range clusters {
		parent[cluster.ClusterID] = cluster.ParentID
	}
	return hierarchyDepths(parent)
}

func hierarchyDepths(parent map[int64]int64) map[int64]int {
	depths := map[int64]int{}
	var depth func(int64, map[int64]bool) int
	depth = func(id int64, visiting map[int64]bool) int {
		if depths[id] > 0 {
			return depths[id]
		}
		if visiting[id] {
			return 1
		}
		visiting[id] = true
		p := parent[id]
		value := 1
		if p != 0 {
			value = depth(p, visiting) + 1
		}
		delete(visiting, id)
		depths[id] = value
		return value
	}
	for id := range parent {
		depth(id, map[int64]bool{})
	}
	return depths
}

type clusterHierarchyState struct {
	ParentID     int64
	ParentMedoid int64
	Level        int
	Path         string
	Children     []int64
	ChildMedoids []int64
}

// hierarchyPlanChanges compares arbitrary-depth trees by medoid path. The
// parent-medoid + ordered-child-medoids fallback matches a legacy two-level split
// that merely gained a synthetic ancestor, so migration does not report a false
// dissolve/re-split.
func hierarchyPlanChanges(current, proposed []db.MemoryCluster) (splits, dissolves []clusterPlanHierarchyChange) {
	old := clusterHierarchyStates(current)
	next := clusterHierarchyStates(proposed)
	usedNext := make([]bool, len(next))
	for _, before := range old {
		match := -1
		for i, after := range next {
			if !usedNext[i] && before.Level == after.Level && before.Path == after.Path {
				match = i
				break
			}
		}
		if match < 0 {
			for i, after := range next {
				if !usedNext[i] && before.ParentMedoid == after.ParentMedoid && equalInt64s(before.ChildMedoids, after.ChildMedoids) {
					match = i
					break
				}
			}
		}
		if match < 0 {
			dissolves = append(dissolves, clusterPlanHierarchyChange{
				ParentID: before.ParentID,
				ChildIDs: append([]int64(nil), before.Children...),
			})
			continue
		}
		usedNext[match] = true
		after := next[match]
		if !equalInt64s(before.Children, after.Children) {
			dissolves = append(dissolves, clusterPlanHierarchyChange{ParentID: before.ParentID, ChildIDs: append([]int64(nil), before.Children...)})
			splits = append(splits, clusterPlanHierarchyChange{
				ParentID: after.ParentID,
				ChildIDs: append([]int64(nil), after.Children...),
			})
		}
	}
	for i, after := range next {
		if !usedNext[i] {
			splits = append(splits, clusterPlanHierarchyChange{ParentID: after.ParentID, ChildIDs: append([]int64(nil), after.Children...)})
		}
	}
	sort.Slice(splits, func(i, j int) bool { return splits[i].ParentID < splits[j].ParentID })
	sort.Slice(dissolves, func(i, j int) bool { return dissolves[i].ParentID < dissolves[j].ParentID })
	return splits, dissolves
}

func clusterHierarchyStates(clusters []db.MemoryCluster) []clusterHierarchyState {
	children := map[int64][]db.MemoryCluster{}
	for _, c := range clusters {
		if c.ParentID != 0 {
			children[c.ParentID] = append(children[c.ParentID], c)
		}
	}
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			if children[parentID][i].MedoidID != children[parentID][j].MedoidID {
				return children[parentID][i].MedoidID < children[parentID][j].MedoidID
			}
			return children[parentID][i].ClusterID < children[parentID][j].ClusterID
		})
	}
	var roots []db.MemoryCluster
	for _, cluster := range clusters {
		if cluster.ParentID == 0 && cluster.ClusterID != memory.UnclusteredID {
			roots = append(roots, cluster)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ClusterID < roots[j].ClusterID })
	var out []clusterHierarchyState
	var walk func(db.MemoryCluster, int, []int64)
	walk = func(parent db.MemoryCluster, level int, path []int64) {
		path = append(append([]int64(nil), path...), parent.MedoidID)
		kids := children[parent.ClusterID]
		if len(kids) > 0 {
			state := clusterHierarchyState{ParentID: parent.ClusterID, ParentMedoid: parent.MedoidID, Level: level, Path: medoidPlanPath(path)}
			for _, child := range kids {
				state.Children = append(state.Children, child.ClusterID)
				state.ChildMedoids = append(state.ChildMedoids, child.MedoidID)
			}
			out = append(out, state)
		}
		for _, child := range kids {
			walk(child, level+1, path)
		}
	}
	for _, root := range roots {
		walk(root, 1, nil)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level < out[j].Level
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func medoidPlanPath(path []int64) string {
	var b strings.Builder
	for _, medoid := range path {
		fmt.Fprintf(&b, "%d/", medoid)
	}
	return b.String()
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
