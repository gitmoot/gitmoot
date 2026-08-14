package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestRuntimeJobPlanFieldsHaveSingleGatedProducer pins the source-level reason
// the deliver-time plan gate is complete: Mailbox.deliver is the only production
// code that may construct a runtime.Job carrying Plan or PlanInto, and the gate is
// immediately above that literal. A new adapter entry path must deliberately join
// this gate instead of silently creating a second door.
func TestRuntimeJobPlanFieldsHaveSingleGatedProducer(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", ".."))
	want := []string{"internal/workflow/mailbox.go::Mailbox.deliver"}
	var got []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if shouldSkipPlanProducerDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		pkgPath := planFieldPackagePath(rel)
		imports := planFieldImportPaths(parsed)
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CompositeLit:
					// FAIL-CLOSED. This used to ask "is the literal's type runtime.Job?"
					// and return false when it could not tell — so every type spelling
					// the matcher did not recognise was silently exempt. Three review
					// rounds walked past it that way, most recently with
					// `type X = runtime.Job` (a plain refactor idiom, not an exotic act):
					// the matcher compares the type NAME syntactically, so an alias under
					// any other name was invisible and hasPlanField was never consulted.
					//
					// The question is inverted: ANY composite literal that sets Plan or
					// PlanInto is reported unless its type is on an explicit allowlist of
					// types that legitimately carry those fields. A new type, a rename, or
					// an alias now defaults to REPORTED. That is noisier and it is the
					// safe direction: the failure mode becomes "the census names something
					// you must add to the allowlist", not "the census went quiet".
					if hasPlanField(n) && !planFieldTypeIsAllowlisted(n.Type, pkgPath, imports) {
						got = append(got, filepath.ToSlash(rel)+"::"+functionSymbol(function))
					}
				case *ast.AssignStmt:
					// job.Plan = true — the composite-literal scan alone missed this, and
					// a compiled mutant that built a runtime.Job then set Plan AFTERWARDS
					// delivered it straight past the mailbox gate while this test still
					// passed. A seam guard that recognises only one syntax for writing a
					// field is not a seam guard.
					if assignsPlanField(n) {
						got = append(got, filepath.ToSlash(rel)+"::"+functionSymbol(function))
					}
				case *ast.TypeSpec:
					// A function-local `type JobPayload = runtime.Job` SHADOWS the real
					// type, so a bare literal under that name satisfies the owning-package
					// test above and would be exempt while producing a genuine
					// runtime.Job. Declaring an allowlisted name anywhere inside a body is
					// therefore reported: the allowlist grants exemption to five specific
					// types, not to their spellings.
					if declaresAllowlistedName(n) {
						got = append(got, filepath.ToSlash(rel)+"::"+functionSymbol(function))
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan runtime.Job plan producers: %v", err)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("runtime.Job plan producers = %v, want exactly %v; route every producer through the deliver-time plan gate", got, want)
	}
}

func shouldSkipPlanProducerDir(rel string) bool {
	if rel == "." {
		return false
	}
	base := filepath.Base(rel)
	return base == ".git" || base == ".gitmoot" || base == "node_modules" || base == "build" || base == "dist" || base == "repos" || base == "GOALS"
}

func hasPlanField(literal *ast.CompositeLit) bool {
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		name, ok := field.Key.(*ast.Ident)
		if ok && (name.Name == "Plan" || name.Name == "PlanInto") {
			return true
		}
	}
	return false
}

func functionSymbol(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return function.Name.Name
	}
	receiver := function.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	if name, ok := receiver.(*ast.Ident); ok {
		return name.Name + "." + function.Name.Name
	}
	return function.Name.Name
}

