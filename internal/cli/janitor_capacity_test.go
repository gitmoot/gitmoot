package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const janitorTestPolicyHash = "95a20125287a185d0000000000000000000000000000000000000000000000ab"

// janitorStatusJSON renders the producer's status envelope the way the live
// janitor does (schema 1, kind status, data.capacity).
func janitorStatusJSON(sampled time.Time, ttl time.Duration, policy string, volumes ...string) string {
	return fmt.Sprintf(`{"schema_version":1,"kind":"status","data":{"inventory_report":{},"capacity":{"sampled_at":%q,"valid_until":%q,"policy_hash":%q,"volumes":[%s]}}}`,
		sampled.UTC().Format(time.RFC3339Nano), sampled.Add(ttl).UTC().Format(time.RFC3339Nano), policy, strings.Join(volumes, ","))
}

func janitorVolumeJSON(device uint64, state string, hold bool) string {
	return fmt.Sprintf(`{"filesystem_key":"device:%d","device":%d,"roots":["/root"],"state":%q,"available_bytes":1,"total_bytes":2,"target_bytes":1,"reserve_bytes":1,"recovery_bytes":1,"admission_hold":%t,"reason":"r"}`, device, device, state, hold)
}

// replaceJanitorStatus makes every janitor status call return out (or err) and
// counts the calls, so a test can prove the consumer started no process.
func replaceJanitorStatus(t *testing.T, out string, err error) *int {
	t.Helper()
	calls := 0
	previous := runJanitorStatus
	runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) {
		calls++
		return []byte(out), err
	}
	t.Cleanup(func() { runJanitorStatus = previous })
	return &calls
}

func enableJanitorCapacity(t *testing.T, paths config.Paths) {
	t.Helper()
	appendConfig(t, paths, fmt.Sprintf("\n[janitor_capacity]\nenabled = true\nexecutable = \"/usr/local/bin/janitor\"\npolicy_hash = %q\n", janitorTestPolicyHash))
}

func appendConfig(t *testing.T, paths config.Paths, section string) {
	t.Helper()
	f, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(section); err != nil {
		t.Fatal(err)
	}
}

// The producer's decision table (#2359). Only a fresh, approved, covering sample
// whose device is target_recovered or cleanup_needed with admission_hold=false
// admits; everything else, including every way the sample can be wrong, holds.
func TestJanitorCapacityDecision(t *testing.T) {
	now := time.Date(2026, 10, 29, 10, 20, 0, 0, time.UTC)
	const dev = uint64(2306)
	devices := map[uint64]string{dev: "/root/.gitmoot"}
	ok := janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "target_recovered", false))
	for _, tc := range []struct {
		name, out string
		admit     bool
	}{
		{"recovered", ok, true},
		{"cleanup needed without a hold", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "cleanup_needed", false)), true},
		{"cleanup needed carrying a critical hold", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "cleanup_needed", true)), false},
		{"critical", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "critical", true)), false},
		{"no safe reclaim", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "no_safe_reclaim", true)), false},
		{"measurement unknown even without a hold", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "measurement_unknown", false)), false},
		{"an unknown future state", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "frozen", false)), false},
		{"stale sample", janitorStatusJSON(now.Add(-3*time.Minute), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "target_recovered", false)), false},
		{"sample from the future", janitorStatusJSON(now.Add(5*time.Minute), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(dev, "target_recovered", false)), false},
		{"other policy", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, strings.Repeat("0", 64), janitorVolumeJSON(dev, "target_recovered", false)), false},
		{"device not covered", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, janitorVolumeJSON(9999, "target_recovered", false)), false},
		{"unmeasurable root fallback key", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, `{"filesystem_key":"root:/root","device":2306,"state":"target_recovered","admission_hold":false}`), false},
		{"admission_hold missing", janitorStatusJSON(now.Add(-30*time.Second), 2*time.Minute, janitorTestPolicyHash, `{"filesystem_key":"device:2306","device":2306,"state":"target_recovered"}`), false},
		{"no capacity target configured", `{"schema_version":1,"kind":"status","data":{"inventory_report":{}}}`, false},
		{"unsupported schema", strings.Replace(ok, `"schema_version":1`, `"schema_version":2`, 1), false},
		{"not JSON", "janitor: database is locked", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := parseJanitorCapacity([]byte(tc.out), janitorTestPolicyHash, now)
			reason := ""
			if err != nil {
				reason = err.Error()
			} else {
				reason = snapshot.decide(devices)
			}
			if tc.admit != (reason == "") {
				t.Fatalf("admit=%v, got hold reason %q", tc.admit, reason)
			}
		})
	}
	// A second host device the work writes to must be covered too: a healthy
	// home device cannot hide an unreported scratch device.
	snapshot, err := parseJanitorCapacity([]byte(ok), janitorTestPolicyHash, now)
	if err != nil {
		t.Fatal(err)
	}
	if reason := snapshot.decide(map[uint64]string{dev: "/root/.gitmoot", 77: "/scratch"}); !strings.Contains(reason, "device 77 (/scratch) is not covered") {
		t.Fatalf("uncovered second device reason = %q", reason)
	}
}

