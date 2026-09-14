package workflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2180. The pool is resolved at the enqueue chokepoint every producer passes,
// not at each dispatch site. Per-site arming is the defect: `gitmoot review
// request` armed the pool at its own construction site and every other producer
// - `gitmoot agent review` and the engine's own review legs - enqueued with an
// EMPTY pool, which is indistinguishable from a pool with no alternatives and
// therefore made the provider fallback unreachable for them.
type testStoreHandle struct{ store *db.Store }

func (h *testStoreHandle) payload(t *testing.T, prepared PreparedEnqueue) JobPayload {
	t.Helper()
	payload, err := ParseJobPayload(prepared.Job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	return payload
}

func TestPrepareEnqueueResolvesAReviewPoolAndLeavesNonReviewJobsPoolLess(t *testing.T) {
	configured := []string{"devin/swe-2", "openai-codex/gpt-5.6-sol"}
	var askedFor []string

	newMailbox := func(t *testing.T) (Mailbox, *testStoreHandle) {
		t.Helper()
		store := openEngineStore(t)
		seedAgent(t, store, "reviewer", []string{"review", "implement"}, "owner/repo")
		mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))
		mailbox.ReviewModelPool = func(purpose string) []string {
			askedFor = append(askedFor, purpose)
			return configured
		}
		return mailbox, &testStoreHandle{store: store}
	}

	t.Run("a review is armed with the configured pool for its purpose", func(t *testing.T) {
		askedFor = nil
		mailbox, h := newMailbox(t)
		prepared, err := mailbox.PrepareEnqueue(context.Background(), JobRequest{
			ID: "job-review", Agent: "reviewer", Action: "review", Repo: "owner/repo",
			Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40),
			ReviewPurpose: "security",
		})
		if err != nil {
			t.Fatalf("PrepareEnqueue: %v", err)
		}
		payload := h.payload(t, prepared)
		if !slices.Equal(payload.ReviewModelPool, configured) {
			t.Fatalf("pool = %v, want %v", payload.ReviewModelPool, configured)
		}
		if !slices.Equal(askedFor, []string{"security"}) {
			t.Fatalf("resolver asked for %v, want the request's own purpose", askedFor)
		}
		// The pool arms the fallback; it must not choose a model for an agent
		// the operator named (#2186 review, F1).
		if payload.Model != "" {
			t.Fatalf("model = %q, want the agent's own", payload.Model)
		}
	})

	t.Run("a non-review job stays pool-less", func(t *testing.T) {
		askedFor = nil
		mailbox, h := newMailbox(t)
		prepared, err := mailbox.PrepareEnqueue(context.Background(), JobRequest{
			ID: "job-implement", Agent: "reviewer", Action: "implement", Repo: "owner/repo",
			Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40),
		})
		if err != nil {
			t.Fatalf("PrepareEnqueue: %v", err)
		}
		if pool := h.payload(t, prepared).ReviewModelPool; len(pool) != 0 {
			t.Fatalf("implement job acquired pool %v: the claim walk and the blocker both read this field", pool)
		}
		if len(askedFor) != 0 {
			t.Fatalf("resolver consulted for a non-review job: %v", askedFor)
		}
	})

	t.Run("a caller-supplied pool is preserved verbatim", func(t *testing.T) {
		askedFor = nil
		mailbox, h := newMailbox(t)
		chosen := []string{"kimi-code/k3"}
		prepared, err := mailbox.PrepareEnqueue(context.Background(), JobRequest{
			ID: "job-chosen", Agent: "reviewer", Action: "review", Repo: "owner/repo",
			Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40),
			ReviewPurpose: "code", ReviewModelPool: chosen,
		})
		if err != nil {
			t.Fatalf("PrepareEnqueue: %v", err)
		}
		if pool := h.payload(t, prepared).ReviewModelPool; !slices.Equal(pool, chosen) {
			t.Fatalf("pool = %v, want the caller's %v", pool, chosen)
		}
		if len(askedFor) != 0 {
			t.Fatalf("resolver consulted despite a caller pool: %v", askedFor)
		}
	})

	t.Run("an unstated purpose resolves the code chain", func(t *testing.T) {
		askedFor = nil
		mailbox, _ := newMailbox(t)
		if _, err := mailbox.PrepareEnqueue(context.Background(), JobRequest{
			ID: "job-nopurpose", Agent: "reviewer", Action: "review", Repo: "owner/repo",
			Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40),
		}); err != nil {
			t.Fatalf("PrepareEnqueue: %v", err)
		}
		if !slices.Equal(askedFor, []string{"code"}) {
			t.Fatalf("resolver asked for %v, want the code default", askedFor)
		}
	})
}

// The engine half. Engine-produced review legs - native fanout, follow-up
// rounds, delegated reviews - never touch the CLI, so a resolver wired only
// into CLI dispatch leaves the fallback unreachable for every review the daemon
// produces on its own. Asserted through a REAL enqueue rather than by reading
// the field back, so the copy at engine_types.go and the resolution at the
// chokepoint are proved together.
func TestEngineProducedReviewsCarryTheResolvedPool(t *testing.T) {
	store := openEngineStore(t)
	seedAgent(t, store, "engine-reviewer", []string{"review"}, "owner/repo")
	configured := []string{"devin/swe-2", "openai-codex/gpt-5.6-sol"}
	engine := Engine{
		Store:                   store,
		ResolveDeliveryWorktree: UnavailableDeliveryWorktreeResolver("test"),
		ReviewModelPool:         func(string) []string { return configured },
	}

	prepared, err := engine.mailbox().PrepareEnqueue(context.Background(), JobRequest{
		ID: "job-engine-review", Agent: "engine-reviewer", Action: "review", Repo: "owner/repo",
		Branch: "main", PullRequest: 7, HeadSHA: strings.Repeat("c", 40), ReviewPurpose: "code",
	})
	if err != nil {
		t.Fatalf("PrepareEnqueue: %v", err)
	}
	payload, err := ParseJobPayload(prepared.Job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(payload.ReviewModelPool, configured) {
		t.Fatalf("engine review pool = %v, want %v: the resolver does not reach engine-produced reviews", payload.ReviewModelPool, configured)
	}
}
