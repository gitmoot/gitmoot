package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1969's remaining acceptance: a repository declares whether it CONSUMES review
// findings or treats them as ADVISORY.
//
// THE GATE THIS RELAXES IS LIVE, not theoretical. Measured on this box:
// EnsureLedgerObligationsObserved refused 16 times across 5 repositories,
// including jerryfane/vetrina#126 held on 37 prior findings all "still open" and
// gitmoot/coordinator-checkin#3 on 17. That is the wedge
// findings_ledger_writer.go's header predicts when the write half runs without
// the read half: a guard rejecting valid input because nobody was ever told the
// uids it demands.
//
// So the declaration has to do three things, and each is a subtest: an
// undeclared repository must be unchanged, an advisory one must merge past the
// obligations, and an advisory merge must SAY what it went past. The third is
// the one that keeps "advisory" from meaning "silent", which would reproduce
// #1969's defect with an operator's signature on it.
func TestAdvisoryDeclarationRelaxesTheLedgerGateAndRecordsWhatItPassed(t *testing.T) {
	// The obligation is recorded at a PRIOR head and evaluated at a later one,
	// which is the real wedge: an observation recorded AT the evaluated head is
	// already discharged there and pends nothing. My first fixture used one head
	// for both and the premise assertion below caught it.
	priorHead := strings.Repeat("ab12cd34ef", 4)
	head := strings.Repeat("99f00d1234", 4)

	for _, tc := range []struct {
		name        string
		advisory    bool
		wantRefusal bool
		wantNote    bool
	}{
		{name: "undeclared repository still refuses", advisory: false, wantRefusal: true},
		{name: "advisory repository merges past and records it", advisory: true, wantNote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-1969", RepoFullName: "owner/repo", GoalID: "goal", Title: "t",
				State: string(TaskReviewing), Branch: "feature",
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
				Repo: "owner/repo", PullRequest: 7, HeadSHA: priorHead,
				ObserverJob: "local-review-round-1", State: db.FindingOpen, Severity: "P1",
				RoundLabel: "F1", Title: "an obligation nobody observed",
				Detail: "recorded and never answered", File: "internal/a.go",
				EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test"}, ExecutedCount: 1,
			}); err != nil {
				t.Fatalf("RecordReviewFindingObservation: %v", err)
			}

			request := MergeRequest{
				Repo: "owner/repo", PullRequest: 7, TaskID: "task-1969",
				Reviewer: "reviewer", FindingsAdvisory: tc.advisory,
			}
			gate := PolicyMergeGate{Store: store}

			// THE PREMISE, and it is what makes the advisory arm mean anything:
			// without the declaration this obligation really does refuse. Built
			// from a CONSUMING request so the premise cannot be satisfied by the
			// very relaxation under test.
			consuming := request
			consuming.FindingsAdvisory = false
			refusal := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, head, gate.ledgerScope(consuming))
			if refusal == nil {
				t.Fatal("premise broken: the obligation gate did not refuse, so neither arm tests anything")
			}
			if !strings.Contains(refusal.Error(), "carry no observation at head") {
				t.Fatalf("unexpected refusal shape %q", refusal)
			}

			// THROUGH THE REAL GATE, not by calling the recorder directly. An
			// earlier version of this test invoked recordLedgerAdvisoryNote itself
			// and therefore never exercised the gate's `if request.FindingsAdvisory`
			// branch at all: a mutant that made the gate ignore the declaration
			// entirely left it green. That is the same green-test-exercising-no-path
			// shape this campaign keeps finding in other people's work.
			err := gate.ensureFinalReviewCaptured(ctx, request, head)
			ledgerRefused := err != nil && strings.Contains(err.Error(), "carry no observation at head")
			if tc.wantRefusal && !ledgerRefused {
				t.Fatalf("an undeclared repository did not refuse on its unobserved obligations; err = %v", err)
			}
			if !tc.wantRefusal && ledgerRefused {
				t.Fatalf("an advisory repository still refused on the ledger: %v", err)
			}

			events, err := store.ListTaskEvents(ctx, "task-1969")
			if err != nil {
				t.Fatalf("ListTaskEvents: %v", err)
			}
			var note string
			for _, event := range events {
				if event.Kind == "findings_ledger_advisory_merge" {
					note = event.Reason
				}
			}
			if tc.wantNote {
				if note == "" {
					t.Fatal("an advisory merge recorded nothing; advisory must mean stated, not silent")
				}
				// It has to name the obligation it went past, or the record is a
				// bare flag and a reader learns nothing they could act on.
				if !strings.Contains(note, "owner/repo#7-f1") {
					t.Fatalf("advisory note does not name the obligation it passed: %q", note)
				}
				if !strings.Contains(note, "advisory") {
					t.Fatalf("advisory note does not name the declaration that permitted it: %q", note)
				}
			}
			if tc.wantRefusal && note != "" {
				t.Fatalf("an undeclared repository recorded an advisory note: %q", note)
			}
		})
	}
}

// The resolver defaults FALSE, so a missing seam can never relax the gate. This
// is the fail-closed direction and it is the one worth pinning: the failure that
// matters is a repository silently becoming advisory, never one staying
// consuming.
func TestFindingsAdvisoryDefaultsToConsumingWithNoResolver(t *testing.T) {
	if (Engine{}).findingsAdvisory("owner/repo") {
		t.Fatal("a nil FindingsAdvisory resolver reported advisory; an unwired seam must never relax the obligation gate")
	}
	engine := Engine{FindingsAdvisory: func(string) bool { return true }}
	if !engine.findingsAdvisory("owner/repo") {
		t.Fatal("an installed resolver was ignored")
	}
}
