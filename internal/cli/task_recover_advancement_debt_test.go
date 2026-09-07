package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1174: a supported `gitmoot task recover` must reclaim a task parked in
// awaiting_human_merge whose only apparently-live job is TERMINAL and held live
// solely by stale advancement debt.
//
// MEASURED INCIDENT (messages 126776 and 126838): task adhoc-c65bb333 sat in
// awaiting_human_merge while job
// local-implement-checkin-builder-18d2c814b3b36343 was SUCCEEDED with a
// trailing advance_retry naming obsolete head ae06783e, against a checkout and
// PR already at the approved cb415318. jobKeepsTaskLive
// (internal/workflow/task_liveness.go:139) reports a settled job with open debt
// as live, so recovery refused; native advancement expects `reviewing`, so
// neither path could progress.
//
// EVERY FIXTURE ROW IS WRITTEN THROUGH A SANCTIONED STORE API. UpsertRepo,
// UpsertAgent, UpsertTask, CreateJob and AddJobEvent - the last being the same
// durable call daemon_worker.go uses to write advance_awaiting_human. No SQL, no
// direct sqlite handle, no file surgery, no job kill or cancel, no review
// replay, no head rollback, no daemon or config change.
type recoverDebtFixture struct {
	store    *db.Store
	home     string
	repoDir  string
	worktree string
	taskID   string
	jobID    string
}

