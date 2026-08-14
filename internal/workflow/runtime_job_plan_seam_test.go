package workflow

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
)

const runtimePackagePath = "github.com/gitmoot/gitmoot/internal/runtime"

type planCensusBuildContext struct {
	name   string
	goos   string
	goarch string
	tags   []string
}

var planCensusReleaseBuildContexts = []planCensusBuildContext{
	{name: "linux/amd64", goos: "linux", goarch: "amd64"},
	{name: "linux/arm64", goos: "linux", goarch: "arm64"},
	{name: "darwin/amd64", goos: "darwin", goarch: "amd64"},
	{name: "darwin/arm64", goos: "darwin", goarch: "arm64"},
}

// TestRuntimeJobPlanFieldsHaveSingleGatedProducer is a semantic regression
// guard on the deliver-time plan gate. It type-checks the non-test packages
// selected by `./...` in every GOOS/GOARCH context built by release.yml, with
// CGO disabled and custom build tags pinned empty. Like `go list ./...`, that
// scope excludes testdata, nested modules, test files, and files guarded by
// custom tags; fixture tests below make those boundaries explicit. For each
// release context it resolves runtime.Job once and reports:
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
	totalStarted := time.Now()
	for _, buildContext := range planCensusReleaseBuildContexts {
		t.Run(buildContext.name, func(t *testing.T) {
			started := time.Now()
			got, err := planCensusLoadInContext(root, buildContext, "./...")
			if err != nil {
				t.Fatalf("load semantic runtime.Job plan census: %v", err)
			}
			t.Logf("semantic runtime.Job plan census wall time: %s", time.Since(started).Round(time.Millisecond))
			want := []string{"internal/workflow/mailbox.go::Mailbox.deliver"}
			if !slices.Equal(got, want) {
				t.Fatalf("runtime.Job plan producers = %v, want exactly %v; route every producer through the deliver-time plan gate", got, want)
			}
		})
	}
	t.Logf("all release-context semantic census wall time: %s", time.Since(totalStarted).Round(time.Millisecond))
}