// assignsPlanField reports whether a statement writes Plan or PlanInto through a
// selector, e.g. `job.Plan = true` or `delivery.PlanInto = x`. The composite-literal
// scan alone left this syntax invisible, so a producer could construct a
// runtime.Job, set the plan primitives on the next line, and never appear in the
// seam census the test exists to pin.
//
// The LHS is UNWRAPPED before matching. A first version compared the bare node to
// *ast.SelectorExpr, and a reviewer walked straight past it with
// `*(&delivery.Plan) = true` — an ast.StarExpr wrapping a unary & of the selector,
// which reaches the same field and looked like nothing. Recognising one spelling of
// a write is exactly the defect this helper was added to fix, so it must not
// reproduce it one level down. Deref, address-of and parens are all peeled.
func assignsPlanField(assign *ast.AssignStmt) bool {
	for _, lhs := range assign.Lhs {
		if selectorNamesPlanField(lhs) {
			return true
		}
	}
	return false
}

// selectorNamesPlanField peels *, & and ( ) wrappers off an expression and reports
// whether what remains selects Plan or PlanInto.
func selectorNamesPlanField(expr ast.Expr) bool {
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.X
		case *ast.StarExpr:
			expr = e.X
		case *ast.UnaryExpr:
			if e.Op != token.AND {
				return false
			}
			expr = e.X
		case *ast.SelectorExpr:
			return e.Sel != nil && (e.Sel.Name == "Plan" || e.Sel.Name == "PlanInto")
		default:
			return false
		}
	}
}

// TestAssignsPlanFieldArmsTheCensus makes the assignment detector's RESULT
// load-bearing. The census test alone did not: mutations making assignsPlanField
// always return false, or dropping PlanInto recognition, both left it green,
// because no fixture in the tree exercised the assignment path. A detector nobody
// tests is a detector that silently stops detecting — which is precisely how the
// composite-literal-only version survived until a reviewer mutated production code
// to walk past it.
//
// Each case is parsed from source, so the fixtures are the real syntax a producer
// would write rather than hand-built AST nodes that could drift from the parser.
func TestAssignsPlanFieldArmsTheCensus(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"job.Plan = true", true},
		{"job.PlanInto = x", true},
		{"delivery.Plan, delivery.PlanInto = a, b", true},
		{"p.Plan = true", true},
		// The forms a reviewer used to bypass the first version.
		{"*(&delivery.Plan) = true", true},
		{"*pj.PlanInto = s", true},
		{"(delivery.Plan) = true", true},
		{"*(&(delivery.PlanInto)) = s", true},
		// Must NOT fire: unrelated fields, and a plan-shaped name that is not a field write.
		{"job.Model = m", false},
		{"job.Prompt = p", false},
		{"Plan = true", false},
		{"m[\"Plan\"] = true", false},
		{"job.PlanModeSomething = x", false},
	}
	for _, tc := range cases {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\nfunc f() {\n"+tc.src+"\n}\n", 0)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.src, err)
		}
		var got bool
		ast.Inspect(file, func(n ast.Node) bool {
			if assign, ok := n.(*ast.AssignStmt); ok && assignsPlanField(assign) {
				got = true
			}
			return true
		})
		if got != tc.want {
			t.Fatalf("assignsPlanField(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// planFieldAllowlist names the types that legitimately carry Plan/PlanInto and are
// NOT a delivered runtime.Job: the payload and request shapes the mailbox itself
// owns, plus the runtime's own request struct. Everything else that sets those
// fields is reported by the census.
//
// This is an ALLOWLIST on purpose. The previous design asked "is this a
// runtime.Job?" and exempted everything it could not identify, which is fail-open:
// three review rounds bypassed it with a spelling it did not know, most recently a
// type alias. Inverting the default means a new type, a rename or an alias shows up
// as a census failure naming itself — a maintainer then decides whether it belongs
// here. Adding a name to this list is a deliberate act with a diff; being invisible
// to the census was not.
// planFieldAllowlist maps a type to the IMPORT PATH of the package that owns it.
// Keying on the bare type name was fail-open (`other.JobPayload{Plan:true}` was
// exempt); keying on the qualifier's LOCAL name was still fail-open, because an
// import alias is an arbitrary local name a producer chooses for itself:
//
//	import workflow "github.com/gitmoot/gitmoot/internal/censusshim" // type JobPayload = runtime.Job
//	_ = workflow.JobPayload{Plan: true}                              // exempt, and a real producer
//
// A reviewer built exactly that as compiled code in the tree and the census stayed
// green. The remedy needs no type resolution: parsed.Imports already maps every
// qualifier to its import PATH, so identity is available for the asking. A
// qualifier that cannot be resolved to a path is NOT allowlisted, so an unusual
// import form costs a census failure rather than an exemption.
var planFieldAllowlist = map[string]string{
	"JobPayload":             "github.com/gitmoot/gitmoot/internal/workflow", // mailbox.go — the persisted payload
	"JobRequest":             "github.com/gitmoot/gitmoot/internal/workflow", // mailbox.go — the enqueue request
	"RuntimeContractRequest": "github.com/gitmoot/gitmoot/internal/runtime",  // preflight.go — preflight scoping
	// Surfaced BY the inversion, and exactly the decision it exists to force: both
	// have a field literally named Plan that has nothing to do with plan mode.
	// groomApplyResult.Plan is a plan-file PATH (string); DelegationTimeoutDefaults.Plan
	// is a DURATION. The pre-inversion matcher never saw either.
	"groomApplyResult":          "github.com/gitmoot/gitmoot/internal/cli",      // memory_groom.go — Plan is a file path
	"DelegationTimeoutDefaults": "github.com/gitmoot/gitmoot/internal/workflow", // Plan is a timeout duration
}

// planFieldModulePath is this module's path, so a file's own package identity can be
// derived from its position in the tree without loading type information.
const planFieldModulePath = "github.com/gitmoot/gitmoot"

// planFieldPackagePath returns the import path of the package containing a file at
// repo-relative path rel.
func planFieldPackagePath(rel string) string {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." {
		return planFieldModulePath
	}
	return planFieldModulePath + "/" + dir
}

// planFieldImportPaths maps each qualifier a file can write to the import path it
// resolves to. A dot-import has no qualifier and is deliberately absent, so a bare
// name arriving that way is checked against the file's OWN package and reported. A
// named import uses its alias; otherwise the path's last element is the qualifier,
// which is wrong only when a package name differs from its directory — and that
// case fails CLOSED, because the qualifier then resolves to nothing.
func planFieldImportPaths(parsed *ast.File) map[string]string {
	paths := make(map[string]string, len(parsed.Imports))
	for _, spec := range parsed.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		} else if index := strings.LastIndex(path, "/"); index >= 0 {
			name = path[index+1:]
		} else {
			name = path
		}
		// No skip for "." or "_": neither is a legal selector qualifier, so a dot- or
		// blank-import entry can never be looked up. A guard for it was untestable by
		// construction — the same dead-branch shape as the deleted StarExpr case.
		paths[name] = path
	}
	return paths
}

