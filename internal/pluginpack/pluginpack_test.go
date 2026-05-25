package pluginpack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
)

func TestBuildCodexPackage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "gitmoot")
	result, err := Build(BuildOptions{
		Provider: ProviderCodex,
		OutDir:   out,
		Info:     buildinfo.Info{Version: "v1.2.3"},
		SourceFS: validSkillFS(),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if result.Path != out {
		t.Fatalf("Path = %q, want %q", result.Path, out)
	}
	assertFileContains(t, filepath.Join(out, "skills", "gitmoot", "SKILL.md"), "Gitmoot")
	assertFileContains(t, filepath.Join(out, "skills", "gitmoot", "references", "CLI.md"), "gitmoot version")

	manifest := readJSON(t, filepath.Join(out, ".codex-plugin", "plugin.json"))
	if got := manifest["name"]; got != PluginName {
		t.Fatalf("manifest name = %v, want %q", got, PluginName)
	}
	if got := manifest["version"]; got != "1.2.3" {
		t.Fatalf("manifest version = %v, want 1.2.3", got)
	}
	if got := manifest["skills"]; got != "./skills/" {
		t.Fatalf("manifest skills = %v, want ./skills/", got)
	}
	iface, ok := manifest["interface"].(map[string]any)
	if !ok {
		t.Fatalf("manifest interface missing or invalid: %#v", manifest["interface"])
	}
	if got := iface["privacyPolicyURL"]; got != PrivacyURL {
		t.Fatalf("manifest privacyPolicyURL = %v, want %q", got, PrivacyURL)
	}
	if got := iface["termsOfServiceURL"]; got != TermsURL {
		t.Fatalf("manifest termsOfServiceURL = %v, want %q", got, TermsURL)
	}
	defaultPrompt, ok := iface["defaultPrompt"].([]any)
	if !ok || len(defaultPrompt) != 1 || defaultPrompt[0] != "Use $gitmoot to check agent status." {
		t.Fatalf("manifest defaultPrompt invalid: %#v", manifest["interface"])
	}
}

func TestBuildClaudePackage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "gitmoot")
	_, err := Build(BuildOptions{
		Provider: ProviderClaude,
		OutDir:   out,
		Info:     buildinfo.Info{Version: "dev"},
		SourceFS: validSkillFS(),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	assertFileContains(t, filepath.Join(out, "skills", "gitmoot", "SKILL.md"), "Gitmoot")

	manifest := readJSON(t, filepath.Join(out, ".claude-plugin", "plugin.json"))
	if got := manifest["name"]; got != PluginName {
		t.Fatalf("manifest name = %v, want %q", got, PluginName)
	}
	if got := manifest["version"]; got != "0.0.0-dev" {
		t.Fatalf("manifest version = %v, want 0.0.0-dev", got)
	}
	if _, ok := manifest["skills"]; ok {
		t.Fatalf("claude manifest should not include codex skills pointer: %#v", manifest)
	}
}

func TestBuildDefaultPackageDir(t *testing.T) {
	home := t.TempDir()
	result, err := Build(BuildOptions{
		Provider: ProviderCodex,
		Home:     home,
		SourceFS: validSkillFS(),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	want := filepath.Join(home, "plugins", "build", "codex", "gitmoot")
	if result.Path != want {
		t.Fatalf("Path = %q, want %q", result.Path, want)
	}
	assertFileContains(t, filepath.Join(want, "skills", "gitmoot", "SKILL.md"), "Gitmoot")
}

func TestBuildUsesEmbeddedSkillByDefault(t *testing.T) {
	out := filepath.Join(t.TempDir(), "gitmoot")
	_, err := Build(BuildOptions{
		Provider: ProviderCodex,
		OutDir:   out,
		Info:     buildinfo.Info{Version: "0.1.0"},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	assertFileContains(t, filepath.Join(out, "skills", "gitmoot", "SKILL.md"), "Gitmoot Agent Skill")
	assertFileContains(t, filepath.Join(out, "skills", "gitmoot", "references", "GOAL_TEMPLATE.md"), "codex exec review is clean; ready for manual /review.")
	assertFileContains(t, filepath.Join(out, "skills", "gitmoot", "references", "RESULT_CONTRACT.md"), "gitmoot_result")
	assertFileContains(t, filepath.Join(out, "skills", "gitmoot", "presets", "gitmoot-plan-and-goal.md"), "Gitmoot Plan And Goal Writer")
}

func TestBuildRefusesOverwriteWithoutForce(t *testing.T) {
	out := filepath.Join(t.TempDir(), "gitmoot")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Build(BuildOptions{
		Provider: ProviderCodex,
		OutDir:   out,
		SourceFS: validSkillFS(),
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Build() error = %v, want already exists", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "stale.txt")); statErr != nil {
		t.Fatalf("stale file was removed without force: %v", statErr)
	}
}

func TestBuildForceReplacesExistingPackage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "gitmoot")
	if _, err := Build(BuildOptions{
		Provider: ProviderCodex,
		OutDir:   out,
		SourceFS: validSkillFS(),
	}); err != nil {
		t.Fatalf("initial Build() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(out, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Build(BuildOptions{
		Provider: ProviderCodex,
		OutDir:   out,
		Force:    true,
		SourceFS: validSkillFS(),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file still exists after force rebuild: %v", err)
	}
	assertFileContains(t, filepath.Join(out, "skills", "gitmoot", "SKILL.md"), "Gitmoot")
}

func TestBuildForceRefusesArbitraryExplicitOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "not-a-plugin")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "important.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Build(BuildOptions{
		Provider: ProviderCodex,
		OutDir:   out,
		Force:    true,
		SourceFS: validSkillFS(),
	})
	if err == nil || !strings.Contains(err.Error(), "refusing forced replacement") {
		t.Fatalf("Build() error = %v, want refusing forced replacement", err)
	}
	assertFileContains(t, filepath.Join(out, "important.txt"), "keep")
}

func TestBuildValidatesSkillSource(t *testing.T) {
	tests := []struct {
		name   string
		source fstest.MapFS
		want   string
	}{
		{
			name:   "missing skill root",
			source: fstest.MapFS{},
			want:   "canonical skill",
		},
		{
			name: "missing skill file",
			source: fstest.MapFS{
				"gitmoot/references/CLI.md": {Data: []byte("commands")},
			},
			want: "SKILL.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Build(BuildOptions{
				Provider: ProviderCodex,
				OutDir:   filepath.Join(t.TempDir(), "gitmoot"),
				SourceFS: tt.source,
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Build() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func validSkillFS() fstest.MapFS {
	return fstest.MapFS{
		"gitmoot/SKILL.md":                {Data: []byte("# Gitmoot\n")},
		"gitmoot/references/CLI.md":       {Data: []byte("gitmoot version\n")},
		"gitmoot/references/SAFETY.md":    {Data: []byte("safety\n")},
		"gitmoot/references/WORKFLOWS.md": {Data: []byte("workflow\n")},
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(content), want) {
		t.Fatalf("%s does not contain %q:\n%s", path, want, content)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value map[string]any
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", path, err, content)
	}
	return value
}
