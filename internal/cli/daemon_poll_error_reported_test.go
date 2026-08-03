package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/github"
)

// writerFunc adapts a func to io.Writer so the test can observe the supervisor's stdout as it
// is written and stop the loop the moment it has proved what it needs, rather than guessing a
// duration.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestSingleRepoSupervisorReportsPollErrorAndKeepsPolling guards the REAL production path.
//
// The previous round's guard drove daemon.Daemon.Run, which has NO production callers --
// `gitmoot daemon run --repo` reaches runSingleRepoSupervisor via daemon_lifecycle.go:397.
// Review demonstrated the consequence: a mutant restoring BOTH single-repo call sites to
// discarded errors left that test green. Two production sites were changed and only the one
// nothing calls was guarded, which is a test for dead code presented as coverage.
//
// The two halves are asserted together because they are in tension, and satisfying either
// alone is a regression:
//
//	REPORTED  - a fix that surfaces the error by returning from the loop would satisfy
//	            "reported" and break the watcher.
//	CONTINUES - the pre-existing behaviour, which is exactly what was insufficient: the
//	            refusal #1381 introduced produced no log line, no escalation note and no
//	            persisted error, so the task stayed retryable and failed identically every
//	            interval with nothing emitted.
func TestSingleRepoSupervisorReportsPollErrorAndKeepsPolling(t *testing.T) {
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	pollFailure := errors.New("merge reason: a gate miss requires a gate or a cause; an all-blank miss is a caller defect")
	client := &countingPollFakeGitHub{err: pollFailure}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var out strings.Builder
	sink := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		out.Write(p)
		mu.Unlock()
		// Stop once the poll has demonstrably run TWICE. Continuation is proved by the
		// POLL recurring, not by the report recurring -- those are separate facts, and
		// the reporter deliberately rate-limits identical causes, so counting log lines
		// would measure the rate limiter rather than the loop.
		if client.calls() >= 2 {
			cancel()
		}
		return len(p), nil
	})

	live := newDaemonReloadableConfig(200*time.Millisecond, 1, false)
	d := daemon.Daemon{
		Repo:         github.Repository{Owner: "owner", Name: "repo"},
		Store:        store,
		GitHub:       client,
		PollInterval: 200 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() { done <- runSingleRepoSupervisor(ctx, home, d, store, live, "", sink) }()

	deadline := time.After(45 * time.Second)
	for client.calls() < 2 {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("supervisor returned %v, want context.Canceled", err)
			}
		case <-deadline:
			cancel()
			mu.Lock()
			got := out.String()
			mu.Unlock()
			t.Fatalf("poll ran %d times in 45s, want >= 2; stdout=%q", client.calls(), got)
		case <-time.After(20 * time.Millisecond):
		}
	}
	// Cancel, then JOIN. The previous version cancelled and never received from done, which
	// left the supervisor goroutine running into the temp-store teardown -- a test leaking
	// work into another test's environment.
	//
	// The join is lifecycle cleanup, not an assertion about shutdown LATENCY: draining the
	// in-flight tracker and worker loop is a separate property with its own timeouts, and this
	// guard does not claim it. The bound is generous for that reason, and both assertions
	// below were established while the loop was still running.
	cancel()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Error("supervisor goroutine did not exit within 90s of cancellation; it is leaking into store teardown")
	}

	mu.Lock()
	got := out.String()
	mu.Unlock()

	// REPORTED, carrying the cause. Asserting only that SOMETHING was written would pass
	// against a loop that logs every tick regardless of outcome.
	if !strings.Contains(got, "poll error") {
		t.Fatalf("no poll error was reported; a refusal nobody can observe is a panic traded for silence.\nstdout=%q", got)
	}
	if !strings.Contains(got, pollFailure.Error()) {
		t.Fatalf("reported line does not carry the underlying cause %q.\nstdout=%q", pollFailure.Error(), got)
	}
	// CONTINUES: the poll ran again after the failure was reported.
	if n := client.calls(); n < 2 {
		t.Fatalf("poll ran %d times, want >= 2: a reported error must not stop the watcher.\nstdout=%q", n, got)
	}
}

// countingPollFakeGitHub fails every poll and counts them under a mutex, so the test goroutine
// can read the count while the supervisor goroutine writes it.
type countingPollFakeGitHub struct {
	github.NoopClient
	mu  sync.Mutex
	n   int
	err error
}

func (f *countingPollFakeGitHub) ListPullRequests(context.Context, github.Repository, string) ([]github.PullRequest, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return nil, f.err
}

