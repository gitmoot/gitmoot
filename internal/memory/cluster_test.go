package memory

import (
	"reflect"
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

// TestBuildClusterHierarchyDeterministic uses a graph whose full pass produces
// a 22-fact parent and whose induced second pass produces valid 18/4 children.
// Repeating the same seed must preserve the complete tree, including hashed ids.
func TestBuildClusterHierarchyDeterministic(t *testing.T) {
	nodes := make([]ClusterNode, 28)
	for i := range nodes {
		nodes[i] = ClusterNode{ID: int64(i + 1), Text: "hierarchy deterministic fact"}
	}
	raw := [][3]int{
		{0, 11, 4}, {1, 8, 5}, {1, 17, 5}, {1, 20, 1}, {2, 4, 4}, {2, 9, 2},
		{2, 14, 1}, {2, 15, 4}, {2, 18, 2}, {2, 22, 2}, {3, 12, 3}, {3, 23, 4},
		{3, 26, 1}, {4, 14, 1}, {4, 20, 5}, {4, 23, 3}, {4, 24, 4}, {5, 16, 3},
		{5, 18, 5}, {5, 22, 1}, {5, 25, 3}, {6, 9, 3}, {6, 14, 2}, {6, 27, 1},
		{7, 14, 4}, {7, 21, 3}, {7, 27, 4}, {9, 16, 4}, {10, 14, 2}, {10, 15, 4},
		{11, 13, 3}, {11, 24, 2}, {12, 15, 1}, {13, 20, 1}, {14, 22, 1}, {15, 16, 4},
		{15, 17, 5}, {15, 18, 4}, {15, 20, 5}, {15, 21, 2}, {15, 23, 4}, {15, 25, 5},
		{15, 27, 3}, {16, 19, 5}, {16, 24, 1}, {18, 22, 2}, {18, 27, 5}, {19, 27, 1},
		{20, 22, 3}, {20, 23, 2}, {20, 25, 5}, {21, 26, 3}, {23, 25, 3}, {24, 27, 1},
		{26, 27, 3},
	}
	edges := make([]ClusterEdge, 0, len(raw))
	for _, e := range raw {
		edges = append(edges, ClusterEdge{A: int64(e[0] + 1), B: int64(e[1] + 1), Weight: e[2]})
	}

	first := BuildClusterHierarchy(nodes, edges, nil)
	second := BuildClusterHierarchy(nodes, edges, nil)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("hierarchy not deterministic:\nfirst=%+v\nsecond=%+v", first, second)
	}
	parents, children := hierarchyShape(first)
	if parents != 3 || children != 2 {
		t.Fatalf("hierarchy shape = %d parents/%d children, want 3/2: %+v", parents, children, first)
	}
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
	state := map[int64]bool{}
	if existing {
		state[1] = true
	}
	return buildClusterHierarchy(top, nodes, edges, state)
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
