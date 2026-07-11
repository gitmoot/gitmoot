package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestPipelineAddEnabledTriggerPersistsPendingBinding(t *testing.T) {
	home := t.TempDir()
	specFile := writeSpec(t, "name: mail-flow\nrepo: owner/repo\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo ok}\n")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"pipeline", "add", specFile, "--enable", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "pipeline bind-trigger mail-flow") {
		t.Fatalf("missing actionable pending warning: %s", stderr.String())
	}
	if err := withStore(home, func(store *db.Store) error {
		rec, ok, err := store.GetPipeline(context.Background(), "mail-flow")
		if err != nil || !ok {
			return fmt.Errorf("GetPipeline: ok=%v err=%v", ok, err)
		}
		binding, err := decodeTriggerBinding(rec.TriggerBinding)
		if err != nil {
			return err
		}
		if !rec.Enabled || binding.State != triggerBindingPending || binding.BindingID == "" {
			return fmt.Errorf("record=%+v binding=%+v", rec, binding)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

const testPipelineSpec = `name: deploy-flow
repo: jerryfane/gitmoot
schedule:
  interval: 24h
  jitter: 15m
stages:
  - id: source
    cmd: git fetch --all
  - id: score
    cmd: ./score.sh
    needs: [source]
  - id: deploy
    cmd: ./deploy.sh
    needs: [score]
`

func writeSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}

// TestPipelineAddListShowEnableDisableRemove exercises the full CLI lifecycle
// through Run() with an isolated home, asserting the registry round-trip and the
// hidden-runner-agent behavior.
func TestPipelineAddListShowEnableDisableRemove(t *testing.T) {
	home := t.TempDir()
	run := func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := Run(append(args, "--home", home), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	spec := writeSpec(t, testPipelineSpec)

	out, errOut, code := run("pipeline", "add", spec, "--enable")
	if code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "added pipeline deploy-flow") || !strings.Contains(out, "enabled") || !strings.Contains(out, "3 stages") {
		t.Fatalf("add stdout=%q", out)
	}

	out, errOut, code = run("pipeline", "list")
	if code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "deploy-flow") || !strings.Contains(out, "enabled") || !strings.Contains(out, "24h") {
		t.Fatalf("list stdout=%q", out)
	}

	out, errOut, code = run("pipeline", "show", "deploy-flow")
	if code != 0 {
		t.Fatalf("show exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "enabled: true") || !strings.Contains(out, "interval: 24h") ||
		!strings.Contains(out, "score\tneeds=source") || !strings.Contains(out, "cmd=./deploy.sh") {
		t.Fatalf("show stdout=%q", out)
	}

	// The hidden shell runner agent exists but is filtered out of `agent list`.
	out, _, _ = run("agent", "list")
	if strings.Contains(out, "pipeline-deploy-flow-runner") {
		t.Fatalf("runner agent leaked into agent list: %q", out)
	}
	if _, _, code = run("agent", "show", "pipeline-deploy-flow-runner"); code != 0 {
		t.Fatalf("runner agent should exist (agent show exit=%d)", code)
	}

	if _, errOut, code = run("pipeline", "disable", "deploy-flow"); code != 0 {
		t.Fatalf("disable exit=%d stderr=%s", code, errOut)
	}
	out, _, _ = run("pipeline", "show", "deploy-flow")
	if !strings.Contains(out, "enabled: false") {
		t.Fatalf("expected disabled, show=%q", out)
	}
	if _, errOut, code = run("pipeline", "enable", "deploy-flow"); code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, errOut)
	}
	out, _, _ = run("pipeline", "show", "deploy-flow")
	if !strings.Contains(out, "enabled: true") {
		t.Fatalf("expected enabled, show=%q", out)
	}

	if _, errOut, code = run("pipeline", "remove", "deploy-flow"); code != 0 {
		t.Fatalf("remove exit=%d stderr=%s", code, errOut)
	}
	// Removing the pipeline also disposes the runner agent (best-effort).
	if _, _, code = run("agent", "show", "pipeline-deploy-flow-runner"); code == 0 {
		t.Fatalf("runner agent should be removed with the pipeline")
	}
}

func TestPipelineShowJSON(t *testing.T) {
	home := t.TempDir()
	spec := writeSpec(t, testPipelineSpec)
	if code := Run([]string{"pipeline", "add", spec, "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("add exit=%d", code)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "show", "deploy-flow", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("show --json exit=%d stderr=%s", code, stderr.String())
	}
	var decoded struct {
		Name     string `json:"name"`
		Enabled  bool   `json:"enabled"`
		SpecHash string `json:"spec_hash"`
		Stages   []struct {
			ID    string   `json:"id"`
			Cmd   string   `json:"cmd"`
			Needs []string `json:"needs"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode show json: %v (%s)", err, stdout.String())
	}
	if decoded.Name != "deploy-flow" || decoded.Enabled || decoded.SpecHash == "" {
		t.Fatalf("unexpected json header: %+v", decoded)
	}
	if len(decoded.Stages) != 3 || decoded.Stages[2].ID != "deploy" || len(decoded.Stages[2].Needs) != 1 {
		t.Fatalf("unexpected stages: %+v", decoded.Stages)
	}
}

func TestPipelineListJSON(t *testing.T) {
	home := t.TempDir()
	spec := writeSpec(t, testPipelineSpec)
	if code := Run([]string{"pipeline", "add", spec, "--enable", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("add exit=%d", code)
	}
	var stdout bytes.Buffer
	if code := Run([]string{"pipeline", "list", "--json", "--home", home}, &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("list --json exit=%d", code)
	}
	var decoded []struct {
		Name     string `json:"name"`
		Enabled  bool   `json:"enabled"`
		Interval string `json:"interval"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode list json: %v (%s)", err, stdout.String())
	}
	if len(decoded) != 1 || decoded[0].Name != "deploy-flow" || !decoded[0].Enabled || decoded[0].Interval != "24h" {
		t.Fatalf("unexpected list json: %+v", decoded)
	}
}

func TestPipelineValidationExitCodes(t *testing.T) {
	home := t.TempDir()
	run := func(args ...string) int {
		return Run(append(args, "--home", home), &bytes.Buffer{}, &bytes.Buffer{})
	}
	// Invalid spec -> validation error exit 2.
	dup := writeSpec(t, "name: p\nstages:\n  - {id: a, cmd: echo}\n  - {id: a, cmd: echo}\n")
	if code := run("pipeline", "add", dup); code != 2 {
		t.Fatalf("dup-id add exit=%d, want 2", code)
	}
	// Missing file -> runtime error exit 1.
	if code := run("pipeline", "add", filepath.Join(t.TempDir(), "nope.yaml")); code != 1 {
		t.Fatalf("missing-file add exit=%d, want 1", code)
	}
	// Show/enable/remove of a missing pipeline -> exit 1.
	if code := run("pipeline", "show", "ghost"); code != 1 {
		t.Fatalf("show missing exit=%d, want 1", code)
	}
	if code := run("pipeline", "enable", "ghost"); code != 1 {
		t.Fatalf("enable missing exit=%d, want 1", code)
	}
	if code := run("pipeline", "remove", "ghost"); code != 1 {
		t.Fatalf("remove missing exit=%d, want 1", code)
	}
	// Unknown subcommand -> usage error exit 2.
	if code := run("pipeline", "bogus"); code != 2 {
		t.Fatalf("unknown subcommand exit=%d, want 2", code)
	}
}

// TestPipelineRunnerNameCollisionRefused proves `pipeline add` refuses to clobber
// a pre-existing non-shell agent occupying the runner name.
func TestPipelineRunnerNameCollisionRefused(t *testing.T) {
	home := t.TempDir()
	// Pre-seed a real codex agent named like the runner would be.
	if err := withStore(home, func(store *db.Store) error {
		return store.UpsertAgent(context.Background(), db.Agent{
			Name: "pipeline-clash-runner", Role: "planner", Runtime: "codex",
			Capabilities: []string{"ask"}, AutonomyPolicy: "auto", HealthStatus: "unknown",
		})
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	spec := writeSpec(t, "name: clash\nstages:\n  - {id: a, cmd: echo}\n")
	var stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", spec, "--home", home}, &bytes.Buffer{}, &stderr); code != 1 {
		t.Fatalf("colliding add exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "collides with an existing codex agent") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	// The pipeline row must not have been created.
	if code := Run([]string{"pipeline", "show", "clash", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatalf("pipeline should not exist after refused add")
	}
}
