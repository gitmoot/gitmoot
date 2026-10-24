package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

// A persistently inert wake outbox must print its health line exactly ONCE
// however long it persists (#1758): inert is a permanent state, so the repeat
// carries no information and used to cost ~12.8k journal lines/day. The drain
// loop returns no error at all, so its health can never reach the worker
// supervisor's escalation ladder.
func TestReplyWakeOutboxInertHealthLogsOnceAcrossDrainLoopTicks(t *testing.T) {
	store := daemonWorkerStore(t)
	insertOptionalBlockedWake(t, store, "owner")

	ctx := context.Background()
	worker := defaultJobWorker(store, io.Discard, t.TempDir())
	stdout := &syncBuffer{}
	loop := newReplyWakeDrainLoop(store, worker, newInflightJobTracker(ctx), stdout)
	now := time.Now().UTC()
	drains := maxConsecutiveWorkerTickFailures + 2
	for index := range drains {
		loop.tick(ctx, now.Add(time.Duration(index)*time.Second))
	}
	if got := strings.Count(stdout.String(), "reply wake outbox drain health:"); got != 1 {
		t.Fatalf("inert health lines = %d over %d drains, want exactly 1; log=%q", got, drains, stdout.String())
	}
	if strings.Contains(stdout.String(), "reply wake outbox drain unhealthy:") ||
		!strings.Contains(stdout.String(), "pending=0 held=0 inert=1 route_removed=0 aged_attempted=0") {
		t.Fatalf("inert reply wake health log = %q", stdout.String())
	}
}

// A held working recipient's batch becomes due without an insert or repo sweep.
func TestReplyWakeOutboxDrainLoopFlushesHeldTailWithZeroEnabledRepos(t *testing.T) {
	store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{
		States:       map[string]org.RoleLiveState{"owner": {State: org.StateWorking}},
		PaneBindings: map[string]org.PaneBinding{"owner": {PaneID: "w1:p1"}},
		Sessions:     map[string]org.SessionActivity{"owner": {Agent: "omp"}},
	}})
	ctx := context.Background()
	for index := range 4 {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "release/tail", Author: "worker", Body: fmt.Sprint(index),
			AddressedTarget: "owner",
		}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil {
		t.Fatal(err)
	}
	oldestAt, err := time.Parse(time.RFC3339Nano, pending[0].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, sink.sink)
	repos, err := store.ListRepos(ctx)
	if err != nil || len(repos) != 0 {
		t.Fatalf("repos = %+v, err=%v; test requires a zero-repo fleet", repos, err)
	}
	loop := newReplyWakeDrainLoop(store, worker, nil, io.Discard)
	preHold := oldestAt.Add(replyWakeCoalescingWindow - time.Millisecond)
	loop.tick(ctx, preHold)
	if wake.promptCalls != 0 {
		t.Fatalf("first tick must drain and hold the tail: prompts=%v", wake.prompts)
	}
	loop.tick(ctx, oldestAt.Add(replyWakeCoalescingWindow))
	if wake.promptCalls != 1 {
		t.Fatalf("tail wake = calls=%d prompt=%q", wake.promptCalls, wake.prompt)
	}
}

// An idle recipient's new review result is eligible on the next short tick.
func TestReplyWakeDrainLoopDeliversNewReviewResultOnNextTick(t *testing.T) {
	store, sink, wake, home := reviewResultWakeHarness(t)
	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, sink.sink)
	loop := newReplyWakeDrainLoop(store, worker, nil, io.Discard)
	ctx := context.Background()
	start := time.Now().UTC()
	loop.tick(ctx, start)
	loop.tick(ctx, start.Add(replyWakeDrainInterval))
	insertReviewResultWake(t, store)
	loop.tick(ctx, start.Add(2*replyWakeDrainInterval))
	if wake.promptCalls != 1 || !strings.Contains(wake.prompt, "acme/widget#46@head") {
		t.Fatalf("review result wake = calls=%d prompt=%q, want delivery on the next tick", wake.promptCalls, wake.prompt)
	}
}

// Stopping the drain loop waits for an in-flight delivery to record its
// outcome, so a supervisor cannot close the store under a claimed batch.
func TestReplyWakeDrainLoopStopWaitsForInFlightDelivery(t *testing.T) {
	store, _, _, home := reviewResultWakeHarness(t)
	insertReviewResultWake(t, store)
	wake := &blockingReplyWake{started: make(chan struct{}), release: make(chan struct{})}
	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, &eventRuleSink{wake: wake})
	stop := startReplyWakeDrainLoop(context.Background(), store, worker, nil, io.Discard)
	select {
	case <-wake.started:
	case <-time.After(10 * time.Second):
		t.Fatal("drain loop did not start delivering the review result")
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while a claimed delivery was still in flight")
	case <-time.After(500 * time.Millisecond):
	}
	close(wake.release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not return after the delivery finished")
	}
	delivered, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 {
		t.Fatalf("delivered = %+v err=%v, want the in-flight wake recorded before stop returned", delivered, err)
	}
}