// Off by default: with no [janitor_capacity] section the dispatch listing is
// unchanged and no janitor process is started.
func TestJanitorCapacityDisabledStartsNoProcess(t *testing.T) {
	ctx, _, _, worker := diskGuardDispatchFixture(t)
	calls := replaceJanitorStatus(t, "", errors.New("must not run"))
	jobs := []db.Job{{ID: "queued-job", State: string(workflow.JobQueued), Payload: `{"repo":"owner/repo"}`}}
	if got := janitorPressureQueuedDispatch(ctx, worker, jobs, "", ""); len(got) != 1 {
		t.Fatalf("disabled consumer dropped jobs: %+v", got)
	}
	if *calls != 0 {
		t.Fatalf("disabled consumer ran janitor %d times", *calls)
	}
}

// Enabled: a held device keeps the queued job out of this dispatch pass (it
// stays queued, with one janitor_pressure_held event), and a fresh recovered
// sample releases it.
func TestJanitorPressureHoldsQueuedWorkUntilRecovery(t *testing.T) {
	ctx, store, paths, worker := diskGuardDispatchFixture(t)
	enableJanitorCapacity(t, paths)
	devices, err := janitorHostDevices([]string{paths.Home, seatTempParent()})
	if err != nil {
		t.Fatal(err)
	}
	volumes := func(state string, hold bool) []string {
		out := []string{}
		for d := range devices {
			out = append(out, janitorVolumeJSON(d, state, hold))
		}
		return out
	}
	job := db.Job{ID: "held-job", Agent: "agent", Type: "review", State: string(workflow.JobQueued), Payload: `{"repo":"owner/unregistered"}`}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	replaceJanitorStatus(t, janitorStatusJSON(time.Now().Add(-10*time.Second), 2*time.Minute, janitorTestPolicyHash, volumes("critical", true)...), nil)
	if got := janitorPressureQueuedDispatch(ctx, worker, []db.Job{job}, "", ""); len(got) != 0 {
		t.Fatalf("critical device dispatched %+v", got)
	}
	latest, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != string(workflow.JobQueued) || !daemonWorkerHasEvent(events, janitorPressureHeldEventKind) {
		t.Fatalf("held job state=%s events=%+v; want queued with %s", latest.State, events, janitorPressureHeldEventKind)
	}
	if !strings.Contains(events[len(events)-1].Message, "janitor holds device") {
		t.Fatalf("hold event does not name the device: %q", events[len(events)-1].Message)
	}

	replaceJanitorStatus(t, janitorStatusJSON(time.Now().Add(-10*time.Second), 2*time.Minute, janitorTestPolicyHash, volumes("target_recovered", false)...), nil)
	if got := janitorPressureQueuedDispatch(ctx, worker, []db.Job{job}, "", ""); len(got) != 1 {
		t.Fatalf("recovered device still holds: %+v", got)
	}

	// Producer unavailable: unknown, so it holds again.
	replaceJanitorStatus(t, "", errors.New("janitor status timed out after 5s"))
	if got := janitorPressureQueuedDispatch(ctx, worker, []db.Job{job}, "", ""); len(got) != 0 {
		t.Fatalf("unavailable producer admitted %+v", got)
	}
}

// A foreground dispatch is refused BEFORE it creates a job or a worktree.
func TestJanitorPressureRefusesForegroundDispatchBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", "shell", "true", []string{"ask"}, "owner/repo")
	paths := config.PathsForHome(home)
	enableJanitorCapacity(t, paths)
	replaceJanitorStatus(t, `{"schema_version":1,"kind":"status","data":{"inventory_report":{}}}`, nil)

	_, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Instructions: "hello", Home: home})
	if err == nil || !strings.Contains(err.Error(), "janitor pressure holds new heavy work") {
		t.Fatalf("dispatch error = %v, want the janitor hold", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused dispatch created jobs: %+v", jobs)
	}
}
