package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1207, bounded half: a deploy must not need an idle window that never arrives.
//
// The guarantee under test is narrow and load-bearing: while draining, the
// daemon claims nothing, and QUEUED WORK IS NOT LOST. A drain that dropped or
// failed queued jobs would be worse than the restart it replaces.

func TestDrainSentinelStartsAbsentAndTogglesBothWays(t *testing.T) {
	home := t.TempDir()

	on, err := daemonDrainActiveForHome(home)
	if err != nil {
		t.Fatalf("initial read: %v", err)
	}
	if on {
		t.Fatal("a fresh home reports draining; the daemon would claim nothing on first start")
	}

	if err := setDaemonDrain(home, true); err != nil {
		t.Fatalf("set drain: %v", err)
	}
	if on, err = daemonDrainActiveForHome(home); err != nil || !on {
		t.Fatalf("after set: draining=%v err=%v, want true", on, err)
	}

	if err := setDaemonDrain(home, false); err != nil {
		t.Fatalf("clear drain: %v", err)
	}
	if on, err = daemonDrainActiveForHome(home); err != nil || on {
		t.Fatalf("after clear: draining=%v err=%v, want false", on, err)
	}

	// Clearing an already-clear drain is not an error: an operator re-running
	// --clear after a restart must not see a failure that looks like a problem.
	if err := setDaemonDrain(home, false); err != nil {
		t.Fatalf("second clear should be a no-op, got %v", err)
	}
}

// THE FLAG IS THE FILE'S EXISTENCE, NOT ITS CONTENTS. A truncated or garbled
// body must still drain, because anything that parses the body can fail open -
// and failing open resumes dispatch during a deploy, which is the single
// outcome drain exists to prevent.
func TestDrainHoldsWithAnUnreadableBody(t *testing.T) {
	home := t.TempDir()
	if err := setDaemonDrain(home, true); err != nil {
		t.Fatalf("set drain: %v", err)
	}
	path, err := daemonDrainSentinelPath(home)
	if err != nil {
		t.Fatalf("sentinel path: %v", err)
	}
	if err := os.WriteFile(path, []byte{0xff, 0x00, 0xff}, 0o600); err != nil {
		t.Fatalf("corrupt sentinel: %v", err)
	}
	on, err := daemonDrainActiveForHome(home)
	if err != nil {
		t.Fatalf("read corrupted sentinel: %v", err)
	}
	if !on {
		t.Fatal("a corrupted sentinel stopped draining; the flag must be existence, not content")
	}
}

// AN UNRESOLVABLE HOME IS NOT A DRAIN. Reporting true would wedge every daemon
// whose home cannot be resolved; reporting false is correct because no operator
// asked for a drain there.
func TestDrainIsInactiveWithoutAHome(t *testing.T) {
	on, err := daemonDrainActiveForHome("")
	if err != nil {
		t.Fatalf("empty home: %v", err)
	}
	if on {
		t.Fatal("an empty home reported draining")
	}
}

// THE CLI CONTRACT, both directions, through Run rather than the helpers - the
// helper-only shape is the defect #2061 f3 filed against this seat, and a
// command that never wires its subcommand would pass a helper test.
func TestDaemonDrainCommandSetsAndClearsTheSentinel(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	// --timeout 0 so the wait loop reports immediately rather than sleeping: the
	// subject here is the sentinel and the wiring, not the wait.
	code := Run([]string{"daemon", "drain", "--home", home, "--timeout", "0"}, &stdout, &stderr)
	if code != 0 && code != 1 {
		t.Fatalf("daemon drain exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped claiming") {
		t.Fatalf("drain did not report that claiming stopped:\n%s", stdout.String())
	}
	on, err := daemonDrainActiveForHome(home)
	if err != nil || !on {
		t.Fatalf("command did not set the sentinel: draining=%v err=%v", on, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"daemon", "drain", "--home", home, "--clear"}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon drain --clear exit = %d, stderr=%s", code, stderr.String())
	}
	if on, err = daemonDrainActiveForHome(home); err != nil || on {
		t.Fatalf("command did not clear the sentinel: draining=%v err=%v", on, err)
	}
}

// THE GUARANTEE THAT MATTERS: queued work survives a drain untouched. A drain
// that lost, cancelled or mutated queued jobs would be worse than the restart it
// replaces, and nothing else in this file would notice.
func TestDrainLeavesQueuedWorkUntouched(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedDaemonWorkerAgent(t, store, "worker", "shell", "printf ok", []string{"review"}, "owner/repo")

	before := queuedJobSnapshot(t, store)
	if len(before) == 0 {
		t.Skip("fixture produced no queued jobs; nothing to protect")
	}

	if err := setDaemonDrain(home, true); err != nil {
		t.Fatalf("set drain: %v", err)
	}
	after := queuedJobSnapshot(t, store)
	if len(after) != len(before) {
		t.Fatalf("queued job count changed across a drain: %d -> %d", len(before), len(after))
	}
	for id, state := range before {
		if after[id] != state {
			t.Fatalf("queued job %s changed state across a drain: %q -> %q", id, state, after[id])
		}
	}
	// And the sentinel lives inside the resolved gitmoot home, not beside it -
	// re-resolving a home is the #446/#459 bug class.
	path, err := daemonDrainSentinelPath(home)
	if err != nil {
		t.Fatalf("sentinel path: %v", err)
	}
	if strings.Contains(filepath.ToSlash(path), ".gitmoot/.gitmoot") {
		t.Fatalf("sentinel path double-resolved the home: %s", path)
	}
}

func queuedJobSnapshot(t *testing.T, store *db.Store) map[string]string {
	t.Helper()
	jobs, err := store.ListQueuedJobs(context.Background())
	if err != nil {
		t.Fatalf("snapshot queued jobs: %v", err)
	}
	snap := make(map[string]string, len(jobs))
	for _, job := range jobs {
		snap[job.ID] = job.State
	}
	return snap
}

// THE PRODUCTION PATH. listPendingQueuedJobs is the choke every scheduler passes
// immediately before selecting work, so this is where drain has to bite. The
// helper tests above would all pass against a build where the guard was never
// wired into it - the exact shape #2061 f3 filed against this seat.
func TestDrainStopsTheDispatchPathFromSelectingWork(t *testing.T) {
	ctx, _, _, worker := diskGuardDispatchFixture(t)

	before, err := listPendingQueuedJobs(ctx, worker, "owner/repo", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("fixture offered no eligible work, so this test could not detect a drain")
	}

	original := daemonDrainHome
	daemonDrainHome = worker.ConfigHome
	t.Cleanup(func() { daemonDrainHome = original })
	if err := setDaemonDrain(worker.ConfigHome, true); err != nil {
		t.Fatalf("set drain: %v", err)
	}

	during, err := listPendingQueuedJobs(ctx, worker, "owner/repo", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs while draining: %v", err)
	}
	if len(during) != 0 {
		t.Fatalf("dispatch selected %d jobs while draining; the daemon would claim during a deploy", len(during))
	}

	// AND IT RESUMES. A drain that could not be lifted would be an outage with a
	// friendlier name.
	if err := setDaemonDrain(worker.ConfigHome, false); err != nil {
		t.Fatalf("clear drain: %v", err)
	}
	after, err := listPendingQueuedJobs(ctx, worker, "owner/repo", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs after clearing: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("after clearing drain %d jobs are eligible, want the original %d", len(after), len(before))
	}
}
