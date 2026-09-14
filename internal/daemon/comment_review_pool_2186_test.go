package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2186 round 4 (P2). The PR-comment producer - `/gitmoot @agent review` - used
// to build its own Mailbox and forward three engine fields BY NAME.
// ReviewModelPool was the fourth and was not on the list, so the daemon held a
// home-aware resolver and discarded it, falling through to a resolver reading
// the DEFAULT config home.
//
// The failure was silent rather than empty, which is what made it worse than
// the bug it replaced: the built-in router settings seed a pool, so the job
// carried a plausible-but-WRONG pool and no review_pool_unresolved advisory.
//
// The sentinel pool below can only come from the engine's own resolver: no
// config default can produce it. That is the fixture rule this PR adopted after
// two equality-by-coincidence survivors - a pool equal to the default cannot
// tell "forwarded" from "fell back".
func TestCommentDispatchedReviewUsesTheEnginesOwnResolver(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	sentinel := []string{"sentinel/daemon-a", "sentinel/daemon-b"}

	daemon := Daemon{
		Store: store,
		Workflow: &workflow.Engine{
			Store:                   store,
			ResolveDeliveryWorktree: workflow.UnavailableDeliveryWorktreeResolver("test"),
			ReviewModelPool:         func(string) []string { return sentinel },
		},
	}

	job, created, err := daemon.enqueueJob(ctx, workflow.JobRequest{
		ID: "job-comment-review", Agent: "reviewer", Action: "review",
		Repo: "owner/repo", Branch: "main", PullRequest: 3,
		HeadSHA: strings.Repeat("a", 40), ReviewPurpose: "code",
	})
	if err != nil {
		t.Fatalf("enqueueJob: %v", err)
	}
	if !created {
		t.Fatal("enqueueJob reported no new job")
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !slices.Equal(payload.ReviewModelPool, sentinel) {
		t.Fatalf("comment-dispatched review pool = %v, want the engine's own %v: this producer resolved against the wrong home",
			payload.ReviewModelPool, sentinel)
	}
}

// A daemon with NO engine wired must still enqueue. It gets no pool, and the
// advisory is what makes that visible rather than silent.
func TestCommentDispatchWithoutAnEngineAnnouncesTheMissingPool(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	daemon := Daemon{Store: store}

	job, _, err := daemon.enqueueJob(ctx, workflow.JobRequest{
		ID: "job-no-engine", Agent: "reviewer", Action: "review",
		Repo: "owner/repo", Branch: "main", PullRequest: 4,
		HeadSHA: strings.Repeat("b", 40), ReviewPurpose: "code",
	})
	if err != nil {
		t.Fatalf("enqueueJob: %v", err)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.ReviewModelPool) != 0 {
		t.Fatalf("engine-less daemon invented a pool %v", payload.ReviewModelPool)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(e db.JobEvent) bool { return e.Kind == "review_pool_unresolved" }) {
		t.Fatal("no review_pool_unresolved advisory: a pool-less review is silently without a fallback")
	}
}
