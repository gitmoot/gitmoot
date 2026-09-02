package workflow

import (
	"context"
	"fmt"
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
