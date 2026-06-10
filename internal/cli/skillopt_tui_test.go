package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// replaceSkillOptTrainInitTUI stubs the TUI gate and (optionally) the runner.
func replaceSkillOptTrainInitTUI(capable bool, run func(home, scope string, stdout io.Writer, values *skillOptTrainInitInputs, missing []string) error) func() {
	prevCapable := skillOptTrainInitTUICapable
	prevRun := runSkillOptTrainInitTUI
	skillOptTrainInitTUICapable = func() bool { return capable }
	if run != nil {
		runSkillOptTrainInitTUI = run
	}
	return func() {
		skillOptTrainInitTUICapable = prevCapable
		runSkillOptTrainInitTUI = prevRun
	}
}

func TestSkillOptTrainInitTUIDispatchWritesScaffold(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	seedPlannerTemplate(t, home)

	restore := replaceSkillOptTrainInitTUI(true, func(_, _ string, _ io.Writer, values *skillOptTrainInitInputs, _ []string) error {
		values.Name = "tui-flow"
		values.Template = "planner"
		values.ReviewRepo = "jerryfane/gitmoot"
		values.ArtifactKind = "text"
		values.Preview = "text-table"
		values.Request = "Improve planner summaries."
		return nil
	})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	cfg, err := skillopt.LoadTrainInitConfig(filepath.Join(workspace, ".gitmoot", "skillopt", "tui-flow", "config.toml"))
	if err != nil {
		t.Fatalf("LoadTrainInitConfig: %v", err)
	}
	if cfg.Name != "tui-flow" || cfg.Template != "planner" || cfg.ReviewRepo != "jerryfane/gitmoot" || cfg.Preview != "text-table" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestSkillOptTrainInitTUIDispatchAbortWritesNothing(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	seedPlannerTemplate(t, home)

	restore := replaceSkillOptTrainInitTUI(true, func(_, _ string, _ io.Writer, _ *skillOptTrainInitInputs, _ []string) error {
		return errSkillOptTrainInitAborted
	})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("abort exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "aborted: no scaffold written") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, err := skillopt.LoadTrainInitConfig(filepath.Join(workspace, ".gitmoot", "skillopt", "any", "config.toml")); err == nil {
		t.Fatal("no scaffold should have been written on abort")
	}
}

func TestSkillOptTrainInitTUIDispatchPartialFails(t *testing.T) {
	home := t.TempDir()
	chdirTemp(t)
	seedPlannerTemplate(t, home)

	restore := replaceSkillOptTrainInitTUI(true, func(_, _ string, _ io.Writer, values *skillOptTrainInitInputs, _ []string) error {
		values.Name = "tui-flow" // leaves the other required fields empty
		return nil
	})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("partial exit = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing required fields") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestSkillOptTrainInitTUIIncapableFallsBackToLineWizard proves the dispatch
// ordering: when the TUI is not capable (the `go test` reality — pipes, not
// char devices), the existing line wizard runs unchanged.
func TestSkillOptTrainInitTUIIncapableFallsBackToLineWizard(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	seedPlannerTemplate(t, home)

	restore := replaceSkillOptTrainInitTUI(false, nil) // not capable; runner must not be called
	defer restore()
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()
	restoreStdin := replaceSkillOptTrainInitStdin("line-flow\nplanner\njerryfane/gitmoot\ntext\ntext-table\nImprove planner.\n")
	defer restoreStdin()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("line wizard exit = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Choose a template:") {
		t.Fatalf("expected the line wizard to run:\n%s", stdout.String())
	}
	cfg, err := skillopt.LoadTrainInitConfig(filepath.Join(workspace, ".gitmoot", "skillopt", "line-flow", "config.toml"))
	if err != nil {
		t.Fatalf("LoadTrainInitConfig: %v", err)
	}
	if cfg.Name != "line-flow" {
		t.Fatalf("config = %+v", cfg)
	}
}
