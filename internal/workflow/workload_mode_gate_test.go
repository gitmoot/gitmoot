package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

func newWorkloadModeGateScenario(t *testing.T) (*db.Store, *fakeMergeGateGitHub, PolicyMergeGate, MergeRequest) {
	t.Helper()
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "mode-review", agent: "reviewer", headSHA: "head123", decision: "approved", hasResult: true,
	})
	gh.files = []github.PullRequestFile{{
		Filename: "AGENTS.md",
		Patch:    "@@ -1 +1 @@\n-**Current mode: DRAIN.**\n+**Current mode: STEADY.**",
	}}
	return store, gh, gate, request
}

func insertOperatingModeDecision(t *testing.T, store *db.Store, mode string) db.WorkflowNote {
	t.Helper()
	note, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "gitmoot/worktree-lifecycle",
		Author:     "owner",
		Repo:       "gitmoot/gitmoot",
		Body:       fmt.Sprintf("[operating-mode repo=gitmoot/gitmoot mode=%s]", mode),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowNote decision: %v", err)
	}
	return note
}

func insertModeReconciliation(t *testing.T, store *db.Store, decision db.WorkflowNote, mode, head string) db.WorkflowNote {
	t.Helper()
	note, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "gitmoot/worktree-lifecycle",
		Author:     "coordinator",
		Repo:       "gitmoot/gitmoot",
		Body: fmt.Sprintf("[workload-mode-reconciliation repo=gitmoot/gitmoot pr=9 head=%s mode=%s decision_note=%d]",
			head, mode, decision.ID),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowNote reconciliation: %v", err)
	}
	return note
}

func TestPolicyMergeGateRequiresExactModeReconciliation(t *testing.T) {
	t.Run("missing reconciliation defers before merge", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		insertOperatingModeDecision(t, store, "STEADY")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !decision.Ready || decision.Merged || len(gh.merges) != 0 || !strings.Contains(decision.Reason.Render(), "requires reconciliation") {
			t.Fatalf("decision=%+v merges=%d", decision, len(gh.merges))
		}
	})

	t.Run("matching exact-head reconciliation merges", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		mode := insertOperatingModeDecision(t, store, "STEADY")
		insertModeReconciliation(t, store, mode, "STEADY", "head123")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !decision.Merged || len(gh.merges) != 1 {
			t.Fatalf("decision=%+v merges=%d", decision, len(gh.merges))
		}
	})

	t.Run("later operating-mode decision invalidates reconciliation", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		mode := insertOperatingModeDecision(t, store, "STEADY")
		insertModeReconciliation(t, store, mode, "STEADY", "head123")
		insertOperatingModeDecision(t, store, "DRAIN")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !decision.Ready || decision.Merged || len(gh.merges) != 0 {
			t.Fatalf("decision=%+v merges=%d", decision, len(gh.merges))
		}
	})
}

func TestPipelineAutoMergerRechecksModeReconciliationAtMergeBoundary(t *testing.T) {
	store, gh, _, _ := newWorkloadModeGateScenario(t)
	mode := insertOperatingModeDecision(t, store, "STEADY")
	insertModeReconciliation(t, store, mode, "STEADY", "head123")
	merger := PipelineAutoMerger{Store: store, GitHub: gh}
	request := PipelineAutoMergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", Pipeline: "release", RunID: "run-1", StageID: "merge"}

	readiness, err := merger.Evaluate(context.Background(), request)
	if err != nil || !readiness.Ready {
		t.Fatalf("Evaluate readiness=%+v err=%v", readiness, err)
	}
	insertOperatingModeDecision(t, store, "DRAIN")

	result, err := merger.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.Merged || len(gh.merges) != 0 || !strings.Contains(result.Reason, "requires reconciliation") {
		t.Fatalf("result=%+v merges=%d", result, len(gh.merges))
	}
}

func TestModeSensitivePullRequestFailsClosedOnMissingPatch(t *testing.T) {
	gh := &fakeMergeGateGitHub{files: []github.PullRequestFile{{Filename: "AGENTS.md"}}}
	got, err := inspectModeSensitivePullRequest(context.Background(), gh, github.Repository{Owner: "gitmoot", Name: "gitmoot"}, 9)
	if err != nil {
		t.Fatalf("inspectModeSensitivePullRequest: %v", err)
	}
	if !got.required || !got.ambiguous {
		t.Fatalf("inspection = %+v, want required ambiguous reconciliation", got)
	}
}

// insertRawModeReconciliation writes a reconciliation row with an arbitrary
// decision_note value, including the documented literal "none".
func insertRawModeReconciliation(t *testing.T, store *db.Store, mode, head, decisionNote string) db.WorkflowNote {
	t.Helper()
	note, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "gitmoot/worktree-lifecycle",
		Author:     "coordinator",
		Repo:       "gitmoot/gitmoot",
		Body: fmt.Sprintf("[workload-mode-reconciliation repo=gitmoot/gitmoot pr=9 head=%s mode=%s decision_note=%s]",
			head, mode, decisionNote),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowNote reconciliation: %v", err)
	}
	return note
}

