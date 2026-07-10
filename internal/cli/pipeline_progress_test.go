package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func TestPipelineProgressLineTrackerSanitizesRedactsCapsAndIgnoresEmpty(t *testing.T) {
	tracker := &pipelineProgressLineTracker{}
	_, _ = tracker.Write([]byte("first\n\n\x1b[31mtoken=abcdefghijklmnopqrstuvwxyz1234567890\x1b[0m\x00\n"))
	got := tracker.LastLine()
	if strings.Contains(got, "\x1b") || strings.ContainsRune(got, '\x00') || strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("sanitized line = %q", got)
	}
	_, _ = tracker.Write([]byte(strings.Repeat("\u00e9", 300)))
	got = tracker.LastLine()
	if len(got) > pipelineProgressLineBytes || !strings.Contains(got, "\u00e9") {
		t.Fatalf("capped UTF-8 line bytes=%d valid content=%q", len(got), got)
	}
}

func TestEmitPipelineProgressThresholdCadenceAndCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	if err := store.CreateJob(ctx, db.Job{ID: "job-progress", Agent: "worker", Type: "ask", State: "running"}); err != nil {
		t.Fatal(err)
	}
	tracker := &pipelineProgressLineTracker{}
	_, _ = tracker.Write([]byte("working\n"))
	ticks := make(chan time.Time, 2)
	done := make(chan struct{})
	start := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	go func() {
		emitPipelineProgress(ctx, store, io.Discard, "job-progress", start, tracker, ticks)
		close(done)
	}()
	if _, ok, err := store.GetLatestJobEventByKind(ctx, "job-progress", "progress"); err != nil || ok {
		t.Fatalf("event existed before threshold tick: ok=%v err=%v", ok, err)
	}
	ticks <- start.Add(time.Minute)
	ticks <- start.Add(90 * time.Second)
	close(ticks)
	<-done
	events, err := store.ListJobEvents(ctx, "job-progress")
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Message, `"elapsed":"1m30s"`) || !strings.Contains(events[0].Message, `"activity":"working"`) {
		t.Fatalf("cadenced latest-only events=%+v err=%v", events, err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	blockedTicks := make(chan time.Time)
	go func() {
		emitPipelineProgress(ctx2, store, io.Discard, "job-progress", start, tracker, blockedTicks)
		close(stopped)
	}()
	cancel2()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("emitter did not stop on context cancellation")
	}
}

func TestCockpitAndProgressShareRuntimeOutput(t *testing.T) {
	home := t.TempDir()
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)
	agent := runtime.Agent{Name: "lead", Role: "builder", Runtime: runtime.ShellRuntime, RuntimeRef: "echo shared-line"}
	tracker := &pipelineProgressLineTracker{}
	adapter, logPath, logFile := worker.cockpitTeeAdapter(agent, t.TempDir(), "job-shared", tracker)
	if logFile == nil {
		t.Fatal("expected cockpit log")
	}
	defer logFile.Close()
	if _, err := adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(logged), "shared-line") || tracker.LastLine() != "shared-line" {
		t.Fatalf("cockpit=%q tracker=%q err=%v", logged, tracker.LastLine(), err)
	}
}

func TestPipelineRunProgressRenderingAndJSON(t *testing.T) {
	now := time.Date(2026, 7, 10, 10, 2, 0, 0, time.UTC)
	started := now.Add(-2 * time.Minute)
	view := pipelineRunView{
		run: db.PipelineRun{ID: "run", Pipeline: "flow", State: pipeline.RunRunning, StartedAt: started},
		stages: []db.PipelineRunStage{
			{StageID: "work", State: pipeline.StageRunning, JobID: "job-work", StartedAt: started},
			{StageID: "gate", State: pipeline.StageQueued, StartedAt: now.Add(-time.Minute)},
			{StageID: "tree", State: pipeline.StageRunning, JobID: "job-tree", StartedAt: started},
			{StageID: "done", State: pipeline.StageSucceeded, JobID: "job-done", StartedAt: started, FinishedAt: now.Add(-time.Minute)},
		},
		progress: map[string]db.JobEvent{
			"job-work": {JobID: "job-work", Kind: "progress", Message: `{"elapsed":"1m55s","activity":"testing"}`, CreatedAt: now.Add(-5 * time.Second).Format(time.RFC3339Nano)},
			"job-tree": {JobID: "job-tree", Kind: "progress", Message: `{"elapsed":"30s","activity":"delegating"}`, CreatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano)},
		},
		orchestrate: map[string]bool{"tree": true},
	}
	var out bytes.Buffer
	printPipelineRunFunnelAt(&out, view, now)
	for _, want := range []string{"work: RUNNING; enqueued 2m0s ago", "last activity 5s ago: testing", "gate: QUEUED; enqueued 1m0s ago", "last activity 2m0s ago: delegating", "(sub-tree running; no per-stage progress)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
	jsonView := pipelineRunToJSON(view)
	if jsonView.Stages[0].StartedAt == "" || jsonView.Stages[0].Progress == nil || jsonView.Stages[0].Progress.Activity != "testing" {
		t.Fatalf("JSON stage = %+v", jsonView.Stages[0])
	}
	if jsonView.Stages[3].FinishedAt == "" {
		t.Fatalf("finished JSON stage = %+v", jsonView.Stages[3])
	}
}

func TestPipelineSlowShellStageProgressE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	cmd := `printf 'phase-one\n'; sleep 0.25; ` + pipelineStageResultCmd("approved", "done", nil)
	specFile := writeSpec(t, "name: slow-progress\nrepo: owner/repo\nstages:\n"+pipelineE2EStage("slow", cmd, ""))
	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "slow-progress", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())
	stage := stageRow(t, store, runID, "slow")
	worker := defaultJobWorker(store, io.Discard, home)
	worker.ProgressThreshold = 5 * time.Millisecond
	worker.ProgressInterval = 5 * time.Millisecond
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, time.Now().UTC())
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		event, ok, err := store.GetLatestJobEventByKind(ctx, stage.JobID, "progress")
		if err != nil {
			t.Fatal(err)
		}
		if ok && strings.Contains(event.Message, "phase-one") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress event with last shell line did not arrive; last=%+v", event)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := runPipelineScanOnce(ctx, store, newPipelineStageEnqueuer(store, home), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "show", runID, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline show exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "last activity") || !strings.Contains(out.String(), "phase-one") {
		t.Fatalf("pipeline show missing live progress:\n%s", out.String())
	}
	if err := <-workerDone; err != nil {
		t.Fatalf("worker tick: %v", err)
	}
}
