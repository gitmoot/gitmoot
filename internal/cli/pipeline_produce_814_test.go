package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestPipelineProduceStageJobRequestShapeAndRetryNote(t *testing.T) {
	retries := 2
	stage := pipeline.Stage{ID: "export", Agent: "producer", Action: "produce", Prompt: "Write data.", Write: true, Writes: []string{"/data/a", "/data/b"}, Network: true, Check: "test -s /data/a/out", CheckRetries: &retries}
	req := pipelineStageJobRequest(db.Pipeline{Name: "p", Repo: "owner/repo"}, stage, db.PipelineRun{ID: "prun-p-1"}, 1, "UPSTREAM\n", pipelineStagePRBinding{}, false)
	if req.Action != "produce" || req.Sender != workflow.PipelineJobSender || req.Branch != "" || req.TaskID != "" || req.PullRequest != 0 {
		t.Fatalf("produce request identity fields = %+v", req)
	}
	if len(req.WritablePaths) != 2 || !req.Network || req.CheckRetries != 2 || req.Check == "" {
		t.Fatalf("produce request options = %+v", req)
	}
	if !strings.Contains(req.Instructions, "previous attempt may have written partial data") {
		t.Fatalf("retry instructions missing reconciliation note: %q", req.Instructions)
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
