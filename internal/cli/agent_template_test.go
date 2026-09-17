package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
)

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
	// #2205 removed the goal-file surface, so the planner's outputs are
	// plan,tasks in BOTH install states. The subject of this assertion - that
	// `show` reports the built-in's metadata for an uninstalled template -
	// survives; only the value moved, which is why it is re-pinned rather than
	// deleted.
	for _, want := range []string{"installed: no", "metadata:", "outputs: plan,tasks", "evaluation:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout.String())
		}
	}
}

// TestAgentTemplateWriteVerbsAreGone pins the #2204 reduction at the CLI
// boundary: the authoring and distribution verbs are not merely undocumented,
// they are rejected, and the read verbs still work in the same invocation.
func TestAgentTemplateWriteVerbsAreGone(t *testing.T) {
	home := t.TempDir()
	for _, verb := range []string{"add", "export", "publish", "pull", "remote", "draft", "diff", "revert", "update", "validate"} {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"agent", "template", verb, "--home", home, "planner"}, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("agent template %s exit code = %d, want 2 (stdout=%s stderr=%s)", verb, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "unknown agent template command \""+verb+"\"") {
			t.Fatalf("agent template %s stderr = %s", verb, stderr.String())
		}
		if !strings.Contains(stderr.String(), "read-only installed data") {
			t.Fatalf("agent template %s stderr does not explain the read-only surface:\n%s", verb, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "template", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("template list exit code = %d, stderr=%s", code, stderr.String())
	}
}

func TestAgentTemplateListShowsInstalledCustomTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	installLocalTemplate(t, home, "frontend-reviewer", testLocalTemplateContent("frontend-reviewer", "Review frontend changes.\n"))
	installLocalTemplate(t, home, "api-reviewer", testLocalTemplateContent("api-reviewer", "Review API changes.\n"))

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
	// --output plan still selects the planner alone: no other built-in emits it.
	code = Run([]string{"agent", "template", "list", "--home", home, "--output", "plan", "--runtime", "codex"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("template list filter exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "planner") || strings.Contains(stdout.String(), "frontend-reviewer") || strings.Contains(stdout.String(), "thermo-nuclear-code-quality-review") {
		t.Fatalf("plan filter output =\n%s", stdout.String())
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

func TestAgentPromptPrintsCustomTemplateContent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	installLocalTemplate(t, home, "review-fe", testLocalTemplateContent("review-fe", "Review frontend changes.\n"))

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
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	installLocalTemplate(t, home, "review-fe", testLocalTemplateContent("review-fe", "Review frontend changes.\n"))
	installLocalTemplate(t, home, "clash", testLocalTemplateContent("clash", "Wrong direct template.\n"))

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
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	installLocalTemplate(t, home, "review-fe", testLocalTemplateContent("review-fe", "Review frontend changes.\n"))
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
	// #2204 removed every authoring verb, so the guidance can no longer point at
	// one; it must still name the template and say what is missing.
	if !strings.Contains(stderr.String(), "agent template planner is not installed") {
		t.Fatalf("stderr missing planner guidance:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "seed the agent_templates row") {
		t.Fatalf("stderr does not say how to install a template:\n%s", stderr.String())
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
		SourceRepo:     "gitmoot/gitmoot",
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
}

func seedCachedAgentTemplate(t *testing.T, home string, template db.AgentTemplate) {
	t.Helper()
	store, err := dbtest.Open(t, filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(context.Background(), template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
}

// installLocalTemplate seeds one agent_templates row from template content. It
// is how a template gets installed after #2204 removed the authoring verbs:
// straight into the store, exactly as an operator now does it by hand. The
// metadata column is derived from the content's own frontmatter so the row is
// shaped the way the surviving read path (list/show, agent prompt,
// Mailbox.templateSnapshot) expects.
func installLocalTemplate(t *testing.T, home, id, content string) {
	t.Helper()
	parsed, err := agenttemplate.ParseTemplateContent(content)
	if err != nil {
		t.Fatalf("seed template %s: content does not parse: %v", id, err)
	}
	metadataJSON, err := agenttemplate.MarshalMetadata(parsed.Metadata)
	if err != nil {
		t.Fatalf("seed template %s: marshal metadata: %v", id, err)
	}
	sum := sha256.Sum256([]byte(content))
	seedCachedAgentTemplate(t, home, db.AgentTemplate{
		ID:             id,
		Name:           parsed.Metadata.Name,
		Description:    parsed.Metadata.Description,
		SourceRepo:     "local",
		SourceRef:      "file",
		SourcePath:     filepath.Join(t.TempDir(), id+".md"),
		ResolvedCommit: "sha256:" + hex.EncodeToString(sum[:]),
		Content:        content,
		MetadataJSON:   metadataJSON,
	})
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
