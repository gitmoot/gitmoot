package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestStableReviewTaskIdentityLetsSecondRoundVerdictReachGate(t *testing.T) {
	repoDir, oldHead, newHead := c1ReviewRepository(t)
	firstTaskID := mintC1ReviewTaskID(t, repoDir, oldHead)
	secondTaskID := mintC1ReviewTaskID(t, repoDir, newHead)

	decision := evaluateC1GateScenario(t, firstTaskID, secondTaskID, oldHead, newHead, firstTaskID, "reviewer", true)
	if !decision.Merged {
		t.Fatalf("second-round verdict did not reach first-round task: first=%q second=%q decision=%+v", firstTaskID, secondTaskID, decision)
	}
}

func TestStableReviewTaskIdentityKeepsHeadRoundIsolation(t *testing.T) {
	const stableTaskID = "review-pr-17-stable"
	decision := evaluateC1GateScenario(t, stableTaskID, stableTaskID, "old-head", "new-head", stableTaskID, "reviewer", false)
	if !decision.LeaveOpen || decision.Merged || !strings.Contains(decision.Reason.Render(), "different head SHA") {
		t.Fatalf("wrong-head verdict was not rejected by the head comparison: %+v", decision)
	}
}

func TestC1MeasurementEngineDispatchedImplementerIdentity(t *testing.T) {
	repoDir, oldHead, newHead := c1ReviewRepository(t)
	if oldHead == newHead {
		t.Fatalf("measurement requires different heads, both were %q", oldHead)
	}
	stableFirst := mintC1ReviewTaskID(t, repoDir, oldHead)
	stableSecond := mintC1ReviewTaskID(t, repoDir, newHead)
	legacyFirst := fmt.Sprintf("review-pr-%d-%s", 17, shortHash("owner/repo\x00"+oldHead))
	legacySecond := fmt.Sprintf("review-pr-%d-%s", 17, shortHash("owner/repo\x00"+newHead))

	for _, tc := range []struct {
		name            string
		implementTaskID string
		reviewTaskID    string
		wantAgents      int
		wantFailClosed  bool
	}{
		{name: "before_C1", implementTaskID: legacyFirst, reviewTaskID: legacySecond, wantAgents: 0, wantFailClosed: true},
		{name: "after_C1", implementTaskID: stableFirst, reviewTaskID: stableSecond, wantAgents: 1, wantFailClosed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := evaluateC1GateScenario(t, tc.implementTaskID, tc.reviewTaskID, oldHead, newHead, tc.reviewTaskID, "reviewer", true)
			implementingAgents, failClosed := observedC1ImplementerResolution(t, decision)
			if implementingAgents != tc.wantAgents || failClosed != tc.wantFailClosed {
				t.Fatalf("implementingAgents=%d failClosed=%v decision=%+v; want agents=%d failClosed=%v", implementingAgents, failClosed, decision, tc.wantAgents, tc.wantFailClosed)
			}
			if tc.wantFailClosed && decision.Merged {
				t.Fatalf("unknown implementer passed independence gate: %+v", decision)
			}
			if !tc.wantFailClosed && !decision.Merged {
				t.Fatalf("independent review did not merge after implementer resolution: %+v", decision)
			}
			t.Logf("old_head=%s new_head=%s implementingAgents=%d independence_fail_closed=%v", oldHead, newHead, implementingAgents, failClosed)
		})
	}
}

func observedC1ImplementerResolution(t *testing.T, decision workflow.MergeDecision) (int, bool) {
	t.Helper()
	switch {
	case decision.Ready && decision.Merged && !decision.LeaveOpen && !decision.Reason.IsGateMiss() && !decision.Deferred:
		return 1, false
	case !decision.Ready && !decision.Merged && decision.LeaveOpen && decision.Reason.IsGateMiss() && !decision.Deferred:
		return 0, true
	default:
		t.Fatalf("gate decision did not expose a structured implementer-resolution outcome: %+v", decision)
		return 0, false
	}
}

