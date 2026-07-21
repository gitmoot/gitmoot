package memory

import (
	"reflect"
	"sort"
	"testing"
)

// triangleEdges returns the three undirected edges of a triangle over a,b,c with
// the given weight.
func triangleEdges(a, b, c int64, w int) []ClusterEdge {
	return []ClusterEdge{{A: a, B: b, Weight: w}, {A: b, B: c, Weight: w}, {A: a, B: c, Weight: w}}
}

// TestBuildClustersDeterministic runs the exact same graph three times and asserts
// byte-identical output — the hard determinism requirement (#763).
func TestBuildClustersDeterministic(t *testing.T) {
	nodes := []ClusterNode{
		{ID: 1, Text: "database index query"},
		{ID: 2, Text: "database index query"},
		{ID: 3, Text: "database index query"},
		{ID: 4, Text: "network retry socket"},
		{ID: 5, Text: "network retry socket"},
		{ID: 6, Text: "network retry socket"},
		{ID: 7, Text: "isolated orphan fact"},
	}
	var edges []ClusterEdge
	edges = append(edges, triangleEdges(1, 2, 3, 4)...)
	edges = append(edges, triangleEdges(4, 5, 6, 4)...)
	// node 7 has no edges -> unclustered.

	first := BuildClusters(nodes, edges)
	for i := 0; i < 3; i++ {
		got := BuildClusters(nodes, edges)
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("BuildClusters not deterministic on run %d:\n first=%+v\n got  =%+v", i, first, got)
		}
	}

	// Two real communities + the unclustered bucket last.
	if len(first.Clusters) != 3 {
		t.Fatalf("clusters = %d, want 3 (2 communities + unclustered): %+v", len(first.Clusters), first.Clusters)
	}
	// Real communities numbered from 1 in ascending medoid-id order.
	a := first.Clusters[0]
	b := first.Clusters[1]
	unc := first.Clusters[2]
	if a.ID != 1 || b.ID != 2 || unc.ID != UnclusteredID {
		t.Fatalf("cluster ids = %d,%d,%d, want 1,2,%d", a.ID, b.ID, unc.ID, UnclusteredID)
	}
	if !reflect.DeepEqual(a.Members, []int64{1, 2, 3}) {
		t.Fatalf("cluster A members = %v, want [1 2 3]", a.Members)
	}
	if !reflect.DeepEqual(b.Members, []int64{4, 5, 6}) {
		t.Fatalf("cluster B members = %v, want [4 5 6]", b.Members)
	}
	// Medoid of a symmetric triangle is the lowest id.
	if a.MedoidID != 1 || b.MedoidID != 4 {
		t.Fatalf("medoids = %d,%d, want 1,4", a.MedoidID, b.MedoidID)
	}
	// The isolated node lands in the unclustered bucket.
	if unc.Label != UnclusteredLabel || !reflect.DeepEqual(unc.Members, []int64{7}) {
		t.Fatalf("unclustered = %q %v, want %q [7]", unc.Label, unc.Members, UnclusteredLabel)
	}
}

// TestBuildClustersLabelDistinctiveness asserts the tf-idf-shaped label picks the
// cluster's own distinctive terms (never a term shared across the whole corpus).
func TestBuildClustersLabelDistinctiveness(t *testing.T) {
	// Both clusters share the generic word "fact"; each has its own distinctive
	// vocabulary. The generic term (df=2) must lose to the distinctive terms (df=1).
	nodes := []ClusterNode{
		{ID: 1, Text: "fact database index query"},
		{ID: 2, Text: "fact database index query"},
		{ID: 3, Text: "fact database index query"},
		{ID: 4, Text: "fact network retry socket"},
		{ID: 5, Text: "fact network retry socket"},
		{ID: 6, Text: "fact network retry socket"},
	}
	var edges []ClusterEdge
	edges = append(edges, triangleEdges(1, 2, 3, 4)...)
	edges = append(edges, triangleEdges(4, 5, 6, 4)...)

	res := BuildClusters(nodes, edges)
	if len(res.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2: %+v", len(res.Clusters), res.Clusters)
	}
	// Ranking is (tf DESC, df ASC, term ASC). Within a cluster every term has the
	// same tf(3), but "fact" has df=2 (both clusters) so it ranks last; the three
	// distinctive terms tie on df=1 and fall back to alphabetical order.
	if got, want := res.Clusters[0].Label, "database-index-query"; got != want {
		t.Fatalf("cluster A label = %q, want %q", got, want)
	}
	if got, want := res.Clusters[1].Label, "network-retry-socket"; got != want {
		t.Fatalf("cluster B label = %q, want %q", got, want)
	}
}