// planCensusLoadInContext is the sole loading and scanning entry point used by
// both the real census and the compiled fixtures. Tests never call lower-level
// predicates directly, so a helper can pass only while it remains wired into
// the real path.
func planCensusLoadInContext(root string, buildContext planCensusBuildContext, patterns ...string) ([]string, error) {
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
	config := &packages.Config{
		Mode:       mode,
		Dir:        absoluteRoot,
		Tests:      false,
		BuildFlags: []string{"-tags=" + strings.Join(buildContext.tags, ",")},
	}
	if buildContext.goos != "" || buildContext.goarch != "" {
		config.Env = planCensusEnvironment(
			"GOOS="+buildContext.goos,
			"GOARCH="+buildContext.goarch,
			"CGO_ENABLED=0",
		)
	}
	loaded, err := packages.Load(config, patterns...)
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

func planCensusEnvironment(overrides ...string) []string {
	overridden := make(map[string]struct{}, len(overrides))
	for _, override := range overrides {
		name, _, _ := strings.Cut(override, "=")
		overridden[name] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replace := overridden[name]; !replace {
			environment = append(environment, entry)
		}
	}
	return append(environment, overrides...)
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
	got, err := planCensusLoadInContext(root, planCensusBuildContext{name: "host-default"}, "./internal/workflow/testdata/semantic_plan_census/...")
	if err != nil {
		t.Fatalf("load compiled semantic census fixtures: %v", err)
	}

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
		assertPlanCensusAbsent(t, got, "::UnkeyedPositionalControl")
	})
	t.Run("struct copy is a documented limit", func(t *testing.T) {
		assertPlanCensusAbsent(t, got, "::StructCopyLimit")
	})
	t.Run("reflection is a documented limit", func(t *testing.T) {
		assertPlanCensusAbsent(t, got, "::ReflectionLimit")
	})
	t.Run("unrelated pointer receiver", func(t *testing.T) {
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

// TestSemanticPlanCensusPredecessorBypassLanes plants each acceptance lane in
// its own compiled module. Their package paths and symbol names are intentional:
// the syntactic census at 1f384083 misses each module independently, while the
// semantic census resolves every producer to runtime.Job.
func TestSemanticPlanCensusPredecessorBypassLanes(t *testing.T) {
	lanes := []struct {
		name        string
		files       map[string]string
		assertShape func(*testing.T, string)
		want        []string
	}{
		{
			name: "alias chains",
			files: map[string]string{
				"internal/workflow/alias_same.go": `package workflow

import "github.com/gitmoot/gitmoot/internal/runtime"

type sameFileAlias = runtime.Job
type JobPayload = sameFileAlias

func AliasSameFile() { _ = JobPayload{Plan: true} }
`,
				"internal/workflow/alias_cross_file_a.go": `package workflow

import "github.com/gitmoot/gitmoot/internal/runtime"

type crossFileAlias = runtime.Job
`,
				"internal/workflow/alias_cross_file_b.go": `package workflow

type JobRequest = crossFileAlias

func AliasCrossFile() { _ = JobRequest{PlanInto: "@smol"} }
`,
				"internal/workflow/alias_cross_package_a.go": `package workflow

import "github.com/gitmoot/gitmoot/internal/runtime"

type crossPackageAlias = runtime.Job
type DelegationTimeoutDefaults = crossPackageAlias
`,
				"internal/consumer/alias_cross_package_b.go": `package consumer

import "github.com/gitmoot/gitmoot/internal/workflow"

func AliasCrossPackage() { _ = workflow.DelegationTimeoutDefaults{Plan: true} }
`,
			},
			want: []string{
				"internal/consumer/alias_cross_package_b.go::AliasCrossPackage",
				"internal/workflow/alias_cross_file_b.go::AliasCrossFile",
				"internal/workflow/alias_same.go::AliasSameFile",
			},
			assertShape: assertSemanticPlanAliasFixtureShape,
		},
		{
			name: "generic embedding",
			files: map[string]string{
				"internal/cli/skillopt_trainrun_tui.go": `package cli

import "github.com/gitmoot/gitmoot/internal/runtime"

type genericCarrier[T any] struct {
	runtime.Job
	Value T
}
type genericAlias[T any] = genericCarrier[T]

var runSkillOptTrainRunConfirmTUI = func() {
	var deps genericAlias[string]
	deps.Plan = true
}
`,
			},
			assertShape: assertSemanticPlanGenericFixtureShape,
			want:        []string{"internal/cli/skillopt_trainrun_tui.go::var runSkillOptTrainRunConfirmTUI"},
		},
		{
			name: "unkeyed positional",
			files: map[string]string{
				"internal/workflow/unkeyed.go": `package workflow

import "github.com/gitmoot/gitmoot/internal/runtime"

func UnkeyedDirect() { _ = runtime.Job{"", true, ""} }
func UnkeyedSlice() { _ = []runtime.Job{{"", true, ""}} }
func UnkeyedMap() { _ = map[string]runtime.Job{"job": {"", true, "@smol"}} }
`,
			},
			want: []string{
				"internal/workflow/unkeyed.go::UnkeyedDirect",
				"internal/workflow/unkeyed.go::UnkeyedMap",
				"internal/workflow/unkeyed.go::UnkeyedSlice",
			},
			assertShape: assertSemanticPlanUnkeyedFixtureShape,
		},
		{
			name: "pointer from call",
			files: map[string]string{
				"internal/cli/skillopt_trainrun_tui.go": `package cli

import "github.com/gitmoot/gitmoot/internal/runtime"

func jobPtr() *runtime.Job { return new(runtime.Job) }

var runSkillOptTrainRunConfirmTUI = func() {
	deps := jobPtr()
	deps.Plan = true
}
`,
			},
			assertShape: assertSemanticPlanPointerFixtureShape,
			want:        []string{"internal/cli/skillopt_trainrun_tui.go::var runSkillOptTrainRunConfirmTUI"},
		},
	}
	for _, lane := range lanes {
		t.Run(lane.name, func(t *testing.T) {
			root := writeSemanticPlanFixtureModule(t, lane.files)
			lane.assertShape(t, root)
			got, err := planCensusLoadInContext(root, planCensusBuildContext{name: "host-default"}, "./...")
			if err != nil {
				t.Fatalf("load compiled lane: %v", err)
			}
			t.Logf("semantic producers: %v", got)
			if !slices.Equal(got, lane.want) {
				t.Fatalf("semantic producers = %v, want exactly %v", got, lane.want)
			}
		})
	}
}

func assertSemanticPlanAliasFixtureShape(t *testing.T, root string) {
	t.Helper()
	cases := []struct {
		file   string
		name   string
		target string
	}{
		{"internal/workflow/alias_same.go", "sameFileAlias", "runtime.Job"},
		{"internal/workflow/alias_same.go", "JobPayload", "sameFileAlias"},
		{"internal/workflow/alias_cross_file_a.go", "crossFileAlias", "runtime.Job"},
		{"internal/workflow/alias_cross_file_b.go", "JobRequest", "crossFileAlias"},
		{"internal/workflow/alias_cross_package_a.go", "crossPackageAlias", "runtime.Job"},
		{"internal/workflow/alias_cross_package_a.go", "DelegationTimeoutDefaults", "crossPackageAlias"},
	}
	for _, check := range cases {
		file := parseSemanticPlanFixture(t, root, check.file)
		if got := semanticPlanFixtureAliasTarget(file, check.name); got != check.target {
			t.Errorf("%s alias %s target = %q, want %q", check.file, check.name, got, check.target)
		}
	}
	consumer := parseSemanticPlanFixture(t, root, "internal/consumer/alias_cross_package_b.go")
	if !semanticPlanFixtureHasCompositeType(consumer, "workflow.DelegationTimeoutDefaults") {
		t.Error("cross-package alias fixture no longer constructs workflow.DelegationTimeoutDefaults")
	}
}

func assertSemanticPlanGenericFixtureShape(t *testing.T, root string) {
	t.Helper()
	file := parseSemanticPlanFixture(t, root, "internal/cli/skillopt_trainrun_tui.go")
	carrier := semanticPlanFixtureTypeSpec(file, "genericCarrier")
	if carrier == nil || carrier.TypeParams == nil || carrier.TypeParams.NumFields() != 1 {
		t.Fatal("generic fixture carrier must retain one type parameter")
	}
	structure, ok := carrier.Type.(*ast.StructType)
	if !ok || !semanticPlanFixtureStructEmbeds(structure, "runtime.Job") {
		t.Fatal("generic fixture carrier must embed runtime.Job")
	}
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		value, ok := node.(*ast.ValueSpec)
		if !ok || len(value.Names) != 1 || value.Names[0].Name != "deps" {
			return true
		}
		indexed, ok := value.Type.(*ast.IndexExpr)
		found = ok && semanticPlanFixtureExprName(indexed.X) == "genericAlias" && semanticPlanFixtureExprName(indexed.Index) == "string"
		return !found
	})
	if !found {
		t.Fatal("generic fixture deps must be declared as genericAlias[string]")
	}
}

