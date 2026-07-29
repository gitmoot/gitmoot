package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestMergeGateStoreAccessSurface is the interface-level firewall between the
// merge authority and display-only review evidence. The eleven calls below are
// the complete *db.Store surface used by PolicyMergeGate, plus the native review
// aggregation read in allRequiredReviewersApproved. Adding any store call makes
// this test fail until its authority implications are reviewed explicitly.
func TestMergeGateStoreAccessSurface(t *testing.T) {
	want := []string{
		"AcquireResourceLock",
		"ClearTaskWorktreePath",
		"GetBranchLock",
		"GetNoCIObservation",
		"GetTask",
		"ListJobs",
		"ReleaseLockWithEvent",
		"ReleaseResourceLock",
		"UpsertMergeGate",
		"UpsertNoCIObservation",
		"UpsertPullRequest",
	}
	sort.Strings(want)

	mergeGate := parseGoFile(t, "merge_gate.go")
	routing := parseGoFile(t, "engine_routing_merge.go")
	got := storeMethods(mergeGate, "g", "")
	got = append(got, storeMethods(routing, "e", "allRequiredReviewersApproved")...)
	got = uniqueSorted(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge-gate store access surface changed:\n got: %v\nwant: %v\n"+
			"display-plane reads such as workflow_notes and proof artifacts must never enter the gate",
			got, want)
	}

	forbidden := map[string]string{
		"GetJobForProof":                "proof artifact projection",
		"GetLatestJobEventByKind":       "session review display event",
		"GetServiceRun":                 "proof/artifact receipt",
		"GetWorkflowNote":               "workflow_notes",
		"ListJobEvents":                 "session review display events",
		"ListWorkflowNotes":             "workflow_notes",
		"ListWorkflowNotesByBodyPrefix": "workflow_notes",
	}
	for _, method := range got {
		if plane, blocked := forbidden[method]; blocked {
			t.Fatalf("merge gate reads forbidden display plane %s through Store.%s", plane, method)
		}
	}
	assertNoMergeGateImport(t, mergeGate, "github.com/gitmoot/gitmoot/internal/proof")
	assertNoMergeGateImport(t, routing, "github.com/gitmoot/gitmoot/internal/proof")
}

func parseGoFile(t *testing.T, name string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

func storeMethods(file *ast.File, receiver, function string) []string {
	var methods []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || (function != "" && fn.Name.Name != function) {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			store, ok := method.X.(*ast.SelectorExpr)
			if !ok || store.Sel.Name != "Store" {
				return true
			}
			ident, ok := store.X.(*ast.Ident)
			if ok && ident.Name == receiver {
				methods = append(methods, method.Sel.Name)
			}
			return true
		})
	}
	return methods
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func assertNoMergeGateImport(t *testing.T, file *ast.File, forbidden string) {
	t.Helper()
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote import %s: %v", spec.Path.Value, err)
		}
		if strings.TrimSpace(path) == forbidden {
			t.Fatalf("merge gate imports forbidden proof package %q", forbidden)
		}
	}
}