// AGENTS.md documents `decision_note=none` for a PR-SOURCED decision, where the
// PR itself is the decision. That value used to be matchable only while the repo
// had zero operating-mode notes, so from the first typed note onward every row
// written per the documented instruction was skipped and the PR held forever
// with no stated cause (#1783 review, P2).
func TestWorkloadModeGateAcceptsDocumentedPRSourcedDecisionNote(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	insertOperatingModeDecision(t, store, "DRAIN")
	insertRawModeReconciliation(t, store, "STEADY", "head123", "none")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.Merged || len(gh.merges) != 1 {
		t.Fatalf("a PR-sourced reconciliation must merge: decision=%+v merges=%d reason=%q",
			decision, len(gh.merges), decision.Reason.Render())
	}
}

// A PR-sourced row still cannot ratify a SUPERSEDED decision: an owner note
// landing after the row invalidates it, exactly as for a row citing an id.
func TestWorkloadModeGateRejectsSupersededPRSourcedRow(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	insertRawModeReconciliation(t, store, "STEADY", "head123", "none")
	insertOperatingModeDecision(t, store, "DRAIN")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("a superseded PR-sourced row must not merge: decision=%+v merges=%d", decision, len(gh.merges))
	}
	if reason := decision.Reason.Render(); !strings.Contains(reason, "predates operating-mode note") {
		t.Fatalf("hold reason must name the supersession: %q", reason)
	}
}

// Citing a decision note proved only that someone READ it. With the newest note
// deciding STEADY, a PR flipping the marker to THROUGHPUT merged cleanly by
// citing that STEADY note. A row may only ratify the mode its decision decided.
func TestWorkloadModeGateRejectsRowCitingADisagreeingDecision(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	gh.files = []github.PullRequestFile{{
		Filename: "AGENTS.md",
		Patch:    "@@ -1 +1 @@\n-**Current mode: DRAIN.**\n+**Current mode: THROUGHPUT.**",
	}}
	decisionNote := insertOperatingModeDecision(t, store, "STEADY")
	insertRawModeReconciliation(t, store, "THROUGHPUT", "head123", strconv.FormatInt(decisionNote.ID, 10))

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("a row citing a disagreeing decision must not merge: decision=%+v merges=%d", decision, len(gh.merges))
	}
	if reason := decision.Reason.Render(); !strings.Contains(reason, "cannot ratify") {
		t.Fatalf("hold reason must say the row cannot ratify that mode: %q", reason)
	}
}

// The hold used to say only "requires reconciliation", and the native path holds
// through g.pending, which retries silently. A coordinator who HAD written a row
// therefore saw an indefinitely pending PR with nothing pointing at the cause.
func TestWorkloadModeGateHoldNamesTheMismatchedRow(t *testing.T) {
	store, _, gate, request := newWorkloadModeGateScenario(t)
	newest := insertOperatingModeDecision(t, store, "STEADY")
	insertRawModeReconciliation(t, store, "STEADY", "head123", "999999")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	reason := decision.Reason.Render()
	if !strings.Contains(reason, "cites decision_note=999999") ||
		!strings.Contains(reason, strconv.FormatInt(newest.ID, 10)) {
		t.Fatalf("hold reason must name the row and the newest note: %q", reason)
	}
}

// Enforcement is documented against the gitmoot/* scope, and the gate compared
// FullName to the literal "gitmoot/gitmoot", so a mode-marker change in any
// other gitmoot repository merged with no reconciliation at all (#1783 review,
// P3).
func TestModeSensitivePullRequestCoversTheDocumentedOwnerScope(t *testing.T) {
	patch := "@@ -1 +1 @@\n-**Current mode: DRAIN.**\n+**Current mode: STEADY.**"
	for name, test := range map[string]struct {
		repo         github.Repository
		wantRequired bool
	}{
		"flagship":        {repo: github.Repository{Owner: "gitmoot", Name: "gitmoot"}, wantRequired: true},
		"sibling in org":  {repo: github.Repository{Owner: "gitmoot", Name: "gitmoot-dashboard"}, wantRequired: true},
		"outside the org": {repo: github.Repository{Owner: "jerryfane", Name: "joltra"}, wantRequired: false},
	} {
		t.Run(name, func(t *testing.T) {
			gh := &fakeMergeGateGitHub{files: []github.PullRequestFile{{Filename: "AGENTS.md", Patch: patch}}}
			got, err := inspectModeSensitivePullRequest(context.Background(), gh, test.repo, 9)
			if err != nil {
				t.Fatalf("inspectModeSensitivePullRequest: %v", err)
			}
			if got.required != test.wantRequired {
				t.Fatalf("required = %v, want %v for %s", got.required, test.wantRequired, test.repo.FullName())
			}
		})
	}
}

// The decision lookup is scoped to the repository IN SQL. Ordering across every
// repo and truncating was a fail-OPEN corner: losing this repo's decision note
// made the gate read "no decision exists", which dropped the recency check and
// let a stale PR-sourced row satisfy the gate (#1783 review, P3).
func TestWorkloadModeGateFindsTheDecisionBehindOtherReposNotes(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	decisionNote := insertOperatingModeDecision(t, store, "STEADY")
	// Bury it under more operating-mode notes than the scan window holds, all for
	// other repositories.
	for i := 0; i < workloadModeReconciliationScan+10; i++ {
		if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
			WorkflowID: "other/lane",
			Author:     "owner",
			Repo:       fmt.Sprintf("other/repo-%d", i),
			Body:       fmt.Sprintf("[operating-mode repo=other/repo-%d mode=DRAIN]", i),
		}); err != nil {
			t.Fatalf("InsertWorkflowNote filler: %v", err)
		}
	}
	insertRawModeReconciliation(t, store, "STEADY", "head123", strconv.FormatInt(decisionNote.ID, 10))

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.Merged || len(gh.merges) != 1 {
		t.Fatalf("the buried decision must still be found: decision=%+v merges=%d reason=%q",
			decision, len(gh.merges), decision.Reason.Render())
	}
}

