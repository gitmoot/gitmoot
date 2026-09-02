package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const editFixture = `# Gitmoot local configuration.

[paths]
database = "/home/.gitmoot/gitmoot.db"

[agents.planner]
# the planner runs ask jobs
runtime = "codex"
template = "gitmoot-plan-and-goal"
max_background = 4
idle_timeout = "10m"

[feedback]
repo = "owner/feedback"
`

func editTestPaths(t *testing.T, contents string) Paths {
	t.Helper()
	home := t.TempDir()
	paths := PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return paths
}

func TestSetConfigScalarPreservesCommentsAndOtherKeys(t *testing.T) {
	paths := editTestPaths(t, editFixture)
	if err := SetConfigScalar(paths, []string{"agents", "planner", "max_background"}, IntScalar(8)); err != nil {
		t.Fatalf("SetConfigScalar: %v", err)
	}
	out, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "max_background = 8") {
		t.Fatalf("value not updated:\n%s", got)
	}
	// Comments and untouched keys survive.
	for _, want := range []string{
		"# Gitmoot local configuration.",
		"# the planner runs ask jobs",
		`template = "gitmoot-plan-and-goal"`,
		`idle_timeout = "10m"`,
		`repo = "owner/feedback"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("lost %q after edit:\n%s", want, got)
		}
	}
	// The change re-parses through the real loaders.
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes after edit: %v", err)
	}
	if types["planner"].MaxBackground != 8 {
		t.Fatalf("max_background = %d, want 8", types["planner"].MaxBackground)
	}
}

func TestSetConfigScalarStringValue(t *testing.T) {
	paths := editTestPaths(t, editFixture)
	if err := SetConfigScalar(paths, []string{"feedback", "repo"}, StringScalar("owner/other")); err != nil {
		t.Fatalf("SetConfigScalar: %v", err)
	}
	repo, err := LoadDefaultFeedbackRepo(paths)
	if err != nil || repo != "owner/other" {
		t.Fatalf("feedback repo = %q (err %v)", repo, err)
	}
}

func TestSetConfigScalarMissingKeyErrors(t *testing.T) {
	paths := editTestPaths(t, editFixture)
	if err := SetConfigScalar(paths, []string{"agents", "planner", "nonexistent"}, IntScalar(1)); err == nil {
		t.Fatal("expected error for a missing key")
	}
	// Adding new keys is not allowed here (stays an $EDITOR job).
	if err := SetConfigScalar(paths, []string{"agents", "ghost", "max_background"}, IntScalar(1)); err == nil {
		t.Fatal("expected error for a missing section")
	}
}

func TestSetConfigScalarRevertsOnInvalidResult(t *testing.T) {
	paths := editTestPaths(t, editFixture)
	original, _ := os.ReadFile(paths.ConfigFile)
	// max_background must be an integer; writing a non-int string makes the
	// re-parse fail, so the write must be reverted.
	err := SetConfigScalar(paths, []string{"agents", "planner", "max_background"}, StringScalar("not-an-int"))
	if err == nil {
		t.Fatal("expected validation failure")
	}
	after, _ := os.ReadFile(paths.ConfigFile)
	if string(after) != string(original) {
		t.Fatalf("file not reverted after invalid edit:\n%s", string(after))
	}
}

func TestSetConfigScalarPreservesTrailingComment(t *testing.T) {
	fixture := "[agents.planner]\nmax_background = 4 # ops cap, do not exceed\n"
	paths := editTestPaths(t, fixture)
	if err := SetConfigScalar(paths, []string{"agents", "planner", "max_background"}, IntScalar(8)); err != nil {
		t.Fatalf("SetConfigScalar: %v", err)
	}
	out, _ := os.ReadFile(paths.ConfigFile)
	got := string(out)
	if !strings.Contains(got, "max_background = 8") {
		t.Fatalf("value not updated:\n%s", got)
	}
	if !strings.Contains(got, "ops cap, do not exceed") {
		t.Fatalf("trailing comment on the edited line was dropped:\n%s", got)
	}
}
