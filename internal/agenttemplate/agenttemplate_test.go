package agenttemplate

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/skills"
)

func TestBuiltinsIncludesPlannerAndThermoTemplates(t *testing.T) {
	definitions := Builtins()
	// 2 since #2588 retired the review-panel and verifier coordinator recipes.
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
	if planner.SourceRepo != "gitmoot/gitmoot" || planner.SourcePath != "skills/gitmoot/agent-templates/planner.md" {
		t.Fatalf("planner source = %+v", planner)
	}
	// review-panel, verifier and decompose-and-verify were delegation fan-out
	// recipes; #2588 removed the delegation engine. They must no longer be
	// built-ins, their ids must still be recognised so `agent template add` refuses
	// them by name, and none has a successor.
	for _, id := range []string{"review-panel", "verifier", "decompose-and-verify"} {
		if _, ok := Lookup(id); ok {
			t.Fatalf("%s is retired and must not be a built-in definition", id)
		}
		if !IsRetired(id) {
			t.Fatalf("IsRetired(%s) = false, want true", id)
		}
		if replacement, ok := RetiredReplacement(id); ok || replacement != "" {
			t.Fatalf("RetiredReplacement(%s) = %q, %v; want no successor", id, replacement, ok)
		}
	}
}

// TestEmbeddedAgentTemplatesMatchBuiltins is the registry/embed parity gate: an
// agent-template .md present in the embedded skill tree must be registered as a
// built-in (or a documented bootstrap command like `orchestrate <id>` silently
// fails), and every gitmoot-sourced built-in must point at an embedded file that
// actually exists. Either gap fails here so the orphan is CI-visible.
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
				"documented `orchestrate` commands for it would fail", entry.Name())
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

func testTemplateContent(id string, body string) string {
	return FormatTemplateContent(testMetadata(id), body)
}

func testMetadata(id string) Metadata {
	return Metadata{
		ID:                   id,
		Name:                 id,
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
