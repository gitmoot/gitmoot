package preset

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestBuiltinsIncludesPlannerAndThermoPresets(t *testing.T) {
	definitions := Builtins()
	if len(definitions) != 3 {
		t.Fatalf("builtin count = %d, want 3", len(definitions))
	}
	thermo, ok := Lookup(ThermoNuclearCodeQualityReviewID)
	if !ok {
		t.Fatal("thermo preset missing")
	}
	if thermo.Mutation || !reflect.DeepEqual(thermo.DefaultCapabilities, []string{"ask", "review"}) {
		t.Fatalf("thermo definition = %+v", thermo)
	}
	planner, ok := Lookup(GitmootPlanAndGoalID)
	if !ok {
		t.Fatal("planner preset missing")
	}
	if !planner.Mutation || planner.DefaultRole != "planner" || !reflect.DeepEqual(planner.DefaultCapabilities, []string{"ask"}) {
		t.Fatalf("planner definition = %+v", planner)
	}
	if planner.SourceRepo != "jerryfane/gitmoot" || planner.SourcePath != "skills/gitmoot/presets/gitmoot-plan-and-goal.md" {
		t.Fatalf("planner source = %+v", planner)
	}
	lite, ok := Lookup(GitmootPlanLiteID)
	if !ok {
		t.Fatal("lite planner preset missing")
	}
	if lite.Mutation || lite.DefaultRole != "planner" || !reflect.DeepEqual(lite.DefaultCapabilities, []string{"ask"}) {
		t.Fatalf("lite planner definition = %+v", lite)
	}
	if lite.SourceRepo != "jerryfane/gitmoot" || lite.SourcePath != "skills/gitmoot/presets/gitmoot-plan-lite.md" {
		t.Fatalf("lite planner source = %+v", lite)
	}
}

func TestUpdatePlannerPreset(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	updated, err := Update(ctx, store, fakeFetcher{commit: "def456", content: "Plan carefully."}, GitmootPlanAndGoalID)
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if updated.ID != GitmootPlanAndGoalID || updated.ResolvedCommit != "def456" || updated.Content != "Plan carefully." {
		t.Fatalf("updated planner preset = %+v", updated)
	}
	if updated.SourceRepo != "jerryfane/gitmoot" || updated.SourcePath != "skills/gitmoot/presets/gitmoot-plan-and-goal.md" {
		t.Fatalf("updated source = %+v", updated)
	}
}

func TestUpdateLitePlannerPreset(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	updated, err := Update(ctx, store, fakeFetcher{commit: "fed789", content: "Plan quickly."}, GitmootPlanLiteID)
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if updated.ID != GitmootPlanLiteID || updated.ResolvedCommit != "fed789" || updated.Content != "Plan quickly." {
		t.Fatalf("updated lite planner preset = %+v", updated)
	}
	if updated.SourceRepo != "jerryfane/gitmoot" || updated.SourcePath != "skills/gitmoot/presets/gitmoot-plan-lite.md" {
		t.Fatalf("updated lite source = %+v", updated)
	}
}

func TestGHFetcherUsesGitHubAPIAndDecodesContent(t *testing.T) {
	runner := &fakeRunner{}
	fetcher := GHFetcher{Runner: runner}

	sha, err := fetcher.ResolveRef(context.Background(), "cursor/plugins", "main")
	if err != nil {
		t.Fatalf("ResolveRef returned error: %v", err)
	}
	if sha != "abc123" {
		t.Fatalf("sha = %q, want abc123", sha)
	}
	file, err := fetcher.FetchFile(context.Background(), "cursor/plugins", sha, "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md")
	if err != nil {
		t.Fatalf("FetchFile returned error: %v", err)
	}
	if file.Content != "preset body" {
		t.Fatalf("content = %q", file.Content)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %+v", runner.calls)
	}
	if !strings.Contains(strings.Join(runner.calls[1].args, " "), "-X GET repos/cursor/plugins/contents/cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md -f ref=abc123") {
		t.Fatalf("fetch args = %+v", runner.calls[1].args)
	}
}

