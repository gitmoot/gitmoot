package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestPipelineShellIsolationRequestMarker(t *testing.T) {
	rec := db.Pipeline{Name: "shell-isolation", Repo: "owner/repo"}
	stage := pipeline.Stage{ID: "run", Cmd: "printf ok", Isolate: true}

	manual := pipeline.PipelineStageJobRequest(rec, stage, db.PipelineRun{ID: "manual-run", Trigger: "manual"}, 0, "", pipeline.PipelineStagePRBinding{}, false)
	if !manual.IsolateShellStage {
		t.Fatal("manual isolate:true shell stage did not carry its enqueue-only isolation marker")
	}
	if got := envEntryValue(manual.ShellEnv, "GITMOOT_CHECKOUT"); got != "" {
		t.Fatalf("GITMOOT_CHECKOUT was injected before worktree allocation: %q", got)
	}

	legacy := pipeline.PipelineStageJobRequest(rec, pipeline.Stage{ID: "run", Cmd: "printf ok"}, db.PipelineRun{ID: "legacy-run", Trigger: "manual"}, 0, "", pipeline.PipelineStagePRBinding{}, false)
	if legacy.IsolateShellStage || legacy.ReadOnlyWorktree || legacy.WorktreePath != "" {
		t.Fatalf("default shell request changed from shared-checkout shape: %+v", legacy)
	}

	service := pipeline.PipelineStageJobRequest(rec, stage, db.PipelineRun{ID: "service-run", Trigger: "service"}, 0, "", pipeline.PipelineStagePRBinding{}, false)
	if service.IsolateShellStage {
		t.Fatal("service shell stage entered the non-service fail-open isolation path")
	}
}

