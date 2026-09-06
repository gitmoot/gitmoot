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
func TestJobWatchSurfacesWithheldHolderDirectly(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "watch-withheld", workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-17",
		PullRequest: 17,
	})
	store.Close()
	seedBranchLock(t, home, "owner/repo", "task-17", "implement-holds-the-repo")

	out, code := watchUntil(t, home, "watch-withheld", "HOLD:", false)
	if code != 0 {
		t.Fatalf("job watch exit = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "implement-holds-the-repo") {
		t.Errorf("job watch output = %q, want the holding owner named on the watch surface", out)
	}
	if !strings.Contains(out, "task-17") {
		t.Errorf("job watch output = %q, want the held branch named", out)
	}
}

// TestJobWatchTranscriptSurfacesHoldThatBeginsAfterStartup is F1's regression and
// it kills the reversion "sample the hold once before Follow instead of on every
// poll" - the exact defect the review proved, where extracting holdLine made the
// two modes share their FORMATTING while their POLLING stayed divergent.
//
// The job starts RUNNING with no hold, so a one-shot sample before Follow sees
// nothing; the hold is applied only after the watch is already following.
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