// insertRawOperatingMode writes an operating-mode note with an arbitrary field
// list, including bodies the parser rejects.
func insertRawOperatingMode(t *testing.T, store *db.Store, repoColumn, body string) db.WorkflowNote {
	t.Helper()
	note, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "gitmoot/worktree-lifecycle",
		Author:     "owner",
		Repo:       repoColumn,
		Body:       body,
	})
	if err != nil {
		t.Fatalf("InsertWorkflowNote raw decision: %v", err)
	}
	return note
}

// An UNREADABLE newest decision is not "no decision". Skipping it returned
// id==0, which dropped the supersession check AND made decision_note=none
// satisfiable by a row written before the owner decided, so both halves of the
// mode guard failed together (#1783 review, F1).
func TestWorkloadModeGateHoldsWhenTheNewestDecisionIsUnreadable(t *testing.T) {
	for name, body := range map[string]string{
		"malformed field list": "[operating-mode repo=gitmoot/gitmoot mode=DRAIN urgent]",
		"unknown mode":         "[operating-mode repo=gitmoot/gitmoot mode=PAUSED]",
	} {
		t.Run(name, func(t *testing.T) {
			store, gh, gate, request := newWorkloadModeGateScenario(t)
			// A row that WOULD satisfy the gate if the newest decision were absent.
			insertRawModeReconciliation(t, store, "STEADY", "head123", "none")
			unreadable := insertRawOperatingMode(t, store, "gitmoot/gitmoot", body)

			decision, err := gate.Evaluate(context.Background(), request)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if decision.Merged || len(gh.merges) != 0 {
				t.Fatalf("an unreadable decision must hold: decision=%+v merges=%d", decision, len(gh.merges))
			}
			rendered := decision.Reason.Render()
			if !strings.Contains(rendered, strconv.FormatInt(unreadable.ID, 10)) {
				t.Fatalf("hold must name the unreadable note %d: %q", unreadable.ID, rendered)
			}
		})
	}
}

// The fail-closed rule must not freeze repositories it cannot prove are
// affected: one typo in another lane's note would otherwise hold every
// gitmoot-owned repo, and a valid reconciliation must still merge.
func TestWorkloadModeGateIgnoresAnUnreadableNoteFromAnotherRepo(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	mode := insertOperatingModeDecision(t, store, "STEADY")
	insertModeReconciliation(t, store, mode, "STEADY", "head123")
	insertRawOperatingMode(t, store, "other/repo", "[operating-mode repo=other/repo mode=DRAIN urgent]")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.Merged || len(gh.merges) != 1 {
		t.Fatalf("another repo's malformed note must not hold this one: decision=%+v merges=%d reason=%q",
			decision, len(gh.merges), decision.Reason.Render())
	}
}

// The headline invariant is EXACT-HEAD reconciliation, and no test refused a row
// written at a stale head: every fixture used the current head, so deleting the
// head clause changed nothing (#1783 review, F5b). Same for the pr clause (F5c).
func TestWorkloadModeGateRefusesRowsForAnotherHeadOrPullRequest(t *testing.T) {
	t.Run("stale head", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		mode := insertOperatingModeDecision(t, store, "STEADY")
		stale := insertModeReconciliation(t, store, mode, "STEADY", "head000")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Merged || len(gh.merges) != 0 {
			t.Fatalf("a row at a stale head must not reconcile: decision=%+v merges=%d", decision, len(gh.merges))
		}
		rendered := decision.Reason.Render()
		if !strings.Contains(rendered, strconv.FormatInt(stale.ID, 10)) || !strings.Contains(rendered, "head") {
			t.Fatalf("hold must name the stale row and both heads: %q", rendered)
		}
	})

	t.Run("another pull request", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		decisionNote := insertOperatingModeDecision(t, store, "STEADY")
		other, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
			WorkflowID: "gitmoot/worktree-lifecycle",
			Author:     "coordinator",
			Repo:       "gitmoot/gitmoot",
			Body: fmt.Sprintf("[workload-mode-reconciliation repo=gitmoot/gitmoot pr=77 head=head123 mode=STEADY decision_note=%d]",
				decisionNote.ID),
		})
		if err != nil {
			t.Fatalf("InsertWorkflowNote: %v", err)
		}

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Merged || len(gh.merges) != 0 {
			t.Fatalf("another PR's row must not reconcile this one: decision=%+v merges=%d", decision, len(gh.merges))
		}
		if rendered := decision.Reason.Render(); !strings.Contains(rendered, strconv.FormatInt(other.ID, 10)) {
			t.Fatalf("hold must name the row that reconciles another PR: %q", rendered)
		}
	})
}

