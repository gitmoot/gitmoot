package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
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
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedDaemonWorkerAgent(t, store, "worker", "shell", "printf ok", []string{"review"}, "owner/repo")

	// SEEDED EXPLICITLY, AND A MISSING FIXTURE IS A FAILURE RATHER THAN A SKIP.
	// The first version of this test called t.Skip here, and the agent fixture
	// creates no jobs - so the "guarantee that matters" asserted NOTHING and
	// reported PASS. That is this campaign's own defect class (a record that
	// cannot evidence what it claims) inside the test defending against it.
	// Found in review (#2096).
	for _, id := range []string{"queued-alpha", "queued-beta"} {
		if err := store.CreateJobWithEvent(ctx, db.Job{
			ID: id, Agent: "worker", Type: "review", State: string(workflow.JobQueued),
			Repo: "owner/repo", Payload: "{}",
		}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	before := queuedJobSnapshot(t, store)
	if len(before) != 2 {
		t.Fatalf("fixture must produce the queued jobs this test protects; got %d", len(before))
	}

	// THE COMMAND, NOT THE HELPER. setDaemonDrain only writes a file, so driving
	// it here would leave every line of runDaemonDrain untested and a drain that
	// cancelled queued work would still pass.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"daemon", "drain", "--home", home, "--timeout", "5s"}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon drain exit = %d, stderr=%s", code, stderr.String())
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

// A STAT ERROR THAT IS NOT "MISSING" MUST NOT READ AS "NOT DRAINING".
//
// The guard's default arm returns an error, and the caller aborts the listing on
// it - so an unreadable sentinel pauses dispatch rather than resuming it. That
// branch had no test, and it is the one a mutant survives.
//
// THE FIXTURE SHAPE IS gm-staged's, FROM THEIR OWN MISTAKE: their version of
// this test made the marker path a DIRECTORY. A directory STATS FINE, so the
// test reached the ordinary present branch, asserted the right conclusion for
// the wrong reason, and a mutant flipping the error arm survived it. Making the
// marker's PARENT a regular file yields ENOTDIR, which is a real stat failure,
// and the assertion checks the error is NEITHER nil NOR IsNotExist before
// concluding anything.
func TestDrainStatFailureIsAnErrorNotAQuietResume(t *testing.T) {
	home := t.TempDir()
	path, err := daemonDrainSentinelPath(home)
	if err != nil {
		t.Fatalf("sentinel path: %v", err)
	}
	parent := filepath.Dir(path)
	if err := os.RemoveAll(parent); err != nil {
		t.Fatalf("clear parent: %v", err)
	}
	// The parent is now a FILE, so stat of a path beneath it fails ENOTDIR.
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("make parent a file: %v", err)
	}

	on, statErr := daemonDrainActiveForHome(home)
	if statErr == nil {
		t.Fatal("an unreadable sentinel returned no error; dispatch would resume during a deploy")
	}
	if errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("ENOTDIR was classified as missing, which resumes claiming: %v", statErr)
	}
	if on {
		t.Fatal("the guard reported draining AND an error; the caller must decide on the error alone")
	}
}
