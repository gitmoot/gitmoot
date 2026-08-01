package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func livenessTestJob(t *testing.T, id string, pid int) (context.Context, config.Paths, *db.Store, time.Time, time.Duration) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	payload := workflow.JobPayload{Repo: "owner/repo", RuntimePID: pid, RuntimePIDStartTime: "recorded-start", RuntimePGID: pid}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: id, Agent: "reviewer", Type: "review", State: string(workflow.JobRunning), Payload: string(encoded)}); err != nil {
		t.Fatal(err)
	}
	quiet := 5 * time.Minute
	now := time.Now().UTC().Add(2 * time.Hour)
	path := transcript.JobLogPath(paths.Logs, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("frozen transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-quiet - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return ctx, paths, store, now, quiet
}

func runLivenessSamples(t *testing.T, sweep *daemonLivenessSweep, ctx context.Context, store *db.Store, paths config.Paths, now time.Time, quiet time.Duration) {
	t.Helper()
	for _, at := range []time.Time{now, now.Add(daemonWorkerLoopInterval)} {
		if err := sweep.sweepRunningJobLiveness(ctx, store, io.Discard, paths, at, 30*time.Minute, quiet, "owner/repo", ""); err != nil {
			t.Fatalf("sweep at %s: %v", at, err)
		}
	}
}

func TestProbeRuntimePIDUsesProcessStartIdentity(t *testing.T) {
	pid := os.Getpid()
	identity := workflow.RuntimeProcessIdentity(pid)
	if identity == "" {
		t.Fatal("current process identity is empty")
	}
	if got := probeRuntimePID(pid, identity); got != runtimePIDLive {
		t.Fatalf("probe current pid = %v, want live", got)
	}
	if got := probeRuntimePID(pid, identity+"-reused"); got != runtimePIDIdentityMismatch {
		t.Fatalf("probe reused pid = %v, want identity mismatch", got)
	}
}

func TestDaemonLivenessSweepDeadPIDFrozenLogFails(t *testing.T) {
	ctx, paths, store, now, quiet := livenessTestJob(t, "dead", 4242)
	sweep := newDaemonLivenessSweep()
	sweep.probe = func(int, string) runtimePIDState { return runtimePIDDead }
	runLivenessSamples(t, sweep, ctx, store, paths, now, quiet)
	job, err := store.GetJob(ctx, "dead")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("dead frozen job state = %q, want failed", job.State)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if payload.RuntimePID != 0 || payload.RuntimePIDStartTime != "" || payload.RuntimePGID != 0 {
		t.Fatalf("terminal runtime identity = %+v, want cleared", payload)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	message := ""
	for _, event := range events {
		if event.Kind == jobRecoveryFailedEvent {
			message = event.Message
			break
		}
	}
	if message == "" {
		t.Fatalf("events = %+v, want terminal failed event", events)
	}
	for _, leg := range []string{"updated_at frozen", "job log byte-frozen", "runtime pid 4242 is dead"} {
		if !strings.Contains(message, leg) {
			t.Fatalf("failed event %q missing predicate leg %q", message, leg)
		}
	}
}

func TestDaemonLivenessSweepLivePIDVetoesFrozenLog(t *testing.T) {
	ctx, paths, store, now, quiet := livenessTestJob(t, "live", os.Getpid())
	sweep := newDaemonLivenessSweep()
	sweep.probe = func(int, string) runtimePIDState { return runtimePIDLive }
	runLivenessSamples(t, sweep, ctx, store, paths, now, quiet)
	job, err := store.GetJob(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("live quiet job state = %q, want running", job.State)
	}
}

func TestDaemonLivenessSweepQuietThresholdVetoes(t *testing.T) {
	ctx, paths, store, now, quiet := livenessTestJob(t, "recent", 4242)
	path := transcript.JobLogPath(paths.Logs, "recent")
	recent := now.Add(-quiet + time.Minute)
	if err := os.Chtimes(path, recent, recent); err != nil {
		t.Fatal(err)
	}
	sweep := newDaemonLivenessSweep()
	sweep.probe = func(int, string) runtimePIDState { return runtimePIDDead }
	runLivenessSamples(t, sweep, ctx, store, paths, now, quiet)
	job, err := store.GetJob(ctx, "recent")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("under-threshold job state = %q, want running", job.State)
	}
}

func TestDaemonLivenessSweepPIDReuseIsIndeterminate(t *testing.T) {
	ctx, paths, store, now, quiet := livenessTestJob(t, "reused", os.Getpid())
	sweep := newDaemonLivenessSweep()
	runLivenessSamples(t, sweep, ctx, store, paths, now, quiet)
	job, err := store.GetJob(ctx, "reused")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("identity-mismatch job state = %q, want running", job.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "job_liveness_identity_mismatch" || !strings.Contains(events[0].Message, "identity mismatch") {
		t.Fatalf("identity mismatch events = %+v", events)
	}
}

func TestDaemonLivenessSweepKillsRecordedProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.Command("sh", "-c", "sleep 300 & echo $! > "+pidFile+"; exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	grandchild := waitForPIDFile(t, pidFile)
	waitForProcessState(t, pgid, "Z")
	identity := workflow.RuntimeProcessIdentity(pgid)
	if identity == "" {
		t.Fatal("group leader start-time identity is empty")
	}

	ctx, paths, store, now, quiet := livenessTestJob(t, "group-dead", pgid)
	job, err := store.GetJob(ctx, "group-dead")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	payload.RuntimePID = pgid
	payload.RuntimePGID = pgid
	payload.RuntimePIDStartTime = identity
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}

	runLivenessSamples(t, newDaemonLivenessSweep(), ctx, store, paths, now, quiet)
	job, err = store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("zombie-leader job state = %q, want failed", job.State)
	}
	waitForProcessGone(t, grandchild)
}

