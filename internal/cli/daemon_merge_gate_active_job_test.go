package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
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

	decision, err := (newHostDaemonMergeGate(store, gh, checkout, "")).Evaluate(context.Background(), request)
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

	decision, err := (newHostDaemonMergeGate(store, gh, checkout, "")).Evaluate(context.Background(), request)
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

	decision, err := (newHostDaemonMergeGate(store, gh, checkout, paths.Home)).Evaluate(context.Background(), request)
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

// The chart here is built so a LITERAL "jarvis" and a correct parent lookup
// DISAGREE: jarvis is declared, but on a branch that does not own owner/repo, so
// the escalating role's parent is coordinator. A fixture whose parent is jarvis
// cannot tell the two apart, which is how the hardcoded target survived (#1727).
func TestDaemonMergeGateMissingReviewEscalatesToParentRoleOnce(t *testing.T) {
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
scope = ["oversight/*"]
pane = "w1:p1"
[org.roles."coordinator"]
parent = "owner"
scope = ["owner/*"]
pane = "w1:p2"
[org.roles."worker"]
parent = "coordinator"
scope = ["owner/repo"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadOrg(paths); err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	gate := newHostDaemonMergeGate(store, gh, checkout, paths.Home)
	renderedReason := ""
	for attempt := 0; attempt < 2; attempt++ {
		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate attempt %d: %v", attempt+1, err)
		}
		if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || !strings.Contains(decision.Reason.Render(), "final agent review is not captured") {
			t.Fatalf("decision = %+v", decision)
		}
		if got := decision.Reason.Render(); renderedReason == "" {
			renderedReason = got
		} else if got != renderedReason {
			t.Fatalf("rendered reason changed across idempotent evaluations: first=%q attempt_%d=%q", renderedReason, attempt+1, got)
		}
	}
	notes, err := store.ListWorkflowNotes(context.Background(), request.WorkflowID, 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %+v, err=%v; want one escalation", notes, err)
	}
	from, to, wf, question, ok := workflow.ParseOrgEscalateNote(notes[0].Body)
	if !ok || from != "worker" || to != "coordinator" || wf != request.WorkflowID || question != renderedReason {
		t.Fatalf("escalation = from=%q to=%q wf=%q question=%q ok=%v", from, to, wf, question, ok)
	}
	if strings.Contains(notes[0].Body, "jarvis") {
		t.Fatalf("escalation addressed the pre-#1727 literal instead of the chart parent: %q", notes[0].Body)
	}
	outbox, err := store.ListWakeOutbox(context.Background(), "")
	if err != nil || len(outbox) != 1 || outbox[0].State != db.WakeOutboxStatePending ||
		outbox[0].TargetRole != "coordinator" || outbox[0].SourceID != fmt.Sprint(notes[0].ID) {
		t.Fatalf("merge-gate wake outbox = %+v, err=%v", outbox, err)
	}
	// An escalation must route AS an escalation. With no wake kind the row keys
	// "reply:<role>", and delivery then demands an on=reply rule the escalation
	// routes provisioned by org seat add do not satisfy (#1728 review).
	if outbox[0].CoalesceKey != "escalation:coordinator" {
		t.Fatalf("merge-gate wake coalesce key = %q, want escalation:coordinator", outbox[0].CoalesceKey)
	}
}

