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
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/sandbox"
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
	tempRoot, err := makeShortTestTempRoot(slices.Compact([]string{os.TempDir(), "/tmp"}))
	if err != nil {
		fmt.Fprintf(os.Stderr, "no short test temp root, keeping TMPDIR=%s: %v\n", os.TempDir(), err)
	} else if err := os.Setenv("TMPDIR", tempRoot); err != nil {
		fmt.Fprintf(os.Stderr, "set TMPDIR: %v\n", err)
		os.Exit(1)
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
