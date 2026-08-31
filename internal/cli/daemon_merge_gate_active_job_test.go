package cli

import (
	"context"
	"encoding/json"
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

// The chart starts with a literal "jarvis" on an unrelated branch so the first
// target must come from the selected role's actual parent. The next two ticks
// mutate route inputs around one unchanged gate miss: author churn under the same
// parent must deduplicate, while a new accountable parent must receive a new note.
func TestDaemonMergeGateEscalatesOncePerAccountableRecipient(t *testing.T) {
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t, false)
	request.WorkflowID = "goal-1017"
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	writeChart := func(body string) {
		t.Helper()
		if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.LoadOrg(paths); err != nil {
			t.Fatalf("LoadOrg: %v", err)
		}
	}
	evaluate := func() string {
		t.Helper()
		decision, err := newHostDaemonMergeGate(store, gh, checkout, paths.Home).Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !decision.LeaveOpen || !decision.Reason.IsGateMiss() ||
			!strings.Contains(decision.Reason.Render(), "final agent review is not captured") {
			t.Fatalf("decision = %+v", decision)
		}
		return decision.Reason.Render()
	}
	notes := func() []db.WorkflowNote {
		t.Helper()
		got, err := store.ListWorkflowNotes(context.Background(), request.WorkflowID, 0)
		if err != nil {
			t.Fatalf("ListWorkflowNotes: %v", err)
		}
		return got
	}

	writeChart(`
[org.roles."owner"]
scope = ["*"]
[org.roles."jarvis"]
parent = "owner"
scope = ["oversight/*"]
[org.roles."coordinator"]
parent = "owner"
scope = ["owner/*"]
[org.roles."worker"]
parent = "coordinator"
scope = ["owner/repo"]
`)
	renderedReason := evaluate()
	if got := notes(); len(got) != 1 {
		t.Fatalf("first evaluation wrote %d notes, want 1: %+v", len(got), got)
	}

	writeChart(`
[org.roles."owner"]
scope = ["*"]
[org.roles."jarvis"]
parent = "owner"
scope = ["oversight/*"]
[org.roles."coordinator"]
parent = "owner"
scope = ["owner/*"]
[org.roles."alpha"]
parent = "coordinator"
scope = ["owner/repo"]
[org.roles."worker"]
parent = "coordinator"
scope = ["owner/repo"]
`)
	if got := evaluate(); got != renderedReason {
		t.Fatalf("rendered reason changed after author churn: first=%q second=%q", renderedReason, got)
	}
	if got := notes(); len(got) != 1 {
		t.Fatalf("same-recipient author churn wrote %d notes, want 1: %+v", len(got), got)
	}

	writeChart(`
[org.roles."owner"]
scope = ["*"]
[org.roles."jarvis"]
parent = "owner"
scope = ["oversight/*"]
[org.roles."coordinator"]
parent = "owner"
scope = ["owner/*"]
[org.roles."coordinator-b"]
parent = "owner"
scope = ["owner/*"]
[org.roles."alpha"]
parent = "coordinator-b"
scope = ["owner/repo"]
[org.roles."worker"]
parent = "coordinator"
scope = ["owner/repo"]
`)
	if got := evaluate(); got != renderedReason {
		t.Fatalf("rendered reason changed after recipient change: first=%q third=%q", renderedReason, got)
	}
	gotNotes := notes()
	if len(gotNotes) != 2 {
		t.Fatalf("new recipient left %d notes, want 2: %+v", len(gotNotes), gotNotes)
	}
	gotRoutes := map[string]string{}
	for _, note := range gotNotes {
		from, to, wf, question, ok := workflow.ParseOrgEscalateNote(note.Body)
		if !ok || wf != request.WorkflowID || question != renderedReason {
			t.Fatalf("invalid escalation note %+v: from=%q to=%q wf=%q question=%q ok=%v", note, from, to, wf, question, ok)
		}
		gotRoutes[to] = from
		if strings.Contains(note.Body, "to=jarvis") {
			t.Fatalf("escalation addressed the pre-#1727 literal: %q", note.Body)
		}
	}
	if gotRoutes["coordinator"] != "worker" || gotRoutes["coordinator-b"] != "alpha" {
		t.Fatalf("escalation routes = %+v, want coordinator<-worker and coordinator-b<-alpha", gotRoutes)
	}
	outbox, err := store.ListWakeOutbox(context.Background(), "")
	if err != nil || len(outbox) != 2 {
		t.Fatalf("merge-gate wake outbox = %+v, err=%v; want 2 rows", outbox, err)
	}
	gotTargets := map[string]string{}
	for _, row := range outbox {
		if row.State != db.WakeOutboxStatePending {
			t.Fatalf("wake outbox row = %+v, want pending", row)
		}
		gotTargets[row.TargetRole] = row.CoalesceKey
	}
	if gotTargets["coordinator"] != "reply:coordinator" ||
		gotTargets["coordinator-b"] != "reply:coordinator-b" {
		t.Fatalf("wake targets = %+v", gotTargets)
	}
}

