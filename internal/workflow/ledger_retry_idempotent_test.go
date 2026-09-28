package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// The first fix keyed "already done" on rows written, so a review whose
// findings were ALL refused wrote none and repeated its refusal and summary
// events on every retry: the same runaway, moved to job_events (#2269 review).
func TestRetriesOfAnAllRefusedReviewAddNoEvents(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	job := insertLedgerRetryJob(t, store, "rev-refused", strings.Repeat("c", 40), []string{
		refusalEchoFixture, refusalEchoFixture,
	})
	count := func() map[string]int {
		events, err := store.ListJobEvents(ctx, job.id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, e := range events {
			if strings.HasPrefix(e.Kind, "findings_ledger") {
				out[e.Kind]++
			}
		}
		return out
	}
	if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), job.payload); err != nil {
		t.Fatal(err)
	}
	first := count()
	if first["findings_ledger_refused"] != 2 || first[ledgerDoneEventKind] != 1 {
		t.Fatalf("first pass events %v, want 2 refusals and the done marker", first)
	}
	for i := 0; i < 5; i++ {
		if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), job.payload); err != nil {
			t.Fatal(err)
		}
	}
	if after := count(); fmt.Sprint(after) != fmt.Sprint(first) {
		t.Fatalf("events after 5 retries %v, want unchanged %v", after, first)
	}
}

// A pass that stopped part-way (the daemon died after writing some findings,
// before the done marker) is finished by the next retry without writing its
// first findings again.
func TestAnInterruptedPassIsFinishedWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	first := `{"severity":"P2","title":"nil map write","file":"a.go","line":1}`
	job := insertLedgerRetryJob(t, store, "rev-crash", strings.Repeat("d", 40), []string{first})
	// The interrupted pass: it recorded the first finding (built exactly as
	// the writer builds it, with this job as observer) and died before its
	// done marker.
	var wire reviewFindingWire
	if err := json.Unmarshal([]byte(first), &wire); err != nil {
		t.Fatal(err)
	}
	obs, ok := engine.ledgerObservationFor(mustJob(t, store, job.id), job.payload, wire, job.payload.HeadSHA, "gitmoot/gitmoot")
	if !ok {
		t.Fatal("fixture finding is not recordable")
	}
	if _, err := store.RecordReviewFindingObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	full := job.payload
	full.Result = &AgentResult{Decision: "changes_requested", Severity: "P2", Summary: "s", TestsRun: []string{"go test ./..."}, Evidence: "EXECUTED",
		Findings: []json.RawMessage{json.RawMessage(first), json.RawMessage(`{"severity":"P3","title":"stale comment","file":"b.go","line":2}`)}}
	if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), full); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2300)
	if len(rows) != 2 {
		t.Fatalf("got %d observations after the resumed pass, want 2 (the first not repeated)", len(rows))
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

// RetryJob re-runs the same job id and keeps its events, so a "done" marker
// keyed on the head alone dropped the retried run's different findings at that
// head (#2269 review).
func TestARetriedJobRecordsItsNewFindingsAtTheSameHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	job := insertLedgerRetryJob(t, store, "rev-rerun", strings.Repeat("e", 40), []string{
		`{"severity":"P2","title":"nil map write","file":"a.go","line":1}`,
	})
	if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), job.payload); err != nil {
		t.Fatal(err)
	}
	rerun := job.payload
	rerun.Result = &AgentResult{Decision: "changes_requested", Severity: "P1", Summary: "s", TestsRun: []string{"go test ./..."}, Evidence: "EXECUTED",
		Findings: []json.RawMessage{json.RawMessage(`{"severity":"P1","title":"token logged","file":"b.go","line":9}`)}}
	for i := 0; i < 3; i++ {
		if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), rerun); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2300)
	if len(rows) != 2 {
		t.Fatalf("got %d observations, want 2 (one per run, the rerun's written once)", len(rows))
	}
}

// A store failure is not a verdict on the finding: the next retry must write
// what the failed pass could not, instead of treating that pass as complete.
func TestAFindingTheStoreFailedToWriteIsWrittenOnTheNextPass(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	job := insertLedgerRetryJob(t, store, "rev-busy", strings.Repeat("f", 40), []string{
		`{"severity":"P2","title":"nil map write","file":"a.go","line":1}`,
	})
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `
CREATE TRIGGER fail_ledger_insert
BEFORE INSERT ON review_finding_observations
BEGIN
	SELECT RAISE(ABORT, 'database is locked');
END`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), job.payload); err != nil {
			t.Fatal(err)
		}
	}
	if rows, _ := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2300); len(rows) != 0 {
		t.Fatalf("the trigger did not fire: %d observations written", len(rows))
	}
	events, _ := store.ListJobEvents(ctx, job.id)
	pending := 0
	for _, e := range events {
		if e.Kind == ledgerRetryPendingEventKind {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("%d retry-pending events after 3 failed passes, want 1", pending)
	}
	if _, err := raw.ExecContext(ctx, `DROP TRIGGER fail_ledger_insert`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), job.payload); err != nil {
			t.Fatal(err)
		}
	}
	if rows, _ := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2300); len(rows) != 1 {
		t.Fatalf("got %d observations once the store recovered, want 1", len(rows))
	}
}

// A resumed pass must not mistake a different finding for one it already
// wrote just because the two share their text: here only relevance_keys differ.
func TestAResumedPassKeepsFindingsThatDifferOnlyInRelevanceKeys(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	first := `{"severity":"P2","title":"stale cache","file":"a.go","line":1,"relevance_keys":["a.go"]}`
	second := `{"severity":"P2","title":"stale cache","file":"a.go","line":1,"relevance_keys":["internal/b.go"]}`
	job := insertLedgerRetryJob(t, store, "rev-keys", strings.Repeat("c", 40), []string{first, second})
	var wire reviewFindingWire
	if err := json.Unmarshal([]byte(first), &wire); err != nil {
		t.Fatal(err)
	}
	obs, ok := engine.ledgerObservationFor(mustJob(t, store, job.id), job.payload, wire, job.payload.HeadSHA, "gitmoot/gitmoot")
	if !ok || len(obs.RelevanceKeys) == 0 {
		t.Fatalf("fixture must carry relevance keys: ok=%v keys=%v", ok, obs.RelevanceKeys)
	}
	if _, err := store.RecordReviewFindingObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.id), job.payload); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2300)
	if len(rows) != 2 {
		t.Fatalf("got %d observations, want 2 (the second finding differs in relevance_keys)", len(rows))
	}
}