// The row's mode must equal the mode the PR ADDS. On the decision_note=none
// path that is the ONLY binding constraint, so with it deleted a PR-sourced row
// could ratify a marker change it disagreed with (#1783 review, F5a).
func TestWorkloadModeGateRefusesAPRSourcedRowThatDisagreesWithTheMarker(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	// The scenario's patch adds "**Current mode: STEADY.**"; the row says DRAIN.
	row := insertRawModeReconciliation(t, store, "DRAIN", "head123", "none")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("a PR-sourced row must still match the marker: decision=%+v merges=%d", decision, len(gh.merges))
	}
	rendered := decision.Reason.Render()
	if !strings.Contains(rendered, strconv.FormatInt(row.ID, 10)) || !strings.Contains(rendered, "STEADY") {
		t.Fatalf("hold must name the row and the mode the PR adds: %q", rendered)
	}
}

// When the marker patch is AMBIGUOUS the expected mode falls back to the newest
// decision, so a row must agree with that decision. Nothing exercised the
// fallback, so deleting it changed no test (#1783 review, F5d).
func TestWorkloadModeGateFallsBackToTheDecisionModeForAnAmbiguousPatch(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	// Marker lines change on BOTH sides with no readable added mode: ambiguous.
	gh.files = []github.PullRequestFile{{
		Filename: "AGENTS.md",
		Patch:    "@@ -1 +1 @@\n-**Current mode: DRAIN.**\n+**Current mode: SOMETHING.**",
	}}
	insertOperatingModeDecision(t, store, "STEADY")
	row := insertRawModeReconciliation(t, store, "DRAIN", "head123", "none")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("an ambiguous patch must still be held against the decision mode: decision=%+v merges=%d", decision, len(gh.merges))
	}
	if rendered := decision.Reason.Render(); !strings.Contains(rendered, strconv.FormatInt(row.ID, 10)) {
		t.Fatalf("hold must name the disagreeing row: %q", rendered)
	}
}

// GitHub treats owner/repo case insensitively, so a byte comparison on the owner
// silently disabled the whole gate for a repo recorded as "Gitmoot" (#1783
// review, F3).
func TestModeSensitivePullRequestIgnoresOwnerCase(t *testing.T) {
	gh := &fakeMergeGateGitHub{files: []github.PullRequestFile{{
		Filename: "AGENTS.md",
		Patch:    "@@ -1 +1 @@\n-**Current mode: DRAIN.**\n+**Current mode: STEADY.**",
	}}}
	got, err := inspectModeSensitivePullRequest(context.Background(), gh, github.Repository{Owner: "Gitmoot", Name: "gitmoot"}, 9)
	if err != nil {
		t.Fatalf("inspectModeSensitivePullRequest: %v", err)
	}
	if !got.required || got.mode != "STEADY" {
		t.Fatalf("inspection = %+v, want a required STEADY reconciliation for owner \"Gitmoot\"", got)
	}
}

// A note whose repo COLUMN differs only by case must still be visible on both
// note streams; the byte-equal SQL filter dropped it and took the fail-open
// path with it (#1783 review, F3).
func TestWorkloadModeGateFindsNotesRecordedWithDifferentRepoCase(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	decisionNote := insertRawOperatingMode(t, store, "Gitmoot/Gitmoot", "[operating-mode repo=Gitmoot/Gitmoot mode=STEADY]")
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "gitmoot/worktree-lifecycle",
		Author:     "coordinator",
		Repo:       "Gitmoot/Gitmoot",
		Body: fmt.Sprintf("[workload-mode-reconciliation repo=Gitmoot/Gitmoot pr=9 head=head123 mode=STEADY decision_note=%d]",
			decisionNote.ID),
	}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.Merged || len(gh.merges) != 1 {
		t.Fatalf("case-different notes must reconcile: decision=%+v merges=%d reason=%q",
			decision, len(gh.merges), decision.Reason.Render())
	}
}

// The merge-boundary recheck must produce a WAITING disposition, not a terminal
// one: the pipeline folds any !Merged into a blocked stage with "retry
// stopped", so an owner note landing in the Evaluate->Merge window ended the run
// and needed a human where the pre-merge path simply waited (#1783 review, F4).
func TestPipelineAutoMergerHoldsMergeBoundaryAsWaiting(t *testing.T) {
	store, gh, _, _ := newWorkloadModeGateScenario(t)
	mode := insertOperatingModeDecision(t, store, "STEADY")
	insertModeReconciliation(t, store, mode, "STEADY", "head123")
	merger := PipelineAutoMerger{Store: store, GitHub: gh}
	request := PipelineAutoMergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", Pipeline: "release", RunID: "run-1", StageID: "merge"}
	insertOperatingModeDecision(t, store, "DRAIN")

	result, err := merger.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.Merged || len(gh.merges) != 0 {
		t.Fatalf("result=%+v merges=%d, want no merge", result, len(gh.merges))
	}
	if !result.Waiting {
		t.Fatalf("a reconciliation hold must be retryable, not terminal: result=%+v", result)
	}

	// A head drift at the merge boundary stays TERMINAL: the reviewed head is
	// gone, so no amount of waiting makes this request mergeable.
	gh.pr.HeadSHA = "head999"
	drift, err := merger.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("Merge after drift: %v", err)
	}
	if drift.Merged || drift.Waiting {
		t.Fatalf("head drift must not be retryable: result=%+v", drift)
	}
}

