package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func seedQueuedJobAs(t *testing.T, store *db.Store, id, agent string, payload workflow.JobPayload) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID:      id,
		Agent:   agent,
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: string(encoded),
		Repo:    payload.Repo,
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

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

// TestJobWatchTranscriptSurfacesHoldWithAnExistingLog covers `job watch
// --transcript` through runJobTranscriptWatch's REAL renderer rather than by
// inference from shared code.
//
// The distinction that makes this test necessary: when no log exists, that
// function prints "transcript unavailable" and DELEGATES to runJobEventWatch,
// inheriting its HOLD line for free. This test seeds a log so the delegation is
// NOT taken, which is the path where transcript.Follow blocks on new log lines
// and would otherwise show nothing at all while the job sits held - the mode an
// operator is likelier to reach for on a job that already ran once.
func TestJobWatchTranscriptSurfacesHoldWithAnExistingLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent-herdr.sock"))
	store := openCLIJobStore(t, home)
	if err := store.UpsertAgent(context.Background(), db.Agent{Name: "shell-seat", Runtime: runtime.ShellRuntime}); err != nil {
		t.Fatal(err)
	}
	seedQueuedJobAs(t, store, "watch-transcript-hold", "shell-seat", workflow.JobPayload{
		Repo:           "owner/repo",
		PullRequest:    16,
		BlockerClass:   "runtime_quota",
		BlockerRetryAt: "2026-09-06T04:20:00Z",
	})
	store.Close()

	logPath := filepath.Join(config.PathsForHome(home).Logs, "jobs", transcript.LegacyLogName("watch-transcript-hold")+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("shell started\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &syncBuffer{}
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"job", "watch", "watch-transcript-hold", "--home", home, "--transcript", "--poll", "20ms"}, out, &stderr)
	}()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(out.String(), "HOLD:") {
		if time.Now().After(deadline) {
			t.Fatalf("no HOLD line from --transcript; output:\n%s\nstderr:\n%s", out.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	settleWatchedJob(t, home, "watch-transcript-hold")
	<-done

	if strings.Contains(out.String(), "transcript unavailable") {
		t.Fatal("the delegation path was taken, so this did not exercise transcript.Follow at all")
	}
	if !strings.Contains(out.String(), "runtime_quota") {
		t.Errorf("--transcript output = %q, want the blocker class", out.String())
	}
	if !strings.Contains(out.String(), "2026-09-06T04:20:00Z") {
		t.Errorf("--transcript output = %q, want the recorded retry time", out.String())
	}
}

// TestJobWatchSurfacesWithheldHolderDirectly is #1553's cause observed DIRECTLY
// on the watch surface, not inferred from shared code. It kills the reversion
// "surface the hold only for the blocker-payload class", which would leave a
// withheld job silent on watch while job list explained it.
// TestJobWatchNeverInfersAHoldFromABranchLaneRow is the watch-surface half of
// the same defect: the withheld cause was read through shared derivation, so the
// watch path inherited the false hold. The causal rendering it is replaced by is
// covered by TestJobWatchSurfacesTheCausalCheckoutContentionHold below.
func TestJobWatchNeverInfersAHoldFromABranchLaneRow(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "watch-lane-row", workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-17",
		PullRequest: 17,
	})
	seedBranchLock(t, home, "owner/repo", "task-17", "implement-holds-the-repo")
	store.Close()

	out, _ := watchUntil(t, home, "watch-lane-row", "", false)
	if strings.Contains(out, "HOLD:") {
		t.Fatalf("job watch output = %q, want no HOLD: a lane row cannot prove this job is withheld", out)
	}
}

