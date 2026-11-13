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
				"gitmoot job answer <job-id>",
				"gitmoot review request --pr <number>",
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
				"gitmoot lock list",
				"gitmoot job answer <job-id>",
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
				"gitmoot lock list",
				"gitmoot job answer <job-id>",
				"gitmoot review request --pr",
				"gitmoot review status --pr",
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

// TestReviewDeadlineDocsKeepLoadBearingFacts pins the facts a reader needs in
// the skill CLI reference's review-deadline block (#2192), so an edit cannot
// silently drop them.
func TestReviewDeadlineDocsKeepLoadBearingFacts(t *testing.T) {
	const (
		blockStart = "The effective deadline resolves in this order:"
		blockEnd   = "`quiet_kill_after` controls the transcript-silence leg"
	)
	text := readRepoFile(t, "skills", "gitmoot", "references", "CLI.md")
	start := strings.Index(text, blockStart)
	end := strings.Index(text, blockEnd)
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("skills/gitmoot/references/CLI.md: review-deadline block not found (start=%d end=%d); if the docs were restructured, update this test's markers deliberately", start, end)
	}
	skill := text[start:end]
	// The facts a reader needs must actually be in the block.
	for _, want := range []string{
		"[review_router]",
		"job_timeout_payload_invalid",
		"review_class_deadline_default",
		"must be positive",
		"stored job type is `review`",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("review-deadline docs no longer mention %q", want)
		}
	}
}

// TestReviewRouterDocsKeepLoadBearingFacts pins the load-bearing facts of the
// skill CLI reference's review-router section (#2196).
func TestReviewRouterDocsKeepLoadBearingFacts(t *testing.T) {
	const (
		blockStart = "`gitmoot review request` is the front door"
		blockEnd   = "What one request does, in order:"
	)
	text := readRepoFile(t, "skills", "gitmoot", "references", "CLI.md")
	start := strings.Index(text, blockStart)
	end := strings.Index(text, blockEnd)
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("skills/gitmoot/references/CLI.md: review-router block not found (start=%d end=%d); if the docs were restructured, update these markers deliberately", start, end)
	}
	skill := text[start:end]
	for _, want := range []string{
		"routes THROUGH this command",
		"--lead <implementer>",
		"ATTACHING request is told what it lost",
		// Was "names the flag on stderr" until #2199: the stderr claim was
		// inaccurate for --foreground and a missing --org-role, which print
		// nothing. The DURABLE record is the job event, so that is what the docs
		// must keep naming.
		"router_bypassed",
		"UNDERCOUNTS",
		// #2199 round 2 named 'gitmoot job show' - which never lists events -
		// and claimed the other arms 'do not print', which is false whenever an
		// advance warning fires. Both are pinned so a confident wrong command
		// cannot come back.
		"gitmoot job events <id>",
		"not the discriminator",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("review-router docs no longer mention %q", want)
		}
	}
}
