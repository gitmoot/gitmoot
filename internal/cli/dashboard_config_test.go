package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/config"
)

const sampleConfigTOML = `# Gitmoot local configuration.

[paths]
database = "/home/.gitmoot/gitmoot.db"
logs = "/home/.gitmoot/logs"
workspaces = "/home/.gitmoot/workspaces"

[agents.planner]
runtime = "codex"
template = "gitmoot-plan-and-goal"
role = "planner"
capabilities = ["ask"]
max_background = 4
idle_timeout = "10m"
job_timeout = "45m"

[parallel_sessions]
same_session = "fork_temp_session"
merge_back = "summary"
max_temp_sessions_per_agent = 4
eligible_actions = ["ask", "review", "implement"]

[feedback]
repo = "owner/feedback"
`

func writeConfig(t *testing.T, contents string) config.Paths {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return paths
}

func sectionByTitle(view tui.ConfigView, title string) (tui.ConfigSection, bool) {
	for _, s := range view.Sections {
		if s.Title == title {
			return s, true
		}
	}
	return tui.ConfigSection{}, false
}

func editableByLabel(section tui.ConfigSection, label string) (tui.ConfigField, bool) {
	for _, f := range section.Editable {
		if f.Label == label {
			return f, true
		}
	}
	return tui.ConfigField{}, false
}

func TestBuildDashboardConfigView(t *testing.T) {
	paths := writeConfig(t, sampleConfigTOML)
	view := buildDashboardConfigView(paths, dashboardDaemonDetail{Flags: []string{"--workers", "4"}, WorkDir: "/work"})

	if view.Path != paths.ConfigFile {
		t.Fatalf("path = %q", view.Path)
	}
	if _, ok := sectionByTitle(view, "paths"); !ok {
		t.Fatal("missing paths section")
	}
	agents, ok := sectionByTitle(view, "agent types")
	if !ok || len(agents.Rows) != 2 || agents.Rows[1][0] != "planner" {
		t.Fatalf("agent types section wrong: %+v", agents)
	}
	// Sessions: the title carries its domain and the scalars are inline-editable.
	sessions, ok := sectionByTitle(view, "parallel sessions (session policy)")
	if !ok || len(sessions.Rows) != 4 {
		t.Fatalf("parallel sessions section wrong: %+v", sessions)
	}
	wantEditable := map[string]tui.ConfigKind{
		"parallel_sessions · same_session":                tui.ConfigText,
		"parallel_sessions · merge_back":                  tui.ConfigText,
		"parallel_sessions · max_temp_sessions_per_agent": tui.ConfigInt,
		"parallel_sessions · eligible_actions":            tui.ConfigText,
	}
	if len(sessions.Editable) != len(wantEditable) {
		t.Fatalf("parallel sessions editable count = %d, want %d: %+v", len(sessions.Editable), len(wantEditable), sessions.Editable)
	}
	for _, f := range sessions.Editable {
		kind, ok := wantEditable[f.Label]
		if !ok {
			t.Fatalf("unexpected editable field %q", f.Label)
		}
		if f.Kind != kind {
			t.Fatalf("editable %q kind = %v, want %v", f.Label, f.Kind, kind)
		}
		if len(f.KeyPath) != 2 || f.KeyPath[0] != "parallel_sessions" {
			t.Fatalf("editable %q keypath wrong: %v", f.Label, f.KeyPath)
		}
	}
	// eligible_actions prefills the bracketed TOML literal so the writer can
	// round-trip it back to a string array.
	eligible, _ := editableByLabel(sessions, "parallel_sessions · eligible_actions")
	if eligible.Value != `["ask", "review", "implement"]` {
		t.Fatalf("eligible_actions value = %q", eligible.Value)
	}
	feedback, ok := sectionByTitle(view, "feedback")
	if !ok || feedback.Rows[0][1] != "owner/feedback" {
		t.Fatalf("feedback section wrong: %+v", feedback)
	}
	daemon, ok := sectionByTitle(view, "daemon (persisted)")
	if !ok || daemon.Rows[0][1] != "--workers 4" {
		t.Fatalf("daemon section wrong: %+v", daemon)
	}
}

