package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2069 f1, raised by the review that APPROVED this PR and left open at its head.
//
// `state` beats `disposition` in firstNonEmptyLedgerText. That precedence is
// defensible and is unchanged. What was not defensible is that it was SILENT:
// every other reversal in findings_ledger_writer.go is audible (#1936 emits
// findings_ledger_downgraded), so a reviewer who wrote two disagreeing answers
// had no way to learn from the ledger which one was taken.
//
// Fails before the fix: zero findings_ledger_state_conflict events.

func recordFindingWithStateAndDisposition(t *testing.T, state string, disposition string) []db.JobEvent {
	t.Helper()
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	head := strings.Repeat("d", 40)
	finding := map[string]any{
		"id": "F-1", "severity": "P1", "title": "boundary check",
		"detail": "the boundary check is inverted", "file": "internal/x.go",
	}
	if state != "" {
		finding["state"] = state
	}
	if disposition != "" {
		finding["disposition"] = disposition
	}
	raw, err := json.Marshal(finding)
	if err != nil {
		t.Fatalf("marshal finding: %v", err)
	}

	job := db.Job{ID: "review-conflict", Agent: "gm-review-opus", Type: "review"}
	payload := JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2069", PullRequest: 2069, HeadSHA: head,
		TaskID: "task-2069", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "two answers",
			TestsRun: []string{"go test ./internal/workflow/"},
			Evidence: "EXECUTED",
			Findings: []json.RawMessage{raw},
		},
	}
	insertCompletedJob(t, store, job, payload)
	stored := mustJob(t, store, job.ID)
	if err := engine.RecordReviewFindingsToLedger(ctx, stored, payload); err != nil {
		t.Fatalf("RecordReviewFindingsToLedger: %v", err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	return events
}

func countEventKind(events []db.JobEvent, kind string) int {
	n := 0
	for _, event := range events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

// THE DEFECT. Two answers, disagreeing, one of them silently discarded.
func TestDisagreeingStateAndDispositionIsAudible(t *testing.T) {
	events := recordFindingWithStateAndDisposition(t, "open", "answered")
	if got := countEventKind(events, "findings_ledger_state_conflict"); got != 1 {
		t.Fatalf("findings_ledger_state_conflict events = %d, want 1: a discarded answer left no trace", got)
	}
}

// NEGATIVE CONTROLS, so a fix that fires on every finding cannot pass. Agreement
// in different letter case is agreement, not conflict.
func TestAgreeingAnswersEmitNoConflict(t *testing.T) {
	for _, tc := range []struct{ name, state, disposition string }{
		{"identical", "answered", "answered"},
		{"case-insensitive", "Answered", "answered"},
		{"state only", "answered", ""},
		{"disposition only", "", "answered"},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := recordFindingWithStateAndDisposition(t, tc.state, tc.disposition)
			if got := countEventKind(events, "findings_ledger_state_conflict"); got != 0 {
				t.Fatalf("conflict events = %d, want 0 for state=%q disposition=%q", got, tc.state, tc.disposition)
			}
		})
	}
}
