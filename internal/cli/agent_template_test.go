package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestAgentTemplateUpdateInstallsThermoTemplate(t *testing.T) {
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "Review deeply.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"agent", "template", "update", "--home", home, "thermo-nuclear-code-quality-review"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template update exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated thermo-nuclear-code-quality-review at abc123") {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "thermo-nuclear-code-quality-review"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"installed: yes", "version: v1", "version id: thermo-nuclear-code-quality-review@v1", "promotion state: current", "content hash: sha256:", "resolved commit: abc123", "metadata:", "outputs: review_findings", "Review deeply."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAgentTemplateUpdateRejectsRemovedPlannerHereTemplate(t *testing.T) {
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "fed789",
		content: "Plan quickly.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	removedPlannerHereID := "planner-" + "here"

	code := Run([]string{"agent", "template", "update", "--home", home, removedPlannerHereID}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("template update exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent template "+removedPlannerHereID+" is retired; use planner") {
		t.Fatalf("stderr missing retired planner guidance:\n%s", stderr.String())
	}
}

func TestAgentTemplateDiffDoesNotMutateCachedTemplate(t *testing.T) {
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "old body",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	if code := Run([]string{"agent", "template", "update", "--home", home, "thermo-nuclear-code-quality-review"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template update exit code = %d, stderr=%s", code, stderr.String())
	}

	restore()
	restore = replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "def456",
		content: "new body",
	})
	defer restore()
	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "diff", "--home", home, "thermo-nuclear-code-quality-review"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template diff exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"cached:   abc123", "upstream: def456", "-old body", "+new body"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("diff output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "thermo-nuclear-code-quality-review"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "resolved commit: abc123") || strings.Contains(stdout.String(), "def456") {
		t.Fatalf("diff mutated cached template:\n%s", stdout.String())
	}
}

func TestAgentTemplateListShowsAvailableBuiltin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"agent", "template", "list", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "thermo-nuclear-code-quality-review") || !strings.Contains(stdout.String(), "planner") || !strings.Contains(stdout.String(), "available") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	removedPlannerHereID := "planner-" + "here"
	if strings.Contains(stdout.String(), removedPlannerHereID) {
		t.Fatalf("removed planner id should not be listed as a builtin:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "planner"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"installed: no", "metadata:", "outputs: plan,goal_file", "evaluation:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAgentTemplateAddInstallsLocalCustomTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	content := testLocalTemplateContent("frontend-reviewer", "Review frontend changes.\n")
	if err := os.WriteFile(promptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	code := Run([]string{"agent", "template", "add", "frontend-reviewer",
		"--home", home,
		"--file", promptPath,
		"--name", "Frontend Reviewer",
		"--description", "Reviews UI.",
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added frontend-reviewer at sha256:") {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"name: Frontend Reviewer", "description: Reviews UI.", "source: local@file:", "metadata:", "outputs: response", "installed: yes", "version: v1", "promotion state: current", "content hash: sha256:", "Review frontend changes."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAgentTemplateAddRejectsInvalidLocalFiles(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(emptyPath, []byte("\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	plainPath := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(plainPath, []byte("Review frontend changes.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cases := [][]string{
		{"agent", "template", "add", "--home", home, "frontend-reviewer"},
		{"agent", "template", "add", "--home", home, "--file", filepath.Join(dir, "missing.md"), "frontend-reviewer"},
		{"agent", "template", "add", "--home", home, "--file", dir, "frontend-reviewer"},
		{"agent", "template", "add", "--home", home, "--file", emptyPath, "frontend-reviewer"},
		{"agent", "template", "add", "--home", home, "--file", emptyPath, "Bad"},
		{"agent", "template", "add", "--home", home, "--file", plainPath, "frontend-reviewer"},
	}
	for _, args := range cases {
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, &stdout, &stderr); code == 0 {
			t.Fatalf("Run(%v) exit code = 0, stdout=%s stderr=%s", args, stdout.String(), stderr.String())
		}
	}
}

func TestAgentTemplateAddFromRepoInstallsAndUpdatesRemoteTemplate(t *testing.T) {
	content := testLocalTemplateContent("frontend-reviewer", "Review frontend changes.\n")
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{commit: "remote-sha", content: content})
	defer restore()
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"agent", "template", "add", "frontend-reviewer",
		"--home", home,
		"--from-repo", "jerry/templates",
		"--path", "templates/frontend-reviewer.md",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template add --from-repo exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added frontend-reviewer at remote-sha") {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"source: jerry/templates@main:templates/frontend-reviewer.md", "installed: yes", "Review frontend changes."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout.String())
		}
	}

	// The generalized update path re-fetches a pulled row from its stored source.
	restore()
	restore = replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "remote-sha-2",
		content: testLocalTemplateContent("frontend-reviewer", "Review frontend changes deeply.\n"),
	})
	defer restore()
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "update", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template update exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated frontend-reviewer at remote-sha-2") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "show", "--home", home, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template show after update exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"version: v2", "Review frontend changes deeply."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show after update missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAgentTemplateAddRejectsConflictingSources(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	code := Run([]string{"agent", "template", "add", "frontend-reviewer",
		"--home", home,
		"--file", "ignored.md",
		"--from-repo", "jerry/templates",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("conflicting sources exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "either --file or --from-repo") {
		t.Fatalf("stderr missing conflict guidance:\n%s", stderr.String())
	}
}

