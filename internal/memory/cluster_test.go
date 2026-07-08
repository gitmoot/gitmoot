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
