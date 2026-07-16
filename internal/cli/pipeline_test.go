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
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
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
repo: gitmoot/gitmoot
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
	columns := strings.Split(strings.TrimSpace(out), "\t")
	if len(columns) != 7 || columns[0] != "deploy-flow" || columns[1] != "enabled" || columns[2] != "24h" || columns[3] != "gitmoot/gitmoot" || columns[4] != "gitmoot/gitmoot" {
		t.Fatalf("list stdout=%q", out)
	}

	out, errOut, code = run("pipeline", "show", "deploy-flow")
	if code != 0 {
		t.Fatalf("show exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "group: gitmoot/gitmoot (default)") || !strings.Contains(out, "enabled: true") || !strings.Contains(out, "interval: 24h") ||
		!strings.Contains(out, "score\t[SHELL]\tcmd: ./score.sh\tneeds=source") ||
		!strings.Contains(out, "deploy\t[SHELL]\tcmd: ./deploy.sh\tneeds=score") {
		t.Fatalf("show stdout=%q", out)
	}
	if strings.Contains(out, "description:") {
		t.Fatalf("show emitted an unset description: %q", out)
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

func TestPipelineDisplayMode(t *testing.T) {
	triggerSpec := "name: mail\nrepo: owner/repo\ntrigger: {kind: email}\nstages:\n  - {id: run, cmd: echo}\n"
	pipelineTriggerSpec := "name: downstream\nrepo: owner/downstream\ntrigger: {kind: pipeline, pipeline: upstream}\nstages:\n  - {id: run, cmd: echo}\n"
	scheduledSpec := "name: nightly\nschedule: {interval: 24h}\nstages:\n  - {id: run, cmd: echo}\n"
	manualSpec := "name: manual\nstages:\n  - {id: run, cmd: echo}\n"
	tests := []struct {
		name   string
		record db.Pipeline
		want   string
	}{
		{name: "trigger bound", record: db.Pipeline{SpecYAML: triggerSpec, TriggerBinding: `{"state":"bound"}`}, want: "email-triggered (bound)"},
		{name: "trigger pending", record: db.Pipeline{SpecYAML: triggerSpec, TriggerBinding: `{"state":"pending"}`}, want: "email-triggered (pending)"},
		{name: "trigger never bound", record: db.Pipeline{SpecYAML: triggerSpec}, want: "email-triggered (unbound)"},
		{name: "trigger plus schedule hybrid", record: db.Pipeline{SpecYAML: "name: both\nrepo: owner/repo\ntrigger: {kind: email}\nschedule: {interval: 6h}\nstages:\n  - {id: run, cmd: echo}\n", TriggerBinding: `{"state":"bound"}`, Interval: "6h"}, want: "email-triggered (bound), scheduled 6h"},
		{name: "pipeline trigger", record: db.Pipeline{SpecYAML: pipelineTriggerSpec}, want: "after: upstream"},
		{name: "schedule", record: db.Pipeline{SpecYAML: scheduledSpec, Interval: "24h"}, want: "scheduled 24h"},
		{name: "neither", record: db.Pipeline{SpecYAML: manualSpec}, want: "manual"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pipelineDisplayMode(tt.record); got != tt.want {
				t.Fatalf("pipelineDisplayMode() = %q, want %q", got, tt.want)
			}
		})
	}
	if got := pipelineDisplayMode(db.Pipeline{SpecYAML: pipelineTriggerSpec}, true); got != "after: upstream (upstream missing)" {
		t.Fatalf("missing-upstream pipelineDisplayMode() = %q", got)
	}
}

func TestResolvedPipelineGroup(t *testing.T) {
	tests := []struct {
		name        string
		record      db.Pipeline
		want        string
		wantDefault bool
	}{
		{
			name:        "explicit group",
			record:      db.Pipeline{Repo: "owner/repo", SpecYAML: "name: grouped\nrepo: owner/repo\ngroup: Product Operations\nstages: [{id: run, cmd: echo}]\n"},
			want:        "Product Operations",
			wantDefault: false,
		},
		{
			name:        "repo fallback",
			record:      db.Pipeline{Repo: "owner/repo", SpecYAML: "name: plain\nrepo: owner/repo\nstages: [{id: run, cmd: echo}]\n"},
			want:        "owner/repo",
			wantDefault: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, defaulted := resolvedPipelineGroup(tt.record)
			if got != tt.want || defaulted != tt.wantDefault {
				t.Fatalf("resolvedPipelineGroup() = %q,%v, want %q,%v", got, defaulted, tt.want, tt.wantDefault)
			}
		})
	}
}

func TestPipelineShowSelfDescribingStagesAndTriggerListInterval(t *testing.T) {
	home := t.TempDir()
	if err := withStore(home, func(store *db.Store) error {
		return store.UpsertAgent(context.Background(), db.Agent{
			Name: "display-agent", Runtime: "codex", Model: "gpt-test", Effort: "high",
			Capabilities: []string{"ask", "implement"}, AutonomyPolicy: "workspace-write", HealthStatus: "ok",
		})
	}); err != nil {
		t.Fatalf("seed display agent: %v", err)
	}

	displaySpec := writeSpec(t, `name: display-flow
repo: owner/repo
group: Product Operations
description: |
  Coordinates product operations.
  Preserves the declared stage order.
stages:
  - id: shell
    cmd: |
      echo first
      echo this-command-is-deliberately-long-so-the-display-preview-has-to-truncate-with-an-ellipsis-marker
  - id: registered
    agent: display-agent
    action: ask
    prompt: |
      Review "quoted" input on one line.
      This prompt is deliberately long so its preview is truncated while the complete prompt remains available in JSON.
    needs: [shell]
    timeout: 10m
    retry: 2
  - id: missing
    agent: ghost-agent
    action: review
    prompt: Inspect the registered stage result.
    needs: [shell]
  - id: implement
    agent: display-agent
    action: implement
    prompt: Implement the approved result.
    write: true
    needs: [registered, missing]
  - id: merged
    gate: pr_merged
    source: implement
    needs: [implement]
    timeout: 1h
`)
	if code := Run([]string{"pipeline", "add", displaySpec, "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("add display pipeline exit=%d", code)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "show", "display-flow", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("show exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"group: Product Operations",
		"description:\n  Coordinates product operations.\n  Preserves the declared stage order.\n",
		"mode: manual",
		"shell\t[SHELL]\tcmd: echo first; echo this-command-is-deliberately-long",
		"registered\t[AGENT ask]\tdisplay-agent (codex/gpt-test effort=high)\ttimeout=10m\tretry=2\tneeds=shell",
		`prompt: "Review \"quoted\" input on one line. This prompt is deliberately long`,
		"missing\t[AGENT review]\tghost-agent (unregistered)\tneeds=shell",
		"merged\t[GATE pr_merged]\tsource=implement\ttimeout=1h\tneeds=implement",
		"…",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "show", "display-flow", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("show --json exit=%d stderr=%s", code, stderr.String())
	}
	var decoded pipelineJSON
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode show json: %v (%s)", err, stdout.String())
	}
	if decoded.Mode != "manual" || len(decoded.Stages) != 5 {
		t.Fatalf("unexpected display JSON header/stages: %+v", decoded)
	}
	if got := decoded.Stages[0]; got.Kind != "shell" || got.CmdPreview == "" || got.Cmd == got.CmdPreview {
		t.Fatalf("unexpected shell JSON: %+v", got)
	}
	if got := decoded.Stages[1]; got.Kind != "agent_ask" || got.AgentRuntime != "codex" || got.PromptPreview == "" || got.Prompt == got.PromptPreview {
		t.Fatalf("unexpected registered agent JSON: %+v", got)
	}
	if got := decoded.Stages[2]; got.Kind != "agent_review" || got.AgentRuntime != "" {
		t.Fatalf("unexpected unregistered agent JSON: %+v", got)
	}
	if got := decoded.Stages[4]; got.Kind != "gate" {
		t.Fatalf("unexpected gate JSON: %+v", got)
	}

	triggerSpec := writeSpec(t, "name: mail-flow\nrepo: owner/repo\ntrigger: {kind: email}\nstages:\n  - {id: run, cmd: echo}\n")
	if code := Run([]string{"pipeline", "add", triggerSpec, "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("add trigger pipeline exit=%d", code)
	}
	stdout.Reset()
	if code := Run([]string{"pipeline", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, stderr.String())
	}
	rows := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	foundTrigger := false
	foundDisplay := false
	for _, row := range rows {
		columns := strings.Split(row, "\t")
		if len(columns) != 8 {
			t.Fatalf("pipeline list changed column count: row=%q", row)
		}
		if columns[0] == "mail-flow" {
			foundTrigger = true
			if columns[2] != "email" {
				t.Fatalf("trigger pipeline interval column = %q, want email", columns[2])
			}
			if columns[4] != "owner/repo" {
				t.Fatalf("trigger pipeline group column = %q, want repo fallback", columns[4])
			}
			if columns[7] != "" {
				t.Fatalf("trigger pipeline description column = %q, want empty", columns[7])
			}
		}
		if columns[0] == "display-flow" {
			foundDisplay = true
			if columns[2] != "-" {
				t.Fatalf("manual pipeline interval column = %q, want -", columns[2])
			}
			if columns[4] != "Product Operations" {
				t.Fatalf("explicit pipeline group column = %q, want Product Operations", columns[4])
			}
			if columns[7] != "Coordinates product operations. Preserves the declared stage…" {
				t.Fatalf("pipeline description column = %q, want truncated description", columns[7])
			}
		}
	}
	if !foundTrigger {
		t.Fatalf("trigger pipeline missing from list: %s", stdout.String())
	}
	if !foundDisplay {
		t.Fatalf("display pipeline missing from list: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "list", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("list --json exit=%d stderr=%s", code, stderr.String())
	}
	var listed []pipelineJSON
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("decode list json: %v (%s)", err, stdout.String())
	}
	foundJSONDescription := false
	for _, item := range listed {
		switch item.Name {
		case "display-flow":
			foundJSONDescription = true
			if item.Description != "Coordinates product operations.\nPreserves the declared stage order." {
				t.Fatalf("list JSON description = %q, want full text", item.Description)
			}
		case "mail-flow":
			if item.Description != "" {
				t.Fatalf("list JSON empty description = %q", item.Description)
			}
		}
	}
	if !foundJSONDescription {
		t.Fatalf("display pipeline missing from list JSON: %s", stdout.String())
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
		Group    string `json:"group"`
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
	if decoded.Name != "deploy-flow" || decoded.Group != "gitmoot/gitmoot" || decoded.Enabled || decoded.SpecHash == "" {
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
		Group    string `json:"group"`
		Enabled  bool   `json:"enabled"`
		Interval string `json:"interval"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode list json: %v (%s)", err, stdout.String())
	}
	if len(decoded) != 1 || decoded[0].Name != "deploy-flow" || decoded[0].Group != "gitmoot/gitmoot" || !decoded[0].Enabled || decoded[0].Interval != "24h" {
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

func TestPipelineAddTriggerCycleDetection(t *testing.T) {
	triggerSpec := func(name, upstream string) string {
		return fmt.Sprintf("name: %s\nrepo: owner/%s\ntrigger: {kind: pipeline, pipeline: %s}\nstages: [{id: run, cmd: echo}]\n", name, name, upstream)
	}
	t.Run("cycle rejected and dropping edge unblocks", func(t *testing.T) {
		home := t.TempDir()
		a := writeSpec(t, triggerSpec("A", "B"))
		if code := Run([]string{"pipeline", "add", a, "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("add A->B exit=%d", code)
		}
		b := writeSpec(t, triggerSpec("B", "A"))
		var stderr bytes.Buffer
		if code := Run([]string{"pipeline", "add", b, "--home", home}, &bytes.Buffer{}, &stderr); code == 0 {
			t.Fatalf("add B->A unexpectedly succeeded")
		}
		if !strings.Contains(stderr.String(), "B -> A -> B") {
			t.Fatalf("cycle error does not name chain: %s", stderr.String())
		}
		manualA := writeSpec(t, "name: A\nrepo: owner/A\nstages: [{id: run, cmd: echo}]\n")
		if code := Run([]string{"pipeline", "add", manualA, "--home", home}, &bytes.Buffer{}, &stderr); code != 0 {
			t.Fatalf("re-add A without edge exit=%d stderr=%s", code, stderr.String())
		}
		if code := Run([]string{"pipeline", "add", b, "--home", home}, &bytes.Buffer{}, &stderr); code != 0 {
			t.Fatalf("B->A remained blocked after A dropped edge: exit=%d stderr=%s", code, stderr.String())
		}
	})

	t.Run("chain accepted", func(t *testing.T) {
		home := t.TempDir()
		for _, edge := range [][2]string{{"A", "B"}, {"B", "C"}} {
			spec := writeSpec(t, triggerSpec(edge[0], edge[1]))
			var stderr bytes.Buffer
			if code := Run([]string{"pipeline", "add", spec, "--home", home}, &bytes.Buffer{}, &stderr); code != 0 {
				t.Fatalf("add %s->%s exit=%d stderr=%s", edge[0], edge[1], code, stderr.String())
			}
		}
	})
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

// Reviewer-required pins: hybrid list value, multibyte-safe previews, and the
// orchestrate/produce badges (previously untested).
func TestPipelineListIntervalHybridKeepsInterval(t *testing.T) {
	hybrid := db.Pipeline{SpecYAML: "name: both\nrepo: owner/repo\ntrigger: {kind: email}\nschedule: {interval: 6h}\nstages:\n  - {id: run, cmd: echo}\n", Interval: "6h"}
	if got := pipelineListInterval(hybrid); got != "email+6h" {
		t.Fatalf("hybrid list interval = %q, want %q", got, "email+6h")
	}
	triggerOnly := db.Pipeline{SpecYAML: "name: mail\nrepo: owner/repo\ntrigger: {kind: email}\nstages:\n  - {id: run, cmd: echo}\n"}
	if got := pipelineListInterval(triggerOnly); got != "email" {
		t.Fatalf("trigger-only list interval = %q, want %q", got, "email")
	}
	pipelineTrigger := db.Pipeline{SpecYAML: "name: downstream\nrepo: owner/downstream\ntrigger: {kind: pipeline, pipeline: upstream}\nstages:\n  - {id: run, cmd: echo}\n"}
	if got := pipelineListInterval(pipelineTrigger); got != "after: upstream" {
		t.Fatalf("pipeline-trigger list interval = %q", got)
	}
	if got := pipelineListInterval(pipelineTrigger, true); got != "after: upstream (upstream missing)" {
		t.Fatalf("missing-upstream list interval = %q", got)
	}
}

func TestPipelinePreviewMultibyteSafe(t *testing.T) {
	prompt := strings.Repeat("\u65e5\u672c\u8a9e\U0001f680", 60) // 240 runes of multibyte text
	preview := pipelinePromptPreview(prompt)
	if !utf8.ValidString(preview) {
		t.Fatalf("prompt preview is not valid UTF-8: %q", preview)
	}
	if got := len([]rune(preview)); got > 101 {
		t.Fatalf("prompt preview rune length = %d, want <= 101", got)
	}
	if !strings.HasSuffix(preview, "\u2026") {
		t.Fatalf("truncated preview should end with ellipsis: %q", preview)
	}

	description := strings.Repeat("界", 61)
	descriptionPreview := pipelineDescriptionPreview(description)
	if !utf8.ValidString(descriptionPreview) {
		t.Fatalf("description preview is not valid UTF-8: %q", descriptionPreview)
	}
	if got := len([]rune(descriptionPreview)); got != 61 {
		t.Fatalf("description preview rune length = %d, want 61", got)
	}
	if !strings.HasSuffix(descriptionPreview, "\u2026") {
		t.Fatalf("truncated description should end with ellipsis: %q", descriptionPreview)
	}
}

func TestPipelineStageBadgesOrchestrateAndProduce(t *testing.T) {
	orchestrate := pipeline.Stage{ID: "coord", Agent: "lead", Action: "ask", Prompt: "run the tree", Orchestrate: true}
	if got := pipelineStageBadge(orchestrate); !strings.Contains(got, "ORCHESTRATE") {
		t.Fatalf("orchestrate badge = %q", got)
	}
	produce := pipeline.Stage{ID: "export", Agent: "producer", Action: "produce", Prompt: "write data", Write: true, Writes: []string{"/data"}}
	if got := pipelineStageBadge(produce); !strings.Contains(got, "PRODUCE") {
		t.Fatalf("produce badge = %q", got)
	}
}
