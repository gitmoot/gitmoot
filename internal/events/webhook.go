package events

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultWebhookFlushTimeout bounds how long Flush waits for the drain goroutine
// to deliver the already-queued events before a short-lived caller (a CLI command)
// returns. It is generous relative to a single POST (DefaultWebhookTimeout) so a
// small queue drains, but still bounded so a hung consumer can never wedge the
// process on exit.
const DefaultWebhookFlushTimeout = 5 * time.Second

// DefaultWebhookTimeout bounds a single outbound POST so a hung consumer can
// never stall the drain goroutine indefinitely. It is the fallback when
// [events].timeout is unset.
const DefaultWebhookTimeout = 2 * time.Second

// defaultWebhookBuffer bounds the in-flight event queue. On a full buffer Emit
// drops (never blocks the caller); the drop is surfaced via the OnDrop hook so
// the daemon can record a single best-effort event_sink_drop job event without
// this package importing the db layer.
const defaultWebhookBuffer = 256

// webhookSink is the pilot Sink: it POSTs each event as application/json to one
// configured URL. Emit is fire-and-forget — it hands the event to a small
// buffered channel drained by ONE background goroutine, so a slow/hung/erroring
// consumer (bounded by the http.Client timeout) never blocks or fails a job. On
// a full buffer or any transport error the event is dropped (best-effort) and
// OnDrop, if set, is invoked. There is no outbox/retry: at-least-once delivery
// is the documented graduate step.
type webhookSink struct {
	url    string
	client *http.Client
	queue  chan queued
	// done is closed by the drain goroutine when it has finished delivering every
	// queued event (i.e. after queue is closed and emptied). Flush waits on it.
	done chan struct{}
	// closeOnce guards the single queue close so Flush is idempotent and a
	// post-Flush Emit can never panic by sending on a closed channel.
	closeOnce sync.Once
	// closed is set under mu when the queue has been closed; Emit reads it to drop
	// (rather than send on a closed channel) after a Flush.
	mu     sync.Mutex
	closed bool
	// OnDrop, when set, is called best-effort when an event is dropped (full
	// buffer or transport failure). The daemon wires it to record a single
	// event_sink_drop job event. It must itself be non-blocking/best-effort.
	OnDrop func(event Event, reason string)
}

type queued struct {
	event Event
}

// NewWebhookSink constructs a webhook Sink that POSTs events to url with a
// bounded per-request timeout (DefaultWebhookTimeout when timeout <= 0) and
// starts its single drain goroutine. It returns nil when url is empty so the
// caller (the daemon) treats an unconfigured webhook as "no sink" — preserving
// the off-by-default, byte-identical guarantee (a nil Sink is a no-op).
func NewWebhookSink(url string, timeout time.Duration) *webhookSink {
	if url == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	s := &webhookSink{
		url:    url,
		client: &http.Client{Timeout: timeout},
		queue:  make(chan queued, defaultWebhookBuffer),
		done:   make(chan struct{}),
	}
	go s.drain()
	return s
}

// Emit enqueues event for delivery and returns immediately. If the buffer is
// full it drops the event (best-effort) rather than blocking the caller — the
// core best-effort guarantee that a dead/slow consumer never stalls a job. ctx
// is honored only as a courtesy: a cancelled ctx drops rather than blocks.
func (s *webhookSink) Emit(ctx context.Context, event Event) {
	if s == nil {
		return
	}
	// After Flush has closed the queue a send would panic; drop instead. Holding mu
	// across the send keeps the closeOnce in Flush from racing the send.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.dropped(event, "sink flushed")
		return
	}
	select {
	case s.queue <- queued{event: event}:
	case <-ctx.Done():
		s.dropped(event, "context cancelled")
	default:
		// Buffer full: drop rather than block the caller.
		s.dropped(event, "buffer full")
	}
	s.mu.Unlock()
}

// Flush closes the queue and waits — bounded by ctx and DefaultWebhookFlushTimeout
// — for the drain goroutine to deliver every already-enqueued event, then returns.
// It is the synchronous counterpart to the fire-and-forget Emit, for SHORT-LIVED
// callers (a CLI command) that would otherwise exit and destroy the queued POSTs
// before the background goroutine runs. It is idempotent (safe to call twice / via
// defer) and nil-safe. The daemon, which reuses one long-lived cached sink across
// the whole process, must NEVER Flush per-call — only the per-invocation CLI sink
// is flushed.
func (s *webhookSink) Flush(ctx context.Context) {
	if s == nil {
		return
	}
	// Close the queue exactly once so drain can range to completion; guard with mu
	// so a concurrent Emit drops instead of sending on the closed channel.
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.queue)
		s.mu.Unlock()
	})
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(DefaultWebhookFlushTimeout)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// drain is the single goroutine that serializes delivery. Running one goroutine
// (rather than a goroutine per Emit) bounds outbound concurrency and keeps the
// channel the only synchronization point, so concurrent Emit from many workers
// is race-clean.
func (s *webhookSink) drain() {
	defer close(s.done)
	for q := range s.queue {
		s.post(q.event)
	}
}

func (s *webhookSink) post(event Event) {
	body, err := json.Marshal(event)
	if err != nil {
		s.dropped(event, "marshal failed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		s.dropped(event, "request build failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		s.dropped(event, "transport error")
		return
	}
	defer resp.Body.Close()
	// Drain and discard the body so the connection can be reused; the consumer's
	// response content is irrelevant to a fire-and-forget transport.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		s.dropped(event, "non-2xx response")
	}
}

func (s *webhookSink) dropped(event Event, reason string) {
	if s.OnDrop != nil {
		s.OnDrop(event, reason)
	}
}