func c1ReviewRepository(t *testing.T) (repoDir string, oldHead string, newHead string) {
	t.Helper()
	ctx := context.Background()
	repoDir = t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("round one\n"), 0o644); err != nil {
		t.Fatalf("WriteFile round one: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "round one")
	var err error
	oldHead, err = (gitutil.NewHostClient(repoDir)).HeadSHA(ctx)
	if err != nil || oldHead == "" {
		t.Fatalf("round-one head = %q, err=%v", oldHead, err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("round two\n"), 0o644); err != nil {
		t.Fatalf("WriteFile round two: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "round two")
	newHead, err = (gitutil.NewHostClient(repoDir)).HeadSHA(ctx)
	if err != nil || newHead == "" || newHead == oldHead {
		t.Fatalf("round heads old=%q new=%q err=%v", oldHead, newHead, err)
	}
	return repoDir, oldHead, newHead
}

func mintC1ReviewTaskID(t *testing.T, repoDir string, headSHA string) string {
	t.Helper()
	_ = repoDir
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() { _ = store.Close() })
	request, err := prepareLocalReviewTask(
		context.Background(),
		store,
		github.Repository{Owner: "owner", Name: "repo"},
		localAgentDispatchRequest{Home: home, PullRequest: 17, HeadSHA: headSHA},
	)
	if err != nil {
		t.Fatalf("prepareLocalReviewTask(%s): %v", headSHA, err)
	}
	return request.TaskID
}

func evaluateC1GateScenario(t *testing.T, implementTaskID string, reviewTaskID string, oldHead string, newHead string, gateTaskID string, reviewerAgent string, includeCurrentReview bool) workflow.MergeDecision {
	t.Helper()
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t, false)
	gh.pr.HeadSHA = newHead
	request.HeadSHA = newHead
	request.TaskID = gateTaskID
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "implement-round", Agent: "implementer", Type: "implement", State: string(workflow.JobSucceeded),
	}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: oldHead, TaskID: implementTaskID,
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "review-round-one", Agent: reviewerAgent, Type: "review", State: string(workflow.JobSucceeded),
	}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: oldHead, TaskID: implementTaskID,
		ReviewRound: "review-1", Result: &workflow.AgentResult{Decision: "approved", Summary: "round one approved"},
	})
	if includeCurrentReview {
		seedDaemonMergeGateJob(t, store, db.Job{
			ID: "review-round-two", Agent: reviewerAgent, Type: "review", State: string(workflow.JobSucceeded),
		}, workflow.JobPayload{
			Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: newHead, TaskID: reviewTaskID,
			ReviewRound: "review-2", Result: &workflow.AgentResult{Decision: "approved", Summary: "round two approved"},
		})
	}
	decision, err := (newHostDaemonMergeGate(store, gh, checkout, "")).Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return decision
}

