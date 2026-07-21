package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/transcript"
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
	var settled atomic.Bool
	lines := make(chan string, 4)
	followDone := make(chan error, 1)
	go func() {
		followDone <- transcript.Follow(context.Background(), logPath, transcript.FollowOptions{
			PollInterval: time.Millisecond,
			Settled:      func(context.Context) (bool, error) { return settled.Load(), nil },
		}, func(line string) error {
			lines <- line
			return nil
		})
	}()
	if _, err := adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	settled.Store(true)
	if err := <-followDone; err != nil {
		t.Fatal(err)
	}
	close(lines)
	var followed []string
	for line := range lines {
		followed = append(followed, line)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(logged), "shared-line") || tracker.LastLine() != "shared-line" || strings.Join(followed, "\n") != "shared-line" {
		t.Fatalf("cockpit=%q tracker=%q follower=%q err=%v", logged, tracker.LastLine(), followed, err)
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
		tokens:      123,
		stageTokens: map[string]pipelineStageTokens{"work": {input: 100, output: 23}},
	}
	var out bytes.Buffer
	printPipelineRunFunnelAt(&out, view, now)
	for _, want := range []string{"tokens: 123 (best-effort)", "work: RUNNING; started 2m0s ago", "last activity 5s ago: testing", "gate: QUEUED; enqueued 1m0s ago", "last activity 2m0s ago: delegating", "(sub-tree running; no per-stage progress)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "gate: QUEUED; started") {
		t.Fatalf("queued stage rendered as started:\n%s", out.String())
	}
	jsonView := pipelineRunToJSON(view)
	if jsonView.Stages[0].StartedAt == "" || jsonView.Stages[0].Progress == nil || jsonView.Stages[0].Progress.Activity != "testing" {
		t.Fatalf("JSON stage = %+v", jsonView.Stages[0])
	}
	if jsonView.Tokens != 123 || jsonView.Stages[0].InputTokens == nil || *jsonView.Stages[0].InputTokens != 100 || jsonView.Stages[0].OutputTokens == nil || *jsonView.Stages[0].OutputTokens != 23 {
		t.Fatalf("JSON tokens = run %d stage %v/%v", jsonView.Tokens, jsonView.Stages[0].InputTokens, jsonView.Stages[0].OutputTokens)
	}
	if jsonView.Stages[3].FinishedAt == "" {
		t.Fatalf("finished JSON stage = %+v", jsonView.Stages[3])
	}
}

func TestSumPipelineRunTokensIncludesOrchestrateTreesWithoutDoubleCounting(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	runID := "prun-token-test"
	create := func(id, root string, input, output int) {
		t.Helper()
		payload := `{"root_job_id":"` + root + `"}`
		if err := store.CreateJob(ctx, db.Job{ID: id, Agent: "a", Type: "ask", State: "succeeded", Payload: payload}); err != nil {
			t.Fatalf("CreateJob(%s): %v", id, err)
		}
		if err := store.UpdateJobUsage(ctx, id, input, output); err != nil {
			t.Fatalf("UpdateJobUsage(%s): %v", id, err)
		}
	}
	create("ordinary", runID, 7, 3)
	orch0 := pipelineStageJobID(runID, "tree", 0)
	orch1 := pipelineStageJobID(runID, "tree", 1)
	create(orch0, orch0, 11, 1)
	create(orch0+"/child", orch0, 13, 2)
	create(orch1, orch1, 17, 4)

	base, err := store.SumJobTokensByRoot(ctx, runID)
	if err != nil || base != 10 {
		t.Fatalf("run-rooted base = %d, err=%v; orchestrate trees must be disjoint", base, err)
	}
	total, err := sumPipelineRunTokens(ctx, store, runID, []db.PipelineRunStage{{StageID: "ordinary", JobID: "ordinary"}, {StageID: "tree", JobID: orch1, Attempt: 1}}, map[string]bool{"tree": true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 58 {
		t.Fatalf("total tokens = %d, want 58 (10 ordinary + 27 tree attempt 0 + 21 tree attempt 1)", total)
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
	worker.PipelineProgressThreshold = 5 * time.Millisecond
	worker.PipelineProgressInterval = 5 * time.Millisecond
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
