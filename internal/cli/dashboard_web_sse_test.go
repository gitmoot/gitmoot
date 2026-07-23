package cli

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

func TestWebDataSourceSubscribeSharesPollerAndSeedsLateJoiner(t *testing.T) {
	var stateCalls atomic.Int64
	ds := &webDataSource{
		ssePollInterval: 20 * time.Millisecond,
		sseState: func(context.Context, string) (dashboard.State, error) {
			stateCalls.Add(1)
			return dashboard.State{RunID: "root", Title: "cached"}, nil
		},
	}

	first, cancelFirst, err := ds.Subscribe(context.Background(), "root")
	if err != nil {
		t.Fatalf("Subscribe(first): %v", err)
	}
	defer cancelFirst()
	want := receiveSSEState(t, first)

	second, cancelSecond, err := ds.Subscribe(context.Background(), "root")
	if err != nil {
		t.Fatalf("Subscribe(second): %v", err)
	}
	defer cancelSecond()
	started := time.Now()
	if got := receiveSSEState(t, second); got.RunID != want.RunID || got.Title != want.Title {
		t.Fatalf("late-join seed = %+v, want %+v", got, want)
	}
	if elapsed := time.Since(started); elapsed >= ds.ssePollInterval {
		t.Fatalf("late joiner waited %v for cached state (poll interval %v)", elapsed, ds.ssePollInterval)
	}

	ds.registryMu.Lock()
	poller := ds.pollers["root"]
	pollerCount := len(ds.pollers)
	ds.registryMu.Unlock()
	if pollerCount != 1 || poller == nil {
		t.Fatalf("poller registry = %d entries, root=%v; want one shared poller", pollerCount, poller != nil)
	}
	poller.mu.Lock()
	refs := poller.refs
	poller.mu.Unlock()
	if refs != 2 {
		t.Fatalf("shared poller refs = %d, want 2", refs)
	}

	// The single shared poller keeps rebuilding on its own ticker; pollerCount==1
	// above already proves two viewers do NOT get one rebuild loop each.
	if !waitForCondition(t, time.Second, func() bool { return stateCalls.Load() >= 3 }) {
		t.Fatal("shared poller did not keep rebuilding")
	}
}

func TestWebDataSourceSubscribeSharedFanoutOnChange(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(),
		db.Job{ID: "root", Agent: "worker", Type: "ask", State: "running"},
		db.JobEvent{Kind: "running", Message: "initial"}); err != nil {
		store.Close()
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	store.Close()

	ds := &webDataSource{home: home, ssePollInterval: 20 * time.Millisecond}
	ds.sseState = func(ctx context.Context, runID string) (dashboard.State, error) {
		return ds.State(ctx, runID)
	}

	first, cancelFirst, err := ds.Subscribe(context.Background(), "root")
	if err != nil {
		t.Fatalf("Subscribe(first): %v", err)
	}
	initial := receiveSSEState(t, first)
	if len(initial.Nodes) != 1 || len(initial.Nodes[0].Events) != 1 {
		t.Fatalf("initial state = %+v, want one node with one event", initial)
	}

	second, cancelSecond, err := ds.Subscribe(context.Background(), "root")
	if err != nil {
		cancelFirst()
		t.Fatalf("Subscribe(second): %v", err)
	}
	_ = receiveSSEState(t, second)
	ds.registryMu.Lock()
	poller := ds.pollers["root"]
	ds.registryMu.Unlock()
	t.Cleanup(func() {
		cancelFirst()
		cancelSecond()
		select {
		case <-poller.done:
		case <-time.After(time.Second):
			t.Error("SSE poller did not stop during cleanup")
		}
	})

	store, err = db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID: "root", Kind: "progress", Message: "changed",
	}); err != nil {
		store.Close()
		t.Fatalf("AddJobEvent: %v", err)
	}
	store.Close()

	for i, ch := range []<-chan dashboard.State{first, second} {
		got := receiveSSEState(t, ch)
		if len(got.Nodes) != 1 || len(got.Nodes[0].Events) != 2 {
			t.Fatalf("subscriber %d update = %+v, want two events", i, got)
		}
	}
}

// TestWebDataSourceSubscribeCoalescingLatestWins proves F2's fix: a subscriber
// that lags behind a burst of distinct snapshots receives the LATEST state, not
// a stale queued one, and never freezes (buffer-1 drain-then-send fanout).
func TestWebDataSourceSubscribeCoalescingLatestWins(t *testing.T) {
	var ticks atomic.Int64
	ds := &webDataSource{
		ssePollInterval: 5 * time.Millisecond,
		sseState: func(context.Context, string) (dashboard.State, error) {
			// A distinct fingerprint every tick (event count grows) so every tick
			// is a real change that the poller pushes.
			n := int(ticks.Add(1))
			return dashboard.State{RunID: "root", Nodes: []dashboard.Node{
				{ID: "root", State: "running", Events: make([]dashboard.Event, n)},
			}}, nil
		},
	}
	ch, cancel, err := ds.Subscribe(context.Background(), "root")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// Do NOT read while the poller pushes several distinct snapshots into the
	// buffer-1 channel; coalescing must keep only the newest.
	if !waitForCondition(t, time.Second, func() bool { return ticks.Load() >= 6 }) {
		t.Fatal("poller did not produce enough ticks")
	}
	got := receiveSSEState(t, ch)
	if len(got.Nodes) == 0 || len(got.Nodes[0].Events) < 5 {
		t.Fatalf("coalescing delivered a stale snapshot with %d events; want the latest (>=5)", len(got.Nodes[0].Events))
	}
}

