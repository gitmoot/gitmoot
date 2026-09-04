package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// #1865: the gate used to request a branch update for EVERY behind head at
// verdict-advancement time. The update creates a merge commit and supersedes
// the head the verdict is bound to within seconds, so the next poll finds an
// unreviewed head and dispatches a fresh paid review. Six occurrences were
// measured in 114 minutes on 2026-09-04 (notes 117997, 118011, 118013).
//
// Every case below drives the PRODUCTION advancement path - PolicyMergeGate
// .Evaluate - not ensureBranchFresh in isolation, so a routing change that
// leaves real verdict advancement on the old path fails these tests.

func behindMergeGateStore(t *testing.T) *db.Store {
	t.Helper()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	return store
}

func behindMergeGateClient(compare github.CompareResult) *fakeMergeGateGitHub {
	mergeable := true
	return &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success"},
		compare:     compare,
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
}

func evaluateBehindMergeGate(t *testing.T, gh *fakeMergeGateGitHub) MergeDecision {
	t.Helper()
	gate := PolicyMergeGate{AutoMerge: true, Store: behindMergeGateStore(t), GitHub: gh, Git: &fakeMergeGateGit{clean: true}}
	decision, err := gate.Evaluate(context.Background(), MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	return decision
}

// Acceptance 1: the verdict's own head is what merges. No update is requested,
// so nothing supersedes the reviewed head.
func TestMergeGateMergesBehindHeadWhenBaseAllowsIt(t *testing.T) {
	gh := behindMergeGateClient(github.CompareResult{Status: "behind", BehindBy: 1})
	gh.strictKnown = true
	gh.strictBase = false

	decision := evaluateBehindMergeGate(t, gh)

	if !decision.Merged {
		t.Fatalf("behind head must merge on its own verdict: decision = %+v", decision)
	}
	if len(gh.updates) != 0 {
		t.Fatalf("no branch update may be requested: updates = %+v", gh.updates)
	}
	if len(gh.merges) != 1 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	// The merge is fenced to the REVIEWED head; that is what makes this a fix
	// rather than a suppression of the pending decision.
	if gh.merges[0].MatchHeadCommit != "head123" {
		t.Fatalf("merge must be fenced to the reviewed head: %+v", gh.merges[0])
	}
	if gh.strictCalls == 0 {
		t.Fatal("protection must actually be consulted, not assumed")
	}
	if len(gh.strictBranches) == 0 || gh.strictBranches[0] != "main" {
		t.Fatalf("protection must be read for the BASE branch: %v", gh.strictBranches)
	}
}

// Acceptance 1, other arm: where GitHub does require an up-to-date head, the
// update is still the only way to merge, so the pre-#1865 path must survive.
func TestMergeGateStillUpdatesWhenBaseRequiresUpToDateHead(t *testing.T) {
	gh := behindMergeGateClient(github.CompareResult{Status: "behind", BehindBy: 1})
	gh.strictKnown = true
	gh.strictBase = true

	decision := evaluateBehindMergeGate(t, gh)

	if decision.Merged || !strings.Contains(decision.Reason.Render(), "branch update") {
		t.Fatalf("strict base must still take the update path: decision = %+v", decision)
	}
	if len(gh.updates) != 1 || gh.updates[0].ExpectedHeadSHA != "head123" {
		t.Fatalf("update inputs = %+v", gh.updates)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

// The guard fails CLOSED. An unprotected branch and a token that cannot read
// protection are indistinguishable, so an undetermined answer keeps the old
// behaviour instead of merging a head GitHub may refuse.
func TestMergeGateFailsClosedWhenProtectionIsUndetermined(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeMergeGateGitHub)
	}{
		{"unknown", func(f *fakeMergeGateGitHub) { f.strictKnown = false }},
		{"error", func(f *fakeMergeGateGitHub) { f.strictErr = errors.New("permission denied") }},
		{"known false but errored", func(f *fakeMergeGateGitHub) {
			f.strictKnown = true
			f.strictErr = errors.New("boom")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := behindMergeGateClient(github.CompareResult{Status: "behind", BehindBy: 1})
			tc.set(gh)

			decision := evaluateBehindMergeGate(t, gh)

			if decision.Merged {
				t.Fatalf("undetermined protection must not merge a behind head: %+v", decision)
			}
			if len(gh.updates) != 1 {
				t.Fatalf("update inputs = %+v", gh.updates)
			}
		})
	}
}

// A diverged branch is not merely behind: merging it can conflict, so the
// update stays mandatory there even when protection allows behind merges.
func TestMergeGateStillUpdatesDivergedBranch(t *testing.T) {
	gh := behindMergeGateClient(github.CompareResult{Status: "diverged", BehindBy: 1, AheadBy: 1})
	gh.strictKnown = true
	gh.strictBase = false

	decision := evaluateBehindMergeGate(t, gh)

	if decision.Merged {
		t.Fatalf("diverged branch must not merge without an update: %+v", decision)
	}
	if len(gh.updates) != 1 {
		t.Fatalf("update inputs = %+v", gh.updates)
	}
	if gh.strictCalls != 0 {
		t.Fatalf("diverged must not even consult protection: calls = %d", gh.strictCalls)
	}
}

// Acceptance 3: the success case. A branch already up to date advances exactly
// as before - the new guard must not reject valid input, and must not spend an
// API call it does not need.
func TestMergeGateMergesUpToDateBranchUnchanged(t *testing.T) {
	for _, status := range []string{"identical", "ahead", ""} {
		t.Run("status="+status, func(t *testing.T) {
			gh := behindMergeGateClient(github.CompareResult{Status: status})
			gh.strictKnown = true
			gh.strictBase = true // must be irrelevant when not behind

			decision := evaluateBehindMergeGate(t, gh)

			if !decision.Merged {
				t.Fatalf("up-to-date branch must merge: %+v", decision)
			}
			if len(gh.updates) != 0 {
				t.Fatalf("updates = %+v", gh.updates)
			}
			if gh.strictCalls != 0 {
				t.Fatalf("protection must not be read when the head is not behind: calls = %d", gh.strictCalls)
			}
		})
	}
}
