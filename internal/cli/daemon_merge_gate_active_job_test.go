package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type activeJobMergeGateGitHub struct {
	github.NoopClient
	pr          github.PullRequest
	mergeInputs []github.MergePullRequestInput
	statuses    []github.CommitStatusInput
}

func (f *activeJobMergeGateGitHub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	return f.pr, nil
}

func (f *activeJobMergeGateGitHub) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	return github.CombinedStatus{State: "success"}, nil
}

func (f *activeJobMergeGateGitHub) CompareCommits(context.Context, github.Repository, string, string) (github.CompareResult, error) {
	return github.CompareResult{Status: "ahead", AheadBy: 1}, nil
}

func (f *activeJobMergeGateGitHub) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	return []github.PullRequestCheck{{Name: "build", State: "SUCCESS", Bucket: "pass"}}, nil
}

func (f *activeJobMergeGateGitHub) CreateCommitStatus(_ context.Context, input github.CommitStatusInput) (github.CommitStatus, error) {
	f.statuses = append(f.statuses, input)
	return github.CommitStatus{State: input.State, Context: input.Context}, nil
}

func (f *activeJobMergeGateGitHub) MergePullRequest(_ context.Context, input github.MergePullRequestInput) (github.MergeResult, error) {
	f.mergeInputs = append(f.mergeInputs, input)
	return github.MergeResult{Merged: true, SHA: "merge123"}, nil
}

func TestDaemonMergeGateHoldsWhileImplementJobActiveOnBranch(t *testing.T) {
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t)
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "fix-round-running", Agent: "implementer", Type: "implement", State: string(workflow.JobRunning),
	}, workflow.JobPayload{Repo: request.Repo, Branch: request.Branch, TaskID: request.TaskID})

	decision, err := (daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout}).Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !decision.Deferred || decision.Merged || decision.BlockClass != workflow.MergeBlockTransient {
		t.Fatalf("active-job decision = %+v, want transient deferred not-ready hold", decision)
	}
	for _, want := range []string{"active implement job fix-round-running", "branch fix-round"} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("hold reason %q does not contain %q", decision.Reason, want)
		}
	}
	if len(gh.mergeInputs) != 0 {
		t.Fatalf("active branch was merged/deleted: %+v", gh.mergeInputs)
	}
}

func TestDaemonMergeGateWithoutActiveBranchJobPreservesMergePath(t *testing.T) {
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t)

	decision, err := (daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout}).Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Deferred || !decision.Merged || decision.MergeCommitSHA != "merge123" {
		t.Fatalf("no-active-job decision = %+v, want existing merged path", decision)
	}
	if len(gh.mergeInputs) != 1 || gh.mergeInputs[0].Method != "squash" || gh.mergeInputs[0].Number != 17 ||
		!gh.mergeInputs[0].DeleteBranch || gh.mergeInputs[0].MatchHeadCommit != "head123" {
		t.Fatalf("merge inputs = %+v, want one unchanged squash/delete request", gh.mergeInputs)
	}
}

func TestFindActiveJobForBranchCoversAllJobTypesAndActiveStates(t *testing.T) {
	for _, jobType := range []string{"ask", "review", "implement"} {
		for _, state := range []workflow.JobState{workflow.JobQueued, workflow.JobRunning} {
			t.Run(jobType+"/"+string(state), func(t *testing.T) {
				store := daemonWorkerStore(t)
				seedDaemonMergeGateJob(t, store, db.Job{
					ID: "settled-first", Type: jobType, State: string(workflow.JobSucceeded),
				}, workflow.JobPayload{Repo: "owner/repo", Branch: "fix-round"})
				seedDaemonMergeGateJob(t, store, db.Job{
					ID: "target-active", Type: jobType, State: string(state),
				}, workflow.JobPayload{Repo: "owner/repo", Branch: "fix-round", TaskID: "another-task"})
				seedDaemonMergeGateJob(t, store, db.Job{
					ID: "wrong-branch-active", Type: jobType, State: string(state),
				}, workflow.JobPayload{Repo: "owner/repo", Branch: "other"})

				job, found, err := findActiveJobForBranch(context.Background(), store, "owner/repo", "fix-round")
				if err != nil {
					t.Fatal(err)
				}
				if !found || job.ID != "target-active" || job.Type != jobType || job.State != string(state) {
					t.Fatalf("active branch job = %+v found=%v, want %s %s", job, found, jobType, state)
				}
			})
		}
	}
}

func TestFindActiveImplementJobForTaskStillIgnoresOtherActiveTypes(t *testing.T) {
	store := daemonWorkerStore(t)
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "a-ask", Type: "ask", State: string(workflow.JobRunning),
	}, workflow.JobPayload{Repo: "owner/repo", Branch: "fix-round", TaskID: "task-1017"})
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "z-implement", Type: "implement", State: string(workflow.JobQueued),
	}, workflow.JobPayload{Repo: "owner/repo", Branch: "fix-round", TaskID: "task-1017"})

	job, found, err := findActiveImplementJobForTask(context.Background(), store, "owner/repo", "fix-round", "task-1017")
	if err != nil {
		t.Fatal(err)
	}
	if !found || job.ID != "z-implement" {
		t.Fatalf("active implement job = %+v found=%v, want z-implement", job, found)
	}
}

func daemonMergeGateActiveJobFixture(t *testing.T) (*db.Store, string, *activeJobMergeGateGitHub, workflow.MergeRequest) {
	t.Helper()
	t.Setenv("GITMOOT_DISABLE_NATIVE_MERGE_GATE", "")
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-1017", RepoFullName: "owner/repo", GoalID: "goal-1017", Title: "Fix round",
		State: string(workflow.TaskReadyToMerge), Branch: "fix-round",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo", Number: 17, URL: "https://github.com/owner/repo/pull/17",
		HeadBranch: "fix-round", BaseBranch: "main", HeadSHA: "head123", State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "review-approved", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded),
	}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: "head123", TaskID: "task-1017",
		ReviewRound: "review-2", Result: &workflow.AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &activeJobMergeGateGitHub{pr: github.PullRequest{
		Number: 17, Title: "Fix round", State: "open", URL: "https://github.com/owner/repo/pull/17",
		HeadRef: "fix-round", BaseSHA: "base123", HeadSHA: "head123", Mergeable: &mergeable,
	}}
	request := workflow.MergeRequest{
		Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: "head123",
		TaskID: "task-1017", Reviewer: "reviewer",
	}
	return store, checkout, gh, request
}

func seedDaemonMergeGateJob(t *testing.T, store *db.Store, job db.Job, payload workflow.JobPayload) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = string(raw)
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: job.State, Message: "test fixture"}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", job.ID, err)
	}
}
