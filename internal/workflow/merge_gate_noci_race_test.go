package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

// workflowAwareFakeGitHub extends fakeMergeGateGitHub with the OPTIONAL #596
// layer-2 workflow-awareness capability so tests can exercise the ".github/
// workflows exists at head" path (and its error/fail-safe branch).
type workflowAwareFakeGitHub struct {
	*fakeMergeGateGitHub
	workflowsExist bool
	workflowsErr   error
	workflowCalls  int
}

func (f *workflowAwareFakeGitHub) WorkflowsExistAtRef(context.Context, github.Repository, string) (bool, error) {
	f.workflowCalls++
	if f.workflowsErr != nil {
		return false, f.workflowsErr
	}
	return f.workflowsExist, nil
}

// setupApprovedNoCIPR wires the minimum state for a mergeable, review-approved PR
// whose head reports ZERO external CI (only the gitmoot/merge-gate contexts,
// which the gate skips). It is the shared fixture for the no-CI race tests.
func setupApprovedNoCIPR(t *testing.T, store *db.Store, headSHA string) *fakeMergeGateGitHub {
	t.Helper()
	ctx := context.Background()
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/noted", Branch: "task-11", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "jerryfane/noted",
		Branch:      "task-11",
		PullRequest: 11,
		HeadSHA:     headSHA,
		TaskID:      "task-11",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	return &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:    11,
			Title:     "Task 11",
			State:     "open",
			URL:       "https://github.com/jerryfane/noted/pull/11",
			HeadRef:   "task-11",
			BaseRef:   "main",
			HeadSHA:   headSHA,
			Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success"},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge-11"},
	}
}

func noCIRequest() MergeRequest {
	return MergeRequest{Repo: "jerryfane/noted", PullRequest: 11, TaskID: "task-11", Reviewer: "audit"}
}

// TestPolicyMergeGateNoCIRaceDefersThenRequiresLateCheck is the #596 regression:
// evaluation 1 sees zero external statuses/checks (the GitHub Actions creation-lag
// window); a check-run APPEARS before evaluation 2 (as in the live table where the
// run was created 2s after the old code stamped gitmoot/ci). With the grace fix,
// evaluation 1 defers (mergePending) instead of stamping gitmoot/ci and merging
// unguarded, so evaluation 2 observes and requires the check that has since
// appeared — and the PR merges only after the check passes.
func TestPolicyMergeGateNoCIRaceDefersThenRequiresLateCheck(t *testing.T) {
	resetWorkflowPresenceCache()
	ctx := context.Background()
	store := openEngineStore(t)
	gh := setupApprovedNoCIPR(t, store, "d342f97")
	clock := &fakeClock{now: time.Date(2026, 7, 1, 22, 23, 32, 0, time.UTC)}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir(), MinCIWait: time.Minute, Clock: clock.Now}

	// Evaluation 1: zero external CI in the Actions lag window -> defer, do NOT
	// stamp gitmoot/ci, do NOT merge.
	d1, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 1 returned error: %v", err)
	}
	if d1.Merged {
		t.Fatal("evaluation 1 merged in the Actions lag window — the #596 race is not closed")
	}
	if !d1.Ready || !strings.Contains(d1.Reason, "waiting to confirm no external CI") {
		t.Fatalf("evaluation 1 decision = %+v, want pending waiting-to-confirm", d1)
	}
	if hasStatus(gh.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("evaluation 1 stamped gitmoot/ci — the empty gate must defer, statuses=%+v", gh.statuses)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("evaluation 1 issued a merge, merges=%+v", gh.merges)
	}

	// The CI check-run appears (pending) between evaluations, exactly as observed
	// live 2s after the old stamp.
	gh.checks = []github.PullRequestCheck{{Name: "ci", Bucket: "pending", State: "IN_PROGRESS"}}
	clock.advance(2 * time.Second)

	// Evaluation 2: the now-visible check gates the merge (pending), still no merge
	// and still no synthetic gitmoot/ci stamp.
	d2, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 2 returned error: %v", err)
	}
	if d2.Merged {
		t.Fatal("evaluation 2 merged while the external CI check was pending")
	}
	if !strings.Contains(d2.Reason, "pending") {
		t.Fatalf("evaluation 2 decision = %+v, want pending-on-external-check", d2)
	}
	if hasStatus(gh.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("evaluation 2 stamped gitmoot/ci despite a visible check, statuses=%+v", gh.statuses)
	}

	// The check passes; the PR merges — gated by real CI, never through an empty gate.
	gh.checks = []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}}
	clock.advance(5 * time.Second)
	d3, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 3 returned error: %v", err)
	}
	if !d3.Merged {
		t.Fatalf("evaluation 3 decision = %+v, want merged after CI passed", d3)
	}
	if hasStatus(gh.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("gitmoot/ci was stamped even though real CI existed, statuses=%+v", gh.statuses)
	}
}

