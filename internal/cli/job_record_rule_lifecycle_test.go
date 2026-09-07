package cli

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
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

// #1942 review, P2. evaluateRules processes matching rules SERIALLY, each
// spending its own herdr probe plus prompt, so a join sized for ONE such
// sequence expired mid-configuration as soon as a second observer rule matched -
// and the command then closed the store while rule two was in flight, producing
// `org event wake counter increment failed ... sql: database is closed`. Same
// operator-facing string as #1938, one layer out.
//
// The join is therefore a PROGRESS watchdog: it extends while rules keep
// completing. This test pins that with two matching observer rules and a
// per-rule window far smaller than the total work, so a total-budget
// implementation cannot pass it.
func TestMultipleObserverRulesExtendTheJoinInsteadOfReleasingTheStore(t *testing.T) {
	home, store := seedTwoObserverRuleHome(t)
	defer store.Close()
	ctx := context.Background()

	const perRule = 150 * time.Millisecond
	wake := &pacedEventWake{delay: perRule}
	sink := &eventRuleSink{store: store, home: home, wake: wake}

	sink.Emit(ctx, events.Event{Type: events.EventJobFinished, JobID: "session-implement-impl-3"})

	// The window MUST sit between one rule's cost and the total: wider than a
	// single rule so honest progress is not punished, narrower than both rules so
	// a TOTAL-budget implementation expires here and only a progress watchdog
	// survives. 4*perRule would have passed either way, which is how the first
	// version of this test failed to pin the fix at all.
	if !sink.waitForPendingRuleWork(3 * perRule / 2) {
		t.Fatalf("join abandoned multi-rule work: %d of 2 rules completed, so closing the store here is what #1942's P2 measured", sink.progress.Load())
	}
	if got := sink.progress.Load(); got < 2 {
		t.Fatalf("progress = %d, want both matching rules evaluated before the store is released", got)
	}
	if sink.storeReleased() {
		t.Fatal("store was marked released even though the work completed")
	}
}

// The other half of the same guard: when work genuinely wedges, the join gives
// up AND the residual work must stop asking the store anything, rather than
// discovering a closed handle inside a counter write.
func TestWedgedRuleWorkIsAbandonedBeforeTheStoreIsTouched(t *testing.T) {
	home, store := seedTwoObserverRuleHome(t)
	defer store.Close()
	ctx := context.Background()

	release := make(chan struct{})
	wake := &pacedEventWake{block: release}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	sink.Emit(ctx, events.Event{Type: events.EventJobFinished, JobID: "session-implement-impl-4"})

	if sink.waitForPendingRuleWork(200 * time.Millisecond) {
		t.Fatal("join reported success while the wake was wedged")
	}
	if !sink.storeReleased() {
		t.Fatal("giving up did not mark the store released, so residual rule work would still query it")
	}
	close(release)
	if !sink.waitForPendingRuleWork(30 * time.Second) {
		t.Fatal("residual work never unwound after release")
	}
	// THE GUARD IS THE POINT: rule one was in flight when the store was released,
	// so rule two must never be evaluated at all. Without the released check the
	// loop proceeds to the next rule and reaches its counter write - which is the
	// closed-store failure #1942's P2 measured, one rule later.
	if got := wake.prompts(); got != 1 {
		t.Fatalf("prompts sent = %d, want 1: rule two was evaluated after the store was released", got)
	}
}

// pacedEventWake reports herdr available and makes each prompt cost a known
// delay, so a test can size work against the join's window. With block set it
// wedges instead, which is the abandon case.
type pacedEventWake struct {
	delay time.Duration
	block chan struct{}
	mu    sync.Mutex
	sent  int
}

func (w *pacedEventWake) Available(context.Context) bool { return true }

func (w *pacedEventWake) AgentPrompt(ctx context.Context, _ string, _ string, _ string) (bool, bool, error) {
	w.mu.Lock()
	w.sent++
	w.mu.Unlock()
	if w.block != nil {
		select {
		case <-w.block:
		case <-ctx.Done():
		}
		// DELIVERED, deliberately: a non-delivery makes evaluateRules return after
		// this rule, which would let the loop exit for a reason unrelated to the
		// release guard and leave that guard unpinned. Reporting delivery keeps the
		// loop alive so the guard is the only thing that can stop rule two.
		return true, false, nil
	}
	select {
	case <-time.After(w.delay):
	case <-ctx.Done():
	}
	return true, false, nil
}

func (w *pacedEventWake) ResolvePaneByLabel(context.Context, string) (string, bool) {
	return "w1:p1", true
}

func (w *pacedEventWake) prompts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sent
}