// #1530 acceptance 2 + 5, driven through the REAL dispatch path
// (dispatchLocalAgentJob): a review dispatched at a head the branch-owning
// task's registered checkout has NOT reached must still bind to that task,
// record a review_task_head_divergence job event naming BOTH SHAs, and mint no
// review-pr-* identity. The matching-head subtest pins the unchanged case: a
// bind without divergence records no event.
func TestDispatchReviewRebindsOwningTaskOnHeadDivergence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		diverged      bool
		wantEvent     bool
		wantEventSHAa string // filled from fixture heads
		wantEventSHAb string
	}{
		{name: "diverged head binds and records the divergence", diverged: true, wantEvent: true},
		{name: "matching head binds without a divergence event", diverged: false, wantEvent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := blockerE2EHome(t)
			checkout, firstHead, secondHead := readonlyReviewWorktreeGitCheckout(t)
			seedReviewDispatchFixture(t, store, checkout)
			replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
				return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
			})
			// The implement task owns (repo, branch); its registered checkout is a
			// worktree pinned at the FIRST head — the strand a fix-worktree leg
			// leaves behind when it pushes from an independent clone.
			taskWorktree := filepath.Join(t.TempDir(), "task-worktree")
			runGit(t, checkout, "worktree", "add", "--detach", taskWorktree, firstHead)
			if got := readonlyWorktreeHead(t, taskWorktree); got != firstHead || firstHead == secondHead {
				t.Fatalf("fixture pin failed: task worktree head=%q first=%q second=%q", got, firstHead, secondHead)
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID: "adhoc-impl", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Implement the fix",
				State: string(workflow.TaskPullRequestOpen), Branch: "feature/review", WorktreePath: taskWorktree,
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			dispatchHead := firstHead
			if tc.diverged {
				dispatchHead = secondHead
			}
			out, err := dispatchLocalAgentJob(ctx, store, reviewDispatchRequest(home, dispatchHead))
			if err != nil {
				t.Fatalf("dispatchLocalAgentJob: %v", err)
			}
			job, err := store.GetJob(ctx, out.JobID)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			payload, err := daemonJobPayload(job)
			if err != nil {
				t.Fatalf("daemonJobPayload: %v", err)
			}
			if payload.TaskID != "adhoc-impl" {
				t.Fatalf("review payload TaskID=%q, want rebound to the branch-owning task %q", payload.TaskID, "adhoc-impl")
			}
			events, err := store.ListJobEvents(ctx, job.ID)
			if err != nil {
				t.Fatalf("ListJobEvents: %v", err)
			}
			var divergenceEvents []db.JobEvent
			for _, event := range events {
				if event.Kind == "review_task_head_divergence" {
					divergenceEvents = append(divergenceEvents, event)
				}
			}
			if !tc.wantEvent {
				if len(divergenceEvents) != 0 {
					t.Fatalf("matching-head bind recorded divergence events %+v; want none", divergenceEvents)
				}
			} else {
				if len(divergenceEvents) != 1 {
					t.Fatalf("diverged-head bind recorded %d divergence events; want 1: %+v", len(divergenceEvents), divergenceEvents)
				}
				divergence := divergenceEvents[0].Message
				for _, fragment := range []string{"adhoc-impl", firstHead, secondHead} {
					if !strings.Contains(divergence, fragment) {
						t.Fatalf("divergence event = %q, want it to name %q", divergence, fragment)
					}
				}
			}
			tasks, err := store.ListTasks(ctx)
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			for _, task := range tasks {
				if strings.HasPrefix(task.ID, "review-pr-") {
					t.Fatalf("minted a fresh review identity %q despite the owning-task rebind", task.ID)
				}
			}
		})
	}
}

func TestDispatchReviewDivergenceEventFailureRollsBackEnqueue(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout, firstHead, secondHead := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	taskWorktree := filepath.Join(t.TempDir(), "task-worktree")
	runGit(t, checkout, "worktree", "add", "--detach", taskWorktree, firstHead)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "adhoc-impl", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Implement the fix",
		State: string(workflow.TaskPullRequestOpen), Branch: "feature/review", WorktreePath: taskWorktree,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_review_task_head_divergence
		BEFORE INSERT ON job_events
		WHEN NEW.kind = 'review_task_head_divergence'
		BEGIN
			SELECT RAISE(FAIL, 'forced divergence event failure');
		END`); err != nil {
		t.Fatalf("create divergence event rejection trigger: %v", err)
	}

	out, err := dispatchLocalAgentJob(ctx, store, reviewDispatchRequest(home, secondHead))
	if err == nil || !strings.Contains(err.Error(), "forced divergence event failure") {
		t.Fatalf("dispatchLocalAgentJob output=%+v err=%v, want required-event failure", out, err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("required divergence event failure left runnable jobs: %+v", jobs)
	}
	var eventCount int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM job_events`).Scan(&eventCount); err != nil {
		t.Fatalf("count job events after rollback: %v", err)
	}
	if eventCount != 0 {
		t.Fatalf("required divergence event failure left %d partial audit events; want 0", eventCount)
	}
}

