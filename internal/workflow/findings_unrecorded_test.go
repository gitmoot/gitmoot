package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// A FINDING THE LEDGER DID NOT RECORD IS READABLE BY PULL REQUEST (tandem#2).
// The skip used to exist only as a job event, so `gitmoot findings` printed "no
// findings recorded" for a PR whose review had reported a P2.
func TestUnrecordedLedgerFindingsNamesTheJobAndIndex(t *testing.T) {
	ctx := context.Background()
	run := recordShapedFindings(t, true, false,
		`{"severity":"P2","title":"recorded","file":"internal/a.go"}`,
		`42`)
	if len(run.rows) != 1 {
		t.Fatalf("premise: %d rows recorded, want 1", len(run.rows))
	}
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "jerryfane/tandem")
	job := db.Job{ID: "rev-unrecorded", Agent: "rev", Type: "review"}
	insertCompletedJob(t, store, job, JobPayload{
		Repo: "jerryfane/tandem", PullRequest: 2, HeadSHA: "24ce1fdbbc8e53217131f009012298bcdb852116",
		Result: &AgentResult{Decision: "changes_requested"},
	})
	other := db.Job{ID: "rev-other-pr", Agent: "rev", Type: "review"}
	insertCompletedJob(t, store, other, JobPayload{Repo: "jerryfane/tandem", PullRequest: 3, Result: &AgentResult{Decision: "approved"}})
	for _, event := range append(run.events,
		// A job retried before #2268 repeated its skip on every pass: counted once.
		db.JobEvent{Kind: ledgerSkippedEventKind, Message: "finding[1] not recorded to the #1822 ledger: repeated pass"},
		db.JobEvent{Kind: ledgerRefusedEventKind, Message: "finding[2] at claimed severity P3 was REFUSED, not recorded: ..."},
	) {
		event.JobID = job.ID
		if err := store.AddJobEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: other.ID, Kind: ledgerSkippedEventKind, Message: "finding[0] not recorded to the #1822 ledger: other PR"}); err != nil {
		t.Fatal(err)
	}

	unrecorded, err := UnrecordedLedgerFindings(ctx, store, "jerryfane/tandem", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(unrecorded) != 2 {
		t.Fatalf("unrecorded = %+v, want finding[1] once and finding[2]", unrecorded)
	}
	if got := unrecorded[0]; got.JobID != job.ID || got.Index != 1 || got.Kind != ledgerSkippedEventKind ||
		got.Message != "finding[1] not recorded to the #1822 ledger: repeated pass" {
		t.Fatalf("unrecorded[0] = %+v, want finding[1]'s latest skip", got)
	}
	if got := unrecorded[1]; got.Index != 2 || got.Kind != ledgerRefusedEventKind {
		t.Fatalf("unrecorded[1] = %+v, want the refusal of finding[2]", got)
	}
}
