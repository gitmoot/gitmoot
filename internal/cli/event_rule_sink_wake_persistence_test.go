package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/events"
)

// #1836 REGRESSION. A wake that was DELIVERED must not decay to
// delivery_unknown because recording it lost a race with the store's write
// lock.
//
// BOTH TESTS ENTER THROUGH evaluateRules' DELIVERED BRANCH, never by calling
// FinishWakeOutbox directly, because the issue's own acceptance shape says so
// and the reason is sound: a test that pins the helper passes even if no
// production path reaches it. The routing mutant that deletes the sink's
// finish call must fail these.

// wakePersistenceFixture stands up an isolated home, store and rule set on a
// REAL database file, because the defect is about the write lock on that file.
// Never the shared /root/.gitmoot home.
func wakePersistenceFixture(t *testing.T) (string, *db.Store, *fakeEventWake, *eventRuleSink) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile,
		[]byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	wake := &fakeEventWake{}
	return home, store, wake, &eventRuleSink{store: store, home: home, wake: wake}
}

// wakeOutboxRow reads the row's state and attempt count straight from the
// database, so the assertion is about what was PERSISTED rather than about what
// the sink returned. ListWakeOutbox takes a state filter; "" is every state,
// which is what a test asserting the state must use.
func wakeOutboxRow(t *testing.T, store *db.Store, id int64) (string, int) {
	t.Helper()
	entries, err := store.ListWakeOutbox(context.Background(), "")
	if err != nil {
		t.Fatalf("list wake outbox: %v", err)
	}
	for _, entry := range entries {
		if entry.ID == id {
			return entry.State, entry.AttemptCount
		}
	}
	t.Fatalf("wake outbox row %d not found", id)
	return "", 0
}

// seedClaimedWake drives the REAL production sequence the daemon uses:
// InsertWakeOutbox persists the obligation, ListWakeOutboxObligations projects
// it, ClaimWakeOutbox moves it to `attempted` and stamps attempt_count, and
// wakeOutboxEvent builds the event the sink consumes. Constructing an
// obligation struct by hand would leave WakeOutboxIDs empty and the row absent,
// so nothing would be persisted for the assertion to be about.
func seedClaimedWake(t *testing.T, store *db.Store, sourceID, role string) (events.Event, int64) {
	t.Helper()
	ctx := context.Background()
	if err := store.InsertWakeOutbox(ctx, db.WakeOutboxSourceWorkflowNote, sourceID,
		db.WakeOutboxKindReply, []string{role}); err != nil {
		t.Fatalf("insert wake outbox: %v", err)
	}
	projection, err := store.ListWakeOutboxObligations(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("list wake outbox obligations: %v", err)
	}
	var batch []db.WakeOutboxObligation
	for _, obligation := range projection.Pending {
		if obligation.SourceID == sourceID {
			batch = append(batch, obligation)
		}
	}
	if len(batch) != 1 {
		t.Fatalf("projected %d obligations for %s, want 1", len(batch), sourceID)
	}
	ids := []int64{batch[0].ID}
	if claimed, err := store.ClaimWakeOutbox(ctx, ids, time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("claim wake outbox: claimed=%v err=%v", claimed, err)
	}
	event, err := wakeOutboxEvent(batch, time.Now())
	if err != nil {
		t.Fatalf("build wake outbox event: %v", err)
	}
	// The claimed ids are attached by the CALLER in production, at
	// reply_wake_outbox.go:177 immediately after ClaimWakeOutbox succeeds, not
	// by wakeOutboxEvent. Mirroring that here keeps the fixture faithful to the
	// sequence the daemon actually executes.
	event.WakeOutboxIDs = ids
	return event, ids[0]
}

