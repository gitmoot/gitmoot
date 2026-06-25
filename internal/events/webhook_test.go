package events

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookSinkDeliversExactJSON(t *testing.T) {
	received := make(chan Event, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		var ev Event
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("Unmarshal body: %v", err)
		}
		received <- ev
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewWebhookSink(srv.URL, time.Second)
	if sink == nil {
		t.Fatal("NewWebhookSink returned nil for a valid URL")
	}
	want := NewEvent(EventJobNeedsAttention, "job-1", "root-1", "jerryfane/gitmoot", "awaiting_human", "please decide", time.Now(), nil)
	sink.Emit(context.Background(), want)

	select {
	case got := <-received:
		if got.Type != EventJobNeedsAttention || got.JobID != "job-1" || got.RootID != "root-1" || got.Repo != "jerryfane/gitmoot" || got.Status != "awaiting_human" || got.Detail != "please decide" {
			t.Fatalf("received event = %+v, want %+v", got, want)
		}
		if got.SchemaVersion != 1 {
			t.Fatalf("schema_version = %d, want 1", got.SchemaVersion)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook consumer never received the event")
	}
}

func TestWebhookSinkNilForEmptyURL(t *testing.T) {
	if sink := NewWebhookSink("", time.Second); sink != nil {
		t.Fatal("NewWebhookSink(\"\") must return nil so the daemon treats it as no sink")
	}
}

func TestWebhookSinkSlowConsumerNeverBlocksEmit(t *testing.T) {
	// A handler that blocks well past the sink timeout proves Emit returns
	// immediately (fire-and-forget) — the core best-effort guarantee that a hung
	// consumer never stalls a job.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // block until the test releases it
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	sink := NewWebhookSink(srv.URL, 50*time.Millisecond)
	dropped := make(chan string, 4)
	sink.OnDrop = func(_ Event, reason string) { dropped <- reason }

	start := time.Now()
	for i := 0; i < 4; i++ {
		sink.Emit(context.Background(), NewEvent(EventJobFinished, "job", "root", "o/r", "succeeded", "", time.Now(), nil))
	}
	elapsed := time.Since(start)
	// Four fire-and-forget Emits must return effectively instantly, far below the
	// 50ms per-request timeout (let alone the handler's indefinite block).
	if elapsed > 40*time.Millisecond {
		t.Fatalf("Emit blocked for %s; must be fire-and-forget", elapsed)
	}
}

func TestWebhookSinkHungHandlerDoesNotBlockBeyondTimeout(t *testing.T) {
	// The drain goroutine bounds each POST by the http.Client timeout; a hung
	// handler results in a transport-timeout drop, never an indefinite stall.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	sink := NewWebhookSink(srv.URL, 50*time.Millisecond)
	dropped := make(chan string, 1)
	sink.OnDrop = func(_ Event, reason string) { dropped <- reason }
	sink.Emit(context.Background(), NewEvent(EventJobFinished, "job", "root", "o/r", "succeeded", "", time.Now(), nil))

	select {
	case reason := <-dropped:
		if reason != "transport error" {
			t.Fatalf("drop reason = %q, want transport error (timeout)", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hung handler should have produced a timeout drop")
	}
}

func TestWebhookSinkBadStatusDropsWithoutError(t *testing.T) {
	var dropReason atomic.Value
	dropped := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink := NewWebhookSink(srv.URL, time.Second)
	sink.OnDrop = func(_ Event, reason string) {
		dropReason.Store(reason)
		dropped <- struct{}{}
	}
	// Emit must not panic or return an error path; the job is unaffected.
	sink.Emit(context.Background(), NewEvent(EventJobFailed, "job", "root", "o/r", "failed", "boom", time.Now(), nil))

	select {
	case <-dropped:
		if r, _ := dropReason.Load().(string); r != "non-2xx response" {
			t.Fatalf("drop reason = %q, want non-2xx response", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("500 response should have produced a drop")
	}
}

func TestWebhookSinkRefusedConnectionDropsWithoutError(t *testing.T) {
	// A closed server (refused connection) is a transport error: dropped, never
	// fatal.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing is listening

	sink := NewWebhookSink(url, 200*time.Millisecond)
	dropped := make(chan string, 1)
	sink.OnDrop = func(_ Event, reason string) { dropped <- reason }
	sink.Emit(context.Background(), NewEvent(EventJobBlocked, "job", "root", "o/r", "blocked", "", time.Now(), nil))

	select {
	case reason := <-dropped:
		if reason != "transport error" {
			t.Fatalf("drop reason = %q, want transport error", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refused connection should have produced a transport drop")
	}
}

func TestWebhookSinkConcurrentEmitIsRaceClean(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewWebhookSink(srv.URL, time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				sink.Emit(context.Background(), NewEvent(EventJobFinished, "job", "root", "o/r", "succeeded", "", time.Now(), nil))
			}
		}()
	}
	wg.Wait()
	// Give the single drain goroutine time to flush whatever fit in the buffer.
	// We don't assert an exact count (drop-on-full is allowed); the point is the
	// -race detector finds no data race across concurrent Emit + drain.
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt64(&count) == 0 {
		t.Fatal("expected at least some events delivered under concurrency")
	}
}
