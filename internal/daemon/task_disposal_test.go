package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type taskDisposalGitHub struct {
	*fakeGitHub
	issues   map[int64]github.Issue
	issueErr error
}

func (f *taskDisposalGitHub) GetIssue(_ context.Context, _ github.Repository, number int64) (github.Issue, error) {
	if f.issueErr != nil {
		return github.Issue{}, f.issueErr
	}
	issue, ok := f.issues[number]
	if !ok {
		return github.Issue{}, fmt.Errorf("issue %d missing", number)
	}
	return issue, nil
}

func TestTaskDisposalEvidenceTiers(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(t *testing.T, store *db.Store, gh *taskDisposalGitHub, repo github.Repository)
		wantState  workflow.TaskState
		wantTier   string
		wantReason string
	}{
		{
			name: "tier1 own PR merged",
			configure: func(t *testing.T, store *db.Store, gh *taskDisposalGitHub, repo github.Repository) {
				seedTaskDisposalJob(t, store, "job-own", "task-under-test", repo.FullName(), "feature/own", 41, "Implement issue #1344")
				gh.pullsByNumber[41] = github.PullRequest{Number: 41, State: "closed", Merged: true, HeadRef: "feature/own"}
			},
			wantState: workflow.TaskMerged, wantTier: taskDisposalTierOwnPRMerged, wantReason: "own PR #41",
		},
		{
			name: "tier2 successor task merged",
			configure: func(t *testing.T, store *db.Store, _ *taskDisposalGitHub, repo github.Repository) {
				seedTaskDisposalJob(t, store, "job-original", "task-under-test", repo.FullName(), "feature/original", 0, "Implement issue #1344")
				if err := store.UpsertTask(context.Background(), db.Task{ID: "task-successor", RepoFullName: repo.FullName(), State: string(workflow.TaskMerged), Branch: "feature/successor"}); err != nil {
					t.Fatal(err)
				}
				seedTaskDisposalJob(t, store, "job-successor", "task-successor", repo.FullName(), "feature/successor", 52, "Fix issue #1344")
				setTaskUpdatedAt(t, store, "task-successor", time.Now().UTC().Add(time.Hour))
			},
			wantState: workflow.TaskSuperseded, wantTier: taskDisposalTierSuccessorMerged, wantReason: "successor task task-successor",
		},
		{
			name: "tier3 referenced subject closed",
			configure: func(t *testing.T, store *db.Store, gh *taskDisposalGitHub, repo github.Repository) {
				seedTaskDisposalJob(t, store, "job-subject", "task-under-test", repo.FullName(), "feature/subject", 0, "Implement issue #1344")
				gh.issues[1344] = github.Issue{Number: 1344, State: "closed"}
			},
			wantState: workflow.TaskSuperseded, wantTier: taskDisposalTierSubjectClosed, wantReason: "referenced subject owner/repo#1344",
		},
		{
			name: "tier4 bounded fallback",
			configure: func(t *testing.T, store *db.Store, gh *taskDisposalGitHub, repo github.Repository) {
				seedTaskDisposalJob(t, store, "job-no-evidence", "task-under-test", repo.FullName(), "feature/none", 0, "Investigate the scheduler")
			},
			wantState: workflow.TaskStranded, wantTier: taskDisposalTierStranded, wantReason: taskDisposalReasonNoEvidence,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			repo := github.Repository{Owner: "owner", Name: "repo"}
			seedStaleRepo(t, store, repo)
			if err := store.UpsertTask(ctx, db.Task{ID: "task-under-test", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked), Branch: "feature/original"}); err != nil {
				t.Fatal(err)
			}
			gh := &taskDisposalGitHub{fakeGitHub: &fakeGitHub{pullsByNumber: map[int64]github.PullRequest{}, comments: map[int64][]github.IssueComment{}}, issues: map[int64]github.Issue{}}
			test.configure(t, store, gh, repo)
			d := Daemon{Repo: repo, Store: store, GitHub: gh, Now: futureClock}
			if err := d.reconcileTaskDisposals(ctx, time.Hour); err != nil {
				t.Fatalf("reconcileTaskDisposals: %v", err)
			}
			got, err := store.GetTask(ctx, "task-under-test")
			if err != nil {
				t.Fatal(err)
			}
			if got.State != string(test.wantState) || got.DisposalTier != test.wantTier || !strings.Contains(got.DisposalReason, test.wantReason) || got.DisposedAt == "" {
				t.Fatalf("disposed task = %+v, want state=%s tier=%s reason containing %q", got, test.wantState, test.wantTier, test.wantReason)
			}
			if test.wantState == workflow.TaskStranded && !strings.Contains(got.DisposalReason, "escalation unroutable") {
				t.Fatalf("unroutable stranded task did not record routing fact: %+v", got)
			}
			events, err := store.ListTaskEvents(ctx, got.ID)
			if err != nil || len(events) != 1 || events[0].Kind != "task_disposed_"+test.wantTier {
				t.Fatalf("task events = %+v, err=%v", events, err)
			}
		})
	}
}

