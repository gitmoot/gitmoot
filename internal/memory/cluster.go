package memory

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strings"
)

// This file holds the PURE, deterministic community-detection + labeling logic
// for emergent memory clusters (#763 Track A). It carries no SQL and no I/O: the
// caller (internal/db + internal/cli) builds the k-NN similarity graph from the
// existing FTS/bm25 vault-link signal, hands the nodes + undirected weighted
// edges here, and this returns a fully-determined clustering. Determinism is a
// hard requirement — the same graph MUST yield byte-identical clusters, labels,
// medoids, and cluster ids on every run — so every step below is a pure function
// of the input with fixed iteration order and fixed tie-breaks (never a map
// range, never wall-clock, never a random seed).

// UnclusteredID is the reserved cluster id for facts with no similarity
// neighbors (graph degree 0). They are grouped under a single 'unclustered'
// bucket rather than each becoming a singleton, so the bridge/UI can show them
// together. Real communities are numbered from 1.
const UnclusteredID int64 = 0

// UnclusteredLabel is the fixed label of the reserved degree-0 bucket.
const UnclusteredLabel = "unclustered"

// clusterMaxLabelTerms is how many distinctive terms a computed label carries.
const clusterMaxLabelTerms = 3

// clusterLabelRounds caps the label-propagation passes. Determinism does NOT
// depend on convergence: the algorithm is a pure function of the input for ANY
// fixed cap (the same fixed sequence of in-order, fixed-tie-break updates runs
// every time), so a run that hits the cap mid-oscillation still produces
// byte-identical output. The cap only bounds work; on the small memory graphs
// this targets (~hundreds of facts) label propagation settles well within it.
const clusterLabelRounds = 100

// ClusterSplitThreshold is the number of facts at which an unsplit top-level
// cluster becomes eligible for one deterministic child-clustering pass.
const ClusterSplitThreshold = 20

// ClusterSplitKeepThreshold is the strict lower hysteresis boundary for an
// existing split. A parent with more than this many facts may keep a valid
// split; at or below it the children dissolve.
const ClusterSplitKeepThreshold = 12

// ClusterMinChildFacts is the minimum size of every accepted child community.
const ClusterMinChildFacts = 4

const (
	DefaultClusterFanout       = 12
	DefaultClusterFanoutKeep   = 9
	DefaultClusterHierarchyCap = 4
)

const (
	syntheticClusterIDBase  = int64(1) << 51
	syntheticClusterIDLimit = int64(1) << 52
)

func IsSyntheticClusterID(id int64) bool {
	return id >= syntheticClusterIDBase && id < syntheticClusterIDLimit
}

// ClusterNode is one fact participating in the similarity graph. Text is the
// concatenation the labeler tokenizes (key + content); the caller supplies it so
// this package never re-derives the fact shape.
type ClusterNode struct {
	ID   int64
	Text string
	Repo string
}

// ClusterEdge is one UNDIRECTED weighted similarity edge. A and B are fact ids
// (order irrelevant — the builder normalizes A<B); Weight is a positive integer
// similarity (higher == more mutually similar). The caller derives weight from
// the same bm25+id-tiebreak neighbor ranking the vault [[links]] use.
type ClusterEdge struct {
	A      int64
	B      int64
	Weight int
}

// Cluster is one detected community.
type Cluster struct {
	ID            int64   // 1-based; UnclusteredID (0) for the degree-0 bucket
	ParentID      int64   // 0 for top-level clusters; otherwise the immediate parent
	Label         string  // computed distinctive-term label (override applied by the store, not here)
	MedoidID      int64   // member with the highest intra-cluster similarity (lowest id tie-break); 0 for unclustered
	Members       []int64 // member fact ids, ascending; populated on leaves only in a hierarchy
	BaseCommunity bool    // true only for a community produced by the global BuildClusters pass
}

// ClusterHierarchyState is one persisted hierarchy node, reconstructed by the
// caller from existing rows. MedoidPath is the root-to-node medoid path;
// ChildMedoids and ChildIDs are in deterministic sibling order. Synthetic marks
// a fan-out parent rather than a fact-size split parent.
type ClusterHierarchyState struct {
	Level        int
	MedoidPath   []int64
	ChildMedoids []int64
	ChildIDs     []int64
	ClusterID    int64
	Scope        string
	Synthetic    bool
}

type ClusterHierarchyOptions struct {
	Fanout     int
	FanoutKeep int
	DepthCap   int
	Existing   []ClusterHierarchyState
}

func DefaultClusterHierarchyOptions() ClusterHierarchyOptions {
	return ClusterHierarchyOptions{
		Fanout:     DefaultClusterFanout,
		FanoutKeep: DefaultClusterFanoutKeep,
		DepthCap:   DefaultClusterHierarchyCap,
	}
}

// ClusterResult is the full deterministic clustering.
type ClusterResult struct {
	Clusters []Cluster // real communities first (ascending medoid id), then the unclustered bucket if non-empty
}

