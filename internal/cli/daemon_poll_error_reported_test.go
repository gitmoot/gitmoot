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

	// Wait for the poll to run twice. This loop DOES select on done -- the fix was not to stop
	// receiving, it was to RECORD that the sole value was consumed so the join below skips its
	// own receive. `supervised` is that record, and it guarantees exactly one receive across
	// the test.
	//
	// The earlier version received here and unconditionally received AGAIN in the join, so a
	// supervisor returning early left the join waiting forever for a second value that never
	// comes. That, not ambient package load, is why the targeted test took 90s -- I diagnosed
	// it as contention and wrote that wrong cause into a comment. A later comment then said
	// this loop "never receives", which was also untrue. This is the accurate description.
	var supervised bool
	var supervisorErr error
	deadline := time.After(45 * time.Second)
waitForPolls:
	for client.calls() < 2 {
		select {
		case supervisorErr = <-done:
			supervised = true
			break waitForPolls
		case <-deadline:
			cancel()
			mu.Lock()
			got := out.String()
			mu.Unlock()
			t.Fatalf("poll ran %d times in 45s, want >= 2; stdout=%q", client.calls(), got)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// JOIN, and REQUIRE termination. The goroutine must not outlive the test into store
	// teardown; logging on timeout, as an earlier version did, only REPORTS the leak when the
	// finding asked for it to be prevented.
	//
	// STATED HONESTLY: three minutes IS a wall-clock shutdown-latency contract. Any finite
	// deadline is. The previous comment claimed it "fails only if the goroutine never
	// terminates", which is not something a timeout can distinguish, and review said so. The
	// contract is: this supervisor finishes shutting down within three minutes of cancellation,
	// roughly twelve times the 15s in-flight drain budget and far above anything observed. If
	// it is ever exceeded on a healthy machine, investigate shutdown rather than raise this.
	cancel()
	if !supervised {
		select {
		case supervisorErr = <-done:
		case <-time.After(3 * time.Minute):
			t.Fatal("supervisor did not terminate within three minutes of cancellation; it is leaking into store teardown")
		}
	}
	if supervisorErr != nil && !errors.Is(supervisorErr, context.Canceled) {
		t.Fatalf("supervisor returned %v, want context.Canceled", supervisorErr)
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

// TestPollErrorReportersAreIndependentPerMode pins the MEDIUM review found: one reporter was
// shared by the full and recovery polls, and only a successful FULL poll called recordSuccess,
// which cleared EVERY recorded cause.
//
// The sequence that broke it: recovery fails R -> an unrelated full poll succeeds -> recovery
// fails R again. R was re-emitted as a fresh onset although the recovery path never succeeded,
// and repeated busy/idle alternation walked around the global burst ceiling entirely.
//
// A reporter per mode is the fix, and this asserts the property that makes it one: a success in
// mode A must NOT clear mode B's episode. Testing the reporters in isolation would not catch the
// defect -- the type was always correct, the WIRING was not -- so this drives the pair the way
// the supervisor does.
func TestPollErrorReportersAreIndependentPerMode(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	// Drive the object that OWNS the routing, the same one the supervisor constructs. The
	// first version of this test drove two loose reporters it created itself, so it passed
	// against the very aliasing defect it was written for -- the reporters were always
	// correct, the wiring was not.
	r := newPollErrorReporters(time.Hour)

	if !r.shouldReport(recoveryPoll, "R", base) {
		t.Fatal("first recovery failure suppressed")
	}
	// A FULL poll succeeds. Under the shared reporter this cleared recovery's episode.
	r.recordSuccess(fullPoll)
	if r.shouldReport(recoveryPoll, "R", base.Add(time.Second)) {
		t.Fatal("a FULL-poll success cleared the RECOVERY episode: R re-emits as a fresh onset although the recovery path never succeeded")
	}

	// The converse, so this asserts INDEPENDENCE rather than one direction.
	if !r.shouldReport(fullPoll, "F", base.Add(2*time.Second)) {
		t.Fatal("first full-poll failure suppressed")
	}
	r.recordSuccess(recoveryPoll)
	if r.shouldReport(fullPoll, "F", base.Add(3*time.Second)) {
		t.Fatal("a RECOVERY success cleared the FULL episode")
	}

	// And each mode must still end its OWN episode -- otherwise "independent" is satisfied by
	// reporters that never reset, which is the onset-hiding defect this limiter replaced.
	r.recordSuccess(fullPoll)
	if !r.shouldReport(fullPoll, "F", base.Add(4*time.Second)) {
		t.Fatal("a full-poll success did not end the full-poll episode; a recurrence after recovery must report")
	}
	r.recordSuccess(recoveryPoll)
	if !r.shouldReport(recoveryPoll, "R", base.Add(5*time.Second)) {
		t.Fatal("a recovery success did not end the recovery episode")
	}
}

// TestBoundPollDerivesEverythingFromItsMode is the OBSERVABLE recovery-path guard review asked
// for, and it closes the route the previous fix left open.
//
// Round 5 collapsed shouldReport/recordSuccess into one call so the mode was supplied once.
// Review showed that was still not enough: changing ONLY the recovery call site's mode to
// fullPoll compiled and survived every guard. My comment called that "a consistent
// misattribution rather than a silent cross-mode reset" -- WRONG, and review said so. Recovery
// failures then share the FULL reporter, so an unrelated successful full poll clears the ongoing
// recovery episode: the original defect, reached by another route.
//
// So the mode now selects the poll FUNCTION and the label too. Mutating a call site's mode
// changes which work runs, which this guard observes directly.
func TestBoundPollDerivesEverythingFromItsMode(t *testing.T) {
	r := newPollErrorReporters(time.Hour)
	// Drive the same constructor the supervisor uses, in the same order, so a transposition
	// inside it is caught here rather than being invisible behind a function-local branch.
	full, recovery := r.runners(daemon.Daemon{})

	if full.mode != fullPoll || recovery.mode != recoveryPoll {
		t.Fatalf("bind returned modes full=%v recovery=%v", full.mode, recovery.mode)
	}
	if full.label != "poll error" || recovery.label != "recovery poll error" {
		t.Fatalf("labels are not derived from the mode: full=%q recovery=%q", full.label, recovery.label)
	}
	// The bound functions must DIFFER. A bind returning the same poll for both modes would
	// satisfy mode and label while running the wrong work -- the failure this guard exists for.
	if fmt.Sprintf("%p", full.poll) == fmt.Sprintf("%p", recovery.poll) {
		t.Fatal("both modes bound the SAME poll function; the mode does not select the work")
	}
}

func TestBoundPollRoutesOutcomesToOneEpisode(t *testing.T) {
	fail := func(context.Context) error { return errors.New("boom") }
	ok := func(context.Context) error { return nil }
	r := newPollErrorReporters(time.Hour)
	var out strings.Builder

	failRecovery := boundPoll{mode: recoveryPoll, poll: fail, label: "recovery poll error", reporters: r}
	okFull := boundPoll{mode: fullPoll, poll: ok, label: "poll error", reporters: r}
	okRecovery := boundPoll{mode: recoveryPoll, poll: ok, label: "recovery poll error", reporters: r}

	failRecovery.run(context.Background(), time.Minute, &out)
	if !strings.Contains(out.String(), "recovery poll error: boom") {
		t.Fatalf("first recovery failure was not reported; got %q", out.String())
	}

	okFull.run(context.Background(), time.Minute, &out)
	before := out.Len()
	failRecovery.run(context.Background(), time.Minute, &out)
	if out.Len() != before {
		t.Fatalf("a FULL-poll success cleared the RECOVERY episode; the repeat re-reported as a fresh onset.\nstdout=%q", out.String())
	}

	okRecovery.run(context.Background(), time.Minute, &out)
	before = out.Len()
	failRecovery.run(context.Background(), time.Minute, &out)
	if out.Len() == before {
		t.Fatal("a recovery success did not end the recovery episode; the recurrence must report")
	}
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