func TestTaskDisposalAwaitingHumanMergeOpenPRFallsToStranded(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	if err := store.UpsertTask(ctx, db.Task{ID: "protected", RepoFullName: repo.FullName(), State: string(workflow.TaskAwaitingHumanMerge), Branch: "feature/protected"}); err != nil {
		t.Fatal(err)
	}
	seedTaskDisposalJob(t, store, "job-protected", "protected", repo.FullName(), "feature/protected", 77, "Implement issue #1344")
	gh := &taskDisposalGitHub{fakeGitHub: &fakeGitHub{pullsByNumber: map[int64]github.PullRequest{77: {Number: 77, State: "open"}}}, issues: map[int64]github.Issue{1344: {Number: 1344, State: "closed"}}}
	if err := (Daemon{Repo: repo, Store: store, GitHub: gh, Now: futureClock}).reconcileTaskDisposals(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetTask(ctx, "protected")
	if got.State != string(workflow.TaskStranded) || !strings.Contains(got.DisposalReason, "awaiting_human_merge is protected") {
		t.Fatalf("protected task = %+v", got)
	}
}

func TestTaskDisposalClosedOwnPRDoesNotMasqueradeAsClosedIssue(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	if err := store.UpsertTask(ctx, db.Task{ID: "closed-pr-open-issue", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked), Branch: "feature/closed-pr"}); err != nil {
		t.Fatal(err)
	}
	seedTaskDisposalJob(t, store, "job-closed-pr", "closed-pr-open-issue", repo.FullName(), "feature/closed-pr", 78, "Implement issue #1344")
	gh := &taskDisposalGitHub{
		fakeGitHub: &fakeGitHub{pullsByNumber: map[int64]github.PullRequest{78: {Number: 78, State: "closed"}}},
		issues:     map[int64]github.Issue{1344: {Number: 1344, State: "open"}},
	}
	if err := (Daemon{Repo: repo, Store: store, GitHub: gh, Now: futureClock}).reconcileTaskDisposals(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetTask(ctx, "closed-pr-open-issue")
	if got.State != string(workflow.TaskStranded) || got.DisposalTier != taskDisposalTierStranded {
		t.Fatalf("closed own PR incorrectly substituted for open issue state: %+v", got)
	}
}

func TestTaskDisposalTier3UnresolvableSubjectsStrand(t *testing.T) {
	tests := []struct {
		name        string
		subjectRepo string
		issueErr    error
	}{
		{name: "invalid subject repository", subjectRepo: "not-a-repository"},
		{name: "issue lookup error", subjectRepo: "owner/repo", issueErr: errors.New("forced issue lookup failure")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			repo := github.Repository{Owner: "owner", Name: "repo"}
			seedStaleRepo(t, store, repo)
			candidate := db.StaleTaskCandidate{
				ID: "tier3-unresolvable", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked),
				UpdatedAt: time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05"),
			}
			if err := store.UpsertTask(ctx, db.Task{ID: candidate.ID, RepoFullName: repo.FullName(), State: candidate.State}); err != nil {
				t.Fatal(err)
			}
			gh := &taskDisposalGitHub{
				fakeGitHub: &fakeGitHub{}, issues: map[int64]github.Issue{1344: {Number: 1344, State: "open"}}, issueErr: test.issueErr,
			}
			d := Daemon{Repo: repo, Store: store, GitHub: gh, Now: futureClock}
			err := d.disposeTaskCandidate(ctx, candidate, nil, map[string]taskDisposalEvidence{
				candidate.ID: {subjectRepo: test.subjectRepo, subjectIssue: 1344},
			}, nil, nil, config.OrgConfig{})
			if err != nil {
				t.Fatalf("disposeTaskCandidate error = %v", err)
			}
			got, err := store.GetTask(ctx, candidate.ID)
			if err != nil || got.State != string(workflow.TaskStranded) ||
				got.DisposalTier != taskDisposalTierStranded ||
				!strings.Contains(got.DisposalReason, taskDisposalReasonUnavailable) {
				t.Fatalf("stranded task = %+v, err=%v", got, err)
			}
		})
	}
}

