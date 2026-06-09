package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalSkillFrontmatter(t *testing.T) {
	text := readRepoFile(t, "skills", "gitmoot", "SKILL.md")
	frontmatter := parseFrontmatter(t, text)

	for _, want := range []string{
		"name: gitmoot",
		"description: Use Gitmoot",
		"license: Apache-2.0",
		"compatibility:",
		"metadata:",
		"gitmoot-version:",
	} {
		if !strings.Contains(frontmatter, want) {
			t.Fatalf("frontmatter missing %q:\n%s", want, frontmatter)
		}
	}

	for _, want := range []string{
		"GitHub PR comments",
		"agent subscriptions",
		"daemon checks",
		"jobs",
		"branch locks",
		"agent-templates",
		"template capture",
		"custom prompt agents",
		"Codex",
		"Claude Code",
	} {
		if !strings.Contains(frontmatter, want) {
			t.Fatalf("description missing trigger term %q:\n%s", want, frontmatter)
		}
	}
}

func TestCanonicalSkillReferencesExist(t *testing.T) {
	text := readRepoFile(t, "skills", "gitmoot", "SKILL.md")
	for _, ref := range []string{
		"references/CLI.md",
		"references/WORKFLOWS.md",
		"references/TEMPLATE_CAPTURE.md",
		"references/GOAL_TEMPLATE.md",
		"references/RESULT_CONTRACT.md",
		"references/SAFETY.md",
	} {
		if !strings.Contains(text, ref) {
			t.Fatalf("canonical SKILL.md missing reference %q", ref)
		}
		path := filepath.Join(append([]string{"..", "..", "skills", "gitmoot"}, strings.Split(ref, "/")...)...)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("referenced file %s missing: %v", ref, err)
		}
	}
}

func TestCanonicalSkillDocumentsResultAndRereadGuidance(t *testing.T) {
	text := readRepoFile(t, "skills", "gitmoot", "SKILL.md")
	for _, want := range []string{
		"gitmoot_result",
		"blocked",
		"failed",
		"branch locks",
		"runtime session locks",
		"--workers 1",
		"Reread this `SKILL.md`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("canonical SKILL.md missing %q", want)
		}
	}
}

func TestCanonicalSkillDocumentsLocalAgentAsk(t *testing.T) {
	text := readRepoFile(t, "skills", "gitmoot", "SKILL.md")
	cli := readRepoFile(t, "skills", "gitmoot", "references", "CLI.md")
	workflows := readRepoFile(t, "skills", "gitmoot", "references", "WORKFLOWS.md")
	for _, check := range []struct {
		name string
		text string
		want []string
	}{
		{
			name: "skill",
			text: text,
			want: []string{
				"gitmoot agent run <agent>",
				"gitmoot agent ask <agent>",
				"The plugin is only the runtime discovery surface",
				"gitmoot agent prompt <agent-or-template>",
				"TEMPLATE_CAPTURE.md",
			},
		},
		{
			name: "cli",
			text: cli,
			want: []string{
				"gitmoot agent run project-planner --repo owner/repo",
				"gitmoot agent ask project-planner --repo owner/repo",
				"gitmoot agent prompt frontend-reviewer",
				"agent ask` is for analysis, planning, and questions only",
				"gitmoot agent type set planner",
				"gitmoot job watch <job-id>",
			},
		},
		{
			name: "workflows",
			text: workflows,
			want: []string{
				"gitmoot agent ask project-planner --repo owner/repo",
				"Current-Chat Custom Agent Prompt",
				"Execution Model",
				"runtime:<runtime>:<runtime_ref>",
			},
		},
	} {
		for _, want := range check.want {
			if !strings.Contains(check.text, want) {
				t.Fatalf("%s missing %q", check.name, want)
			}
		}
	}
}

func TestCanonicalSkillDocumentsTemplateCapture(t *testing.T) {
	text := readRepoFile(t, "skills", "gitmoot", "SKILL.md")
	capture := readRepoFile(t, "skills", "gitmoot", "references", "TEMPLATE_CAPTURE.md")
	for _, want := range []string{
		"capture this session as a Gitmoot agent",
		"template\", \"turn this workflow into a Gitmoot template\"",
		"draft a reusable",
		"agent template from this chat",
		"cannot read hidden model memory",
		"Do not install, overwrite,",
		"or update a permanent template",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("canonical SKILL.md missing template capture guidance %q", want)
		}
	}
	for _, want := range []string{
		"Template capture is current-chat distillation",
		"Draft Structure",
		"## Role",
		"## When To Use",
		"## Workflow",
		"## Inputs And Context",
		"## Commands And Tools",
		"## Output Contract",
		"## Safety Rules",
		"## Examples",
		"## Non-Goals",
		"gitmoot agent template add",
	} {
		if !strings.Contains(capture, want) {
			t.Fatalf("TEMPLATE_CAPTURE.md missing %q", want)
		}
	}
}

func TestRootSkillCompatibilityEntrypoint(t *testing.T) {
	text := readRepoFile(t, "SKILL.md")
	frontmatter := parseFrontmatter(t, text)
	for _, want := range []string{
		"name: gitmoot",
		"description: Use Gitmoot",
		"version: 0.1.0",
		"openclaw:",
		"requires:",
		"bins:",
		"- gitmoot",
		"- git",
		"- gh",
		"envVars:",
		"- name: GH_TOKEN",
		"required: false",
	} {
		if !strings.Contains(frontmatter, want) {
			t.Fatalf("root SKILL.md frontmatter missing %q:\n%s", want, frontmatter)
		}
	}
	if strings.Contains(frontmatter, "requires:\n      env:") || strings.Contains(frontmatter, "requires.env") {
		t.Fatalf("optional GH_TOKEN must not be declared as required env:\n%s", frontmatter)
	}
	for _, want := range []string{
		"skills/gitmoot/",
		"gitmoot agent prompt <agent-or-template>",
		"gitmoot.io/SKILL.md",
		"gitmoot_result",
		"template capture",
		"branch locks",
		"runtime session locks",
		"gitmoot agent ask <agent>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("root SKILL.md missing compatibility content %q", want)
		}
	}
}

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(contents)
}

func parseFrontmatter(t *testing.T, text string) string {
	t.Helper()
	if !strings.HasPrefix(text, "---\n") {
		t.Fatal("SKILL.md missing YAML frontmatter opener")
	}
	parts := strings.SplitN(text, "---\n", 3)
	if len(parts) != 3 {
		t.Fatal("SKILL.md missing YAML frontmatter closer")
	}
	return parts[1]
}