// BuildClusters runs deterministic community detection over the similarity graph
// and returns labeled clusters. It is a PURE function: same nodes + edges ⇒
// byte-identical result, always.
//
// Algorithm (id-ordered label propagation):
//  1. Every node starts labeled with its own id.
//  2. Nodes are visited in STRICT ascending-id order, repeatedly. On each visit a
//     node adopts the label carrying the greatest summed edge weight among its
//     neighbors; ties are broken by the LOWEST label value.
//  3. Passes repeat until a full pass makes no change (converged) or the fixed
//     round cap is hit.
//
// Why it is deterministic: the node visit order is fixed (sorted ids), the
// initial labels are fixed (the ids themselves), the neighbor weighting is a sum
// (order-independent), and every tie-break resolves to the lowest label — there
// is no map-iteration order, randomness, or time dependence anywhere. Given the
// same graph the exact same sequence of updates runs, so the labels, the derived
// medoids, the cluster-id numbering, and the labels are identical every run.
//
// Degree-0 nodes keep their own label and are collected into the single reserved
// 'unclustered' bucket (UnclusteredID). Real communities are numbered from 1 in
// ascending medoid-id order so the numbering itself is stable across runs.
func BuildClusters(nodes []ClusterNode, edges []ClusterEdge) ClusterResult {
	// Stable, de-duplicated node id list and text lookup.
	textByID := make(map[int64]string, len(nodes))
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		if _, seen := textByID[n.ID]; seen {
			continue
		}
		textByID[n.ID] = n.Text
		ids = append(ids, n.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// Symmetric adjacency with summed weights. Self-loops and edges touching an
	// unknown node are ignored so the graph is always well-formed.
	adj := make(map[int64]map[int64]int, len(ids))
	for _, id := range ids {
		adj[id] = map[int64]int{}
	}
	for _, e := range edges {
		if e.A == e.B || e.Weight <= 0 {
			continue
		}
		if _, ok := textByID[e.A]; !ok {
			continue
		}
		if _, ok := textByID[e.B]; !ok {
			continue
		}
		adj[e.A][e.B] += e.Weight
		adj[e.B][e.A] += e.Weight
	}

	// Label propagation.
	label := make(map[int64]int64, len(ids))
	for _, id := range ids {
		label[id] = id
	}
	for round := 0; round < clusterLabelRounds; round++ {
		changed := false
		for _, id := range ids {
			nbrs := adj[id]
			if len(nbrs) == 0 {
				continue // isolated: keep own label (becomes unclustered)
			}
			best, ok := dominantLabel(nbrs, label)
			if ok && best != label[id] {
				label[id] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// Group by final label. Degree-0 nodes go to the unclustered bucket.
	groups := map[int64][]int64{}
	var unclustered []int64
	for _, id := range ids {
		if len(adj[id]) == 0 {
			unclustered = append(unclustered, id)
			continue
		}
		groups[label[id]] = append(groups[label[id]], id)
	}

	// Materialize each group: sort members, pick medoid, then order groups by
	// medoid id so cluster ids are assigned deterministically.
	type pending struct {
		members []int64
		medoid  int64
	}
	pend := make([]pending, 0, len(groups))
	for lbl := range groups {
		members := append([]int64(nil), groups[lbl]...)
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
		pend = append(pend, pending{members: members, medoid: medoidOf(members, adj)})
	}
	sort.Slice(pend, func(i, j int) bool { return pend[i].medoid < pend[j].medoid })

	// Per-cluster term sets (over ALL members) drive the corpus document
	// frequency used for label distinctiveness.
	termSets := make([]map[string]struct{}, len(pend))
	for i, p := range pend {
		set := map[string]struct{}{}
		for _, id := range p.members {
			for _, t := range clusterTerms(textByID[id]) {
				set[t] = struct{}{}
			}
		}
		termSets[i] = set
	}
	df := map[string]int{}
	for _, set := range termSets {
		for t := range set {
			df[t]++
		}
	}

	result := ClusterResult{Clusters: make([]Cluster, 0, len(pend)+1)}
	for i, p := range pend {
		cid := int64(i + 1)
		result.Clusters = append(result.Clusters, Cluster{
			ID:       cid,
			Label:    clusterLabel(p.members, p.medoid, textByID, df),
			MedoidID: p.medoid,
			Members:  p.members,
		})
	}
	if len(unclustered) > 0 {
		sort.Slice(unclustered, func(i, j int) bool { return unclustered[i] < unclustered[j] })
		result.Clusters = append(result.Clusters, Cluster{
			ID:       UnclusteredID,
			Label:    UnclusteredLabel,
			MedoidID: 0,
			Members:  unclustered,
		})
	}
	return result
}

// BuildClusterHierarchy is the compatibility entry point for callers that only
// know the original one-level split state. New callers should use
// BuildClusterHierarchyWithOptions so fan-out and medoid-path state are explicit.
func BuildClusterHierarchy(nodes []ClusterNode, edges []ClusterEdge, existingSplitParentMedoids map[int64]bool) ClusterResult {
	options := DefaultClusterHierarchyOptions()
	for medoid, split := range existingSplitParentMedoids {
		if split {
			options.Existing = append(options.Existing, ClusterHierarchyState{Level: 1, MedoidPath: []int64{medoid}})
		}
	}
	return BuildClusterHierarchyWithOptions(nodes, edges, options)
}

func buildClusterHierarchy(top ClusterResult, nodes []ClusterNode, edges []ClusterEdge, existingSplitParentMedoids map[int64]bool) ClusterResult {
	options := DefaultClusterHierarchyOptions()
	for medoid, split := range existingSplitParentMedoids {
		if split {
			options.Existing = append(options.Existing, ClusterHierarchyState{Level: 1, MedoidPath: []int64{medoid}})
		}
	}
	return buildClusterHierarchyWithOptions(top, nodes, edges, options)
}

// BuildClusterHierarchyWithOptions recursively applies the existing fact-size
// split and balances every rendered repo-scope sibling level. Crowded levels are
// packed under deterministic average-linkage synthetic parents, while persisted
// medoid-path state supplies both hysteresis and stable child identities.
func BuildClusterHierarchyWithOptions(nodes []ClusterNode, edges []ClusterEdge, options ClusterHierarchyOptions) ClusterResult {
	top := BuildClusters(nodes, edges)
	return buildClusterHierarchyWithOptions(top, nodes, edges, options)
}

type hierarchyNode struct {
	cluster  Cluster
	children []*hierarchyNode
	scope    string
}

type hierarchyStateIndex struct {
	states []ClusterHierarchyState
}

func buildClusterHierarchyWithOptions(top ClusterResult, nodes []ClusterNode, edges []ClusterEdge, options ClusterHierarchyOptions) ClusterResult {
	options = normalizeHierarchyOptions(options)
	nodeByID := make(map[int64]ClusterNode, len(nodes))
	for _, n := range nodes {
		if _, seen := nodeByID[n.ID]; !seen {
			nodeByID[n.ID] = n
		}
	}

	adj := hierarchyAdjacency(nodes, edges)
	usedIDs := make(map[int64]bool, len(top.Clusters))
	var roots []*hierarchyNode
	var unclustered *hierarchyNode
	for _, c := range top.Clusters {
		usedIDs[c.ID] = true
		c.BaseCommunity = c.ID != UnclusteredID
		n := &hierarchyNode{cluster: c, scope: scopeForMembers(c.Members, nodeByID)}
		if c.ID == UnclusteredID {
			unclustered = n
		} else {
			roots = append(roots, n)
		}
	}
	state := hierarchyStateIndex{states: append([]ClusterHierarchyState(nil), options.Existing...)}
	roots = balanceHierarchyLevel(roots, 1, 0, nil, nodeByID, adj, state, options, usedIDs)
	for _, root := range roots {
		expandHierarchyNode(root, 1, []int64{root.cluster.MedoidID}, nodeByID, edges, adj, state, options, usedIDs)
	}

	out := ClusterResult{Clusters: make([]Cluster, 0, len(top.Clusters))}
	for _, root := range roots {
		flattenHierarchy(root, 0, &out.Clusters)
	}
	if unclustered != nil {
		flattenHierarchy(unclustered, 0, &out.Clusters)
	}
	return out
}

func normalizeHierarchyOptions(options ClusterHierarchyOptions) ClusterHierarchyOptions {
	if options.Fanout <= 1 {
		options.Fanout = DefaultClusterFanout
	}
	if options.FanoutKeep <= 0 || options.FanoutKeep >= options.Fanout {
		options.FanoutKeep = DefaultClusterFanoutKeep
		if options.FanoutKeep >= options.Fanout {
			options.FanoutKeep = options.Fanout - 1
		}
	}
	if options.DepthCap <= 0 || options.DepthCap > DefaultClusterHierarchyCap {
		options.DepthCap = DefaultClusterHierarchyCap
	}
	return options
}

func expandHierarchyNode(parent *hierarchyNode, level int, path []int64, nodeByID map[int64]ClusterNode, edges []ClusterEdge, adj map[int64]map[int64]int, state hierarchyStateIndex, options ClusterHierarchyOptions, usedIDs map[int64]bool) {
	if level >= options.DepthCap {
		return
	}
	if len(parent.children) == 0 {
		existing, hadExisting := state.splitState(path, parent.cluster.MedoidID)
		if splitSizeEligible(len(parent.cluster.Members), hadExisting) {
			children := splitHierarchyCluster(parent.cluster, nodeByID, edges)
			if validChildSplit(children, len(parent.cluster.Members)) {
				assignSplitChildIDs(parent, children, level, path, existing, hadExisting, usedIDs)
			}
		}
	}
	if len(parent.children) == 0 {
		return
	}
	if !IsSyntheticClusterID(parent.cluster.ID) {
		parent.children = balanceHierarchyLevel(parent.children, level+1, parent.cluster.ID, path, nodeByID, adj, state, options, usedIDs)
	}
	for _, child := range parent.children {
		childPath := appendPath(path, child.cluster.MedoidID)
		expandHierarchyNode(child, level+1, childPath, nodeByID, edges, adj, state, options, usedIDs)
	}
}

func splitHierarchyCluster(parent Cluster, nodeByID map[int64]ClusterNode, edges []ClusterEdge) []Cluster {
	members := make(map[int64]bool, len(parent.Members))
	subNodes := make([]ClusterNode, 0, len(parent.Members))
	for _, id := range parent.Members {
		members[id] = true
		if n, ok := nodeByID[id]; ok {
			subNodes = append(subNodes, n)
		}
	}
	subEdges := make([]ClusterEdge, 0, len(edges))
	for _, e := range edges {
		if members[e.A] && members[e.B] {
			subEdges = append(subEdges, e)
		}
	}
	return BuildClusters(subNodes, subEdges).Clusters
}

func assignSplitChildIDs(parent *hierarchyNode, children []Cluster, level int, path []int64, existing ClusterHierarchyState, hadExisting bool, usedIDs map[int64]bool) {
	medoids := clusterMedoids(children)
	reuse := hadExisting && equalMedoids(medoids, existing.ChildMedoids) && len(existing.ChildIDs) == len(children)
	parent.children = make([]*hierarchyNode, 0, len(children))
	parent.cluster.Members = nil
	for i := range children {
		id := int64(0)
		if reuse && !usedIDs[existing.ChildIDs[i]] {
			id = existing.ChildIDs[i]
		} else if level == 1 && len(path) == 1 {
			id = nextChildClusterID(parent.cluster.MedoidID, medoids, children[i].MedoidID, usedIDs)
		} else {
			id = nextHierarchyClusterID(path, medoids, children[i].MedoidID, level+1, false, usedIDs)
		}
		children[i].ID = id
		children[i].ParentID = parent.cluster.ID
		usedIDs[id] = true
		parent.children = append(parent.children, &hierarchyNode{cluster: children[i]})
	}
}

func (s hierarchyStateIndex) splitState(path []int64, medoid int64) (ClusterHierarchyState, bool) {
	key := medoidPathKey(path)
	for _, candidate := range s.states {
		if !candidate.Synthetic && medoidPathKey(candidate.MedoidPath) == key {
			return candidate, true
		}
	}
	// Migration fallback: a pre-balanced two-level split has no synthetic ancestor
	// in its path yet. Match its stable parent medoid once so it moves intact.
	for _, candidate := range s.states {
		if len(candidate.MedoidPath) > 0 && candidate.MedoidPath[len(candidate.MedoidPath)-1] == medoid && !candidate.Synthetic {
			return candidate, true
		}
	}
	return ClusterHierarchyState{}, false
}

func balanceHierarchyLevel(items []*hierarchyNode, level int, parentID int64, parentPath []int64, nodeByID map[int64]ClusterNode, adj map[int64]map[int64]int, state hierarchyStateIndex, options ClusterHierarchyOptions, usedIDs map[int64]bool) []*hierarchyNode {
	if level >= options.DepthCap || len(items) <= options.FanoutKeep {
		return items
	}
	byScope := map[string][]*hierarchyNode{}
	for _, item := range items {
		if item.scope == "" {
			item.scope = scopeForMembers(descendantMembers(item), nodeByID)
		}
		byScope[item.scope] = append(byScope[item.scope], item)
	}
	var out []*hierarchyNode
	for _, scope := range sortedStringKeys(byScope) {
		scoped := byScope[scope]
		for layer := 0; layer < options.DepthCap-level; layer++ {
			existing := state.syntheticGroups(level+layer, parentPath, scope, scoped)
			if len(scoped) <= options.Fanout && !(len(existing) > 0 && len(scoped) > options.FanoutKeep) {
				break
			}
			groups := preserveSyntheticGroups(scoped, existing, options.Fanout, adj)
			if len(groups) == 0 {
				groups = averageLinkageGroups(scoped, options.Fanout, adj)
			}
			next := materializeSyntheticGroups(groups, level+layer, parentID, parentPath, scope, nodeByID, adj, usedIDs)
			if len(next) >= len(scoped) {
				scoped = next
				break
			}
			scoped = next
		}
		out = append(out, scoped...)
	}
	sortHierarchyNodes(out)
	return out
}

type syntheticGroup struct {
	items    []*hierarchyNode
	existing *ClusterHierarchyState
}

func (s hierarchyStateIndex) syntheticGroups(level int, parentPath []int64, scope string, items []*hierarchyNode) []ClusterHierarchyState {
	var out []ClusterHierarchyState
	itemMedoids := map[int64]bool{}
	itemIDs := map[int64]bool{}
	for _, item := range items {
		itemMedoids[item.cluster.MedoidID] = true
		itemIDs[item.cluster.ID] = true
	}
	for _, candidate := range s.states {
		if !candidate.Synthetic || candidate.Level != level || candidate.Scope != scope || len(candidate.MedoidPath) == 0 {
			continue
		}
		if equalMedoids(candidate.MedoidPath[:len(candidate.MedoidPath)-1], parentPath) && intersectsInt64(candidate.ChildIDs, itemIDs) {
			out = append(out, candidate)
		}
	}
	if len(out) == 0 {
		// Multi-level rebuild fallback: child IDs identify the exact inner-to-outer
		// layer even when synthetic and fact medoids happen to be equal.
		for _, candidate := range s.states {
			if !candidate.Synthetic || candidate.Scope != scope {
				continue
			}
			if intersectsInt64(candidate.ChildIDs, itemIDs) {
				out = append(out, candidate)
			}
		}
	}
	if len(out) == 0 {
		// Boundary migration fallback: compact top-level IDs may shift when a base
		// community is added/removed, while its medoid identity remains stable.
		for _, candidate := range s.states {
			if candidate.Synthetic && candidate.Scope == scope && intersectsInt64(candidate.ChildMedoids, itemMedoids) {
				out = append(out, candidate)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClusterID < out[j].ClusterID })
	return out
}

func preserveSyntheticGroups(items []*hierarchyNode, existing []ClusterHierarchyState, fanout int, adj map[int64]map[int64]int) []syntheticGroup {
	if len(existing) == 0 {
		return nil
	}
	byMedoid := map[int64]*hierarchyNode{}
	byID := map[int64]*hierarchyNode{}
	for _, item := range items {
		byMedoid[item.cluster.MedoidID] = item
		byID[item.cluster.ID] = item
	}
	used := map[int64]bool{}
	groups := make([]syntheticGroup, 0, len(existing))
	for i := range existing {
		group := syntheticGroup{existing: &existing[i]}
		for childIndex, childID := range existing[i].ChildIDs {
			item := byID[childID]
			if item == nil && childIndex < len(existing[i].ChildMedoids) {
				item = byMedoid[existing[i].ChildMedoids[childIndex]]
			}
			if item != nil && !used[item.cluster.ID] {
				group.items = append(group.items, item)
				used[item.cluster.ID] = true
			}
		}
		if len(group.items) > 0 {
			groups = append(groups, group)
		}
	}
	for _, item := range items {
		if used[item.cluster.ID] {
			continue
		}
		best := -1
		for i := range groups {
			if len(groups[i].items) >= fanout {
				continue
			}
			if best < 0 || compareNodeGroupAffinity(item, groups[i].items, groups[best].items, adj) > 0 {
				best = i
			}
		}
		if best < 0 {
			groups = append(groups, syntheticGroup{items: []*hierarchyNode{item}})
		} else {
			groups[best].items = append(groups[best].items, item)
		}
	}
	for i := range groups {
		sortHierarchyNodes(groups[i].items)
	}
	return groups
}

func intersectsInt64(values []int64, set map[int64]bool) bool {
	for _, value := range values {
		if set[value] {
			return true
		}
	}
	return false
}

type linkageTree struct {
	items       []*hierarchyNode
	members     []int64
	left, right *linkageTree
}

func averageLinkageGroups(items []*hierarchyNode, fanout int, adj map[int64]map[int64]int) []syntheticGroup {
	forest := make([]*linkageTree, 0, len(items))
	for _, item := range items {
		forest = append(forest, &linkageTree{items: []*hierarchyNode{item}, members: descendantMembers(item)})
	}
	for len(forest) > 1 {
		bestI, bestJ := 0, 1
		for i := 0; i < len(forest); i++ {
			for j := i + 1; j < len(forest); j++ {
				if betterLinkagePair(forest[i], forest[j], forest[bestI], forest[bestJ], adj) {
					bestI, bestJ = i, j
				}
			}
		}
		merged := &linkageTree{
			items:   append(append([]*hierarchyNode(nil), forest[bestI].items...), forest[bestJ].items...),
			members: mergeSortedInt64s(forest[bestI].members, forest[bestJ].members),
			left:    forest[bestI], right: forest[bestJ],
		}
		sortHierarchyNodes(merged.items)
		forest = append(forest[:bestJ], forest[bestJ+1:]...)
		forest = append(forest[:bestI], forest[bestI+1:]...)
		forest = append(forest, merged)
		sort.Slice(forest, func(i, j int) bool {
			return hierarchyNodeSliceKey(forest[i].items) < hierarchyNodeSliceKey(forest[j].items)
		})
	}
	var cut []*linkageTree
	cutLinkageTree(forest[0], fanout, &cut)
	sort.Slice(cut, func(i, j int) bool { return hierarchyNodeSliceKey(cut[i].items) < hierarchyNodeSliceKey(cut[j].items) })
	groups := make([]syntheticGroup, 0, len(cut))
	for _, tree := range cut {
		groups = append(groups, syntheticGroup{items: tree.items})
	}
	return groups
}

func cutLinkageTree(tree *linkageTree, fanout int, out *[]*linkageTree) {
	if len(tree.items) <= fanout || tree.left == nil || tree.right == nil {
		*out = append(*out, tree)
		return
	}
	cutLinkageTree(tree.left, fanout, out)
	cutLinkageTree(tree.right, fanout, out)
}

func betterLinkagePair(a, b, bestA, bestB *linkageTree, adj map[int64]map[int64]int) bool {
	an, ad := averageAffinity(a.members, b.members, adj)
	bn, bd := averageAffinity(bestA.members, bestB.members, adj)
	left, right := an*bd, bn*ad
	if left != right {
		return left > right
	}
	return hierarchyNodeSliceKey(append(a.items, b.items...)) < hierarchyNodeSliceKey(append(bestA.items, bestB.items...))
}

func materializeSyntheticGroups(groups []syntheticGroup, level int, parentID int64, parentPath []int64, scope string, nodeByID map[int64]ClusterNode, adj map[int64]map[int64]int, usedIDs map[int64]bool) []*hierarchyNode {
	var out []*hierarchyNode
	for i := range groups {
		group := &groups[i]
		members := groupMembers(group.items)
		medoid := medoidOf(members, adj)
		childMedoids := nodeMedoids(group.items)
		id := int64(0)
		if group.existing != nil && !usedIDs[group.existing.ClusterID] {
			id = group.existing.ClusterID
			if containsInt64(members, lastMedoid(group.existing.MedoidPath)) {
				medoid = lastMedoid(group.existing.MedoidPath)
			}
		}
		if id == 0 {
			id = nextHierarchyClusterID(parentPath, childMedoids, medoid, level, true, usedIDs)
		}
		usedIDs[id] = true
		parent := &hierarchyNode{cluster: Cluster{ID: id, ParentID: parentID, MedoidID: medoid}, children: group.items, scope: scope}
		for _, child := range parent.children {
			child.cluster.ParentID = id
		}
		parent.cluster.Label = syntheticGroupLabel(parent, groups, nodeByID)
		out = append(out, parent)
	}
	return out
}

func syntheticGroupLabel(parent *hierarchyNode, siblings []syntheticGroup, nodeByID map[int64]ClusterNode) string {
	df := map[string]int{}
	for _, sibling := range siblings {
		seen := map[string]struct{}{}
		for _, id := range groupMembers(sibling.items) {
			for _, term := range clusterTerms(nodeByID[id].Text) {
				seen[term] = struct{}{}
			}
		}
		for term := range seen {
			df[term]++
		}
	}
	return clusterLabel(descendantMembers(parent), parent.cluster.MedoidID, nodeTextMap(nodeByID), df)
}

func nextHierarchyClusterID(path, siblings []int64, medoid int64, level int, synthetic bool, used map[int64]bool) int64 {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(level))
	_, _ = h.Write(buf[:])
	for _, value := range append(append([]int64(nil), path...), siblings...) {
		binary.BigEndian.PutUint64(buf[:], uint64(value))
		_, _ = h.Write(buf[:])
	}
	binary.BigEndian.PutUint64(buf[:], uint64(medoid))
	_, _ = h.Write(buf[:])
	sum := h.Sum(nil)
	base := int64(1) << 52
	mask := base - 1
	if synthetic {
		base = syntheticClusterIDBase
		mask = base - 1
	}
	id := base | int64(binary.BigEndian.Uint64(sum[:8])&uint64(mask))
	for used[id] {
		id++
		if id > base+mask {
			id = base
		}
	}
	return id
}

func flattenHierarchy(node *hierarchyNode, parentID int64, out *[]Cluster) {
	node.cluster.ParentID = parentID
	if len(node.children) > 0 {
		node.cluster.Members = nil
	}
	*out = append(*out, node.cluster)
	for _, child := range node.children {
		flattenHierarchy(child, node.cluster.ID, out)
	}
}

func hierarchyAdjacency(nodes []ClusterNode, edges []ClusterEdge) map[int64]map[int64]int {
	adj := make(map[int64]map[int64]int, len(nodes))
	for _, node := range nodes {
		if adj[node.ID] == nil {
			adj[node.ID] = map[int64]int{}
		}
	}
	for _, edge := range edges {
		if edge.A == edge.B || edge.Weight <= 0 || adj[edge.A] == nil || adj[edge.B] == nil {
			continue
		}
		adj[edge.A][edge.B] += edge.Weight
		adj[edge.B][edge.A] += edge.Weight
	}
	return adj
}

func scopeForMembers(members []int64, nodeByID map[int64]ClusterNode) string {
	counts := map[string]int{}
	for _, id := range members {
		counts[nodeByID[id].Repo]++
	}
	best, bestCount := "", -1
	for scope, count := range counts {
		if count > bestCount || (count == bestCount && scope < best) {
			best, bestCount = scope, count
		}
	}
	return best
}

func sortedStringKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortHierarchyNodes(nodes []*hierarchyNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].cluster.MedoidID != nodes[j].cluster.MedoidID {
			return nodes[i].cluster.MedoidID < nodes[j].cluster.MedoidID
		}
		return nodes[i].cluster.ID < nodes[j].cluster.ID
	})
}

func hierarchyNodeSliceKey(nodes []*hierarchyNode) string {
	medoids := nodeMedoids(nodes)
	var b strings.Builder
	for _, medoid := range medoids {
		b.WriteString(itoa(medoid))
		b.WriteByte('/')
	}
	return b.String()
}

func descendantMembers(node *hierarchyNode) []int64 {
	if len(node.children) == 0 {
		return append([]int64(nil), node.cluster.Members...)
	}
	var out []int64
	for _, child := range node.children {
		out = mergeSortedInt64s(out, descendantMembers(child))
	}
	return out
}

func groupMembers(nodes []*hierarchyNode) []int64 {
	var out []int64
	for _, node := range nodes {
		out = mergeSortedInt64s(out, descendantMembers(node))
	}
	return out
}

func clusterMedoids(clusters []Cluster) []int64 {
	medoids := make([]int64, len(clusters))
	for i := range clusters {
		medoids[i] = clusters[i].MedoidID
	}
	return medoids
}

func nodeMedoids(nodes []*hierarchyNode) []int64 {
	medoids := make([]int64, len(nodes))
	for i := range nodes {
		medoids[i] = nodes[i].cluster.MedoidID
	}
	sort.Slice(medoids, func(i, j int) bool { return medoids[i] < medoids[j] })
	return medoids
}

func equalMedoids(a, b []int64) bool {
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

func appendPath(path []int64, medoid int64) []int64 {
	out := append([]int64(nil), path...)
	return append(out, medoid)
}

func medoidPathKey(path []int64) string {
	var b strings.Builder
	for _, medoid := range path {
		b.WriteString(itoa(medoid))
		b.WriteByte('/')
	}
	return b.String()
}

func compareNodeGroupAffinity(item *hierarchyNode, candidate, best []*hierarchyNode, adj map[int64]map[int64]int) int {
	in, id := averageAffinity(descendantMembers(item), groupMembers(candidate), adj)
	bn, bd := averageAffinity(descendantMembers(item), groupMembers(best), adj)
	left, right := in*bd, bn*id
	if left > right {
		return 1
	}
	if left < right {
		return -1
	}
	if hierarchyNodeSliceKey(candidate) < hierarchyNodeSliceKey(best) {
		return 1
	}
	return -1
}

func averageAffinity(a, b []int64, adj map[int64]map[int64]int) (int64, int64) {
	denominator := int64(len(a) * len(b))
	if denominator == 0 {
		return 0, 1
	}
	var numerator int64
	for _, left := range a {
		for _, right := range b {
			numerator += int64(adj[left][right])
		}
	}
	return numerator, denominator
}

func mergeSortedInt64s(a, b []int64) []int64 {
	out := make([]int64, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] <= b[j] {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

func containsInt64(values []int64, target int64) bool {
	i := sort.Search(len(values), func(i int) bool { return values[i] >= target })
	return i < len(values) && values[i] == target
}

func lastMedoid(path []int64) int64 {
	if len(path) == 0 {
		return 0
	}
	return path[len(path)-1]
}

func nodeTextMap(nodes map[int64]ClusterNode) map[int64]string {
	out := make(map[int64]string, len(nodes))
	for id, node := range nodes {
		out[id] = node.Text
	}
	return out
}

func splitSizeEligible(size int, existing bool) bool {
	if existing {
		return size > ClusterSplitKeepThreshold
	}
	return size >= ClusterSplitThreshold
}

// validChildSplit accepts only a complete partition into at least two real
// communities, each meeting the minimum size. The total guard also rejects a
// malformed result that omitted or duplicated a parent member.
func validChildSplit(children []Cluster, parentSize int) bool {
	if len(children) < 2 {
		return false
	}
	total := 0
	for _, child := range children {
		if child.ID == UnclusteredID || child.MedoidID == 0 || len(child.Members) < ClusterMinChildFacts {
			return false
		}
		total += len(child.Members)
	}
	return total == parentSize
}

// nextChildClusterID places children in the high positive JSON-safe integer
// range, keeping them disjoint from the compact 1-based top-level IDs. The hash
// input is the hierarchy identity tuple: parent medoid, ordered sibling medoids,
// and this child's medoid. Collision probing is deterministic because parents
// and children are traversed in deterministic medoid order.
func nextChildClusterID(parentMedoid int64, orderedSiblingMedoids []int64, childMedoid int64, used map[int64]bool) int64 {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(parentMedoid))
	_, _ = h.Write(buf[:])
	for _, medoid := range orderedSiblingMedoids {
		binary.BigEndian.PutUint64(buf[:], uint64(medoid))
		_, _ = h.Write(buf[:])
	}
	binary.BigEndian.PutUint64(buf[:], uint64(childMedoid))
	_, _ = h.Write(buf[:])
	sum := h.Sum(nil)
	const childIDBase = int64(1) << 52
	const childIDMask = childIDBase - 1
	id := childIDBase | int64(binary.BigEndian.Uint64(sum[:8])&uint64(childIDMask))
	for used[id] {
		if id == childIDBase|childIDMask {
			id = childIDBase
		} else {
			id++
		}
	}
	return id
}

// dominantLabel returns the neighbor label with the greatest summed edge weight,
// breaking ties by the LOWEST label value. Candidate labels are collected then
// sorted ascending so the scan is order-independent and the tie-break is exact.
func dominantLabel(nbrs map[int64]int, label map[int64]int64) (int64, bool) {
	weightByLabel := map[int64]int{}
	for nbr, w := range nbrs {
		weightByLabel[label[nbr]] += w
	}
	if len(weightByLabel) == 0 {
		return 0, false
	}
	cands := make([]int64, 0, len(weightByLabel))
	for l := range weightByLabel {
		cands = append(cands, l)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i] < cands[j] })
	best := cands[0]
	bestW := weightByLabel[best]
	for _, l := range cands[1:] {
		if weightByLabel[l] > bestW { // strict: first (lowest) label wins ties
			best, bestW = l, weightByLabel[l]
		}
	}
	return best, true
}

// medoidOf returns the member with the greatest total edge weight to the OTHER
// members of the same cluster, lowest id breaking ties. members must be sorted
// ascending so the tie-break naturally resolves to the lowest id.
func medoidOf(members []int64, adj map[int64]map[int64]int) int64 {
	if len(members) == 0 {
		return 0
	}
	inCluster := make(map[int64]struct{}, len(members))
	for _, id := range members {
		inCluster[id] = struct{}{}
	}
	medoid := members[0]
	bestW := -1
	for _, id := range members { // ascending: first max wins == lowest id tie-break
		sum := 0
		for nbr, w := range adj[id] {
			if _, ok := inCluster[nbr]; ok {
				sum += w
			}
		}
		if sum > bestW {
			medoid, bestW = id, sum
		}
	}
	return medoid
}

// clusterLabel picks up to clusterMaxLabelTerms distinctive terms and joins them
// with '-'. Candidate terms are ANCHORED to the medoid fact (for label stability
// as membership shifts), ranked by (term-frequency-across-the-cluster DESC,
// corpus document-frequency ASC, term ASC) — a tf-idf-shaped ordering kept in
// integers so it is byte-deterministic (no float compares). Frequent-inside-yet-
// rare-in-corpus terms win. A cluster whose medoid yields no usable terms falls
// back to a stable "cluster-<medoid>" label.
func clusterLabel(members []int64, medoid int64, textByID map[int64]string, df map[string]int) string {
	// Term frequency summed across every member of the cluster.
	tf := map[string]int{}
	for _, id := range members {
		for _, t := range clusterTerms(textByID[id]) {
			tf[t]++
		}
	}
	// Candidate pool = distinct terms of the medoid fact.
	seen := map[string]struct{}{}
	var cands []string
	for _, t := range clusterTerms(textByID[medoid]) {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		cands = append(cands, t)
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if tf[a] != tf[b] {
			return tf[a] > tf[b]
		}
		if df[a] != df[b] {
			return df[a] < df[b]
		}
		return a < b
	})
	if len(cands) > clusterMaxLabelTerms {
		cands = cands[:clusterMaxLabelTerms]
	}
	if len(cands) == 0 {
		return "cluster-" + itoa(medoid)
	}
	return strings.Join(cands, "-")
}

