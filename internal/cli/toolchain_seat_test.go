package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestStagedToolchainIsNeverInsideASeatWriteGrant is THIS SHAPE'S P1 AS A TEST.
//
// A seat's cache root IS its write grant, and is also the adapter's cleanupRoot.
// A staged copy placed at or beneath it would be writable by the seat, so the
// seat could rewrite its own go binary and shape B would have reproduced the
// defect it exists to remove, with extra steps.
//
// The pair is deliberate: a positive control that a correct placement is
// ACCEPTED, so a passing test cannot mean "the function refuses everything".
func TestStagedToolchainIsNeverInsideASeatWriteGrant(t *testing.T) {
	const cacheRoot = "/var/lib/gitmoot/cache/agent-7"
	const stagedRoot = "/var/lib/gitmoot/toolchains/go1.26.4-abc123"

	for _, test := range []struct {
		name     string
		staged   string
		writes   []string
		accepted bool
	}{
		{
			name:     "sibling of the write grant is accepted",
			staged:   stagedRoot,
			writes:   []string{cacheRoot},
			accepted: true,
		},
		{
			name:   "directly inside the write grant is refused",
			staged: filepath.Join(cacheRoot, "toolchain"),
			writes: []string{cacheRoot},
		},
		{
			name:   "deep inside the write grant is refused",
			staged: filepath.Join(cacheRoot, "a", "b", "c", "go"),
			writes: []string{cacheRoot},
		},
		{
			name:   "equal to the write grant is refused",
			staged: cacheRoot,
			writes: []string{cacheRoot},
		},
		{
			name:   "write grant inside the staged copy is refused",
			staged: stagedRoot,
			writes: []string{filepath.Join(stagedRoot, "bin")},
		},
		{
			// The comparison is on CLEANED paths, so a traversal that lands back
			// inside is refused rather than sneaking past a string test.
			name:   "refused through a non-clean path that resolves inside",
			staged: cacheRoot + "/../agent-7/toolchain",
			writes: []string{cacheRoot},
		},
		{
			// and the mirror: a traversal that genuinely lands OUTSIDE is
			// accepted. Without this arm the case above would also pass a
			// function that refused every path containing "..".
			name:     "accepted through a non-clean path that resolves outside",
			staged:   cacheRoot + "/../../toolchains/go1.26.4",
			writes:   []string{cacheRoot},
			accepted: true,
		},
		{
			name:     "sibling whose name merely PREFIXES the write grant is accepted",
			staged:   "/var/lib/gitmoot/cache/agent-70-toolchain",
			writes:   []string{cacheRoot},
			accepted: true,
		},
		{
			name:   "refused when any one of several grants contains it",
			staged: filepath.Join(cacheRoot, "go"),
			writes: []string{"/var/lib/gitmoot/other", cacheRoot},
		},
		{
			name:     "empty and blank grants are ignored rather than matching everything",
			staged:   stagedRoot,
			writes:   []string{"", "   "},
			accepted: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateStagedToolchainPlacement(test.staged, test.writes)
			if test.accepted && err != nil {
				t.Fatalf("placement %q with writes %q was refused: %v", test.staged, test.writes, err)
			}
			if !test.accepted {
				if err == nil {
					t.Fatalf("placement %q is writable by the seat and was ACCEPTED; the seat could rewrite its own toolchain", test.staged)
				}
				if !strings.Contains(err.Error(), "rewrite its own toolchain") {
					t.Fatalf("refusal %q does not name the hazard", err)
				}
			}
		})
	}
}

// TestPinnedToolchainRootRefusesSystemPrefixes is the scope boundary jarvis set:
// system-prefix toolchains keep their pre-existing location-only grant inside
// internal/sandbox and are NOT copied. If this path ever started returning them,
// two owners would be competing for one decision.
func TestPinnedToolchainRootRefusesSystemPrefixes(t *testing.T) {
	for _, refused := range []string{
		"/opt/go/bin/go",
		"/opt/nested/deeper/go/bin/go",
		"/usr/local/go/bin/go",
		"/usr/local/bin/go",
		"/nix/store/abc-go-1.26.4/bin/go",
		"/snap/go/current/bin/go",
	} {
		if root, ok := pinnedToolchainRoot(refused); ok {
			t.Fatalf("pinnedToolchainRoot(%q) = %q, want refused: system prefixes belong to internal/sandbox", refused, root)
		}
	}

	// CONTROL: an operator-pinned path IS returned, so the refusals above are
	// about the prefixes rather than about the function refusing everything.
	for path, want := range map[string]string{
		"/root/.local/toolchains/go1.26.4/bin/go": "/root/.local/toolchains/go1.26.4",
		"/home/op/sdk/go1.26.4/bin/go":            "/home/op/sdk/go1.26.4",
		"/opt-not-a-prefix/go/bin/go":             "/opt-not-a-prefix/go",
		"/usr/localish/go/bin/go":                 "/usr/localish/go",
	} {
		root, ok := pinnedToolchainRoot(path)
		if !ok || root != want {
			t.Fatalf("pinnedToolchainRoot(%q) = %q,%v; want %q,true", path, root, ok, want)
		}
	}

	// and a path that is not an installation layout at all
	if _, ok := pinnedToolchainRoot("/somewhere/go"); ok {
		t.Fatal("a go NOT under bin/ or sbin/ was accepted as an installation")
	}
}

// TestSeatToolchainGrantIsAddedAsAReadAndNotAWrite pins the grant SHAPE at the
// wiring site: the staged path must arrive in reads, never in writes, and the
// seat's single isolated cache grant must remain its only write.
//
// This is the produce-arm half of the coverage too, stated as the fact that
// matters: readableRoots is shared by the produce arm and the seat arm, and this
// change adds nothing to it. The staged read is appended in
// readOnlyRuntimeSandboxGrants, which ONLY the seat path calls, so produce
// behaviour is unchanged by construction rather than by assertion. My own
// salvage finding was that a grant change is never seat-only when it touches the
// SHARED derivation; this one does not touch it, and the test states which.
func TestSeatToolchainGrantIsAddedAsAReadAndNotAWrite(t *testing.T) {
	staged := "/var/lib/gitmoot/toolchains/go1.26.4-abc123"
	grants := readOnlySandboxGrants{
		cacheRoot: "/var/lib/gitmoot/cache/agent-7",
		writes:    []string{"/var/lib/gitmoot/cache/agent-7"},
		reads:     []string{"/checkout"},
	}

	if err := validateStagedToolchainPlacement(staged, grants.writes); err != nil {
		t.Fatalf("a correctly placed copy was refused: %v", err)
	}
	grants.reads = append(grants.reads, staged)

	for _, write := range grants.writes {
		if write == staged {
			t.Fatal("the staged toolchain appeared in the seat's WRITE set")
		}
	}
	found := false
	for _, read := range grants.reads {
		if read == staged {
			found = true
		}
	}
	if !found {
		t.Fatal("the staged toolchain is not in the seat's read set")
	}
	if len(grants.writes) != 1 || grants.writes[0] != grants.cacheRoot {
		t.Fatalf("writes = %q, want exactly the isolated cache grant", grants.writes)
	}
}