func TestJobWatchTranscriptSurfacesHoldThatBeginsAfterStartup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent-herdr.sock"))
	store := openCLIJobStore(t, home)
	if err := store.UpsertAgent(context.Background(), db.Agent{Name: "shell-seat", Runtime: runtime.ShellRuntime}); err != nil {
		t.Fatal(err)
	}
	// RUNNING and unheld at startup, so the pre-Follow sample can see nothing.
	encoded, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", PullRequest: 18})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID: "watch-late-hold", Agent: "shell-seat", Type: "ask",
		State: string(workflow.JobRunning), Payload: string(encoded), Repo: "owner/repo",
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	logPath := filepath.Join(config.PathsForHome(home).Logs, "jobs", transcript.LegacyLogName("watch-late-hold")+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("shell started\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &syncBuffer{}
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"job", "watch", "watch-late-hold", "--home", home, "--transcript", "--poll", "20ms"}, out, &stderr)
	}()
	// let the watch attach and take its one pre-Follow look while nothing is held
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(out.String(), "HOLD:") {
		t.Fatalf("a hold printed before one was applied; fixture is wrong:\n%s", out.String())
	}

	// NOW apply the hold, after the watch is already following.
	late := openCLIJobStore(t, home)
	heldPayload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 18,
		BlockerClass: "runtime_quota", BlockerRetryAt: "2026-09-06T06:30:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := late.TransitionJobStatePayloadWithEvent(context.Background(), "watch-late-hold",
		string(workflow.JobRunning), string(workflow.JobQueued), string(heldPayload),
		db.JobEvent{JobID: "watch-late-hold", Kind: "blocker_deferred", Message: "runtime_quota: attempt 1"})
	if err != nil {
		t.Fatalf("apply late hold: %v", err)
	}
	if !moved {
		t.Fatal("late hold transition moved no row")
	}
	late.Close()

	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(out.String(), "HOLD:") {
		if time.Now().After(deadline) {
			t.Fatalf("a hold that began AFTER startup never printed; --transcript samples it once instead of polling:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	settleWatchedJob(t, home, "watch-late-hold")
	<-done
	if !strings.Contains(out.String(), "runtime_quota") {
		t.Errorf("--transcript output = %q, want the late hold's class", out.String())
	}
}

// TestJobWatchDoesNotRestateAReplayedDeferralEvent is F4's regression and it kills
// the reversion "derive HOLD even when a reason-bearing event exists" - which
// printed the identical quota detail twice, once as the replayed blocker_deferred
// event and again as HOLD, in a change whose own comment claimed the operator
// would otherwise see nothing.
func TestJobWatchDoesNotRestateAReplayedDeferralEvent(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "watch-replayed", workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 19,
		BlockerClass: "runtime_quota", BlockerRetryAt: "2026-09-06T07:00:00Z",
	})
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID: "watch-replayed", Kind: "blocker_deferred", Message: "runtime_quota: attempt 1",
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
	store.Close()

	out := &syncBuffer{}
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"job", "watch", "watch-replayed", "--home", home, "--poll", "20ms"}, out, &stderr)
	}()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(out.String(), "blocker_deferred") {
		if time.Now().After(deadline) {
			t.Fatalf("the deferral event never replayed; fixture is wrong:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond) // many polls in which a duplicate could appear
	settleWatchedJob(t, home, "watch-replayed")
	<-done

	if n := strings.Count(out.String(), "HOLD:"); n != 0 {
		t.Errorf("HOLD printed %d times alongside the replayed blocker_deferred event; the event already told the operator:\n%s", n, out.String())
	}
	if strings.Count(out.String(), "runtime_quota") != 1 {
		t.Errorf("the quota detail appears %d times, want exactly once:\n%s", strings.Count(out.String(), "runtime_quota"), out.String())
	}
}

// TestJobWatchAttachedInsideTheWriteWindowStatesTheDeferralOnce reproduces
// #1943's F4 probe exactly: the checkout-contention path writes the blocker
// PAYLOAD and the blocker_deferred EVENT in two separate store operations
// (internal/cli/job_blocker_checkout.go:212 then :217), so a watcher can attach
// between them. It then sees the payload and prints HOLD, and the event arrives
// on a later poll - and the previous guard, which only consulted the CURRENT
// poll's events, could not retract the HOLD it had already emitted.
//
// The deferral must be stated ONCE across the whole watch, not once per label.
func TestJobWatchAttachedInsideTheWriteWindowStatesTheDeferralOnce(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	// State 1: payload written, event NOT yet - the window the watcher attaches in.
	seedQueuedJob(t, store, "window-job", workflow.JobPayload{
		Repo:            "owner/repo",
		Branch:          "task-3",
		PullRequest:     3,
		BlockerClass:    "checkout_contention",
		BlockerRetryAt:  time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano),
		BlockerAttempts: 1,
	})

	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- runJobWatch([]string{"window-job", "--home", home, "--poll", "20ms"}, &out, &out)
	}()

	// Wait for the HOLD to be printed from the payload alone.
	deadline := time.Now().Add(8 * time.Second)
	for !strings.Contains(out.String(), "HOLD:") {
		if time.Now().After(deadline) {
			t.Fatalf("HOLD never printed from the payload; output = %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// State 2: the production-shaped event lands, exactly as the classifier writes it.
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "window-job",
		Kind:    blockerDeferredEventKind,
		Message: "checkout_contention: attempt 1/3, retry at 2026-09-06T16:00:00Z: branch task-3 is locked by other-agent",
	}); err != nil {
		t.Fatalf("AddJobEvent: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if _, err := store.TransitionJobState(context.Background(), "window-job",
		string(workflow.JobQueued), string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("watch did not exit; output = %q", out.String())
	}
	store.Close()

	got := out.String()
	holds := strings.Count(got, "HOLD:")
	events := strings.Count(got, blockerDeferredEventKind)
	if holds != 1 {
		t.Fatalf("HOLD printed %d times, want exactly 1; output = %q", holds, got)
	}
	// The event must NOT also be printed: HOLD already told the operator this.
	if events != 0 {
		t.Fatalf("the same deferral was ALSO printed as a %s event (%d times) - stated twice under two labels; output = %q",
			blockerDeferredEventKind, events, got)
	}
}

// TestJobWatchTranscriptNoLogDelegatesAndInheritsSuppression pins #1943's F3:
// the docs asserted --transcript renders log lines rather than events and prints
// HOLD in EVERY case, which is false for its own no-log fallback. With no
// retained log the command prints "transcript unavailable; showing job events"
// and delegates to event watch, so it inherits that mode's reason-event
// suppression and shows the EVENT rather than HOLD.
//
// Both arms are asserted because the documented contract now distinguishes them,
// and a fix that simply always printed HOLD would satisfy the second alone.
func TestJobWatchTranscriptNoLogDelegatesAndInheritsSuppression(t *testing.T) {
	for _, tt := range []struct {
		name      string
		withEvent bool
		wantHold  bool
	}{
		{
			name:      "a reason event exists, so the event speaks and HOLD stays silent",
			withEvent: true,
			wantHold:  false,
		},
		{
			name:      "no reason event, so the payload-only hold is still surfaced",
			withEvent: false,
			wantHold:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HERDR_ENV", "")
			t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent-herdr.sock"))
			store := openCLIJobStore(t, home)
			if err := store.UpsertAgent(context.Background(), db.Agent{Name: "shell-seat", Runtime: runtime.ShellRuntime}); err != nil {
				t.Fatal(err)
			}
			seedQueuedJobAs(t, store, "no-log-job", "shell-seat", workflow.JobPayload{
				Repo:           "owner/repo",
				PullRequest:    18,
				BlockerClass:   "checkout_contention",
				BlockerRetryAt: "2026-09-06T05:00:00Z",
			})
			if tt.withEvent {
				if err := store.AddJobEvent(context.Background(), db.JobEvent{
					JobID:   "no-log-job",
					Kind:    blockerDeferredEventKind,
					Message: "checkout_contention: attempt 1/3, retry at 2026-09-06T05:00:00Z: branch task-8 is locked by other-agent",
				}); err != nil {
					t.Fatal(err)
				}
			}
			store.Close()
			// NO log file is written on purpose: that is what selects the fallback.

			out := &syncBuffer{}
			var stderr syncBuffer
			done := make(chan int, 1)
			go func() {
				done <- Run([]string{"job", "watch", "no-log-job", "--home", home, "--transcript", "--poll", "20ms"}, out, &stderr)
			}()
			deadline := time.Now().Add(15 * time.Second)
			for !strings.Contains(out.String(), "transcript unavailable") {
				if time.Now().After(deadline) {
					t.Fatalf("the no-log fallback was never taken; output:\n%s\nstderr:\n%s", out.String(), stderr.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			// Give the delegated loop several polls in which it could print either form.
			time.Sleep(300 * time.Millisecond)
			settleWatchedJob(t, home, "no-log-job")
			<-done

			got := out.String()
			gotHold := strings.Contains(got, "HOLD:")
			if gotHold != tt.wantHold {
				t.Fatalf("no-log --transcript HOLD present = %v, want %v; the docs now state this case explicitly. output:\n%s",
					gotHold, tt.wantHold, got)
			}
			if tt.withEvent && !strings.Contains(got, blockerDeferredEventKind) {
				t.Fatalf("no-log --transcript output = %q, want the delegated event line", got)
			}
		})
	}
}

// TestJobWatchSurfacesTheWithheldCauseDirectly is the DIRECT observable
// withheld-watch assertion required by directive 124112: #1887 names both the
// quota-deferred and the repo-withheld cause, and asserting the withheld half
// "should render" through shared loadStuckReason is an inference, not evidence.
//
// It is observed here through the real watch renderer. The cause it asserts is
// the CAUSAL one - the daemon pre-flight emits "branch %s is locked by %s"
// (internal/cli/workflow.go:1283) and job_blocker_checkout.go persists it as the
// checkout_contention BlockerClass - because #1943's review proved the previous
// version of this test pinned an INVENTED rendering read out of a branch-lock
// lane row. That test was deleted; this replaces it on the same surface.
func TestJobWatchSurfacesTheWithheldCauseDirectly(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	// The withheld shape: a repo-contention hold with NO retry time recorded,
	// which is the measured common case (42 of 55 rows).
	seedQueuedJob(t, store, "withheld-watch", workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-21",
		PullRequest:  21,
		BlockerClass: "checkout_contention",
	})
	store.Close()

	out, _ := watchUntil(t, home, "withheld-watch", "HOLD:", false)
	if !strings.Contains(out, "checkout_contention") {
		t.Fatalf("job watch output = %q, want the withheld cause named on the watch surface", out)
	}
	// The absent retry must read as unknown here too, never as a zero time.
	if !strings.Contains(out, "retry time unknown") {
		t.Fatalf("job watch output = %q, want the absent retry rendered unknown", out)
	}
	if strings.Contains(out, "0001-01-01") || strings.Contains(out, "1970-01-01") {
		t.Fatalf("job watch output = %q, must never format an absent retry as a zero time", out)
	}
}

// TestJobWatchStillReportsALaterRetryOfTheSameClass is the regression for
// #1943's second-round F4: the first fix latched suppression on the CLASS and
// never cleared it, so after HOLD spoke for attempt 1 every later
// blocker_deferred event of that class was swallowed and a distinct
// `attempt 2/3` retry vanished from a live watch.
//
// Suppression is now consumed by the single event it pairs with, so this asserts
// BOTH halves on one watch: the paired attempt-1 event stays suppressed, and the
// distinct attempt-2 event is still reported.
func TestJobWatchStillReportsALaterRetryOfTheSameClass(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "retry-job", workflow.JobPayload{
		Repo:            "owner/repo",
		Branch:          "task-5",
		PullRequest:     5,
		BlockerClass:    "checkout_contention",
		BlockerRetryAt:  time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano),
		BlockerAttempts: 1,
	})

	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- runJobWatch([]string{"retry-job", "--home", home, "--poll", "20ms"}, &out, &out)
	}()
	deadline := time.Now().Add(8 * time.Second)
	for !strings.Contains(out.String(), "HOLD:") {
		if time.Now().After(deadline) {
			t.Fatalf("HOLD never printed from the payload; output = %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The event PAIRED with the hold just rendered: must stay suppressed.
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "retry-job",
		Kind:    blockerDeferredEventKind,
		Message: "checkout_contention: attempt 1/3, retry at 2026-09-06T17:00:00Z: branch task-5 is locked by other-agent",
	}); err != nil {
		t.Fatalf("AddJobEvent attempt 1: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// A DISTINCT later retry of the SAME class: must be reported.
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "retry-job",
		Kind:    blockerDeferredEventKind,
		Message: "checkout_contention: attempt 2/3, retry at 2026-09-06T17:05:00Z: branch task-5 is locked by other-agent",
	}); err != nil {
		t.Fatalf("AddJobEvent attempt 2: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if _, err := store.TransitionJobState(context.Background(), "retry-job",
		string(workflow.JobQueued), string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("watch did not exit; output = %q", out.String())
	}
	store.Close()

	got := out.String()
	if strings.Contains(got, "attempt 1/3") {
		t.Fatalf("the event paired with the rendered HOLD was reported too - stated twice; output = %q", got)
	}
	if !strings.Contains(got, "attempt 2/3") {
		t.Fatalf("a DISTINCT later retry of the same class was swallowed by latched suppression; output = %q", got)
	}
}

// TestJobWatchReportsALaterRetryWhenTheEarlierEventNeverLanded is #1943 f6's
// MISSING-PAIR boundary, and it is a different failure from the sticky-class one
// next to it. The daemon writes the blocker payload and its event as two store
// operations and job_blocker.go:499 documents a crash between them as expected,
// so attempt 1's event can simply never exist. Suppression armed on the CLASS
// alone then outlived the deferral it was armed for and consumed attempt 2's
// event instead, hiding the retry entirely.
//
// Modelled exactly as the reviewer's probe: attempt 1's HOLD is rendered, its
// event is deliberately omitted, then attempt 2 lands WITH its event.
func TestJobWatchReportsALaterRetryWhenTheEarlierEventNeverLanded(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "missing-pair", workflow.JobPayload{
		Repo:            "owner/repo",
		Branch:          "task-7",
		PullRequest:     7,
		BlockerClass:    "checkout_contention",
		BlockerRetryAt:  time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano),
		BlockerAttempts: 1,
	})

	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- runJobWatch([]string{"missing-pair", "--home", home, "--poll", "20ms"}, &out, &out)
	}()
	deadline := time.Now().Add(8 * time.Second)
	for !strings.Contains(out.String(), "HOLD:") {
		if time.Now().After(deadline) {
			t.Fatalf("HOLD never printed for attempt 1; output = %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ATTEMPT 1'S EVENT IS DELIBERATELY NEVER WRITTEN - the daemon died in the
	// documented window between the payload and the event.
	//
	// Attempt 2 now lands with its own payload AND its own event.
	job, err := store.GetJob(context.Background(), "missing-pair")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	payload.BlockerAttempts = 2
	payload.BlockerRetryAt = time.Now().UTC().Add(90 * time.Second).Format(time.RFC3339Nano)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := store.UpdateJobPayload(context.Background(), "missing-pair", string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "missing-pair",
		Kind:    blockerDeferredEventKind,
		Message: "checkout_contention: attempt 2/3, retry at 2026-09-06T18:00:00Z: branch task-7 is locked by other-agent",
	}); err != nil {
		t.Fatalf("AddJobEvent attempt 2: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if _, err := store.TransitionJobState(context.Background(), "missing-pair",
		string(workflow.JobQueued), string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("watch did not exit; output = %q", out.String())
	}
	store.Close()

	if got := out.String(); !strings.Contains(got, "attempt 2/3") {
		t.Fatalf("attempt 2 was swallowed by attempt 1's arming, whose own event never landed; output = %q", got)
	}
}

// TestJobWatchReportsARetryWhoseDetailQuotesAnEarlierAttempt is #1943 f6's
// SECOND shape, and it is a defect in my own previous fix rather than in the
// original code. Binding suppression to class AND attempt was right, but the
// match searched the WHOLE event message with strings.Contains, and the
// classifiers append a free-form checkout error after the canonical prefix
// (job_blocker_checkout.go:215). A perfectly valid attempt-2 event whose DETAIL
// contains the text "attempt 1/" - here a checkout path - therefore matched
// attempt 1's stale token and was swallowed.
//
// The prefix is the only part of that message this code owns the format of; the
// tail is arbitrary data and must never be matched against.
func TestJobWatchReportsARetryWhoseDetailQuotesAnEarlierAttempt(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "detail-quote", workflow.JobPayload{
		Repo:            "owner/repo",
		Branch:          "task-11",
		PullRequest:     11,
		BlockerClass:    "checkout_contention",
		BlockerRetryAt:  time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano),
		BlockerAttempts: 1,
	})

	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- runJobWatch([]string{"detail-quote", "--home", home, "--poll", "20ms"}, &out, &out)
	}()
	deadline := time.Now().Add(8 * time.Second)
	for !strings.Contains(out.String(), "HOLD:") {
		if time.Now().After(deadline) {
			t.Fatalf("HOLD never printed for attempt 1; output = %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Attempt 1's event never commits (the documented crash window), and attempt
	// 2's VALID event carries "attempt 1/" inside its free-form detail.
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "detail-quote",
		Kind:    blockerDeferredEventKind,
		Message: "checkout_contention: attempt 2/3, retry at 2026-09-06T19:30:00Z: checkout /tmp/attempt 1/stale has uncommitted changes",
	}); err != nil {
		t.Fatalf("AddJobEvent attempt 2: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if _, err := store.TransitionJobState(context.Background(), "detail-quote",
		string(workflow.JobQueued), string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("watch did not exit; output = %q", out.String())
	}
	store.Close()

	if got := out.String(); !strings.Contains(got, "attempt 2/3") {
		t.Fatalf("attempt 2 was swallowed because its DETAIL quoted an earlier attempt token - the match must be anchored to the canonical prefix; output = %q", got)
	}
}
