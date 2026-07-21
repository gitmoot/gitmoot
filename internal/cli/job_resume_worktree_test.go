package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type selfDirtyResumeFixture struct {
	store         *db.Store
	worktree      string
	completedWork string
	head          string
	adapter       *cliWorkerFakeAdapter
	worker        jobWorker
}

func newSelfDirtyResumeFixture(t *testing.T, jobID string) selfDirtyResumeFixture {
	return newSelfDirtyResumeFixtureWithLockOwner(t, jobID, "lead")
}

func newSelfDirtyResumeFixtureWithLockOwner(t *testing.T, jobID string, lockOwner string) selfDirtyResumeFixture {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-resume")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-resume", worktree, "main")
	head, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	completedWork := filepath.Join(worktree, "completed-work.txt")
	if err := os.WriteFile(completedWork, []byte("completed before runtime death\n"), 0o644); err != nil {
		t.Fatalf("WriteFile completed work returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-resume",
		RepoFullName: "owner/repo",
		GoalID:       "goal-1",
		Title:        "Resume completed work",
		State:        string(workflow.TaskImplementing),
		Branch:       "task-resume",
		WorktreePath: worktree,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if lockOwner != "" {
		if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-resume", Owner: lockOwner}); err != nil || !acquired {
			t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
		}
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:           jobID,
		Agent:        "lead",
		Action:       "implement",
		Repo:         "owner/repo",
		Branch:       "task-resume",
		HeadSHA:      head,
		GoalID:       "goal-1",
		TaskID:       "task-resume",
		TaskTitle:    "Resume completed work",
		Instructions: "Finish the implementation.",
	})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store}
	}
	return selfDirtyResumeFixture{
		store:         store,
		worktree:      worktree,
		completedWork: completedWork,
		head:          head,
		adapter:       adapter,
		worker:        worker,
	}
}

func assertSelfDirtyResumeDeferred(t *testing.T, fixture selfDirtyResumeFixture, jobID string) workflow.JobPayload {
	t.Helper()
	job, err := fixture.store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) || payload.BlockerAttempts != 1 ||
		payload.BlockerClass != string(blockerClassCheckoutContention) || payload.BlockerRetryAt == "" ||
		payload.BlockerSuggestedAction == "" || payload.ResumedSelfDirtyWorktree {
		t.Fatalf("self-dirty fallback = state=%q payload=%+v", job.State, payload)
	}
	events, err := fixture.store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "worktree_resumed" {
			t.Fatalf("blocked job recorded a resume event: %+v", events)
		}
	}
	return payload
}

func TestResumeSelfDirtySkipsWhenProcessLive(t *testing.T) {
	ctx := context.Background()
	fixture := newSelfDirtyResumeFixture(t, "job-live-worktree")
	previous := taskWorktreeLiveness
	taskWorktreeLiveness = func(path string) (bool, bool) {
		return sameCheckoutPath(path, fixture.worktree), true
	}
	t.Cleanup(func() { taskWorktreeLiveness = previous })

	if err := runQueuedJobsForRepo(ctx, fixture.worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if fixture.adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want live-process gate to stop delivery", fixture.adapter.calls)
	}
	job, err := fixture.store.GetJob(ctx, "job-live-worktree")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) || payload.BlockerAttempts != 1 || payload.BlockerClass != string(blockerClassCheckoutContention) {
		t.Fatalf("live-process fallback = state=%q payload=%+v", job.State, payload)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil {
		t.Fatalf("parse BlockerRetryAt: %v", err)
	}
	if until := time.Until(retryAt); until < checkoutDirtyBackoff-time.Minute || until > checkoutDirtyBackoff+time.Minute {
		t.Fatalf("live-process fallback retry delay = %s, want about %s", until, checkoutDirtyBackoff)
	}
	if payload.BlockerSuggestedAction == "" || payload.ResumedSelfDirtyWorktree {
		t.Fatalf("live-process fallback lost defer shape: %+v", payload)
	}
	if _, err := os.Stat(fixture.completedWork); err != nil {
		t.Fatalf("live-process fallback changed the worktree: %v", err)
	}
}

