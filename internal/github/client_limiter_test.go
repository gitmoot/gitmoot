package github

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// A gh call that ULTIMATELY fails with a secondary rate limit must engage the
// shared limiter's process-wide backoff so subsequent calls pause (#683).
func TestRunSecondaryFailureEngagesBackoff(t *testing.T) {
	clock := newFakeClock()
	limiter := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    60 * time.Second,
		MaxBackoff:     5 * time.Minute,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	// Both attempts return the secondary limit + Retry-After, so the call exhausts
	// its retries and finally fails secondary.
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "HTTP 403: secondary rate limit\nRetry-After: 30"},
			{Stderr: "HTTP 403: secondary rate limit\nRetry-After: 30"},
		},
		errs: []error{errors.New("exit 1"), errors.New("exit 1")},
	}
	client := GhClient{
		Runner:     runner,
		Limiter:    limiter,
		MaxRetries: 1,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	}
	_, err := client.GetCombinedStatus(context.Background(), Repository{Owner: "o", Name: "r"}, "sha")
	if err == nil {
		t.Fatalf("expected error on persistent secondary limit")
	}
	state := limiter.Snapshot()
	if !state.InBackoff {
		t.Fatalf("expected limiter in backoff after final secondary failure")
	}
	if state.BackoffRemaining != 30*time.Second {
		t.Fatalf("BackoffRemaining = %s, want 30s (Retry-After honored)", state.BackoffRemaining)
	}
	if state.SecondaryHits != 1 {
		t.Fatalf("SecondaryHits = %d, want 1 (one final failure, not one per attempt)", state.SecondaryHits)
	}
}

// A secondary hit that a retry RECOVERS from must NOT pause the whole process:
// the call succeeds, so the limiter stays clear (byte-identical to today).
func TestRunRecoveredSecondaryDoesNotBackoff(t *testing.T) {
	clock := newFakeClock()
	limiter := NewRateLimiter(RateLimiterConfig{
		BackoffEnabled: true,
		BaseBackoff:    60 * time.Second,
		MaxBackoff:     5 * time.Minute,
		Now:            clock.Now,
		Sleep:          clock.Sleep,
	})
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "HTTP 429: secondary rate limit"},
			{Stdout: `{"id": 42, "state": "pending", "context": "gitmoot/task"}`},
		},
		errs: []error{errors.New("exit 1"), nil},
	}
	client := GhClient{
		Runner:     runner,
		Limiter:    limiter,
		MaxRetries: 1,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	}
	status, err := client.CreateCommitStatus(context.Background(), CommitStatusInput{
		Repo:    Repository{Owner: "o", Name: "r"},
		SHA:     "sha",
		State:   "pending",
		Context: "gitmoot/task",
	})
	if err != nil {
		t.Fatalf("CreateCommitStatus: %v", err)
	}
	if status.ID != 42 {
		t.Fatalf("status = %+v", status)
	}
	if state := limiter.Snapshot(); state.InBackoff {
		t.Fatalf("recovered retry should not engage process-wide backoff: %+v", state)
	}
}

// With an inert (default) limiter, a single gh call adds no scheduling latency —
// no concurrency slot, no spacing, no backoff wait.
func TestRunInertLimiterNoExtraLatency(t *testing.T) {
	clock := newFakeClock()
	limiter := NewRateLimiter(RateLimiterConfig{Now: clock.Now, Sleep: clock.Sleep})
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"nameWithOwner":"o/r"}`}}}
	client := GhClient{Runner: runner, Limiter: limiter}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := clock.totalSlept(); got != 0 {
		t.Fatalf("inert limiter added %s latency, want 0", got)
	}
}
