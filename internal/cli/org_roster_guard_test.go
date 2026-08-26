package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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

// TestOrgRosterSeamIsTheOnlyRolesCallSite parses every non-test Go file in the
// repository for executable `.Roles()` calls and requires each hit to be in the
// allow-list above. This is intentionally syntax-based: comments documenting
// the seam must not make an implementation with no real Roles call pass.
func TestOrgRosterSeamIsTheOnlyRolesCallSite(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", repoRoot, err)
	}
	fset := token.NewFileSet()
	scanned := 0
	scannedOutsideLegacyRoots := 0
	seamCalls := 0
	var violations []string
	err := filepath.WalkDir(repoRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		if !strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "cmd/") {
			scannedOutsideLegacyRoots++
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Roles" {
				return true
			}
			position := fset.Position(selector.Sel.Pos())
			if orgRosterAllowedRolesCallSites[rel] {
				seamCalls++
			} else {
				violations = append(violations, position.String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A broken walk that scans nothing would pass vacuously; require the scan
	// to have actually covered the tree, including the one allowed file.
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test Go files; the walk is broken, not the tree clean", scanned)
	}
	if scannedOutsideLegacyRoots == 0 {
		t.Fatal("scanned no non-test Go files outside internal/ and cmd/; repository-wide guard scope regressed")
	}
	if seamCalls == 0 {
		t.Fatal("internal/cli/org_roster.go no longer contains an executable .Roles() call; the guard's subject moved")
	}
	if len(violations) != 0 {
		t.Fatalf("direct config.OrgConfig.Roles() call sites outside the roster seam: %v — route them through resolveOrgRoster/loadOrgRoster (internal/cli/org_roster.go, #1635)", violations)
	}
}
