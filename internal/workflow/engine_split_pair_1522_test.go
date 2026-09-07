package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1522: the fix leg was dispatched on the FIRST blocking verdict at a head, so
// a cross-family pair that splits is unsatisfiable in one round. The first
// objection's leg pushes a new head while the sibling is still reading the old
// one, and the sibling's verdict is stale the moment it lands.
//
// MEASURED over the live job store, and larger than the 8 occurrences the issue
// records: 37 of 334 dispatched fix legs (11.1%) were created while at least one
// sibling review AT THE SAME HEAD was still running. Those siblings then landed
// 51 verdicts against a head that no longer existed: 29 approvals at an average
// of 12.0 minutes late (worst 58 minutes) and 22 further objections at 9.2
// minutes late.
//
// The 29 stale approvals are the merge-integrity half. An "approved" recorded
// for a commit the fix leg superseded minutes later is a record asserting
// something it cannot support.
//
// NOT REPRODUCED, and reported on the issue rather than fixed: defect (A), "the
// leg predates the record that authorises it", measured there as -1s in 5 of 5.
// It does not survive re-derivation against the full store. 325 of 330 matched
// legs have BOTH the verdict's `succeeded` and its `advance_started` job events
// stamped before the leg's created_at, so causality is reconstructible from the
// ordered event log. The -1s is an artifact of comparing against jobs.updated_at,
// which is the last write to the row and not the verdict time.
func TestSplitPairDefersTheFixLegUntilTheSiblingReviewSettles(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	enableAutoFix(t, store, 1678)
	// The durable implementer attribution row autoFixOwner requires, exactly as
	// TestFollowUpReviewScopesFindingsAndFilesFromReviewerHead seeds it. Present
	// here so a run against the pre-fix engine fails on the ORDERING assertion
	// below rather than on an attribution gap it would hit first.
	insertCompletedJob(t, store, db.Job{ID: "initial-implement", Agent: "lead", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		LeadAgent:   "lead",
	})

	// The sibling family, dispatched at the same head and still running.
	insertActiveJob(t, store, db.Job{ID: "sibling-claude", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		HeadSHA:     "head-one",
		LeadAgent:   "lead",
	}, JobRunning)

	insertPriorReviewResult(t, store, "codex-objection", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Three MAJORs on the boundary path.",
		Findings: []json.RawMessage{
			json.RawMessage(`{"id":"F-1","file":"internal/boundary.go","summary":"unguarded failure path"}`),
		},
	})
	if err := engine.AdvanceJob(ctx, "codex-objection"); err != nil {
		t.Fatalf("AdvanceJob objection: %v", err)
	}

	if _, err := store.GetJob(ctx, "implement-lead-task-1678-review-1"); err == nil {
		t.Fatal("the fix leg was dispatched while a sibling review at the same head was still running; its verdict is stale on arrival and its round is wasted")
	}
	assertJobEvent(t, store, "codex-objection", "auto_fix_deferred_live_sibling", "sibling-claude")

	// The objection must still transition the task. Deferring the WRITER must not
	// make the PR read as unobjected.
	assertTaskState(t, store, "task-1678", TaskChangesRequested)
}

// The deadlock control, and the reason the approving arm is wired to the same
// helper: a pair that splits objection-then-APPROVAL must still get its fix leg.
// The approval fixes nothing and the objection's arm has already returned, so
// without this the leg is deferred and never dispatched.
func TestSplitPairApprovalIsTheLastSettlerAndOwesTheFixLeg(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	enableAutoFix(t, store, 1678)
	// The durable implementer attribution row autoFixOwner requires, exactly as
	// TestFollowUpReviewScopesFindingsAndFilesFromReviewerHead seeds it. Without
	// it the dispatch blocks on an attribution gap before it can reach the
	// ordering this test is about.
	insertCompletedJob(t, store, db.Job{ID: "initial-implement", Agent: "lead", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		LeadAgent:   "lead",
	})

	// The objection already settled at this head, with its leg deferred.
	insertPriorReviewResult(t, store, "codex-objection", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Three MAJORs on the boundary path.",
		Findings: []json.RawMessage{
			json.RawMessage(`{"id":"F-1","file":"internal/boundary.go","summary":"unguarded failure path"}`),
		},
	})
	// The sibling approves the SAME head, and is the last review to settle.
	insertPriorReviewResult(t, store, "claude-approval", "head-one", AgentResult{
		Decision: "approved",
		Summary:  "No blocking findings from this lens.",
	})
	if err := engine.AdvanceJob(ctx, "claude-approval"); err != nil {
		t.Fatalf("AdvanceJob approval: %v", err)
	}

	fix, err := store.GetJob(ctx, "implement-lead-task-1678-review-1")
	if err != nil {
		t.Fatalf("the approving sibling settled last and no fix leg was dispatched, so the objection is stranded: %v", err)
	}
	fixPayload, err := unmarshalPayload(fix.Payload)
	if err != nil {
		t.Fatalf("unmarshal fix payload: %v", err)
	}
	// The leg must carry the OBJECTION's findings, not the approval's silence.
	if !strings.Contains(fixPayload.Instructions, `"id":"F-1"`) {
		t.Fatalf("the fix leg dispatched by the approving arm does not carry the objection it exists for: %q", fixPayload.Instructions)
	}
}