// declaresAllowlistedName reports whether a type declaration borrows an allowlisted
// name. A function-local `type JobPayload = runtime.Job` shadows the real type, so a
// bare literal under that name passes the owning-package test while producing a
// genuine runtime.Job. The exemption belongs to five specific types, not to their
// spellings, so any body-scoped declaration of one of those names is reported.
func declaresAllowlistedName(spec *ast.TypeSpec) bool {
	if spec == nil || spec.Name == nil {
		return false
	}
	_, listed := planFieldAllowlist[spec.Name.Name]
	return listed
}

// planFieldTypeIsAllowlisted reports whether a composite literal's type is one of
// the shapes allowed to carry plan fields without being a census producer. An
// unrecognised or unnamed type is NOT allowlisted, so it is reported. pkgPath is the
// import path of the file's own package and imports maps its qualifiers to paths.
func planFieldTypeIsAllowlisted(expr ast.Expr, pkgPath string, imports map[string]string) bool {
	switch typed := expr.(type) {
	case *ast.SelectorExpr:
		// pkg.Type{...}. The qualifier is a LOCAL name the producer chooses, so it is
		// resolved through the file's imports to a package PATH before comparison. An
		// alias that borrows the owner's name (`import workflow ".../censusshim"`)
		// therefore buys nothing, and a qualifier absent from the import table — a
		// package whose name differs from its directory, say — resolves to "" and is
		// reported rather than assumed.
		qualifier, ok := typed.X.(*ast.Ident)
		if !ok {
			return false
		}
		owner, listed := planFieldAllowlist[typed.Sel.Name]
		return listed && imports[qualifier.Name] == owner
	case *ast.Ident:
		// Bare Type{...} — only exempt when the FILE's own package owns the name.
		owner, listed := planFieldAllowlist[typed.Name]
		return listed && pkgPath == owner
	// No *ast.StarExpr or *ast.ParenExpr case: Go has no syntax that puts either in
	// CompositeLit.Type. '&runtime.Job{...}' parses the & OUTSIDE the literal, so the
	// type is still a SelectorExpr, and element literals inside a slice or map carry
	// ArrayType, MapType or a nil type. Both branches existed here and were dead:
	// mutating them to return true changed no test, not because they were untested
	// but because nothing can reach them. Verified with a parser probe over
	// &runtime.Job{...}, []*runtime.Job{{...}} and map[string]*runtime.Job{...}.
	// Dead code in a fail-closed predicate is worse than absent code — it invites
	// the reader to believe a case is handled.
	default:
		// Unnamed, generic-instantiated, array/map element literals and anything else
		// the walk cannot name: NOT allowlisted. Fail closed.
		return false
	}
}

