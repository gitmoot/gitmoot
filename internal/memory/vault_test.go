package memory

import (
	"strings"
	"testing"
)

func TestSlugIsLowercaseAndFilesystemSafe(t *testing.T) {
	cases := map[string]string{
		"CI Flake":             "ci-flake",
		"arm64/CI is flaky!":   "arm64-ci-is-flaky",
		"   spaced   out   ":   "spaced-out",
		"":                     "untitled",
		"!!!":                  "untitled",
		"Already-Slugged":      "already-slugged",
		"weird__under..scores": "weird-under-scores",
		"CI-FLAKE":             "ci-flake", // same slug as "CI Flake" — id prefix disambiguates
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVaultFilenameIDPrefixMakesCollisionsImpossible(t *testing.T) {
	// Two keys that slug identically must still get distinct filenames because
	// the zero-padded id prefix disambiguates.
	a := VaultFilename(12, "CI Flake")
	b := VaultFilename(13, "ci-flake")
	if a == b {
		t.Fatalf("expected distinct filenames, both = %q", a)
	}
	if a != "000000012-ci-flake.md" {
		t.Fatalf("filename = %q, want zero-padded id prefix", a)
	}
	if VaultStem(12, "CI Flake") != "000000012-ci-flake" {
		t.Fatalf("stem mismatch: %q", VaultStem(12, "CI Flake"))
	}
}

func TestRenderVaultNoteFrontmatterKeysSortedNoExportedAt(t *testing.T) {
	m := VaultMemory{
		ID: 7, OwnerKind: OwnerKindAgent, OwnerRef: "builder", Repo: "acme/widget",
		Scope: ScopeRepo, Key: "ci-flake", Content: "arm64 CI is flaky",
		Provenance: "distill", SourceJob: "job-1", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z",
	}
	got := RenderVaultNote(m, nil)
	if strings.Contains(got, "exported_at") {
		t.Fatalf("frontmatter must not contain exported_at:\n%s", got)
	}
	// Extract the frontmatter block and assert keys are in sorted order.
	parts := strings.SplitN(got, "---\n", 3)
	if len(parts) < 3 {
		t.Fatalf("malformed frontmatter fences:\n%s", got)
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(parts[1]), "\n") {
		keys = append(keys, strings.SplitN(line, ":", 2)[0])
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatalf("frontmatter keys not strictly sorted: %v", keys)
		}
	}
	// agent field present for an agent owner; the body carries the content.
	if !strings.Contains(parts[1], "agent: \"builder\"") {
		t.Errorf("missing agent field:\n%s", parts[1])
	}
	if !strings.Contains(got, "arm64 CI is flaky") {
		t.Errorf("body missing content:\n%s", got)
	}
	if !strings.Contains(got, "## Links") {
		t.Errorf("body missing Links section:\n%s", got)
	}
}

func TestRenderVaultNoteRoleOwnerHasNoAgentField(t *testing.T) {
	m := VaultMemory{ID: 1, OwnerKind: OwnerKindRole, OwnerRef: "reviewer", OwnerVersion: "v2",
		Scope: ScopeGeneral, Key: "k", Content: "c", UpdatedAt: "t"}
	got := RenderVaultNote(m, nil)
	if strings.Contains(got, "agent:") {
		t.Fatalf("role owner must not emit an agent field:\n%s", got)
	}
}

func TestRenderVaultNoteIncludesAuthorWhenPreserved(t *testing.T) {
	m := VaultMemory{
		ID: 2, OwnerKind: OwnerKindShared, OwnerRef: SharedOwnerRef, AuthorRef: "lead",
		Scope: ScopeRepo, Repo: "acme/widget", Key: "shared-fact", Content: "shared content",
		UpdatedAt: "t",
	}
	got := RenderVaultNote(m, nil)
	if !strings.Contains(got, "author: \"lead\"") {
		t.Fatalf("shared note must include preserved author frontmatter:\n%s", got)
	}
	if strings.Contains(got, "agent:") {
		t.Fatalf("shared owner must not emit agent frontmatter:\n%s", got)
	}
	parsed, err := ParseVaultNote(got)
	if err != nil {
		t.Fatalf("parse rendered note: %v", err)
	}
	if parsed.VaultMemory().AuthorRef != "lead" {
		t.Fatalf("parsed author = %q, want lead", parsed.VaultMemory().AuthorRef)
	}
}

func TestRenderVaultNoteLinksSortedByTargetID(t *testing.T) {
	links := []VaultLink{
		{TargetID: 30, Stem: "000000030-c", Key: "c"},
		{TargetID: 10, Stem: "000000010-a", Key: "a"},
		{TargetID: 20, Stem: "000000020-b", Key: "b"},
	}
	got := RenderVaultNote(VaultMemory{ID: 1, OwnerKind: OwnerKindAgent, OwnerRef: "x", Key: "k", Content: "c", UpdatedAt: "t"}, links)
	ia := strings.Index(got, "000000010-a")
	ib := strings.Index(got, "000000020-b")
	ic := strings.Index(got, "000000030-c")
	if !(ia < ib && ib < ic) {
		t.Fatalf("links not sorted by target id:\n%s", got)
	}
}

func TestRenderVaultNoteDeterministic(t *testing.T) {
	m := VaultMemory{ID: 7, OwnerKind: OwnerKindAgent, OwnerRef: "builder", Repo: "acme/widget",
		Scope: ScopeRepo, Key: "ci-flake", Content: "arm64 CI is flaky", UpdatedAt: "t"}
	links := []VaultLink{{TargetID: 9, Stem: "000000009-x", Key: "x"}}
	if RenderVaultNote(m, links) != RenderVaultNote(m, links) {
		t.Fatal("RenderVaultNote is not deterministic")
	}
}

func TestVaultSnapshotHashOrderIndependentAndEmpty(t *testing.T) {
	a := []VaultFileDigest{{ID: 2, UpdatedAt: "t2", Sum: "bb"}, {ID: 1, UpdatedAt: "t1", Sum: "aa"}}
	b := []VaultFileDigest{{ID: 1, UpdatedAt: "t1", Sum: "aa"}, {ID: 2, UpdatedAt: "t2", Sum: "bb"}}
	if VaultSnapshotHash(a) != VaultSnapshotHash(b) {
		t.Fatal("snapshot hash must be independent of input order (sorts by id)")
	}
	// Empty vault has a stable, well-defined hash (sha256 of the empty sequence).
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := VaultSnapshotHash(nil); got != emptySHA {
		t.Fatalf("empty snapshot hash = %q, want %q", got, emptySHA)
	}
	// A content change flips the hash.
	c := []VaultFileDigest{{ID: 1, UpdatedAt: "t1", Sum: "changed"}}
	if VaultSnapshotHash(c) == VaultSnapshotHash(b[:1]) {
		t.Fatal("snapshot hash must change when a file digest changes")
	}
}

func TestRenderVaultIndexGroupsAndSorts(t *testing.T) {
	owner := VaultOwnerKey{Kind: OwnerKindAgent, Ref: "builder"}
	mems := []VaultMemory{
		{ID: 3, Scope: ScopeRepo, Repo: "z/repo", Key: "zeta"},
		{ID: 1, Scope: ScopeGeneral, Key: "gamma"},
		{ID: 2, Scope: ScopeRepo, Repo: "a/repo", Key: "alpha"},
		{ID: 4, Scope: ScopeGeneral, Key: "beta"},
	}
	got := RenderVaultIndex(owner, mems)
	// General section first, repos sorted after.
	iGeneral := strings.Index(got, "## General")
	iA := strings.Index(got, "## a/repo")
	iZ := strings.Index(got, "## z/repo")
	if !(iGeneral >= 0 && iGeneral < iA && iA < iZ) {
		t.Fatalf("index sections out of order:\n%s", got)
	}
	// Within General, beta sorts before gamma.
	if strings.Index(got, "|beta]]") > strings.Index(got, "|gamma]]") {
		t.Fatalf("general keys not sorted:\n%s", got)
	}
	if RenderVaultIndex(owner, mems) != got {
		t.Fatal("RenderVaultIndex is not deterministic")
	}
}

func TestVaultIndexFilenameDisambiguatesOwners(t *testing.T) {
	agent := VaultIndexFilename(VaultOwnerKey{Kind: OwnerKindAgent, Ref: "builder"})
	role := VaultIndexFilename(VaultOwnerKey{Kind: OwnerKindRole, Ref: "builder", Version: "v2"})
	if agent == role {
		t.Fatalf("owner index filenames must not collide: %q", agent)
	}
	if !strings.HasPrefix(agent, "_index-") {
		t.Fatalf("index filename should sort ahead of notes: %q", agent)
	}
}

// Slugging is lossy: separator-only and case-only differences in a ref collapse
// to the same slug. The raw-tuple hash suffix must keep such owners' index files
// distinct so one index can never silently overwrite another.
func TestVaultIndexFilenameNoSlugCollision(t *testing.T) {
	cases := []struct{ a, b VaultOwnerKey }{
		// '_' and '-' both slug to '-'.
		{
			VaultOwnerKey{Kind: OwnerKindAgent, Ref: "build_bot"},
			VaultOwnerKey{Kind: OwnerKindAgent, Ref: "build-bot"},
		},
		// Case-only difference (collides on case-insensitive filesystems).
		{
			VaultOwnerKey{Kind: OwnerKindAgent, Ref: "Builder"},
			VaultOwnerKey{Kind: OwnerKindAgent, Ref: "builder"},
		},
		// Version differing only by separator/case.
		{
			VaultOwnerKey{Kind: OwnerKindRole, Ref: "rev", Version: "v_1"},
			VaultOwnerKey{Kind: OwnerKindRole, Ref: "rev", Version: "v-1"},
		},
	}
	for _, c := range cases {
		fa := VaultIndexFilename(c.a)
		fb := VaultIndexFilename(c.b)
		if fa == fb {
			t.Fatalf("owner index filenames collide: %+v and %+v both -> %q", c.a, c.b, fa)
		}
	}
	// The filename stays deterministic for a fixed owner.
	o := VaultOwnerKey{Kind: OwnerKindAgent, Ref: "builder", Version: "v2"}
	if VaultIndexFilename(o) != VaultIndexFilename(o) {
		t.Fatal("VaultIndexFilename is not deterministic")
	}
}