// A row recorded under ANOTHER repo column is invisible to the repo-scoped
// lookup, so the coordinator who filed it against the wrong workflow saw only
// the generic hold with no cause (#1783 review, F2).
func TestWorkloadModeGateHoldNamesAMisfiledReconciliationRow(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	decisionNote := insertOperatingModeDecision(t, store, "STEADY")
	misfiled, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "other/lane",
		Author:     "coordinator",
		Repo:       "other/repo",
		Body: fmt.Sprintf("[workload-mode-reconciliation repo=gitmoot/gitmoot pr=9 head=head123 mode=STEADY decision_note=%d]",
			decisionNote.ID),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("a misfiled row must not reconcile: decision=%+v merges=%d", decision, len(gh.merges))
	}
	rendered := decision.Reason.Render()
	if !strings.Contains(rendered, strconv.FormatInt(misfiled.ID, 10)) || !strings.Contains(rendered, "other/repo") {
		t.Fatalf("hold must name the misfiled row and the repo it was recorded under: %q", rendered)
	}
}

// Directive 110704 asked whether the fail-closed rule can WEDGE a legitimately
// reconciled PR. Holding unconditionally did: one malformed note froze every
// mode-marker PR in the repo until someone edited an append-only journal, and
// the coordinator's own remedy - a fresh exact-head row - could not clear it.
// An unreadable note is therefore a RECENCY BOUNDARY, not a veto.
func TestWorkloadModeGateUnreadableDecisionIsABoundaryNotAWedge(t *testing.T) {
	t.Run("a row filed after the unreadable note reconciles", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		insertRawOperatingMode(t, store, "gitmoot/gitmoot", "[operating-mode repo=gitmoot/gitmoot mode=DRAIN urgent]")
		// The PR's marker adds STEADY, and the row agrees with the PR.
		insertRawModeReconciliation(t, store, "STEADY", "head123", "none")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !decision.Merged || len(gh.merges) != 1 {
			t.Fatalf("a fresh row must clear an unreadable note: decision=%+v merges=%d reason=%q",
				decision, len(gh.merges), decision.Reason.Render())
		}
	})

	t.Run("a row citing the unreadable note reconciles", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		unreadable := insertRawOperatingMode(t, store, "gitmoot/gitmoot", "[operating-mode repo=gitmoot/gitmoot mode=PAUSED]")
		insertRawModeReconciliation(t, store, "STEADY", "head123", strconv.FormatInt(unreadable.ID, 10))

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !decision.Merged || len(gh.merges) != 1 {
			t.Fatalf("a row naming the unreadable note must reconcile: decision=%+v merges=%d reason=%q",
				decision, len(gh.merges), decision.Reason.Render())
		}
	})

	t.Run("a row that predates the unreadable note is held and told what to do", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		stale := insertRawModeReconciliation(t, store, "STEADY", "head123", "none")
		unreadable := insertRawOperatingMode(t, store, "gitmoot/gitmoot", "[operating-mode repo=gitmoot/gitmoot mode=PAUSED]")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Merged || len(gh.merges) != 0 {
			t.Fatalf("a row older than the unreadable note must not reconcile: decision=%+v merges=%d", decision, len(gh.merges))
		}
		rendered := decision.Reason.Render()
		for _, want := range []string{
			strconv.FormatInt(stale.ID, 10),
			strconv.FormatInt(unreadable.ID, 10),
			"file a new exact-head row",
		} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("hold must name both notes and the remedy %q: %q", want, rendered)
			}
		}
	})

	t.Run("an unreadable note plus an unreadable marker is genuinely unknowable", func(t *testing.T) {
		store, gh, gate, request := newWorkloadModeGateScenario(t)
		gh.files = []github.PullRequestFile{{
			Filename: "AGENTS.md",
			Patch:    "@@ -1 +1 @@\n-**Current mode: DRAIN.**\n+**Current mode: SOMETHING.**",
		}}
		unreadable := insertRawOperatingMode(t, store, "gitmoot/gitmoot", "[operating-mode repo=gitmoot/gitmoot mode=PAUSED]")
		insertRawModeReconciliation(t, store, "STEADY", "head123", "none")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Merged || len(gh.merges) != 0 {
			t.Fatalf("nothing readable remains, so the gate must hold: decision=%+v merges=%d", decision, len(gh.merges))
		}
		rendered := decision.Reason.Render()
		if !strings.Contains(rendered, strconv.FormatInt(unreadable.ID, 10)) || !strings.Contains(rendered, "no readable decision remains") {
			t.Fatalf("hold must say why nothing can be checked: %q", rendered)
		}
	})
}

