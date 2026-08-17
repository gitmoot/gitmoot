package agenttemplate

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/skills"
)

func TestBuiltinsIncludesPlannerAndThermoTemplates(t *testing.T) {
	definitions := Builtins()
	if len(definitions) != 5 {
		t.Fatalf("builtin count = %d, want 5", len(definitions))
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
	if planner.SourceRepo != "gitmoot/gitmoot" || planner.SourcePath != "skills/gitmoot/agent-templates/planner.md" {
		t.Fatalf("planner source = %+v", planner)
	}
	reviewPanel, ok := Lookup(ReviewPanelTemplateID)
	if !ok {
		t.Fatal("review-panel template missing")
	}
	if reviewPanel.Mutation || reviewPanel.DefaultRole != "coordinator" || !reflect.DeepEqual(reviewPanel.DefaultCapabilities, []string{"ask", "review"}) {
		t.Fatalf("review-panel definition = %+v", reviewPanel)
	}
	if reviewPanel.SourceRepo != "gitmoot/gitmoot" || reviewPanel.SourcePath != "skills/gitmoot/agent-templates/review-panel.md" {
		t.Fatalf("review-panel source = %+v", reviewPanel)
	}
	decompose, ok := Lookup(DecomposeAndVerifyTemplateID)
	if !ok {
		t.Fatal("decompose-and-verify template missing")
	}
	if !decompose.Mutation || decompose.DefaultRole != "coordinator" || !reflect.DeepEqual(decompose.DefaultCapabilities, []string{"ask", "review", "implement"}) {
		t.Fatalf("decompose-and-verify definition = %+v", decompose)
	}
	if decompose.SourceRepo != "gitmoot/gitmoot" || decompose.SourcePath != "skills/gitmoot/agent-templates/decompose-and-verify.md" {
		t.Fatalf("decompose-and-verify source = %+v", decompose)
	}
	verifier, ok := Lookup(VerifierTemplateID)
	if !ok {
		t.Fatal("verifier template missing")
	}
	if !verifier.Mutation || verifier.DefaultRole != "coordinator" || !reflect.DeepEqual(verifier.DefaultCapabilities, []string{"ask", "review", "implement"}) {
		t.Fatalf("verifier definition = %+v", verifier)
	}
	if verifier.SourceRepo != "gitmoot/gitmoot" || verifier.SourcePath != "skills/gitmoot/agent-templates/verifier.md" {
		t.Fatalf("verifier source = %+v", verifier)
	}
}

// TestEmbeddedAgentTemplatesMatchBuiltins is the registry/embed parity gate: an
// agent-template .md present in the embedded skill tree must be registered as a
// built-in (or a documented bootstrap command like `orchestrate <id>` /
// `agent template update <id>` silently fails), and every gitmoot-sourced
// built-in must point at an embedded file that actually exists. Either gap
// fails here so the orphan is CI-visible.
func TestEmbeddedAgentTemplatesMatchBuiltins(t *testing.T) {
	const dir = "gitmoot/agent-templates"
	entries, err := fs.ReadDir(skills.FS, dir)
	if err != nil {
		t.Fatalf("read embedded agent-templates dir: %v", err)
	}

	// Map every gitmoot-sourced built-in by its embedded source path.
	builtinByPath := map[string]Definition{}
	for _, def := range Builtins() {
		if def.SourceRepo != "gitmoot/gitmoot" {
			continue
		}
		if !strings.HasPrefix(def.SourcePath, "skills/"+dir+"/") {
			continue
		}
		builtinByPath[def.SourcePath] = def
	}

	// Every embedded .md template must be registered as a built-in.
	embeddedPaths := map[string]struct{}{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		sourcePath := "skills/" + dir + "/" + entry.Name()
		embeddedPaths[sourcePath] = struct{}{}
		if _, ok := builtinByPath[sourcePath]; !ok {
			t.Errorf("embedded agent template %s is not registered in the builtins slice; "+
				"documented `orchestrate`/`agent template update` commands for it would fail", entry.Name())
		}
	}

	// Every gitmoot-sourced built-in under agent-templates/ must point at an
	// embedded file that exists.
	for path, def := range builtinByPath {
		if _, ok := embeddedPaths[path]; !ok {
			t.Errorf("built-in %s references embedded template %q, but no such file is embedded", def.ID, path)
		}
	}
}

func TestEmbeddedBuiltinTemplatesParseAndValidate(t *testing.T) {
	for _, def := range Builtins() {
		if def.SourceRepo != "gitmoot/gitmoot" {
			continue
		}
		path := strings.TrimPrefix(def.SourcePath, "skills/")
		data, err := fs.ReadFile(skills.FS, path)
		if err != nil {
			t.Fatalf("read embedded template %s: %v", def.ID, err)
		}
		parsed, err := ParseTemplateContent(string(data))
		if err != nil {
			t.Fatalf("template %s did not validate: %v", def.ID, err)
		}
		if parsed.Metadata.ID != def.ID {
			t.Fatalf("template %s metadata id = %q", def.ID, parsed.Metadata.ID)
		}
		if !reflect.DeepEqual(parsed.Metadata.Capabilities, def.DefaultCapabilities) {
			t.Fatalf("template %s capabilities = %v, want %v", def.ID, parsed.Metadata.Capabilities, def.DefaultCapabilities)
		}
		// The MetadataForDefinition fallback (used when a fetched file lacks
		// frontmatter) must agree with the embedded file's frontmatter for the
		// coordinator recipes, or a no-frontmatter fetch would report different
		// tags/inputs/outputs than the template actually declares.
		if def.ID == ReviewPanelTemplateID || def.ID == DecomposeAndVerifyTemplateID || def.ID == VerifierTemplateID {
			fallback := MetadataForDefinition(def)
			if !reflect.DeepEqual(fallback.Tags, parsed.Metadata.Tags) ||
				!reflect.DeepEqual(fallback.Inputs, parsed.Metadata.Inputs) ||
				!reflect.DeepEqual(fallback.Outputs, parsed.Metadata.Outputs) {
				t.Fatalf("template %s: MetadataForDefinition fallback diverges from frontmatter (tags %v/%v inputs %v/%v outputs %v/%v)",
					def.ID, fallback.Tags, parsed.Metadata.Tags, fallback.Inputs, parsed.Metadata.Inputs, fallback.Outputs, parsed.Metadata.Outputs)
			}
		}
	}
}

func TestUpdatePlannerTemplate(t *testing.T) {
	ctx := context.Background()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	content := testTemplateContent(PlannerTemplateID, "# Planner\n\nPlan carefully.\n")
	updated, err := Update(ctx, store, fakeFetcher{commit: "def456", content: content}, PlannerTemplateID)
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if updated.ID != PlannerTemplateID || updated.ResolvedCommit != "def456" || updated.Content != content || !strings.Contains(updated.MetadataJSON, `"id":"planner"`) || !strings.Contains(updated.MetadataJSON, `"outputs":["response"]`) {
		t.Fatalf("updated planner template = %+v", updated)
	}
	if updated.SourceRepo != "gitmoot/gitmoot" || updated.SourcePath != "skills/gitmoot/agent-templates/planner.md" {
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
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
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
	if added.ResolvedCommit != HashContent(content) || added.Content != content || !strings.Contains(added.MetadataJSON, `"id":"frontend-reviewer"`) {
		t.Fatalf("added template content = %+v", added)
	}
}

func TestAddLocalRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
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
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
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
	if updated.Content != newContent || updated.ResolvedCommit != HashContent(newContent) || !strings.Contains(updated.MetadataJSON, `"name":"Frontend Review Lead"`) {
		t.Fatalf("UpdateLocal content = %+v", updated)
	}
}

func TestIsRemoteGatesOnRealOwnerRepo(t *testing.T) {
	remote := []string{"jerry/templates", "owner/repo"}
	for _, repo := range remote {
		if !IsRemoteRepo(repo) {
			t.Fatalf("IsRemoteRepo(%q) = false, want true", repo)
		}
	}
	// The "local" sentinel and malformed values must NOT be treated as remote,
	// so local custom rows keep using UpdateLocal.
	for _, repo := range []string{"", LocalSourceRepo, "noslash", "owner/", "/repo", "owner/repo/extra"} {
		if IsRemoteRepo(repo) {
			t.Fatalf("IsRemoteRepo(%q) = true, want false", repo)
		}
	}
	if IsRemote(db.AgentTemplate{SourceRepo: LocalSourceRepo, SourceRef: LocalSourceRef}) {
		t.Fatal("local row reported as remote")
	}
	if !IsRemote(db.AgentTemplate{SourceRepo: "jerry/templates", SourceRef: "main"}) {
		t.Fatal("owner/repo row not reported as remote")
	}
}

func TestAddRemoteInstallsAndUpdatesFetchedTemplate(t *testing.T) {
	ctx := context.Background()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	content := testTemplateContent("frontend-reviewer", "# Frontend Reviewer\n\nReview UI changes.\n")
	added, err := AddRemote(ctx, store, fakeFetcher{commit: "sha-add", content: content}, "frontend-reviewer", "jerry/templates", "main", "templates/frontend-reviewer.md")
	if err != nil {
		t.Fatalf("AddRemote returned error: %v", err)
	}
	if added.SourceRepo != "jerry/templates" || added.SourceRef != "main" || added.SourcePath != "templates/frontend-reviewer.md" {
		t.Fatalf("added remote source = %+v", added)
	}
	if added.ResolvedCommit != "sha-add" || added.Content != content || !strings.Contains(added.MetadataJSON, `"id":"frontend-reviewer"`) {
		t.Fatalf("added remote content = %+v", added)
	}
	if IsLocal(added) || !IsRemote(added) {
		t.Fatalf("added remote row should be remote, not local: %+v", added)
	}

	// A pulled row re-fetches from its stored source via UpdateRemote.
	newContent := testTemplateContent("frontend-reviewer", "# Frontend Reviewer\n\nReview UI changes carefully.\n")
	updated, err := UpdateRemote(ctx, store, fakeFetcher{commit: "sha-upd", content: newContent}, added)
	if err != nil {
		t.Fatalf("UpdateRemote returned error: %v", err)
	}
	if updated.ResolvedCommit != "sha-upd" || updated.Content != newContent {
		t.Fatalf("updated remote row = %+v", updated)
	}
	if updated.SourceRepo != "jerry/templates" || updated.SourcePath != "templates/frontend-reviewer.md" {
		t.Fatalf("updated remote source drifted = %+v", updated)
	}
}

func TestAddRemoteRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	valid := testTemplateContent("frontend-reviewer", "# Frontend Reviewer\n\nReview UI.\n")
	mismatch := testTemplateContent("other-id", "# Other\n\nReview UI.\n")

	cases := []struct {
		name    string
		id      string
		repo    string
		content string
	}{
		{name: "builtin id", id: ThermoNuclearCodeQualityReviewID, repo: "jerry/templates", content: valid},
		{name: "retired id", id: "planner-" + "here", repo: "jerry/templates", content: valid},
		{name: "bad id", id: "Bad", repo: "jerry/templates", content: valid},
		{name: "not owner/repo", id: "frontend-reviewer", repo: "local", content: valid},
		{name: "frontmatter mismatch", id: "frontend-reviewer", repo: "jerry/templates", content: mismatch},
	}
	for _, tc := range cases {
		if _, err := AddRemote(ctx, store, fakeFetcher{commit: "sha", content: tc.content}, tc.id, tc.repo, "main", "templates/x.md"); err == nil {
			t.Fatalf("%s: AddRemote returned nil", tc.name)
		}
	}
}

