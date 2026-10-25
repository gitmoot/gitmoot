package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type cleanupPublicationRetryRunner struct {
	subprocess.ExecRunner
	once   sync.Once
	before func()
}

func (r *cleanupPublicationRetryRunner) RunExactEnv(ctx context.Context, dir string, env []string, stdout, stderr io.Writer, command string, args ...string) error {
	r.once.Do(r.before)
	return r.ExecRunner.RunExactEnv(ctx, dir, env, stdout, stderr, command, args...)
}

func TestCleanupDiffPublicationDoesNotOverwriteRetriedGeneration(t *testing.T) {
	world := newCleanupCLIWorld(t, "succeeded", "review")
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(world.path, "result.txt"), []byte("old generation edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &cleanupPublicationRetryRunner{before: func() {
		if err := world.store.UpdateJobState(ctx, world.job.ID, "failed"); err != nil {
			t.Fatal(err)
		}
		retried, err := workflow.RetryJob(ctx, world.store, world.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := workflow.ParseJobPayload(retried.Payload)
		if err != nil {
			t.Fatal(err)
		}
		payload.ReadOnlyWorktreeDiff = "new generation evidence"
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := world.store.UpdateJobPayload(ctx, world.job.ID, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}}
	hook := askReviewDiffPrecleanupHookForRunner(world.store, runner)
	if err := hook(ctx, world.job.ID, world.job.Type, world.payload); err == nil {
		t.Fatal("old capture was accepted after retry changed its owner generation")
	}
	current, err := world.store.GetJob(ctx, world.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(current.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "queued" || current.LifecycleGeneration != world.job.LifecycleGeneration+1 || payload.ReadOnlyWorktreeDiff != "new generation evidence" {
		t.Fatalf("old publication overwrote the retried lifecycle: job=%+v diff=%q", current, payload.ReadOnlyWorktreeDiff)
	}
}