// #1530 acceptance 6: the measured strand end to end on an isolated home with
// the shell runtime. A fix-worktree implement leg pushes the PR branch from an
// independent clone (through the real implementation finalizer) and the clone is
// removed; the implement task's registered checkout stays behind. The next
// review must bind to the implement task — not mint review-pr-* — so the merge
// gate can attribute it.
func TestFixWorktreeStrandReviewBindsImplementTaskE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	const branch = "feature/fix-strand"
	registered := createDaemonWorkerGitCheckout(t, "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runDaemonWorkerGit(t, filepath.Dir(remote), "init", "--bare", remote)
	// origin stays the GitHub remote (dispatch repo resolution requires it); the
	// bare repo stands in for the forge and is reached through a second remote.
	runDaemonWorkerGit(t, registered, "remote", "add", "forge", remote)
	runDaemonWorkerGit(t, registered, "switch", "-c", branch)
	runDaemonWorkerGit(t, registered, "push", "-u", "forge", branch)
	runDaemonWorkerGit(t, registered, "switch", "main")
	strandedHead := strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "forge/"+branch))
	// The implement task's registered checkout: a linked worktree on the PR
	// branch at the pre-fix head.
	taskWorktree := filepath.Join(t.TempDir(), "task-worktree")
	runDaemonWorkerGit(t, registered, "worktree", "add", taskWorktree, branch)
	seedReviewDispatchFixture(t, store, registered)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "adhoc-impl", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Implement the fix",
		State: string(workflow.TaskPullRequestOpen), Branch: branch, WorktreePath: taskWorktree,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo", Number: 1530, URL: "https://example.invalid/pull/1530",
		HeadBranch: branch, BaseBranch: "main", State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	// The fix-worktree leg: an independent clone that commits and pushes through
	// the real finalizer, then is removed — exactly the engine's
	// delegation_worktree_removed cleanup. Nothing advances the task's
	// registered checkout.
	fixWorktree := filepath.Join(t.TempDir(), "fix-worktree")
	runDaemonWorkerGit(t, filepath.Dir(fixWorktree), "clone", "--branch", branch, remote, fixWorktree)
	configureTestGit(t, fixWorktree)
	if err := os.WriteFile(filepath.Join(fixWorktree, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatalf("write fix change: %v", err)
	}
	if _, err := (newHostDaemonImplementationFinalizer(store, github.NoopClient{})).FinalizeImplementation(
		ctx, db.Job{ID: "fix-leg", Agent: "implementer", Type: "implement"}, workflow.JobPayload{
			Repo: "owner/repo", Branch: branch, PullRequest: 1530, TaskID: "adhoc-impl",
			FixWorktree: true, WorktreePath: fixWorktree,
			Result: &workflow.AgentResult{Decision: "implemented"},
		}); err != nil {
		t.Fatalf("FinalizeImplementation (fix-worktree leg): %v", err)
	}
	advancedHead := lsRemoteHead(t, registered, "forge", branch)
	if advancedHead == "" || advancedHead == strandedHead {
		t.Fatalf("fix leg did not advance the branch: remote head=%q stranded=%q", advancedHead, strandedHead)
	}
	if err := os.RemoveAll(fixWorktree); err != nil {
		t.Fatalf("remove fix worktree: %v", err)
	}
	// Fixture pin: the registered checkout is genuinely stranded BEHIND the PR
	// head. Without this the rebind below is vacuous.
	if got := readonlyWorktreeHead(t, taskWorktree); got != strandedHead {
		t.Fatalf("registered checkout moved: head=%q, want stranded at %q", got, strandedHead)
	}
	// Make the advanced head reachable from the registered checkout so the
	// exact-head review worktree allocation can resolve it.
	runDaemonWorkerGit(t, registered, "fetch", "forge")
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", LeadAgent: "implementer",
		Action: "review", Instructions: "Review the advanced head.", Background: true,
		PullRequest: 1530, Branch: branch, HeadSHA: advancedHead, Home: home,
	})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob: %v", err)
	}
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}
	if payload.TaskID != "adhoc-impl" {
		t.Fatalf("review payload TaskID=%q, want the implement task %q; a review-pr-* mint is the #1530 defect", payload.TaskID, "adhoc-impl")
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	divergence := ""
	for _, event := range events {
		if event.Kind == "review_task_head_divergence" {
			divergence = event.Message
		}
	}
	for _, fragment := range []string{"adhoc-impl", strandedHead, advancedHead} {
		if !strings.Contains(divergence, fragment) {
			t.Fatalf("divergence event = %q, want it to name %q", divergence, fragment)
		}
	}
	// The merge gate can now attribute the review round to the implement job:
	// with the rebound identity at the advanced head, the independence check
	// resolves the implementer and merges.
	decision := evaluateC1GateScenario(t, payload.TaskID, payload.TaskID, strandedHead, advancedHead, payload.TaskID, "reviewer", true)
	if !decision.Merged {
		t.Fatalf("merge gate could not attribute the rebound review: %+v", decision)
	}
}
