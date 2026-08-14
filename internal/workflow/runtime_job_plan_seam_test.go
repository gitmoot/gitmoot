package workflow

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
)

const runtimePackagePath = "github.com/gitmoot/gitmoot/internal/runtime"

// TestRuntimeJobPlanFieldsHaveSingleGatedProducer is a semantic regression
// guard on the deliver-time plan gate. It type-checks every non-test package in
// the module, resolves runtime.Job once, and reports:
//
//   - composite literals that initialize runtime.Job.Plan or PlanInto, including
//     aliases, alias chains, omitted element types, and unkeyed positional forms;
//   - assignments whose resolved field object is runtime.Job.Plan or PlanInto,
//     including promoted fields and receivers returned by calls.
//
// The guard intentionally does not claim to detect value flow through a plain
// struct copy: `j2 := j1` writes no field the type checker can attribute. It also
// cannot interpret reflection such as FieldByName("Plan"); that target exists only
// as runtime data. TestSemanticPlanCensusCompiledFixtures keeps both limits as
// explicit negative assertions so they cannot be mistaken for covered cases.
func TestRuntimeJobPlanFieldsHaveSingleGatedProducer(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	started := time.Now()
	got, err := planCensusLoad(root, "./...")
	if err != nil {
		t.Fatalf("load semantic runtime.Job plan census: %v", err)
	}
	t.Logf("semantic runtime.Job plan census wall time: %s", time.Since(started).Round(time.Millisecond))
	want := []string{"internal/workflow/mailbox.go::Mailbox.deliver"}
	if !slices.Equal(got, want) {
		t.Fatalf("runtime.Job plan producers = %v, want exactly %v; route every producer through the deliver-time plan gate", got, want)
	}
}

// planCensusLoad is the sole loading and scanning entry point used by both the
// real census and the compiled fixtures. Tests never call lower-level predicates
// directly, so a helper can pass only while it remains wired into the real path.
func planCensusLoad(root string, patterns ...string) ([]string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve census root: %w", err)
	}
	mode := packages.NeedName |
		packages.NeedFiles |
		packages.NeedCompiledGoFiles |
		packages.NeedImports |
		packages.NeedDeps |
		packages.NeedTypes |
		packages.NeedSyntax |
		packages.NeedTypesInfo
	loaded, err := packages.Load(&packages.Config{Mode: mode, Dir: absoluteRoot, Tests: false}, patterns...)
	if err != nil {
		return nil, err
	}
	if err := planCensusPackageErrors(loaded); err != nil {
		return nil, err
	}
	job, err := planCensusResolveJob(loaded)
	if err != nil {
		return nil, err
	}
	return planCensusProducers(absoluteRoot, loaded, job), nil
}

func planCensusPackageErrors(loaded []*packages.Package) error {
	var messages []string
	packages.Visit(loaded, nil, func(pkg *packages.Package) {
		for _, loadErr := range pkg.Errors {
			messages = append(messages, loadErr.Error())
		}
	})
	if len(messages) == 0 {
		return nil
	}
	slices.Sort(messages)
	return fmt.Errorf("type-check packages:\n%s", strings.Join(messages, "\n"))
}

type planCensusJobType struct {
	typ          types.Type
	fields       map[*types.Var]struct{}
	fieldIndexes map[int]struct{}
}