func (f *countingPollFakeGitHub) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// TestSupervisorPollErrorReporterBoundsWithoutHidingAnOnset pins all three failures review
// found in the first limiter, as separate cases, because they are independent and a single
// "the limiter works" assertion answers none of them.
func TestSupervisorPollErrorReporterBoundsWithoutHidingAnOnset(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	t.Run("identical cause is limited, first is always reported", func(t *testing.T) {
		r := newPollErrorReporter(time.Minute)
		if !r.shouldReport("rate limited", base) {
			t.Fatal("the FIRST occurrence was suppressed; an operator must always see a failure's onset")
		}
		if r.shouldReport("rate limited", base.Add(59*time.Second)) {
			t.Fatal("an identical cause inside the window was reported; a persistent failure floods the log")
		}
		if !r.shouldReport("rate limited", base.Add(time.Minute)) {
			t.Fatal("an identical cause was still suppressed AFTER the window; the failure would go silent while still happening")
		}
	})

	t.Run("alternating causes do not defeat the window", func(t *testing.T) {
		// The first limiter remembered only the PREVIOUS cause, so A/B/A/B emitted every
		// occurrence -- the limit existed in name only for the commonest real pattern, two
		// interleaved faults.
		r := newPollErrorReporter(time.Minute)
		if !r.shouldReport("A", base) || !r.shouldReport("B", base) {
			t.Fatal("first occurrences of A and B must both report")
		}
		if r.shouldReport("A", base.Add(time.Second)) {
			t.Fatal("cause A reported again inside its window because B intervened; the limiter is single-slot")
		}
		if r.shouldReport("B", base.Add(2*time.Second)) {
			t.Fatal("cause B reported again inside its window because A intervened; the limiter is single-slot")
		}
	})

	t.Run("a success ends the episode so a recurrence is a new onset", func(t *testing.T) {
		// The dangerous one. A/success/A is a fault that recovered and RETURNED. Suppressing
		// the second A tells the operator the incident ended when a new one had begun.
		r := newPollErrorReporter(time.Hour)
		if !r.shouldReport("flaky remote", base) {
			t.Fatal("first occurrence suppressed")
		}
		r.recordSuccess()
		if !r.shouldReport("flaky remote", base.Add(time.Second)) {
			t.Fatal("a recurrence AFTER a successful poll was suppressed: a limiter meant to bound a flood is hiding the onset of a new incident")
		}
	})

	t.Run("ever-changing cause text is still bounded", func(t *testing.T) {
		// Per-cause keying bounds each cause and says nothing about their NUMBER. An error
		// embedding a timestamp or SHA makes every occurrence a new key, so per-cause
		// limiting alone degrades to no limiting -- exactly the flood being fixed.
		r := newPollErrorReporter(time.Hour)
		reported := 0
		for i := 0; i < 500; i++ {
			if r.shouldReport(fmt.Sprintf("transient failure at seq %d", i), base.Add(time.Duration(i)*time.Millisecond)) {
				reported++
			}
		}
		if reported > pollErrorReporterBurst {
			t.Fatalf("emitted %d lines for 500 distinct causes in one window, want at most %d; unique text defeats the limit entirely", reported, pollErrorReporterBurst)
		}
		if reported == 0 {
			t.Fatal("emitted nothing at all; a bound that reports nothing is not a bound, it is silence")
		}
		// And the burst must REFILL, or a fault that changes text is permanently muted.
		if !r.shouldReport("a later distinct failure", base.Add(2*time.Hour)) {
			t.Fatal("the global burst never refilled; after one noisy window the operator is muted forever")
		}
	})

	t.Run("tracked causes are bounded", func(t *testing.T) {
		r := newPollErrorReporter(time.Hour)
		for i := 0; i < 500; i++ {
			r.shouldReport(fmt.Sprintf("cause %d", i), base.Add(time.Duration(i)*time.Millisecond))
		}
		if len(r.seen) > pollErrorReporterTrackedCauses {
			t.Fatalf("tracking %d causes, want at most %d; the map grows without bound on varying text", len(r.seen), pollErrorReporterTrackedCauses)
		}
	})
}

// TestSupervisorPollErrorReporterRateLimitsRepeats bounds the MEDIUM finding.
//
// stdout here is the foreground stream, and under `gitmoot daemon start` it is appended to
// daemon.log, which `gitmoot daemon logs` reads in full. Emission was bounded only by the
// configured poll interval, which has no minimum -- so a persistently failing poll at a short
// interval writes an unbounded, perfectly repetitive stream into the one place an operator
// looks, with no dedup, no backoff and no rotation. A diagnostic that drowns the log is a
// different way of being unobservable, which is the same defect this PR is fixing.
//
// The rule asserted: an IDENTICAL cause is reported at most once per window, while a CHANGED
// cause reports immediately. Suppressing a new error to honour a rate limit would hide the
// thing the operator needs; that is why the second half is asserted, not assumed.
func TestSupervisorPollErrorReporterRateLimitsRepeats(t *testing.T) {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	reporter := newPollErrorReporter(time.Minute)

	if !reporter.shouldReport("rate limited", base) {
		t.Fatal("the FIRST occurrence of a cause was suppressed; an operator must always see a failure's onset")
	}
	if reporter.shouldReport("rate limited", base.Add(time.Second)) {
		t.Fatal("an identical cause repeated within the window was reported; a persistent failure floods the log an operator reads")
	}
	if reporter.shouldReport("rate limited", base.Add(59*time.Second)) {
		t.Fatal("an identical cause just inside the window was reported")
	}
	if !reporter.shouldReport("rate limited", base.Add(time.Minute)) {
		t.Fatal("an identical cause was still suppressed AFTER the window elapsed; the failure would go silent while it is still happening")
	}

	// A DIFFERENT cause must never be suppressed by a running window. Rate-limiting by
	// time alone would swallow the second, distinct failure -- turning a bounded
	// diagnostic into a lost one.
	if !reporter.shouldReport("a different failure entirely", base.Add(time.Minute).Add(time.Second)) {
		t.Fatal("a NEW cause was suppressed by the previous cause's window; only identical repeats may be rate-limited")
	}
}
