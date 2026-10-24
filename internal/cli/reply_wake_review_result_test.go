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
)

// Review results share the recipient's working hold and idle release policy.
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

func TestReviewResultUsesSameWorkingHoldAsMessages(t *testing.T) {
	store, sink, wake, _ := reviewResultWakeHarness(t)
	insertHeldNoteWake(t, store)
	insertReviewResultWake(t, store)
	now := time.Now().UTC()
	health, err := drainReplyWakeOutboxWithHealth(context.Background(), store, now, replyWakeCoalescingWindow, replyWakeTestDeliveryResolver(sink))
	if err != nil || health.held != 2 || wake.promptCalls != 0 {
		t.Fatalf("working recipient: health=%s prompts=%d err=%v", health, wake.promptCalls, err)
	}
	_, err = drainReplyWakeOutboxWithHealth(context.Background(), store, now.Add(2*time.Minute), replyWakeCoalescingWindow, replyWakeTestDeliveryResolver(sink))
	if err != nil || wake.promptCalls != 2 {
		t.Fatalf("due recipient: prompts=%d err=%v", wake.promptCalls, err)
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
	now := time.Now().UTC().Add(2 * time.Minute)
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
// touch repository work. The assertion is on the outbox itself, not on job
// dispatch, so a host disk guard refusing dispatch cannot change the verdict.
func TestReplyWakeOutboxDrainRunsOutsideRepoSweep(t *testing.T) {
	store, sink, wake, _ := reviewResultWakeHarness(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	insertReviewResultWake(t, store)
	if err := store.InsertWakeOutbox(
		context.Background(), db.WakeOutboxSourceBlocked, "not-json",
		db.WakeOutboxKindBlocked, []string{"owner"},
	); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	worker.EventSinkOverride = sink
	if err := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 1, "", &stdout, time.Now().UTC(), nil, nil,
	); err != nil {
		t.Fatalf("fleet tick: %v", err)
	}
	if strings.Contains(stdout.String(), "reply wake outbox") || wake.promptCalls != 0 {
		t.Fatalf("repo sweep log = %q prompts=%d, want no wake outbox drain inside the sweep", stdout.String(), wake.promptCalls)
	}
	if pending := wakeOutboxRowsBySource(t, store, db.WakeOutboxStatePending); pending[db.WakeOutboxSourceAwaitedFact] != 1 {
		t.Fatalf("pending rows by source after the sweep = %v, want the review result untouched", pending)
	}
	if _, err := drainFleetReplyWakeOutbox(context.Background(), store, worker, time.Now().UTC().Add(2*time.Minute)); err == nil {
		t.Fatal("standalone drain reported a malformed outbox row as healthy")
	}
	if delivered := wakeOutboxRowsBySource(t, store, db.WakeOutboxStateDelivered); delivered[db.WakeOutboxSourceAwaitedFact] != 1 {
		t.Fatalf("delivered rows by source after the standalone drain = %v, want the review result", delivered)
	}
}