func assertSemanticPlanUnkeyedFixtureShape(t *testing.T, root string) {
	t.Helper()
	file := parseSemanticPlanFixture(t, root, "internal/workflow/unkeyed.go")
	for _, name := range []string{"UnkeyedDirect", "UnkeyedSlice", "UnkeyedMap"} {
		function := semanticPlanFixtureFunction(file, name)
		found := false
		if function != nil {
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, ok := node.(*ast.CompositeLit)
				if !ok || len(literal.Elts) != 3 {
					return true
				}
				for _, element := range literal.Elts {
					if _, keyed := element.(*ast.KeyValueExpr); keyed {
						return true
					}
				}
				found = true
				return false
			})
		}
		if !found {
			t.Errorf("%s must retain a three-element unkeyed positional literal", name)
		}
	}
}

func assertSemanticPlanPointerFixtureShape(t *testing.T, root string) {
	t.Helper()
	file := parseSemanticPlanFixture(t, root, "internal/cli/skillopt_trainrun_tui.go")
	jobPtr := semanticPlanFixtureFunction(file, "jobPtr")
	if jobPtr == nil || jobPtr.Type.Results == nil || len(jobPtr.Type.Results.List) != 1 {
		t.Fatal("pointer fixture must declare jobPtr with one result")
	}
	pointer, ok := jobPtr.Type.Results.List[0].Type.(*ast.StarExpr)
	if !ok || semanticPlanFixtureExprName(pointer.X) != "runtime.Job" {
		t.Fatal("pointer fixture jobPtr must return *runtime.Job")
	}
	hasCallOrigin := false
	hasPlanWrite := false
	ast.Inspect(file, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		if assignment.Tok == token.DEFINE {
			name, nameOK := assignment.Lhs[0].(*ast.Ident)
			call, callOK := assignment.Rhs[0].(*ast.CallExpr)
			hasCallOrigin = nameOK && name.Name == "deps" && callOK && semanticPlanFixtureExprName(call.Fun) == "jobPtr"
		}
		selector, selectorOK := assignment.Lhs[0].(*ast.SelectorExpr)
		if selectorOK {
			receiver, receiverOK := selector.X.(*ast.Ident)
			hasPlanWrite = hasPlanWrite || receiverOK && receiver.Name == "deps" && selector.Sel.Name == "Plan"
		}
		return true
	})
	if !hasCallOrigin || !hasPlanWrite {
		t.Fatalf("pointer fixture call origin = %v, Plan write = %v; want both", hasCallOrigin, hasPlanWrite)
	}
}

func parseSemanticPlanFixture(t *testing.T, root, name string) *ast.File {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return parsed
}

func semanticPlanFixtureAliasTarget(file *ast.File, name string) string {
	typed := semanticPlanFixtureTypeSpec(file, name)
	if typed == nil || !typed.Assign.IsValid() {
		return ""
	}
	return semanticPlanFixtureExprName(typed.Type)
}

func semanticPlanFixtureTypeSpec(file *ast.File, name string) *ast.TypeSpec {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typed, ok := specification.(*ast.TypeSpec)
			if ok && typed.Name.Name == name {
				return typed
			}
		}
	}
	return nil
}

