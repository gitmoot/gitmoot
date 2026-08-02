package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
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

func (f *activeJobMergeGateGitHub) ListCheckRunsForRef(context.Context, github.Repository, string) ([]github.PullRequestCheck, error) {
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
		if !strings.Contains(decision.Reason.Render(), want) {
			t.Fatalf("hold reason %q does not contain %q", decision.Reason.Render(), want)
		}
	}
	if len(gh.mergeInputs) != 0 {
		t.Fatalf("active branch was merged/deleted: %+v", gh.mergeInputs)
	}
}

func TestDaemonMergeGateHoldsHumanMergeRequestWhileJobActiveOnBranch(t *testing.T) {
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t)
	request.HumanMergeRequested = true
	seedDaemonMergeGateJob(t, store, db.Job{
		ID: "fix-round-running", Agent: "implementer", Type: "implement", State: string(workflow.JobRunning),
	}, workflow.JobPayload{Repo: request.Repo, Branch: request.Branch, TaskID: request.TaskID})

	decision, err := (daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout}).Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !decision.Deferred || decision.Merged || decision.BlockClass != workflow.MergeBlockTransient {
		t.Fatalf("active human-request decision = %+v, want transient deferred not-ready hold", decision)
	}
	if len(gh.mergeInputs) != 0 {
		t.Fatalf("explicit human request merged active branch: %+v", gh.mergeInputs)
	}
}

func TestDaemonMergeGateDefaultPreservesMergePathWhenMandatoryGatePasses(t *testing.T) {
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize config: %v", err)
	}

	decision, err := (daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout, Home: paths.Home}).Evaluate(context.Background(), request)
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

func TestDaemonMergeGateMissingReviewEscalatesToJarvisOnce(t *testing.T) {
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t, false)
	request.WorkflowID = "goal-1017"
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	content := config.DefaultConfig(paths) + `
[org.roles."owner"]
scope = ["*"]
[org.roles."jarvis"]
parent = "owner"
scope = ["*"]
pane = "w1:p1"
[org.roles."worker"]
parent = "jarvis"
scope = ["owner/repo"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadOrg(paths); err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	gate := daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout, Home: paths.Home}
	for attempt := 0; attempt < 2; attempt++ {
		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate attempt %d: %v", attempt+1, err)
		}
		if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || !strings.Contains(decision.Reason.Render(), "final agent review is not captured") {
			t.Fatalf("decision = %+v", decision)
		}
	}
	notes, err := store.ListWorkflowNotes(context.Background(), request.WorkflowID, 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %+v, err=%v; want one escalation", notes, err)
	}
	from, to, wf, question, ok := workflow.ParseOrgEscalateNote(notes[0].Body)
	if !ok || from != "worker" || to != "jarvis" || wf != request.WorkflowID || !strings.Contains(question, "final agent review is not captured") {
		t.Fatalf("escalation = from=%q to=%q wf=%q question=%q ok=%v", from, to, wf, question, ok)
	}
	outbox, err := store.ListWakeOutbox(context.Background(), "")
	if err != nil || len(outbox) != 1 || outbox[0].State != db.WakeOutboxStatePending ||
		outbox[0].TargetRole != "jarvis" || outbox[0].SourceID != fmt.Sprint(notes[0].ID) {
		t.Fatalf("merge-gate wake outbox = %+v, err=%v", outbox, err)
	}
}

func TestDaemonMergeGateKillSwitchDoesNotEscalate(t *testing.T) {
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t, false)
	request.WorkflowID = "goal-1017"
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+"\n[merge_gate]\nauto_merge = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	decision, err := (daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout, Home: paths.Home}).Evaluate(context.Background(), request)
	if err != nil || !decision.LeaveOpen || decision.EscalateMergeGateMiss {
		t.Fatalf("decision = %+v, err=%v", decision, err)
	}
	notes, err := store.ListWorkflowNotes(context.Background(), request.WorkflowID, 0)
	outbox, outboxErr := store.ListWakeOutbox(context.Background(), "")
	if err != nil || len(notes) != 0 || outboxErr != nil || len(outbox) != 0 {
		t.Fatalf("notes=%+v outbox=%+v err=%v outbox_err=%v", notes, outbox, err, outboxErr)
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

func daemonMergeGateActiveJobFixture(t *testing.T, seedReview ...bool) (*db.Store, string, *activeJobMergeGateGitHub, workflow.MergeRequest) {
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
	if len(seedReview) == 0 || seedReview[0] {
		seedDaemonMergeGateJob(t, store, db.Job{
			ID: "implement-complete", Agent: "implementer", Type: "implement", State: string(workflow.JobSucceeded),
		}, workflow.JobPayload{
			Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: "head123", TaskID: "task-1017",
			Result: &workflow.AgentResult{Decision: "implemented", Summary: "implemented"},
		})
		seedDaemonMergeGateJob(t, store, db.Job{
			ID: "review-approved", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded),
		}, workflow.JobPayload{
			Repo: "owner/repo", Branch: "fix-round", PullRequest: 17, HeadSHA: "head123", TaskID: "task-1017",
			ReviewRound: "review-2", Result: &workflow.AgentResult{Decision: "approved", Summary: "ready"},
		})
	}
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
