package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
)

func TestAgentTemplateUpdateInstallsThermoPreset(t *testing.T) {
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
	for _, want := range []string{"installed: yes", "resolved commit: abc123", "Review deeply."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAgentTemplateUpdateInstallsLitePlannerPreset(t *testing.T) {
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "fed789",
		content: "Plan quickly.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"agent", "template", "update", "--home", home, "planner-here"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template update exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated planner-here at fed789") {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "template", "show", "--home", home, "planner-here"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"name: Gitmoot Planner Here", "default role: planner", "default capabilities: ask", "mutation: false", "Plan quickly."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAgentTemplateDiffDoesNotMutateCachedPreset(t *testing.T) {
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

	code := Run([]string{"agent", "template", "list", "--home", t.TempDir()}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("template list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "thermo-nuclear-code-quality-review") || !strings.Contains(stdout.String(), "planner") || !strings.Contains(stdout.String(), "planner-here") || !strings.Contains(stdout.String(), "available") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestAgentTemplateAddInstallsLocalCustomPreset(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte("Review frontend changes.\n"), 0o600); err != nil {
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
	for _, want := range []string{"name: Frontend Reviewer", "description: Reviews UI.", "source: local@file:", "installed: yes", "Review frontend changes."} {
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

	cases := [][]string{
		{"agent", "template", "add", "--home", home, "frontend-reviewer"},
		{"agent", "template", "add", "--home", home, "--file", filepath.Join(dir, "missing.md"), "frontend-reviewer"},
		{"agent", "template", "add", "--home", home, "--file", dir, "frontend-reviewer"},
		{"agent", "template", "add", "--home", home, "--file", emptyPath, "frontend-reviewer"},
		{"agent", "template", "add", "--home", home, "--file", emptyPath, "Bad"},
	}
	for _, args := range cases {
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, &stdout, &stderr); code == 0 {
			t.Fatalf("Run(%v) exit code = 0, stdout=%s stderr=%s", args, stdout.String(), stderr.String())
		}
	}
}

func TestAgentTemplateListShowsInstalledCustomPreset(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte("Review frontend changes.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	apiPromptPath := filepath.Join(t.TempDir(), "api.md")
	if err := os.WriteFile(apiPromptPath, []byte("Review API changes.\n"), 0o600); err != nil {
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
}

func TestAgentTemplateUpdateAndDiffLocalCustomPreset(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte("Old prompt.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", promptPath, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	if err := os.WriteFile(promptPath, []byte("New prompt.\n"), 0o600); err != nil {
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
}

func TestAgentTemplateDiffReportsExactTrailingNewlineChanges(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte("Prompt.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "frontend-reviewer", "--home", home, "--file", promptPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	if err := os.WriteFile(promptPath, []byte("Prompt.\n\n"), 0o600); err != nil {
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

func replaceAgentTemplateFetcher(fetcher agenttemplate.Fetcher) func() {
	previous := newAgentTemplateFetcher
	newAgentTemplateFetcher = func() agenttemplate.Fetcher {
		return fetcher
	}
	return func() {
		newAgentTemplateFetcher = previous
	}
}

type fakeAgentTemplateFetcher struct {
	commit  string
	content string
}

func (f fakeAgentTemplateFetcher) ResolveRef(context.Context, string, string) (string, error) {
	return f.commit, nil
}

func (f fakeAgentTemplateFetcher) FetchFile(context.Context, string, string, string) (agenttemplate.File, error) {
	return agenttemplate.File{Content: f.content}, nil
}