func semanticPlanFixtureFunction(file *ast.File, name string) *ast.FuncDecl {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	return nil
}

func semanticPlanFixtureHasCompositeType(file *ast.File, want string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if ok && semanticPlanFixtureExprName(literal.Type) == want {
			found = true
			return false
		}
		return !found
	})
	return found
}

func semanticPlanFixtureStructEmbeds(structure *ast.StructType, want string) bool {
	for _, field := range structure.Fields.List {
		if len(field.Names) == 0 && semanticPlanFixtureExprName(field.Type) == want {
			return true
		}
	}
	return false
}

func semanticPlanFixtureExprName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.SelectorExpr:
		prefix := semanticPlanFixtureExprName(expression.X)
		if prefix == "" {
			return expression.Sel.Name
		}
		return prefix + "." + expression.Sel.Name
	default:
		return ""
	}
}

func writeSemanticPlanFixtureModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	common := map[string]string{
		"go.mod": `module github.com/gitmoot/gitmoot

go 1.26.0
`,
		"internal/runtime/job.go": `package runtime

type Job struct {
	Value string
	Plan bool
	PlanInto string
}
`,
	}
	for name, content := range files {
		common[name] = content
	}
	for name, content := range common {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture directory for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return root
}

func TestSemanticPlanCensusBuildContextScope(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	pattern := "./internal/workflow/testdata/semantic_plan_census/producer"
	t.Setenv("GOFLAGS", strings.TrimSpace(os.Getenv("GOFLAGS")+" -tags=plan_census_tagged_fixture"))

	got, err := planCensusLoadInContext(root, planCensusBuildContext{name: "host-default"}, pattern)
	if err != nil {
		t.Fatalf("load fixture in pinned default context: %v", err)
	}
	assertPlanCensusAbsent(t, got, "::TaggedBuildContext")

	got, err = planCensusLoadInContext(root, planCensusBuildContext{
		name: "tagged-fixture",
		tags: []string{"plan_census_tagged_fixture"},
	}, pattern)
	if err != nil {
		t.Fatalf("load fixture in explicit tagged context: %v", err)
	}
	assertPlanCensusProducers(t, got,
		"internal/workflow/testdata/semantic_plan_census/producer/tagged_plan.go::TaggedBuildContext",
	)
}

// TestSemanticPlanCensusReleaseContexts pins the contexts audited by the real
// census to the target matrix in .github/workflows/release.yml. The production
// loop and this contract are deliberately separate: deleting a context from the
// loop's input must fail instead of silently narrowing the guarantee.
func TestSemanticPlanCensusReleaseContexts(t *testing.T) {
	var got []string
	for _, buildContext := range planCensusReleaseBuildContexts {
		got = append(got, fmt.Sprintf("%s:%s/%s:tags=%s", buildContext.name, buildContext.goos, buildContext.goarch, strings.Join(buildContext.tags, ",")))
	}
	want := []string{
		"linux/amd64:linux/amd64:tags=",
		"linux/arm64:linux/arm64:tags=",
		"darwin/amd64:darwin/amd64:tags=",
		"darwin/arm64:darwin/arm64:tags=",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("release census contexts = %v, want %v", got, want)
	}
}

// TestSemanticPlanCensusReleaseContextApplication proves the context list is
// not merely labels around repeated host-context loads. This compiled producer
// is selected only when GOOS=linux and GOARCH=arm64 reach packages.Load.
func TestSemanticPlanCensusReleaseContextApplication(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	want := "internal/workflow/testdata/semantic_plan_census/producer/linux_arm64_plan.go::LinuxARM64BuildContext"
	for _, buildContext := range planCensusReleaseBuildContexts {
		got, err := planCensusLoadInContext(root, buildContext, "./internal/workflow/testdata/semantic_plan_census/producer")
		if err != nil {
			t.Fatalf("load %s fixture context: %v", buildContext.name, err)
		}
		hasProducer := slices.Contains(got, want)
		wantProducer := buildContext.name == "linux/arm64"
		if hasProducer != wantProducer {
			t.Errorf("%s producers = %v, LinuxARM64BuildContext present = %v, want %v", buildContext.name, got, hasProducer, wantProducer)
		}
	}
}

func TestSemanticPlanCensusRejectsPackageErrors(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	_, err := planCensusLoadInContext(root, planCensusBuildContext{
		name: "load-error-fixture",
		tags: []string{"plan_census_load_error"},
	}, "./internal/workflow/testdata/semantic_plan_census/loaderror")
	if err == nil {
		t.Fatal("semantic census accepted a package with a type-check error")
	}
	if !strings.Contains(err.Error(), "undefined: planCensusUndefinedSymbol") {
		t.Fatalf("semantic census error = %q, want undefined fixture symbol", err)
	}
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
