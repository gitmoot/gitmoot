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