// TestBuildClustersPairAndSingletons covers a two-node community and multiple
// isolated nodes all sharing the one unclustered bucket.
func TestBuildClustersPairAndSingletons(t *testing.T) {
	nodes := []ClusterNode{
		{ID: 1, Text: "alpha alpha alpha"},
		{ID: 2, Text: "alpha alpha alpha"},
		{ID: 3, Text: "lonely"},
		{ID: 4, Text: "solo"},
	}
	edges := []ClusterEdge{{A: 1, B: 2, Weight: 3}}

	res := BuildClusters(nodes, edges)
	if len(res.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2 (one pair + unclustered): %+v", len(res.Clusters), res.Clusters)
	}
	pair := res.Clusters[0]
	unc := res.Clusters[1]
	if pair.ID != 1 || !reflect.DeepEqual(pair.Members, []int64{1, 2}) {
		t.Fatalf("pair = id%d %v, want id1 [1 2]", pair.ID, pair.Members)
	}
	if unc.ID != UnclusteredID || !reflect.DeepEqual(unc.Members, []int64{3, 4}) {
		t.Fatalf("unclustered = id%d %v, want id0 [3 4]", unc.ID, unc.Members)
	}
}

// TestBuildClustersEmpty asserts an empty graph yields no clusters.
func TestBuildClustersEmpty(t *testing.T) {
	res := BuildClusters(nil, nil)
	if len(res.Clusters) != 0 {
		t.Fatalf("empty graph clusters = %d, want 0", len(res.Clusters))
	}
}

// TestMedoidHighestIntraSimilarity asserts the medoid is the most-connected member
// (highest total intra-cluster edge weight), lowest id on ties.
func TestMedoidHighestIntraSimilarity(t *testing.T) {
	// Star: node 2 is central (connects to 1,3,4); the spokes each touch only 2.
	nodes := []ClusterNode{
		{ID: 1, Text: "hub spoke"},
		{ID: 2, Text: "hub spoke"},
		{ID: 3, Text: "hub spoke"},
		{ID: 4, Text: "hub spoke"},
	}
	edges := []ClusterEdge{
		{A: 2, B: 1, Weight: 5},
		{A: 2, B: 3, Weight: 5},
		{A: 2, B: 4, Weight: 5},
	}
	res := BuildClusters(nodes, edges)
	if len(res.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1: %+v", len(res.Clusters), res.Clusters)
	}
	if res.Clusters[0].MedoidID != 2 {
		t.Fatalf("medoid = %d, want 2 (the hub)", res.Clusters[0].MedoidID)
	}
}

func TestClusterHierarchyFanoutDeterministicAndHysteretic(t *testing.T) {
	top13, nodes13, edges13 := crowdedTopLevelFixture(13, "acme/repo")
	options := ClusterHierarchyOptions{Fanout: 12, FanoutKeep: 9, DepthCap: 4}
	first := buildClusterHierarchyWithOptions(top13, nodes13, edges13, options)
	if maxChildren(first) > 12 {
		t.Fatalf("fanout = %d, want <= 12: %+v", maxChildren(first), first)
	}
	state := topSyntheticState(first, "acme/repo")
	options.Existing = state
	second := buildClusterHierarchyWithOptions(top13, nodes13, edges13, options)
	third := buildClusterHierarchyWithOptions(top13, nodes13, edges13, options)
	if !reflect.DeepEqual(second, third) {
		t.Fatalf("unchanged hierarchy with persisted state is not deterministic:\nsecond=%+v\nthird=%+v", second, third)
	}

	top11, nodes11, edges11 := crowdedTopLevelFixture(11, "acme/repo")
	shrunk := buildClusterHierarchyWithOptions(top11, nodes11, edges11, options)
	if syntheticCount(shrunk) == 0 {
		t.Fatalf("13->11 boundary dissolved existing grouping above keep=9: %+v", shrunk)
	}
	oldIDs := syntheticIDs(first)
	for id := range syntheticIDs(shrunk) {
		if oldIDs[id] {
			return
		}
	}
	t.Fatalf("13->11 boundary retained no synthetic parent identity: first=%+v shrunk=%+v", first, shrunk)
}