func TestTaskDisposalSuccessorTiming(t *testing.T) {
	blockedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		successorState   workflow.TaskState
		successorUpdated time.Time
		pull             github.PullRequest
		wantState        workflow.TaskState
		wantTier         string
	}{
		{
			name: "earlier merged task is not a successor", successorState: workflow.TaskMerged,
			successorUpdated: blockedAt.Add(-time.Hour), wantState: workflow.TaskStranded, wantTier: taskDisposalTierStranded,
		},
		{
			name: "earlier merged PR is not a successor", successorState: workflow.TaskImplementing,
			successorUpdated: blockedAt.Add(time.Hour),
			pull:             github.PullRequest{Number: 52, State: "closed", Merged: true, MergedAt: blockedAt.Add(-time.Hour).Format(time.RFC3339)},
			wantState:        workflow.TaskStranded, wantTier: taskDisposalTierStranded,
		},
		{
			name: "later merged PR supersedes", successorState: workflow.TaskImplementing,
			successorUpdated: blockedAt.Add(time.Hour),
			pull:             github.PullRequest{Number: 52, State: "closed", Merged: true, MergedAt: blockedAt.Add(2 * time.Hour).Format(time.RFC3339)},
			wantState:        workflow.TaskSuperseded, wantTier: taskDisposalTierSuccessorMerged,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			repo := github.Repository{Owner: "owner", Name: "repo"}
			seedStaleRepo(t, store, repo)
			candidate := db.StaleTaskCandidate{
				ID: "original", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked),
				UpdatedAt: blockedAt.Format("2006-01-02 15:04:05"),
			}
			if err := store.UpsertTask(ctx, db.Task{ID: candidate.ID, RepoFullName: repo.FullName(), State: candidate.State}); err != nil {
				t.Fatal(err)
			}
			successor := db.Task{
				ID: "successor", RepoFullName: repo.FullName(), State: string(test.successorState),
				UpdatedAt: test.successorUpdated.Format("2006-01-02 15:04:05"),
			}
			pulls := map[int64]github.PullRequest{}
			peerEvidence := taskDisposalEvidence{subjectRepo: repo.FullName(), subjectIssue: 1344}
			if test.pull.Number > 0 {
				pulls[test.pull.Number] = test.pull
				peerEvidence.pullRequest = test.pull.Number
			}
			gh := &taskDisposalGitHub{
				fakeGitHub: &fakeGitHub{pullsByNumber: pulls},
				issues:     map[int64]github.Issue{1344: {Number: 1344, State: "open"}},
			}
			d := Daemon{Repo: repo, Store: store, GitHub: gh, Now: futureClock}
			err := d.disposeTaskCandidate(ctx, candidate, []db.Task{successor}, map[string]taskDisposalEvidence{
				candidate.ID: {subjectRepo: repo.FullName(), subjectIssue: 1344},
				successor.ID: peerEvidence,
			}, nil, nil, config.OrgConfig{})
			if err != nil {
				t.Fatalf("disposeTaskCandidate error = %v", err)
			}
			got, err := store.GetTask(ctx, candidate.ID)
			if err != nil || got.State != string(test.wantState) || got.DisposalTier != test.wantTier {
				t.Fatalf("disposed task = %+v, err=%v; want state=%s tier=%s", got, err, test.wantState, test.wantTier)
			}
		})
	}
}