func planCensusResolveJob(loaded []*packages.Package) (planCensusJobType, error) {
	var runtimePkg *packages.Package
	packages.Visit(loaded, nil, func(pkg *packages.Package) {
		if pkg.PkgPath == runtimePackagePath {
			runtimePkg = pkg
		}
	})
	if runtimePkg == nil || runtimePkg.Types == nil {
		return planCensusJobType{}, fmt.Errorf("%s was not loaded", runtimePackagePath)
	}
	object := runtimePkg.Types.Scope().Lookup("Job")
	if object == nil {
		return planCensusJobType{}, fmt.Errorf("%s.Job was not found", runtimePackagePath)
	}
	jobType := object.Type()
	structure, ok := types.Unalias(jobType).Underlying().(*types.Struct)
	if !ok {
		return planCensusJobType{}, fmt.Errorf("%s.Job has type %T, want struct", runtimePackagePath, types.Unalias(jobType).Underlying())
	}
	job := planCensusJobType{
		typ:          jobType,
		fields:       make(map[*types.Var]struct{}, 2),
		fieldIndexes: make(map[int]struct{}, 2),
	}
	for index := 0; index < structure.NumFields(); index++ {
		field := structure.Field(index)
		if field.Name() == "Plan" || field.Name() == "PlanInto" {
			job.fields[field] = struct{}{}
			job.fieldIndexes[index] = struct{}{}
		}
	}
	if len(job.fields) != 2 {
		return planCensusJobType{}, fmt.Errorf("%s.Job plan fields = %d, want Plan and PlanInto", runtimePackagePath, len(job.fields))
	}
	return job, nil
}

func planCensusProducers(root string, loaded []*packages.Package, job planCensusJobType) []string {
	producers := make(map[string]struct{})
	for _, pkg := range loaded {
		for _, file := range pkg.Syntax {
			filename := pkg.Fset.Position(file.Pos()).Filename
			rel, err := filepath.Rel(root, filename)
			if err != nil {
				rel = filename
			}
			rel = filepath.ToSlash(rel)
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.FuncDecl:
					if declaration.Body != nil && planCensusNodeWritesPlan(pkg.TypesInfo, declaration.Body, job) {
						producers[rel+"::"+planCensusFunctionSymbol(declaration)] = struct{}{}
					}
				case *ast.GenDecl:
					if declaration.Tok == token.TYPE {
						continue
					}
					for _, specification := range declaration.Specs {
						value, ok := specification.(*ast.ValueSpec)
						if !ok {
							continue
						}
						name := "package-scope"
						if len(value.Names) > 0 {
							name = "var " + value.Names[0].Name
						}
						for _, expression := range value.Values {
							if planCensusNodeWritesPlan(pkg.TypesInfo, expression, job) {
								producers[rel+"::"+name] = struct{}{}
							}
						}
					}
				}
			}
		}
	}
	result := make([]string, 0, len(producers))
	for producer := range producers {
		result = append(result, producer)
	}
	slices.Sort(result)
	return result
}

func planCensusNodeWritesPlan(info *types.Info, node ast.Node, job planCensusJobType) bool {
	writes := false
	ast.Inspect(node, func(node ast.Node) bool {
		if writes {
			return false
		}
		switch node := node.(type) {
		case *ast.CompositeLit:
			writes = planCensusLiteralWritesPlan(info, node, job)
		case *ast.AssignStmt:
			writes = planCensusAssignmentWritesPlan(info, node, job)
		}
		return !writes
	})
	return writes
}

func planCensusLiteralWritesPlan(info *types.Info, literal *ast.CompositeLit, job planCensusJobType) bool {
	if !types.Identical(info.TypeOf(literal), job.typ) {
		return false
	}
	for index, element := range literal.Elts {
		if keyed, ok := element.(*ast.KeyValueExpr); ok {
			identifier, ok := keyed.Key.(*ast.Ident)
			if !ok {
				continue
			}
			field, ok := info.Uses[identifier].(*types.Var)
			if _, planField := job.fields[field]; ok && planField {
				return true
			}
			continue
		}
		if _, planField := job.fieldIndexes[index]; planField {
			return true
		}
	}
	return false
}

func planCensusAssignmentWritesPlan(info *types.Info, assignment *ast.AssignStmt, job planCensusJobType) bool {
	for _, target := range assignment.Lhs {
		selector := planCensusAssignedSelector(target)
		if selector == nil {
			continue
		}
		selection := info.Selections[selector]
		if selection == nil {
			continue
		}
		field, ok := selection.Obj().(*types.Var)
		if _, planField := job.fields[field]; ok && planField {
			return true
		}
	}
	return false
}