func TestDaemonMergeGateEscalationSkipsArchivedAncestor(t *testing.T) {
	store, checkout, gh, request := daemonMergeGateActiveJobFixture(t, false)
	request.WorkflowID = "goal-archived-ancestor"
	if err := store.UpsertOrgRoleArchived(context.Background(), db.OrgRoleArchived{
		Role: "coordinator", ArchivedAt: "2026-08-31T00:00:00Z",
		ArchivedBy: "herdr-app", ObservedAt: "2026-08-31T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertOrgRoleArchived: %v", err)
	}
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	content := config.DefaultConfig(paths) + `
[org.roles."owner"]
scope = ["*"]
[org.roles."coordinator"]
parent = "owner"
scope = ["owner/*"]
[org.roles."worker"]
parent = "coordinator"
scope = ["owner/repo"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	decision, err := newHostDaemonMergeGate(store, gh, checkout, paths.Home).Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() {
		t.Fatalf("decision = %+v, want gate miss", decision)
	}
	notes, err := store.ListWorkflowNotes(context.Background(), request.WorkflowID, 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %+v, err=%v; want one escalation", notes, err)
	}
	from, to, _, _, ok := workflow.ParseOrgEscalateNote(notes[0].Body)
	if !ok || from != "worker" || to != "owner" {
		t.Fatalf("archived-ancestor escalation = from=%q to=%q ok=%v, want worker->owner", from, to, ok)
	}
	outbox, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(outbox) != 1 || outbox[0].TargetRole != "owner" {
		t.Fatalf("archived-ancestor outbox = %+v, err=%v; want live owner", outbox, err)
	}
}

// The end-to-end test above pins mutable author and recipient behavior through
// gate.Evaluate. This table pins the remaining target-selection branches.
func TestMergeGateEscalationToResolvesChartBranches(t *testing.T) {
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
	for _, test := range []struct {
		name     string
		body     string
		from     string
		declared bool
		want     string
	}{
		{
			name: "declared role escalates to its nearest ancestor", body: populated,
			from: "coordinator", declared: true, want: "lead",
		},
		{
			name: "chart root is the actor", body: populated,
			from: "owner", declared: true, want: "owner",
		},
		{
			name: "repo-name segment colliding with a role never inherits its parent", body: populated,
			from: "vetrina", declared: false, want: "owner",
		},
		{
			name: "absent chart has no live fallback", body: "",
			from: "appkit-demo", declared: false, want: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := chart(t, test.body)
			got := mergeGateEscalationTo(loadOrgRoster(context.Background(), nil, cfg), cfg, test.from, test.declared)
			if got != test.want {
				t.Fatalf("mergeGateEscalationTo(%q, declared=%t) = %q, want %q", test.from, test.declared, got, test.want)
			}
		})
	}
}

func TestMergeGateEscalationToRejectsArchivedOwnerFallback(t *testing.T) {
	store := daemonWorkerStore(t)
	if err := store.UpsertOrgRoleArchived(context.Background(), db.OrgRoleArchived{
		Role: "owner", ArchivedAt: "2026-08-31T00:00:00Z",
		ArchivedBy: "herdr-app", ObservedAt: "2026-08-31T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertOrgRoleArchived: %v", err)
	}
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	content := config.DefaultConfig(paths) + `
[org.roles."owner"]
scope = ["*"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	roster := loadOrgRoster(context.Background(), store, cfg)
	if got := mergeGateEscalationTo(roster, cfg, "repo", false); got != "" {
		t.Fatalf("mergeGateEscalationTo archived fallback = %q, want no recipient", got)
	}
}

func TestMergeGateEscalationFromPrefersSpecificScope(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	content := config.DefaultConfig(paths) + `
[org.roles."owner"]
scope = ["*"]
[org.roles."exact"]
parent = "owner"
scope = ["owner/repo"]
[org.roles."lead"]
parent = "owner"
scope = ["*"]
[org.roles."a"]
parent = "lead"
scope = ["*"]
[org.roles."b"]
parent = "a"
scope = ["*"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	roster := loadOrgRoster(context.Background(), daemonWorkerStore(t), cfg)
	from, declared := mergeGateEscalationFrom(roster, cfg, "owner/repo")
	if from != "exact" || !declared {
		t.Fatalf("mergeGateEscalationFrom = (%q, %t), want exact repo scope over deeper wildcard", from, declared)
	}
}

func TestMergeGateEscalationFromExcludesArchivedSeats(t *testing.T) {
	store := daemonWorkerStore(t)
	if err := store.UpsertOrgRoleArchived(context.Background(), db.OrgRoleArchived{
		Role: "alpha", ArchivedAt: "2026-08-31T00:00:00Z",
		ArchivedBy: "herdr-app", ObservedAt: "2026-08-31T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertOrgRoleArchived: %v", err)
	}
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	content := config.DefaultConfig(paths) + `
[org.roles."owner"]
scope = ["*"]
[org.roles."alpha"]
parent = "owner"
scope = ["owner/repo"]
[org.roles."coordinator"]
parent = "owner"
scope = ["owner/repo"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	from, declared := mergeGateEscalationFrom(loadOrgRoster(context.Background(), store, cfg), cfg, "owner/repo")
	if from != "coordinator" || !declared {
		t.Fatalf("mergeGateEscalationFrom = (%q, %t), want live coordinator instead of archived alpha", from, declared)
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
