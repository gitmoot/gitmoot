package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestReadOnlyFanoutPinsExactHeadOnlyForReviewChildren pins the ACTION scoping of
// exact-head isolation in the read-only fan-out branch of
// allocateAndEnqueueDelegation.
//
// The coordinator carries a head SHA, and its fan-out mixes an `ask` child with a
// `review` child. Keyed on the head alone, BOTH got a detached worktree created
// at that SHA and kept the inherited HeadSHA in their payload, even though the
// code's own comment scoped that to reviews. An `ask` is a question about the
// branch as it stands, and the CLI dispatch path already draws exactly this line
// (internal/cli/agent_dispatch.go clears the inherited HeadSHA whenever
// request.Action != "review"), so the ask child must run off the branch tip and
// carry no head binding, while the review child keeps both.
func TestReadOnlyFanoutPinsExactHeadOnlyForReviewChildren(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "asker", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "auditor", []string{"review"}, "gitmoot/gitmoot")

	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = t.TempDir()
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-005",
		PullRequest: 7,
		HeadSHA:     "head-two",
		TaskID:      "task-5",
		TaskTitle:   "Parent",
		ReviewRound: "review-1",
		Reviewers:   []string{"auditor"},
		Sender:      "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d-ask", Agent: "asker", Action: "ask", Prompt: "what does this module do"},
				{ID: "d-review", Agent: "auditor", Action: "review", Prompt: "review the diff"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	// Both siblings are read-only, so both are isolated; only their base ref and
	// head binding differ.
	if len(manager.detachedCalls) != 2 {
		t.Fatalf("detached worktree calls = %+v, want two", manager.detachedCalls)
	}
	baseFor := func(path string) string {
		t.Helper()
		for _, call := range manager.detachedCalls {
			if call.path == path {
				return call.base
			}
		}
		t.Fatalf("no detached worktree was created at %q; calls = %+v", path, manager.detachedCalls)
		return ""
	}

	askPayload, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/d-ask").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(ask child): %v", err)
	}
	if strings.TrimSpace(askPayload.WorktreePath) == "" {
		t.Fatal("ask fan-out child got no detached worktree")
	}
	if got := baseFor(askPayload.WorktreePath); got != "task-005" {
		t.Fatalf("ask child worktree base = %q, want the branch tip task-005", got)
	}
	if askPayload.HeadSHA != "" {
		t.Fatalf("ask child HeadSHA = %q, want it cleared: its worktree is the branch tip, not that commit", askPayload.HeadSHA)
	}

	reviewPayload, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/d-review").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(review child): %v", err)
	}
	if strings.TrimSpace(reviewPayload.WorktreePath) == "" {
		t.Fatal("review fan-out child got no detached worktree")
	}
	if got := baseFor(reviewPayload.WorktreePath); got != "head-two" {
		t.Fatalf("review child worktree base = %q, want the exact head head-two", got)
	}
	if reviewPayload.HeadSHA != "head-two" {
		t.Fatalf("review child HeadSHA = %q, want the exact head head-two retained", reviewPayload.HeadSHA)
	}
}

