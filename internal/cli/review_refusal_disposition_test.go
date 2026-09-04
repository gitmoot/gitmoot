package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1512: a review leg whose CHECKOUT head can never match its pinned review head
// is refused by the daemon pre-flight before it runs — no adapter, no prompt, no
// deadline started. It used to be finalized with the TIMEOUT verb and the message
// "ended without a result", so a timed-out child, a superseded child and one that
// never started were indistinguishable in the event stream, and any host's count
// of delegation timeouts was inflated by refusals.
//
// The reachable producer (measured in workflow note 115235) is a review child of a
// NON-review coordinator: internal/workflow/engine_run_budgets.go inherits
// ReviewRound and Reviewers from the PARENT payload, so an ask coordinator yields
// a child with PullRequest > 0 and a HeadSHA but a blank ReviewRound and no
// Reviewers — the one shape the engine's allocator and the worker's gate both
// decline, leaving it on the shared checkout.
//
// This drives one real worker tick rather than calling the finalizer directly, so
// it fails on the pre-fix code (which records delegation_timeout_finalized) and
// passes only when the recorded cause is the refusal.
func TestPreflightRefusedReviewChildIsNotFinalizedAsATimeout(t *testing.T) {
	ctx := context.Background()
	// The shared registered checkout sits on main; the review head lives on a PR
	// branch, so no fetch and no elapsed time can make the two equal.
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")

	mainHead := daemonWorkerHeadSHA(t, checkout)
	runDaemonWorkerGit(t, checkout, "checkout", "-b", "feat/x")
	if err := os.WriteFile(checkout+"/feature.txt", []byte("pr work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "feature.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "pr commit")
	prHead := daemonWorkerHeadSHA(t, checkout)
	runDaemonWorkerGit(t, checkout, "checkout", "main")
	if daemonWorkerHeadSHA(t, checkout) != mainHead {
		t.Fatal("setup: shared checkout is not back on main")
	}

	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       77,
		HeadBranch:   "feat/x",
		HeadSHA:      prHead,
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	// The NON-review coordinator parent: no ReviewRound, no Reviewers.
	const parentID = "ask-coordinator-1512"
	encodedParent, err := json.Marshal(workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "feat/x",
		PullRequest:  77,
		HeadSHA:      prHead,
		TaskID:       "task-1512",
		Instructions: "ask coordinator",
	})
	if err != nil {
		t.Fatalf("marshal parent payload: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID:      parentID,
		Agent:   "reviewer",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: string(encodedParent),
	}); err != nil {
		t.Fatalf("CreateJob(parent) returned error: %v", err)
	}

	const childID = parentID + "/delegation/review-child"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:              childID,
		Agent:           "reviewer",
		Action:          "review",
		Repo:            "owner/repo",
		Branch:          "feat/x",
		PullRequest:     77,
		HeadSHA:         prHead,
		TaskID:          "task-1512",
		Instructions:    "review the PR",
		ParentJobID:     parentID,
		DelegationID:    "review-child",
		DelegationDepth: 1,
		DelegatedBy:     "reviewer",
		RootJobID:       parentID,
	})

	child, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(child) returned error: %v", err)
	}
	childPayload, err := daemonJobPayload(child)
	if err != nil {
		t.Fatalf("daemonJobPayload(child) returned error: %v", err)
	}
	// The producer shape this test exists for: both allocators decline it.
	if strings.TrimSpace(childPayload.ReviewRound) != "" || len(childPayload.Reviewers) != 0 || strings.TrimSpace(childPayload.WorktreePath) != "" {
		t.Fatalf("setup: want a path-less child with no review round and no reviewers, got round=%q reviewers=%v path=%q",
			childPayload.ReviewRound, childPayload.Reviewers, childPayload.WorktreePath)
	}

	worker := defaultJobWorker(store, io.Discard)
	if err := runDaemonWorkerTickTracked(ctx, store, worker, 1, false, "owner/repo", "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked returned error: %v", err)
	}

	events, err := store.ListJobEvents(ctx, childID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	// The head-mismatch refusal really happened: this test would otherwise pass
	// vacuously on any change that stopped refusing.
	if !hasResyncEvent(events, "failed") {
		t.Fatalf("events = %+v, want the pre-flight head-mismatch failure", events)
	}
	if hasResyncEvent(events, workflow.JobEventDelegationTimeoutFinalized) {
		t.Fatalf("events = %+v, want NO %s: this leg never ran, so no deadline could elapse",
			events, workflow.JobEventDelegationTimeoutFinalized)
	}
	messages := jobEventMessages(events, workflow.JobEventDelegationRefusedFinalized)
	if len(messages) != 1 {
		t.Fatalf("events = %+v, want exactly one %s event", events, workflow.JobEventDelegationRefusedFinalized)
	}
	if !strings.Contains(messages[0], "was refused before it ran and never started") {
		t.Fatalf("%s message = %q, want it to say the child never started", workflow.JobEventDelegationRefusedFinalized, messages[0])
	}
	if strings.Contains(messages[0], "ended without a result") {
		t.Fatalf("%s message = %q, must not claim the child ran and returned nothing", workflow.JobEventDelegationRefusedFinalized, messages[0])
	}
	// The parent DAG is still advanced exactly as before: the delegation's
	// failure_policy must fire for a refusal as it does for a runtime failure.
	settled, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(settled) returned error: %v", err)
	}
	settledPayload, err := daemonJobPayload(settled)
	if err != nil {
		t.Fatalf("daemonJobPayload(settled) returned error: %v", err)
	}
	if settledPayload.Result == nil {
		t.Fatal("refused child stored no synthetic result, so the parent DAG was never advanced")
	}
	// It is terminal on the FIRST tick and consumes no operational-blocker budget:
	// a remedy must not turn this into a deferral (#1512 requirement 1).
	if settledPayload.BlockerAttempts != 0 || strings.TrimSpace(settledPayload.BlockerClass) != "" {
		t.Fatalf("refused child consumed blocker budget: attempts=%d class=%q",
			settledPayload.BlockerAttempts, settledPayload.BlockerClass)
	}
}