func TestClusterHierarchyThresholdAndMinChild(t *testing.T) {
	if got := manualSplitResult(9, 10, false); childCount(got) != 0 {
		t.Fatalf("19 facts split below trigger: %+v", got)
	}
	if got := manualSplitResult(10, 10, false); childCount(got) != 2 {
		t.Fatalf("20 facts did not split into valid children: %+v", got)
	} else if got.Clusters[0].Label != "parent" || got.Clusters[1].Label == got.Clusters[2].Label {
		t.Fatalf("parent label or sibling-contrastive child labels were not preserved: %+v", got)
	}
	if got := manualSplitResult(3, 17, false); childCount(got) != 0 {
		t.Fatalf("split with a 3-fact child must be rejected: %+v", got)
	}
}

func TestClusterHierarchyHysteresis(t *testing.T) {
	initial := manualSplitResult(10, 11, false)
	if childCount(initial) != 2 {
		t.Fatalf("21-fact parent did not split: %+v", initial)
	}
	preserved := manualSplitResult(6, 7, true)
	if childCount(preserved) != 2 {
		t.Fatalf("13-fact existing split was not preserved: %+v", preserved)
	}
	dissolved := manualSplitResult(5, 6, true)
	if childCount(dissolved) != 0 || len(dissolved.Clusters) != 1 || len(dissolved.Clusters[0].Members) != 11 {
		t.Fatalf("11-fact split did not dissolve to its parent: %+v", dissolved)
	}
}

func TestClusterHierarchyCrowdingIsPerRepoScope(t *testing.T) {
	topA, nodesA, edgesA := crowdedTopLevelFixture(13, "acme/a")
	topB, nodesB, edgesB := crowdedTopLevelFixtureOffset(8, 100, "acme/b")
	top := ClusterResult{Clusters: append(topA.Clusters, topB.Clusters...)}
	nodes := append(nodesA, nodesB...)
	edges := append(edgesA, edgesB...)
	result := buildClusterHierarchyWithOptions(top, nodes, edges, ClusterHierarchyOptions{Fanout: 12, FanoutKeep: 9, DepthCap: 4})
	byID := map[int64]Cluster{}
	for _, cluster := range result.Clusters {
		byID[cluster.ID] = cluster
	}
	for _, cluster := range result.Clusters {
		if cluster.BaseCommunity && cluster.MedoidID >= 101 && cluster.ParentID != 0 {
			t.Fatalf("uncrowded repo B cluster was grouped: %+v", cluster)
		}
	}
	if syntheticCount(result) == 0 {
		t.Fatalf("crowded repo A was not grouped: %+v", result)
	}
	_ = byID
}

func TestClusterHierarchyFanoutUsesMultipleLevelsWithinDepthCap(t *testing.T) {
	top, nodes, edges := crowdedTopLevelFixture(145, "acme/repo")
	options := ClusterHierarchyOptions{Fanout: 12, FanoutKeep: 9, DepthCap: 4}
	result := buildClusterHierarchyWithOptions(top, nodes, edges, options)
	if roots := hierarchyRootCount(result); roots > 12 {
		t.Fatalf("root fanout = %d, want <= 12", roots)
	}
	if children := maxChildren(result); children > 12 {
		t.Fatalf("child fanout = %d, want <= 12", children)
	}
	if depth := hierarchyMaxDepth(result); depth > 4 {
		t.Fatalf("depth = %d, want <= 4", depth)
	}
	options.Existing = hierarchyStateForScope(result, "acme/repo")
	rebuilt := buildClusterHierarchyWithOptions(top, nodes, edges, options)
	if !reflect.DeepEqual(result, rebuilt) {
		t.Fatalf("multi-level hierarchy changed with persisted medoid-path state:\nfirst=%+v\nrebuilt=%+v", result, rebuilt)
	}
}

