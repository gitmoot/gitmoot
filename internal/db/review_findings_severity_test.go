package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordReviewFindingObservationRefusesUnrankableSeverity is issue #1928's
// acceptance criteria 1, 2 and 5 through the PRODUCTION write path.
//
// MEASURED DEFECT: three persisted rows for jerryfane/joltra#831 carry
// severity "" (#831-f1..f3 at head 6360ab56, observer
// local-review-joltra-sol-review-18d2814edd22159c). An empty severity is not
// P3, and no severity policy can disposition it later - the row is an
// obligation that can only ever fail closed. Every other caller-asserted field
// on this boundary (head, state, observed_at, evidence) is already refused when
// it cannot be trusted; severity was the one that was not.
//
// A DEFAULT WOULD BE WORSE THAN A REFUSAL. This boundary is the last point that
// can still tell a missing severity from a chosen one, so defaulting to P3 here
// would silently downgrade an unknown-severity defect at exactly the moment the
// information still exists.
func TestRecordReviewFindingObservationRefusesUnrankableSeverity(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	head := strings.Repeat("a", 40)
	base := func(severity string, observer string) ReviewFindingObservation {
		return ReviewFindingObservation{
			Repo: "owner/repo", PullRequest: 831, HeadSHA: head, ObserverJob: observer,
			State: FindingOpen, Severity: severity, Title: "t", Detail: "d",
			File: "internal/db/review_findings.go", EvidenceKind: EvidenceExecuted,
			ExecutedCommands: []string{"go test ./internal/db/"}, ExecutedCount: 1,
		}
	}

	// The reproduction: the exact shape that produced the joltra#831 rows.
	for _, severity := range []string{"", "   ", "p1", "P4", "critical", "unknown"} {
		uid, err := store.RecordReviewFindingObservation(ctx, base(severity, "review-refused"))
		if !errors.Is(err, ErrFindingSeverity) {
			t.Errorf("severity %q was accepted or misreported: uid=%q err=%v; the ledger would hold a row no severity policy can disposition", severity, uid, err)
		}
		if uid != "" {
			t.Errorf("severity %q minted uid %q; a refused observation must not consume identity", severity, uid)
		}
		// The caller must be able to act on it: the error names the value it got.
		if err != nil && !strings.Contains(err.Error(), "severity") {
			t.Errorf("refusal for %q does not name severity: %v", severity, err)
		}
	}

	// PASSING CONTROL, so a green test cannot mean "the boundary refuses
	// everything": every canonical severity is still accepted, and surrounding
	// whitespace normalises rather than failing.
	for i, severity := range []string{"P0", "P1", "P2", "P3", " P2 "} {
		uid, err := store.RecordReviewFindingObservation(ctx, base(severity, "review-accepted-"+severity))
		if err != nil {
			t.Fatalf("canonical severity %q was refused: %v", severity, err)
		}
		if uid == "" {
			t.Fatalf("canonical severity %q minted no uid", severity)
		}
		var stored string
		if err := store.db.QueryRowContext(ctx, `SELECT severity FROM review_finding_observations WHERE finding_uid = ?`, uid).Scan(&stored); err != nil {
			t.Fatalf("read back %q: %v", uid, err)
		}
		if stored != strings.TrimSpace(severity) {
			t.Errorf("row %d stored severity %q, want %q", i, stored, strings.TrimSpace(severity))
		}
	}
}

// TestLegacyEmptySeverityRowsStayFailClosedObligations is acceptance criterion 4.
//
// The rows already in the ledger must NOT be repaired, defaulted or dismissed by
// this change: a legacy empty-severity finding stays an obligation until a
// reviewer observes it explicitly. The insert goes straight to SQL because the
// Go boundary now refuses this shape - which is the point: the only way such a
// row can exist is from before the refusal, so that is how the fixture is built.
func TestLegacyEmptySeverityRowsStayFailClosedObligations(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	head := strings.Repeat("b", 40)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO review_finding_observations(
		finding_uid, repo, pull_request, head_sha, observer_job, state, severity, evidence_kind)
		VALUES ('owner/repo#831-f1', 'owner/repo', 831, ?, 'legacy-review', 'open', '', 'EXECUTED')`, head); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "owner/repo", 831)
	if err != nil {
		t.Fatalf("list observations: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("legacy row count = %d, want 1: a legacy row must remain readable rather than be filtered out of existence", len(observations))
	}
	legacy := observations[0]
	if legacy.Severity != "" {
		t.Errorf("legacy severity was rewritten to %q; this change must not repair existing rows", legacy.Severity)
	}
	if legacy.State != FindingOpen {
		t.Errorf("legacy state = %q, want open: an empty severity must never auto-dismiss the obligation", legacy.State)
	}
}