// TestPolicyMergeGateNoCIMergesAfterGraceWindow pins that a GENUINELY CI-less repo
// still merges — one grace window later. The first evaluation records the zero
// observation and defers; the second, past MinCIWait with the head unchanged and
// still zero external CI, concludes no-CI, stamps gitmoot/ci, and merges.
func TestPolicyMergeGateNoCIMergesAfterGraceWindow(t *testing.T) {
	resetWorkflowPresenceCache()
	ctx := context.Background()
	store := openEngineStore(t)
	gh := setupApprovedNoCIPR(t, store, "cico001")
	clock := &fakeClock{now: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir(), MinCIWait: time.Minute, Clock: clock.Now}

	d1, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 1 returned error: %v", err)
	}
	if d1.Merged {
		t.Fatal("evaluation 1 merged before the grace window elapsed")
	}
	obs, err := store.GetNoCIObservation(ctx, "jerryfane/noted", 11)
	if err != nil {
		t.Fatalf("expected a recorded observation after evaluation 1: %v", err)
	}
	if obs.HeadSHA != "cico001" {
		t.Fatalf("observation head = %q, want cico001", obs.HeadSHA)
	}

	clock.advance(90 * time.Second) // past MinCIWait
	d2, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 2 returned error: %v", err)
	}
	if !d2.Merged {
		t.Fatalf("evaluation 2 decision = %+v, want merged after the grace window", d2)
	}
	if !hasStatus(gh.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("genuinely CI-less repo should stamp gitmoot/ci once, statuses=%+v", gh.statuses)
	}
}

// TestPolicyMergeGateNoCIObservationResetsOnNewHead pins that a fresh head between
// the two observations restarts the grace window: the gate never merges off the
// old head's clock even after MinCIWait has elapsed.
func TestPolicyMergeGateNoCIObservationResetsOnNewHead(t *testing.T) {
	resetWorkflowPresenceCache()
	ctx := context.Background()
	store := openEngineStore(t)
	gh := setupApprovedNoCIPR(t, store, "headAAA")
	clock := &fakeClock{now: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir(), MinCIWait: time.Minute, Clock: clock.Now}

	if _, err := gate.Evaluate(ctx, noCIRequest()); err != nil {
		t.Fatalf("evaluation 1 returned error: %v", err)
	}

	// A new head is pushed AND the grace window elapses. The gate must NOT merge:
	// the observation belongs to the old head, so this counts as a fresh first
	// observation for headBBB.
	clock.advance(90 * time.Second)
	gh.pr.HeadSHA = "headBBB"
	// The review must also match the new head for the gate to reach ensureStatuses.
	insertCompletedJob(t, store, db.Job{ID: "review-job-2", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "jerryfane/noted", Branch: "task-11", PullRequest: 11, HeadSHA: "headBBB", TaskID: "task-11",
		ReviewRound: "review-2", Result: &AgentResult{Decision: "approved", Summary: "ready"},
	})

	d2, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 2 returned error: %v", err)
	}
	if d2.Merged {
		t.Fatal("evaluation 2 merged off the stale head's grace clock — the observation did not reset on the new head")
	}
	obs, err := store.GetNoCIObservation(ctx, "jerryfane/noted", 11)
	if err != nil {
		t.Fatalf("GetNoCIObservation returned error: %v", err)
	}
	if obs.HeadSHA != "headBBB" {
		t.Fatalf("observation head = %q, want reset to headBBB", obs.HeadSHA)
	}

	// One grace window past the reset at the new head -> merges.
	clock.advance(90 * time.Second)
	d3, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 3 returned error: %v", err)
	}
	if !d3.Merged {
		t.Fatalf("evaluation 3 decision = %+v, want merged one grace window after the new-head reset", d3)
	}
}

