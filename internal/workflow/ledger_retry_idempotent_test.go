package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// A finished review whose post-delivery advance keeps failing is retried every
// few seconds, and each retry re-ran the ledger write. Every pass minted new
// uids for the same findings: themartianapp/among-friends#74's review wrote
// its findings 14,386 times in ten days. The ledger must hold one observation
// per finding however often the same result is recorded.
func TestRecordingTheSameReviewAgainAddsNothing(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	job := insertLedgerRetryJob(t, store, "rev-retry", strings.Repeat("a", 40), []string{
		`{"severity":"P2","title":"nil map write in addAgent","file":"internal/dashboard/access.go","line":112}`,
		`{"severity":"P3","title":"stale comment","file":"internal/dashboard/access.go","line":40}`,
	})
	for i := 0; i < 5; i++ {
		if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), job.payload); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	rows, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2300)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("after 5 passes the ledger holds %d observations, want 2", len(rows))
	}
	events, err := store.ListJobEvents(ctx, job.id)
	if err != nil {
		t.Fatal(err)
	}
	recorded := 0
	for _, e := range events {
		if e.Kind == "findings_ledger_recorded" {
			recorded++
		}
	}
	if recorded != 1 {
		t.Fatalf("%d findings_ledger_recorded events after 5 passes, want 1", recorded)
	}

	// Another review of the same head, and the same review at a new head, are
	// new observations and are still recorded.
	other := insertLedgerRetryJob(t, store, "rev-other", strings.Repeat("a", 40), []string{
		`{"severity":"P2","title":"nil map write in addAgent","file":"internal/dashboard/access.go","line":112}`,
	})
	if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, other.id), other.payload); err != nil {
		t.Fatal(err)
	}
	moved := job.payload
	moved.HeadSHA = strings.Repeat("b", 40)
	if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), moved); err != nil {
		t.Fatal(err)
	}
	rows, _ = store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2300)
	if len(rows) != 5 {
		t.Fatalf("got %d observations, want 5 (2 + 1 from another reviewer + 2 at a new head)", len(rows))
	}
}

type ledgerRetryJob struct {
	id      string
	payload JobPayload
}

func insertLedgerRetryJob(t *testing.T, store *db.Store, id, head string, findings []string) ledgerRetryJob {
	t.Helper()
	raw := make([]json.RawMessage, len(findings))
	for i, f := range findings {
		raw[i] = json.RawMessage(f)
	}
	payload := JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "b", PullRequest: 2300,
		HeadSHA: head, TaskID: "t", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P2", Summary: "s",
			TestsRun: []string{"go test ./..."}, Evidence: "EXECUTED",
			Findings: raw,
		},
	}
	insertCompletedJob(t, store, db.Job{ID: id, Agent: "rev", Type: "review"}, payload)
	return ledgerRetryJob{id: id, payload: payload}
}
