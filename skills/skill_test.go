package skills

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Agent Skills specification (https://agentskills.io/specification) limits
// these fields; CI also runs the reference validator (skills-ref). These checks
// keep the same limits in `go test` so a local run catches them too.
var specName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestSkillFrontmatterFollowsTheSpec(t *testing.T) {
	text := readSkillFile(t, "SKILL.md")
	var frontmatter map[string]any
	if err := yaml.Unmarshal([]byte(parseFrontmatter(t, text)), &frontmatter); err != nil {
		t.Fatalf("SKILL.md frontmatter is not YAML: %v", err)
	}
	allowed := map[string]bool{"name": true, "description": true, "license": true, "compatibility": true, "metadata": true, "allowed-tools": true}
	for key := range frontmatter {
		if !allowed[key] {
			t.Errorf("frontmatter field %q is not in the Agent Skills spec", key)
		}
	}
	name, _ := frontmatter["name"].(string)
	if name != "gitmoot" || len(name) > 64 || !specName.MatchString(name) {
		t.Errorf("name = %q, want %q (the directory name; 1-64 lowercase letters, digits and single hyphens)", name, "gitmoot")
	}
	description, _ := frontmatter["description"].(string)
	if n := len([]rune(description)); n == 0 || n > 1024 {
		t.Errorf("description has %d characters, want 1-1024", n)
	}
	if compatibility, ok := frontmatter["compatibility"]; ok {
		text, _ := compatibility.(string)
		if n := len([]rune(text)); n == 0 || n > 500 {
			t.Errorf("compatibility has %d characters, want 1-500", n)
		}
	}
	if metadata, ok := frontmatter["metadata"]; ok {
		entries, ok := metadata.(map[string]any)
		if !ok {
			t.Fatalf("metadata must be a map, got %T", metadata)
		}
		for key, value := range entries {
			if _, ok := value.(string); !ok {
				t.Errorf("metadata[%q] must be a string, got %T", key, value)
			}
		}
	}
}

// TestSkillDocumentsOnlyTheReducedScope pins the description to what gitmoot
// is for after the #2578 scope reduction, and keeps removed features out of it.
func TestSkillDocumentsOnlyTheReducedScope(t *testing.T) {
	frontmatter := strings.ToLower(parseFrontmatter(t, readSkillFile(t, "SKILL.md")))
	for _, want := range []string{"review", "verdict", "merge gate", "message", "org chart", "escalate", "run cancel", "doctor", "daemon drain", "design-notes", "dashboard", "omp"} {
		if !strings.Contains(frontmatter, want) {
			t.Errorf("frontmatter does not mention %q", want)
		}
	}
	for _, removed := range []string{"orchestrat", "delegation", "template", "heartbeat", "pipeline", "kimi", "webhook", "telemetry"} {
		if strings.Contains(frontmatter, removed) {
			t.Errorf("frontmatter still mentions removed feature %q", removed)
		}
	}
}

func TestSkillMDStaysShort(t *testing.T) {
	text := readSkillFile(t, "SKILL.md")
	if lines := strings.Count(text, "\n"); lines >= 500 {
		t.Fatalf("SKILL.md has %d lines; the spec asks for under 500, with detail in references/", lines)
	}
}

var markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestSkillLinksAreRelativeAndResolve checks every markdown link in the skill:
// in-skill links are relative and name a file that exists; only http(s) links
// may leave the skill.
func TestSkillLinksAreRelativeAndResolve(t *testing.T) {
	err := fs.WalkDir(FS, "gitmoot", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".md") {
			return err
		}
		data, err := fs.ReadFile(FS, name)
		if err != nil {
			return err
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(data), -1) {
			target := match[1]
			if strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "#") {
				continue
			}
			if strings.HasPrefix(target, "/") || strings.Contains(target, "://") {
				t.Errorf("%s: link %q is not relative", name, target)
				continue
			}
			file, _, _ := strings.Cut(target, "#")
			resolved := path.Clean(path.Join(path.Dir(name), file))
			if !strings.HasPrefix(resolved, "gitmoot/") {
				t.Errorf("%s: link %q leaves the skill", name, target)
				continue
			}
			if _, err := fs.Stat(FS, resolved); err != nil {
				t.Errorf("%s: link %q does not resolve: %v", name, target, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	text := readSkillFile(t, "SKILL.md")
	for _, ref := range []string{"references/CLI.md", "references/WORKFLOWS.md", "references/RESULT_CONTRACT.md", "references/SAFETY.md"} {
		if !strings.Contains(text, "]("+ref+")") {
			t.Errorf("SKILL.md does not link %s", ref)
		}
	}
}

// TestSkillHasOneSource pins #2595: the repository holds one copy of the
// skill. gitmoot.io and both plugin packages serve skills/gitmoot.
func TestSkillHasOneSource(t *testing.T) {
	if _, err := os.Stat("../SKILL.md"); !os.IsNotExist(err) {
		t.Fatalf("a repo-root SKILL.md exists (stat err = %v); skills/gitmoot/SKILL.md is the only copy", err)
	}
}

func TestCLIReferenceIsGenerated(t *testing.T) {
	text := readSkillFile(t, "references/CLI.md")
	if !strings.Contains(text, "Code generated by internal/clidoc/clidocgen") {
		t.Fatal("references/CLI.md is not the generated reference; run `go generate ./skills/`")
	}
}

// TestResultContractCarriesTheAttributionFix pins the #2595 carry-over: neither
// the reviewer agent nor lead_agent is an attribution fallback, and a CLI
// review dispatch records no lead_agent.
func TestResultContractCarriesTheAttributionFix(t *testing.T) {
	text := strings.Join(strings.Fields(readSkillFile(t, "references/RESULT_CONTRACT.md")), " ")
	for _, want := range []string{"Neither the run's own reviewer nor `lead_agent` is an attribution fallback", "a CLI review dispatch records no `lead_agent`"} {
		if !strings.Contains(text, want) {
			t.Errorf("RESULT_CONTRACT.md missing %q", want)
		}
	}
	if strings.Contains(text, "delegations") {
		t.Error("RESULT_CONTRACT.md documents delegations; it covers the review result only")
	}
}

func readSkillFile(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(FS, "gitmoot/"+name)
	if err != nil {
		t.Fatalf("read skills/gitmoot/%s: %v", name, err)
	}
	return string(data)
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