// TestPlanFieldCensusFailsClosedOnUnknownTypes proves the inversion. Each fixture is
// parsed from source and run through the same predicates the census uses, so a
// spelling that bypasses the census must also bypass this test — which is the
// property the three earlier bypasses all lacked.
func TestPlanFieldCensusFailsClosedOnUnknownTypes(t *testing.T) {
	const defaultImports = "import (\n\t\"github.com/gitmoot/gitmoot/internal/runtime\"\n\tworkflow \"github.com/gitmoot/gitmoot/internal/workflow\"\n)\n"
	cases := []struct {
		name    string
		imports string // when empty, defaultImports; a case owns its import block so the
		pkg     string // qualifier resolution under test is the file's own, not a fixed map;
		src     string // pkg is the file's repo-relative path, defaulting to the owning one
		want    bool   // want == reported by the census
	}{
		{"qualified runtime.Job", "", "", "_ = runtime.Job{Plan: true}", true},
		{"TYPE ALIAS — the round-5 bypass", "", "", "_ = censusJobAlias{Plan: true, PlanInto: \"@smol\"}", true},
		{"alias of an alias", "", "", "_ = deeperAlias{PlanInto: \"x\"}", true},
		{"bare Ident under dot-import", "", "", "_ = Job{Plan: true}", true},
		{"pointer literal", "", "", "_ = &runtime.Job{Plan: true}", true},
		{"unknown local type", "", "", "_ = someShim{Plan: true}", true},
		{"allowlisted payload", "", "", "_ = JobPayload{Plan: true}", false},
		{"allowlisted request", "", "", "_ = JobRequest{PlanInto: \"x\"}", false},
		{"allowlisted qualified", "", "", "_ = runtime.RuntimeContractRequest{Plan: true}", false},
		{"no plan field at all", "", "", "_ = runtime.Job{Prompt: \"x\"}", false},
		// SAME-NAME IMPOSTOR CONTROLS. Keying the allowlist on the bare type name was
		// fail-open: a borrowed name from any other package bought an exemption, which
		// recreated the type-alias bypass under five allowlisted names. The qualifier
		// is now part of the identity, so each of these must be REPORTED.
		{"impostor: foreign JobPayload", "", "", "_ = other.JobPayload{Plan: true}", true},
		{"impostor: foreign JobRequest", "", "", "_ = shim.JobRequest{PlanInto: \"x\"}", true},
		{"impostor: foreign RuntimeContractRequest", "", "", "_ = fake.RuntimeContractRequest{Plan: true}", true},
		{"impostor: foreign groomApplyResult", "", "", "_ = other.groomApplyResult{Plan: true}", true},
		{"impostor: bare allowlisted name in the WRONG package", "", "", "_ = RuntimeContractRequest{Plan: true}", true},
		{"correct qualifier still exempt", "", "", "_ = workflow.JobPayload{Plan: true}", false},
		// DEFAULT-BRANCH CONTROLS. A reviewer showed that making the default branch
		// return true survived the earlier fixtures, because none of them reached it:
		// every case named a type. These do, so the fail-closed default is now armed.
		{"generic instantiation", "", "", "_ = Wrapper[runtime.Job]{Plan: true}", true},
		{"map element literal", "", "", "_ = map[string]runtime.Job{\"k\": {Plan: true}}", true},
		{"slice element literal", "", "", "_ = []runtime.Job{{PlanInto: \"x\"}}", true},
		// IMPORT-ALIAS IDENTITY. Comparing the qualifier's LOCAL name was still
		// fail-open: the alias is chosen by the producer, so borrowing the owner's name
		// bought the exemption back. A reviewer proved it with compiled code in the
		// tree. Identity is the import PATH, and these three fix the meaning of that.
		{
			name:    "alias BORROWING the owner's name is reported",
			imports: "import workflow \"github.com/gitmoot/gitmoot/internal/censusshim\"\n",
			pkg:     "",
			src:     "_ = workflow.JobPayload{Plan: true}",
			want:    true,
		},
		{
			name:    "legitimate differing alias for the real owner stays exempt",
			imports: "import wf \"github.com/gitmoot/gitmoot/internal/workflow\"\n",
			pkg:     "",
			src:     "_ = wf.JobPayload{Plan: true}",
			want:    false,
		},
		{
			name:    "dot-import into a NON-owning package is reported",
			imports: "import . \"github.com/gitmoot/gitmoot/internal/workflow\"\n",
			pkg:     "internal/cli/x.go",
			src:     "_ = JobPayload{Plan: true}",
			want:    true,
		},
		{
			name:    "qualifier absent from the import table is reported",
			imports: "import \"github.com/gitmoot/gitmoot/internal/runtime\"\n",
			pkg:     "",
			src:     "_ = workflow.JobPayload{Plan: true}",
			want:    true,
		},
	}
	for _, tc := range cases {
		imports := tc.imports
		if imports == "" {
			imports = defaultImports
		}
		pkg := tc.pkg
		if pkg == "" {
			pkg = "internal/workflow/x.go"
		}
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package workflow\n"+imports+"func f() {\n"+tc.src+"\n}\n", 0)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		// The predicate runs against the fixture's OWN resolved imports, so a fixture
		// cannot pass by borrowing a map the production walk would never build.
		resolved := planFieldImportPaths(file)
		var reported bool
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if ok && hasPlanField(lit) && !planFieldTypeIsAllowlisted(lit.Type, planFieldPackagePath(pkg), resolved) {
				reported = true
			}
			return true
		})
		if reported != tc.want {
			t.Fatalf("%s: reported=%v, want %v (src: %s)", tc.name, reported, tc.want, tc.src)
		}
	}
}

// TestDeclaresAllowlistedNameArmsTheShadowBranch pins the local-shadow arm of the
// census. Without a fixture the branch is an assertion about a defect nobody
// reproduced; with one, deleting it fails a test rather than going quiet.
func TestDeclaresAllowlistedNameArmsTheShadowBranch(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"local alias shadowing an allowlisted name", "type JobPayload = runtime.Job", true},
		{"local alias of a second allowlisted name", "type JobRequest = runtime.Job", true},
		{"local definition, not an alias, still borrows the name", "type groomApplyResult struct{ Plan bool }", true},
		{"unrelated local type", "type somethingElse = runtime.Job", false},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package workflow\nfunc f() {\n"+tc.src+"\n_ = 0\n}\n", 0)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		var got bool
		ast.Inspect(file, func(n ast.Node) bool {
			if spec, ok := n.(*ast.TypeSpec); ok && declaresAllowlistedName(spec) {
				got = true
			}
			return true
		})
		if got != tc.want {
			t.Fatalf("%s: declaresAllowlistedName = %v, want %v", tc.name, got, tc.want)
		}
	}
}