// THE DEFECT ITSELF. A delivered wake whose terminal write contends with
// another writer for LONGER THAN THE OLD 5s PROBE DEADLINE must still be
// recorded delivered, not left `attempted` for the age-out sweep to relabel
// delivery_unknown and never retry.
//
// The contention is real: a second connection on the same file holds a write
// transaction, which is exactly the shape the live daemon hits with
// SetMaxOpenConns(1) and several write-capable openers of one database.
func TestDeliveredWakeSurvivesContendedTerminalWrite(t *testing.T) {
	home, store, wake, sink := wakePersistenceFixture(t)
	_ = home
	_ = wake
	ctx := context.Background()

	event, outboxID := seedClaimedWake(t, store, "note-1836", "owner")

	// WHAT THIS TEST COVERS, AND WHAT IT DOES NOT. It drives the real delivered
	// branch end to end while the write lock is genuinely held by a second
	// connection, and asserts the row settles `delivered` with ONE attempt and
	// ONE wake. That is the invariant #1836's fix had to preserve: recording is
	// what must survive contention, delivery must never be repeated.
	//
	// It does NOT discriminate the deadline value, and I measured that rather
	// than assuming it: a mutant restoring the old 5s budget survives this test
	// at 7s, 12s, 17s and two-sequential-hold contention. The reason is
	// structural. The delivered branch resets the missed-wake counter FIRST,
	// that write waits on the lock, and the driver returns only when the lock
	// frees, so the counter absorbs the whole stall and the terminal write then
	// runs on a free lock. From outside the sink there is no way to interleave a
	// second hold between two writes of one sequential path.
	//
	// The budget itself is pinned where it is observable, at the store boundary:
	// db.TestShortCallerBudgetDiscardsAWriteItAlreadyWon holds the lock for a
	// fixed 8s and varies ONLY the caller's budget, and a mutant shrinking
	// DurableWriteBudget back to 5s kills it.
	const hold = 3 * time.Second
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	defer raw.Close()
	blocker, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocking transaction: %v", err)
	}
	if _, err := blocker.ExecContext(ctx,
		`UPDATE wake_outbox SET updated_at = updated_at WHERE id = ?`, outboxID); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	go func() {
		time.Sleep(hold)
		_ = blocker.Rollback()
	}()

	rules := []db.EventRule{{
		ID: "rule-1836", OnKind: "reply", WakeRole: "owner",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}}
	start := time.Now()
	if err := sink.evaluateRules(ctx, event, rules); err != nil {
		t.Fatalf("evaluateRules returned error: %v", err)
	}

	if elapsed := time.Since(start); elapsed < hold {
		t.Fatalf("the delivered branch returned in %s, before the %s hold elapsed; this test did not exercise a contended write",
			elapsed, hold)
	}
	state, attempts := wakeOutboxRow(t, store, outboxID)
	if state != db.WakeOutboxStateDelivered {
		t.Fatalf("wake outbox row state = %q after a DELIVERED wake whose record contended; want %q. Left in this state the age-out sweep relabels it delivery_unknown with policy=expire_without_retry, which is #1836",
			state, db.WakeOutboxStateDelivered)
	}
	// THE INVARIANT THE FIX HAD TO PRESERVE: recording was retried, delivery
	// was not. One attempt, one wake.
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want 1; a contended RECORD must never turn into a repeated DELIVERY", attempts)
	}
	if len(wake.panes) != 1 {
		t.Fatalf("woke %d panes, want 1; the wake must not be re-sent because its record was contended", len(wake.panes))
	}
}

// POSITIVE CONTROL, so the guard above cannot be satisfied by making every
// delivery expensive or repeated: an UNCONTENDED delivery still settles
// delivered in one attempt, with one wake, and promptly.
func TestOrdinaryDeliveredWakeStaysSingleAttempt(t *testing.T) {
	_, store, wake, sink := wakePersistenceFixture(t)
	ctx := context.Background()

	event, outboxID := seedClaimedWake(t, store, "note-plain", "owner")
	rules := []db.EventRule{{
		ID: "rule-plain", OnKind: "reply", WakeRole: "owner",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}}
	start := time.Now()
	if err := sink.evaluateRules(ctx, event, rules); err != nil {
		t.Fatalf("evaluateRules returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("an uncontended delivery took %s; the fix must not make the ordinary path wait on its budget", elapsed)
	}
	state, attempts := wakeOutboxRow(t, store, outboxID)
	if state != db.WakeOutboxStateDelivered {
		t.Fatalf("uncontended wake state = %q, want %q", state, db.WakeOutboxStateDelivered)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count = %d, want 1", attempts)
	}
	if len(wake.panes) != 1 {
		t.Fatalf("woke %d panes, want 1", len(wake.panes))
	}
}
