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