// TestReadOnlyFanoutDeferredEventMatchesWorkerGate pins the audit record emitted
// when detached isolation is UNAVAILABLE at dispatch against what the daemon
// worker actually does with the child.
//
// The consumer is prepareNativeReviewWorktreeForRunner
// (internal/cli/daemon_checkout.go, read at the head of this change), whose gate
// returns early — leaving the child in the SHARED checkout — unless all of:
//
//	job.Type == "review" && PullRequest > 0 && HeadSHA != "" &&
//	ReviewRound != "" && len(Reviewers) > 0 && WorktreePath == ""
//
// job.Type is request.Action verbatim (mailbox.go), and Reviewers/ReviewRound are
// inherited from the parent payload (delegationRequest), so the engine can read
// every conjunct at dispatch. Keyed on HeadSHA alone it could not, and recorded
// "deferred to the job worker; shared-checkout delivery remains forbidden" for
// the two classes below, which the gate rejects and which then ran in the shared
// checkout the event called forbidden.
func TestReadOnlyFanoutDeferredEventMatchesWorkerGate(t *testing.T) {
	fullReviewCoordinator := JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-005",
		PullRequest: 7,
		HeadSHA:     "head-two",
		TaskID:      "task-5",
		TaskTitle:   "Parent",
		ReviewRound: "review-1",
		Reviewers:   []string{"worker-a", "worker-b"},
		Sender:      "coord",
	}

	cases := []struct {
		name string
		// which worker-gate conjunct the children miss, or "" for none
		violates    string
		action      string
		payload     JobPayload
		wantKind    string
		wantOtherIs string
	}{
		{
			// job.Type is "ask", so `job.Type != "review"` returns the gate early.
			name:        "ask children of a full review coordinator",
			violates:    `job.Type != "review"`,
			action:      "ask",
			payload:     fullReviewCoordinator,
			wantKind:    "delegation_worktree_skipped",
			wantOtherIs: "delegation_worktree_deferred",
		},
		{
			// A review child of a NON-review coordinator inherits a blank ReviewRound
			// and no Reviewers, so the gate returns early on those two conjuncts.
			name:     "review children of a non-review coordinator",
			violates: `ReviewRound == "" && len(Reviewers) == 0`,
			action:   "review",
			payload: JobPayload{
				Repo:        "gitmoot/gitmoot",
				Branch:      "task-005",
				PullRequest: 7,
				HeadSHA:     "head-two",
				TaskID:      "task-5",
				TaskTitle:   "Parent",
				Sender:      "coord",
			},
			wantKind:    "delegation_worktree_skipped",
			wantOtherIs: "delegation_worktree_deferred",
		},
		{
			// Single-conjunct isolation: reviewers are inherited but the coordinator
			// is not in a review round, so the gate still returns early.
			name:     "review children of a coordinator with no review round",
			violates: `ReviewRound == ""`,
			action:   "review",
			payload: JobPayload{
				Repo:        "gitmoot/gitmoot",
				Branch:      "task-005",
				PullRequest: 7,
				HeadSHA:     "head-two",
				TaskID:      "task-5",
				TaskTitle:   "Parent",
				Reviewers:   []string{"worker-a", "worker-b"},
				Sender:      "coord",
			},
			wantKind:    "delegation_worktree_skipped",
			wantOtherIs: "delegation_worktree_deferred",
		},
		{
			// Single-conjunct isolation: a PR-less review round (a task review with a
			// recorded head but no pull request) also fails the gate.
			name:     "review children of a PR-less coordinator",
			violates: "PullRequest <= 0",
			action:   "review",
			payload: JobPayload{
				Repo:        "gitmoot/gitmoot",
				Branch:      "task-005",
				HeadSHA:     "head-two",
				TaskID:      "task-5",
				TaskTitle:   "Parent",
				ReviewRound: "review-1",
				Reviewers:   []string{"worker-a", "worker-b"},
				Sender:      "coord",
			},
			wantKind:    "delegation_worktree_skipped",
			wantOtherIs: "delegation_worktree_deferred",
		},
		{
			// Control: every conjunct holds, so the worker really does allocate the
			// exact-head worktree and "deferred" is the truthful record.
			name:        "review children of a full review coordinator",
			violates:    "",
			action:      "review",
			payload:     fullReviewCoordinator,
			wantKind:    "delegation_worktree_deferred",
			wantOtherIs: "delegation_worktree_skipped",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "coord", []string{"review"}, "gitmoot/gitmoot")
			seedAgent(t, store, "worker-a", []string{tc.action}, "gitmoot/gitmoot")
			seedAgent(t, store, "worker-b", []string{tc.action}, "gitmoot/gitmoot")
			engine := testEngine(store)
			// No engine.Home / engine.DelegationWorktrees: dispatch-time detached
			// isolation is unavailable, which is the branch that emits the record.

			parent := tc.payload
			parent.Result = &AgentResult{
				Decision: "approved",
				Summary:  "done",
				Delegations: []Delegation{
					{ID: "d1", Agent: "worker-a", Action: tc.action, Prompt: "one"},
					{ID: "d2", Agent: "worker-b", Action: tc.action, Prompt: "two"},
				},
			}
			insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "review"}, parent)

			if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
				t.Fatalf("AdvanceJob returned error: %v", err)
			}

			if got := countJobEvents(t, store, "parent-job", tc.wantKind); got != 2 {
				t.Fatalf("%s count = %d, want 2 (children violate: %q)", tc.wantKind, got, tc.violates)
			}
			if got := countJobEvents(t, store, "parent-job", tc.wantOtherIs); got != 0 {
				t.Fatalf("%s count = %d, want 0 (children violate: %q)", tc.wantOtherIs, got, tc.violates)
			}

			// Record the ground truth the worker gate will read off each child, so the
			// expectation above is checkable against daemon_checkout.go without this
			// test reimplementing the gate.
			for _, id := range []string{"parent-job/delegation/d1", "parent-job/delegation/d2"} {
				child := mustJob(t, store, id)
				childPayload, err := unmarshalPayload(child.Payload)
				if err != nil {
					t.Fatalf("unmarshalPayload(%s): %v", id, err)
				}
				if strings.TrimSpace(childPayload.WorktreePath) != "" {
					t.Fatalf("%s got a dispatch-time worktree %q; the worker gate's WorktreePath conjunct no longer holds", id, childPayload.WorktreePath)
				}
				gateAdmits := child.Type == "review" &&
					childPayload.PullRequest > 0 &&
					strings.TrimSpace(childPayload.HeadSHA) != "" &&
					strings.TrimSpace(childPayload.ReviewRound) != "" &&
					len(childPayload.Reviewers) > 0
				if gateAdmits != (tc.wantKind == "delegation_worktree_deferred") {
					t.Fatalf("%s: worker gate admits=%t (type=%q pr=%d head=%q round=%q reviewers=%v) but the engine recorded %q",
						id, gateAdmits, child.Type, childPayload.PullRequest, childPayload.HeadSHA,
						childPayload.ReviewRound, childPayload.Reviewers, tc.wantKind)
				}
			}
		})
	}
}
