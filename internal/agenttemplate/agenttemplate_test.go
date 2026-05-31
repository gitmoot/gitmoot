package agenttemplate

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

func TestBuiltinsIncludesPlannerAndThermoTemplates(t *testing.T) {
	definitions := Builtins()
	if len(definitions) != 2 {
		t.Fatalf("builtin count = %d, want 2", len(definitions))
	}
	thermo, ok := Lookup(ThermoNuclearCodeQualityReviewID)
	if !ok {
		t.Fatal("thermo template missing")
	}
	if thermo.Mutation || !reflect.DeepEqual(thermo.DefaultCapabilities, []string{"ask", "review"}) {
		t.Fatalf("thermo definition = %+v", thermo)
	}
	planner, ok := Lookup(PlannerTemplateID)
	if !ok {
		t.Fatal("planner template missing")
	}
	if !planner.Mutation || planner.DefaultRole != "planner" || !reflect.DeepEqual(planner.DefaultCapabilities, []string{"ask"}) {
		t.Fatalf("planner definition = %+v", planner)
	}
	if planner.SourceRepo != "jerryfane/gitmoot" || planner.SourcePath != "skills/gitmoot/agent-templates/planner.md" {
		t.Fatalf("planner source = %+v", planner)
	}
}

func TestUpdatePlannerTemplate(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	content := testTemplateContent(PlannerTemplateID, "# Planner\n\nPlan carefully.\n")
	updated, err := Update(ctx, store, fakeFetcher{commit: "def456", content: content}, PlannerTemplateID)
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if updated.ID != PlannerTemplateID || updated.ResolvedCommit != "def456" || updated.Content != content {
		t.Fatalf("updated planner template = %+v", updated)
	}
	if updated.SourceRepo != "jerryfane/gitmoot" || updated.SourcePath != "skills/gitmoot/agent-templates/planner.md" {
		t.Fatalf("updated source = %+v", updated)
	}
}

func TestParseTemplateContentRequiresFrontmatter(t *testing.T) {
	content := testTemplateContent("frontend-reviewer", "# Frontend Reviewer\n\nReview UI changes.\n")
	parsed, err := ParseTemplateContent(content)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	if parsed.Metadata.ID != "frontend-reviewer" || parsed.Metadata.Kind != TemplateKind || parsed.Metadata.Version != TemplateVersion {
		t.Fatalf("metadata = %+v", parsed.Metadata)
	}
	if strings.TrimSpace(parsed.Body) != "# Frontend Reviewer\n\nReview UI changes." {
		t.Fatalf("body = %q", parsed.Body)
	}

	cases := map[string]string{
		"missing frontmatter": "# Frontend Reviewer\n\nReview UI changes.\n",
		"missing kind": FormatTemplateContent(Metadata{
			ID:                   "frontend-reviewer",
			Name:                 "Frontend Reviewer",
			Description:          "Reviews UI.",
			Version:              TemplateVersion,
			Capabilities:         []string{"ask"},
			RuntimeCompatibility: []string{"codex"},
			Tags:                 []string{"review"},
			Inputs:               []string{"repo"},
			Outputs:              []string{"response"},
		}, "# Frontend Reviewer\n\nReview UI changes.\n"),
		"invalid capability": FormatTemplateContent(Metadata{
			ID:                   "frontend-reviewer",
			Name:                 "Frontend Reviewer",
			Description:          "Reviews UI.",
			Kind:                 TemplateKind,
			Version:              TemplateVersion,
			Capabilities:         []string{"unknown"},
			RuntimeCompatibility: []string{"codex"},
			Tags:                 []string{"review"},
			Inputs:               []string{"repo"},
			Outputs:              []string{"response"},
		}, "# Frontend Reviewer\n\nReview UI changes.\n"),
		"invalid runtime": FormatTemplateContent(Metadata{
			ID:                   "frontend-reviewer",
			Name:                 "Frontend Reviewer",
			Description:          "Reviews UI.",
			Kind:                 TemplateKind,
			Version:              TemplateVersion,
			Capabilities:         []string{"ask"},
			RuntimeCompatibility: []string{"claude-code"},
			Tags:                 []string{"review"},
			Inputs:               []string{"repo"},
			Outputs:              []string{"response"},
		}, "# Frontend Reviewer\n\nReview UI changes.\n"),
		"empty body": FormatTemplateContent(testMetadata("frontend-reviewer"), ""),
	}
	for name, candidate := range cases {
		if _, err := ParseTemplateContent(candidate); err == nil {
			t.Fatalf("%s: ParseTemplateContent returned nil", name)
		}
	}
}