func TestDiffReportsChangedContent(t *testing.T) {
	diff := Diff("same\nold\nend\n", "same\nnew\nend\n")
	for _, want := range []string{"--- cached", "+++ upstream", "-old", "+new"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}

func TestDiffExactReportsTrailingNewlineChange(t *testing.T) {
	diff := DiffExact("same\n", "same\n\n")
	if strings.Contains(diff, "up to date") || !strings.Contains(diff, "+++ upstream") {
		t.Fatalf("diff = %s", diff)
	}
}

func TestValidateID(t *testing.T) {
	for _, id := range []string{"frontend-reviewer", "reviewer2", "a"} {
		if err := ValidateID(id); err != nil {
			t.Fatalf("ValidateID(%q) returned error: %v", id, err)
		}
	}
	for _, id := range []string{"", "Frontend", "-bad", "bad-", "bad--id", "bad_id", "bad.id"} {
		if err := ValidateID(id); err == nil {
			t.Fatalf("ValidateID(%q) returned nil", id)
		}
	}
}

func TestAddLocalInstallsCustomPreset(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	promptPath := filepath.Join(t.TempDir(), "reviewer.md")
	if err := os.WriteFile(promptPath, []byte("Review UI changes.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	added, err := AddLocal(ctx, store, "frontend-reviewer", promptPath, "", "")
	if err != nil {
		t.Fatalf("AddLocal returned error: %v", err)
	}

	if added.ID != "frontend-reviewer" || added.Name != "frontend-reviewer" || added.Description != DefaultLocalDescription {
		t.Fatalf("added preset metadata = %+v", added)
	}
	if added.SourceRepo != LocalSourceRepo || added.SourceRef != LocalSourceRef || !filepath.IsAbs(added.SourcePath) {
		t.Fatalf("added preset source = %+v", added)
	}
	if added.ResolvedCommit != HashContent("Review UI changes.\n") || added.Content != "Review UI changes.\n" {
		t.Fatalf("added preset content = %+v", added)
	}
}

func TestAddLocalRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(emptyPath, []byte(" \n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	validPath := filepath.Join(dir, "valid.md")
	if err := os.WriteFile(validPath, []byte("Prompt."), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cases := []struct {
		id   string
		path string
	}{
		{id: "Bad", path: validPath},
		{id: ThermoNuclearCodeQualityReviewID, path: validPath},
		{id: "missing", path: filepath.Join(dir, "missing.md")},
		{id: "directory", path: dir},
		{id: "empty", path: emptyPath},
	}
	for _, tc := range cases {
		if _, err := AddLocal(ctx, store, tc.id, tc.path, "", ""); err == nil {
			t.Fatalf("AddLocal(%q, %q) returned nil", tc.id, tc.path)
		}
	}
}

func TestUpdateLocalRefreshesFromStoredPath(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	promptPath := filepath.Join(t.TempDir(), "reviewer.md")
	if err := os.WriteFile(promptPath, []byte("Old prompt.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	added, err := AddLocal(ctx, store, "frontend-reviewer", promptPath, "Frontend Reviewer", "Reviews UI.")
	if err != nil {
		t.Fatalf("AddLocal returned error: %v", err)
	}
	if err := os.WriteFile(promptPath, []byte("New prompt.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	updated, err := UpdateLocal(ctx, store, added)
	if err != nil {
		t.Fatalf("UpdateLocal returned error: %v", err)
	}

	if updated.Name != "Frontend Reviewer" || updated.Description != "Reviews UI." {
		t.Fatalf("UpdateLocal changed metadata: %+v", updated)
	}
	if updated.Content != "New prompt.\n" || updated.ResolvedCommit != HashContent("New prompt.\n") {
		t.Fatalf("UpdateLocal content = %+v", updated)
	}
}

type fakeRunner struct {
	calls []fakeCall
}

type fakeCall struct {
	command string
	args    []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	f.calls = append(f.calls, fakeCall{command: command, args: append([]string{}, args...)})
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "/git/ref/heads/main"):
		return subprocess.Result{Command: command, Args: args, Stdout: "abc123\n"}, nil
	case strings.Contains(joined, "/contents/"):
		return subprocess.Result{Command: command, Args: args, Stdout: `{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("preset body")) + `"}`}, nil
	default:
		return subprocess.Result{Command: command, Args: args, Stderr: "unexpected call"}, errors.New("unexpected call")
	}
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	return file, nil
}

type fakeFetcher struct {
	commit  string
	content string
}

func (f fakeFetcher) ResolveRef(context.Context, string, string) (string, error) {
	return f.commit, nil
}

func (f fakeFetcher) FetchFile(context.Context, string, string, string) (File, error) {
	return File{Content: f.content}, nil
}
