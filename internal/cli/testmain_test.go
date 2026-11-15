package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/sandbox"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestMain MUST stay in the default build (#1760 step 3). It is the package's
// only TestMain and it does two things before m.Run that untagged tests depend
// on: it dispatches the hidden `sandbox-exec` shim, and it re-execs as a real
// `gitmoot daemon run` child when the env var below is set. Under `go test` the
// current executable IS cli.test, so a test that re-execs itself without this
// dispatch restarts the whole package suite recursively - measured as a
// 38-minute hang in TestRuntimeCredentialCurationForegroundAndDaemonE2E when
// this function briefly sat behind the e2e tag.

// daemonRunChildHomeEnv, when set, flips the re-exec'd test binary into a REAL
// `gitmoot daemon run --home <value>` process (see TestMain). flock(2) is
// process-scoped — two goroutines cannot exercise it — so the #556 singleton
// E2E must launch genuine child processes, and re-exec'ing the already-built
// test binary is how it gets them without shelling out to `go build`.
const daemonRunChildHomeEnv = "GITMOOT_TEST_DAEMON_RUN_CHILD_HOME"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-exec" {
		// Review jobs wrap every runtime through the current executable. Under
		// go test that executable is cli.test, so dispatch the hidden shim before
		// m.Run instead of recursively starting the package suite.
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	if home := os.Getenv(daemonRunChildHomeEnv); home != "" {
		os.Exit(Run([]string{"daemon", "run", "--home", home}, os.Stdout, os.Stderr))
	}
	// Read-only seats (#2314) make a private temp dir under the daemon's
	// TMPDIR when that is short, else under /tmp. Tests compose many seats and
	// not all of them run the job-end cleanup, so point TMPDIR at this run's own
	// short parent and remove it afterwards.
	//
	// No such parent is NOT fatal. Inside a read-only review seat the only
	// writable temp dir is the seat's own TMPDIR and /tmp is denied, so the
	// suite keeps that TMPDIR: the seat's job-end cleanup removes whatever the
	// tests leave there. Nested seats then use that TMPDIR as their parent when
	// it is short (/tmp/gmr-<8 hex>), else <cache root>/tmp.
	//
	// A run that dies before m.Run returns (timeout panic, signal, killed
	// seat) never reaches the removal below, so each run first sweeps the
	// roots such runs leaked (#2644) and marks its own with its owner.
	parents := slices.Compact([]string{os.TempDir(), "/tmp"})
	owner, ownerKnown := currentTestTempRootOwner()
	if ownerKnown {
		if err := sweepLeakedTestTempRoots(parents, newTestTempRootSweep(owner)); err != nil {
			fmt.Fprintf(os.Stderr, "sweep leaked test temp roots: %v\n", err)
		}
	}
	tempRoot, err := makeShortTestTempRoot(parents)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no short test temp root, keeping TMPDIR=%s: %v\n", os.TempDir(), err)
	} else {
		if ownerKnown {
			if err := writeTestTempRootOwner(tempRoot, owner); err != nil {
				fmt.Fprintf(os.Stderr, "mark test temp root owner: %v\n", err)
			}
		}
		if err := os.Setenv("TMPDIR", tempRoot); err != nil {
			fmt.Fprintf(os.Stderr, "set TMPDIR: %v\n", err)
			os.Exit(1)
		}
	}
	// The disk floor applies to every direct dispatch, so tests must not depend
	// on how full the machine running them is. Default to a healthy reading of
	// any existing path; a missing path still fails closed. Disk-guard tests
	// install their own measurement with replaceDiskGuardMeasurement.
	measureDiskGuardFilesystem = func(path string) (diskFilesystemUsage, error) {
		if _, err := os.Stat(path); err != nil {
			return diskFilesystemUsage{}, err
		}
		return diskFilesystemUsage{TotalBytes: 1_000_000_000_000, FreeBytes: 500_000_000_000}, nil
	}
	code := m.Run()
	if tempRoot != "" {
		if err := removeTestTempRoot(tempRoot); err != nil {
			fmt.Fprintf(os.Stderr, "clean test temp root: %v\n", err)
		}
	}
	if err := cleanupSharedGitmootTestBinary(); err != nil {
		fmt.Fprintf(os.Stderr, "clean shared gitmoot test binary: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// makeShortTestTempRoot makes <parent>/gt<8 hex> under the first parent that
// accepts it and leaves it short enough to parent seat temp dirs: /tmp/gt<8 hex>
// is 15 bytes. A parent too long for that is skipped, an unwritable one is
// tried and passed over, and the error names every parent when none works.
func makeShortTestTempRoot(parents []string) (string, error) {
	var errs []error
	for _, parent := range parents {
		dir, err := makeTestTempRootUnder(parent)
		if err == nil {
			return dir, nil
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}

func makeTestTempRootUnder(parent string) (string, error) {
	for range 16 {
		var suffix [4]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", err
		}
		dir := filepath.Join(parent, "gt"+hex.EncodeToString(suffix[:]))
		if !isSeatTempParent(dir) {
			return "", fmt.Errorf("%s: too long to parent seat temp dirs", dir)
		}
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			// Traversable but not listable by other uids, like the /tmp it
			// stands in for: a root test that runs a command as a configured
			// non-root identity (TestDefaultExecutionBackendUsesConfiguredIdentity)
			// must reach its cwd below here, or exec fails with EACCES. Each
			// t.TempDir below is still 0700.
			if err := os.Chmod(dir, 0o711); err != nil {
				_ = os.Remove(dir)
				return "", err
			}
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free %s/gt<hex> name after 16 attempts", parent)
}

// removeTestTempRoot also removes directories a test made read-only (the go
// command does that to module directories).
func removeTestTempRoot(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// testTempRootName is exactly the name makeTestTempRootUnder produces; the
// sweep never looks at anything else in the parent.
var testTempRootName = regexp.MustCompile(`^gt[0-9a-f]{8}$`)

// testTempRootOwnerFile records which process owns a test temp root, so a
// later run can tell a root leaked by a dead run from one a live run uses.
const testTempRootOwnerFile = ".owner"

// testTempRootMarkerlessGrace is how old a root without an owner marker must
// be before the sweep removes it. Such a root is either legacy (made before
// markers, leaked by #2644) or belongs to a run that made it a moment ago and
// has not written its marker yet. Six hours is far past the 25m-1h timeouts
// any test run here uses, and an active run keeps refreshing the root's mtime
// as its t.TempDirs come and go, so only a long-dead root ever ages past it.
const testTempRootMarkerlessGrace = 6 * time.Hour

// testTempRootOwner identifies a process across pid reuse and reboots: the
// /proc starttime changes when the kernel recycles the pid, and the boot id
// changes on every boot. PIDNamespace scopes the pid: a run in another pid
// namespace sharing this /tmp (a container) names pids this run cannot probe.
type testTempRootOwner struct {
	PID          int
	StartTime    string
	BootID       string
	PIDNamespace string
}

// currentTestTempRootOwner reports this process as an owner. ok is false
// without /proc (non-Linux): liveness cannot be proven there, so the run
// neither marks its root nor sweeps, since the age rule alone would be a guess
// about how long a live run lasts and every root there would be marker-less.
func currentTestTempRootOwner() (testTempRootOwner, bool) {
	owner := testTempRootOwner{
		PID:       os.Getpid(),
		StartTime: workflow.RuntimeProcessIdentity(os.Getpid()),
		BootID:    db.BootID(),
	}
	owner.PIDNamespace, _ = os.Readlink("/proc/self/ns/pid")
	return owner, owner.StartTime != "" && owner.BootID != ""
}

// writeTestTempRootOwner writes the marker through a rename, so a concurrent
// sweep reads either no marker (a young root it keeps) or a whole one.
func writeTestTempRootOwner(root string, owner testTempRootOwner) error {
	marker := filepath.Join(root, testTempRootOwnerFile)
	content := fmt.Sprintf("pid=%d\nstarttime=%s\nboot_id=%s\npidns=%s\n", owner.PID, owner.StartTime, owner.BootID, owner.PIDNamespace)
	if err := os.WriteFile(marker+".tmp", []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(marker+".tmp", marker)
}

func readTestTempRootOwner(root string) (testTempRootOwner, error) {
	data, err := os.ReadFile(filepath.Join(root, testTempRootOwnerFile))
	if err != nil {
		return testTempRootOwner{}, err
	}
	fields := map[string]string{}
	for line := range strings.Lines(string(data)) {
		key, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		if !ok {
			return testTempRootOwner{}, fmt.Errorf("malformed owner marker line %q", line)
		}
		fields[key] = value
	}
	pid, err := strconv.Atoi(fields["pid"])
	if err != nil || pid <= 0 || fields["starttime"] == "" || fields["boot_id"] == "" {
		return testTempRootOwner{}, fmt.Errorf("incomplete owner marker %q", data)
	}
	return testTempRootOwner{PID: pid, StartTime: fields["starttime"], BootID: fields["boot_id"], PIDNamespace: fields["pidns"]}, nil
}

// testTempRootSweep decides which sibling test temp roots are leaked. self
// supplies this host's boot id and pid namespace; liveness probes an owner
// (workflow.RuntimeProcessLiveness outside tests).
type testTempRootSweep struct {
	self     testTempRootOwner
	liveness func(pid int, startTime string) (live, known bool)
	now      time.Time
	grace    time.Duration
}

func newTestTempRootSweep(self testTempRootOwner) testTempRootSweep {
	return testTempRootSweep{self: self, liveness: workflow.RuntimeProcessLiveness, now: time.Now(), grace: testTempRootMarkerlessGrace}
}

// sweepLeakedTestTempRoots removes the leaked gt<8 hex> roots directly under
// each parent. It is best-effort: a parent it cannot list (a read-only seat's
// denied /tmp) is skipped, and removal failures come back joined for one
// stderr line; nothing here may fail the run.
func sweepLeakedTestTempRoots(parents []string, sweep testTempRootSweep) error {
	var errs []error
	for _, parent := range parents {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			// DirEntry types come from lstat, so a symlink is never a dir here.
			if !entry.IsDir() || !testTempRootName.MatchString(entry.Name()) {
				continue
			}
			root := filepath.Join(parent, entry.Name())
			info, err := os.Lstat(root)
			if err != nil || !info.IsDir() {
				continue
			}
			// Another user's root is not ours to judge, and /tmp's sticky bit
			// would refuse its removal anyway.
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
				continue
			}
			if !sweep.leaked(root, info) {
				continue
			}
			if err := removeTestTempRoot(root); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// leaked keeps a root unless its owner is proven gone: a different boot, or
// a pid in this namespace that no longer exists or now names another process.
// An owner whose liveness is unknown, or that lives in another pid namespace,
// is kept. A root without a readable marker falls back to the age rule.
func (s testTempRootSweep) leaked(root string, info fs.FileInfo) bool {
	owner, err := readTestTempRootOwner(root)
	if err != nil {
		return s.now.Sub(info.ModTime()) > s.grace
	}
	if owner.BootID != s.self.BootID {
		return true
	}
	if owner.PIDNamespace != s.self.PIDNamespace {
		return false
	}
	live, known := s.liveness(owner.PID, owner.StartTime)
	return known && !live
}

var readOnlySeatLaunch struct {
	once   sync.Once
	reason string
}

// requireReadOnlySeatLaunchable skips a test that runs a read-only seat when
// this host cannot start one at all: a host container runtime exists and
// sandbox-exec cannot hide it here, so it refuses by design (#2318). That covers
// every hiding failure: no CAP_SYS_ADMIN for the mount namespace, or mounts
// denied because the test already runs inside a Landlock domain (a review
// seat). The CI e2e lane runs as root; locally, run as root outside any sandbox.
func requireReadOnlySeatLaunchable(t *testing.T) {
	t.Helper()
	readOnlySeatLaunch.once.Do(func() {
		executable, err := os.Executable()
		if err != nil {
			return
		}
		command := exec.Command(executable, "sandbox-exec", "--read-only-workdir", "--", "/bin/true")
		command.Dir = os.TempDir()
		output, err := command.CombinedOutput()
		if err != nil && strings.Contains(string(output), sandbox.ContainerRuntimeHidingRefusal) {
			readOnlySeatLaunch.reason = strings.TrimSpace(string(output))
		}
	})
	if readOnlySeatLaunch.reason != "" {
		t.Logf("read-only seats cannot start on this host: %s", readOnlySeatLaunch.reason)
		t.Skip("read-only seat refuses to start: a host container runtime exists and cannot be hidden here; run as root, outside any sandbox")
	}
}