func TestResumeSelfDirtySkipsWrongHeadWhileDirty(t *testing.T) {
	ctx := context.Background()
	fixture := newSelfDirtyResumeFixture(t, "job-wrong-head-dirty")
	runDaemonWorkerGit(t, fixture.worktree, "add", filepath.Base(fixture.completedWork))
	runDaemonWorkerGit(t, fixture.worktree, "commit", "-m", "advance worktree head")
	stillDirty := filepath.Join(fixture.worktree, "still-dirty.txt")
	if err := os.WriteFile(stillDirty, []byte("unfinished correction\n"), 0o644); err != nil {
		t.Fatalf("WriteFile still-dirty work: %v", err)
	}

	if err := runQueuedJobsForRepo(ctx, fixture.worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if fixture.adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want wrong HEAD to stop delivery", fixture.adapter.calls)
	}
	payload := assertSelfDirtyResumeDeferred(t, fixture, "job-wrong-head-dirty")
	if payload.HeadSHA != fixture.head {
		t.Fatalf("fallback payload head = %q, want original %q", payload.HeadSHA, fixture.head)
	}
	actualHead, err := (gitutil.Client{Dir: fixture.worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if actualHead == fixture.head {
		t.Fatalf("test setup did not move worktree HEAD from %q", fixture.head)
	}
	for _, path := range []string{fixture.completedWork, stillDirty} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("wrong-HEAD fallback changed %s: %v", path, err)
		}
	}
}

func TestResumeSelfDirtySkipsMissingBranchLock(t *testing.T) {
	ctx := context.Background()
	fixture := newSelfDirtyResumeFixtureWithLockOwner(t, "job-missing-lock", "")

	if err := runQueuedJobsForRepo(ctx, fixture.worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if fixture.adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want missing branch lock to stop delivery", fixture.adapter.calls)
	}
	assertSelfDirtyResumeDeferred(t, fixture, "job-missing-lock")
	if _, err := os.Stat(fixture.completedWork); err != nil {
		t.Fatalf("missing-lock fallback changed the worktree: %v", err)
	}
}

func TestResumeSelfDirtySkipsForeignBranchLock(t *testing.T) {
	ctx := context.Background()
	fixture := newSelfDirtyResumeFixtureWithLockOwner(t, "job-foreign-lock", "another-agent")

	if err := runQueuedJobsForRepo(ctx, fixture.worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if fixture.adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want foreign branch lock to stop delivery", fixture.adapter.calls)
	}
	assertSelfDirtyResumeDeferred(t, fixture, "job-foreign-lock")
	if _, err := os.Stat(fixture.completedWork); err != nil {
		t.Fatalf("foreign-lock fallback changed the worktree: %v", err)
	}
}

func TestResumeSelfDirtySkipsWhenLivenessUnknown(t *testing.T) {
	tests := []struct {
		name  string
		live  bool
		known bool
	}{
		{name: "unknown", live: false, known: false},
		{name: "live", live: true, known: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSelfDirtyResumeFixture(t, "job-liveness-"+tc.name)
			previous := taskWorktreeLiveness
			taskWorktreeLiveness = func(string) (bool, bool) { return tc.live, tc.known }
			defer func() { taskWorktreeLiveness = previous }()

			if err := runQueuedJobsForRepo(context.Background(), fixture.worker, 1, "", ""); err != nil {
				t.Fatalf("runQueuedJobs returned error: %v", err)
			}
			if fixture.adapter.calls != 0 {
				t.Fatalf("adapter calls = %d, want liveness (%v, %v) to stop delivery", fixture.adapter.calls, tc.live, tc.known)
			}
			assertSelfDirtyResumeDeferred(t, fixture, "job-liveness-"+tc.name)
			if _, err := os.Stat(fixture.completedWork); err != nil {
				t.Fatalf("liveness fallback changed the worktree: %v", err)
			}
		})
	}
}

func TestResumeSelfDirtySkipsDirtyByOther(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	dirtyFile := filepath.Join(checkout, "someone-elses-work.txt")
	if err := os.WriteFile(dirtyFile, []byte("do not touch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirty registered checkout: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "main", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-dirty-by-other", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "main", HeadSHA: head,
	})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"unexpected","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return adapter, nil }

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want dirty registered checkout to remain blocked", adapter.calls)
	}
	job, err := store.GetJob(ctx, "job-dirty-by-other")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	_, wantAction := classifyCheckoutContention(errors.New("checkout " + checkout + " has uncommitted changes"))
	if job.State != string(workflow.JobQueued) || payload.BlockerClass != string(blockerClassCheckoutContention) ||
		payload.BlockerAttempts != 1 || payload.BlockerRetryAt == "" || payload.BlockerSuggestedAction != wantAction ||
		!payload.BlockerPreDelivery || payload.ResumedSelfDirtyWorktree {
		t.Fatalf("dirty-by-other defer shape changed: state=%q payload=%+v", job.State, payload)
	}
	if _, err := os.Stat(dirtyFile); err != nil {
		t.Fatalf("dirty-by-other file was touched: %v", err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "worktree_resumed" {
			t.Fatalf("dirty-by-other job recorded a resume event: %+v", events)
		}
	}
}