func TestWebDataSourceSubscribeRefCountTeardownAndRestart(t *testing.T) {
	ds := newFakeSSEDataSource(time.Hour)
	first, cancelFirst, err := ds.Subscribe(context.Background(), "root")
	if err != nil {
		t.Fatalf("Subscribe(first): %v", err)
	}
	_ = receiveSSEState(t, first)
	second, cancelSecond, err := ds.Subscribe(context.Background(), "root")
	if err != nil {
		t.Fatalf("Subscribe(second): %v", err)
	}
	_ = receiveSSEState(t, second)

	ds.registryMu.Lock()
	original := ds.pollers["root"]
	ds.registryMu.Unlock()
	cancelFirst()
	ds.registryMu.Lock()
	remaining := len(ds.pollers)
	ds.registryMu.Unlock()
	if remaining != 1 {
		t.Fatalf("pollers after first unsubscribe = %d, want 1", remaining)
	}

	cancelSecond()
	ds.registryMu.Lock()
	remaining = len(ds.pollers)
	ds.registryMu.Unlock()
	if remaining != 0 {
		t.Fatalf("pollers after last unsubscribe = %d, want 0", remaining)
	}
	select {
	case <-original.done:
	case <-time.After(time.Second):
		t.Fatal("poller goroutine did not stop after last unsubscribe")
	}

	third, cancelThird, err := ds.Subscribe(context.Background(), "root")
	if err != nil {
		t.Fatalf("Subscribe(after teardown): %v", err)
	}
	_ = receiveSSEState(t, third)
	ds.registryMu.Lock()
	restarted := ds.pollers["root"]
	ds.registryMu.Unlock()
	if restarted == nil || restarted == original {
		t.Fatal("subscribe after teardown did not start a fresh poller")
	}
	cancelThird()
	select {
	case <-restarted.done:
	case <-time.After(time.Second):
		t.Fatal("restarted poller did not stop")
	}
}

func TestWebDataSourceSubscribeSubscriberCap(t *testing.T) {
	ds := newFakeSSEDataSource(time.Hour)
	cancels := make([]func(), 0, sseMaxSubscribersPerRun)
	channels := make([]<-chan dashboard.State, 0, sseMaxSubscribersPerRun)
	for i := 0; i < sseMaxSubscribersPerRun; i++ {
		ch, cancel, err := ds.Subscribe(context.Background(), "root")
		if err != nil {
			t.Fatalf("Subscribe(%d): %v", i, err)
		}
		channels = append(channels, ch)
		cancels = append(cancels, cancel)
	}
	if ch, cancel, err := ds.Subscribe(context.Background(), "root"); err == nil || ch != nil || cancel != nil {
		t.Fatalf("over-cap Subscribe returned channel=%t cancel=%t err=%v; want false, false, error", ch != nil, cancel != nil, err)
	}

	for i, ch := range channels {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Fatalf("existing subscriber %d closed by over-cap request", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("existing subscriber %d did not receive initial state", i)
		}
	}
	for _, cancel := range cancels {
		cancel()
	}
}

func TestWebDataSourceSubscribeConcurrentChurn(t *testing.T) {
	ds := newFakeSSEDataSource(time.Millisecond)
	const workers = 16
	const iterations = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ch, cancel, err := ds.Subscribe(context.Background(), "root")
				if err != nil {
					errs <- err
					return
				}
				select {
				case <-ch:
				case <-time.After(time.Second):
					errs <- fmt.Errorf("timed out waiting for SSE state")
					cancel()
					return
				}
				cancel()
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if !waitForCondition(t, time.Second, func() bool {
		ds.registryMu.Lock()
		defer ds.registryMu.Unlock()
		return len(ds.pollers) == 0
	}) {
		t.Fatal("pollers were not torn down after concurrent churn")
	}
}

func newFakeSSEDataSource(interval time.Duration) *webDataSource {
	return &webDataSource{
		ssePollInterval: interval,
		sseState: func(context.Context, string) (dashboard.State, error) {
			return dashboard.State{RunID: "root", Title: "state"}, nil
		},
	}
}

func receiveSSEState(t *testing.T, ch <-chan dashboard.State) dashboard.State {
	t.Helper()
	select {
	case state, ok := <-ch:
		if !ok {
			t.Fatal("SSE channel closed before state arrived")
		}
		return state
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE state")
		return dashboard.State{}
	}
}
