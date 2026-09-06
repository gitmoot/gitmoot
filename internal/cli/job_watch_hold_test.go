package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// watchUntil drives the REAL `job watch` CLI entry point against a job that is
// deliberately NOT settled, waits for `want` to appear (or for the deadline),
// then settles the job so the watch loop returns.
//
// It has to be shaped this way. `job watch` blocks until the job settles, and a
// deferred job never settles - which is precisely the blindness #1887 is about,
// so any test that only inspects a settled job cannot observe the gap. Driving
// the real command and settling it from outside is what makes this a test of the
// renderer rather than of a helper.
func watchUntil(t *testing.T, home, jobID, want string, jsonOutput bool) (string, int) {
	t.Helper()
	out := &syncBuffer{}
	var stderr syncBuffer
	args := []string{"job", "watch", jobID, "--home", home, "--poll", "20ms"}
	if jsonOutput {
		args = []string{"job", "watch", jobID, "--home", home, "--poll", "20ms", "--json"}
	}
	done := make(chan int, 1)
	go func() { done <- Run(args, out, &stderr) }()

	deadline := time.Now().Add(15 * time.Second)
	for want != "" && !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in job watch output; got:\n%s\nstderr:\n%s", want, out.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// settle the job so the watch loop exits
	settleWatchedJob(t, home, jobID)
	select {
	case code := <-done:
		return out.String(), code
	case <-time.After(15 * time.Second):
		t.Fatalf("job watch did not return after the job settled; output:\n%s", out.String())
		return "", 1
	}
}

func settleWatchedJob(t *testing.T, home, jobID string) {
	t.Helper()
	store := openCLIJobStore(t, home)
	defer store.Close()
	moved, err := store.TransitionJobState(context.Background(), jobID,
		string(workflow.JobQueued), string(workflow.JobSucceeded))
	if err != nil {
		t.Fatalf("TransitionJobState(%s): %v", jobID, err)
	}
	if !moved {
		t.Fatalf("TransitionJobState(%s) moved no row; the watch loop would never exit", jobID)
	}
}

// TestJobWatchSurfacesDeferralHoldWhileWaiting is the watch half of #1887. An
// operator who attaches AFTER the deferral event sees no events at all, and the
// loop prints nothing until settle - so the reason has to be surfaced from the
// payload while waiting.
func TestJobWatchSurfacesDeferralHoldWhileWaiting(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "watch-deferred", workflow.JobPayload{
		Repo:           "owner/repo",
		PullRequest:    11,
		BlockerClass:   "runtime_quota",
		BlockerRetryAt: "2026-09-06T02:55:00Z",
	})
	store.Close()

	out, code := watchUntil(t, home, "watch-deferred", "HOLD:", false)
	if code != 0 {
		t.Fatalf("job watch exit = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "runtime_quota") {
		t.Errorf("job watch output = %q, want the blocker class while the job is held", out)
	}
	if !strings.Contains(out, "2026-09-06T02:55:00Z") {
		t.Errorf("job watch output = %q, want the recorded retry time", out)
	}
}

// TestJobWatchHoldWithoutRetryTimeClaimsNoTime carries the null-retry constraint
// onto the watch surface: absence must read as unknown, never as a zero time.
func TestJobWatchHoldWithoutRetryTimeClaimsNoTime(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "watch-deferred-no-retry", workflow.JobPayload{
		Repo:         "owner/repo",
		PullRequest:  12,
		BlockerClass: "checkout_contention",
	})
	store.Close()

	out, code := watchUntil(t, home, "watch-deferred-no-retry", "HOLD:", false)
	if code != 0 {
		t.Fatalf("job watch exit = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "retry time unknown") {
		t.Errorf("job watch output = %q, want the missing retry time rendered as unknown", out)
	}
	for _, forbidden := range []string{"0001-01-01", "1970-01-01", "next retry )"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("job watch output = %q, must not invent a retry time (%q)", out, forbidden)
		}
	}
}

// TestJobWatchDoesNotRepeatAnUnchangedHold guards the cost of this surface. The
// loop polls continuously, so printing the hold unconditionally would emit the
// same line every interval and bury the events an operator is watching for.
func TestJobWatchDoesNotRepeatAnUnchangedHold(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "watch-repeat", workflow.JobPayload{
		Repo:         "owner/repo",
		PullRequest:  13,
		BlockerClass: "runtime_quota",
	})
	store.Close()

	out := &syncBuffer{}
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"job", "watch", "watch-repeat", "--home", home, "--poll", "20ms"}, out, &stderr)
	}()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(out.String(), "HOLD:") {
		if time.Now().After(deadline) {
			t.Fatalf("no HOLD line appeared; output:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// let many more poll intervals elapse than there are printed lines
	time.Sleep(600 * time.Millisecond)
	settleWatchedJob(t, home, "watch-repeat")
	<-done

	if n := strings.Count(out.String(), "HOLD:"); n != 1 {
		t.Errorf("HOLD line count = %d over ~30 poll intervals, want exactly 1; an unchanged hold must not repeat:\n%s", n, out.String())
	}
}

// TestJobWatchJSONCarriesTheObservedHold covers the non-human surface, and pins
// the naming decision: the settled object reports held_reason rather than
// why_stuck, because by emission time the job is no longer held.
func TestJobWatchJSONCarriesTheObservedHold(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "watch-json", workflow.JobPayload{
		Repo:           "owner/repo",
		PullRequest:    14,
		BlockerClass:   "runtime_quota",
		BlockerRetryAt: "2026-09-06T03:10:00Z",
	})
	store.Close()

	// --json prints only at settle, so give the loop a moment to observe the hold
	// before settling it.
	out := &syncBuffer{}
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"job", "watch", "watch-json", "--home", home, "--poll", "20ms", "--json"}, out, &stderr)
	}()
	time.Sleep(300 * time.Millisecond)
	settleWatchedJob(t, home, "watch-json")
	if code := <-done; code != 0 {
		t.Fatalf("job watch --json exit = %d, stderr=%s", code, stderr.String())
	}

	var decoded jobWatchOutput
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("decode job watch --json: %v (stdout=%s)", err, out.String())
	}
	if !strings.Contains(decoded.HeldReason, "runtime_quota") {
		t.Errorf("held_reason = %q, want the observed blocker class", decoded.HeldReason)
	}
	if decoded.HeldNextRetryAt != "2026-09-06T03:10:00Z" {
		t.Errorf("held_next_retry_at = %q, want the recorded retry time", decoded.HeldNextRetryAt)
	}
}

// TestJobWatchOrdinaryQueuedJobPrintsNoHold is the control: a queued job with no
// blocker payload and no lock must produce no HOLD line at all, or this change
// trades one blindness for constant noise.
func TestJobWatchOrdinaryQueuedJobPrintsNoHold(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "watch-plain", workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 15,
	})
	store.Close()

	out := &syncBuffer{}
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"job", "watch", "watch-plain", "--home", home, "--poll", "20ms"}, out, &stderr)
	}()
	// give the loop many intervals in which it could have printed a spurious hold
	time.Sleep(400 * time.Millisecond)
	settleWatchedJob(t, home, "watch-plain")
	if code := <-done; code != 0 {
		t.Fatalf("job watch exit = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(out.String(), "HOLD:") {
		t.Errorf("job watch output = %q, want NO hold line for an ordinary queued job", out.String())
	}
}
