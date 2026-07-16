package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestPipelineProduceStageJobRequestShapeAndRetryNote(t *testing.T) {
	retries := 2
	stage := pipeline.Stage{ID: "export", Agent: "producer", Action: "produce", Prompt: "Write data.", Write: true, Writes: []string{"/data/a", "/data/b"}, Reads: []string{"/input/a"}, Network: true, Check: "test -s /data/a/out", CheckRetries: &retries}
	req := pipelineStageJobRequest(db.Pipeline{Name: "p", Repo: "owner/repo"}, stage, db.PipelineRun{ID: "prun-p-1"}, 1, "UPSTREAM\n", pipelineStagePRBinding{}, false)
	if req.Action != "produce" || req.Sender != workflow.PipelineJobSender || req.Branch != "" || req.TaskID != "" || req.PullRequest != 0 {
		t.Fatalf("produce request identity fields = %+v", req)
	}
	if len(req.WritablePaths) != 2 || len(req.ReadablePaths) != 1 || req.ReadablePaths[0] != "/input/a" || req.PipelineName != "p" || !req.Network || req.CheckRetries != 2 || req.Check == "" {
		t.Fatalf("produce request options = %+v", req)
	}
	if !strings.Contains(req.Instructions, "previous attempt may have written partial data") {
		t.Fatalf("retry instructions missing reconciliation note: %q", req.Instructions)
	}
	// Byte-exact pre-#863 ordering when no trigger payload is present: the
	// reconcile note keeps top-of-prompt salience AHEAD of upstream context.
	if want := "A previous attempt may have written partial data into your writable paths; reconcile/idempotently overwrite rather than duplicating.\n\nUPSTREAM\nWrite data."; req.Instructions != want {
		t.Fatalf("retry instructions ordering = %q, want %q", req.Instructions, want)
	}
	if !pipelineStageReadOnlyWorktreeEligible(req) {
		t.Fatal("produce request should use a disposable detached worktree cwd")
	}
}

func TestApplyProduceRuntimeGrantsRevalidatesSymlinkAndScopesByAction(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(config.PathsForHome(home).Home, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	safe := filepath.Join(base, "safe")
	if err := os.MkdirAll(safe, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "target")
	if err := os.Symlink(safe, link); err != nil {
		t.Fatal(err)
	}
	payload := workflow.JobPayload{WritablePaths: []string{link}, Network: true}
	addSpec := pipeline.Spec{Name: "p", Stages: []pipeline.Stage{{ID: "export", Agent: "p", Action: "produce", Prompt: "x", Write: true, Writes: []string{link}}}}
	if err := validatePipelineProducePaths(context.Background(), store, home, addSpec); err != nil {
		t.Fatalf("add-time path validation: %v", err)
	}
	agent := runtime.Agent{Name: "p"}
	if err := applyProduceRuntimeGrants(context.Background(), store, home, db.Job{ID: "p1", Type: "produce"}, payload, &agent); err != nil {
		t.Fatalf("safe delivery preflight: %v", err)
	}
	if len(agent.WritablePaths) != 1 || agent.WritablePaths[0] != safe || !agent.ProduceNetwork {
		t.Fatalf("canonical grants = %+v network=%v", agent.WritablePaths, agent.ProduceNetwork)
	}

	// A malicious/stale payload on a non-produce job must never reach the runtime.
	nonProduce := runtime.Agent{Name: "a"}
	if err := applyProduceRuntimeGrants(context.Background(), store, home, db.Job{ID: "a1", Type: "ask"}, payload, &nonProduce); err != nil {
		t.Fatalf("non-produce preflight: %v", err)
	}
	if len(nonProduce.WritablePaths) != 0 || nonProduce.ProduceNetwork {
		t.Fatalf("non-produce agent leaked grants: %+v", nonProduce)
	}

	// Simulate TOCTOU after pipeline add: the formerly-safe symlink is retargeted
	// into the Gitmoot home before delivery. The worker's shared checker must refuse.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(config.PathsForHome(home).Home, link); err != nil {
		t.Fatal(err)
	}
	agent = runtime.Agent{Name: "p"}
	err = applyProduceRuntimeGrants(context.Background(), store, home, db.Job{ID: "p2", Type: "produce"}, payload, &agent)
	if err == nil || !strings.Contains(err.Error(), "produce writable path preflight failed") || len(agent.WritablePaths) != 0 {
		t.Fatalf("retargeted symlink preflight = %v agent=%+v", err, agent)
	}
}

