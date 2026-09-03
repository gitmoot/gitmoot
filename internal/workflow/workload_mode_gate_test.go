package workflow

import (
	"context"
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
