package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func TestSkillOptTrainInitTemplatesJSON(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("custom-writer", "Write short posts.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:         "writer",
		Role:         "writer",
		Runtime:      "codex",
		TemplateID:   "custom-writer",
		Capabilities: []string{"ask"},
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "templates", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("templates exit code = %d, stderr=%s", code, stderr.String())
	}
	var choices []skillopt.TrainInitTemplateChoice
	if err := json.Unmarshal(stdout.Bytes(), &choices); err != nil {
		t.Fatalf("decode choices: %v\n%s", err, stdout.String())
	}
	planner := cliTrainInitChoiceByID(t, choices, "planner")
	if planner.Source != skillopt.TrainInitTemplateChoiceSourceBuiltin || planner.Label == "" {
		t.Fatalf("planner choice = %+v", planner)
	}
	custom := cliTrainInitChoiceByID(t, choices, "custom-writer")
	if !custom.Installed || !custom.Current || custom.CurrentVersion != "custom-writer@v1" || len(custom.Agents) != 1 || custom.Agents[0] != "writer" {
		t.Fatalf("custom choice = %+v", custom)
	}
}

func TestSkillOptTrainInitTemplatesRequiresJSON(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "templates", "--home", home}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "requires --json") {
		t.Fatalf("templates without json code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSkillOptTrainInitCreatesScaffold(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "smithyx-x-posts",
		"--template", "planner",
		"--review-repo", "jerryfane/gitmoot-x-posts-smithyx",
		"--task-kind", "writing",
		"--artifact-kind", "text",
		"--preview", "text-table",
		"--mode", "explore",
		"--request", "Improve the one-shot X post reply voice.",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "next: gitmoot skillopt train start --config .gitmoot/skillopt/smithyx-x-posts/config.toml") {
		t.Fatalf("train init stdout missing next command:\n%s", stdout.String())
	}
	configPath := filepath.Join(workspace, ".gitmoot", "skillopt", "smithyx-x-posts", "config.toml")
	cfg, err := skillopt.LoadTrainInitConfig(configPath)
	if err != nil {
		t.Fatalf("LoadTrainInitConfig returned error: %v", err)
	}
	if cfg.Template != "planner" || cfg.TemplateVersion == "" || cfg.ReviewRepo != "jerryfane/gitmoot-x-posts-smithyx" || cfg.TaskKind != "writing" || cfg.ArtifactKind != "text" || cfg.Preview != "text-table" {
		t.Fatalf("unexpected config = %+v", cfg)
	}
	if cfg.Generation.Source != skillopt.TrainInitGenerationSourceCurrentSkill || cfg.Evaluator.Mode != skillopt.TrainInitEvaluatorModeJudge || cfg.Optimizer.InternalTargetAdapter != skillopt.TrainInitInternalTargetAdapterCodex || cfg.FinalEvaluatorEnabled {
		t.Fatalf("unexpected defaults = %+v", cfg)
	}
	task, err := os.ReadFile(filepath.Join(workspace, ".gitmoot", "skillopt", "smithyx-x-posts", "task.md"))
	if err != nil {
		t.Fatalf("ReadFile task.md returned error: %v", err)
	}
	if strings.TrimSpace(string(task)) != "Improve the one-shot X post reply voice." {
		t.Fatalf("task.md = %q", string(task))
	}
	reviewItems, err := os.ReadFile(filepath.Join(workspace, ".gitmoot", "skillopt", "smithyx-x-posts", "review-items.yml"))
	if err != nil {
		t.Fatalf("ReadFile review-items.yml returned error: %v", err)
	}
	if !strings.Contains(string(reviewItems), "item-001") || !strings.Contains(string(reviewItems), "item-002") {
		t.Fatalf("review-items.yml = %q", string(reviewItems))
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	template, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate planner returned error: %v", err)
	}
	if template.VersionID != cfg.TemplateVersion {
		t.Fatalf("template version = %q, config version = %q", template.VersionID, cfg.TemplateVersion)
	}
}