func TestClusterHierarchyRecursesOversizedChildAndHonorsDepthCap(t *testing.T) {
	nodes := make([]ClusterNode, 0, 24)
	for i := 1; i <= 24; i++ {
		nodes = append(nodes, ClusterNode{ID: int64(i), Text: "alpha storage", Repo: "acme/repo"})
	}
	edges := append(cliqueClusterEdges(1, 12), cliqueClusterEdges(13, 24)...)
	nodeByID := map[int64]ClusterNode{}
	for _, node := range nodes {
		nodeByID[node.ID] = node
	}
	childMembers := make([]int64, 24)
	for i := range childMembers {
		childMembers[i] = int64(i + 1)
	}
	makeRoot := func() *hierarchyNode {
		child := &hierarchyNode{cluster: Cluster{ID: 2, MedoidID: 1, Members: append([]int64(nil), childMembers...)}, scope: "acme/repo"}
		return &hierarchyNode{cluster: Cluster{ID: 1, MedoidID: 1}, children: []*hierarchyNode{child}, scope: "acme/repo"}
	}
	used := map[int64]bool{1: true, 2: true}
	root := makeRoot()
	expandHierarchyNode(root, 1, []int64{1}, nodeByID, edges, hierarchyAdjacency(nodes, edges), hierarchyStateIndex{}, ClusterHierarchyOptions{Fanout: 12, FanoutKeep: 9, DepthCap: 4}, used)
	if len(root.children) != 1 || len(root.children[0].children) != 2 {
		t.Fatalf("oversized child did not recursively split: %+v", root)
	}

	root = makeRoot()
	used = map[int64]bool{1: true, 2: true}
	expandHierarchyNode(root, 1, []int64{1}, nodeByID, edges, hierarchyAdjacency(nodes, edges), hierarchyStateIndex{}, ClusterHierarchyOptions{Fanout: 12, FanoutKeep: 9, DepthCap: 2}, used)
	if len(root.children[0].children) != 0 {
		t.Fatalf("depth cap 2 allowed grandchildren: %+v", root)
	}
}

func crowdedTopLevelFixture(count int, repo string) (ClusterResult, []ClusterNode, []ClusterEdge) {
	return crowdedTopLevelFixtureOffset(count, 0, repo)
}

func crowdedTopLevelFixtureOffset(count int, offset int64, repo string) (ClusterResult, []ClusterNode, []ClusterEdge) {
	var top ClusterResult
	var nodes []ClusterNode
	var edges []ClusterEdge
	for i := 1; i <= count; i++ {
		id := offset + int64(i)
		nodes = append(nodes, ClusterNode{ID: id, Text: "topic " + itoa(id), Repo: repo})
		top.Clusters = append(top.Clusters, Cluster{ID: id, Label: "topic-" + itoa(id), MedoidID: id, Members: []int64{id}, BaseCommunity: true})
		if i > 1 {
			edges = append(edges, ClusterEdge{A: id - 1, B: id, Weight: count - i + 1})
		}
	}
	return top, nodes, edges
}

func topSyntheticState(result ClusterResult, scope string) []ClusterHierarchyState {
	children := map[int64][]Cluster{}
	byID := map[int64]Cluster{}
	for _, cluster := range result.Clusters {
		byID[cluster.ID] = cluster
		children[cluster.ParentID] = append(children[cluster.ParentID], cluster)
	}
	var out []ClusterHierarchyState
	for _, cluster := range result.Clusters {
		if cluster.ParentID != 0 || !IsSyntheticClusterID(cluster.ID) {
			continue
		}
		state := ClusterHierarchyState{Level: 1, MedoidPath: []int64{cluster.MedoidID}, ClusterID: cluster.ID, Scope: scope, Synthetic: true}
		for _, child := range children[cluster.ID] {
			state.ChildMedoids = append(state.ChildMedoids, child.MedoidID)
			state.ChildIDs = append(state.ChildIDs, child.ID)
		}
		out = append(out, state)
	}
	return out
}