func TestResumeSelfDirtyPredicate(t *testing.T) {
	tests := []struct {
		name     string
		cause    func(selfDirtyResumeFixture) error
		mutate   func(*testing.T, selfDirtyResumeFixture, *db.Job, *workflow.JobPayload)
		liveness func(string) (bool, bool)
	}{
		{name: "delegation child", mutate: func(_ *testing.T, _ selfDirtyResumeFixture, _ *db.Job, p *workflow.JobPayload) {
			p.ParentJobID, p.DelegationID = "parent", "child"
		}},
		{name: "wrong head string", cause: func(selfDirtyResumeFixture) error {
			return errors.New("checkout head is abc, not job head def")
		}},
		{name: "lock string", cause: func(selfDirtyResumeFixture) error {
			return errors.New("branch task-resume is locked by another worker")
		}},
		{name: "payload worktree set", mutate: func(_ *testing.T, f selfDirtyResumeFixture, _ *db.Job, p *workflow.JobPayload) {
			p.WorktreePath = f.worktree
		}},
		{name: "missing task", mutate: func(_ *testing.T, _ selfDirtyResumeFixture, _ *db.Job, p *workflow.JobPayload) {
			p.TaskID = "missing"
		}},
		{name: "repo mismatch", mutate: func(_ *testing.T, _ selfDirtyResumeFixture, _ *db.Job, p *workflow.JobPayload) {
			p.Repo = "other/repo"
		}},
		{name: "branch mismatch", mutate: func(_ *testing.T, _ selfDirtyResumeFixture, _ *db.Job, p *workflow.JobPayload) {
			p.Branch = "other-branch"
		}},
		{name: "non implement type", mutate: func(_ *testing.T, _ selfDirtyResumeFixture, j *db.Job, _ *workflow.JobPayload) {
			j.Type = "review"
		}},
		{name: "not queued", mutate: func(_ *testing.T, _ selfDirtyResumeFixture, j *db.Job, _ *workflow.JobPayload) {
			j.State = string(workflow.JobRunning)
		}},
		{name: "budget spent", mutate: func(_ *testing.T, _ selfDirtyResumeFixture, _ *db.Job, p *workflow.JobPayload) {
			p.BlockerAttempts = maxOperationalBlockerRetries
		}},
		{name: "missing expected head", mutate: func(_ *testing.T, _ selfDirtyResumeFixture, _ *db.Job, p *workflow.JobPayload) {
			p.HeadSHA = ""
		}},
		{name: "wrong head while dirty", mutate: func(t *testing.T, f selfDirtyResumeFixture, _ *db.Job, _ *workflow.JobPayload) {
			runDaemonWorkerGit(t, f.worktree, "add", filepath.Base(f.completedWork))
			runDaemonWorkerGit(t, f.worktree, "commit", "-m", "move predicate head")
			if err := os.WriteFile(filepath.Join(f.worktree, "still-dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
				t.Fatalf("WriteFile still-dirty predicate file: %v", err)
			}
		}},
		{name: "missing branch lock", mutate: func(t *testing.T, f selfDirtyResumeFixture, _ *db.Job, _ *workflow.JobPayload) {
			released, err := f.store.ReleaseLock(context.Background(), db.BranchLock{RepoFullName: "owner/repo", Branch: "task-resume", Owner: "lead"})
			if err != nil || !released {
				t.Fatalf("ReleaseLock returned released=%v err=%v", released, err)
			}
		}},
		{name: "foreign branch lock", mutate: func(t *testing.T, f selfDirtyResumeFixture, _ *db.Job, _ *workflow.JobPayload) {
			ctx := context.Background()
			released, err := f.store.ReleaseLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-resume", Owner: "lead"})
			if err != nil || !released {
				t.Fatalf("ReleaseLock returned released=%v err=%v", released, err)
			}
			acquired, err := f.store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-resume", Owner: "another-agent"})
			if err != nil || !acquired {
				t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
			}
		}},
		{name: "liveness unknown", liveness: func(string) (bool, bool) { return false, false }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newSelfDirtyResumeFixture(t, "predicate-job")
			job, err := fixture.store.GetJob(ctx, "predicate-job")
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			payload, err := daemonJobPayload(job)
			if err != nil {
				t.Fatalf("daemonJobPayload returned error: %v", err)
			}
			cause := errors.New("checkout " + fixture.worktree + " has uncommitted changes")
			if tc.cause != nil {
				cause = tc.cause(fixture)
			}
			if tc.mutate != nil {
				tc.mutate(t, fixture, &job, &payload)
			}
			if tc.liveness != nil {
				previous := taskWorktreeLiveness
				taskWorktreeLiveness = tc.liveness
				defer func() { taskWorktreeLiveness = previous }()
			}

			checkout, gotPayload, ok := fixture.worker.resumeSelfDirtyWorktree(ctx, job, payload, runtime.Agent{Name: "lead"}, cause)
			if ok || checkout != "" {
				t.Fatalf("resumeSelfDirtyWorktree = (%q, ok=%v), want predicate miss", checkout, ok)
			}
			if !reflect.DeepEqual(gotPayload, payload) {
				t.Fatalf("predicate miss mutated payload: got=%+v want=%+v", gotPayload, payload)
			}
		})
	}
}

