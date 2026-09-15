package skills

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
		path := filepath.Join(append([]string{"..", "skills", "gitmoot"}, strings.Split(ref, "/")...)...)
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

func TestSkillDocumentsCurrentCLIFamilies(t *testing.T) {
	canonical := readRepoFile(t, "skills", "gitmoot", "SKILL.md")
	root := readRepoFile(t, "SKILL.md")
	cli := readRepoFile(t, "skills", "gitmoot", "references", "CLI.md")
	for _, check := range []struct {
		name string
		text string
		want []string
	}{
		{
			name: "canonical skill",
			text: canonical,
			want: []string{
				"gitmoot --help",
				"gitmoot runtime list",
				"gitmoot memory list",
				"gitmoot pipeline add <spec.yaml>",
				"gitmoot job answer <job-id>",
				"gitmoot review request --pr <number>",
				"gitmoot router summary",
				"gitmoot job gates <id>",
				"gitmoot workflow list",
				"gitmoot dashboard --web",
			},
		},
		{
			name: "root skill",
			text: root,
			want: []string{
				"gitmoot --help",
				"gitmoot runtime list",
				"gitmoot memory ingest",
				"gitmoot memory observations",
				"gitmoot memory confirm",
				"gitmoot memory groom",
				"gitmoot pipeline add",
				"gitmoot job answer <job-id>",
				"gitmoot router summary",
				"gitmoot job open",
				"gitmoot job gates",
				"gitmoot workflow list",
				"gitmoot dashboard --web",
			},
		},
		{
			name: "cli reference",
			text: cli,
			want: []string{
				"gitmoot memory ingest",
				"gitmoot memory observations",
				"gitmoot memory confirm",
				"gitmoot memory groom",
				"gitmoot pipeline add",
				"gitmoot job answer <job-id>",
				"gitmoot review request --pr",
				"gitmoot review status --pr",
				"gitmoot router summary",
				"gitmoot workflow list",
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
	path := filepath.Join(append([]string{".."}, parts...)...)
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

// TestReviewDeadlineDocsAreIdenticalInBothTrees is the committed mechanism the
// #2192 review asked for. The two CLI references are NOT copies of each other
// (4973 vs 3814 lines; they address different readers), so whole-file parity
// would be a false rule. What IS duplicated is the review-deadline block, which
// was synced by hand in one shot - and a hand sync is prevented from drifting
// by nobody. This pins the duplicated REGION byte-for-byte, so editing one copy
// fails until the other is updated.
func TestReviewDeadlineDocsAreIdenticalInBothTrees(t *testing.T) {
	const (
		blockStart = "The effective deadline resolves in this order:"
		blockEnd   = "`quiet_kill_after` controls the transcript-silence leg"
	)
	extract := func(label, text string) string {
		start := strings.Index(text, blockStart)
		end := strings.Index(text, blockEnd)
		if start < 0 || end < 0 || end <= start {
			t.Fatalf("%s: review-deadline block not found (start=%d end=%d); if the docs were restructured, update this test's markers deliberately", label, start, end)
		}
		return strings.TrimSpace(text[start:end])
	}
	website := extract("website/docs/reference/cli.md", readRepoFile(t, "website", "docs", "reference", "cli.md"))
	skill := extract("skills/gitmoot/references/CLI.md", readRepoFile(t, "skills", "gitmoot", "references", "CLI.md"))
	if website != skill {
		t.Fatalf("review-deadline docs have drifted between the two trees.\n--- website ---\n%s\n--- skills ---\n%s", website, skill)
	}
	// The facts a reader needs must actually be in the shared block, so a future
	// edit cannot satisfy parity by deleting them from both copies.
	for _, want := range []string{
		"[review_router]",
		"job_timeout_payload_invalid",
		"review_class_deadline_default",
		"must be positive",
		"stored job type is `review`",
	} {
		if !strings.Contains(website, want) {
			t.Errorf("shared review-deadline docs no longer mention %q", want)
		}
	}
}