func TestPipelineShellIsolationUnmanagedRepoFallsBackSilently(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	request := pipeline.PipelineStageJobRequest(
		db.Pipeline{Name: "shell-unmanaged", Repo: "owner/unmanaged"},
		pipeline.Stage{ID: "run", Cmd: "printf ok", Isolate: true},
		db.PipelineRun{ID: "unmanaged-run", Trigger: "manual"},
		0, "", pipeline.PipelineStagePRBinding{}, false,
	)
	job, err := newPipelineStageEnqueuer(store, home)(ctx, request)
	if err != nil {
		t.Fatalf("enqueue unmanaged isolate:true shell stage: %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.WorktreePath != "" || payload.ReadOnlyWorktree || queuedJobCheckoutKey(ctx, store, job) != "repo:owner/unmanaged" {
		t.Fatalf("unmanaged shell stage did not retain serialized shared-repo shape: %+v", payload)
	}
	if countCLIJobEvents(t, store, job.ID, "readonly_worktree_skipped") != 0 {
		t.Fatal("unmanaged checkout emitted readonly_worktree_skipped, want silent fallback")
	}
	if countCLIJobEvents(t, store, job.ID, "readonly_worktree_allocated") != 0 {
		t.Fatal("unmanaged checkout emitted readonly_worktree_allocated without a worktree")
	}
}

func TestPipelineForkedShellIsolationEventWindowsE2E(t *testing.T) {
	for _, tc := range []struct {
		name    string
		isolate bool
	}{
		{name: "opted-in stages overlap", isolate: true},
		{name: "default stages serialize", isolate: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "throwaway.sock"))
			t.Setenv("HERDR_ENV", "")
			home, _, store := heartbeatLoopE2EHome(t)
			checkout := createDaemonWorkerGitCheckout(t, "main")
			seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
			envFile := writePipelineEnvFile(t, t.TempDir(), "ISOLATION_TOKEN=delivered\n", 0o600)

			pipelineName := "shell-fork-legacy"
			isolateLine := ""
			checkoutCheck := `[ -z "${GITMOOT_CHECKOUT+x}" ] || exit 72`
			if tc.isolate {
				pipelineName = "shell-fork-isolated"
				isolateLine = "    isolate: true\n"
				checkoutCheck = `[ "$GITMOOT_CHECKOUT" = ` + posixQuote(checkout) + ` ] || exit 72`
			}
			childCmd := func(stageID string) string {
				return strings.Join([]string{
					checkoutCheck,
					`[ "$ISOLATION_TOKEN" = delivered ] || exit 73`,
					`[ -r "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE" ] || exit 74`,
					"sleep 2",
					pipelineShellResultCommand(t, "approved", "fork stage "+stageID+" complete"),
				}, "; ")
			}
			specYAML := fmt.Sprintf("name: %s\nrepo: owner/repo\nenv_file: %q\nstages:\n  - id: root\n    cmd: |\n      %s\n  - id: b\n    cmd: |\n      %s\n%s    env_keys: [ISOLATION_TOKEN]\n    needs: [root]\n  - id: c\n    cmd: |\n      %s\n%s    env_keys: [ISOLATION_TOKEN]\n    needs: [root]\n", pipelineName, envFile, pipelineShellResultCommand(t, "approved", "root complete"), childCmd("b"), isolateLine, childCmd("c"), isolateLine)
			runID := addAndStartPipeline(t, home, pipelineName, specYAML)

			enqueue := newPipelineStageEnqueuer(store, home)
			tickWorker := defaultJobWorker(store, io.Discard, home)
			now := time.Now().UTC()
			var b, c db.PipelineRunStage
			for i := 0; i < 8; i++ {
				if err := runEnabledRepoWorkerTicksTracked(ctx, store, tickWorker, 1, "", io.Discard, now, nil, nil); err != nil {
					t.Fatalf("worker tick %d: %v", i, err)
				}
				if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
					t.Fatalf("pipeline scan %d: %v", i, err)
				}
				b = stageRow(t, store, runID, "b")
				c = stageRow(t, store, runID, "c")
				if b.State == pipeline.StageQueued && c.State == pipeline.StageQueued {
					break
				}
			}
			if b.State != pipeline.StageQueued || c.State != pipeline.StageQueued {
				t.Fatalf("fork stages were not both queued: b=%s c=%s", b.State, c.State)
			}

			jobs := make([]db.Job, 0, 2)
			keys := make([]string, 0, 2)
			for _, row := range []db.PipelineRunStage{b, c} {
				job, err := store.GetJob(ctx, row.JobID)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := workflow.ParseJobPayload(job.Payload)
				if err != nil {
					t.Fatal(err)
				}
				key := queuedJobCheckoutKey(ctx, store, job)
				if tc.isolate {
					if payload.WorktreePath == "" || !payload.ReadOnlyWorktree || !strings.HasPrefix(key, "worktree:") {
						t.Fatalf("isolated shell payload/key = %+v / %q", payload, key)
					}
					if got := envEntryValue(payload.ShellEnv, "GITMOOT_CHECKOUT"); got != checkout {
						t.Fatalf("GITMOOT_CHECKOUT = %q, want %q", got, checkout)
					}
					if strings.Contains(payload.Instructions, checkout) {
						t.Fatalf("shell instructions contain agent-only checkout prose: %q", payload.Instructions)
					}
					if n := countCLIJobEvents(t, store, row.JobID, "readonly_worktree_allocated"); n != 1 {
						t.Fatalf("readonly_worktree_allocated events = %d, want 1", n)
					}
				} else {
					if payload.WorktreePath != "" || payload.ReadOnlyWorktree || key != "repo:owner/repo" {
						t.Fatalf("default shell payload/key changed: %+v / %q", payload, key)
					}
					if got := envEntryValue(payload.ShellEnv, "GITMOOT_CHECKOUT"); got != "" {
						t.Fatalf("default shell received GITMOOT_CHECKOUT=%q", got)
					}
				}
				jobs = append(jobs, job)
				keys = append(keys, key)
			}
			if tc.isolate && keys[0] == keys[1] {
				t.Fatalf("isolated fork stages share checkout key %q", keys[0])
			}

			if err := runQueuedJobsForRepo(ctx, tickWorker, 2, "", ""); err != nil {
				t.Fatalf("two-worker dispatch: %v", err)
			}
			for _, job := range jobs {
				settled, _ := terminalJobDecision(t, ctx, store, job.ID)
				if settled.State != string(workflow.JobSucceeded) {
					t.Fatalf("job %s state = %s, want succeeded", job.ID, settled.State)
				}
			}
			bStart, bFinish := pipelineShellJobEventWindow(t, store, b.JobID)
			cStart, cFinish := pipelineShellJobEventWindow(t, store, c.JobID)
			overlapped := bStart.Before(cFinish) && cStart.Before(bFinish)
			if overlapped != tc.isolate {
				t.Fatalf("job-event windows overlap = %v, want %v (b=%s..%s c=%s..%s)", overlapped, tc.isolate, bStart, bFinish, cStart, cFinish)
			}

			if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
				t.Fatal(err)
			}
			run, ok, err := store.GetPipelineRun(ctx, runID)
			if err != nil || !ok || run.State != pipeline.RunSucceeded {
				t.Fatalf("run did not succeed: run=%+v ok=%v err=%v", run, ok, err)
			}
		})
	}
}