func TestTaskDisposalUnresolvableTerminatesAndPassContinues(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	for _, task := range []db.Task{
		{ID: "task-unavailable", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked), Branch: "feature/unavailable"},
		{ID: "task-later", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked), Branch: "feature/later"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	seedTaskDisposalJob(t, store, "job-unavailable", "task-unavailable", repo.FullName(), "feature/unavailable", 90, "Implement issue #1400")
	seedTaskDisposalJob(t, store, "job-later", "task-later", repo.FullName(), "feature/later", 0, "Implement issue #1401")
	gh := &taskDisposalGitHub{fakeGitHub: &fakeGitHub{pullsByNumber: map[int64]github.PullRequest{}}, issues: map[int64]github.Issue{1401: {Number: 1401, State: "closed"}}}
	if err := (Daemon{Repo: repo, Store: store, GitHub: gh, Now: futureClock}).reconcileTaskDisposals(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	stranded, err := store.ListTasksByRepoState(ctx, repo.FullName(), string(workflow.TaskStranded))
	if err != nil || len(stranded) != 1 || stranded[0].ID != "task-unavailable" || !strings.Contains(stranded[0].DisposalReason, taskDisposalReasonUnavailable) {
		t.Fatalf("stranded query = %+v, err=%v", stranded, err)
	}
	blocked, err := store.ListTasksByRepoState(ctx, repo.FullName(), string(workflow.TaskBlocked))
	if err != nil || len(blocked) != 0 {
		t.Fatalf("blocked query after disposal = %+v, err=%v", blocked, err)
	}
	later, _ := store.GetTask(ctx, "task-later")
	if later.State != string(workflow.TaskSuperseded) {
		t.Fatalf("later task was starved by prior failure: %+v", later)
	}
}

func TestTaskDisposalCandidateErrorDoesNotAbortPass(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	for _, task := range []db.Task{
		{ID: "a-error", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked)},
		{ID: "b-later", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked)},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	setTaskUpdatedAt(t, store, "a-error", time.Now().UTC().Add(-3*time.Hour))
	setTaskUpdatedAt(t, store, "b-later", time.Now().UTC().Add(-2*time.Hour))
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_first_task_disposal_event
		BEFORE INSERT ON task_events
		WHEN NEW.task_id = 'a-error'
		BEGIN
			SELECT RAISE(ABORT, 'forced candidate disposal failure');
		END;`); err != nil {
		t.Fatal(err)
	}
	gh := &taskDisposalGitHub{fakeGitHub: &fakeGitHub{}, issues: map[int64]github.Issue{}}
	err = (Daemon{Repo: repo, Store: store, GitHub: gh, Now: futureClock}).reconcileTaskDisposals(ctx, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "forced candidate disposal failure") {
		t.Fatalf("reconcileTaskDisposals error = %v", err)
	}
	failed, err := store.GetTask(ctx, "a-error")
	if err != nil || failed.State != string(workflow.TaskBlocked) {
		t.Fatalf("failed candidate = %+v, err=%v", failed, err)
	}
	later, err := store.GetTask(ctx, "b-later")
	if err != nil || later.State != string(workflow.TaskStranded) || later.DisposalTier != taskDisposalTierStranded {
		t.Fatalf("later candidate = %+v, err=%v; pass aborted after first error", later, err)
	}
}

func TestTaskDisposalStrandedEscalationIsOneShotAndRoutingIndependent(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	paths := config.Paths{Home: strings.TrimSuffix(store.DatabasePath(), "/gitmoot.db"), ConfigFile: taskDisposalConfigPath(store)}
	// The task was owned by lane, but that seat has left the chart. Disposal must
	// still terminate and route to the live repo root instead of retrying forever.
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-routed", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked), Branch: "feature/routed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "feature/routed", Owner: "worker", ActingOrgRole: "lane"}); err != nil {
		t.Fatal(err)
	}
	gh := &taskDisposalGitHub{fakeGitHub: &fakeGitHub{}, issues: map[int64]github.Issue{}}
	d := Daemon{Repo: repo, Store: store, GitHub: gh, Now: futureClock}
	for i := 0; i < 3; i++ {
		if err := d.reconcileTaskDisposals(ctx, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := store.GetTask(ctx, "task-routed")
	if got.State != string(workflow.TaskStranded) || got.DisposalEscalationRole != "owner" {
		t.Fatalf("routed stranded task = %+v", got)
	}
	stranded, err := store.ListTasksByRepoState(ctx, repo.FullName(), string(workflow.TaskStranded))
	if err != nil || len(stranded) != 1 || stranded[0].ID != "task-routed" {
		t.Fatalf("stranded query after three sweeps = %+v, err=%v", stranded, err)
	}
	blocked, err := store.ListTasksByRepoState(ctx, repo.FullName(), string(workflow.TaskBlocked))
	if err != nil || len(blocked) != 0 {
		t.Fatalf("blocked query after three sweeps = %+v, err=%v", blocked, err)
	}
	outbox, err := store.ListWakeOutbox(ctx, "")
	if err != nil || len(outbox) != 1 || outbox[0].TargetRole != "owner" || outbox[0].SourceKind != db.WakeOutboxSourceEscalation {
		t.Fatalf("terminal escalation outbox = %+v, err=%v", outbox, err)
	}
}

func TestPollOnceTaskDisposalTerminatesBeforeForgeListFailure(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	writeStaleTaskConfig(t, store, "1h")
	if err := store.UpsertTask(ctx, db.Task{ID: "past-ttl", RepoFullName: repo.FullName(), State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	client := &taskDisposalGitHub{
		fakeGitHub: &fakeGitHub{listPullRequestsErrs: []error{errors.New("forge unavailable")}},
		issues:     map[int64]github.Issue{},
	}
	err := (Daemon{Repo: repo, Store: store, GitHub: client, Now: futureClock}).PollOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "forge unavailable") {
		t.Fatalf("PollOnce error = %v", err)
	}
	stranded, queryErr := store.ListTasksByRepoState(ctx, repo.FullName(), string(workflow.TaskStranded))
	if queryErr != nil || len(stranded) != 1 || stranded[0].ID != "past-ttl" ||
		!strings.Contains(stranded[0].DisposalReason, taskDisposalReasonNoEvidence) {
		t.Fatalf("stranded query after forge failure = %+v, err=%v", stranded, queryErr)
	}
	blocked, queryErr := store.ListTasksByRepoState(ctx, repo.FullName(), string(workflow.TaskBlocked))
	if queryErr != nil || len(blocked) != 0 {
		t.Fatalf("blocked query after forge failure = %+v, err=%v", blocked, queryErr)
	}
}

func seedTaskDisposalJob(t *testing.T, store *db.Store, id, taskID, repo, branch string, pullRequest int, instructions string) {
	t.Helper()
	payload, err := json.Marshal(workflow.JobPayload{TaskID: taskID, Repo: repo, Branch: branch, PullRequest: pullRequest, Instructions: instructions})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(context.Background(), db.Job{ID: id, Type: "implement", State: string(workflow.JobSucceeded), Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
}

func setTaskUpdatedAt(t *testing.T, store *db.Store, taskID string, at time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, at.UTC().Format("2006-01-02 15:04:05"), taskID); err != nil {
		t.Fatal(err)
	}
}