func newRecoverDebtFixture(t *testing.T, taskState string, jobState string, advanceKind string) recoverDebtFixture {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	writeFile(t, filepath.Join(repoDir, "README.md"), "main\n")
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")

	store := openCLIJobStore(t, home)
	t.Cleanup(func() { store.Close() })
	if err := store.UpsertRepo(ctx, db.Repo{
		Owner: "owner", Name: "repo", DefaultBranch: "main",
		RemoteURL: remoteDir, CheckoutPath: repoDir, PollInterval: "30s",
	}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "lead", Runtime: "shell", RuntimeRef: "true", RepoScope: "owner/repo",
		Capabilities: []string{"implement"}, AutonomyPolicy: "workspace-write", HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}

	taskID := "task-1174"
	worktree, err := workflow.TaskWorktreePath(home, "owner/repo", taskID)
	if err != nil {
		t.Fatalf("TaskWorktreePath returned error: %v", err)
	}
	branch := taskID + "-work"
	runGit(t, repoDir, "worktree", "add", "-b", branch, worktree, "main")
	writeFile(t, filepath.Join(worktree, "feature.txt"), "approved work\n")
	runGit(t, worktree, "add", "feature.txt")
	runGit(t, worktree, "commit", "-m", "approved work")

	if err := store.UpsertTask(ctx, db.Task{
		ID: taskID, RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Parked",
		State: taskState, Branch: branch, WorktreePath: worktree,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	// The job the incident describes: matched to the task, terminal, and carrying
	// an OBSOLETE head in its payload so the "approved head preserved" control
	// has something to observe.
	jobID := "local-implement-lead-1174"
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", Branch: branch, TaskID: taskID,
		PullRequest: 3, HeadSHA: "ae06783e71b58d6770dbd1ccf1969be74743f716",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: jobID, Agent: "lead", Type: "implement", State: jobState, Payload: string(payload),
	}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	if strings.TrimSpace(advanceKind) != "" {
		if err := store.AddJobEvent(ctx, db.JobEvent{
			JobID: jobID, Kind: advanceKind,
			Message: "checkout head is cb415318e91257845d459138ad473acaf58eeb4a, not job head ae06783e71b58d6770dbd1ccf1969be74743f716",
		}); err != nil {
			t.Fatalf("AddJobEvent returned error: %v", err)
		}
	}
	return recoverDebtFixture{store: store, home: home, repoDir: repoDir, worktree: worktree, taskID: taskID, jobID: jobID}
}

func (f recoverDebtFixture) settlementRows(t *testing.T) int {
	t.Helper()
	events, err := f.store.ListJobEvents(context.Background(), f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, event := range events {
		if event.Kind == taskRecoverAdvanceSettlementKind {
			n++
		}
	}
	return n
}

// C1 SUCCESSFUL RECLAIM. This is the RED-before / GREEN-after regression, driven
// at recoverTaskImplementationForRunner - the production function
// `gitmoot task recover` calls at internal/cli/workflow.go:922.
func TestTaskRecoverReclaimsAwaitingHumanMergeWithStaleAdvanceDebt(t *testing.T) {
	ctx := context.Background()
	fx := newRecoverDebtFixture(t, string(workflow.TaskAwaitingHumanMerge), string(workflow.JobSucceeded), "advance_retry")

	if _, err := recoverTaskImplementationForRunner(
		ctx, fx.store, fx.taskID, "owner/repo", "lead", true, &stubTaskRecoverGitHub{}, subprocess.ExecRunner{},
	); err != nil {
		t.Fatalf("recovery refused a task held live only by stale advancement debt: %v", err)
	}

	// C2 TERMINAL JOB NOT OTHERWISE LIVE, asked through the SAME exported
	// predicate every consumer uses, so this is not a re-derivation.
	task, err := fx.store.GetTask(ctx, fx.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, live, err := workflow.FindLiveTaskJob(ctx, fx.store, task); err != nil {
		t.Fatal(err)
	} else if live {
		t.Fatal("the settled job still reads as live after recovery, so other consumers keep treating it as live")
	}
	if n := fx.settlementRows(t); n != 1 {
		t.Fatalf("%s rows = %d, want exactly 1", taskRecoverAdvanceSettlementKind, n)
	}
}

// C3 ACTUAL LIVE JOB STILL REFUSED, and no settlement written. A running job is
// real work in flight; this is the arm that stops the correction from becoming a
// way to walk over it.
func TestTaskRecoverStillRefusesGenuinelyLiveJob(t *testing.T) {
	ctx := context.Background()
	fx := newRecoverDebtFixture(t, string(workflow.TaskAwaitingHumanMerge), string(workflow.JobRunning), "")

	_, err := recoverTaskImplementationForRunner(
		ctx, fx.store, fx.taskID, "owner/repo", "lead", true, &stubTaskRecoverGitHub{}, subprocess.ExecRunner{},
	)
	if err == nil {
		t.Fatal("recovery proceeded while a genuinely live job was running")
	}
	if !strings.Contains(err.Error(), "still has live job") {
		t.Fatalf("refusal lost its original message: %v", err)
	}
	if n := fx.settlementRows(t); n != 0 {
		t.Fatalf("%s rows = %d for a running job, want 0", taskRecoverAdvanceSettlementKind, n)
	}
}

// C4 NON-AWAITING STATE STILL REFUSED. Same terminal job with the same debt, but
// the task is implementing: a live lifecycle keeps the refusal.
func TestTaskRecoverStillRefusesWhenTaskIsNotAwaitingHumanMerge(t *testing.T) {
	ctx := context.Background()
	fx := newRecoverDebtFixture(t, string(workflow.TaskImplementing), string(workflow.JobSucceeded), "advance_retry")

	_, err := recoverTaskImplementationForRunner(
		ctx, fx.store, fx.taskID, "owner/repo", "lead", true, &stubTaskRecoverGitHub{}, subprocess.ExecRunner{},
	)
	if err == nil || !strings.Contains(err.Error(), "still has live job") {
		t.Fatalf("recovery must keep refusing outside awaiting_human_merge; err = %v", err)
	}
	if n := fx.settlementRows(t); n != 0 {
		t.Fatalf("%s rows = %d outside awaiting_human_merge, want 0", taskRecoverAdvanceSettlementKind, n)
	}
}

// CANCELLED CONTROL, which my guard needs even though the directive's list does
// not name it: a cancelled-from-running job is kept live by an INDEPENDENT rule
// (task_liveness.go:142), so settling advancement debt there would clear a debt
// that was never the obstacle.
func TestTaskRecoverDoesNotSettleCancelledFromRunningJob(t *testing.T) {
	ctx := context.Background()
	fx := newRecoverDebtFixture(t, string(workflow.TaskAwaitingHumanMerge), string(workflow.JobCancelled), "")
	if err := fx.store.AddJobEvent(ctx, db.JobEvent{
		JobID: fx.jobID, Kind: string(workflow.JobCancelled), Message: "cancel requested from running",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := recoverTaskImplementationForRunner(
		ctx, fx.store, fx.taskID, "owner/repo", "lead", true, &stubTaskRecoverGitHub{}, subprocess.ExecRunner{},
	)
	if err == nil || !strings.Contains(err.Error(), "still has live job") {
		t.Fatalf("a cancelled-from-running job must keep the refusal; err = %v", err)
	}
	if n := fx.settlementRows(t); n != 0 {
		t.Fatalf("%s rows = %d for a cancelled job, want 0", taskRecoverAdvanceSettlementKind, n)
	}
}

// C5 APPROVED EVIDENCE AND HEAD PRESERVED, and C6 the daemon / non-replay
// control. The correction must add exactly ONE job_events row and no jobs row,
// must not touch the task's branch or worktree, and must not rewrite the
// terminal job's recorded head.
func TestTaskRecoverPreservesApprovedEvidenceAndAddsOnlySettlement(t *testing.T) {
	ctx := context.Background()
	fx := newRecoverDebtFixture(t, string(workflow.TaskAwaitingHumanMerge), string(workflow.JobSucceeded), "advance_retry")

	beforeTask, err := fx.store.GetTask(ctx, fx.taskID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJob, err := fx.store.GetJob(ctx, fx.jobID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := fx.store.ListJobEvents(ctx, fx.jobID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJobs, err := fx.store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := recoverTaskImplementationForRunner(
		ctx, fx.store, fx.taskID, "owner/repo", "lead", true, &stubTaskRecoverGitHub{}, subprocess.ExecRunner{},
	); err != nil {
		t.Fatalf("recovery returned error: %v", err)
	}

	afterJob, err := fx.store.GetJob(ctx, fx.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if afterJob.Payload != beforeJob.Payload {
		t.Fatalf("the terminal job's recorded payload changed; the approved head must be preserved:\nbefore %s\nafter  %s", beforeJob.Payload, afterJob.Payload)
	}
	if afterJob.State != beforeJob.State {
		t.Fatalf("job state changed from %q to %q; recovery must not kill or re-run the job", beforeJob.State, afterJob.State)
	}
	afterTask, err := fx.store.GetTask(ctx, fx.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTask.Branch != beforeTask.Branch || afterTask.WorktreePath != beforeTask.WorktreePath {
		t.Fatalf("task branch/worktree changed: before %+v after %+v", beforeTask, afterTask)
	}

	afterEvents, err := fx.store.ListJobEvents(ctx, fx.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(afterEvents) - len(beforeEvents); got != 1 {
		t.Fatalf("recovery added %d job_events rows to the terminal job, want exactly 1 (the settlement)", got)
	}
	// NON-REPLAY: no review job, and no new job at all on that terminal id.
	afterJobs, err := fx.store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range afterJobs {
		if job.Type == "review" {
			t.Fatalf("recovery dispatched a review job %s; review must not be replayed", job.ID)
		}
	}
	if len(afterJobs) < len(beforeJobs) {
		t.Fatalf("jobs disappeared during recovery: before %d after %d", len(beforeJobs), len(afterJobs))
	}
}

// THE KIND DISTINCTION, pinned because getting it wrong fixes nothing.
// advance_retry OPENS advancement debt (task_liveness.go:161).
// advance_awaiting_human is in NEITHER set: it is a status marker written at
// daemon_worker.go:4075, so it must not by itself make a terminal job read as
// live, and recovery must not need a settlement for it.
func TestTaskRecoverTreatsAwaitingHumanMarkerAsNoDebt(t *testing.T) {
	ctx := context.Background()
	fx := newRecoverDebtFixture(t, string(workflow.TaskAwaitingHumanMerge), string(workflow.JobSucceeded), "advance_awaiting_human")

	task, err := fx.store.GetTask(ctx, fx.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, live, err := workflow.FindLiveTaskJob(ctx, fx.store, task); err != nil {
		t.Fatal(err)
	} else if live {
		t.Fatal("advance_awaiting_human alone made a terminal job read as live; it opens no advancement debt")
	}
	if _, err := recoverTaskImplementationForRunner(
		ctx, fx.store, fx.taskID, "owner/repo", "lead", true, &stubTaskRecoverGitHub{}, subprocess.ExecRunner{},
	); err != nil {
		t.Fatalf("recovery returned error: %v", err)
	}
	if n := fx.settlementRows(t); n != 0 {
		t.Fatalf("%s rows = %d; no debt was open so none should be settled", taskRecoverAdvanceSettlementKind, n)
	}
}