// seedTwoObserverRuleHome writes the org config the wake path needs (a pane per
// role, or resolveRolePane skips every rule and the serial loop never runs) plus
// two matching observer rules, which is the supported multi-rule configuration
// #1942's P2 was measured against.
func seedTwoObserverRuleHome(t *testing.T) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := `
[org.roles."owner"]
scope=["*"]
pane="w1:p0"
[org.roles."watcher-a"]
parent="owner"
scope=["*"]
pane="w1:pa"
[org.roles."watcher-b"]
parent="owner"
scope=["*"]
pane="w1:pb"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"watcher-a", "watcher-b"} {
		if err := store.AddEventRule(context.Background(), db.EventRule{
			ID: "wake-" + role, OnKind: "job-terminal", WakeRole: role,
			Scope: db.EventRuleScopeObserver, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return home, store
}

// joinProbeEventWake records prompts that RAN TO COMPLETION. The distinction
// matters: a counter incremented on entry is satisfied by a goroutine that has
// merely started, which is exactly the state an unjoined command leaves behind.
type joinProbeEventWake struct {
	delay time.Duration
	mu    sync.Mutex
	done  int
}

func (w *joinProbeEventWake) Available(context.Context) bool { return true }

func (w *joinProbeEventWake) AgentPrompt(ctx context.Context, _, _, _ string) (bool, bool, error) {
	select {
	case <-time.After(w.delay):
	case <-ctx.Done():
		return false, false, ctx.Err()
	}
	w.mu.Lock()
	w.done++
	w.mu.Unlock()
	return true, false, nil
}

func (w *joinProbeEventWake) ResolvePaneByLabel(context.Context, string) (string, bool) {
	return "w1:pa", true
}

func (w *joinProbeEventWake) completed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.done
}

// TestJobRecordJoinsRuleWorkBeforeReleasingItsStore is the test this change was
// MISSING, added when adopting #1938. It pins the CALL SITE rather than the
// mechanism.
//
// Adopting seat's note, because it is why the test exists. All four tests
// shipped with the original PR drive `sink.waitForPendingRuleWork` directly. I
// deleted `waitForEventRuleWork(...)` from runJobRecord, which is the entire
// production fix at the defect's own seam, and every one of them still passed.
// A join nothing calls is not a fix, and a suite that cannot tell the
// difference is not a regression guard.
//
// The observable is ORDERING, which is precisely what the join establishes: the
// detached rule work must have finished before the command returns, because the
// command closes the store on return. The fake pauses long enough that an
// unjoined command wins the race every time.
func TestJobRecordJoinsRuleWorkBeforeReleasingItsStore(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[org.roles."owner"]
scope=["*"]
pane="w1:p0"
[org.roles."watcher-a"]
parent="owner"
scope=["*"]
pane="w1:pa"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	seedSessionAgentRepo(t, store)
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "wake-watcher-a", OnKind: "job-terminal", WakeRole: "watcher-a",
		Scope: db.EventRuleScopeObserver, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// The sink is process-cached per (home, database), so installing one here is
	// what puts a fake wake client on the command's real detached path.
	wake := &joinProbeEventWake{delay: 150 * time.Millisecond}
	sink := &eventRuleSink{store: store, home: paths.Home, wake: wake}
	key := strings.TrimSpace(paths.Home) + "\x00" + store.DatabasePath()
	eventSinkCache.Lock()
	previous, had := eventSinkCache.rules[key]
	eventSinkCache.rules[key] = sink
	eventSinkCache.Unlock()
	t.Cleanup(func() {
		eventSinkCache.Lock()
		if had {
			eventSinkCache.rules[key] = previous
		} else {
			delete(eventSinkCache.rules, key)
		}
		eventSinkCache.Unlock()
		_ = store.Close()
	})

	var stdout, stderr bytes.Buffer
	startedAt := time.Now()
	code := Run([]string{
		"job", "record", "--home", home,
		"--agent", "lead",
		"--repo", "owner/repo",
		"--type", "review",
		"--decision", "approved",
		"--summary", "adopted #1938 call-site guard",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job record exit = %d, stderr=%s", code, stderr.String())
	}

	// Asserted with NO waiting: the point is that the work was already done when
	// the command returned, because after this line the real command has closed
	// the store the work reads.
	// COMPLETED, not started. pacedEventWake counts at entry, which passes with
	// no join at all: measured, an unjoined command returns in 9.8ms with the
	// prompt already "counted" and still sleeping. The join is about the work
	// having FINISHED before the store closes, so that is what is asserted.
	if got := wake.completed(); got != 1 {
		t.Fatalf("detached rule work had completed %d prompt(s) when the command returned, want 1; the command released its store underneath its own wake", got)
	}
	if elapsed := time.Since(startedAt); elapsed < wake.delay {
		t.Fatalf("command returned in %s, faster than the %s the detached work needs; it cannot have joined", elapsed, wake.delay)
	}
}