// GitHub OMITS the patch for large files, and AGENTS.md is large, so the
// refusal used to tell the author to fix a marker that was never the problem
// and name no exit they could take (#1783 round-3 review, F-B).
func TestWorkloadModeGateNamesAReachableRemedyWhenGitHubOmitsThePatch(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	gh.files = []github.PullRequestFile{{Filename: "AGENTS.md"}} // patch omitted by the API
	unreadable := insertRawOperatingMode(t, store, "gitmoot/gitmoot", "[operating-mode repo=gitmoot/gitmoot mode=PAUSED]")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("an unreadable note plus an unreadable marker must hold: decision=%+v merges=%d", decision, len(gh.merges))
	}
	rendered := decision.Reason.Render()
	for _, want := range []string{
		strconv.FormatInt(unreadable.ID, 10),
		"GitHub omitted this PR's AGENTS.md patch",
		"not the author's to fix",
		"append a fresh readable operating-mode note",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("hold must contain %q: %q", want, rendered)
		}
	}

	// And the owner-level exit actually works: a fresh readable note clears it.
	insertOperatingModeDecision(t, store, "STEADY")
	insertRawModeReconciliation(t, store, "STEADY", "head123", "none")
	cleared, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate after the fresh note: %v", err)
	}
	if !cleared.Merged || len(gh.merges) != 1 {
		t.Fatalf("the named remedy did not work: decision=%+v merges=%d reason=%q",
			cleared, len(gh.merges), cleared.Reason.Render())
	}
}

// A valid row aged out of the 200-row window used to leave the hold blaming an
// unrelated near-miss row, which reads as a verdict on the operator's own work
// (#1783 round-3 review, F-C). The window is recoverable by refiling, so this
// is a message fix - the hold must SAY the window was full.
func TestWorkloadModeGateHoldNamesTheScanWindowWhenItIsFull(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	mode := insertOperatingModeDecision(t, store, "STEADY")
	insertModeReconciliation(t, store, mode, "STEADY", "head123")
	for i := range workloadModeReconciliationScan + 5 {
		if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
			WorkflowID: "gitmoot/worktree-lifecycle",
			Author:     "coordinator",
			Repo:       "gitmoot/gitmoot",
			Body: fmt.Sprintf("[workload-mode-reconciliation repo=gitmoot/gitmoot pr=%d head=otherhead mode=STEADY decision_note=%d]",
				1000+i, mode.ID),
		}); err != nil {
			t.Fatalf("InsertWorkflowNote filler %d: %v", i, err)
		}
	}

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("an aged-out row cannot reconcile: decision=%+v merges=%d", decision, len(gh.merges))
	}
	if rendered := decision.Reason.Render(); !strings.Contains(rendered, "may have aged out") {
		t.Fatalf("hold must name the full scan window rather than only a near-miss row: %q", rendered)
	}
}

// `workflow note` is written by hand, so an omitted --repo left a malformed
// operating-mode note invisible to the boundary and a stale PR-sourced row
// still reconciled (#1783 round-3 review, F-D).
func TestWorkloadModeGateBoundaryCatchesAMalformedNoteWithNoRepoColumn(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	stale := insertRawModeReconciliation(t, store, "STEADY", "head123", "none")
	unreadable := insertRawOperatingMode(t, store, "", "[operating-mode repo=gitmoot/gitmoot mode=DRAIN urgent]")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("a repo-less malformed note must still supersede: decision=%+v merges=%d reason=%q",
			decision, len(gh.merges), decision.Reason.Render())
	}
	rendered := decision.Reason.Render()
	if !strings.Contains(rendered, strconv.FormatInt(stale.ID, 10)) || !strings.Contains(rendered, strconv.FormatInt(unreadable.ID, 10)) {
		t.Fatalf("hold must name the stale row and the note that superseded it: %q", rendered)
	}

	// A malformed note that names ANOTHER repo, with no repo column, must still
	// be ignored: the anti-wedge narrowing is the point.
	other, _, otherGate, otherRequest := newWorkloadModeGateScenario(t)
	otherMode := insertOperatingModeDecision(t, other, "STEADY")
	insertModeReconciliation(t, other, otherMode, "STEADY", "head123")
	insertRawOperatingMode(t, other, "", "[operating-mode repo=other/repo mode=DRAIN urgent]")
	if decision, err := otherGate.Evaluate(context.Background(), otherRequest); err != nil {
		t.Fatalf("Evaluate: %v", err)
	} else if !decision.Merged {
		t.Fatalf("another repo's repo-less malformed note must not hold this one: %q", decision.Reason.Render())
	}
}

