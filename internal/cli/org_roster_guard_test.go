package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// orgRosterAllowedRolesCallSites is the exhaustive allow-list of non-test
// production files that may call config.OrgConfig.Roles() directly. The roster
// seam (#1635) exists so that seat-lifecycle exclusions (archived, paused) are
// applied in exactly ONE place; a new direct call site is a roster consumer
// that would silently bypass those exclusions — the N-filters drift this repo
// has already paid for (#1283's five-copies-of-the-rule freeze).
//
// If this test failed on your change: route the new consumer through
// resolveOrgRoster / loadOrgRoster (internal/cli/org_roster.go) and pick the
// view it needs — Members() (chart/health/presence/dispatch/routing) or
// Nudgeable() (sweeps/nudges/alarms/automated wakes). Only extend this list
// for the seam itself.
var orgRosterAllowedRolesCallSites = map[string]bool{
	"internal/cli/org_roster.go": true,
}

// TestOrgRosterSeamIsTheOnlyRolesCallSite scans every non-test Go file under
// internal/ and cmd/ for `.Roles()` calls and requires each hit to be in the
// allow-list above. The match is textual by design: a false positive from some
// future unrelated Roles() method fails LOUDLY here and gets resolved by a
// deliberate allow-list edit, which is exactly the review conversation the
// seam wants to force.
func TestOrgRosterSeamIsTheOnlyRolesCallSite(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", repoRoot, err)
	}
	pattern := regexp.MustCompile(`\.Roles\(\)`)
	scanned := 0
	var violations []string
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, top), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			if !pattern.Match(content) {
				return nil
			}
			if !orgRosterAllowedRolesCallSites[rel] {
				violations = append(violations, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A broken walk that scans nothing would pass vacuously; require the scan
	// to have actually covered the tree, including the one allowed file.
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test Go files; the walk is broken, not the tree clean", scanned)
	}
	seam := filepath.Join(repoRoot, "internal", "cli", "org_roster.go")
	if content, err := os.ReadFile(seam); err != nil || !pattern.Match(content) {
		t.Fatalf("seam file %s missing or no longer calls .Roles() (err=%v); the guard's subject moved", seam, err)
	}
	if len(violations) != 0 {
		t.Fatalf("direct config.OrgConfig.Roles() call sites outside the roster seam: %v — route them through resolveOrgRoster/loadOrgRoster (internal/cli/org_roster.go, #1635)", violations)
	}
}
