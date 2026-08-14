package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
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
					if hasPlanField(n) && !planFieldTypeIsAllowlisted(n.Type, parsed.Name.Name) {
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
var planFieldAllowlist = map[string]bool{
	"JobPayload":             true, // internal/workflow/mailbox.go — the persisted payload
	"JobRequest":             true, // internal/workflow/mailbox.go — the enqueue request
	"RuntimeContractRequest": true, // internal/runtime/preflight.go — preflight scoping
	// Surfaced BY the inversion, and exactly the decision it exists to force: both
	// have a field literally named Plan that has nothing to do with plan mode.
	// groomApplyResult.Plan is a plan-file PATH (string); DelegationTimeoutDefaults.Plan
	// is a DURATION. The old name-plus-type matcher never saw either, because it only
	// looked at runtime.Job; the new one sees every Plan field and asks. Listing them
	// here is a deliberate act with a diff, which is the point.
	"groomApplyResult":           true, // internal/cli/memory_groom.go — Plan is a file path
	"DelegationTimeoutDefaults":  true, // internal/workflow — Plan is a timeout duration
}

// planFieldTypeIsAllowlisted reports whether a composite literal's type is one of
// the shapes allowed to carry plan fields without being a census producer. An
// unrecognised or unnamed type is NOT allowlisted, so it is reported.
func planFieldTypeIsAllowlisted(expr ast.Expr, pkgName string) bool {
	switch typed := expr.(type) {
	case *ast.SelectorExpr:
		// pkg.Type{...} — judge on the type name; a qualified runtime.Job is
		// deliberately absent from the allowlist and therefore reported.
		return planFieldAllowlist[typed.Sel.Name]
	case *ast.Ident:
		return planFieldAllowlist[typed.Name]
	case *ast.StarExpr:
		return planFieldTypeIsAllowlisted(typed.X, pkgName)
	case *ast.ParenExpr:
		return planFieldTypeIsAllowlisted(typed.X, pkgName)
	default:
		// Unnamed, generic-instantiated, array/map element literals and anything
		// else the walk cannot name: NOT allowlisted. Fail closed.
		return false
	}
}

// TestPlanFieldCensusFailsClosedOnUnknownTypes proves the inversion. Each fixture is
// parsed from source and run through the same predicates the census uses, so a
// spelling that bypasses the census must also bypass this test — which is the
// property the three earlier bypasses all lacked.
func TestPlanFieldCensusFailsClosedOnUnknownTypes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool // want == reported by the census
	}{
		{"qualified runtime.Job", "_ = runtime.Job{Plan: true}", true},
		{"TYPE ALIAS — the round-5 bypass", "_ = censusJobAlias{Plan: true, PlanInto: \"@smol\"}", true},
		{"alias of an alias", "_ = deeperAlias{PlanInto: \"x\"}", true},
		{"bare Ident under dot-import", "_ = Job{Plan: true}", true},
		{"pointer literal", "_ = &runtime.Job{Plan: true}", true},
		{"unknown local type", "_ = someShim{Plan: true}", true},
		{"allowlisted payload", "_ = JobPayload{Plan: true}", false},
		{"allowlisted request", "_ = JobRequest{PlanInto: \"x\"}", false},
		{"allowlisted qualified", "_ = runtime.RuntimeContractRequest{Plan: true}", false},
		{"no plan field at all", "_ = runtime.Job{Prompt: \"x\"}", false},
	}
	for _, tc := range cases {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package workflow\nfunc f() {\n"+tc.src+"\n}\n", 0)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		var reported bool
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if ok && hasPlanField(lit) && !planFieldTypeIsAllowlisted(lit.Type, "workflow") {
				reported = true
			}
			return true
		})
		if reported != tc.want {
			t.Fatalf("%s: reported=%v, want %v (src: %s)", tc.name, reported, tc.want, tc.src)
		}
	}
}