// clusterTerms tokenizes fact text into normalized label terms using the SAME
// alphanumeric tokenization + stopword policy as the FTS query builder
// (SanitizeFTSQuery), plus a light deterministic plural fold so "clusters" and
// "cluster" collapse to one term. Returns terms in text order WITH repeats (the
// caller counts them for term frequency); pure and allocation-cheap.
func clusterTerms(text string) []string {
	var out []string
	for _, raw := range wordRun.FindAllString(text, -1) {
		tok := strings.ToLower(raw)
		if len(tok) < 3 {
			continue
		}
		if _, ok := ftsKeywords[tok]; ok {
			continue
		}
		if _, ok := tinyStopwords[tok]; ok {
			continue
		}
		out = append(out, foldTerm(tok))
	}
	return out
}

// foldTerm applies a light, deterministic plural fold: a trailing 's' is dropped
// when the remaining stem is at least 3 chars and the word does not end in 'ss'
// (so "class" stays "class" but "clusters" -> "cluster"). It is intentionally
// minimal — a full porter stemmer is unnecessary for label distinctiveness and a
// hand-rolled one would risk surprising, less-legible labels.
func foldTerm(tok string) string {
	if len(tok) >= 4 && strings.HasSuffix(tok, "s") && !strings.HasSuffix(tok, "ss") {
		return tok[:len(tok)-1]
	}
	return tok
}

// itoa is a tiny dependency-free int64 -> string for the fallback label.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
