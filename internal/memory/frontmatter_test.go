package memory

import "testing"

// TestVaultNoteRoundTrip proves RenderVaultNote → ParseVaultNote → re-render is
// byte-identical, so `memory vault import` can recover the exact source record from
// an unedited note (the diff's stable baseline).
func TestVaultNoteRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		mem   VaultMemory
		links []VaultLink
	}{
		{
			name: "repo-scoped with links",
			mem: VaultMemory{
				ID: 42, OwnerKind: OwnerKindAgent, OwnerRef: "builder", OwnerVersion: "",
				Repo: "acme/widget", Scope: ScopeRepo, Key: "ci-flake",
				Content:    "arm64 CI is flaky under load and often needs a rerun",
				Provenance: "confirm", SourceJob: "job-ci-flake",
				CreatedAt: "2026-07-01T00:00:00Z", UpdatedAt: "2026-07-02T00:00:00Z",
			},
			links: []VaultLink{
				{TargetID: 7, Stem: VaultStem(7, "other"), Key: "other"},
				{TargetID: 3, Stem: VaultStem(3, "another"), Key: "another"},
			},
		},
		{
			name: "general scope no links",
			mem: VaultMemory{
				ID: 9, OwnerKind: OwnerKindAgent, OwnerRef: "reviewer",
				Repo: "", Scope: ScopeGeneral, Key: "toolchain-trap",
				Content:   "the default go toolchain is too old\nfor this module",
				CreatedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-01T00:00:00Z",
			},
			links: nil,
		},
		{
			name: "content without trailing newline",
			mem: VaultMemory{
				ID: 100, OwnerKind: OwnerKindAgent, OwnerRef: "builder",
				Repo: "acme/widget", Scope: ScopeRepo, Key: "no-newline",
				Content:   "a single line with no trailing newline",
				CreatedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-01T00:00:00Z",
			},
			links: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := RenderVaultNote(tc.mem, tc.links)
			parsed, err := ParseVaultNote(original)
			if err != nil {
				t.Fatalf("ParseVaultNote: %v", err)
			}
			id, ok := parsed.MemoryID()
			if !ok || id != tc.mem.ID {
				t.Fatalf("MemoryID = (%d, %v), want %d", id, ok, tc.mem.ID)
			}
			reconstructed := parsed.VaultMemory()
			roundTripped := RenderVaultNote(reconstructed, tc.links)
			if roundTripped != original {
				t.Fatalf("round-trip not byte-identical:\n--- original ---\n%q\n--- round-trip ---\n%q", original, roundTripped)
			}
		})
	}
}

// TestParseVaultNoteStripsLinks confirms the derived "## Links" section never leaks
// into the recovered body.
func TestParseVaultNoteStripsLinks(t *testing.T) {
	m := VaultMemory{
		ID: 1, OwnerKind: OwnerKindAgent, OwnerRef: "builder",
		Repo: "acme/widget", Scope: ScopeRepo, Key: "k", Content: "hello world",
		CreatedAt: "t", UpdatedAt: "t",
	}
	note := RenderVaultNote(m, []VaultLink{{TargetID: 2, Stem: "000000002-x", Key: "x"}})
	parsed, err := ParseVaultNote(note)
	if err != nil {
		t.Fatalf("ParseVaultNote: %v", err)
	}
	if got := parsed.Body; got != "hello world\n" {
		t.Fatalf("body = %q, want %q", got, "hello world\n")
	}
}

// TestParseVaultNoteRejectsNonNote rejects content with no frontmatter fence.
func TestParseVaultNoteRejectsNonNote(t *testing.T) {
	if _, err := ParseVaultNote("just some markdown, no frontmatter\n"); err == nil {
		t.Fatal("expected error for a note without frontmatter")
	}
}
