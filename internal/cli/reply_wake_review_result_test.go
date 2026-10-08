package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Owner decision 2026-10-08: a review result skips [org].wake_coalesce_hold,
// every other wake kind keeps it.

func reviewResultWakeHarness(t *testing.T) (*db.Store, synchronousEventRuleTestSink, *fakeEventWake, string) {
	t.Helper()
	store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}, {"lane", "w1:p1"}})
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "fact-lane", OnKind: db.WakeOutboxKindFact, WakeRole: "lane",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	return store, sink, wake, home
}

func insertReviewResultWake(t *testing.T, store *db.Store) {
	t.Helper()
	insertAwaitedFactWakeForTest(t, store, db.AwaitedFactWakePayload{
		ID: 46, WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: "acme/widget#46@head", State: db.AwaitedFactStateSatisfied, JobID: "review-46",
	})
}

func insertHeldNoteWake(t *testing.T, store *db.Store) {
	t.Helper()
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/held", Author: "owner", Body: "status", AddressedTarget: "lane",
	}); err != nil {
		t.Fatal(err)
	}
}

func wakeOutboxRowsBySource(t *testing.T, store *db.Store, state string) map[string]int {
	t.Helper()
	rows, err := store.ListWakeOutbox(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, row := range rows {
		counts[row.SourceKind]++
	}
	return counts
}

func TestReviewResultWakeIsClaimedOnFirstDrainWhileNoteStaysHeld(t *testing.T) {
	store, sink, wake, _ := reviewResultWakeHarness(t)
	insertHeldNoteWake(t, store)
	insertReviewResultWake(t, store)

	health, err := drainReplyWakeOutboxWithHealth(
		context.Background(), store, time.Now().UTC(), replyWakeCoalescingWindow, replyWakeTestDeliveryResolver(sink),
	)
	if err != nil {
		t.Fatalf("drain: %v (health %s)", err, health)
	}
	if wake.promptCalls != 1 || !strings.Contains(wake.prompt, "acme/widget#46@head") {
		t.Fatalf("review result wake = calls=%d prompt=%q, want one immediate wake for the verdict", wake.promptCalls, wake.prompt)
	}
	if delivered := wakeOutboxRowsBySource(t, store, db.WakeOutboxStateDelivered); delivered[db.WakeOutboxSourceAwaitedFact] != 1 || delivered[db.WakeOutboxSourceWorkflowNote] != 0 {
		t.Fatalf("delivered rows by source = %v, want only the review result", delivered)
	}
	if pending := wakeOutboxRowsBySource(t, store, db.WakeOutboxStatePending); pending[db.WakeOutboxSourceWorkflowNote] != 1 {
		t.Fatalf("pending rows by source = %v, want the workflow note still held", pending)
	}
	if health.held != 1 || health.pending != 0 {
		t.Fatalf("health = %s, want only the note held", health)
	}
}

func TestReviewResultWakeIsNotCountedHeld(t *testing.T) {
	store, sink, _, _ := reviewResultWakeHarness(t)
	insertHeldNoteWake(t, store)
	insertReviewResultWake(t, store)

	now := time.Now().UTC()
	health, _ := wakeOutboxObligationHealth(
		context.Background(), store, now.Add(-replyWakeAttemptedUnknownAfter), now,
		replyWakeCoalescingWindow, replyWakeTestDeliveryResolver(sink),
	)
	// The due review result is an outstanding obligation, not a row waiting by
	// design; the note inside its hold is the only held row.
	if health.held != 1 || health.pending != 1 {
		t.Fatalf("health = %s, want held=1 (note) pending=1 (review result)", health)
	}
}

// lockedWakeOutboxSink serializes delivery so concurrent drains can race only
// on the claim, which is the property under test.
type lockedWakeOutboxSink struct {
	mu    *sync.Mutex
	inner synchronousEventRuleTestSink
}

func (s lockedWakeOutboxSink) Emit(ctx context.Context, event events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner.Emit(ctx, event)
}

func (s lockedWakeOutboxSink) emitWakeOutbox(ctx context.Context, event events.Event, rules []db.EventRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.emitWakeOutbox(ctx, event, rules)
}

func TestConcurrentDrainsClaimReviewResultWakeOnce(t *testing.T) {
	store, sink, wake, _ := reviewResultWakeHarness(t)
	insertReviewResultWake(t, store)
	locked := lockedWakeOutboxSink{mu: &sync.Mutex{}, inner: sink}
	resolve := func(ctx context.Context) (replyWakeDelivery, error) {
		rules, err := store.ListEventRules(ctx)
		if err != nil {
			return replyWakeDelivery{}, err
		}
		return replyWakeDelivery{sink: locked, rules: rules}, nil
	}
	now := time.Now().UTC()
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _ = drainReplyWakeOutboxWithHealth(context.Background(), store, now, replyWakeCoalescingWindow, resolve)
		}()
	}
	group.Wait()
	locked.mu.Lock()
	calls := wake.promptCalls
	locked.mu.Unlock()
	if calls != 1 {
		t.Fatalf("prompt calls = %d across concurrent drains, want exactly 1", calls)
	}
	if delivered := wakeOutboxRowsBySource(t, store, db.WakeOutboxStateDelivered); delivered[db.WakeOutboxSourceAwaitedFact] != 1 {
		t.Fatalf("delivered rows by source = %v, want the one review result", delivered)
	}
}

// The wake outbox drains on its own loop, never inside the repository sweep:
// a slow fleet sweep must not delay a wake, and an unhealthy outbox must not
// touch repository work.
func TestReplyWakeOutboxDrainRunsOutsideRepoSweep(t *testing.T) {
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-after-unhealthy-wake", Agent: "audit", Action: "ask",
		Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	if err := store.InsertWakeOutbox(
		context.Background(), db.WakeOutboxSourceBlocked, "not-json",
		db.WakeOutboxKindBlocked, []string{"owner"},
	); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	if err := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 1, "", &stdout, time.Now().UTC(), nil, nil,
	); err != nil {
		t.Fatalf("fleet tick: %v", err)
	}
	job, err := store.GetJob(context.Background(), "job-after-unhealthy-wake")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("repo job state = %q, want succeeded beside an unhealthy wake outbox", job.State)
	}
	if strings.Contains(stdout.String(), "reply wake outbox") {
		t.Fatalf("repo sweep log = %q, want no wake outbox drain inside the sweep", stdout.String())
	}
	if _, err := drainFleetReplyWakeOutbox(context.Background(), store, worker, time.Now().UTC()); err == nil {
		t.Fatal("standalone drain reported a malformed outbox row as healthy")
	}
}
