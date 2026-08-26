package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
)

// Round-3 enforcement of the single-choke-point claim (#1635, PR #1637).
//
// Rounds 1-2 tried to DETECT direct config.OrgConfig.Roles() access and lost
// the syntactic-form enumeration game twice (a comment satisfied the round-1
// regex; a method value bypassed the round-2 CallExpr walk). Round 3 PREVENTS
// instead: the accessor is unexported (sortedRoles), so a plain call, method
// value, method expression, or reflection invocation from outside
// internal/config no longer compiles or is refused by reflect. The compiler
// covers the whole module — the scope the earlier walks approximated.
//
// What remains guardable by test is the ONE exported surface the seam needs:
// config.ResolveOrgRoster, whose nil-observation result is the unfiltered
// roster. TestOrgRosterSeamIsTheOnlyResolveCallSite censuses every reference
// to that IDENTIFIER: a function cannot be referenced without its name
// appearing as an identifier (import aliasing renames the package qualifier,
// not the function; dot-imports keep it; assigning it to a variable still
// writes it once), so this is object-level detection rather than call-form
// enumeration.
//
// STATED LIMITS (the #1626 disposition — hitting one of these later is the
// statement working, not a finding):
//   - reflection can look up ResolveOrgRoster by string and call it;
//   - a deliberate Roots()+Children() structural walk can rebuild the role
//     set (those accessors serve chart/escalation-path structure and stay);
//   - code inside internal/config can call sortedRoles() directly.
// All three are deliberate, review-visible acts. The accidental class this
// guard family exists for — a new consumer innocently enumerating roles and
// silently bypassing lifecycle exclusions — is a compile error.

// orgRosterResolveAllowedFiles are the only non-test production files that may
// reference ResolveOrgRoster: its definition and the cli seam.
var orgRosterResolveAllowedFiles = map[string]bool{
	"internal/config/org_roster.go": true,
	"internal/cli/org_roster.go":    true,
}

func TestOrgRosterSeamIsTheOnlyResolveCallSite(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", repoRoot, err)
	}
	scanned := 0
	seamReferences := 0
	var violations []string
	err := filepath.WalkDir(repoRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			// The module's Go source lives outside these; website/ and node
			// trees hold no production Go and slow the walk down.
			if name == ".git" || name == "node_modules" || name == "website" || name == "testdata" {
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
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		references := 0
		ast.Inspect(parsed, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok && ident.Name == "ResolveOrgRoster" {
				references++
			}
			return true
		})
		if references == 0 {
			return nil
		}
		if rel == "internal/cli/org_roster.go" {
			seamReferences = references
		}
		if !orgRosterResolveAllowedFiles[rel] {
			violations = append(violations, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fail-closed floor: a walk that scans almost nothing is a broken census,
	// not a clean tree (round-1 lesson, kept).
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test Go files; the census walk is broken, not the tree clean", scanned)
	}
	// Positive control: the seam's own reference must be seen. Its absence
	// means the census is broken or the subject moved — never a clean tree.
	if seamReferences == 0 {
		t.Fatal("census found no ResolveOrgRoster reference in internal/cli/org_roster.go; the guard's subject moved or the census is broken")
	}
	if len(violations) != 0 {
		t.Fatalf("ResolveOrgRoster referenced outside the roster seam: %v — consumers draw from loadOrgRoster (internal/cli/org_roster.go, #1635); a direct nil-observation resolve is the unfiltered-roster bypass", violations)
	}
}

// TestOrgConfigMethodSetIsPinned snapshots OrgConfig's exported method set.
// It is a change-forcing guard, not a bypass detector: re-exporting a bulk
// roles accessor, or adding a new one, must arrive together with an edit here
// that names why the single-choke-point policy (#1635) permits it.
func TestOrgConfigMethodSetIsPinned(t *testing.T) {
	want := []string{
		"Ancestors",
		"Children",
		"DirectiveAckTTL",
		"DirectiveDoneTTL",
		"DirectiveMaxNudges",
		"Enabled",
		"Enforce",
		"Path",
		"RecycleAfterFor",
		"RecycleEnforce",
		"Role",
		"Roots",
	}
	typ := reflect.TypeOf(config.OrgConfig{})
	got := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	// Fail-closed: an empty method set means the reflection read is broken or
	// the type moved, never that the policy is satisfied.
	if len(got) == 0 {
		t.Fatal("OrgConfig exports no methods; the pin is reading the wrong type")
	}
	if len(got) != len(want) {
		t.Fatalf("OrgConfig exported methods = %v, pinned %v — a new or re-exported accessor must update this pin alongside a stated reason it honors the #1635 single-choke-point policy", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("OrgConfig exported methods = %v, pinned %v — a new or re-exported accessor must update this pin alongside a stated reason it honors the #1635 single-choke-point policy", got, want)
		}
	}
}
