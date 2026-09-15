package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
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

// #2186 round 5. Inheriting the engine's mailbox must not inherit its DELIVERY
// capability. This producer is constructed with UnavailableDeliveryWorktreeResolver,
// whose whole purpose is to fail loudly if an implement delivery ever reaches
// this context; taking the engine's mailbox wholesale would have replaced that
// sentinel with a real resolver and its change-set machinery, turning a context
// that refuses delivery into one equipped to perform it.
func TestCommentDispatchKeepsItsDeliveryRefusal(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	delivered := false

	daemon := Daemon{
		Store: store,
		Workflow: &workflow.Engine{
			Store:           store,
			ReviewModelPool: func(string) []string { return []string{"sentinel/daemon-a"} },
			// A REAL resolver on the engine: if the producer inherited it, the
			// refusal would be gone and this flag reachable.
			ResolveDeliveryWorktree: func(context.Context, db.Job, workflow.JobPayload) (workflow.DeliveryWorktreeResolution, error) {
				delivered = true
				return workflow.DeliveryWorktreeResolution{}, nil
			},
			ApplyChangeSet: func(context.Context, string, execbackend.ChangeSet) error { return nil },
		},
	}

	job, _, err := daemon.enqueueJob(ctx, workflow.JobRequest{
		ID: "job-delivery-refusal", Agent: "reviewer", Action: "review",
		Repo: "owner/repo", Branch: "main", PullRequest: 5,
		HeadSHA: strings.Repeat("c", 40), ReviewPurpose: "code",
	})
	if err != nil {
		t.Fatalf("enqueueJob: %v", err)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	// Config-bearing resolvers ARE inherited; the capability sentinel is not.
	if len(payload.ReviewModelPool) == 0 {
		t.Fatal("the pool resolver was dropped along with the delivery machinery")
	}
	if delivered {
		t.Fatal("the comment producer inherited the engine's delivery resolver: its loud refusal is gone")
	}
}

// R5-M5: the daemon must pass its sentinel, not nil. Asserted on the mailbox
// itself because the delivery resolver is never invoked during enqueue - a test
// that only watches for a delivery call cannot tell the two apart.
func TestCommentEnqueueMailboxInheritsConfigButNotDeliveryMachinery(t *testing.T) {
	store := testStore(t)
	daemon := Daemon{
		Store: store,
		Workflow: &workflow.Engine{
			Store:                   store,
			ReviewModelPool:         func(string) []string { return []string{"sentinel/daemon-a"} },
			ResolveDeliveryWorktree: workflow.PayloadDeliveryWorktreeResolver,
			ApplyChangeSet:          func(context.Context, string, execbackend.ChangeSet) error { return nil },
			CollectChangeSet: func(context.Context, execbackend.Backend, string) (*execbackend.ChangeSet, error) {
				return nil, nil
			},
		},
	}
	mailbox := daemon.commentEnqueueMailbox()
	if mailbox.ReviewModelPool == nil {
		t.Fatal("config-bearing resolver not inherited")
	}
	if mailbox.ApplyChangeSet != nil || mailbox.CollectChangeSet != nil {
		t.Fatal("delivery machinery inherited: this producer refuses implement delivery and must not carry the means to perform it")
	}
}
