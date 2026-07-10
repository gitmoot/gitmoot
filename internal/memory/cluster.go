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

// ClusterNode is one fact participating in the similarity graph. Text is the
// concatenation the labeler tokenizes (key + content); the caller supplies it so
// this package never re-derives the fact shape.
type ClusterNode struct {
	ID   int64
	Text string
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
	ID       int64   // 1-based; UnclusteredID (0) for the degree-0 bucket
	ParentID int64   // 0 for top-level clusters; children point at their top-level parent
	Label    string  // computed distinctive-term label (override applied by the store, not here)
	MedoidID int64   // member with the highest intra-cluster similarity (lowest id tie-break); 0 for unclustered
	Members  []int64 // member fact ids, ascending
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

// BuildClusterHierarchy builds the flat top-level communities, then gives every
// eligible real community one more pass over its induced internal subgraph.
// ExistingSplitParentMedoids identifies persisted parents that currently have
// children; those parents use the lower hysteresis boundary instead of the split
// trigger. The result has at most two levels. A split parent carries no direct
// Members: its children are the leaves and facts attach only to leaves.
//
// Child labels come directly from BuildClusters over the sibling-only corpus, so
// their tf-idf document frequency is contrastive within the parent. Child IDs
// are derived from the stable parent medoid plus the ordered sibling medoids.
// The same graph and existing-split state therefore yield the same tree.
func BuildClusterHierarchy(nodes []ClusterNode, edges []ClusterEdge, existingSplitParentMedoids map[int64]bool) ClusterResult {
	top := BuildClusters(nodes, edges)
	return buildClusterHierarchy(top, nodes, edges, existingSplitParentMedoids)
}

func buildClusterHierarchy(top ClusterResult, nodes []ClusterNode, edges []ClusterEdge, existingSplitParentMedoids map[int64]bool) ClusterResult {
	nodeByID := make(map[int64]ClusterNode, len(nodes))
	for _, n := range nodes {
		if _, seen := nodeByID[n.ID]; !seen {
			nodeByID[n.ID] = n
		}
	}

	usedIDs := make(map[int64]bool, len(top.Clusters))
	for _, c := range top.Clusters {
		usedIDs[c.ID] = true
	}

	out := ClusterResult{Clusters: make([]Cluster, 0, len(top.Clusters))}
	for _, parent := range top.Clusters {
		parent.ParentID = 0
		if parent.ID == UnclusteredID || !splitSizeEligible(len(parent.Members), existingSplitParentMedoids[parent.MedoidID]) {
			out.Clusters = append(out.Clusters, parent)
			continue
		}

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

		children := BuildClusters(subNodes, subEdges).Clusters
		if !validChildSplit(children, len(parent.Members)) {
			out.Clusters = append(out.Clusters, parent)
			continue
		}

		medoids := make([]int64, len(children))
		for i := range children {
			medoids[i] = children[i].MedoidID
		}
		parent.Members = nil
		out.Clusters = append(out.Clusters, parent)
		for i := range children {
			children[i].ID = nextChildClusterID(parent.MedoidID, medoids, children[i].MedoidID, usedIDs)
			children[i].ParentID = parent.ID
			usedIDs[children[i].ID] = true
			out.Clusters = append(out.Clusters, children[i])
		}
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
