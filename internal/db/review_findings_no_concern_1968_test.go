package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// #1968. A finding with no articulated concern became a durable obligation.
//
// Measured on this box's PRE-CLEANUP store (the backup taken before PHOBOS's
// approved withdrawal pass, so the denominator is the one the defect actually
// produced): of 571 recorded findings, 77 carried an empty title AND detail AND
// rationale, and 46 of those were OPEN. 7 P1, 21 P2, 17 P3, 1 with no severity.
// 73 had to be withdrawn by hand, one written reason at a time.
//
// The bar lives at the store boundary rather than in the writer that produced
// those rows, because the writer is one of several callers: the lens path,
// continuations and any future writer all reach the ledger through here. A rule
// defined against an open set of callers decays as the set widens.
func TestRecordReviewFindingObservationRefusesAnOpeningRowThatArticulatesNothing(t *testing.T) {
	ctx := context.Background()
	store := openNoConcernTestStore(t)

	_, err := store.RecordReviewFindingObservation(ctx, noConcernObservation(ReviewFindingObservation{
		State:            FindingOpen,
		EvidenceKind:     EvidenceExecuted,
		ExecutedCommands: []string{"go test ./internal/db"},
		ExecutedCount:    1,
		File:             "internal/cli/style/style.go",
		Line:             78,
	}))
	if !errors.Is(err, ErrFindingNoConcern) {
		t.Fatalf("RecordReviewFindingObservation = %v, want ErrFindingNoConcern for a row whose only content is a locator", err)
	}
	if !strings.Contains(err.Error(), "internal/cli/style/style.go") {
		t.Fatalf("refusal %q must name the locator it refused, so the producer can see which finding was rejected", err)
	}
}

// THE CONTROL THAT CHANGED THE DESIGN, and it exists because gm-integrity asked
// whether this rule could contradict the #1965 continues_uid normalisation
// instead of only asking about line numbers.
//
// My first predicate was state-blind. Measured against the live ledger, ZERO of
// the 77 no-prose rows are open: 58 are withdrawn and 19 are ANSWERED, and those
// 19 are genuine discharges - P1, P2 and P3, EXECUTED with executed_count 8
// apiece, against real paths like internal/sandbox/exec_linux.go. A state-blind
// bar would have refused all 19 and left 19 real obligations, P1s among them,
// open forever: removing noise by refusing real discharges is a worse ledger.
//
// It would also have killed gm-integrity's own case twice. Those six findings
// were already lost once to an abbreviated uid; they move to answered at a new
// head, so a state-blind bar would have refused the same verdict a second time
// by a different mechanism.
func TestRecordReviewFindingObservationStillAcceptsADischargeWithNoProse(t *testing.T) {
	ctx := context.Background()
	store := openNoConcernTestStore(t)

	uid, err := store.RecordReviewFindingObservation(ctx, noConcernObservation(ReviewFindingObservation{
		State:            FindingAnswered,
		EvidenceKind:     EvidenceExecuted,
		ExecutedCommands: []string{"go test -run TestSandboxExec ./internal/sandbox"},
		ExecutedCount:    8,
		File:             "internal/sandbox/exec_linux.go",
	}))
	if err != nil {
		t.Fatalf("a discharge carrying executed evidence and no prose was refused: %v", err)
	}
	if strings.TrimSpace(uid) == "" {
		t.Fatal("discharge recorded no uid")
	}
}

// An opening row whose whole concern lives in the rationale is real: 7 such rows
// were open on the pre-cleanup store. Refusing them would be the same error as
// refusing the 19 discharges, one field over.
func TestRecordReviewFindingObservationAcceptsAnOpeningRowWhoseConcernIsItsRationale(t *testing.T) {
	ctx := context.Background()
	store := openNoConcernTestStore(t)

	if _, err := store.RecordReviewFindingObservation(ctx, noConcernObservation(ReviewFindingObservation{
		State:            FindingOpen,
		EvidenceKind:     EvidenceExecuted,
		ExecutedCommands: []string{"go build ./..."},
		ExecutedCount:    1,
		Rationale:        "the build fails on a clean clone because the generated file is not committed",
	})); err != nil {
		t.Fatalf("an opening row whose concern is its rationale was refused: %v", err)
	}
}

// PRECEDENCE. The content bar runs after every older bar, so it can only refuse
// a row they all admitted. My first version ran it before the evidence switch
// and preempted ErrFindingDischarge for a STATIC row missing its rationale:
// both errors were true, but a caller matching the discharge sentinel would
// have stopped seeing it. This pins the ordering rather than trusting it.
func TestRecordReviewFindingObservationKeepsTheOlderRefusalsAheadOfTheContentBar(t *testing.T) {
	ctx := context.Background()
	store := openNoConcernTestStore(t)

	for _, tc := range []struct {
		name string
		obs  ReviewFindingObservation
		want error
	}{
		{
			name: "a missing severity still reports the severity refusal",
			obs: noConcernObservationWithSeverity(ReviewFindingObservation{
				State: FindingOpen, EvidenceKind: EvidenceExecuted,
				ExecutedCommands: []string{"go vet ./..."}, ExecutedCount: 1,
			}, ""),
			want: ErrFindingSeverity,
		},
		{
			name: "an evidence-free EXECUTED claim still reports the evidence refusal",
			obs: noConcernObservation(ReviewFindingObservation{
				State: FindingOpen, EvidenceKind: EvidenceExecuted,
			}),
			want: ErrFindingEvidence,
		},
		{
			name: "a STATIC row with no locator still reports the discharge refusal",
			obs: noConcernObservation(ReviewFindingObservation{
				State: FindingOpen, EvidenceKind: EvidenceStatic,
			}),
			want: ErrFindingDischarge,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.RecordReviewFindingObservation(ctx, tc.obs)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v; the content bar must not preempt an older, more specific refusal", err, tc.want)
			}
		})
	}
}

func openNoConcernTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "no-concern.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

// noConcernObservation fills the fields every observation needs so each test
// varies exactly one thing.
func noConcernObservation(obs ReviewFindingObservation) ReviewFindingObservation {
	return noConcernObservationWithSeverity(obs, "P2")
}

func noConcernObservationWithSeverity(obs ReviewFindingObservation, severity string) ReviewFindingObservation {
	obs.Repo = "gitmoot/gitmoot"
	obs.PullRequest = 1968
	obs.HeadSHA = strings.Repeat("ab12cd34ef", 4)
	obs.Severity = severity
	return obs
}