func planCensusAssignedSelector(expression ast.Expr) *ast.SelectorExpr {
	for {
		switch current := expression.(type) {
		case *ast.ParenExpr:
			expression = current.X
		case *ast.StarExpr:
			expression = current.X
		case *ast.UnaryExpr:
			if current.Op != token.AND {
				return nil
			}
			expression = current.X
		case *ast.SelectorExpr:
			return current
		default:
			return nil
		}
	}
}

func planCensusFunctionSymbol(function *ast.FuncDecl) string {
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

func TestSemanticPlanCensusCompiledFixtures(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	got, err := planCensusLoad(root, "./internal/workflow/testdata/semantic_plan_census/...")
	if err != nil {
		t.Fatalf("load compiled semantic census fixtures: %v", err)
	}

	t.Run("alias chains", func(t *testing.T) {
		assertPlanCensusProducers(t, got,
			"internal/workflow/testdata/semantic_plan_census/producer/alias_same.go::AliasSameFile",
			"internal/workflow/testdata/semantic_plan_census/producer/alias_cross_file_b.go::AliasCrossFile",
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::AliasCrossPackage",
		)
	})
	t.Run("keyed PlanInto", func(t *testing.T) {
		assertPlanCensusProducers(t, got,
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::KeyedPlanInto",
		)
	})
	t.Run("unkeyed positional", func(t *testing.T) {
		assertPlanCensusProducers(t, got,
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::UnkeyedDirect",
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::UnkeyedSlice",
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::UnkeyedMap",
		)
	})
	t.Run("generic embedding", func(t *testing.T) {
		assertPlanCensusProducers(t, got,
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::GenericEmbedding",
		)
	})
	t.Run("struct copy is a documented limit", func(t *testing.T) {
		assertPlanCensusAbsent(t, got, "::StructCopyLimit")
	})
	t.Run("reflection is a documented limit", func(t *testing.T) {
		assertPlanCensusAbsent(t, got, "::ReflectionLimit")
	})
	t.Run("pointer from a call", func(t *testing.T) {
		assertPlanCensusProducers(t, got,
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::PointerFromCall",
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::PointerFromCallMultiLHS",
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::PointerFromCallWrapped",
		)
		assertPlanCensusAbsent(t, got, "::UnrelatedPointerFromCall")
	})
	t.Run("unrelated controls", func(t *testing.T) {
		assertPlanCensusAbsent(t, got, "::Controls")
	})
	t.Run("package-level initializer", func(t *testing.T) {
		assertPlanCensusProducers(t, got,
			"internal/workflow/testdata/semantic_plan_census/producer/producer.go::var PackageLevelPlan",
		)
	})
}

func assertPlanCensusProducers(t *testing.T, got []string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !slices.Contains(got, want) {
			t.Errorf("producers = %v, want %s", got, want)
		}
	}
}

func assertPlanCensusAbsent(t *testing.T, got []string, suffix string) {
	t.Helper()
	for _, producer := range got {
		if strings.HasSuffix(producer, suffix) {
			t.Errorf("producers = %v, did not want suffix %s", got, suffix)
		}
	}
}

// TestSemanticPlanCensusHelperCallGraph proves every semantic predicate remains
// reachable from the real module census. It deliberately starts from
// TestRuntimeJobPlanFieldsHaveSingleGatedProducer rather than from fixture tests:
// a helper wired only to its own test is an inert guard.
func TestSemanticPlanCensusHelperCallGraph(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "runtime_job_plan_seam_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	functions := map[string]*ast.FuncDecl{}
	graph := map[string][]string{}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		functions[function.Name.Name] = function
	}
	for name, function := range functions {
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			called, ok := call.Fun.(*ast.Ident)
			if ok {
				if _, local := functions[called.Name]; local {
					graph[name] = append(graph[name], called.Name)
				}
			}
			return true
		})
	}
	reachable := map[string]bool{}
	queue := []string{"TestRuntimeJobPlanFieldsHaveSingleGatedProducer"}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if reachable[name] {
			continue
		}
		reachable[name] = true
		queue = append(queue, graph[name]...)
	}
	for name := range functions {
		if strings.HasPrefix(name, "planCensus") && !reachable[name] {
			t.Errorf("semantic census helper %s is unreachable from the real census entry point", name)
		}
	}
}
