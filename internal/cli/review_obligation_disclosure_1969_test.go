package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1969. The findings ledger has a write half and a read half. The read half -
// disclosing each prior obligation's uid in the review brief - was reachable
// from HandlePullRequestOpened's fan-out ONLY, and the fan-out has never
// recorded a finding on this fleet.
//
// Measured on this box's ledger, 2026-09-05 to 2026-09-07: all 571 recorded
// findings were written by CLI-dispatched review jobs (observer_job LIKE
// 'local-%'), ZERO by the fan-out, and not one of their prompts contains a
// rendered 'uid=' line. So no reviewer has ever been handed a uid by the
// engine, and a uid is obtainable ONLY by being told it.
//
// The consequence, and the separation is total across eight repositories: every
// repository that ever answered a finding had review prompts mentioning
// continues_uid, because a SEAT hand-wrote the obligations in - gitmoot 32
// prompts and 100 answered, keephair 2 and 11, coordinator-checkin 1 and 17.
// Every repository whose prompts never mentioned it answered nothing: joltra
// 0 and 0 despite 49 implement jobs, plus vetrina, numbra, among-friends and
// omp-role-router. 211 findings recorded in those five, none ever answered.
//
// This test drives the REAL dispatch path, which is the whole point: a test
// against HandlePullRequestOpened would have passed throughout the defect.
func TestCLIReviewDispatchDisclosesPriorLedgerObligations(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout, firstHead, secondHead := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})

	// A prior round recorded an open obligation at the FIRST head. The second
	// round is dispatched at the second head and must be told its uid.
	uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo:             "owner/repo",
		PullRequest:      12,
		HeadSHA:          firstHead,
		ObserverJob:      "local-review-prior-round",
		State:            db.FindingOpen,
		Severity:         "P1",
		RoundLabel:       "F1",
		Title:            "the fix leg pushes before it validates the branch",
		Detail:           "asset files are written before branch validation and neither is rolled back",
		File:             "internal/cli/agent_dispatch.go",
		Line:             411,
		EvidenceKind:     db.EvidenceExecuted,
		ExecutedCommands: []string{"go test ./internal/cli"},
		ExecutedCount:    1,
	})
	if err != nil {
		t.Fatalf("RecordReviewFindingObservation: %v", err)
	}
	if strings.TrimSpace(uid) == "" {
		t.Fatal("fixture recorded no uid")
	}

	out, err := dispatchLocalAgentJob(ctx, store, reviewDispatchRequest(home, secondHead))
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob: %v", err)
	}
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}

	// THE UID IS THE ASSERTION, not the surrounding prose. A brief that explains
	// the rules without naming the obligation leaves it exactly as undischargeable
	// as no brief at all, which is the state this test was written against.
	if !strings.Contains(payload.Instructions, uid) {
		t.Fatalf("review prompt does not name the prior obligation %q, so the reviewer cannot cite it as continues_uid and the finding can never be answered; prompt tail: %q",
			uid, tailOf(payload.Instructions, 600))
	}
	if !strings.Contains(payload.Instructions, "continues_uid") {
		t.Fatalf("review prompt names the uid but never says how to cite it; prompt tail: %q", tailOf(payload.Instructions, 600))
	}
	// The operator's own instructions must survive: the brief is additive.
	if !strings.Contains(payload.Instructions, "Review this exact head.") {
		t.Fatalf("the brief replaced the operator's instructions instead of appending to them: %q", payload.Instructions)
	}
}

// The control. A review dispatch with no prior obligations at its head must get
// a byte-identical prompt, or this change becomes prompt noise on every review
// in the fleet, most of which have no ledger history.
func TestCLIReviewDispatchLeavesThePromptUntouchedWithNoObligations(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout, _, secondHead := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})

	request := reviewDispatchRequest(home, secondHead)
	out, err := dispatchLocalAgentJob(ctx, store, request)
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob: %v", err)
	}
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}
	if payload.Instructions != request.Instructions {
		t.Fatalf("prompt changed with an empty ledger:\n got %q\nwant %q", payload.Instructions, request.Instructions)
	}
}

func tailOf(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return "..." + value[len(value)-n:]
}
