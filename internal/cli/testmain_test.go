package cli

import (
	"fmt"
	"os"
	"testing"
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
	code := m.Run()
	if err := cleanupSharedGitmootTestBinary(); err != nil {
		fmt.Fprintf(os.Stderr, "clean shared gitmoot test binary: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
