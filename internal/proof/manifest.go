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
	"path"
	"sort"
	"strconv"
	"strings"
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
	KindArtifact   Kind = "artifact"
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

// ArtifactEvidence is the store-side digest metadata for one service-run file.
// Relpath is the public, slash-separated artifacts/<stage>/<file> path; SHA256
// is the lowercase hex digest of the collected bytes.
type ArtifactEvidence struct {
	Relpath string
	Stage   string
	Size    int64
	SHA256  string
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

// WithArtifactNodes returns a manifest whose root commits one content-addressed
// artifact node per collected service output. The artifact sha256 claim is an
// integrity meta-claim: Gitmoot hashed stored bytes, but did not rerun or attest
// the command that produced them.
func WithArtifactNodes(manifest Manifest, artifacts []ArtifactEvidence, asOf string) (Manifest, error) {
	if err := VerifyManifest(manifest); err != nil {
		return Manifest{}, err
	}
	if len(artifacts) == 0 {
		return manifest, nil
	}
	ordered := append([]ArtifactEvidence(nil), artifacts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Relpath < ordered[j].Relpath })
	seen := make(map[string]struct{}, len(ordered))
	nodes := make(map[string]Node, len(manifest.Nodes)+len(ordered))
	for id, node := range manifest.Nodes {
		nodes[id] = node
	}
	root := manifest.Root
	root.Attrs = cloneStringAttrs(root.Attrs)
	root.Children = append([]string(nil), root.Children...)
	root.Claims = append([]Claim(nil), root.Claims...)
	var totalBytes int64
	for _, artifact := range ordered {
		relpath, digest, err := normalizeArtifactEvidence(artifact)
		if err != nil {
			return Manifest{}, err
		}
		if _, duplicate := seen[relpath]; duplicate {
			return Manifest{}, fmt.Errorf("duplicate artifact relpath %q", relpath)
		}
		seen[relpath] = struct{}{}
		attrs := map[string]string{
			"relpath": relpath,
			"size":    strconv.FormatInt(artifact.Size, 10),
			"sha256":  digest,
			"as_of":   asOf,
		}
		if strings.TrimSpace(artifact.Stage) != "" {
			attrs["stage"] = strings.TrimSpace(artifact.Stage)
		}
		b := newBuilder()
		id := b.add(Node{
			Kind: KindArtifact, Ref: relpath, Attrs: attrs,
			Claims: []Claim{{
				Type: "integrity.artifact_sha256", Grade: GradeVerified,
				Source: "pipeline.artifact_collector", EvidenceRef: relpath, AsOf: asOf,
			}},
		})
		nodes[id] = b.nodes[id]
		root.Children = append(root.Children, id)
		if totalBytes > (1<<63-1)-artifact.Size {
			return Manifest{}, errors.New("artifact byte total overflows int64")
		}
		totalBytes += artifact.Size
	}
	root.Attrs["artifacts_count"] = strconv.Itoa(len(ordered))
	root.Attrs["artifact_bytes"] = strconv.FormatInt(totalBytes, 10)
	normalizeNode(&root)
	oldID := manifest.ProofID
	root.ID = NodeID(root)
	delete(nodes, oldID)
	nodes[root.ID] = root
	updated := Manifest{ProofID: root.ID, Root: root, Nodes: nodes}
	if err := VerifyManifest(updated); err != nil {
		return Manifest{}, err
	}
	return updated, nil
}

// ArtifactEntries returns the verified artifact metadata committed by a
// manifest, sorted by relpath. It rejects malformed or duplicate artifact
// nodes so receipt renderers never guess at public metadata.
func ArtifactEntries(manifest Manifest) ([]ArtifactEvidence, error) {
	if err := VerifyManifest(manifest); err != nil {
		return nil, err
	}
	entries := make([]ArtifactEvidence, 0)
	seen := map[string]struct{}{}
	for _, node := range manifest.Nodes {
		if node.Kind != KindArtifact {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(node.Attrs["size"]), 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("artifact %q has invalid size", node.Ref)
		}
		entry := ArtifactEvidence{
			Relpath: node.Attrs["relpath"], Stage: node.Attrs["stage"],
			Size: size, SHA256: node.Attrs["sha256"],
		}
		relpath, digest, err := normalizeArtifactEvidence(entry)
		if err != nil {
			return nil, err
		}
		if node.Ref != relpath {
			return nil, fmt.Errorf("artifact node ref %q does not match relpath %q", node.Ref, relpath)
		}
		if _, duplicate := seen[relpath]; duplicate {
			return nil, fmt.Errorf("duplicate artifact relpath %q", relpath)
		}
		seen[relpath] = struct{}{}
		entry.Relpath = relpath
		entry.SHA256 = digest
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Relpath < entries[j].Relpath })
	return entries, nil
}

func normalizeArtifactEvidence(artifact ArtifactEvidence) (string, string, error) {
	relpath := strings.TrimSpace(artifact.Relpath)
	cleaned := path.Clean(relpath)
	if artifact.Size < 0 || relpath == "" || cleaned != relpath || strings.HasPrefix(cleaned, "/") ||
		cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || !strings.HasPrefix(cleaned, "artifacts/") {
		return "", "", fmt.Errorf("invalid artifact relpath %q", artifact.Relpath)
	}
	parts := strings.Split(cleaned, "/")
	if len(parts) < 3 || parts[1] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("artifact relpath %q lacks a stage and file", artifact.Relpath)
	}
	if stage := strings.TrimSpace(artifact.Stage); stage != "" && stage != parts[1] {
		return "", "", fmt.Errorf("artifact stage %q does not match relpath %q", artifact.Stage, artifact.Relpath)
	}
	digest := strings.ToLower(strings.TrimSpace(artifact.SHA256))
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return "", "", fmt.Errorf("artifact %q has invalid sha256", relpath)
	}
	return cleaned, digest, nil
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
