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
//
// #2074 review, P2. THE SHAPE HERE IS THE ONE PRODUCTION EMITS. This and the two
// acceptance cases below previously constructed `Status: "behind"`, which GitHub
// cannot report for an open pull request: `behind` requires `ahead_by == 0`, a
// branch with no commits of its own. Measured with the gate's own call
// (CompareCommits -> GET repos/{owner}/{repo}/compare/{base}...{head}) across
// five open PRs on this repo: five `diverged` with `behind_by` 2..9 and
// `ahead_by` 1..5, zero `behind`.
//
// The shape below is copied from one of those responses, #2057's head against
// main: {"status":"diverged","ahead_by":1,"behind_by":4,"total_commits":1}.
//
// The first fix for this converted only the fixture in the test that changed
// behaviour and argued these three could stay, on the grounds that the literal
// `behind` arm of the `||` is still live code. The reviewer rejected that and was
// right: these are PRODUCTION-PATH ACCEPTANCE tests, and the string arm is
// covered deliberately and separately by the unknown-status robustness cases at
// the end of this file. An acceptance test on an unreachable input can stay green
// through a real regression.
func TestMergeGateMergesBehindHeadWhenBaseAllowsIt(t *testing.T) {
	gh := behindMergeGateClient(github.CompareResult{Status: "diverged", BehindBy: 4, AheadBy: 1})
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
	gh := behindMergeGateClient(github.CompareResult{Status: "diverged", BehindBy: 4, AheadBy: 1})
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
			gh := behindMergeGateClient(github.CompareResult{Status: "diverged", BehindBy: 4, AheadBy: 1})
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

// Conflict is the real reason to update, and it is reported by `mergeable`, not
// by the compare status. A diverged head that GitHub says does not merge keeps
// the mandatory update - the pre-#1865 behaviour - because merging the reviewed
// head is not available.
func TestMergeGateStillUpdatesDivergedConflictingBranch(t *testing.T) {
	gh := behindMergeGateClient(github.CompareResult{Status: "diverged", BehindBy: 4, AheadBy: 1})
	conflicting := false
	gh.pr.Mergeable = &conflicting
	gh.strictKnown = true
	gh.strictBase = false

	decision := evaluateBehindMergeGate(t, gh)

	if decision.Merged {
		t.Fatalf("a conflicting head must not merge: %+v", decision)
	}
	if len(gh.updates) != 1 {
		t.Fatalf("update inputs = %+v", gh.updates)
	}
}

// UNKNOWN MERGEABILITY FAILS CLOSED. GitHub computes mergeability
// asynchronously, so a PR read moments after a base move returns null, and a
// token that cannot see it returns null too. Treating null as "fine" would merge
// on the strength of a value GitHub has not produced.
func TestMergeGateStillUpdatesWhenMergeabilityIsUnknown(t *testing.T) {
	gh := behindMergeGateClient(github.CompareResult{Status: "diverged", BehindBy: 4, AheadBy: 1})
	gh.pr.Mergeable = nil
	gh.strictKnown = true
	gh.strictBase = false

	decision := evaluateBehindMergeGate(t, gh)

	if decision.Merged {
		t.Fatalf("unknown mergeability must not merge: %+v", decision)
	}
	if len(gh.updates) != 1 {
		t.Fatalf("update inputs = %+v", gh.updates)
	}
}

// Acceptance 3: the success case. A branch already up to date advances exactly
// as before - the new guard must not reject valid input, and must not spend an
// API call it does not need.
func TestMergeGateMergesUpToDateBranchUnchanged(t *testing.T) {
	// #2074 round two, P2: the blank status is GONE from this ACCEPTANCE table.
	// GitHub's compare status enum is diverged/ahead/behind/identical, so "" is
	// not a production shape, and an acceptance test on an unreachable input can
	// stay green through a real regression. The empty and unrecognised cases are
	// still covered deliberately, as ROBUSTNESS, by
	// TestMergeGateTreatsNumericBehindAsBehindWhateverTheStatusString below.
	for _, status := range []string{"identical", "ahead"} {
		t.Run("status="+status, func(t *testing.T) {
			// A real "ahead" response carries ahead_by > 0; "identical" carries
			// zero on both axes, and passing AheadBy here would itself be a shape
			// production cannot emit. So it is set per status rather than blanket.
			compare := github.CompareResult{Status: status}
			if status == "ahead" {
				compare.AheadBy = 3
			}
			gh := behindMergeGateClient(compare)
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

// #1870 review finding 1 (P2): `compare.BehindBy > 0` in the guard at
// merge_gate.go:1503 was a SURVIVING mutant - deleting it left the whole
// internal/workflow suite green, so nothing pinned the numeric behind-check and
// a later edit could delete it unnoticed. Without that term a compare response
// carrying BehindBy > 0 under an unexpected status string falls through to
// `return MergeDecision{}, false, nil`: merged with NO protection read and NO
// update, which is worse than the pre-#1865 behaviour.
//
// These cases pass ONLY while the numeric check is present.
func TestMergeGateTreatsNumericBehindAsBehindWhateverTheStatusString(t *testing.T) {
	// "" and "unknown" are the DISCRIMINATING cases: they kill the mutant.
	// "BEHIND" does NOT - the guard lowercases the status, so that arm matches
	// the string check with or without the numeric term. It is kept as coverage
	// of the case-folding, not as part of the kill.
	for _, status := range []string{"", "unknown", "BEHIND"} {
		t.Run("status="+status, func(t *testing.T) {
			t.Run("protection allows behind merges", func(t *testing.T) {
				gh := behindMergeGateClient(github.CompareResult{Status: status, BehindBy: 3})
				gh.strictKnown = true
				gh.strictBase = false

				decision := evaluateBehindMergeGate(t, gh)

				// The numeric term is what routes this into the behind branch at
				// all. Drop it and protection is never consulted.
				if gh.strictCalls == 0 {
					t.Fatal("BehindBy > 0 must route through the behind branch and consult protection")
				}
				if !decision.Merged || len(gh.updates) != 0 {
					t.Fatalf("decision = %+v updates = %+v", decision, gh.updates)
				}
				if len(gh.merges) != 1 || gh.merges[0].MatchHeadCommit != "head123" {
					t.Fatalf("merge must stay fenced to the reviewed head: %+v", gh.merges)
				}
			})

			t.Run("protection undetermined still updates", func(t *testing.T) {
				gh := behindMergeGateClient(github.CompareResult{Status: status, BehindBy: 3})
				gh.strictKnown = false

				decision := evaluateBehindMergeGate(t, gh)

				if decision.Merged {
					t.Fatalf("numeric-behind head must not merge on an undetermined read: %+v", decision)
				}
				if len(gh.updates) != 1 || gh.updates[0].ExpectedHeadSHA != "head123" {
					t.Fatalf("update inputs = %+v", gh.updates)
				}
			})
		})
	}
}

// #2074 review, P1. UNKNOWN MERGEABILITY MUST NOT REACH THE NATIVE MERGE, and
// the hole was outside the behind branch entirely: for an already up-to-date
// comparison ensureBranchFresh returns unhandled, so the behind-branch guard
// never runs, and the old test `Mergeable != nil && !*Mergeable` was false for
// nil. Evaluate then merged on mergeability GitHub had not computed.
//
// This drives Evaluate with the shape that exposes it - up to date AND nil -
// which no fixture in this file previously produced.
func TestMergeGateWaitsWhenMergeabilityIsUnknownOnAnUpToDateHead(t *testing.T) {
	for _, status := range []string{"ahead", "identical"} {
		t.Run("status="+status, func(t *testing.T) {
			// #2074 round three, P2: AheadBy is set PER STATUS. "identical" reports
			// ahead_by=0 and behind_by=0 - confirmed against an exact-commit
			// comparison - so a blanket positive value here is a tuple the API
			// cannot emit. The adjacent acceptance test already did this
			// conditionally and I wrote the blanket version anyway, in the same
			// file, one round later.
			compare := github.CompareResult{Status: status}
			if status == "ahead" {
				compare.AheadBy = 2
			}
			gh := behindMergeGateClient(compare)
			gh.pr.Mergeable = nil
			gh.strictKnown = true
			gh.strictBase = true

			decision := evaluateBehindMergeGate(t, gh)

			if decision.Merged {
				t.Fatalf("merged with mergeability GitHub has not determined: %+v", decision)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("the native merge was reached: %+v", gh.merges)
			}
			if !strings.Contains(decision.Reason.Render(), "has not determined") {
				t.Fatalf("decision must say mergeability is undetermined, got %q", decision.Reason.Render())
			}
			// PENDING, NOT BLOCKED, ASSERTED ON THE LIFECYCLE RATHER THAN THE PROSE
			// (#2074 round two, P2). The reviewer changed the production nil arm to
			// g.block with the IDENTICAL reason string and this test still passed,
			// because it only checked that no merge happened and that the reason
			// mentioned undetermined mergeability. Both are true of a block.
			//
			// The inversion matters: a blocked row publishes a FAILURE status and
			// moves the task out of its automatic retry path, turning a race that
			// resolves on the next poll into an operator ticket. So the three
			// distinguishers are asserted directly.
			if !decision.Ready {
				t.Fatal("nil mergeability produced a BLOCK, not a pending: Ready is false, so the task leaves its automatic retry path")
			}
			if decision.BlockClass != 0 {
				t.Fatalf("a pending decision must carry no block class, got %d", decision.BlockClass)
			}
			if !hasStatus(gh.statuses, GitmootMergeGateContext, "pending") {
				t.Fatalf("the published commit status must be pending, got %+v", gh.statuses)
			}
		})
	}
}

// The explicit false case must stay a BLOCK, so the nil arm above cannot be
// implemented by softening a real conflict into a wait.
func TestMergeGateBlocksAnExplicitlyUnmergeableHead(t *testing.T) {
	gh := behindMergeGateClient(github.CompareResult{Status: "ahead", AheadBy: 2})
	conflicting := false
	gh.pr.Mergeable = &conflicting
	gh.strictKnown = true
	gh.strictBase = true

	decision := evaluateBehindMergeGate(t, gh)

	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("a conflicting head must not merge: %+v", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "not mergeable") {
		t.Fatalf("decision must name the conflict, got %q", decision.Reason.Render())
	}
	// The opposite lifecycle from the nil case above. Without this the pair could
	// both be satisfied by one behaviour, which is exactly how the nil test came
	// to accept a block.
	if decision.Ready {
		t.Fatal("an explicit conflict must BLOCK, not wait: Ready is true, so it would keep retrying a merge GitHub has refused")
	}
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "failure") {
		t.Fatalf("a blocked decision must publish a failure status, got %+v", gh.statuses)
	}
}
