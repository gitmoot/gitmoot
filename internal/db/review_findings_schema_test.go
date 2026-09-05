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

// #1850 round 2 F3, ADV-11: THE UPGRADE PROOF FROM A PRE-FIX DATABASE.
//
// The prior head fixed the PK and the CHECK by EDITING the applied #1822
// migration. Migrate iterates positionally and skips versions already in
// schema_migrations, so every database that ran the prior head kept the pre-fix
// table forever - the fixes reached only databases created afterwards. This test
// reconstructs exactly that state and proves the appended rebuild upgrades it.
//
// IT ASSERTS ITS OWN PREMISE FIRST: the pre-fix table really does accept a
// non-hex head and really does reject a second observer, so a later PASS cannot
// come from a fixture that was already correct.
func TestReviewFindingRebuildUpgradesAPreFixDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "prefix.db")
	store, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Recreate the PRE-FIX shape, leaving every migration version recorded as
	// applied, which is precisely the state a developer daemon is in.
	for _, statement := range []string{
		`DROP TABLE review_finding_observations`,
		`CREATE TABLE review_finding_observations (
			finding_uid TEXT NOT NULL,
			repo TEXT NOT NULL,
			pull_request INTEGER NOT NULL DEFAULT 0,
			head_sha TEXT NOT NULL CHECK(length(head_sha) = 40 AND head_sha GLOB '[0-9a-f]*'),
			observed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			observer_job TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL CHECK(state IN ('open','answered','withdrawn','superseded')),
			severity TEXT NOT NULL DEFAULT '',
			round_label TEXT NOT NULL DEFAULT '',
			label_absent INTEGER NOT NULL DEFAULT 0,
			title TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			file TEXT NOT NULL DEFAULT '',
			line INTEGER NOT NULL DEFAULT 0,
			relevance_keys TEXT NOT NULL DEFAULT '[]',
			evidence_kind TEXT NOT NULL CHECK(evidence_kind IN ('EXECUTED','STATIC','QUOTED')),
			executed_commands TEXT NOT NULL DEFAULT '[]',
			executed_count INTEGER NOT NULL DEFAULT 0,
			evidence_locator TEXT NOT NULL DEFAULT '',
			rationale TEXT NOT NULL DEFAULT '',
			source_job TEXT NOT NULL DEFAULT '',
			withdraw_reason TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (finding_uid, head_sha)
		)`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("recreate pre-fix table: %v", err)
		}
	}
	// A DAEMON ON THE PRIOR HEAD HAS APPLIED EVERY VERSION EXCEPT THE NEWEST, so
	// the fixture must remove the rebuild's bookkeeping row. Without this the
	// cached template has ALL versions recorded, Migrate finds nothing to do, and
	// the test fails for a fixture reason rather than a code one - which is
	// exactly how it failed on its first run, and why the premise assertions
	// above matter more than the verdict below.
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if applied != len(migrations) {
		t.Fatalf("fixture holds %d applied migrations for %d defined; the template is not fully migrated", applied, len(migrations))
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, len(migrations)); err != nil {
		t.Fatalf("un-apply the newest migration: %v", err)
	}
	insert := `INSERT INTO review_finding_observations(
		finding_uid, repo, pull_request, head_sha, observer_job, state, evidence_kind)
		VALUES (?, 'owner/repo', 7, ?, ?, 'open', 'EXECUTED')`
	nonHex := "a" + strings.Repeat("Z", 39)

	// PREMISE 1: the pre-fix CHECK accepts a non-hex head.
	if _, err := store.db.ExecContext(ctx, insert, "uid-prefix", nonHex, "observer-a"); err != nil {
		t.Fatalf("premise failed: the pre-fix CHECK already rejected %q, so this fixture is not pre-fix: %v", nonHex, err)
	}
	// PREMISE 2: the pre-fix key rejects a SECOND observer at one head.
	realHead := strings.Repeat("abcdef0123", 4)
	if _, err := store.db.ExecContext(ctx, insert, "uid-two", realHead, "observer-a"); err != nil {
		t.Fatalf("seed first observer: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "uid-two", realHead, "observer-b"); err == nil {
		t.Fatal("premise failed: the pre-fix key already admitted a second observer, so this fixture is not pre-fix")
	}

	// Carry one row across the rebuild to prove the copy is not lossy.
	rowsBefore := reviewFindingRowCount(t, store)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate on a pre-fix database: %v", err)
	}

	// ACCOUNT FOR THE COPY IMMEDIATELY, before the post-upgrade probes below add
	// rows of their own. My first attempt counted after them and mis-stated the
	// arithmetic, which is a test bug that would have read as a lossy copy.
	if got, want := reviewFindingRowCount(t, store), rowsBefore-1; got != want {
		t.Fatalf("row count = %d after the rebuild, want %d (%d before, minus the one non-hex row the new CHECK cannot hold)", got, want, rowsBefore)
	}
	var survivingJunk int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM review_finding_observations WHERE head_sha = ?`, nonHex).Scan(&survivingJunk); err != nil {
		t.Fatalf("count junk rows: %v", err)
	}
	if survivingJunk != 0 {
		t.Fatalf("%d non-conforming row(s) survived a CHECK that forbids them", survivingJunk)
	}

	// AFTER: the CHECK covers all forty characters.
	if _, err := store.db.ExecContext(ctx, insert, "uid-after", nonHex, "observer-a"); err == nil {
		t.Fatalf("after upgrade the CHECK still accepted %q, so F9 did not reach this database", nonHex)
	}
	// AFTER: a second observer at one head is admitted.
	if _, err := store.db.ExecContext(ctx, insert, "uid-two", realHead, "observer-b"); err != nil {
		t.Fatalf("after upgrade a second observer was still rejected, so F8 did not reach this database: %v", err)
	}
}

func reviewFindingRowCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM review_finding_observations`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}
