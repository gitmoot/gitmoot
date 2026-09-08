package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2059: the wire read `id`/`state` while reviewers emit `uid`/`disposition`, so
// a review that ANSWERED findings recorded new OPEN ones instead.
//
// Measured on PR #1930, job local-review-gm-review-opus-18d34115868c7f44: 20
// findings, all carrying uid + severity + disposition + evidence, none carrying
// id, state, title or detail. All 20 declared answered. All 20 were written as
// open under freshly minted uids f36-f55, leaving f1-f33 unobserved.
//
// The prose half already landed with #1928/#1936 - `evidence` feeds Detail. What
// remained were the two fields that decide whether an answer counts: state and
// identity.

func seedLedgerFinding(t *testing.T, store *db.Store, head string, uid string) string {
	t.Helper()
	recorded, err := store.RecordReviewFindingObservation(context.Background(), db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1930, HeadSHA: head,
		ObserverJob: "review-prior", SourceJob: "review-prior",
		Severity: "P1", State: db.FindingOpen,
		Title: "prior finding", Detail: "recorded by an earlier round",
		File: "internal/workflow/x.go", EvidenceLocator: "internal/workflow/x.go:1",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./internal/workflow/ -> ok"}, ExecutedCount: 1,
		Rationale: "seeded",
	})
	if err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	return recorded
}

// The reproduction: `disposition: answered` plus a `uid` naming a prior finding
// must ANSWER that finding, not mint a new open one.
func TestDispositionAndUIDAnswerThePriorFinding(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("e", 40)

	priorUID := seedLedgerFinding(t, store, head, "")

	insertCompletedJob(t, store, db.Job{ID: "review-2059", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 1930, HeadSHA: head,
		TaskID: "task-1930", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "prior finding answered",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{json.RawMessage(`{"uid":"` + priorUID + `","severity":"P1","disposition":"answered","evidence":"internal/workflow/x.go:1 - fixed and re-run","rationale":"re-ran the failing case at this head"}`)},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-2059"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1930)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations: %v", err)
	}
	answered, minted := 0, 0
	for _, obs := range observations {
		if obs.ObserverJob != "review-2059" {
			continue
		}
		if obs.FindingUID == priorUID && obs.State == db.FindingAnswered {
			answered++
			continue
		}
		minted++
	}
	if answered != 1 {
		t.Fatalf("answered observations of %s = %d, want 1; a declared answer must discharge the finding it names. all=%+v", priorUID, answered, observations)
	}
	if minted != 0 {
		t.Fatalf("newly minted findings = %d, want 0; re-opening what a reviewer answered is the #1930 defect", minted)
	}
}

// THE CONTROL, and it is why `uid` is routed only on a TERMINAL disposition. A
// finding declared OPEN that carries a uid is MINTING, not answering. Routing it
// into continues_uid would make the store refuse every new finding an agent
// numbers itself, turning a silent loss into a loud one.
func TestOpenDispositionWithAUIDStillMints(t *testing.T) {
	if got := continuationUID(reviewFindingWire{UID: "#1930-f9"}, db.FindingOpen); got != "" {
		t.Fatalf("continuationUID for an OPEN finding = %q, want empty; an open uid is a mint, not a continuation", got)
	}
	if got := continuationUID(reviewFindingWire{UID: "#1930-f9"}, db.FindingAnswered); got != "#1930-f9" {
		t.Fatalf("continuationUID for an ANSWERED finding = %q, want the uid it answers", got)
	}
	// An explicit continues_uid always wins: it is the canonical key and a
	// reviewer that used it meant it.
	if got := continuationUID(reviewFindingWire{UID: "#1930-f9", ContinuesUID: "#1930-f2"}, db.FindingAnswered); got != "#1930-f2" {
		t.Fatalf("continuationUID = %q, want the explicit continues_uid to win", got)
	}
}

// The state alias, isolated from identity so a failure names which half broke.
func TestDispositionIsReadAsAnAlternateSpellingOfState(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire reviewFindingWire
		want db.FindingState
	}{
		{name: "disposition answered", wire: reviewFindingWire{Disposition: "answered"}, want: db.FindingAnswered},
		{name: "state wins over disposition", wire: reviewFindingWire{State: "withdrawn", Disposition: "answered"}, want: db.FindingWithdrawn},
		{name: "neither means open", wire: reviewFindingWire{}, want: db.FindingOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := db.FindingState(strings.ToLower(firstNonEmptyLedgerText(
				strings.TrimSpace(tc.wire.State), strings.TrimSpace(tc.wire.Disposition))))
			if got == "" {
				got = db.FindingOpen
			}
			if got != tc.want {
				t.Fatalf("declared state = %q, want %q", got, tc.want)
			}
		})
	}
}