// TestAgentTemplateAddWithoutSourceAndNoRemoteIsByteIdenticalError proves the
// off-by-default no-regression invariant for the [template_remote] fallback
// (#476): with NO remote configured, `agent template add <id>` (neither --file
// nor --from-repo) must still hard-error with the documented guidance and exit
// 2, exactly as before the fallback existed. It also exercises
// configuredTemplateRemote returning not-configured for a fresh home.
func TestAgentTemplateAddWithoutSourceAndNoRemoteIsByteIdenticalError(t *testing.T) {
	// Fail loudly if the fallback ever silently reaches the network when no
	// remote is configured.
	restore := replaceAgentTemplateFetcher(explodingFetcher{t: t})
	defer restore()
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	if _, ok := configuredTemplateRemote(home); ok {
		t.Fatalf("configuredTemplateRemote on a fresh home should report not-configured")
	}

	code := Run([]string{"agent", "template", "add", "--home", home, "someid"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("add with no source and no remote exit code = %d (want 2), stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires --file or --from-repo") {
		t.Fatalf("stderr missing flag-required guidance:\n%s", stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Fatalf("add with no source should not print to stdout:\n%s", stdout.String())
	}
}

// TestAgentTemplateAddFallsBackToConfiguredRemote proves the additive fallback
// branch (#476): after `remote set jerry/templates --path agents`, a bare
// `agent template add frontend-reviewer` (no --file/--from-repo) fetches
// agents/frontend-reviewer.md from the configured remote and installs it,
// confirming the fallback wires repo/ref/path correctly.
func TestAgentTemplateAddFallsBackToConfiguredRemote(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	if code := Run([]string{"agent", "template", "remote", "set", "jerry/templates", "--home", home, "--path", "agents"}, &stdout, &stderr); code != 0 {
		t.Fatalf("remote set exit code = %d, stderr=%s", code, stderr.String())
	}

	recorder := &recordingAgentTemplateFetcher{
		commit:  "remote-sha",
		content: testLocalTemplateContent("frontend-reviewer", "Review frontend changes.\n"),
	}
	restore := replaceAgentTemplateFetcher(recorder)
	defer restore()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "add", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add via configured remote exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added frontend-reviewer at remote-sha") {
		t.Fatalf("add via configured remote stdout = %s", stdout.String())
	}
	if recorder.fetchedRepo != "jerry/templates" {
		t.Fatalf("fallback fetched repo = %q, want jerry/templates", recorder.fetchedRepo)
	}
	if recorder.fetchedPath != "agents/frontend-reviewer.md" {
		t.Fatalf("fallback fetched path = %q, want agents/frontend-reviewer.md", recorder.fetchedPath)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "show", "--home", home, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"source: jerry/templates@main:agents/frontend-reviewer.md", "installed: yes", "Review frontend changes."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAgentTemplateExportWritesCustomTemplatesAndSkipsBuiltins(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "Review frontend changes.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", promptPath, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	// Install a built-in too, to prove --all skips it.
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{commit: "abc123", content: "Review deeply."})
	defer restore()
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "update", "--home", home, "thermo-nuclear-code-quality-review"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template update builtin exit code = %d, stderr=%s", code, stderr.String())
	}

	outDir := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "export", "--home", home, "--all", "--to", outDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template export exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "exported frontend-reviewer ->") {
		t.Fatalf("export stdout = %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "thermo-nuclear-code-quality-review") {
		t.Fatalf("export --all should skip built-ins:\n%s", stdout.String())
	}
	exported, err := os.ReadFile(filepath.Join(outDir, "frontend-reviewer.md"))
	if err != nil {
		t.Fatalf("ReadFile exported template returned error: %v", err)
	}
	parsed, err := agenttemplate.ParseTemplateContent(string(exported))
	if err != nil {
		t.Fatalf("exported file did not parse: %v\n%s", err, string(exported))
	}
	if parsed.Metadata.ID != "frontend-reviewer" || !strings.Contains(parsed.Body, "Review frontend changes.") {
		t.Fatalf("exported file = %+v body=%q", parsed.Metadata, parsed.Body)
	}
	if _, err := os.Stat(filepath.Join(outDir, "thermo-nuclear-code-quality-review.md")); !os.IsNotExist(err) {
		t.Fatalf("built-in should not be exported, stat err=%v", err)
	}
}

func TestAgentTemplateExportDryRunWritesNothing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "Review frontend changes.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", promptPath, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	outDir := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "export", "frontend-reviewer", "--home", home, "--to", outDir, "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template export --dry-run exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "would export frontend-reviewer ->") {
		t.Fatalf("dry-run stdout = %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(outDir, "frontend-reviewer.md")); !os.IsNotExist(err) {
		t.Fatalf("--dry-run should not write files, stat err=%v", err)
	}
}

func TestAgentTemplateExportRequiresScope(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	code := Run([]string{"agent", "template", "export", "--home", home}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("export without scope exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires one or more template ids or --all") {
		t.Fatalf("stderr missing scope guidance:\n%s", stderr.String())
	}
}

func TestAgentTemplateLocalUpdatePathUnchangedByGeneralization(t *testing.T) {
	// Additive invariant: a local custom row (SourceRepo "local") must still flow
	// through UpdateLocal — never the remote fetch path — so installing the
	// remote machinery does not change behavior for existing local templates.
	// Set a fetcher that fails loudly if it is ever called for a local row.
	restore := replaceAgentTemplateFetcher(explodingFetcher{t: t})
	defer restore()
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "Old prompt.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", promptPath, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "New prompt.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "update", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("local template update exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated frontend-reviewer at sha256:") {
		t.Fatalf("local update did not use UpdateLocal:\n%s", stdout.String())
	}
}

func TestAgentTemplateDraftWritesDefaultTemplatePath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cwd := chdirTemp(t)

	code := Run([]string{"agent", "template", "draft", "release-planner"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template draft exit code = %d, stderr=%s", code, stderr.String())
	}
	path := filepath.Join(cwd, ".gitmoot", "templates", "release-planner.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	for _, want := range []string{"# Release Planner", "## Role", "## Non-Goals"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("draft missing %q:\n%s", want, string(content))
		}
	}
	for _, want := range []string{"id: release-planner", "kind: agent-template", "runtime_compatibility:"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("draft frontmatter missing %q:\n%s", want, string(content))
		}
	}
	if !strings.Contains(stdout.String(), "drafted release-planner at "+filepath.Join(".gitmoot", "templates", "release-planner.md")) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestAgentTemplateDraftOutputOverwriteRules(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "custom.md")

	code := Run([]string{"agent", "template", "draft", "custom-reviewer", "--output", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template draft exit code = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "draft", "custom-reviewer", "--output", path}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("template draft overwrite exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "already exists; pass --force") {
		t.Fatalf("stderr missing overwrite guidance:\n%s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "draft", "custom-reviewer", "--output", path, "--force"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template draft --force exit code = %d, stderr=%s", code, stderr.String())
	}
}

