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
		runtimeAliases, dotRuntime := runtimeImportAliases(parsed)
		inRuntimePackage := parsed.Name.Name == "runtime" && strings.HasPrefix(filepath.ToSlash(rel), "internal/runtime/")
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CompositeLit:
					// runtime.Job{..., Plan: true, ...}
					if isRuntimeJobType(n.Type, runtimeAliases, dotRuntime, inRuntimePackage) && hasPlanField(n) {
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

func runtimeImportAliases(file *ast.File) (map[string]bool, bool) {
	aliases := map[string]bool{}
	dot := false
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path != "github.com/gitmoot/gitmoot/internal/runtime" {
			continue
		}
		name := "runtime"
		if imported.Name != nil {
			name = imported.Name.Name
		}
		if name == "." {
			dot = true
		} else if name != "_" {
			aliases[name] = true
		}
	}
	return aliases, dot
}

func isRuntimeJobType(expr ast.Expr, aliases map[string]bool, dotRuntime, inRuntimePackage bool) bool {
	switch typed := expr.(type) {
	case *ast.SelectorExpr:
		alias, ok := typed.X.(*ast.Ident)
		return ok && aliases[alias.Name] && typed.Sel.Name == "Job"
	case *ast.Ident:
		return typed.Name == "Job" && (dotRuntime || inRuntimePackage)
	default:
		return false
	}
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
func assignsPlanField(assign *ast.AssignStmt) bool {
	for _, lhs := range assign.Lhs {
		selector, ok := lhs.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if selector.Sel != nil && (selector.Sel.Name == "Plan" || selector.Sel.Name == "PlanInto") {
			return true
		}
	}
	return false
}
