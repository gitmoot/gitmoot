package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2073. A shape census over every finding element in this store found reviewer
// prose riding under three keys the writer never read: `details`, `message` and
// `finding`. A row lands articulate in the job payload and empty in the ledger,
// and the merge gate then names an obligation nobody can read.
//
// The keys are READ, never invented: each is measured from real verdicts. What
// this test pins is the consumer-observable claim - the prose reaches the
// ledger - rather than which struct field it passed through.
func TestAdvanceJobKeepsProseFromTheThreeUnreadKeys(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("c", 40)

	const (
		detailsProse = "ListJobsByType selects jobColumns, so externally_driven is carried; a narrower list would take the weaker reason."
		messageProse = "The reaper ages a claim from the claim row's own created_at, so a resumed value does not reset the wait."
		findingProse = "An unresolved hold is keyed by turn_id plus content_hash, so changing content on the same turn bypasses it."
	)

	insertCompletedJob(t, store, db.Job{ID: "review-2073", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2073", PullRequest: 2073, HeadSHA: head,
		TaskID: "task-2073", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "three unread prose keys",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{
				// {details, file, line, severity, summary}: title survives via
				// `summary`, detail was lost.
				json.RawMessage(`{"severity":"P2","file":"internal/db/store_jobs.go","line":449,"summary":"column list carries externally_driven","details":"` + detailsProse + `"}`),
				// {location, message, severity}: title and detail both lost.
				json.RawMessage(`{"severity":"P2","location":"internal/pipeline/run.go:266","message":"` + messageProse + `"}`),
				// {file, finding, line}: title, detail and severity all lost.
				json.RawMessage(`{"file":"herdres_connector/source_sync.py","line":7105,"finding":"` + findingProse + `"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-2073"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2073)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}

	// TWO of the three land, and the third is refused for a reason this change
	// must NOT fix. {file, finding, line} carries no severity, so the store's
	// ErrFindingSeverity rejects it: a row no severity policy could ever
	// disposition. Reading its prose is necessary and not sufficient, and
	// minting a severity for it would be the invention this writer exists to
	// avoid. The refusal is recorded rather than silent, which is the property
	// worth pinning.
	if len(observations) != 2 {
		t.Fatalf("ledger holds %d row(s); want the two severity-bearing findings", len(observations))
	}

	details := make([]string, 0, len(observations))
	for _, obs := range observations {
		details = append(details, strings.TrimSpace(obs.Detail))
	}
	joined := strings.Join(details, "\n")

	for _, want := range []struct {
		key   string
		prose string
	}{
		{"details", detailsProse},
		{"message", messageProse},
	} {
		if !strings.Contains(joined, want.prose) {
			t.Fatalf("prose sent under %q never reached the ledger; details column held:\n%s", want.key, joined)
		}
	}

	// A row with prose but no title is still readable; a row with neither is the
	// #1968 population. Neither of these may land bare.
	for _, obs := range observations {
		if strings.TrimSpace(obs.Title) == "" && strings.TrimSpace(obs.Detail) == "" {
			t.Fatalf("observation %q landed with neither title nor detail: a bare obligation nobody can answer", obs.FindingUID)
		}
	}

	// The severity-less finding is REFUSED, not discarded: the writer records an
	// event so the loss is countable. Without this the fix would look complete
	// while one measured shape still vanished silently.
	events, err := store.ListJobEvents(ctx, "review-2073")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	refusal := ""
	for _, event := range events {
		if strings.Contains(event.Kind, "refus") || strings.Contains(event.Message, "refus") {
			refusal += event.Kind + " :: " + event.Message + "\n"
		}
	}
	if !strings.Contains(refusal, "severity") {
		t.Fatalf("no recorded refusal naming severity for the {file, finding, line} shape; events were:\n%s", refusal)
	}
}

// The canonical key must still win when a reviewer sends both, or this fix would
// silently reorder prose for the shapes that already worked.
func TestAdvanceJobPrefersTheCanonicalDetailOverTheAlternates(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("d", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-2073-order", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-order", PullRequest: 2074, HeadSHA: head,
		TaskID: "task-order", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "precedence",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{
				json.RawMessage(`{"severity":"P1","file":"a.go","line":1,"title":"t","detail":"canonical wins","details":"alternate loses","message":"alternate loses","finding":"alternate loses"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-2073-order"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2074)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("ledger holds %d row(s), want 1", len(observations))
	}
	if got := strings.TrimSpace(observations[0].Detail); got != "canonical wins" {
		t.Fatalf("detail = %q, want the canonical `detail` key to win over the alternates", got)
	}
}
