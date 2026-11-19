package cli

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestBlockedTaskAlertGoesToTheTasksOwnRole reproduces #2668: "lane" (child of
// "owner", scoped to exactly the repo) is the repo's most specific owner, so
// every blocked-task digest used to wake lane, even for tasks owner requested.
// A task's own attribution now wins; an unattributed task still reaches the
// repo owner, and tasks of different roles get separate digests.
func TestBlockedTaskAlertGoesToTheTasksOwnRole(t *testing.T) {
	const repo = "gitmoot/gitmoot"
	tests := []struct {
		name      string
		requester string
		acting    string
		unowned   bool
		wantRoles []string
	}{
		{name: "review requester", requester: "owner", wantRoles: []string{"jarvis", "owner"}},
		{name: "acting role of the task job", acting: "owner", wantRoles: []string{"jarvis", "owner"}},
		{name: "unattributed task reaches the repo owner", wantRoles: []string{"jarvis", "lane"}},
		{name: "mixed digest splits per role", requester: "owner", unowned: true, wantRoles: []string{"jarvis", "jarvis", "lane", "owner"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, store := blockedTaskRoutingFixture(t)
			ctx := context.Background()

			taskID := "review-pr-7-0a1b2c3d"
			if err := store.UpsertTask(ctx, db.Task{ID: taskID, RepoFullName: repo, GoalID: "local-review", State: string(workflow.TaskBlocked)}); err != nil {
				t.Fatal(err)
			}
			if test.requester != "" || test.acting != "" {
				if err := store.CreateJob(ctx, db.Job{
					ID: "review-job", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded),
					Payload: mustJobPayload(t, workflow.JobPayload{Repo: repo, PullRequest: 7, TaskID: taskID, ActingOrgRole: test.acting}),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.requester != "" {
				key, err := db.ReviewRequestSubjectKey(repo, 7, "0123456789abcdef0123456789abcdef01234567", "code")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.ClaimReviewRequest(ctx, key, "review-job", "code", test.requester, db.ReviewRequestOwner{}); err != nil {
					t.Fatal(err)
				}
			}
			if test.unowned {
				if err := store.UpsertTask(ctx, db.Task{ID: "task-unowned", RepoFullName: repo, State: string(workflow.TaskBlocked)}); err != nil {
					t.Fatal(err)
				}
			}

			sink := &eventRuleSink{store: store, home: home}
			now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
			if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, now); err != nil {
				t.Fatalf("evaluateBlockedTaskEpisodes: %v", err)
			}
			if got := pendingWakeTargetRoles(t, store); !reflect.DeepEqual(got, test.wantRoles) {
				t.Fatalf("blocked task wake targets = %v, want %v", got, test.wantRoles)
			}
		})
	}
}

// blockedTaskRoutingFixture is an org where "lane" (child of "owner", scoped to
// exactly gitmoot/gitmoot) is that repo's most specific owner, "jarvis"
// observes, and "retired" has an addressed rule but no seat.
func blockedTaskRoutingFixture(t *testing.T) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	const orgConfig = `
[org.roles."owner"]
scope=["gitmoot/*", "oversight/*"]
pane="w1:p1"
[org.roles."lane"]
parent="owner"
scope=["gitmoot/gitmoot"]
pane="w1:p2"
[org.roles."jarvis"]
parent="owner"
scope=["oversight/*"]
pane="w1:p4"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(orgConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, rule := range []db.EventRule{
		{ID: "addressed-lane", OnKind: "blocked", WakeRole: "lane", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "addressed-owner", OnKind: "blocked", WakeRole: "owner", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "addressed-retired", OnKind: "blocked", WakeRole: "retired", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "observer-jarvis", OnKind: "blocked", WakeRole: "jarvis", Scope: db.EventRuleScopeObserver, Enabled: true},
	} {
		if err := store.AddEventRule(context.Background(), rule); err != nil {
			t.Fatal(err)
		}
	}
	return home, store
}

// TestClosedUnmergedReviewTaskIsRetiredAndRebindsOnReopen covers #2668: the
// closed PR's review task is dismissed, so the blocked-since lane has nothing to
// alert about, and a review requested again after a reopen binds to a successor.
func TestClosedUnmergedReviewTaskIsRetiredAndRebindsOnReopen(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	const repo = "owner/repo"
	const taskID = "review-pr-12-0a1b2c3d"
	if err := store.UpsertTask(ctx, db.Task{ID: taskID, RepoFullName: repo, GoalID: "local-review",
		Title: "Review PR #12", State: string(workflow.TaskPullRequestOpen), Branch: "feature/review"}); err != nil {
		t.Fatal(err)
	}
	if err := (workflow.Engine{Store: store}).HandleReviewPullRequestClosed(ctx, workflow.PullRequestEvent{
		Repo: repo, Branch: "feature/review", PullRequest: 12, TaskID: taskID, LeadAgent: "github", Sender: "github",
	}, false); err != nil {
		t.Fatalf("HandleReviewPullRequestClosed: %v", err)
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil || task.State != string(workflow.TaskDismissed) {
		t.Fatalf("review task = %+v, err=%v; want dismissed", task, err)
	}
	sink := &recordingSink{}
	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes: %v", err)
	}
	if blocked := sink.byType(events.EventJobBlocked); len(blocked) != 0 {
		t.Fatalf("blocked-since alerts for a closed PR's review = %+v", blocked)
	}

	request, err := prepareLocalReviewTask(ctx, store, github.Repository{Owner: "owner", Name: "repo"}, localAgentDispatchRequest{
		PullRequest: 12, Branch: "feature/review", HeadSHA: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("review after reopen: %v", err)
	}
	successor, err := store.GetTask(ctx, request.TaskID)
	if err != nil || request.TaskID == taskID || db.TaskSuccessorRoot(request.TaskID) != taskID ||
		successor.State != string(workflow.TaskReviewing) || successor.Branch != "feature/review" {
		t.Fatalf("review after reopen bound to %+v, err=%v", successor, err)
	}
}

// TestBlockedTaskAlertRoleIsValidatedNotRederived covers #2668's routing
// contract: the sink honours a blocked-since digest's producer-resolved role
// only while it is on the live roster, otherwise the repo's coordinator gets it.
func TestBlockedTaskAlertRoleIsValidatedNotRederived(t *testing.T) {
	home, store := blockedTaskRoutingFixture(t)
	ctx := context.Background()
	sink := &eventRuleSink{store: store, home: home}
	sink.Emit(ctx, events.Event{Type: events.EventJobBlocked, Cause: "blocked_since", JobID: "task-1",
		Repo: "gitmoot/gitmoot", WakeTargetRole: "retired"})
	// retired is gone, so its alert goes to the coordinator (#2685), not lane.
	if got, want := pendingWakeTargetRoles(t, store), []string{"jarvis", "owner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocked task wake targets = %v, want %v", got, want)
	}
}

// TestBlockedTaskAlertAttributionErrorGoesToRepoOwner is a guard: a task whose
// attribution cannot be read is reported and alerted to the repo owner, never
// dropped.
func TestBlockedTaskAlertAttributionErrorGoesToRepoOwner(t *testing.T) {
	home, store := blockedTaskRoutingFixture(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "gitmoot/gitmoot", State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `ALTER TABLE review_requests RENAME TO review_requests_unreadable`); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	sink := &eventRuleSink{store: store, home: home}
	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, "gitmoot/gitmoot", time.Hour, &stdout, time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes: %v", err)
	}
	if !strings.Contains(stdout.String(), "task-1") {
		t.Fatalf("attribution failure not reported: %q", stdout.String())
	}
	if got, want := pendingWakeTargetRoles(t, store), []string{"jarvis", "lane"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocked task wake targets = %v, want %v", got, want)
	}
}
