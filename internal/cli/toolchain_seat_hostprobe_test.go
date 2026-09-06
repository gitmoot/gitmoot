package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/toolchain"
)

// TestProductionSeatStagingLaunchesRealHostRuntimes is contract item 5 of ruling
// 122157, retained rather than run once and deleted.
//
// IT IS OPT-IN because it copies the operator's ACTUAL installations, including
// the 269 MiB Go toolchain, so it cannot sit in the default gate. It is retained
// because item 5's requirement is precisely the one a design can satisfy on
// fixtures and fail on the host: "a design that merely turns all four into
// MISSING is not accepted." A fixture proves the wiring; only the real
// installations prove availability, since each has a different shape - claude is
// a symlink into a versioned file, kimi a self-contained executable in a
// credential-bearing profile, codex a node package, Go a pinned tree.
//
// Run it with:
//
//	GITMOOT_SEAT_HOST_PROBE=1 go test ./internal/cli/ -run TestProductionSeatStagingLaunchesRealHostRuntimes -v
//
// It asserts LAUNCH, not staging: the staged artifact is executed with the
// runtime's own version flag, because a copy that lands and cannot run is the
// exact #1918 regression this whole shape exists to prevent.
func TestProductionSeatStagingLaunchesRealHostRuntimes(t *testing.T) {
	if os.Getenv("GITMOOT_SEAT_HOST_PROBE") == "" {
		t.Skip("set GITMOOT_SEAT_HOST_PROBE=1 to stage this host's real runtimes")
	}
	installed := map[string]string{}
	for _, name := range append([]string{"go"}, seatRuntimeNames...) {
		if resolved, err := exec.LookPath(name); err == nil {
			installed[name] = resolved
		} else {
			t.Logf("%s is not installed on this host; skipping its arm", name)
		}
	}
	if len(installed) == 0 {
		t.Skip("no runtime and no Go installed; nothing for this host to prove")
	}

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")

	// One seat per runtime class, so a failure names the runtime it belongs to
	// rather than reporting one arm's launch failure for the whole host.
	for _, selected := range []string{runtime.ClaudeRuntime, runtime.KimiRuntime, runtime.CodexRuntime} {
		resolved, ok := installed[selected]
		if !ok {
			continue
		}
		t.Run(selected, func(t *testing.T) {
			home := t.TempDir()
			live := config.PathsForHome(home)
			agent := runtime.Agent{
				Name: "probe-seat", Runtime: selected, ReadOnlySeat: true,
				RepoScope: "gitmoot/gitmoot",
			}
			grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, "gitmoot/gitmoot", true)
			if err != nil {
				t.Fatalf("readOnlyRuntimeSandboxGrants: %v", err)
			}
			seatPath := ""
			for _, entry := range grants.env {
				if strings.HasPrefix(entry, "PATH=") {
					seatPath = strings.TrimPrefix(entry, "PATH=")
				}
			}
			if seatPath == "" {
				t.Fatalf("production set no PATH for the seat; env = %v", grants.env)
			}
			t.Setenv("PATH", seatPath)

			staged, err := exec.LookPath(selected)
			if err != nil {
				t.Fatalf("seat PATH cannot resolve %q: %v (PATH = %q)", selected, err, seatPath)
			}
			if !pathWithin(staged, toolchain.RuntimeRoot(live.Home)) {
				t.Fatalf("%q resolved to %q, outside the engine runtime root %q: the seat is running the host copy", selected, staged, toolchain.RuntimeRoot(live.Home))
			}
			if staged == resolved {
				t.Fatalf("%q resolved to the operator's own installation %q", selected, resolved)
			}
			output, runErr := exec.Command(staged, "--version").CombinedOutput()
			if runErr != nil {
				t.Fatalf("the staged copy of %q does not launch: %v\noutput=%s", selected, runErr, output)
			}
			t.Logf("%s launched from %s: %s", selected, staged, strings.TrimSpace(string(output)))

			// Every read grant must be a tree the daemon owns. A host root here
			// is the exposure class ruling 122157 deleted.
			for _, read := range grants.reads {
				if pathWithin(resolved, read) {
					t.Errorf("read grant %q contains the operator's own %q installation", read, selected)
				}
			}
			if goResolved, ok := installed["go"]; ok {
				stagedGo, lookErr := exec.LookPath("go")
				if lookErr != nil {
					t.Fatalf("seat PATH cannot resolve go: %v", lookErr)
				}
				if !pathWithin(stagedGo, toolchain.Root(live.Home)) {
					t.Fatalf("go resolved to %q, outside the staged toolchain root %q", stagedGo, toolchain.Root(live.Home))
				}
				goOutput, goErr := exec.Command(stagedGo, "version").CombinedOutput()
				if goErr != nil {
					t.Fatalf("the staged Go toolchain does not run: %v\noutput=%s", goErr, goOutput)
				}
				t.Logf("go launched from %s: %s", stagedGo, strings.TrimSpace(string(goOutput)))
				for _, read := range grants.reads {
					if pathWithin(goResolved, read) && !pathWithin(read, toolchain.Root(live.Home)) {
						t.Errorf("read grant %q contains the operator's own Go installation %q", read, filepath.Dir(goResolved))
					}
				}
			}
		})
	}
}
