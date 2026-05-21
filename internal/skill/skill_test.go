package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootSkillFrontmatter(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	text := string(contents)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatal("SKILL.md missing YAML frontmatter opener")
	}
	parts := strings.SplitN(text, "---\n", 3)
	if len(parts) != 3 {
		t.Fatal("SKILL.md missing YAML frontmatter closer")
	}
	frontmatter := parts[1]
	for _, want := range []string{
		"name: gitmoot-agent",
		"description: Use Gitmoot",
		"version: 0.1.0",
		"metadata:",
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
			t.Fatalf("frontmatter missing %q:\n%s", want, frontmatter)
		}
	}
	if strings.Contains(frontmatter, "requires:\n      env:") || strings.Contains(frontmatter, "requires.env") {
		t.Fatalf("optional GH_TOKEN must not be declared as required env:\n%s", frontmatter)
	}
}

func TestRootSkillDocumentsGitmootResultAndRereadGuidance(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	text := string(contents)
	for _, want := range []string{
		"gitmoot_result",
		"approved|changes_requested|blocked|implemented|failed",
		"branch locks",
		"reread this `SKILL.md`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("SKILL.md missing %q", want)
		}
	}
}
