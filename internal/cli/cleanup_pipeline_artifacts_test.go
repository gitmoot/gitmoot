package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func newCleanupServiceWorld(t *testing.T) (cleanupCLIWorld, workflow.Engine, string) {
	t.Helper()
	world := newCleanupCLIWorld(t, "succeeded", "ask")
	const runID = "psr-0123456789abcdef0123456789abcdef"
	const spec = "name: cleanup-artifacts\nstages:\n  - id: build\n    cmd: 'printf build'\n  - id: consume\n    cmd: 'printf consume'\n    needs: [build]\n"
	_, base, sourceSpec, _, err := pipeline.PipelineServiceRunPaths(world.paths, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sourceSpec), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceSpec, []byte(spec), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := world.store.CreatePipelineRun(ctx, db.PipelineRun{ID: runID, Pipeline: "cleanup-artifacts", Trigger: "service", State: "running"}); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []db.PipelineRunStage{
		{RunID: runID, StageID: "build", State: "succeeded", JobID: world.job.ID},
		{RunID: runID, StageID: "consume", State: "blocked"},
	} {
		if err := world.store.CreatePipelineRunStage(ctx, stage); err != nil {
			t.Fatal(err)
		}
	}
	world.payload.Sender = workflow.PipelineJobSender
	world.payload.RuntimeOverride = "shell"
	world.payload.RootJobID = runID
	world.payload.PipelineName = "cleanup-artifacts"
	world.payload.TaskID = ""
	encoded, err := json.Marshal(world.payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobPayload(ctx, world.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(world.path, "out"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world.path, "out", "result.txt"), []byte("first generation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	engine := daemonWorkflowEngine(world.store, nil, world.checkout, world.paths.Home)
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	return world, engine, filepath.Join(base, "artifacts", "build", "result.txt")
}

func TestCleanupPublishesForBlockedPipelineAndRefusesLostArtifacts(t *testing.T) {
	for _, loseArtifact := range []bool{false, true} {
		t.Run(fmt.Sprintf("artifact_lost=%t", loseArtifact), func(t *testing.T) {
			world, engine, publication := newCleanupServiceWorld(t)
			ctx := context.Background()
			removed, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.job.ID)
			if err != nil || removed {
				t.Fatalf("blocked consumer cleanup: removed=%t err=%v", removed, err)
			}
			content, err := os.ReadFile(publication)
			if err != nil || string(content) != "first generation\n" {
				t.Fatalf("blocked consumer did not receive published input: %q %v", content, err)
			}
			job, err := world.store.GetJob(ctx, world.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := workflow.ParseJobPayload(job.Payload)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := payload.CleanupArtifacts
			if snapshot == nil {
				t.Fatal("publication did not preserve generation evidence")
			}
			artifact := filepath.Join(snapshot.Directory, "files", "result.txt")
			if loseArtifact {
				if err := os.Remove(artifact); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(world.path, "result.txt"), []byte("diagnostic after artifact loss\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if removed, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.job.ID); err == nil || removed {
					t.Fatalf("missing immutable artifact did not refuse cleanup: removed=%t err=%v", removed, err)
				}
				if _, err := os.Stat(world.path); err != nil {
					t.Fatalf("preservation failure removed producer: %v", err)
				}
				job, err := world.store.GetJob(ctx, world.job.ID)
				if err != nil {
					t.Fatal(err)
				}
				preserved, err := workflow.ParseJobPayload(job.Payload)
				if err != nil || !strings.Contains(preserved.ReadOnlyWorktreeDiff, "+diagnostic after artifact loss\n") {
					t.Fatalf("artifact failure prevented durable diff collection: %+v %v", preserved, err)
				}
				return
			}
			if err := world.store.UpdatePipelineRunStage(ctx, db.PipelineRunStage{RunID: payload.RootJobID, StageID: "consume", State: "succeeded"}); err != nil {
				t.Fatal(err)
			}
			if err := world.store.UpdatePipelineRun(ctx, db.PipelineRun{ID: payload.RootJobID, Pipeline: "cleanup-artifacts", State: "succeeded"}); err != nil {
				t.Fatal(err)
			}
			if removed, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.job.ID); err != nil || !removed {
				t.Fatalf("satisfied pipeline did not release producer: removed=%t err=%v", removed, err)
			}
			if _, err := os.Stat(world.path); !os.IsNotExist(err) {
				t.Fatalf("producer remained after satisfied consumers: %v", err)
			}
			content, err = os.ReadFile(artifact)
			if err != nil || string(content) != "first generation\n" {
				t.Fatalf("required artifact did not outlive workspace: %q %v", content, err)
			}
		})
	}
}

func TestCleanupArtifactSnapshotsSurviveGenerationReplacement(t *testing.T) {
	world, engine, publication := newCleanupServiceWorld(t)
	ctx := context.Background()
	if _, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.job.ID); err != nil {
		t.Fatal(err)
	}
	firstJob, err := world.store.GetJob(ctx, world.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := workflow.ParseJobPayload(firstJob.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if first.CleanupArtifacts == nil {
		t.Fatal("first generation snapshot missing")
	}
	if err := world.store.UpdateJobState(ctx, world.job.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	retried, err := workflow.RetryJob(ctx, world.store, world.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := workflow.ParseJobPayload(retried.Payload)
	if err != nil {
		t.Fatal(err)
	}
	next.WorktreePath = world.path
	next.Result = &workflow.AgentResult{Decision: "approved", Summary: "second terminal output"}
	encoded, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobPayload(ctx, world.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world.path, "out", "result.txt"), []byte("second generation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobState(ctx, world.job.ID, "succeeded"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.job.ID); err != nil {
		t.Fatal(err)
	}
	secondJob, err := world.store.GetJob(ctx, world.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := workflow.ParseJobPayload(secondJob.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if second.CleanupArtifacts == nil || second.CleanupArtifacts.Directory == first.CleanupArtifacts.Directory {
		t.Fatal("new generation reused mutable artifact evidence")
	}
	for path, want := range map[string]string{
		filepath.Join(first.CleanupArtifacts.Directory, "files", "result.txt"):  "first generation\n",
		filepath.Join(second.CleanupArtifacts.Directory, "files", "result.txt"): "second generation\n",
		publication: "second generation\n",
	} {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != want {
			t.Fatalf("artifact %s: %q %v, want %q", path, content, err, want)
		}
	}
}