// The remedy must be COMPLETE, and this test applies it in the two documented
// steps SEPARATELY. The round-4 review measured that the note alone leaves
// merged=false, and that the earlier test masked it by filing a row at the same
// time (F-7). It also pins patchUnavailable in BOTH directions: a hold for a
// patch GitHub DID return must not claim the API omitted it (F-5).
func TestWorkloadModeGateStatesTheWholeRemedyAndOnlyBlamesTheAPIWhenItIsAtFault(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	gh.files = []github.PullRequestFile{{Filename: "AGENTS.md"}} // patch omitted
	insertRawOperatingMode(t, store, "gitmoot/gitmoot", "[operating-mode repo=gitmoot/gitmoot mode=PAUSED]")

	first, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !strings.Contains(first.Reason.Render(), "AND file an exact-head reconciliation row") {
		t.Fatalf("the hold must name BOTH steps: %q", first.Reason.Render())
	}

	// STEP ONE ONLY: a fresh readable note. This must NOT merge by itself - that
	// is what the incomplete remedy text implied.
	insertOperatingModeDecision(t, store, "STEADY")
	step1, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate after step one: %v", err)
	}
	if step1.Merged || len(gh.merges) != 0 {
		t.Fatalf("the note alone must not clear the hold: decision=%+v merges=%d", step1, len(gh.merges))
	}

	// STEP TWO: the exact-head row. Now it merges.
	insertRawModeReconciliation(t, store, "STEADY", "head123", "none")
	step2, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate after step two: %v", err)
	}
	if !step2.Merged || len(gh.merges) != 1 {
		t.Fatalf("both steps together must clear the hold: decision=%+v merges=%d reason=%q",
			step2, len(gh.merges), step2.Reason.Render())
	}

	// A hold for a patch GitHub DID return must not blame the API.
	other, otherGH, otherGate, otherRequest := newWorkloadModeGateScenario(t)
	insertRawOperatingMode(t, other, "gitmoot/gitmoot", "[operating-mode repo=gitmoot/gitmoot mode=PAUSED]")
	otherGH.files = []github.PullRequestFile{{
		Filename: "AGENTS.md",
		Patch:    "@@ -1 +1 @@\n-**Current mode: DRAIN.**\n+**Current mode: SOMETHING.**",
	}}
	held, err := otherGate.Evaluate(context.Background(), otherRequest)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if strings.Contains(held.Reason.Render(), "GitHub omitted") {
		t.Fatalf("a returned patch must not be reported as omitted by GitHub: %q", held.Reason.Render())
	}
}

// The scan-window notice must fire only when the window is actually FULL: a
// mutant relaxing the condition to '>= 0' printed it on every hold and survived
// (#1783 round-4 review, F-5).
func TestWorkloadModeGateDoesNotClaimAFullWindowWhenItIsNot(t *testing.T) {
	store, gh, gate, request := newWorkloadModeGateScenario(t)
	insertOperatingModeDecision(t, store, "STEADY")

	decision, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("no row exists, so this must hold: decision=%+v merges=%d", decision, len(gh.merges))
	}
	if strings.Contains(decision.Reason.Render(), "may have aged out") {
		t.Fatalf("an empty window must not be reported as full: %q", decision.Reason.Render())
	}
}

// The lenient repo scan must cover the shapes a HAND-WRITTEN note takes. The
// first version matched a bare `repo=x` token only, so a trailing comma or
// spaces around `=` still let a stale row merge (#1783 round-4 review, F-6).
func TestWorkloadModeGateBoundaryToleratesHandWrittenRepoFields(t *testing.T) {
	for name, body := range map[string]string{
		"trailing comma": "[operating-mode repo=gitmoot/gitmoot, mode=DRAIN urgent]",
		"spaced equals":  "[operating-mode repo = gitmoot/gitmoot mode=DRAIN urgent]",
		"trailing brace": "[operating-mode repo=gitmoot/gitmoot] mode=DRAIN urgent",
	} {
		t.Run(name, func(t *testing.T) {
			store, gh, gate, request := newWorkloadModeGateScenario(t)
			stale := insertRawModeReconciliation(t, store, "STEADY", "head123", "none")
			unreadable := insertRawOperatingMode(t, store, "", body)

			decision, err := gate.Evaluate(context.Background(), request)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if decision.Merged || len(gh.merges) != 0 {
				t.Fatalf("a repo-less malformed note must supersede the stale row: decision=%+v merges=%d reason=%q",
					decision, len(gh.merges), decision.Reason.Render())
			}
			rendered := decision.Reason.Render()
			if !strings.Contains(rendered, strconv.FormatInt(stale.ID, 10)) || !strings.Contains(rendered, strconv.FormatInt(unreadable.ID, 10)) {
				t.Fatalf("hold must name the stale row and the note that superseded it: %q", rendered)
			}
		})
	}

	// Still narrow: another repo's repo-less malformed note, in the same shapes,
	// must not hold this repository.
	for name, body := range map[string]string{
		"trailing comma": "[operating-mode repo=other/repo, mode=DRAIN urgent]",
		"spaced equals":  "[operating-mode repo = other/repo mode=DRAIN urgent]",
	} {
		t.Run("ignores "+name, func(t *testing.T) {
			store, gh, gate, request := newWorkloadModeGateScenario(t)
			mode := insertOperatingModeDecision(t, store, "STEADY")
			insertModeReconciliation(t, store, mode, "STEADY", "head123")
			insertRawOperatingMode(t, store, "", body)

			decision, err := gate.Evaluate(context.Background(), request)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if !decision.Merged || len(gh.merges) != 1 {
				t.Fatalf("another repo's malformed note must not hold this one: decision=%+v reason=%q",
					decision, decision.Reason.Render())
			}
		})
	}
}

