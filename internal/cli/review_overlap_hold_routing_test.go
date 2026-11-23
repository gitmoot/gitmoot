package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// With nothing running nothing holds a review (#2726): an earlier pinned-local
// review B of an older head, which cannot take a host slot, leaves the
// current-head E2B review C free to take an open remote slot.
func TestReviewHoldNeverLetsAQueuedReviewHoldAnother(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, false, 4)
	queueDiskGuardJob(t, ctx, store, "review-b", "rev-omp", "review", `,"head_sha":"`+overlapHeadB+`","exec_backend":"local"`)
	queueDiskGuardJob(t, ctx, store, "review-c", "rev-omp", "review", `,"head_sha":"`+overlapHeadC+`","exec_backend":"remote"`)

	pending, err := listPendingQueuedJobs(ctx, worker, "owner/repo", "", true)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, job := range pending {
		listed = append(listed, job.ID)
	}
	if !slices.Equal(listed, []string{"review-b", "review-c"}) {
		t.Fatalf("listed %v with nothing running, want review-b and review-c", listed)
	}
	queued, _ := selectRunnableByLane(ctx, store, pending, 0, 2, true, nil, nil)
	if len(queued) != 1 || queued[0].ID != "review-c" {
		t.Fatalf("zero host slots and two E2B slots selected %v, want review-c", queued)
	}
	if events, err := store.ListJobEvents(ctx, "review-c"); err != nil {
		t.Fatal(err)
	} else {
		for _, event := range events {
			if event.Kind == reviewHeldForRunningReviewEventKind {
				t.Fatalf("review-c carries a hold event with nothing running: %+v", event)
			}
		}
	}
}

// A held review is never routed to remote (#2726): with a routing cap of one,
// local review A running and B of A's pull request held, the disk guard's
// warning band routes C, a review of another pull request, and leaves B as it
// was.
func TestReviewHoldKeepsAHeldReviewOutOfRemoteRouting(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 1)
	queueDiskGuardJob(t, ctx, store, "review-a", "rev-omp", "review", `,"head_sha":"`+overlapHeadA+`","exec_backend":"local"`)
	if claimed, err := store.ClaimRunningJob(ctx, "review-a", string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 1, "boot"); err != nil || !claimed {
		t.Fatalf("claim review-a: claimed=%v err=%v", claimed, err)
	}
	queueDiskGuardJob(t, ctx, store, "review-b", "rev-omp", "review", `,"head_sha":"`+overlapHeadB+`"`)
	queueDiskGuardJob(t, ctx, store, "review-c", "rev-omp", "review", `,"pull_request":8,"head_sha":"`+overlapHeadC+`"`)

	for range 2 {
		if got := strings.Join(pendingIDs(t, ctx, worker), ","); got != "review-c" {
			t.Fatalf("warning-band pending = %q, want review-c: held review-b must not take the one routing slot", got)
		}
	}
	if b := storedBackend(t, ctx, store, "review-b"); b != "" {
		t.Fatalf("held review-b backend = %q, want unset: a held review is not routed", b)
	}
	if c := storedBackend(t, ctx, store, "review-c"); c != "remote" {
		t.Fatalf("review-c backend = %q, want remote", c)
	}
}

// The routing cap compares heads as the hold does (#2737): with local review A
// running and X, a review of A's pull request already routed at A's head with
// whitespace padding, the hold leaves X eligible, so X keeps the one routing
// slot and C, a review of another pull request, is not routed beside it.
func TestReviewHoldRoutingCapComparesHeadsAsTheHoldDoes(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 1)
	for _, job := range []struct{ id, payload string }{
		{"review-a", `{"repo":"owner/repo","pull_request":7,"head_sha":"` + overlapHeadA + `","exec_backend":"local"}`},
		{"review-x", `{"repo":"owner/repo","pull_request":7,"head_sha":"\t` + overlapHeadA + `\n","exec_backend":"remote"}`},
		{"review-c", `{"repo":"owner/repo","pull_request":8,"head_sha":"` + overlapHeadC + `"}`},
	} {
		if err := store.CreateJobWithEvent(ctx, db.Job{ID: job.id, Agent: "rev-omp", Type: "review", State: string(workflow.JobQueued), Payload: job.payload},
			db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent %s: %v", job.id, err)
		}
		if job.id == "review-a" {
			if claimed, err := store.ClaimRunningJob(ctx, "review-a", string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 1, "boot"); err != nil || !claimed {
				t.Fatalf("claim review-a: claimed=%v err=%v", claimed, err)
			}
		}
	}

	if got := strings.Join(pendingIDs(t, ctx, worker), ","); got != "review-x" {
		t.Fatalf("warning-band pending = %q, want review-x: the hold leaves it eligible, so it keeps the one routing slot", got)
	}
	if c := storedBackend(t, ctx, store, "review-c"); c != "" {
		t.Fatalf("review-c backend = %q, want unset: review-x holds the routing slot", c)
	}
}