func TestValidateDashboardConfigCleanAndBroken(t *testing.T) {
	clean := writeConfig(t, sampleConfigTOML)
	if problems := validateDashboardConfig(clean); len(problems) != 0 {
		t.Fatalf("clean config should validate, got %v", problems)
	}

	broken := writeConfig(t, "[agents.bad]\nmax_background = \"not-an-int\"\n")
	problems := validateDashboardConfig(broken)
	if len(problems) == 0 {
		t.Fatal("broken config should report a problem")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "agents") {
		t.Fatalf("problem should name the section: %v", problems)
	}
}

func TestEditConfigCmdHonorsEditorEnv(t *testing.T) {
	t.Setenv("EDITOR", "true") // a real no-op binary, so ExecProcess succeeds
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte(sampleConfigTOML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := editConfigCmd(cfg)
	if cmd == nil {
		t.Fatal("editConfigCmd returned nil")
	}
	// We can't fully run ExecProcess outside a tea program, but the returned
	// cmd must be non-nil and the env path is exercised in the hand test.
	_ = tea.Cmd(cmd)
}

func TestConfigScalarForKind(t *testing.T) {
	intVal := configScalarForKind(tui.ConfigInt, "8")
	strVal := configScalarForKind(tui.ConfigDuration, "15m")
	repoVal := configScalarForKind(tui.ConfigText, "owner/x")
	// A bracketed ConfigText value is written as a TOML string array, while a
	// plain ConfigText scalar (the session enums) stays a string.
	listVal := configScalarForKind(tui.ConfigText, `["ask", "review"]`)
	sessionVal := configScalarForKind(tui.ConfigText, "queue")
	// Apply each to a fixture and confirm the stored TOML type round-trips.
	paths := writeConfig(t, sampleConfigTOML)
	if err := config.SetConfigScalar(paths, []string{"agents", "planner", "max_background"}, intVal); err != nil {
		t.Fatalf("int write: %v", err)
	}
	if err := config.SetConfigScalar(paths, []string{"agents", "planner", "idle_timeout"}, strVal); err != nil {
		t.Fatalf("duration write: %v", err)
	}
	if err := config.SetConfigScalar(paths, []string{"feedback", "repo"}, repoVal); err != nil {
		t.Fatalf("repo write: %v", err)
	}
	if err := config.SetConfigScalar(paths, []string{"parallel_sessions", "eligible_actions"}, listVal); err != nil {
		t.Fatalf("eligible_actions list write: %v", err)
	}
	if err := config.SetConfigScalar(paths, []string{"parallel_sessions", "same_session"}, sessionVal); err != nil {
		t.Fatalf("same_session write: %v", err)
	}
	types, err := config.LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if types["planner"].MaxBackground != 8 || types["planner"].IdleTimeout != "15m" {
		t.Fatalf("typed writes wrong: %+v", types["planner"])
	}
	policy, err := config.LoadParallelSessionPolicy(paths)
	if err != nil {
		t.Fatalf("reload policy: %v", err)
	}
	if strings.Join(policy.EligibleActions, ",") != "ask,review" {
		t.Fatalf("eligible_actions list round-trip wrong: %+v", policy.EligibleActions)
	}
	if policy.SameSession != "queue" {
		t.Fatalf("same_session scalar round-trip wrong: %q", policy.SameSession)
	}
}

func TestParseConfigStringArrayLiteral(t *testing.T) {
	cases := []struct {
		in    string
		items []string
		ok    bool
	}{
		{`["ask", "review", "implement"]`, []string{"ask", "review", "implement"}, true},
		{`[ask, review]`, []string{"ask", "review"}, true},
		{`[]`, []string{}, true},
		{"queue", nil, false},
		{"owner/repo", nil, false},
	}
	for _, tc := range cases {
		items, ok := parseConfigStringArrayLiteral(tc.in)
		if ok != tc.ok {
			t.Fatalf("%q: ok = %v, want %v", tc.in, ok, tc.ok)
		}
		if ok && strings.Join(items, ",") != strings.Join(tc.items, ",") {
			t.Fatalf("%q: items = %v, want %v", tc.in, items, tc.items)
		}
	}
}
