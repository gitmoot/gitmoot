package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2008, the tenth consumer. reconcileReviewingPullRequest skips a review whose
// payload head does not equal the pull's head. For a HEADLESS row that skip is
// correct and must stay correct - a review naming no head cannot be the review
// "at" this head - but it was silent, on the one consumer that re-runs every
// poll tick. These tests pin what the row is told and, just as hard, what it is
// never told.

const daemonExclusionConsumer = "daemon.reconcileReviewingPullRequest"

func seedExclusionReviewJob(t *testing.T, store *db.Store, repo, id, headSHA string, externallyDriven bool) {
	t.Helper()
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo,
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     headSHA,
		TaskID:      "task-007",
		TaskTitle:   "Task 7",
		LeadAgent:   "lead",
		Reviewers:   []string{"audit"},
		ReviewRound: id,
	})
	if err != nil {
		t.Fatalf("Marshal review payload returned error: %v", err)
	}
	job := db.Job{
		ID:      id,
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobSucceeded),
		Payload: string(payload),
	}
	event := db.JobEvent{JobID: id, Kind: string(workflow.JobSucceeded), Message: "seeded review job"}
	if externallyDriven {
		err = store.CreateExternallyDrivenJobWithEvent(context.Background(), job, event)
	} else {
		err = store.CreateJobWithEvent(context.Background(), job, event)
	}
	if err != nil {
		t.Fatalf("create review job %q returned error: %v", id, err)
	}
}

func seedReviewingTask(t *testing.T, store *db.Store, repo github.Repository) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-007", RepoFullName: repo.FullName(), GoalID: "goal-1",
		Title: "Task 7", State: string(workflow.TaskReviewing), Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: 7, HeadBranch: "task-7",
		BaseBranch: "main", HeadSHA: "head-now", State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
}

func exclusionMessages(t *testing.T, store *db.Store, jobID string) []string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var out []string
	for _, event := range events {
		if event.Kind == workflow.HeadBoundExclusionEventKind {
			out = append(out, event.Message)
		}
	}
	return out
}

func storedHeadSHA(t *testing.T, store *db.Store, jobID string) string {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("Unmarshal payload returned error: %v", err)
	}
	return payload.HeadSHA
}

// The two headless classes must not collapse into one reason. Reporting every
// headless row as a self-rooted session row would be an over-attribution
// committed by the change meant to improve honesty.
func TestDaemonReconcileRecordsWhyItSkippedAHeadlessReview(t *testing.T) {
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	for _, tc := range []struct {
		name             string
		externallyDriven bool
		wantReason       string
	}{
		{"session row", true, workflow.HeadBoundExclusionSessionRow},
		{"ordinary headless row", false, workflow.HeadBoundExclusionNoHeadRecorded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			seedReviewingTask(t, store, repo)
			seedExclusionReviewJob(t, store, repo.FullName(), "review-headless", "", tc.externallyDriven)
			d := Daemon{Repo: repo, Store: store, Workflow: &workflow.Engine{Store: store}}

			if err := d.reconcileReviewingPullRequest(context.Background(), reviewPull("head-now"), nil); err != nil {
				t.Fatalf("reconcileReviewingPullRequest returned error: %v", err)
			}

			messages := exclusionMessages(t, store, "review-headless")
			if len(messages) != 1 {
				t.Fatalf("exclusion events = %d (%v), want exactly one", len(messages), messages)
			}
			if !strings.Contains(messages[0], tc.wantReason) {
				t.Fatalf("exclusion message = %q, want the %s reason %q", messages[0], tc.name, tc.wantReason)
			}
			if !strings.Contains(messages[0], daemonExclusionConsumer) {
				t.Fatalf("exclusion message = %q, want it to name the consumer %q", messages[0], daemonExclusionConsumer)
			}

			// The trap of this campaign: making the skip legible and making the
			// row head-bound look identical from a distance. Only one of them is
			// the fix.
			if head := storedHeadSHA(t, store, "review-headless"); head != "" {
				t.Fatalf("stored head = %q, want it still empty: recording why a row was excluded must never write a head", head)
			}
			task, err := store.GetTask(context.Background(), "task-007")
			if err != nil {
				t.Fatalf("GetTask returned error: %v", err)
			}
			if task.State != string(workflow.TaskReviewing) {
				t.Fatalf("task state = %q, want reviewing: a headless review still advances nothing", task.State)
			}
		})
	}
}

// A row at a DIFFERENT non-empty head is also skipped here, and must stay
// silent: it was excluded by the head comparison, not for want of a head. This
// is the column that a predicate loosened to "skipped for any reason" fills.
func TestDaemonReconcileStaysSilentForAReviewAtAnotherHead(t *testing.T) {
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	store := testStore(t)
	seedReviewingTask(t, store, repo)
	seedExclusionReviewJob(t, store, repo.FullName(), "review-other-head", "head-before", false)
	d := Daemon{Repo: repo, Store: store, Workflow: &workflow.Engine{Store: store}}

	if err := d.reconcileReviewingPullRequest(context.Background(), reviewPull("head-now"), nil); err != nil {
		t.Fatalf("reconcileReviewingPullRequest returned error: %v", err)
	}

	if messages := exclusionMessages(t, store, "review-other-head"); len(messages) != 0 {
		t.Fatalf("exclusion events = %v, want none: the row was excluded by the head comparison, not for want of a head", messages)
	}
}

// This consumer re-runs on every poll tick, which is what turned an earlier
// unconditional annotation into a million job_events rows. The message carries
// consumer and reason only so the at-most-once claim holds across ticks.
func TestDaemonReconcileExclusionDoesNotGrowPerPollTick(t *testing.T) {
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	store := testStore(t)
	seedReviewingTask(t, store, repo)
	seedExclusionReviewJob(t, store, repo.FullName(), "review-headless", "", true)
	d := Daemon{Repo: repo, Store: store, Workflow: &workflow.Engine{Store: store}}

	for tick := 0; tick < 4; tick++ {
		if err := d.reconcileReviewingPullRequest(context.Background(), reviewPull("head-now"), nil); err != nil {
			t.Fatalf("reconcileReviewingPullRequest tick %d returned error: %v", tick, err)
		}
	}

	if messages := exclusionMessages(t, store, "review-headless"); len(messages) != 1 {
		t.Fatalf("exclusion events after 4 poll ticks = %d (%v), want exactly one", len(messages), messages)
	}
}