func TestDaemonLivenessSweepPGIDIdentityMismatchLeaksWithoutSignal(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	ctx, paths, store, now, quiet := livenessTestJob(t, "pgid-reused", pgid)
	sweep := newDaemonLivenessSweep()
	sweep.probe = func(int, string) runtimePIDState { return runtimePIDDead }
	runLivenessSamples(t, sweep, ctx, store, paths, now, quiet)

	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatalf("identity-mismatched group leader was signaled: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if state := runtimeProcessState(pgid); state == "" || state == "Z" {
		t.Fatalf("identity-mismatched group leader state = %q, want live and unsignaled", state)
	}
	events, err := store.ListJobEvents(ctx, "pgid-reused")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if strings.Contains(event.Message, "pgid recycle suspected, orphans leaked") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("events = %+v, want pgid recycle suspected, orphans leaked", events)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

func waitForProcessState(t *testing.T, pid int, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err == nil {
			closeParen := strings.LastIndexByte(string(data), ')')
			if closeParen >= 0 {
				fields := strings.Fields(string(data[closeParen+1:]))
				if len(fields) > 0 && fields[0] == want {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d did not reach state %s", pid, want)
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) {
			return
		}
		if err == nil {
			closeParen := strings.LastIndexByte(string(data), ')')
			if closeParen >= 0 {
				fields := strings.Fields(string(data[closeParen+1:]))
				if len(fields) > 0 && fields[0] == "Z" {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d survived process-group kill", pid)
}

func TestDaemonLivenessSweepLegacyNeedsDoubleQuietAndNoLease(t *testing.T) {
	ctx, paths, store, now, quiet := livenessTestJob(t, "legacy", 0)
	path := transcript.JobLogPath(paths.Logs, "legacy")
	old := now.Add(-2*quiet - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	sweep := newDaemonLivenessSweep()
	runLivenessSamples(t, sweep, ctx, store, paths, now, quiet)
	job, err := store.GetJob(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("legacy frozen job state = %q, want failed after 2x quiet with no lease", job.State)
	}
}

func TestDaemonLivenessSweepLegacyHeldLeaseVetoes(t *testing.T) {
	ctx, paths, store, now, quiet := livenessTestJob(t, "legacy-held", 0)
	path := transcript.JobLogPath(paths.Logs, "legacy-held")
	old := now.Add(-2*quiet - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:legacy-held", OwnerJobID: "legacy-held", OwnerToken: "token",
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
	}, now); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock = %v, %v", acquired, err)
	}
	sweep := newDaemonLivenessSweep()
	runLivenessSamples(t, sweep, ctx, store, paths, now, quiet)
	job, err := store.GetJob(ctx, "legacy-held")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("legacy lease-held job state = %q, want running", job.State)
	}
}
