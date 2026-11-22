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
	queued, _ := selectRunnableByLane(ctx, store, pending, 0, 2, true, nil, nil, nil)
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