func TestResumeSelfDirtyBudgetExhaustedStaysTerminal(t *testing.T) {
	ctx := context.Background()
	fixture := newSelfDirtyResumeFixture(t, "job-resume-budget")
	job, err := fixture.store.GetJob(ctx, "job-resume-budget")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	payload.BlockerAttempts = maxOperationalBlockerRetries
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	if err := fixture.store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload returned error: %v", err)
	}

	if err := runQueuedJobsForRepo(ctx, fixture.worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if fixture.adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want exhausted resume budget to stop delivery", fixture.adapter.calls)
	}
	job, err = fixture.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob after run returned error: %v", err)
	}
	payload, err = daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload after run returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) || payload.BlockerAttempts != maxOperationalBlockerRetries || payload.ResumedSelfDirtyWorktree {
		t.Fatalf("budget-exhausted result = state=%q payload=%+v", job.State, payload)
	}
	events, err := fixture.store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	foundExhausted := false
	for _, event := range events {
		foundExhausted = foundExhausted || event.Kind == blockerExhaustedEventKind
		if event.Kind == "worktree_resumed" {
			t.Fatalf("budget-exhausted job recorded a resume event: %+v", events)
		}
	}
	if !foundExhausted {
		t.Fatalf("budget-exhausted job missing %s event: %+v", blockerExhaustedEventKind, events)
	}
	if _, err := os.Stat(fixture.completedWork); err != nil {
		t.Fatalf("budget-exhausted fallback changed the worktree: %v", err)
	}
	if payload.HeadSHA != fixture.head {
		t.Fatalf("budget-exhausted payload head = %q, want %q", payload.HeadSHA, fixture.head)
	}
}

func TestResumeSelfDirtySkipsWhenRuntimeLeaseHeld(t *testing.T) {
	ctx := context.Background()
	fixture := newSelfDirtyResumeFixture(t, "job-live-lease")
	now := time.Now().UTC()
	acquired, err := fixture.store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   "runtime:codex:self-dirty-test",
		OwnerJobID:    "job-live-lease",
		OwnerToken:    "owner-token",
		OwnerHostname: "other-host",
		OwnerPID:      123,
		ExpiresAt:     now.Add(time.Hour).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	if err := runQueuedJobsForRepo(ctx, fixture.worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	if fixture.adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want live runtime lease to stop delivery", fixture.adapter.calls)
	}
	job, err := fixture.store.GetJob(ctx, "job-live-lease")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) || payload.BlockerAttempts != 1 || payload.ResumedSelfDirtyWorktree {
		t.Fatalf("live-lease fallback = state=%q payload=%+v", job.State, payload)
	}
}

func TestResumeSelfDirtyPredicateRequiresExactDirtyText(t *testing.T) {
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)
	job := db.Job{ID: "job", Type: "implement", State: string(workflow.JobQueued)}
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "task", TaskID: "task"}
	for _, cause := range []error{nil, errors.New("checkout is dirty"), errors.New("checkout head is abc, not job head def")} {
		if checkout, got, ok := worker.resumeSelfDirtyWorktree(context.Background(), job, payload, runtime.Agent{Name: "lead"}, cause); ok || checkout != "" || !reflect.DeepEqual(got, payload) {
			t.Fatalf("cause %v unexpectedly resumed: checkout=%q payload=%+v ok=%v", cause, checkout, got, ok)
		}
	}
}

func TestResumeSelfDirtyEventMessageNamesPathAndAttempt(t *testing.T) {
	fixture := newSelfDirtyResumeFixture(t, "job-resume-event")
	if err := runQueuedJobsForRepo(context.Background(), fixture.worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}
	events, err := fixture.store.ListJobEvents(context.Background(), "job-resume-event")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind != "worktree_resumed" {
			continue
		}
		if !strings.Contains(event.Message, fixture.worktree) || !strings.Contains(event.Message, "attempt 1/") ||
			!strings.Contains(event.Message, "prior attempt's completed work present uncommitted; re-delivering") {
			t.Fatalf("worktree_resumed message = %q", event.Message)
		}
		return
	}
	t.Fatalf("worktree_resumed event missing: %+v", events)
}