func TestApplyProduceRuntimeReadableGrantsRevalidateAtDelivery(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	keychainPath := filepath.Join(base, "operator-keychain", "keychain.env")
	if err := os.MkdirAll(filepath.Dir(keychainPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := config.DefaultConfig(paths) + "\n[credentials]\nkeychain_path = \"" + keychainPath + "\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	readTarget := filepath.Join(base, "input")
	writeTarget := filepath.Join(base, "output")
	for _, dir := range []string{readTarget, writeTarget} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	readLink := filepath.Join(base, "read-link")
	if err := os.Symlink(readTarget, readLink); err != nil {
		t.Fatal(err)
	}
	specYAML := "name: p\nstages:\n  - id: export\n    agent: p\n    action: produce\n    prompt: x\n    write: true\n    writes: [" + writeTarget + "]\n    reads: [" + readLink + "]\n"
	if err := store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{Name: "p", SpecYAML: specYAML}); err != nil {
		t.Fatal(err)
	}
	payload := workflow.JobPayload{PipelineName: "p", WritablePaths: []string{writeTarget}, ReadablePaths: []string{readLink}}
	agent := runtime.Agent{Name: "p"}
	if err := applyProduceRuntimeGrants(context.Background(), store, home, db.Job{ID: "produce-read", Type: "produce"}, payload, &agent); err != nil {
		t.Fatalf("safe delivery preflight: %v", err)
	}
	if len(agent.ReadablePaths) != 1 || agent.ReadablePaths[0] != readTarget || len(agent.WritablePaths) != 1 || agent.WritablePaths[0] != writeTarget {
		t.Fatalf("runtime grants = reads %v writes %v", agent.ReadablePaths, agent.WritablePaths)
	}

	if err := os.Remove(readLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(paths.Home, readLink); err != nil {
		t.Fatal(err)
	}
	agent = runtime.Agent{Name: "p"}
	err = applyProduceRuntimeGrants(context.Background(), store, home, db.Job{ID: "produce-read", Type: "produce"}, payload, &agent)
	if err == nil || !strings.Contains(err.Error(), "produce readable path preflight failed") || len(agent.ReadablePaths) != 0 {
		t.Fatalf("retargeted read symlink preflight = %v agent=%+v", err, agent)
	}
}

func TestApplyProduceRuntimeGrantsAutoIncludesClaudeHooksAndPreservesNoReads(t *testing.T) {
	base := t.TempDir()
	operatorHome := filepath.Join(base, "operator")
	configDir := filepath.Join(operatorHome, ".claude")
	hookDir := filepath.Join(base, "operator-hooks")
	inputDir := filepath.Join(base, "input")
	outputDir := filepath.Join(base, "output")
	for _, dir := range []string{configDir, hookDir, inputDir, outputDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", operatorHome)
	t.Setenv(runtime.ClaudeConfigDirEnv, "")
	hookPath := filepath.Join(hookDir, "prompt-hook.sh")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaudeProduceSettings(t, configDir, fmt.Sprintf(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":%q}]}]}}`, hookPath))
	userState := filepath.Join(operatorHome, ".claude.json")
	if err := os.WriteFile(userState, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(base, "config-home")
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	createProduceGrantPipeline(t, store, inputDir, outputDir)
	job := db.Job{ID: "claude-hooks", Agent: "producer", Type: "produce", State: string(workflow.JobQueued)}
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	payload := workflow.JobPayload{PipelineName: "p", WritablePaths: []string{outputDir}, ReadablePaths: []string{inputDir}}
	agent := runtime.Agent{Name: "producer", Runtime: runtime.ClaudeRuntime}
	if err := applyProduceRuntimeGrants(context.Background(), store, home, job, payload, &agent); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{inputDir, configDir, hookDir} {
		if !containsString(agent.ReadablePaths, want) {
			t.Fatalf("readable paths = %v, missing %q", agent.ReadablePaths, want)
		}
	}
	if len(agent.ReadableFiles) != 1 || agent.ReadableFiles[0] != userState {
		t.Fatalf("readable files = %v, want %q", agent.ReadableFiles, userState)
	}

	withoutReads := runtime.Agent{Name: "producer", Runtime: runtime.ClaudeRuntime}
	if err := applyProduceRuntimeGrants(context.Background(), store, home, db.Job{ID: "no-reads", Type: "produce"}, workflow.JobPayload{WritablePaths: []string{outputDir}}, &withoutReads); err != nil {
		t.Fatal(err)
	}
	if withoutReads.ReadablePaths != nil || withoutReads.ReadableFiles != nil {
		t.Fatalf("no-reads stage gained runtime reads: dirs=%v files=%v", withoutReads.ReadablePaths, withoutReads.ReadableFiles)
	}
}

func TestApplyProduceRuntimeGrantsRefusesProtectedClaudeHookLoudly(t *testing.T) {
	base := t.TempDir()
	operatorHome := filepath.Join(base, "operator")
	configDir := filepath.Join(operatorHome, ".claude")
	inputDir := filepath.Join(base, "input")
	outputDir := filepath.Join(base, "output")
	for _, dir := range []string{configDir, inputDir, outputDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", operatorHome)
	t.Setenv(runtime.ClaudeConfigDirEnv, "")
	home := filepath.Join(base, "config-home")
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)), 0o600); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(paths.Home, "protected-hook.sh")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaudeProduceSettings(t, configDir, fmt.Sprintf(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":%q}]}]}}`, hookPath))
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	createProduceGrantPipeline(t, store, inputDir, outputDir)
	agent := runtime.Agent{Name: "producer", Runtime: runtime.ClaudeRuntime}
	err = applyProduceRuntimeGrants(context.Background(), store, home, db.Job{ID: "protected", Type: "produce"}, workflow.JobPayload{
		PipelineName: "p", WritablePaths: []string{outputDir}, ReadablePaths: []string{inputDir},
	}, &agent)
	if err == nil || !strings.Contains(err.Error(), hookPath) || !strings.Contains(err.Error(), "Gitmoot home") || !strings.Contains(err.Error(), "reads:") {
		t.Fatalf("protected hook preflight = %v", err)
	}
	if agent.ReadablePaths != nil || agent.ReadableFiles != nil {
		t.Fatalf("failed preflight mutated agent reads: dirs=%v files=%v", agent.ReadablePaths, agent.ReadableFiles)
	}
}

func TestApplyProduceRuntimeGrantsRecordsClaudeHookParseWarning(t *testing.T) {
	base := t.TempDir()
	operatorHome := filepath.Join(base, "operator")
	configDir := filepath.Join(operatorHome, ".claude")
	inputDir := filepath.Join(base, "input")
	outputDir := filepath.Join(base, "output")
	for _, dir := range []string{configDir, inputDir, outputDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", operatorHome)
	t.Setenv(runtime.ClaudeConfigDirEnv, "")
	writeClaudeProduceSettings(t, configDir, `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"./relative-hook.sh"}]}]}}`)
	home := filepath.Join(base, "config-home")
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	createProduceGrantPipeline(t, store, inputDir, outputDir)
	job := db.Job{ID: "hook-warning", Agent: "producer", Type: "produce", State: string(workflow.JobQueued)}
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{Name: "producer", Runtime: runtime.ClaudeRuntime}
	if err := applyProduceRuntimeGrants(context.Background(), store, home, job, workflow.JobPayload{
		PipelineName: "p", WritablePaths: []string{outputDir}, ReadablePaths: []string{inputDir},
	}, &agent); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "produce_runtime_resource_warning" && strings.Contains(event.Message, "relative path") && strings.Contains(event.Message, "UserPromptSubmit") {
			return
		}
	}
	t.Fatalf("Claude hook warning event missing: %+v", events)
}

func writeClaudeProduceSettings(t *testing.T, configDir, body string) {
	t.Helper()
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func createProduceGrantPipeline(t *testing.T, store *db.Store, inputDir, outputDir string) {
	t.Helper()
	specYAML := "name: p\nstages:\n  - id: export\n    agent: producer\n    action: produce\n    prompt: x\n    write: true\n    reads: [" + inputDir + "]\n    writes: [" + outputDir + "]\n"
	if err := store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{Name: "p", SpecYAML: specYAML}); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineProduceWorktreeAllocationFailsClosedButAskFailsOpen(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	home := t.TempDir()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo"}); err != nil {
		t.Fatal(err)
	}
	const produceSpec = `name: produce-cwd
repo: owner/repo
stages:
  - id: export
    agent: producer
    action: produce
    prompt: write data
    write: true
    writes: [/tmp/gitmoot-produce-cwd-test]
`
	rec, spec := newTestPipeline(t, store, "produce-cwd", produceSpec)
	run := startTestRun(t, store, rec, spec, newPipelineStageEnqueuer(store, home), time.Now().UTC())
	if run.State != pipeline.RunFailed {
		t.Fatalf("produce run state = %q, want failed", run.State)
	}
	stage := stageRow(t, store, run.ID, "export")
	if stage.State != pipeline.StageFailed || !strings.Contains(stage.Summary, "requires a disposable detached worktree") {
		t.Fatalf("produce stage = %+v", stage)
	}
	events, err := store.ListJobEvents(ctx, stage.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPipelineProduceEvent(events, "produce_worktree_failed") {
		t.Fatalf("produce job events = %+v", events)
	}

	const askSpec = `name: ask-cwd
repo: owner/repo
stages:
  - id: inspect
    agent: asker
    prompt: inspect
`
	askRec, askParsed := newTestPipeline(t, store, "ask-cwd", askSpec)
	askRun := startTestRun(t, store, askRec, askParsed, newPipelineStageEnqueuer(store, home), time.Now().UTC())
	askStage := stageRow(t, store, askRun.ID, "inspect")
	if askRun.State != pipeline.RunRunning || askStage.State != pipeline.StageQueued {
		t.Fatalf("ask fail-open changed: run=%q stage=%+v", askRun.State, askStage)
	}
}

func hasPipelineProduceEvent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func TestValidatePipelineProducePathsRejectsProtectedAndSymlinkedTargets(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(config.PathsForHome(home).Home, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkout := filepath.Join(base, "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"home":             config.PathsForHome(home).Home,
		"checkout parent":  base,
		"symlink checkout": filepath.Join(link, "data"),
		"root":             string(filepath.Separator),
	} {
		t.Run(name, func(t *testing.T) {
			spec := pipeline.Spec{Name: "p", Stages: []pipeline.Stage{{ID: "a", Agent: "p", Action: "produce", Prompt: "x", Write: true, Writes: []string{target}}}}
			if err := validatePipelineProducePaths(context.Background(), store, home, spec); err == nil {
				t.Fatalf("target %q was accepted", target)
			}
		})
	}
}

func TestValidatePipelineProduceReadPaths(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "operator-home")
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	keychainPath := filepath.Join(base, "operator-keychain", "keychain.env")
	if err := os.MkdirAll(filepath.Dir(keychainPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := config.DefaultConfig(paths) + "\n[credentials]\nkeychain_path = \"" + keychainPath + "\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	writeDir := filepath.Join(base, "outputs")
	readDir := filepath.Join(base, "inputs")
	writeInsideRead := filepath.Join(base, "shared", "outputs")
	for _, dir := range []string{writeDir, readDir, writeInsideRead} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(keychainPath, []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envDir := filepath.Join(base, "pipeline-env")
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(envDir, "secrets.env")
	if err := os.WriteFile(envFile, []byte("OTHER=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		reads   []string
		writes  []string
		envFile string
		want    string
	}{
		{name: "inside home", reads: []string{filepath.Join(paths.Home, "state")}, writes: []string{writeDir}, want: "Gitmoot home"},
		{name: "contains keychain", reads: []string{filepath.Dir(keychainPath)}, writes: []string{writeDir}, want: "keychain_path"},
		{name: "contains env file", reads: []string{envDir}, writes: []string{writeDir}, envFile: envFile, want: "env_file"},
		{name: "equals write", reads: []string{writeDir}, writes: []string{writeDir}, want: "contains a declared writes path"},
		{name: "contains write", reads: []string{filepath.Dir(writeInsideRead)}, writes: []string{writeInsideRead}, want: "contains a declared writes path"},
		{name: "valid", reads: []string{readDir}, writes: []string{writeDir}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := pipeline.Spec{Name: "p", EnvFile: tt.envFile, Stages: []pipeline.Stage{{
				ID: "produce", Agent: "p", Action: "produce", Prompt: "x", Write: true,
				Writes: tt.writes, Reads: tt.reads,
			}}}
			err := validatePipelineProducePaths(context.Background(), store, home, spec)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("valid read path rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if (tt.name == "contains keychain" && strings.Contains(err.Error(), keychainPath)) || (tt.name == "contains env file" && strings.Contains(err.Error(), envFile)) {
				t.Fatalf("secret source path leaked in error: %v", err)
			}
		})
	}
}
