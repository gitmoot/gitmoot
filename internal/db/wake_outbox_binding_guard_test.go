package db

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const wakeOutboxPredicateGenerator = "wakeOutboxObligationPredicate"

type wakeOutboxBindingFunction struct {
	decl *ast.FuncDecl
}

type wakeOutboxBindingAnalysis struct {
	constructionSites map[string]struct{}
	predicateBound    map[string]struct{}
}

func TestWakeOutboxObligationBindingIsTransitive(t *testing.T) {
	dir := wakeOutboxGuardDirectory(t)
	files := wakeOutboxProductionFiles(t, dir)
	analysis := analyzeWakeOutboxBinding(t, files)
	if err := wakeOutboxBindingViolation(analysis); err != nil {
		t.Fatal(err)
	}

	fixture := filepath.Join(dir, "testdata", "wake_outbox_handwritten_query.go.txt")
	negative := analyzeWakeOutboxBinding(t, []string{fixture})
	err := wakeOutboxBindingViolation(negative)
	if err == nil {
		t.Fatalf("committed handwritten-query fixture %s unexpectedly passed the binding guard", fixture)
	}
	if !strings.Contains(err.Error(), "construction/projection sites only") {
		t.Fatalf("committed handwritten-query fixture violation = %v, want set mismatch", err)
	}
}

func wakeOutboxGuardDirectory(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve wake outbox guard source path")
	}
	return filepath.Dir(file)
}

func wakeOutboxProductionFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	return files
}

func analyzeWakeOutboxBinding(t *testing.T, paths []string) wakeOutboxBindingAnalysis {
	t.Helper()
	fset := token.NewFileSet()
	functions := make(map[string]wakeOutboxBindingFunction)
	idsByName := make(map[string][]string)
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			id := wakeOutboxFunctionID(function)
			functions[id] = wakeOutboxBindingFunction{decl: function}
			idsByName[function.Name.Name] = append(idsByName[function.Name.Name], id)
		}
	}

	calls := make(map[string]map[string]struct{}, len(functions))
	reverseCalls := make(map[string]map[string]struct{}, len(functions))
	constructionSites := make(map[string]struct{})
	for id, function := range functions {
		calls[id] = make(map[string]struct{})
		ast.Inspect(function.decl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := wakeOutboxCalledFunctionName(call.Fun)
			for _, callee := range idsByName[name] {
				calls[id][callee] = struct{}{}
				if reverseCalls[callee] == nil {
					reverseCalls[callee] = make(map[string]struct{})
				}
				reverseCalls[callee][id] = struct{}{}
			}
			return true
		})
		if wakeOutboxReturnsProjection(function.decl) ||
			(wakeOutboxReturnsSQLTuple(function.decl) && wakeOutboxContainsSelect(function.decl)) {
			constructionSites[id] = struct{}{}
		}
	}

	predicateBound := make(map[string]struct{})
	for id, functionCalls := range calls {
		for _, generatorID := range idsByName[wakeOutboxPredicateGenerator] {
			if _, ok := functionCalls[generatorID]; ok {
				predicateBound[id] = struct{}{}
			}
		}
	}
	queue := wakeOutboxSetNames(predicateBound)
	for len(queue) > 0 {
		callee := queue[0]
		queue = queue[1:]
		for caller := range reverseCalls[callee] {
			if _, seen := predicateBound[caller]; seen {
				continue
			}
			predicateBound[caller] = struct{}{}
			queue = append(queue, caller)
		}
	}
	return wakeOutboxBindingAnalysis{
		constructionSites: constructionSites,
		predicateBound:    predicateBound,
	}
}

func wakeOutboxFunctionID(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return function.Name.Name
	}
	return wakeOutboxExpressionName(function.Recv.List[0].Type) + "." + function.Name.Name
}

func wakeOutboxExpressionName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return wakeOutboxExpressionName(value.X)
	case *ast.SelectorExpr:
		return wakeOutboxExpressionName(value.X) + "." + value.Sel.Name
	default:
		return fmt.Sprintf("%T", expression)
	}
}

func wakeOutboxCalledFunctionName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func wakeOutboxReturnsProjection(function *ast.FuncDecl) bool {
	if function.Type.Results == nil {
		return false
	}
	for _, result := range function.Type.Results.List {
		if wakeOutboxExpressionName(result.Type) == "WakeOutboxObligationProjection" {
			return true
		}
	}
	return false
}

func wakeOutboxReturnsSQLTuple(function *ast.FuncDecl) bool {
	if function.Type.Results == nil || len(function.Type.Results.List) != 2 {
		return false
	}
	first, firstOK := function.Type.Results.List[0].Type.(*ast.Ident)
	second, secondOK := function.Type.Results.List[1].Type.(*ast.ArrayType)
	if !firstOK || first.Name != "string" || !secondOK {
		return false
	}
	element, ok := second.Elt.(*ast.Ident)
	return ok && element.Name == "any"
}

func wakeOutboxContainsSelect(function *ast.FuncDecl) bool {
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && strings.Contains(strings.ToLower(value), "from wake_outbox") {
			found = true
			return false
		}
		return true
	})
	return found
}

func wakeOutboxBindingViolation(analysis wakeOutboxBindingAnalysis) error {
	onlyConstruction := wakeOutboxSetDifference(analysis.constructionSites, analysis.predicateBound)
	onlyPredicate := wakeOutboxSetDifference(analysis.predicateBound, analysis.constructionSites)
	if len(analysis.constructionSites) == 0 {
		return fmt.Errorf("wake outbox obligation guard found no construction or projection sites")
	}
	if len(onlyConstruction) == 0 && len(onlyPredicate) == 0 {
		return nil
	}
	return fmt.Errorf(
		"wake outbox obligation binding set mismatch: construction/projection sites only=%v; predicate-bound transitive callers only=%v",
		onlyConstruction,
		onlyPredicate,
	)
}

func wakeOutboxSetDifference(left, right map[string]struct{}) []string {
	var difference []string
	for value := range left {
		if _, ok := right[value]; !ok {
			difference = append(difference, value)
		}
	}
	sort.Strings(difference)
	return difference
}

func wakeOutboxSetNames(values map[string]struct{}) []string {
	names := make([]string, 0, len(values))
	for value := range values {
		names = append(names, value)
	}
	sort.Strings(names)
	return names
}