func TestUpdateRemoteRejectsLocalRow(t *testing.T) {
	ctx := context.Background()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	local := db.AgentTemplate{ID: "frontend-reviewer", SourceRepo: LocalSourceRepo, SourceRef: LocalSourceRef}
	if _, err := UpdateRemote(ctx, store, fakeFetcher{commit: "sha", content: ""}, local); err == nil {
		t.Fatal("UpdateRemote on a local row returned nil")
	}
}

func TestExportReconstructsStoredTemplate(t *testing.T) {
	ctx := context.Background()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	content := testTemplateContent("frontend-reviewer", "# Frontend Reviewer\n\nReview UI changes.\n")
	added, err := AddRemote(ctx, store, fakeFetcher{commit: "sha-add", content: content}, "frontend-reviewer", "jerry/templates", "main", "templates/frontend-reviewer.md")
	if err != nil {
		t.Fatalf("AddRemote returned error: %v", err)
	}
	exported, err := Export(added)
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	// The export must round-trip back through the parser to the same id/body.
	parsed, err := ParseTemplateContent(exported)
	if err != nil {
		t.Fatalf("exported content did not parse: %v\n%s", err, exported)
	}
	if parsed.Metadata.ID != "frontend-reviewer" || !strings.Contains(parsed.Body, "Review UI changes.") {
		t.Fatalf("exported template = %+v body=%q", parsed.Metadata, parsed.Body)
	}
	if exported != content {
		t.Fatalf("export did not round-trip:\n got: %q\nwant: %q", exported, content)
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