func TestSkillOptTrainInitCompletesFromPromptAnswers(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	scope := skillOptTrainInitPromptScope(workspace, "")
	restoreFetcher := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restoreFetcher()
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--prompts", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("interactive train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "interactive_prompts_created: 6") || !strings.Contains(stdout.String(), "gitmoot interactive answer --home "+home+" "+skillOptTrainInitPromptID(scope, "template")+" <value>") {
		t.Fatalf("prompt stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "rerun gitmoot skillopt train init --home "+home) {
		t.Fatalf("prompt stdout missing home-aware rerun command = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"interactive", "show", "--home", home, skillOptTrainInitPromptID(scope, "template"), "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("show template prompt exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var templatePrompt db.InteractivePrompt
	if err := json.Unmarshal(stdout.Bytes(), &templatePrompt); err != nil {
		t.Fatalf("decode template prompt: %v\n%s", err, stdout.String())
	}
	if templatePrompt.AnswerFormat != "text" || len(templatePrompt.Choices) != 0 {
		t.Fatalf("template prompt should accept template refs without choice restriction: %+v", templatePrompt)
	}

	answers := map[string]string{
		"name":          "prompt-flow",
		"template":      "planner",
		"review-repo":   "gitmoot/gitmoot",
		"artifact-kind": "text",
		"preview":       "text-table",
		"request":       "Improve planner summaries.",
	}
	for field, value := range answers {
		stdout.Reset()
		stderr.Reset()
		code = Run([]string{"interactive", "answer", "--home", home, skillOptTrainInitPromptID(scope, strings.ReplaceAll(field, "-", "_")), value, "--source", "agent"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("answer %s exit code = %d, stdout=%s stderr=%s", field, code, stdout.String(), stderr.String())
		}
	}

	restoreInteractive()
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("prompt-backed train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	cfg, err := skillopt.LoadTrainInitConfig(filepath.Join(workspace, ".gitmoot", "skillopt", "prompt-flow", "config.toml"))
	if err != nil {
		t.Fatalf("LoadTrainInitConfig returned error: %v", err)
	}
	if cfg.Name != "prompt-flow" || cfg.Template != "planner" || cfg.ReviewRepo != "gitmoot/gitmoot" || cfg.ArtifactKind != "text" || cfg.Preview != "text-table" || cfg.Mode != db.EvalRunModeExplore {
		t.Fatalf("config from prompts = %+v", cfg)
	}
	task, err := os.ReadFile(filepath.Join(workspace, ".gitmoot", "skillopt", "prompt-flow", "task.md"))
	if err != nil {
		t.Fatalf("ReadFile task.md returned error: %v", err)
	}
	if strings.TrimSpace(string(task)) != "Improve planner summaries." {
		t.Fatalf("task.md = %q", string(task))
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	prompts, err := store.ListInteractivePrompts(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInteractivePrompts returned error: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("prompt answers should be consumed after scaffold creation: %+v", prompts)
	}
}

func TestSkillOptTrainInitWizardConfirm(t *testing.T) {
	answers := "confirm-flow\nplanner\ngitmoot/gitmoot\ntext\ntext-table\nImprove planner summaries.\n"
	cases := []struct {
		name         string
		stdin        string
		wantScaffold bool
		wantOut      string
	}{
		{name: "accept with y", stdin: answers + "y\n", wantScaffold: true, wantOut: "Review:"},
		{name: "accept with blank", stdin: answers + "\n", wantScaffold: true, wantOut: "Create scaffold?"},
		{name: "decline with n", stdin: answers + "n\n", wantScaffold: false, wantOut: "aborted: no scaffold written"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			workspace := chdirTemp(t)
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatalf("Initialize returned error: %v", err)
			}
			store, err := db.Open(paths.Database)
			if err != nil {
				t.Fatalf("Open returned error: %v", err)
			}
			if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
				t.Fatalf("UpsertAgentTemplate returned error: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
			restoreInteractive := replaceSkillOptTrainInitInteractive(true)
			defer restoreInteractive()
			restoreStdin := replaceSkillOptTrainInitStdin(tc.stdin)
			defer restoreStdin()

			var stdout, stderr bytes.Buffer
			code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.wantOut) {
				t.Fatalf("stdout missing %q:\n%s", tc.wantOut, stdout.String())
			}
			scaffold := filepath.Join(workspace, ".gitmoot", "skillopt", "confirm-flow", "config.toml")
			_, statErr := os.Stat(scaffold)
			if tc.wantScaffold && statErr != nil {
				t.Fatalf("expected scaffold, stat err = %v", statErr)
			}
			if !tc.wantScaffold && statErr == nil {
				t.Fatalf("declined wizard should not write a scaffold")
			}
		})
	}
}

func TestSkillOptTrainInitWizardCompletesFromStdin(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()
	restoreStdin := replaceSkillOptTrainInitStdin("wizard-flow\nplanner\ngitmoot/gitmoot\ntext\ntext-table\nImprove planner summaries.\n")
	defer restoreStdin()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("wizard train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Choose a template:", "planner", "Custom file", "Preview kind:", "[1/6]", "gitmoot interactive answer "} {
		if !strings.Contains(out, want) {
			t.Fatalf("wizard stdout missing %q:\n%s", want, out)
		}
	}
	// One-time tip, not a per-question hint line.
	if strings.Count(out, "gitmoot interactive answer ") != 1 {
		t.Fatalf("expected exactly one answer-hint tip, got %d:\n%s", strings.Count(out, "gitmoot interactive answer "), out)
	}
	// Version label is not doubled (would render "planner @planner@<ver>").
	if strings.Contains(out, "planner @planner@") {
		t.Fatalf("template version label looks doubled:\n%s", out)
	}
	cfg, err := skillopt.LoadTrainInitConfig(filepath.Join(workspace, ".gitmoot", "skillopt", "wizard-flow", "config.toml"))
	if err != nil {
		t.Fatalf("LoadTrainInitConfig returned error: %v", err)
	}
	if cfg.Name != "wizard-flow" || cfg.Template != "planner" || cfg.ReviewRepo != "gitmoot/gitmoot" || cfg.ArtifactKind != "text" || cfg.Preview != "text-table" || cfg.Mode != db.EvalRunModeExplore {
		t.Fatalf("config from wizard = %+v", cfg)
	}
}

func TestSkillOptTrainInitWizardCompletesFromInteractiveAnswers(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	scope := skillOptTrainInitPromptScope(workspace, "")
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()
	// Blocking stdin (an unwritten pipe) so the wizard can only be driven by
	// `interactive answer`.
	pr, pw := io.Pipe()
	defer pw.Close()
	previousStdin := skillOptTrainInitStdin
	skillOptTrainInitStdin = func() io.Reader { return pr }
	defer func() { skillOptTrainInitStdin = previousStdin }()
	previousPoll := skillOptTrainInitWizardPoll
	skillOptTrainInitWizardPoll = 15 * time.Millisecond
	defer func() { skillOptTrainInitWizardPoll = previousPoll }()

	type wizardResult struct {
		code   int
		stderr string
	}
	resultCh := make(chan wizardResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
		resultCh <- wizardResult{code: code, stderr: stderr.String()}
	}()

	answers := map[string]string{
		"name":          "agent-flow",
		"template":      "planner",
		"review_repo":   "gitmoot/gitmoot",
		"artifact_kind": "text",
		"preview":       "text-table",
		"request":       "Improve planner summaries.",
	}
	for range answers {
		prompt := waitPendingInteractivePrompt(t, home, 5*time.Second)
		field, ok := skillOptTrainInitPromptField(scope, prompt.ID)
		if !ok {
			t.Fatalf("unexpected prompt id %q", prompt.ID)
		}
		value, ok := answers[field]
		if !ok {
			t.Fatalf("no scripted answer for field %q", field)
		}
		var answerOut, answerErr bytes.Buffer
		if code := Run([]string{"interactive", "answer", "--home", home, prompt.ID, value, "--source", "agent"}, &answerOut, &answerErr); code != 0 {
			t.Fatalf("answer %s exit code = %d, stderr=%s", field, code, answerErr.String())
		}
	}

	select {
	case res := <-resultCh:
		if res.code != 0 {
			t.Fatalf("agent-driven wizard exit code = %d, stderr=%s", res.code, res.stderr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("agent-driven wizard did not complete")
	}
	cfg, err := skillopt.LoadTrainInitConfig(filepath.Join(workspace, ".gitmoot", "skillopt", "agent-flow", "config.toml"))
	if err != nil {
		t.Fatalf("LoadTrainInitConfig returned error: %v", err)
	}
	if cfg.Name != "agent-flow" || cfg.Template != "planner" || cfg.ReviewRepo != "gitmoot/gitmoot" || cfg.ArtifactKind != "text" || cfg.Preview != "text-table" {
		t.Fatalf("config from agent answers = %+v", cfg)
	}
}

func TestSkillOptTrainInitWizardIncompleteFailsCleanly(t *testing.T) {
	home := t.TempDir()
	_ = chdirTemp(t)
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()
	// Only the name is answered; stdin then hits EOF for the remaining fields.
	restoreStdin := replaceSkillOptTrainInitStdin("partial-wizard\n")
	defer restoreStdin()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("incomplete wizard exit code = %d, want 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing required fields") {
		t.Fatalf("incomplete wizard stderr = %q", stderr.String())
	}
}

func TestSkillOptTrainInitWizardUnterminatedFinalLineDoesNotHang(t *testing.T) {
	home := t.TempDir()
	_ = chdirTemp(t)
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()
	// The final answered line ("partial") has no trailing newline, so its read
	// returns EOF alongside the value. Later required fields remain, so the
	// wizard must report them and exit instead of blocking on a dead stdin.
	restoreStdin := replaceSkillOptTrainInitStdin("partial")
	defer restoreStdin()

	type result struct {
		code   int
		stderr string
	}
	resultCh := make(chan result, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
		resultCh <- result{code: code, stderr: stderr.String()}
	}()
	select {
	case res := <-resultCh:
		if res.code != 2 {
			t.Fatalf("unterminated wizard exit code = %d, want 2; stderr=%s", res.code, res.stderr)
		}
		if !strings.Contains(res.stderr, "missing required fields") {
			t.Fatalf("unterminated wizard stderr = %q", res.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wizard hung on an unterminated final stdin line")
	}
}

func TestSkillOptTrainInitWizardInterpretChoice(t *testing.T) {
	prompt := db.InteractivePrompt{Choices: []string{"none", "text-table", "vue"}, Default: "text-table"}
	cases := []struct {
		name       string
		input      string
		wantValue  string
		wantStatus string
	}{
		{name: "by number", input: "2", wantValue: "text-table", wantStatus: "ok"},
		{name: "by literal value", input: "vue", wantValue: "vue", wantStatus: "ok"},
		{name: "blank uses default", input: "", wantValue: "text-table", wantStatus: "ok"},
		{name: "out of range reasks", input: "9", wantValue: "", wantStatus: "reask"},
		{name: "unknown reasks", input: "bogus", wantValue: "", wantStatus: "reask"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, status := skillOptTrainInitWizardInterpret("preview", tc.input, prompt, nil)
			if value != tc.wantValue || status != tc.wantStatus {
				t.Fatalf("interpret(%q) = (%q, %q), want (%q, %q)", tc.input, value, status, tc.wantValue, tc.wantStatus)
			}
		})
	}
}

func TestSkillOptTrainInitWizardInterpretTemplate(t *testing.T) {
	choices := []skillopt.TrainInitTemplateChoice{{ID: "planner"}, {ID: "designer"}}
	prompt := db.InteractivePrompt{}
	cases := []struct {
		name       string
		input      string
		wantValue  string
		wantStatus string
	}{
		{name: "by number", input: "1", wantValue: "planner", wantStatus: "ok"},
		{name: "by id", input: "designer", wantValue: "designer", wantStatus: "ok"},
		{name: "custom index", input: "3", wantValue: "", wantStatus: "custom"},
		{name: "literal ref", input: "owner/repo@v2", wantValue: "owner/repo@v2", wantStatus: "ok"},
		{name: "out of range reasks", input: "9", wantValue: "", wantStatus: "reask"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, status := skillOptTrainInitWizardInterpret("template", tc.input, prompt, choices)
			if value != tc.wantValue || status != tc.wantStatus {
				t.Fatalf("interpret(%q) = (%q, %q), want (%q, %q)", tc.input, value, status, tc.wantValue, tc.wantStatus)
			}
		})
	}
}

func TestSkillOptTrainInitNamedPromptRerunIncludesName(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	scope := skillOptTrainInitPromptScope(workspace, "named-flow")
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--prompts", "--home", home, "--name", "named-flow"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("named interactive train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), skillOptTrainInitPromptID(scope, "template")) {
		t.Fatalf("named prompt stdout missing encoded scope:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "rerun gitmoot skillopt train init --home "+home+" --name named-flow") {
		t.Fatalf("named prompt stdout missing name-aware rerun command:\n%s", stdout.String())
	}
}

func TestSkillOptTrainInitPartialDefaultPromptRerunStaysDefaultScoped(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	scope := skillOptTrainInitPromptScope(workspace, "")
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--prompts", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("interactive train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"interactive", "answer", "--home", home, skillOptTrainInitPromptID(scope, "name"), "partial-flow"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("answer name exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "init", "--prompts", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("partial prompt rerun exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "--name partial-flow") {
		t.Fatalf("default-scoped partial rerun should not switch scopes:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), skillOptTrainInitPromptID(scope, "template")) {
		t.Fatalf("default-scoped partial rerun should keep empty prompt ids:\n%s", stdout.String())
	}
}

func TestSkillOptTrainInitPromptRerunPreservesSuppliedFlags(t *testing.T) {
	home := t.TempDir()
	_ = chdirTemp(t)
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()

	requestFile := filepath.Join(t.TempDir(), "request.md")
	if err := os.WriteFile(requestFile, []byte("Improve prompts.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile request returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--prompts",
		"--home", home,
		"--template", "planner",
		"--artifact-kind", "text",
		"--request-file", requestFile,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("interactive train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"--template planner",
		"--artifact-kind text",
		"--request-file " + requestFile,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("prompt rerun command did not preserve %s:\n%s", want, output)
		}
	}
	if strings.Contains(output, "--request Improve prompts") {
		t.Fatalf("prompt rerun command should preserve request-file, not inline request text:\n%s", output)
	}
}

func TestSkillOptTrainInitPromptRerunDropsEmptyRequestFile(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	scope := skillOptTrainInitPromptScope(workspace, "")
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()

	requestFile := filepath.Join(t.TempDir(), "empty-request.md")
	if err := os.WriteFile(requestFile, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatalf("WriteFile request returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--prompts",
		"--home", home,
		"--template", "planner",
		"--request-file", requestFile,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("interactive train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "--request-file") {
		t.Fatalf("empty request-file should not be preserved when request is prompted:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), skillOptTrainInitPromptID(scope, "request")) {
		t.Fatalf("empty request-file should create request prompt:\n%s", stdout.String())
	}
}

func TestSkillOptTrainInitClearsInvalidPromptAnswers(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	scope := skillOptTrainInitPromptScope(workspace, "")
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--prompts", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("interactive train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	answers := map[string]string{
		"name":          "bad-prompt-flow",
		"template":      "planner",
		"review-repo":   "not-a-repo",
		"artifact-kind": "text",
		"preview":       "text-table",
		"request":       "Improve planner summaries.",
	}
	for field, value := range answers {
		stdout.Reset()
		stderr.Reset()
		code = Run([]string{"interactive", "answer", "--home", home, skillOptTrainInitPromptID(scope, strings.ReplaceAll(field, "-", "_")), value, "--source", "agent"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("answer %s exit code = %d, stdout=%s stderr=%s", field, code, stdout.String(), stderr.String())
		}
	}

	restoreInteractive()
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "init", "--home", home}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("invalid prompt-backed init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "review-repo") {
		t.Fatalf("stderr missing review repo validation error: %s", stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	prompts, err := store.ListInteractivePrompts(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInteractivePrompts returned error: %v", err)
	}
	if len(prompts) != 5 {
		t.Fatalf("only the invalid prompt answer should be cleared, got: %+v", prompts)
	}
	for _, prompt := range prompts {
		if strings.Contains(prompt.ID, ".review-repo") {
			t.Fatalf("invalid review repo prompt should be cleared: %+v", prompts)
		}
	}
}

func TestSkillOptTrainInitKeepsPromptAnswersForExplicitFlagErrors(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	scope := skillOptTrainInitPromptScope(workspace, "")
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--prompts", "--home", home, "--preview", "bogus"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("interactive train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	answers := map[string]string{
		"name":          "explicit-error-flow",
		"template":      "planner",
		"review-repo":   "gitmoot/gitmoot",
		"artifact-kind": "text",
		"request":       "Improve planner summaries.",
	}
	for field, value := range answers {
		stdout.Reset()
		stderr.Reset()
		code = Run([]string{"interactive", "answer", "--home", home, skillOptTrainInitPromptID(scope, strings.ReplaceAll(field, "-", "_")), value, "--source", "agent"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("answer %s exit code = %d, stdout=%s stderr=%s", field, code, stdout.String(), stderr.String())
		}
	}

	restoreInteractive()
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "init", "--home", home, "--preview", "bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("explicit invalid preview init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	prompts, err := store.ListInteractivePrompts(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInteractivePrompts returned error: %v", err)
	}
	if len(prompts) != len(answers) {
		t.Fatalf("explicit flag error should not clear prompt answers: %+v", prompts)
	}
	for _, prompt := range prompts {
		if prompt.State != db.InteractivePromptStateResolved {
			t.Fatalf("prompt answer should remain resolved after explicit flag error: %+v", prompt)
		}
	}
}

func TestSkillOptTrainInitFullyFlaggedBypassesPromptCreation(t *testing.T) {
	home := t.TempDir()
	_ = chdirTemp(t)
	restoreFetcher := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restoreFetcher()
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "flagged-flow",
		"--template", "planner",
		"--review-repo", "gitmoot/gitmoot",
		"--task-kind", "writing",
		"--artifact-kind", "text",
		"--preview", "text-table",
		"--request", "Improve this.",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	prompts, err := store.ListInteractivePrompts(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInteractivePrompts returned error: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("fully flagged init created prompts: %+v", prompts)
	}
}

func TestSkillOptTrainInitPromptScopesDoNotCollide(t *testing.T) {
	scopes := map[string]string{
		"workspace-a-empty":   skillOptTrainInitPromptScope("/tmp/workspace-a", ""),
		"workspace-b-empty":   skillOptTrainInitPromptScope("/tmp/workspace-b", ""),
		"workspace-a-default": skillOptTrainInitPromptScope("/tmp/workspace-a", "default"),
		"workspace-a-Foo":     skillOptTrainInitPromptScope("/tmp/workspace-a", "Foo"),
		"workspace-a-foo":     skillOptTrainInitPromptScope("/tmp/workspace-a", "foo"),
	}
	seen := map[string]string{}
	for label, scope := range scopes {
		if previous, ok := seen[scope]; ok {
			t.Fatalf("scope collision: %s and %s both use %s", previous, label, scope)
		}
		seen[scope] = label
	}
}

func TestSkillOptTrainInitAppliesModeDefaults(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "validate-defaults",
		"--template", "planner",
		"--review-repo", "gitmoot/gitmoot",
		"--artifact-kind", "text",
		"--preview", "none",
		"--mode", "validate",
		"--request", "Validate planner summaries.",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train init validate exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	cfg, err := skillopt.LoadTrainInitConfig(filepath.Join(workspace, ".gitmoot", "skillopt", "validate-defaults", "config.toml"))
	if err != nil {
		t.Fatalf("LoadTrainInitConfig returned error: %v", err)
	}
	if cfg.Mode != db.EvalRunModeValidate || cfg.ExplorationLevel != db.ExplorationLevelLow || cfg.Options != 2 {
		t.Fatalf("validate config defaults = mode %q exploration %q options %d", cfg.Mode, cfg.ExplorationLevel, cfg.Options)
	}
}

func TestSkillOptTrainInitMissingRequiredFieldsIsAtomic(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	restoreInteractive := replaceSkillOptTrainInitInteractive(false)
	defer restoreInteractive()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--home", home, "--name", "missing-fields"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("train init missing exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing required fields") || !strings.Contains(stderr.String(), "example: gitmoot skillopt train init") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, ".gitmoot", "skillopt", "missing-fields")); !os.IsNotExist(err) {
		t.Fatalf("scaffold residue err = %v, want not exist", err)
	}
	paths := config.PathsForHome(home)
	if _, err := os.Stat(paths.Database); !os.IsNotExist(err) {
		t.Fatalf("database residue err = %v, want not exist", err)
	}
}

func TestSkillOptTrainInitRejectsMalformedReviewRepo(t *testing.T) {
	home := t.TempDir()
	_ = chdirTemp(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "bad-repo",
		"--template", "planner",
		"--review-repo", "not-a-repo",
		"--task-kind", "writing",
		"--artifact-kind", "text",
		"--preview", "text-table",
		"--request", "Improve this.",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "review-repo") {
		t.Fatalf("bad repo code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSkillOptTrainInitRejectsUnsupportedTaskKind(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "bad-task-kind",
		"--template", "planner",
		"--review-repo", "jerryfane/gitmoot-previews",
		"--task-kind", "writng",
		"--artifact-kind", "text",
		"--preview", "text-table",
		"--request", "Improve this.",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `task kind "writng" is not supported`) {
		t.Fatalf("bad task kind code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, ".gitmoot", "skillopt", "bad-task-kind")); !os.IsNotExist(err) {
		t.Fatalf("scaffold residue err = %v, want not exist", err)
	}
}

func TestSkillOptTrainInitRejectsUnsupportedPreview(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "bad-preview",
		"--template", "planner",
		"--review-repo", "jerryfane/gitmoot-previews",
		"--task-kind", "writing",
		"--artifact-kind", "text",
		"--preview", "custom",
		"--request", "Improve this.",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `preview "custom" is not supported`) {
		t.Fatalf("bad preview code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, ".gitmoot", "skillopt", "bad-preview")); !os.IsNotExist(err) {
		t.Fatalf("scaffold residue err = %v, want not exist", err)
	}
}

func TestSkillOptTrainInitHelpIncludesCreateCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "init", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("help exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gitmoot skillopt train init --name <name>") || !strings.Contains(stdout.String(), "gitmoot skillopt train init templates --json") {
		t.Fatalf("help output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "--help"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "gitmoot skillopt train start --template <id>") {
		t.Fatalf("train help regression code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