// The end-to-end test above pins the parent path through gate.Evaluate. This pins
// the branches a single gate run cannot reach, and every case expects a DIFFERENT
// role: a table whose rows all expect "owner" cannot discriminate its own ladder.
func TestMergeGateEscalationToResolvesChartAndDeliverability(t *testing.T) {
	chart := func(t *testing.T, body string) config.OrgConfig {
		t.Helper()
		paths := config.PathsForHome(t.TempDir())
		if err := config.Initialize(paths); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadOrg(paths)
		if err != nil {
			t.Fatalf("LoadOrg: %v", err)
		}
		return cfg
	}
	// "vetrina" is BOTH a declared role and a plausible repo-name segment, which is
	// the collision the fromDeclared flag exists to refuse. Its parent is DELIBERATELY
	// not the root: if provenance were ignored, the chart would place it and route to
	// "lead", so the expected "owner" is a discriminating value rather than a
	// coincidence of the ladder's last rung.
	populated := `
[org.roles."owner"]
scope = ["*"]
[org.roles."lead"]
parent = "owner"
scope = ["*"]
[org.roles."coordinator"]
parent = "lead"
scope = ["owner/repo"]
[org.roles."vetrina"]
parent = "lead"
scope = ["other/repo"]
`
	reachable := func(roles ...string) func(string) bool {
		return func(role string) bool { return slices.Contains(roles, role) }
	}
	for _, test := range []struct {
		name        string
		body        string
		from        string
		declared    bool
		deliverable func(string) bool
		want        string
	}{
		{
			name: "nearest ancestor when it can be woken", body: populated,
			from: "coordinator", declared: true, deliverable: reachable("lead", "owner"), want: "lead",
		},
		{
			name: "climbs past an ancestor with no wake route", body: populated,
			from: "coordinator", declared: true, deliverable: reachable("owner"), want: "owner",
		},
		{
			name: "keeps the nearest ancestor when nobody can be woken", body: populated,
			from: "coordinator", declared: true, deliverable: reachable(), want: "lead",
		},
		{
			name: "chart root is the actor", body: populated,
			from: "owner", declared: true, deliverable: reachable("owner"), want: "owner",
		},
		{
			name: "repo-name segment colliding with a role never inherits its parent", body: populated,
			from: "vetrina", declared: false, deliverable: reachable("lead", "owner"), want: "owner",
		},
		{
			name: "absent chart falls back to the root name", body: "",
			from: "appkit-demo", declared: false, deliverable: reachable(), want: "owner",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := mergeGateEscalationTo(chart(t, test.body), test.from, test.declared, test.deliverable)
			if got != test.want {
				t.Fatalf("mergeGateEscalationTo(%q, declared=%t) = %q, want %q", test.from, test.declared, got, test.want)
			}
		})
	}
}

// repoOrgOwner picks the deepest scope match and breaks equal-depth ties by name.
// Pre-#1727 that chose only the note's author; it now chooses the RECIPIENT's
// branch too, so the tie-break is load-bearing and pinned here.
func TestMergeGateEscalationFromBreaksEqualDepthScopeTiesByName(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	content := config.DefaultConfig(paths) + `
[org.roles."owner"]
scope = ["*"]
[org.roles."zeta"]
parent = "owner"
scope = ["*"]
[org.roles."alpha"]
parent = "owner"
scope = ["*"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	from, declared := mergeGateEscalationFrom(cfg, "owner/repo")
	if from != "alpha" || !declared {
		t.Fatalf("mergeGateEscalationFrom = (%q, %t), want the alphabetically first equal-depth match", from, declared)
	}
}

func TestDaemonMergeGateEmptyReasonRefusesEscalation(t *testing.T) {
	store := daemonWorkerStore(t)
	const workflowID = "goal-empty-reason"

	err := (daemonMergeGate{Store: store}).escalateMergeGateMiss(
		context.Background(),
		workflow.MergeRequest{Repo: "owner/repo", WorkflowID: workflowID},
		workflow.MergeReason{},
	)
	notes, notesErr := store.ListWorkflowNotes(context.Background(), workflowID, 0)
	if notesErr != nil {
		t.Fatalf("ListWorkflowNotes: %v", notesErr)
	}
	if len(notes) != 0 {
		t.Fatalf("empty reason wrote %d workflow note(s), want none: %+v", len(notes), notes)
	}
	if err == nil || !strings.Contains(err.Error(), "refusing to journal an empty operator instruction") {
		t.Fatalf("error = %v, want explicit empty-reason refusal", err)
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
	decision, err := (newHostDaemonMergeGate(store, gh, checkout, paths.Home)).Evaluate(context.Background(), request)
	if err != nil || !decision.LeaveOpen || decision.Reason.IsGateMiss() {
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
