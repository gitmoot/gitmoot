package workflow

import (
	"context"
	"testing"
)

// #1236, the serious half: the native review fanout must never enlist the
// agent that implemented the head as a reviewer of that head. Observed on
// dashboard PR #137, where the fanout picked `galaxy-impl` — the seat that
// implemented it — to review its own work. The merge gate ignores verdict
// authorship (#1114), so a self-approval produced this way is indistinguishable
// from a real one at the gate.
func TestFanoutRefusesToEnlistTheImplementer(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement", "review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "gitmoot/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		HeadSHA:           "head123",
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "impl",
		Sender:            "impl",
		RequiredReviewers: []string{"impl", "reviewer"},
	})
	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	var reviewers []string
	for _, job := range jobs {
		if job.Type == "review" {
			reviewers = append(reviewers, job.Agent)
		}
	}
	for _, agent := range reviewers {
		if agent == "impl" {
			t.Fatalf("the implementer was enlisted to review its own head: review jobs = %v", reviewers)
		}
	}
	if len(reviewers) != 1 || reviewers[0] != "reviewer" {
		t.Fatalf("review jobs = %v, want exactly [reviewer]", reviewers)
	}
}

// #1236, the second defect: the default reviewer roster is not repo-aware. On
// dashboard PR #137 the roster was ["lead"], a seat scoped to gitmoot/gitmoot,
// so the fanout could not have produced a verdict even if left alone.
func TestFanoutDropsReviewersNotScopedToTheRepo(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot-dashboard")
	seedAgent(t, store, "offscope", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "dash-reviewer", []string{"review"}, "gitmoot/gitmoot-dashboard")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "gitmoot/gitmoot-dashboard",
		Branch:            "task-137",
		PullRequest:       137,
		HeadSHA:           "9ec5ceb916a9",
		TaskID:            "task-137",
		LeadAgent:         "impl",
		Sender:            "impl",
		RequiredReviewers: []string{"offscope", "dash-reviewer"},
	})
	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	var reviewers []string
	for _, job := range jobs {
		if job.Type == "review" {
			reviewers = append(reviewers, job.Agent)
		}
	}
	if len(reviewers) != 1 || reviewers[0] != "dash-reviewer" {
		t.Fatalf("review jobs = %v, want exactly [dash-reviewer]", reviewers)
	}
}

// The safety-critical consequence of filtering. HandlePullRequestOpened treats
// an EMPTY roster as "this repo has no native review discipline" and runs the
// MERGE GATE directly. Filtering must therefore never let a roster that WAS
// configured collapse into that path: a PR whose every configured reviewer is
// ineligible must fail CLOSED — no review jobs, and above all no merge gate —
// rather than inheriting the unconfigured-repo behaviour and becoming eligible
// to merge with no review at all.
func TestFanoutWithNoEligibleReviewerDoesNotRunTheMergeGate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement", "review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Merged: true}}
	engine.MergeGate = gate

	// The only configured reviewer IS the implementer, so the roster empties.
	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "gitmoot/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		HeadSHA:           "head123",
		TaskID:            "task-7",
		LeadAgent:         "impl",
		Sender:            "impl",
		RequiredReviewers: []string{"impl"},
	})
	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}

	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran for a PR with no eligible reviewer: %+v", gate.requests)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("expected no review jobs, found %+v", job)
		}
	}
}

// A repo with NO reviewers configured at all must keep its existing behaviour —
// the merge gate runs. This is the arm the filter must not disturb.
func TestFanoutUnconfiguredRosterStillRunsTheMergeGate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Reason: PlainReason("ci is pending")}}
	engine.MergeGate = gate

	// The gate's "ci is pending" decision blocks the task, so an error here is
	// the expected shape; what this test asserts is that the gate RAN.
	_ = engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "head123",
		TaskID:      "task-7",
		LeadAgent:   "impl",
		Sender:      "impl",
	})
	if len(gate.requests) != 1 {
		t.Fatalf("merge gate requests = %+v, want exactly one for an unconfigured roster", gate.requests)
	}
}

func TestFanoutSelectsOneRuntimeFamilyInConfiguredOrder(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "owner/repo")
	seedReviewLoopAgent(t, store, "codex-a", "codex", "gpt-5.6-sol")
	seedReviewLoopAgent(t, store, "codex-b", "codex", "gpt-5.6-sol")
	seedReviewLoopAgent(t, store, "claude-a", "claude", "opus-5")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "owner/repo",
		Branch:            "task-7",
		PullRequest:       7,
		HeadSHA:           "head123",
		TaskID:            "task-7",
		LeadAgent:         "impl",
		Sender:            "impl",
		RequiredReviewers: []string{"codex-a", "claude-a", "codex-b"},
	})
	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	var reviewers []string
	for _, job := range jobs {
		if job.Type == "review" {
			reviewers = append(reviewers, job.Agent)
		}
	}
	if len(reviewers) != 2 || reviewers[0] != "codex-a" || reviewers[1] != "codex-b" {
		t.Fatalf("review jobs = %v, want [codex-a codex-b]", reviewers)
	}
}

func TestConfiguredNativeFanoutOffSkipsReviewsAndMergeGate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "owner/repo")
	seedAgent(t, store, "reviewer", []string{"review"}, "owner/repo")
	engine := testEngine(store)
	engine.NativeReviewFanoutEnabled = func(string) bool { return false }
	gate := &fakeMergeGate{decision: MergeDecision{Merged: true}}
	engine.MergeGate = gate

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "owner/repo",
		Branch:            "task-8",
		PullRequest:       8,
		HeadSHA:           "head-off",
		TaskID:            "task-8",
		LeadAgent:         "impl",
		Sender:            "impl",
		RequiredReviewers: []string{"reviewer"},
	})
	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	var reviewJobs int
	for _, job := range jobs {
		if job.Type == "review" {
			reviewJobs++
		}
	}
	if reviewJobs != 0 || len(gate.requests) != 0 {
		t.Fatalf("fanout off produced %d review jobs and %d merge-gate requests", reviewJobs, len(gate.requests))
	}
}
