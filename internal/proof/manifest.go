// Package proof projects structured Gitmoot store records into a deterministic,
// content-addressed evidence manifest. It deliberately has no CLI or network
// coupling.
package proof

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

const hashPrefix = "sha256:"

// Kind identifies one node in a proof manifest.
type Kind string

const (
	KindRoot       Kind = "root"
	KindSession    Kind = "session"
	KindDelegation Kind = "delegation"
	KindCommit     Kind = "commit"
	KindTest       Kind = "test"
	KindReview     Kind = "review"
	KindPR         Kind = "pr"
)

// Grade is the strength of evidence behind a claim.
type Grade string

const (
	GradeReported Grade = "reported"
	GradeObserved Grade = "observed"
	GradeVerified Grade = "verified"
)

// Claim is one evidence-graded assertion attached to a node.
type Claim struct {
	Type        string `json:"type"`
	Grade       Grade  `json:"grade"`
	Source      string `json:"source"`
	EvidenceRef string `json:"evidence_ref"`
	AsOf        string `json:"as_of"`
}

// Node is one content-addressed record. ID is the SHA-256 of the canonical JSON
// formed by every field except ID; parent nodes refer to child IDs.
type Node struct {
	ID       string            `json:"id"`
	Kind     Kind              `json:"kind"`
	Ref      string            `json:"ref"`
	Attrs    map[string]string `json:"attrs"`
	Children []string          `json:"children"`
	Claims   []Claim           `json:"claims"`
}

// Manifest is the portable content-addressed proof artifact. Root is repeated
// in Nodes so consumers can start either from ProofID or the embedded anchor.
type Manifest struct {
	ProofID string          `json:"proof_id"`
	Root    Node            `json:"root"`
	Nodes   map[string]Node `json:"nodes"`
}

type nodeContent struct {
	Kind     Kind              `json:"kind"`
	Ref      string            `json:"ref"`
	Attrs    map[string]string `json:"attrs"`
	Children []string          `json:"children"`
	Claims   []Claim           `json:"claims"`
}

// NodeID recomputes the content address of node.
func NodeID(node Node) string {
	normalizeNode(&node)
	raw, err := json.Marshal(nodeContent{
		Kind: node.Kind, Ref: node.Ref, Attrs: node.Attrs,
		Children: node.Children, Claims: node.Claims,
	})
	if err != nil {
		panic(fmt.Sprintf("proof: marshal canonical node: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hashPrefix + hex.EncodeToString(sum[:])
}

// Marshal returns the canonical JSON representation used by `proof --json`.
// encoding/json sorts string map keys, while projection normalizes all slices.
func Marshal(manifest Manifest) ([]byte, error) {
	return json.Marshal(manifest)
}

// WithVerifiedRootClaim returns a new manifest whose root commits an additional
// store-only verification claim. Existing child hashes are retained; the root
// and ProofID are re-addressed, so the claim cannot be removed without changing
// the public proof identifier.
func WithVerifiedRootClaim(manifest Manifest, claimType, source, evidenceRef, asOf string, attrs map[string]string) (Manifest, error) {
	if err := VerifyManifest(manifest); err != nil {
		return Manifest{}, err
	}
	root := manifest.Root
	root.Attrs = cloneStringAttrs(root.Attrs)
	for key, value := range attrs {
		root.Attrs[key] = value
	}
	root.Children = append([]string(nil), root.Children...)
	root.Claims = append(append([]Claim(nil), root.Claims...), Claim{
		Type: claimType, Grade: GradeVerified, Source: source,
		EvidenceRef: evidenceRef, AsOf: asOf,
	})
	normalizeNode(&root)
	oldID := manifest.ProofID
	root.ID = NodeID(root)
	nodes := make(map[string]Node, len(manifest.Nodes))
	for id, node := range manifest.Nodes {
		if id != oldID {
			nodes[id] = node
		}
	}
	nodes[root.ID] = root
	updated := Manifest{ProofID: root.ID, Root: root, Nodes: nodes}
	if err := VerifyManifest(updated); err != nil {
		return Manifest{}, err
	}
	return updated, nil
}

func cloneStringAttrs(attrs map[string]string) map[string]string {
	cloned := make(map[string]string, len(attrs))
	for key, value := range attrs {
		cloned[key] = value
	}
	return cloned
}

// VerifyManifest recomputes node hashes and validates child references, DAG
// acyclicity, and that every shipped node is committed by ProofID.
func VerifyManifest(manifest Manifest) error {
	if manifest.ProofID == "" {
		return errors.New("proof id is empty")
	}
	root, ok := manifest.Nodes[manifest.ProofID]
	if !ok {
		return fmt.Errorf("proof root %q is absent from nodes", manifest.ProofID)
	}
	if NodeID(root) != manifest.ProofID || NodeID(manifest.Root) != manifest.ProofID {
		return errors.New("proof root content hash does not match")
	}
	for id, node := range manifest.Nodes {
		if node.ID != id || NodeID(node) != id {
			return fmt.Errorf("node %q content hash does not match", id)
		}
		for _, child := range node.Children {
			if _, exists := manifest.Nodes[child]; !exists {
				return fmt.Errorf("node %q references missing child %q", id, child)
			}
		}
	}
	reachable, err := reachableNodeIDs(manifest)
	if err != nil {
		return err
	}
	if len(reachable) != len(manifest.Nodes) {
		ids := make([]string, 0, len(manifest.Nodes))
		for id := range manifest.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if _, ok := reachable[id]; !ok {
				return fmt.Errorf("unreachable node %q not committed by proof id", id)
			}
		}
	}
	return nil
}

func reachableNodeIDs(manifest Manifest) (map[string]struct{}, error) {
	state := make(map[string]uint8, len(manifest.Nodes))
	reachable := make(map[string]struct{}, len(manifest.Nodes))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("proof DAG contains a cycle at %q", id)
		case 2:
			return nil
		}
		node, ok := manifest.Nodes[id]
		if !ok {
			return fmt.Errorf("proof DAG references missing node %q", id)
		}
		state[id] = 1
		reachable[id] = struct{}{}
		for _, child := range node.Children {
			if err := visit(child); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	if err := visit(manifest.ProofID); err != nil {
		return nil, err
	}
	return reachable, nil
}

type builder struct {
	nodes map[string]Node
}

func newBuilder() *builder {
	return &builder{nodes: make(map[string]Node)}
}

func (b *builder) add(node Node) string {
	node.Claims = append(node.Claims, Claim{
		Type: "integrity.content_hash", Grade: GradeVerified,
		Source: "proof.projector", EvidenceRef: "self", AsOf: node.Attrs["as_of"],
	})
	normalizeNode(&node)
	node.ID = NodeID(node)
	b.nodes[node.ID] = node
	return node.ID
}

func normalizeNode(node *Node) {
	if node.Attrs == nil {
		node.Attrs = map[string]string{}
	}
	if node.Children == nil {
		node.Children = []string{}
	}
	if node.Claims == nil {
		node.Claims = []Claim{}
	}
	sort.Strings(node.Children)
	sort.Slice(node.Claims, func(i, j int) bool {
		a, b := node.Claims[i], node.Claims[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.Grade != b.Grade {
			return a.Grade < b.Grade
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.EvidenceRef != b.EvidenceRef {
			return a.EvidenceRef < b.EvidenceRef
		}
		return a.AsOf < b.AsOf
	})
}
