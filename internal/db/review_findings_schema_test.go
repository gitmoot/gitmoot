package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// #1850 F9, P3. The CHECK read `head_sha GLOB '[0-9a-f]*'`, which is a single
// bracket expression followed by `*`: it pins character ONE and leaves the other
// 39 unconstrained. Measured by the reviewer with a raw INSERT: 'a' + 39 'Z'
// characters was ACCEPTED while the migration comment claimed 40 hex. The Go
// layer's headSHAPattern caught it on every real write, which is why this is P3
// and not higher, but a schema comment that overstates the schema is the same
// defect class as a code comment naming a call site that does not exist.
//
// This test goes AROUND the Go validator on purpose, straight to SQL, because
// the claim under test is the DATABASE's guarantee and not the store method's.
func TestReviewFindingHeadSHACheckConstrainsAllFortyCharacters(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	insert := `INSERT INTO review_finding_observations(
		finding_uid, repo, pull_request, head_sha, observer_job, state, evidence_kind)
		VALUES (?, 'owner/repo', 7, ?, 'probe', 'open', 'EXECUTED')`

	// The reviewer's exact probe: first character hex, remaining 39 not.
	nonHex := "a" + strings.Repeat("Z", 39)
	if _, err := store.db.ExecContext(ctx, insert, "uid-nonhex", nonHex); err == nil {
		t.Fatalf("the CHECK accepted %q, so the advertised 40-hex database guarantee is false", nonHex)
	}
	// Wrong length must still be refused.
	if _, err := store.db.ExecContext(ctx, insert, "uid-short", strings.Repeat("a", 39)); err == nil {
		t.Fatal("the CHECK accepted a 39-character head")
	}
	// PASSING CASE, asserted so the constraint cannot be satisfied by refusing
	// everything: a real 40-hex head inserts cleanly.
	if _, err := store.db.ExecContext(ctx, insert, "uid-ok", strings.Repeat("abcdef0123", 4)); err != nil {
		t.Fatalf("the CHECK rejected a valid 40-hex head: %v", err)
	}
}

// #1850 F10, P3. The scan discarded json.Unmarshal errors, so a malformed
// relevance_keys value yielded a nil key set. A nil key set matches nothing,
// which silently stops an answered finding ever being mandatory again - and it
// is indistinguishable from a reviewer's deliberate narrow key set. Since
// normaliseKeys guarantees a non-empty array on write, a decode failure on read
// is real corruption and must be reported rather than swallowed.
func TestReviewFindingListReportsCorruptRelevanceKeys(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	head := strings.Repeat("a", 40)
	if _, err := store.RecordReviewFindingObservation(ctx, ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: head, ObserverJob: "review-1",
		State: FindingOpen, Title: "t", File: "internal/run.go",
		EvidenceKind: EvidenceExecuted, ExecutedCommands: []string{"probe"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A healthy row reads back cleanly first, so the failure below is attributable
	// to the corruption and not to the fixture.
	if rows, err := store.ListReviewFindingObservations(ctx, "owner/repo", 7); err != nil || len(rows) != 1 {
		t.Fatalf("healthy read returned %d row(s), err %v", len(rows), err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE review_finding_observations SET relevance_keys = '{not json'`); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}
	rows, err := store.ListReviewFindingObservations(ctx, "owner/repo", 7)
	if err == nil {
		t.Fatalf("a corrupt relevance_keys value read back as %d row(s) with no error, so relevance silently matches nothing", len(rows))
	}
	if !strings.Contains(err.Error(), "relevance_keys") {
		t.Fatalf("error does not name the corrupt column: %v", err)
	}
}