// F-11 at the SOURCE. The pipeline keys a hold episode's budget on this key, so
// the key must not move when the operator-facing detail does. Driven through
// Evaluate, because a pipeline test that injects a key through a stub pins the
// stub, not the construction.
func TestPipelineAutoMergerReconciliationKeyIgnoresVolatileDetail(t *testing.T) {
	store, gh, _, _ := newWorkloadModeGateScenario(t)
	decision := insertOperatingModeDecision(t, store, "STEADY")
	// A row for the WRONG head: reconciliation still holds, and the hold's detail
	// names this row as the near miss.
	insertModeReconciliation(t, store, decision, "STEADY", "otherhead1")
	merger := PipelineAutoMerger{Store: store, GitHub: gh}
	request := PipelineAutoMergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", Pipeline: "release", RunID: "run-1", StageID: "merge"}

	first, err := merger.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !first.ReconciliationHold || strings.TrimSpace(first.ReconciliationKey) == "" {
		t.Fatalf("readiness = %+v, want a keyed reconciliation hold", first)
	}

	// Churn in the volatile half: the same hold, same head, same decision, but
	// the marker patch now reads as ambiguous, which appends a clause to the
	// detail. The DETAIL must move and the KEY must not.
	gh.files = []github.PullRequestFile{{
		Filename: "AGENTS.md",
		Patch:    "@@ -1 +1 @@\n-**Current mode: DRAIN.**",
	}}
	second, err := merger.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate after churn: %v", err)
	}
	if second.Reason == first.Reason {
		t.Fatalf("this fixture must change the detail, or it proves nothing: %q", second.Reason)
	}
	if second.ReconciliationKey != first.ReconciliationKey {
		t.Fatalf("episode key moved with the detail: %q then %q - external churn would reset the hold's budget",
			first.ReconciliationKey, second.ReconciliationKey)
	}
	// And the key must not simply embed the detail, which would key on churn
	// while comparing equal in this pair by accident.
	if strings.Contains(first.ReconciliationKey, "otherhead1") || strings.Contains(first.ReconciliationKey, "near") {
		t.Fatalf("episode key carries near-miss text: %q", first.ReconciliationKey)
	}
	for _, want := range []string{"head=head123", "decision-note=" + strconv.FormatInt(decision.ID, 10)} {
		if !strings.Contains(first.ReconciliationKey, want) {
			t.Fatalf("episode key %q must name %q", first.ReconciliationKey, want)
		}
	}
}

// Waiting carries a SAFETY PRECONDITION - no GitHub mutation was attempted -
// because the pipeline RELEASES its at-most-once merge claim on a Waiting
// return. The type says so, and a comment is not a guard: a mutant that mapped a
// post-request merge error onto {Waiting: true} passed internal/workflow and
// internal/pipeline fully green, because nothing pinned the mapping (#1783
// round-5 review, F-9). A merge API error may have reached GitHub, so it must
// surface as an ERROR, never as a releasable wait.
func TestPipelineAutoMergerMergeNeverMapsAMutationErrorToWaiting(t *testing.T) {
	store, gh, _, _ := newWorkloadModeGateScenario(t)
	mode := insertOperatingModeDecision(t, store, "STEADY")
	// Reconciliation satisfied, so Merge reaches the mutation rather than
	// returning the legitimate reconciliation Waiting before it.
	insertModeReconciliation(t, store, mode, "STEADY", "head123")
	mergeErr := errors.New("merge API timed out after the request was sent")
	gh.mergeErr = mergeErr
	merger := PipelineAutoMerger{Store: store, GitHub: gh}
	request := PipelineAutoMergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", Pipeline: "release", RunID: "run-1", StageID: "merge"}

	result, err := merger.Merge(context.Background(), request)
	if !errors.Is(err, mergeErr) {
		t.Fatalf("Merge error = %v, want the merge API error to surface", err)
	}
	if result.Waiting {
		t.Fatalf("a refusal that may have reached GitHub must not set Waiting: %+v", result)
	}
	if result.Merged {
		t.Fatalf("a failed merge must not report Merged: %+v", result)
	}
}

// The pipeline records and bounds the RECONCILIATION class specifically, so
// Evaluate must mark it. Without this, the class flag was set in production and
// asserted only through a pipeline stub, so deleting the production line changed
// no test (#1783 round-4 review, F-1's second half).
func TestPipelineAutoMergerEvaluateMarksTheReconciliationHold(t *testing.T) {
	store, gh, _, _ := newWorkloadModeGateScenario(t)
	insertOperatingModeDecision(t, store, "STEADY")
	merger := PipelineAutoMerger{Store: store, GitHub: gh}
	request := PipelineAutoMergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", Pipeline: "release", RunID: "run-1", StageID: "merge"}

	readiness, err := merger.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !readiness.Waiting || !readiness.ReconciliationHold {
		t.Fatalf("readiness = %+v, want a marked reconciliation hold", readiness)
	}
	if !strings.Contains(readiness.Reason, "requires reconciliation") {
		t.Fatalf("readiness reason = %q", readiness.Reason)
	}

	// A NON-reconciliation wait must NOT carry the class, or the pipeline would
	// bound CI waits too.
	pending, pendingGH, _, _ := newWorkloadModeGateScenario(t)
	mode := insertOperatingModeDecision(t, pending, "STEADY")
	insertModeReconciliation(t, pending, mode, "STEADY", "head123")
	pendingGH.pr.Mergeable = nil
	pendingMerger := PipelineAutoMerger{Store: pending, GitHub: pendingGH}
	pendingReadiness, err := pendingMerger.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate (mergeability unknown): %v", err)
	}
	if !pendingReadiness.Waiting || pendingReadiness.ReconciliationHold {
		t.Fatalf("a mergeability wait must not be a reconciliation hold: %+v", pendingReadiness)
	}
}
