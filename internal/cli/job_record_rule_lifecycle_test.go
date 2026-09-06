package cli

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/events"
)

// #1938. `gitmoot job record` logged
// `org event rules list failed ... error="sql: database is closed"` AFTER the row
// was committed and rendered, on three of three invocations.
//
// The write was never the problem. CloseExternalJobWithUsage emits the terminal
// job event; a `job-terminal` kind is NOT in durableWakeKind's set, so the sink
// takes the DETACHED path and spawns a goroutine that reads event_rules from the
// command's own store. withStoreAndPaths closes that store as soon as the command
// function returns, so the read raced a closed database: success on stdout, exit
// 0, a warning the operator is invited to ignore, and NO wake for the subscribed
// role. A coordinator waiting on that wake waits forever in front of a green
// command.
//
// The two tests below pin the two halves that must both hold. They are
// deliberately about the SINK'S LIFECYCLE rather than about log text: the first
// asserts detached work is joinable before the store closes, the second asserts a
// genuine lookup failure still reaches the operator. Asserting only the first is
// how "stop logging it" passes for a fix.

// A successful record must leave no detached rule work reading a closed store.
// The join is what makes that true, so the test drives the real sink, spawns the
// real detached goroutine, and then asserts the wait actually observed it -
// closing the store only afterwards, exactly as the fixed command path does.
func TestEventRuleWorkIsJoinableBeforeTheStoreCloses(t *testing.T) {
	home := t.TempDir()
	store, err := dbtest.Open(t, filepath.Join(home, "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "wake-terminal", OnKind: "job-terminal", WakeRole: "watcher",
		Scope: db.EventRuleScopeObserver, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	released := make(chan struct{})
	wake := &blockingEventWake{released: released}
	sink := &eventRuleSink{store: store, home: home, wake: wake}

	sink.Emit(ctx, events.Event{Type: events.EventJobFinished, JobID: "session-implement-impl-1"})

	// The detached goroutine is now in flight and holding the store. A zero-length
	// join must therefore REPORT INCOMPLETE rather than lying: that is the signal
	// the command path relies on to know rule work is outstanding.
	if sink.waitForPendingRuleWork(0) {
		t.Fatal("join reported complete while the detached wake was still running; the command would close the store underneath it")
	}
	close(released)
	if !sink.waitForPendingRuleWork(30 * time.Second) {
		t.Fatal("join never completed after the wake was released")
	}
	if got := wake.calls(); got == 0 {
		t.Fatal("detached rule work never reached the wake client, so the join proved nothing")
	}
	// Only now is closing safe - which is the ordering the fix establishes.
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

// A GENUINE rules-lookup failure must still be reported. This is the half a
// silencing "fix" would break, and it is asserted at the seam where the failure
// can actually occur: the store is OPEN and healthy enough to have built the
// sink, and the rules read then fails on its own.
//
// Note what this cannot be done through the command end-to-end: the SAME
// ListEventRules call gates sink construction (resolveDaemonEventSink), so a
// permanently unreadable event_rules table yields no rule sink at all and hence
// no report, while a transiently unreadable one requires winning a microsecond
// window between construction and emit. The reporting path is therefore
// reachable only for a failure that appears AFTER construction, which is exactly
// what dropping the table here simulates.
func TestGenuineRuleLookupFailureIsStillReported(t *testing.T) {
	home := t.TempDir()
	store, err := dbtest.Open(t, filepath.Join(home, "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "wake-terminal", OnKind: "job-terminal", WakeRole: "watcher",
		Scope: db.EventRuleScopeObserver, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Construction-time readability is established above; the failure is injected
	// only now, so the read inside the detached evaluation is the one that breaks.
	raw, err := sql.Open("sqlite", filepath.Join(home, "gitmoot.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(ctx, "DROP TABLE event_rules"); err != nil {
		t.Fatalf("inject rules-read failure: %v", err)
	}
	raw.Close()

	var buf lockedBuffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	sink := &eventRuleSink{store: store, home: home, wake: &fakeEventWake{}}
	sink.Emit(ctx, events.Event{Type: events.EventJobFinished, JobID: "session-implement-impl-2"})
	if !sink.waitForPendingRuleWork(30 * time.Second) {
		t.Fatal("detached rule work did not settle")
	}

	logged := buf.String()
	if !strings.Contains(logged, "org event rules list failed") {
		t.Fatalf("a genuine rules-lookup failure was not reported; log = %q", logged)
	}
	if !strings.Contains(logged, "session-implement-impl-2") {
		t.Fatalf("the report does not name the job whose wake was lost; log = %q", logged)
	}
	// And it must be reported as a rules problem, not as a closed store: the
	// closed-store wording is what #1938 taught operators to ignore.
	if strings.Contains(logged, "database is closed") {
		t.Fatalf("a live-store lookup failure reported itself as a closed database; log = %q", logged)
	}
}

// blockingEventWake holds the detached goroutine inside the sink until released,
// so a test can observe the window in which the store must stay open.
type blockingEventWake struct {
	released chan struct{}
	mu       sync.Mutex
	seen     int
}

func (w *blockingEventWake) Available(ctx context.Context) bool {
	w.mu.Lock()
	w.seen++
	w.mu.Unlock()
	select {
	case <-w.released:
	case <-ctx.Done():
	}
	return false
}

func (w *blockingEventWake) AgentPrompt(context.Context, string, string, string) (bool, bool, error) {
	return false, false, nil
}

func (w *blockingEventWake) ResolvePaneByLabel(context.Context, string) (string, bool) {
	return "", false
}

func (w *blockingEventWake) calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}