func TestPipelineShellIsolationAllocationFailureFallsOpenE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	const pipelineName = "shell-isolation-fail-open"
	specYAML := "name: " + pipelineName + "\nrepo: owner/repo\nstages:\n  - id: root\n    cmd: |\n      " + pipelineShellResultCommand(t, "approved", "root complete") + "\n  - id: run\n    cmd: |\n      " + pipelineShellResultCommand(t, "approved", "shared fallback ran") + "\n    isolate: true\n    needs: [root]\n"
	runID := addAndStartPipeline(t, home, pipelineName, specYAML)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Now().UTC()
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, now, nil, nil); err != nil {
		t.Fatal(err)
	}
	brokenHome := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(brokenHome, []byte("force worktree parent creation failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	enqueue := newPipelineStageEnqueuer(store, brokenHome)
	if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
		t.Fatalf("fail-open enqueue: %v", err)
	}
	row := stageRow(t, store, runID, "run")
	job, err := store.GetJob(ctx, row.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.WorktreePath != "" || payload.ReadOnlyWorktree || queuedJobCheckoutKey(ctx, store, job) != "repo:owner/repo" {
		t.Fatalf("allocation failure did not keep shared-checkout request: %+v", payload)
	}
	if got := envEntryValue(payload.ShellEnv, "GITMOOT_CHECKOUT"); got != "" {
		t.Fatalf("failed allocation injected GITMOOT_CHECKOUT=%q", got)
	}
	if countCLIJobEvents(t, store, row.JobID, "readonly_worktree_skipped") != 1 {
		t.Fatal("allocation failure did not emit readonly_worktree_skipped")
	}
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, now, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := runPipelineScanOnce(ctx, store, newPipelineStageEnqueuer(store, home), now); err != nil {
		t.Fatal(err)
	}
	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil || !ok || run.State != pipeline.RunSucceeded {
		t.Fatalf("fail-open run did not succeed: run=%+v ok=%v err=%v", run, ok, err)
	}
}

func TestPipelineShellDefaultStillMutatesSharedCheckoutE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	const pipelineName = "shell-shared-mutation"
	cmd := "printf legacy-write > shared-output.txt; " + pipelineShellResultCommand(t, "approved", "wrote shared checkout")
	runID := addAndStartPipeline(t, home, pipelineName, "name: "+pipelineName+"\nrepo: owner/repo\nstages:\n  - id: run\n    cmd: |\n      "+cmd+"\n")
	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
			t.Fatal(err)
		}
		if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, now, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(filepath.Join(checkout, "shared-output.txt"))
	if err != nil || string(got) != "legacy-write" {
		t.Fatalf("default shell stage did not write shared checkout: content=%q err=%v", got, err)
	}
	row := stageRow(t, store, runID, "run")
	job, err := store.GetJob(ctx, row.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.WorktreePath != "" || payload.ReadOnlyWorktree || countCLIJobEvents(t, store, row.JobID, "readonly_worktree_allocated") != 0 {
		t.Fatalf("default shell stage unexpectedly isolated: %+v", payload)
	}
}

func addAndStartPipeline(t *testing.T, home, name, specYAML string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", writeSpec(t, specYAML), "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", name, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	return strings.TrimSpace(out.String())
}

func pipelineShellJobEventWindow(t *testing.T, store *db.Store, jobID string) (time.Time, time.Time) {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	var started, finished time.Time
	for _, event := range events {
		switch event.Kind {
		case string(workflow.JobRunning):
			if started.IsZero() {
				started = pipelineEventTime(event.CreatedAt)
			}
		case string(workflow.JobSucceeded):
			finished = pipelineEventTime(event.CreatedAt)
		}
	}
	if started.IsZero() || finished.IsZero() {
		t.Fatalf("job %s has incomplete event window: %s..%s events=%+v", jobID, started, finished, events)
	}
	return started, finished
}