func TestContentForDefinitionWrapsGenericSkillContent(t *testing.T) {
	definition, ok := Lookup(ThermoNuclearCodeQualityReviewID)
	if !ok {
		t.Fatal("thermo template missing")
	}
	content, err := ContentForDefinition(definition, "# Thermo\n\nReview deeply.\n")
	if err != nil {
		t.Fatalf("ContentForDefinition returned error: %v", err)
	}
	parsed, err := ParseTemplateContent(content)
	if err != nil {
		t.Fatalf("wrapped content did not parse: %v\n%s", err, content)
	}
	if parsed.Metadata.ID != ThermoNuclearCodeQualityReviewID || !strings.Contains(parsed.Body, "Review deeply.") {
		t.Fatalf("wrapped template = %+v body=%q", parsed.Metadata, parsed.Body)
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
	if file.Content != "template body" {
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

func TestAddLocalInstallsCustomTemplate(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	promptPath := filepath.Join(t.TempDir(), "reviewer.md")
	content := testTemplateContent("frontend-reviewer", "# Frontend Reviewer\n\nReview UI changes.\n")
	if err := os.WriteFile(promptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	added, err := AddLocal(ctx, store, "frontend-reviewer", promptPath, "", "")
	if err != nil {
		t.Fatalf("AddLocal returned error: %v", err)
	}

	if added.ID != "frontend-reviewer" || added.Name != "Frontend Reviewer" || added.Description != "Reviews UI." {
		t.Fatalf("added template metadata = %+v", added)
	}
	if added.SourceRepo != LocalSourceRepo || added.SourceRef != LocalSourceRef || !filepath.IsAbs(added.SourcePath) {
		t.Fatalf("added template source = %+v", added)
	}
	if added.ResolvedCommit != HashContent(content) || added.Content != content {
		t.Fatalf("added template content = %+v", added)
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
	if err := os.WriteFile(validPath, []byte(testTemplateContent("valid", "# Valid\n\nPrompt.")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	noFrontmatterPath := filepath.Join(dir, "no-frontmatter.md")
	if err := os.WriteFile(noFrontmatterPath, []byte("Prompt."), 0o600); err != nil {
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
		{id: "no-frontmatter", path: noFrontmatterPath},
		{id: "mismatch", path: validPath},
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
	oldContent := testTemplateContent("frontend-reviewer", "# Frontend Reviewer\n\nOld prompt.\n")
	if err := os.WriteFile(promptPath, []byte(oldContent), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	added, err := AddLocal(ctx, store, "frontend-reviewer", promptPath, "Frontend Reviewer", "Reviews UI.")
	if err != nil {
		t.Fatalf("AddLocal returned error: %v", err)
	}
	newContent := FormatTemplateContent(Metadata{
		ID:                   "frontend-reviewer",
		Name:                 "Frontend Review Lead",
		Description:          "Reviews frontend behavior and polish.",
		Kind:                 TemplateKind,
		Version:              TemplateVersion,
		Capabilities:         []string{"ask"},
		RuntimeCompatibility: []string{"codex", "claude"},
		Tags:                 []string{"review"},
		Inputs:               []string{"repo"},
		Outputs:              []string{"response"},
	}, "# Frontend Review Lead\n\nNew prompt.\n")
	if err := os.WriteFile(promptPath, []byte(newContent), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	updated, err := UpdateLocal(ctx, store, added)
	if err != nil {
		t.Fatalf("UpdateLocal returned error: %v", err)
	}

	if updated.Name != "Frontend Review Lead" || updated.Description != "Reviews frontend behavior and polish." {
		t.Fatalf("UpdateLocal metadata = %+v", updated)
	}
	if updated.Content != newContent || updated.ResolvedCommit != HashContent(newContent) {
		t.Fatalf("UpdateLocal content = %+v", updated)
	}
}

func testTemplateContent(id string, body string) string {
	return FormatTemplateContent(testMetadata(id), body)
}

func testMetadata(id string) Metadata {
	return Metadata{
		ID:                   id,
		Name:                 titleFromID(id),
		Description:          "Reviews UI.",
		Kind:                 TemplateKind,
		Version:              TemplateVersion,
		Capabilities:         []string{"ask"},
		RuntimeCompatibility: []string{"codex", "claude"},
		Tags:                 []string{"review"},
		Inputs:               []string{"repo"},
		Outputs:              []string{"response"},
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
		return subprocess.Result{Command: command, Args: args, Stdout: `{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("template body")) + `"}`}, nil
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