// One leg per head, built so that #1533's guard CANNOT be what satisfies it:
// that guard refuses a second writer only while the first is queued or running,
// so the first leg is SETTLED here before the second objection advances.
//
// WHAT THE PRE-FIX ENGINE ACTUALLY DOES, measured rather than assumed, because
// my first version of this comment guessed wrong. It does not produce two legs
// in this shape. It fails the second review's advance outright:
//
//	AdvanceJob second objection: constraint failed: UNIQUE constraint failed: jobs.id (1555)
//
// The fix-leg id is deterministic from task and review round, so two objections
// in one round derive the SAME id and the second insert collides. The advance
// therefore never reconciles and the daemon re-drives it on every tick, while
// the collision is the only thing standing between this PR and a duplicate
// writer. That is protection by accident: change the id derivation, or let the
// two objections fall in different rounds, and the duplicate leg is back.
//
// After the fix the second objection reaches a DECISION instead of a constraint
// error: the head already carries a fix pass, so it records that and settles.
// The distinction phobos asked to keep visible is exactly this: #1533 changed
// the symptom of a split pair, and only the head rule changes the outcome.
func TestASecondObjectionAtAnAlreadyDispatchedHeadDoesNotDispatchAgain(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	enableAutoFix(t, store, 1678)
	insertCompletedJob(t, store, db.Job{ID: "initial-implement", Agent: "lead", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		LeadAgent:   "lead",
	})

	insertPriorReviewResult(t, store, "codex-objection", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "First family objection.",
		Findings: []json.RawMessage{json.RawMessage(`{"id":"F-1","file":"internal/a.go","summary":"first"}`)},
	})
	if err := engine.AdvanceJob(ctx, "codex-objection"); err != nil {
		t.Fatalf("AdvanceJob first objection: %v", err)
	}
	first := fixLegIDs(t, store)
	if len(first) != 1 {
		t.Fatalf("first objection produced fix legs %v, want exactly 1", first)
	}
	// Settle it, so the branch is idle and the concurrency bound cannot be what
	// refuses the second dispatch.
	leg, err := store.GetJob(ctx, first[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateJobState(ctx, leg.ID, string(JobSucceeded)); err != nil {
		t.Fatalf("settle the first leg: %v", err)
	}

	insertPriorReviewResult(t, store, "claude-objection", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Second family objection at the same head.",
		Findings: []json.RawMessage{json.RawMessage(`{"id":"F-2","file":"internal/b.go","summary":"second"}`)},
	})
	if err := engine.AdvanceJob(ctx, "claude-objection"); err != nil {
		t.Fatalf("AdvanceJob second objection: %v", err)
	}

	after := fixLegIDs(t, store)
	if len(after) != 1 {
		t.Fatalf("fix legs at one head = %v, want exactly 1; a second leg for a head whose fix pass already landed re-does or conflicts with work that exists", after)
	}
}

func fixLegIDs(t *testing.T, store *db.Store) []string {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	var legs []string
	for _, candidate := range jobs {
		if candidate.Type != "implement" {
			continue
		}
		candidatePayload, err := unmarshalPayload(candidate.Payload)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", candidate.ID, err)
		}
		if candidatePayload.FixWorktree {
			legs = append(legs, candidate.ID)
		}
	}
	return legs
}

// The liveness control a deferral guard breaks if it is written carelessly: a
// SOLE review at a head, with no sibling ever dispatched, must dispatch
// immediately. Withholding a fix must never become the accidental default.
func TestSoleObjectionAtAHeadStillDispatchesImmediately(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	enableAutoFix(t, store, 1678)
	// The durable implementer attribution row autoFixOwner requires, exactly as
	// TestFollowUpReviewScopesFindingsAndFilesFromReviewerHead seeds it. Without
	// it the dispatch blocks on an attribution gap before it can reach the
	// ordering this test is about.
	insertCompletedJob(t, store, db.Job{ID: "initial-implement", Agent: "lead", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		LeadAgent:   "lead",
	})

	// A settled review at a DIFFERENT head, and an active review on a different
	// head, neither of which may defer this one.
	insertPriorReviewResult(t, store, "older-round", "head-zero", AgentResult{
		Decision: "changes_requested",
		Summary:  "an objection about a superseded head",
	})
	insertActiveJob(t, store, db.Job{ID: "reviewer-on-next-head", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		HeadSHA:     "head-two",
		LeadAgent:   "lead",
	}, JobRunning)

	insertPriorReviewResult(t, store, "lone-objection", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Fix the boundary check.",
		Findings: []json.RawMessage{json.RawMessage(`{"id":"F-1","file":"internal/boundary.go","summary":"boundary"}`)},
	})
	if err := engine.AdvanceJob(ctx, "lone-objection"); err != nil {
		t.Fatalf("AdvanceJob lone objection: %v", err)
	}
	if _, err := store.GetJob(ctx, "implement-lead-task-1678-review-1"); err != nil {
		t.Fatalf("a sole objection at its head did not dispatch, so the guard withholds fixes instead of ordering them: %v", err)
	}
}

func assertJobEvent(t *testing.T, store *db.Store, jobID string, kind string, mustContain string) {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	for _, event := range events {
		if event.Kind == kind && strings.Contains(event.Message, mustContain) {
			return
		}
	}
	t.Fatalf("job %s has no %q event naming %q; the decision is invisible to anyone reading the record. events = %+v", jobID, kind, mustContain, events)
}
