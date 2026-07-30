package subprocess

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRunnerWrappersImplementEnvironmentPIDStreaming(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read subprocess package: %v", err)
	}
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check("github.com/gitmoot/gitmoot/internal/subprocess", fset, files, nil)
	if err != nil {
		t.Fatalf("type-check subprocess package: %v", err)
	}

	// A transparent wrapper owns an Inner Runner. TeeRunner is deliberately not
	// in this class: its Inner is already a StreamRunner and it terminates the
	// wrapper chain by adapting live output back to the plain Runner surface.
	runnerType := pkg.Scope().Lookup("Runner").Type()
	contract := pkg.Scope().Lookup("EnvPIDStreamRunner").Type().Underlying().(*types.Interface)
	contract.Complete()

	var wrappers []string
	for _, name := range pkg.Scope().Names() {
		typeName, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := typeName.Type().(*types.Named)
		if !ok {
			continue
		}
		structType, ok := named.Underlying().(*types.Struct)
		if !ok || !wrapsRunner(structType, runnerType) {
			continue
		}
		wrappers = append(wrappers, name)
		if !types.Implements(named, contract) && !types.Implements(types.NewPointer(named), contract) {
			t.Errorf("runner wrapper %s does not implement EnvPIDStreamRunner", name)
		}
	}
	sort.Strings(wrappers)
	if len(wrappers) == 0 {
		t.Fatal("found no runner wrappers; contract check passed vacuously")
	}
}

func wrapsRunner(structType *types.Struct, runnerType types.Type) bool {
	for i := 0; i < structType.NumFields(); i++ {
		field := structType.Field(i)
		if field.Name() == "Inner" && types.Identical(field.Type(), runnerType) {
			return true
		}
	}
	return false
}