func hierarchyStateForScope(result ClusterResult, scope string) []ClusterHierarchyState {
	children := map[int64][]Cluster{}
	var roots []Cluster
	for _, cluster := range result.Clusters {
		if cluster.ParentID == 0 {
			if cluster.ID != UnclusteredID {
				roots = append(roots, cluster)
			}
		} else {
			children[cluster.ParentID] = append(children[cluster.ParentID], cluster)
		}
	}
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			if children[parentID][i].MedoidID != children[parentID][j].MedoidID {
				return children[parentID][i].MedoidID < children[parentID][j].MedoidID
			}
			return children[parentID][i].ID < children[parentID][j].ID
		})
	}
	var out []ClusterHierarchyState
	var walk func(Cluster, int, []int64)
	walk = func(cluster Cluster, level int, path []int64) {
		path = append(append([]int64(nil), path...), cluster.MedoidID)
		kids := children[cluster.ID]
		if len(kids) > 0 {
			state := ClusterHierarchyState{Level: level, MedoidPath: path, ClusterID: cluster.ID, Scope: scope, Synthetic: IsSyntheticClusterID(cluster.ID)}
			for _, child := range kids {
				state.ChildMedoids = append(state.ChildMedoids, child.MedoidID)
				state.ChildIDs = append(state.ChildIDs, child.ID)
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
	return out
}

func maxChildren(result ClusterResult) int {
	counts := map[int64]int{}
	for _, cluster := range result.Clusters {
		if cluster.ParentID != 0 {
			counts[cluster.ParentID]++
		}
	}
	max := 0
	for _, count := range counts {
		if count > max {
			max = count
		}
	}
	return max
}

func syntheticCount(result ClusterResult) int {
	return len(syntheticIDs(result))
}

func syntheticIDs(result ClusterResult) map[int64]bool {
	out := map[int64]bool{}
	for _, cluster := range result.Clusters {
		if IsSyntheticClusterID(cluster.ID) {
			out[cluster.ID] = true
		}
	}
	return out
}

func hierarchyRootCount(result ClusterResult) int {
	count := 0
	for _, cluster := range result.Clusters {
		if cluster.ID != UnclusteredID && cluster.ParentID == 0 {
			count++
		}
	}
	return count
}

func hierarchyMaxDepth(result ClusterResult) int {
	parent := map[int64]int64{}
	for _, cluster := range result.Clusters {
		parent[cluster.ID] = cluster.ParentID
	}
	max := 0
	for _, cluster := range result.Clusters {
		depth := 1
		seen := map[int64]bool{cluster.ID: true}
		for p := parent[cluster.ID]; p != 0 && !seen[p]; p = parent[p] {
			seen[p] = true
			depth++
		}
		if depth > max {
			max = depth
		}
	}
	return max
}

func cliqueClusterEdges(first, last int) []ClusterEdge {
	var edges []ClusterEdge
	for i := first; i <= last; i++ {
		for j := i + 1; j <= last; j++ {
			edges = append(edges, ClusterEdge{A: int64(i), B: int64(j), Weight: 1})
		}
	}
	return edges
}

func manualSplitResult(left, right int, existing bool) ClusterResult {
	total := left + right
	nodes := make([]ClusterNode, 0, total)
	for i := 1; i <= total; i++ {
		word := "alpha storage"
		if i > left {
			word = "beta network"
		}
		nodes = append(nodes, ClusterNode{ID: int64(i), Text: word})
	}
	edges := append(cliqueClusterEdges(1, left), cliqueClusterEdges(left+1, total)...)
	members := make([]int64, total)
	for i := range members {
		members[i] = int64(i + 1)
	}
	top := ClusterResult{Clusters: []Cluster{{ID: 1, Label: "parent", MedoidID: 1, Members: members}}}
	options := ClusterHierarchyOptions{
		Fanout:     DefaultClusterFanout,
		FanoutKeep: DefaultClusterFanoutKeep,
		DepthCap:   DefaultClusterHierarchyCap,
	}
	if existing {
		options.Existing = []ClusterHierarchyState{{Level: 1, MedoidPath: []int64{1}}}
	}
	return buildClusterHierarchyWithOptions(top, nodes, edges, options)
}

func hierarchyShape(result ClusterResult) (parents, children int) {
	for _, c := range result.Clusters {
		if c.ParentID == 0 {
			parents++
		} else {
			children++
		}
	}
	return parents, children
}

func childCount(result ClusterResult) int {
	_, children := hierarchyShape(result)
	return children
}