func TestAgentTemplateValidateAcceptsDraftedTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "planner.md")
	if code := Run([]string{"agent", "template", "draft", "planner-reviewer", "--output", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("template draft exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "validate", path}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template validate exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "valid agent template: "+path) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestAgentTemplateValidateRejectsIncompleteTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "bad.md")
	content, err := agenttemplate.DraftCaptureTemplate("bad")
	if err != nil {
		t.Fatalf("DraftCaptureTemplate returned error: %v", err)
	}
	content = strings.Replace(content, "## When To Use\n\n", "## Missing Section\n\n", 1)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	code := Run([]string{"agent", "template", "validate", path}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("template validate exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing required section") {
		t.Fatalf("stderr missing validation detail:\n%s", stderr.String())
	}
}

func TestAgentTemplateValidateRejectsNonRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket fixture is not portable to windows")
	}
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "template.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	defer listener.Close()

	code := Run([]string{"agent", "template", "validate", path}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("template validate exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "is not a regular file") {
		t.Fatalf("stderr missing non-regular-file detail:\n%s", stderr.String())
	}
}

func TestAgentTemplateListShowsInstalledCustomTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "Review frontend changes.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	apiPromptPath := filepath.Join(t.TempDir(), "api.md")
	if err := os.WriteFile(apiPromptPath, []byte(testLocalTemplateContent("api-reviewer", "Review API changes.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", promptPath, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", apiPromptPath, "api-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "list", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template list exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"thermo-nuclear-code-quality-review", "api-reviewer", "frontend-reviewer", "installed@sha256:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("list output missing %q:\n%s", want, stdout.String())
		}
	}
	if strings.Index(stdout.String(), "api-reviewer") > strings.Index(stdout.String(), "frontend-reviewer") {
		t.Fatalf("custom agent templates are not sorted:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "list", "--home", home, "--output", "goal_file", "--runtime", "codex"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template list filter exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "planner") || strings.Contains(stdout.String(), "frontend-reviewer") || strings.Contains(stdout.String(), "thermo-nuclear-code-quality-review") {
		t.Fatalf("goal_file filter output =\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "list", "--home", home, "--output", "response", "--tag", "review"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template list custom filter exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "api-reviewer") || !strings.Contains(stdout.String(), "frontend-reviewer") || strings.Contains(stdout.String(), "planner") {
		t.Fatalf("response filter output =\n%s", stdout.String())
	}
}

func TestAgentTemplateListFiltersMigratedFrontmatterTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()

	seedCachedAgentTemplate(t, home, db.AgentTemplate{
		ID:             "migrated-reviewer",
		Name:           "Migrated Reviewer",
		Description:    "Migrated custom template",
		SourceRepo:     "local",
		SourceRef:      "file",
		SourcePath:     "/tmp/migrated-reviewer.md",
		ResolvedCommit: "sha256:abc123",
		Content:        testLocalTemplateContent("migrated-reviewer", "Review migrated changes.\n"),
	})

	code := Run([]string{"agent", "template", "list", "--home", home, "--output", "response", "--runtime", "codex"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "migrated-reviewer") {
		t.Fatalf("migrated template missing from filtered list:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "migrated-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "metadata:") || !strings.Contains(stdout.String(), "outputs: response") {
		t.Fatalf("migrated template metadata missing from show:\n%s", stdout.String())
	}
}

func TestAgentTemplateUpdateAndDiffLocalCustomTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "Old prompt.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", promptPath, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "New prompt.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "diff", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template diff exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"cached:   sha256:", "upstream: sha256:", "-Old prompt.", "+New prompt."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("diff output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Old prompt.") || strings.Contains(stdout.String(), "New prompt.") {
		t.Fatalf("diff mutated cached template:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "update", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template update exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated frontend-reviewer at sha256:") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show after update exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"version: v2", "version id: frontend-reviewer@v2", "promotion state: current", "New prompt."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show after update missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAgentTemplateDiffReportsExactTrailingNewlineChanges(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	content := testLocalTemplateContent("frontend-reviewer", "Prompt.\n")
	if err := os.WriteFile(promptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "frontend-reviewer", "--home", home, "--file", promptPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	if err := os.WriteFile(promptPath, []byte(content+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "diff", "--home", home, "frontend-reviewer"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template diff exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "template content is up to date") || !strings.Contains(stdout.String(), "+++ upstream") {
		t.Fatalf("diff did not report exact newline change:\n%s", stdout.String())
	}
}

func TestAgentPromptPrintsCustomTemplateContent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "review-fe.md")
	content := testLocalTemplateContent("review-fe", "Review frontend changes.\n")
	if err := os.WriteFile(promptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "review-fe", "--home", home, "--file", promptPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "prompt", "--home", home, "review-fe"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("agent prompt exit code = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != "Review frontend changes.\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestAgentPromptResolvesRegisteredAgentBeforeTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	templateDir := t.TempDir()
	reviewPath := filepath.Join(templateDir, "review-fe.md")
	reviewContent := testLocalTemplateContent("review-fe", "Review frontend changes.\n")
	if err := os.WriteFile(reviewPath, []byte(reviewContent), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	clashPath := filepath.Join(templateDir, "clash.md")
	if err := os.WriteFile(clashPath, []byte(testLocalTemplateContent("clash", "Wrong direct template.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "review-fe", "--home", home, "--file", reviewPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add review-fe exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "add", "clash", "--home", home, "--file", clashPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add clash exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "subscribe", "clash",
		"--home", home,
		"--runtime", "shell",
		"--session", "cat",
		"--role", "reviewer",
		"--template", "review-fe",
		"--capability", "ask",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "prompt", "--home", home, "clash"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent prompt exit code = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != "Review frontend changes.\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestAgentPromptJSONIncludesAgentMetadata(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "review-fe.md")
	content := testLocalTemplateContent("review-fe", "Review frontend changes.\n")
	if err := os.WriteFile(promptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "review-fe", "--home", home, "--file", promptPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "subscribe", "review-fe-agent",
		"--home", home,
		"--runtime", "shell",
		"--session", "cat",
		"--role", "reviewer",
		"--template", "review-fe",
		"--capability", "ask",
		"--capability", "review",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "prompt", "review-fe-agent", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent prompt --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var output agentPromptOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("prompt JSON did not parse: %v\n%s", err, stdout.String())
	}
	if output.Kind != "agent" || output.Name != "review-fe-agent" || output.TemplateID != "review-fe" || output.Runtime != "shell" || output.Role != "reviewer" {
		t.Fatalf("prompt JSON metadata = %+v", output)
	}
	if strings.Join(output.Capabilities, ",") != "ask,review" || output.Content != "Review frontend changes." || !strings.HasPrefix(output.ResolvedCommit, "sha256:") {
		t.Fatalf("prompt JSON content = %+v", output)
	}
}

func TestAgentPromptReportsMissingTemplateGuidance(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()

	code := Run([]string{"agent", "prompt", "--home", home, "planner"}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("agent prompt exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent template planner is not installed; run gitmoot agent template update planner") {
		t.Fatalf("stderr missing planner update guidance:\n%s", stderr.String())
	}
}

func TestAgentPromptDoesNotInitializeMissingHome(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"agent", "prompt", "--home", home, "planner"}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("agent prompt exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Gitmoot state is not initialized") || !strings.Contains(stderr.String(), "run gitmoot init first") {
		t.Fatalf("stderr missing initialization guidance:\n%s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".gitmoot")); !os.IsNotExist(err) {
		t.Fatalf("agent prompt initialized Gitmoot home, stat err=%v", err)
	}
}

func TestRetiredPlannerHereTemplateIsHiddenAndBlocked(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	retiredID := "planner-" + "here"
	seedCachedAgentTemplate(t, home, db.AgentTemplate{
		ID:             retiredID,
		Name:           "Retired Planner",
		Description:    "Old cached builtin",
		SourceRepo:     "jerryfane/gitmoot",
		SourceRef:      "main",
		SourcePath:     "skills/gitmoot/agent-templates/" + retiredID + ".md",
		ResolvedCommit: "old",
		Content:        "Old planner prompt.\n",
	})

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("template list exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), retiredID) {
		t.Fatalf("retired template should be hidden from list:\n%s", stdout.String())
	}

	for _, args := range [][]string{
		{"agent", "template", "show", "--home", home, retiredID},
		{"agent", "template", "diff", "--home", home, retiredID},
		{"agent", "template", "update", "--home", home, retiredID},
		{"agent", "prompt", "--home", home, retiredID},
	} {
		stdout.Reset()
		stderr.Reset()
		code := Run(args, &stdout, &stderr)
		if code == 0 {
			t.Fatalf("%v exit code = 0, stdout=%s stderr=%s", args, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "agent template "+retiredID+" is retired; use planner") {
			t.Fatalf("%v stderr missing retired guidance:\n%s", args, stderr.String())
		}
	}

	promptPath := filepath.Join(t.TempDir(), "retired.md")
	if err := os.WriteFile(promptPath, []byte("Do not revive.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "template", "add", retiredID, "--home", home, "--file", promptPath}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("template add retired exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent template "+retiredID+" is retired; use planner") {
		t.Fatalf("add retired stderr missing guidance:\n%s", stderr.String())
	}
}

func seedCachedAgentTemplate(t *testing.T, home string, template db.AgentTemplate) {
	t.Helper()
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(context.Background(), template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
}

func testLocalTemplateContent(id string, body string) string {
	return agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
		ID:                   id,
		Name:                 testTemplateName(id),
		Description:          "Reviews UI.",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask"},
		RuntimeCompatibility: []string{"codex", "claude"},
		Tags:                 []string{"review"},
		Inputs:               []string{"repo"},
		Outputs:              []string{"response"},
	}, body)
}

func testTemplateName(id string) string {
	parts := strings.Split(id, "-")
	for index, part := range parts {
		if part == "" {
			continue
		}
		parts[index] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func replaceAgentTemplateFetcher(fetcher agenttemplate.Fetcher) func() {
	previous := newAgentTemplateFetcher
	newAgentTemplateFetcher = func() agenttemplate.Fetcher {
		return fetcher
	}
	return func() {
		newAgentTemplateFetcher = previous
	}
}

func replaceSkillOptTrainInitInteractive(interactive bool) func() {
	previous := skillOptTrainInitInteractive
	active := true
	skillOptTrainInitInteractive = func() bool {
		return active && interactive
	}
	return func() {
		active = false
		skillOptTrainInitInteractive = previous
	}
}

func replaceSkillOptTrainInitStdin(input string) func() {
	previous := skillOptTrainInitStdin
	skillOptTrainInitStdin = func() io.Reader {
		return strings.NewReader(input)
	}
	return func() {
		skillOptTrainInitStdin = previous
	}
}

type explodingFetcher struct {
	t *testing.T
}

func (f explodingFetcher) ResolveRef(context.Context, string, string) (string, error) {
	f.t.Fatal("fetcher ResolveRef called for a local custom template")
	return "", nil
}

func (f explodingFetcher) FetchFile(context.Context, string, string, string) (agenttemplate.File, error) {
	f.t.Fatal("fetcher FetchFile called for a local custom template")
	return agenttemplate.File{}, nil
}

type fakeAgentTemplateFetcher struct {
	commit  string
	content string
}

// recordingAgentTemplateFetcher serves content like fakeAgentTemplateFetcher but
// records the repo/ref/path it was asked to fetch, so tests can assert the caller
// wired the source correctly.
type recordingAgentTemplateFetcher struct {
	commit      string
	content     string
	fetchedRepo string
	fetchedRef  string
	fetchedPath string
}

func (f *recordingAgentTemplateFetcher) ResolveRef(_ context.Context, repo string, ref string) (string, error) {
	f.fetchedRepo = repo
	f.fetchedRef = ref
	return f.commit, nil
}

func (f *recordingAgentTemplateFetcher) FetchFile(_ context.Context, repo string, _ string, path string) (agenttemplate.File, error) {
	f.fetchedRepo = repo
	f.fetchedPath = path
	return agenttemplate.File{Content: f.content}, nil
}

func (f fakeAgentTemplateFetcher) ResolveRef(context.Context, string, string) (string, error) {
	return f.commit, nil
}

func (f fakeAgentTemplateFetcher) FetchFile(context.Context, string, string, string) (agenttemplate.File, error) {
	return agenttemplate.File{Content: f.content}, nil
}

func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd returned error: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore Chdir returned error: %v", err)
		}
	})
	return dir
}