// TestPolicyMergeGateWorkflowAwarenessNeverConcludesNoCI pins layer 2: when the
// head tree demonstrably carries .github/workflows, a zero-external observation is
// an Actions creation lag, so the gate NEVER concludes no-CI — even long past the
// grace window — and never stamps gitmoot/ci.
func TestPolicyMergeGateWorkflowAwarenessNeverConcludesNoCI(t *testing.T) {
	resetWorkflowPresenceCache()
	ctx := context.Background()
	store := openEngineStore(t)
	base := setupApprovedNoCIPR(t, store, "wfhead1")
	gh := &workflowAwareFakeGitHub{fakeMergeGateGitHub: base, workflowsExist: true}
	clock := &fakeClock{now: time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir(), MinCIWait: time.Minute, Clock: clock.Now}

	for i, wait := range []time.Duration{0, 5 * time.Minute, 30 * time.Minute} {
		clock.advance(wait)
		d, err := gate.Evaluate(ctx, noCIRequest())
		if err != nil {
			t.Fatalf("evaluation %d returned error: %v", i+1, err)
		}
		if d.Merged {
			t.Fatalf("evaluation %d merged a workflow-configured repo with no check run yet", i+1)
		}
		if !strings.Contains(d.Reason, "workflows") {
			t.Fatalf("evaluation %d reason = %q, want a workflow-awareness pending reason", i+1, d.Reason)
		}
	}
	if hasStatus(base.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("workflow-aware repo stamped gitmoot/ci, statuses=%+v", base.statuses)
	}
	if gh.workflowCalls != 1 {
		t.Fatalf("workflow-awareness reads = %d, want 1 (cached per head)", gh.workflowCalls)
	}
}

// TestPolicyMergeGateWorkflowReadErrorFailsSafeToGrace pins that a FAILED
// workflow-awareness read fails safe toward the grace path (never an instant
// stamp): evaluation 1 still defers, and the repo only merges after a second
// zero observation past the grace window.
func TestPolicyMergeGateWorkflowReadErrorFailsSafeToGrace(t *testing.T) {
	resetWorkflowPresenceCache()
	ctx := context.Background()
	store := openEngineStore(t)
	base := setupApprovedNoCIPR(t, store, "wferr01")
	gh := &workflowAwareFakeGitHub{fakeMergeGateGitHub: base, workflowsErr: errors.New("HTTP 500")}
	clock := &fakeClock{now: time.Date(2026, 7, 1, 7, 0, 0, 0, time.UTC)}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir(), MinCIWait: time.Minute, Clock: clock.Now}

	d1, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 1 returned error: %v", err)
	}
	if d1.Merged {
		t.Fatal("evaluation 1 merged despite a workflow-read error — must fail safe to grace, not instant-stamp")
	}

	clock.advance(90 * time.Second)
	d2, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("evaluation 2 returned error: %v", err)
	}
	if !d2.Merged {
		t.Fatalf("evaluation 2 decision = %+v, want merged via the grace path after the read error", d2)
	}
}

// TestPolicyMergeGateRequireExternalCIHardBlocks pins layer 3: with
// require_external_ci, an empty gate hard-blocks with an actionable reason rather
// than ever stamping gitmoot/ci.
func TestPolicyMergeGateRequireExternalCIHardBlocks(t *testing.T) {
	resetWorkflowPresenceCache()
	ctx := context.Background()
	store := openEngineStore(t)
	gh := setupApprovedNoCIPR(t, store, "reqci01")
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir(), RequireExternalCI: true}

	d, err := gate.Evaluate(ctx, noCIRequest())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if d.Ready || d.Merged {
		t.Fatalf("decision = %+v, want a hard block (not ready, not merged)", d)
	}
	if !strings.Contains(d.Reason, "requires external CI") {
		t.Fatalf("block reason = %q, want an actionable require-external-CI message", d.Reason)
	}
	if d.BlockClass != MergeBlockQuality {
		t.Fatalf("block class = %v, want MergeBlockQuality", d.BlockClass)
	}
	if hasStatus(gh.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("require_external_ci stamped gitmoot/ci, statuses=%+v", gh.statuses)
	}
	if _, err := store.GetNoCIObservation(ctx, "jerryfane/noted", 11); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("require_external_ci recorded a grace observation; err=%v", err)
	}
}

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }
