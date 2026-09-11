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

// TestStagedToolchainIsNeverInsideASeatWriteGrant is THIS SHAPE'S P1 AS A TEST.
//
// A seat's cache root IS its write grant, and is also the adapter's cleanupRoot.
// A staged copy placed at or beneath it would be writable by the seat, so the
// seat could rewrite its own go binary and shape B would have reproduced the
// defect it exists to remove, with extra steps.
//
// The pair is deliberate: a positive control that a correct placement is
// ACCEPTED, so a passing test cannot mean "the function refuses everything".
func TestStagedToolchainIsNeverInsideASeatWriteGrant(t *testing.T) {
	const cacheRoot = "/var/lib/gitmoot/cache/agent-7"
	const stagedRoot = "/var/lib/gitmoot/toolchains/go1.26.4-abc123"

	for _, test := range []struct {
		name     string
		staged   string
		writes   []string
		accepted bool
	}{
		{
			name:     "sibling of the write grant is accepted",
			staged:   stagedRoot,
			writes:   []string{cacheRoot},
			accepted: true,
		},
		{
			name:   "directly inside the write grant is refused",
			staged: filepath.Join(cacheRoot, "toolchain"),
			writes: []string{cacheRoot},
		},
		{
			name:   "deep inside the write grant is refused",
			staged: filepath.Join(cacheRoot, "a", "b", "c", "go"),
			writes: []string{cacheRoot},
		},
		{
			name:   "equal to the write grant is refused",
			staged: cacheRoot,
			writes: []string{cacheRoot},
		},
		{
			name:   "write grant inside the staged copy is refused",
			staged: stagedRoot,
			writes: []string{filepath.Join(stagedRoot, "bin")},
		},
		{
			// The comparison is on CLEANED paths, so a traversal that lands back
			// inside is refused rather than sneaking past a string test.
			name:   "refused through a non-clean path that resolves inside",
			staged: cacheRoot + "/../agent-7/toolchain",
			writes: []string{cacheRoot},
		},
		{
			// and the mirror: a traversal that genuinely lands OUTSIDE is
			// accepted. Without this arm the case above would also pass a
			// function that refused every path containing "..".
			name:     "accepted through a non-clean path that resolves outside",
			staged:   cacheRoot + "/../../toolchains/go1.26.4",
			writes:   []string{cacheRoot},
			accepted: true,
		},
		{
			name:     "sibling whose name merely PREFIXES the write grant is accepted",
			staged:   "/var/lib/gitmoot/cache/agent-70-toolchain",
			writes:   []string{cacheRoot},
			accepted: true,
		},
		{
			name:   "refused when any one of several grants contains it",
			staged: filepath.Join(cacheRoot, "go"),
			writes: []string{"/var/lib/gitmoot/other", cacheRoot},
		},
		{
			name:     "empty and blank grants are ignored rather than matching everything",
			staged:   stagedRoot,
			writes:   []string{"", "   "},
			accepted: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateStagedToolchainPlacement(test.staged, test.writes)
			if test.accepted && err != nil {
				t.Fatalf("placement %q with writes %q was refused: %v", test.staged, test.writes, err)
			}
			if !test.accepted {
				if err == nil {
					t.Fatalf("placement %q is writable by the seat and was ACCEPTED; the seat could rewrite its own toolchain", test.staged)
				}
				if !strings.Contains(err.Error(), "rewrite its own toolchain") {
					t.Fatalf("refusal %q does not name the hazard", err)
				}
			}
		})
	}
}

func TestStageSeatToolchainShadowsMissingGo(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	paths := config.PathsForHome(t.TempDir())
	root, env, diagnostic, err := stageSeatToolchain(paths, "")
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic != "" {
		t.Fatalf("an ordinarily absent Go emitted a host diagnostic: %q", diagnostic)
	}
	if !pathWithin(root, toolchain.RuntimeRoot(paths.Home)) {
		t.Fatalf("missing Go command root = %q, outside engine runtime root %q", root, toolchain.RuntimeRoot(paths.Home))
	}
	var path string
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			path = strings.TrimPrefix(entry, "PATH=")
		}
	}
	t.Setenv("PATH", path)
	command, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("missing Go has no explicit command on seat PATH: %v", err)
	}
	output, runErr := exec.Command(command).CombinedOutput()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 126 || !strings.Contains(string(output), "runtime unavailable") {
		t.Fatalf("missing Go did not fail explicitly: err=%v output=%q", runErr, output)
	}
}

// TestReadOnlyGrantsStageTheToolchainThroughProduction drives the REAL grant
// builder, because the test it replaces did not.
//
// WHY THIS EXISTS. The previous test constructed readOnlySandboxGrants itself and
// appended the staged path by hand, so a mutant deleting the staged-toolchain
// block in daemon_worker.go left it green. A reviewer demonstrated exactly that.
// A test that pins a helper is not a test of the path: this one enters through
// readOnlyRuntimeSandboxGrants and therefore fails when the wiring is removed.
//
// The fixture is a SMALL pinned installation on PATH rather than the host's real
// 269 MiB toolchain, so the test proves the wiring without paying for a full copy.
func TestReadOnlyGrantsStageTheToolchainThroughProduction(t *testing.T) {
	home := t.TempDir()
	live := config.PathsForHome(home)

	// a minimal but real Go installation the stager will accept
	install := filepath.Join(t.TempDir(), "go1.26.4")
	binDir := filepath.Join(install, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte("#!/bin/sh\necho go1.26.4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "go.env"), []byte("GOTOOLCHAIN=local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Keep the host's large runtime installations out of this toolchain-only
	// test; the production runtime wiring has its own small fixtures below.
	t.Setenv("PATH", strings.Join([]string{binDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)))

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")

	agent := runtime.Agent{
		Name: "seat", Runtime: runtime.CodexRuntime, ReadOnlySeat: true,
		RepoScope: "gitmoot/gitmoot",
	}
	grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, "gitmoot/gitmoot", true)
	if err != nil {
		t.Fatalf("readOnlyRuntimeSandboxGrants: %v", err)
	}

	// 1. the staged copy is granted as a READ. This is the assertion the deletion
	//    mutant must break.
	stagedRoot := toolchain.Root(live.Home)
	var staged string
	for _, granted := range grants.reads {
		if filepath.Dir(granted) == stagedRoot {
			staged = granted
			break
		}
	}
	if staged == "" {
		t.Fatalf("production granted no staged toolchain read; reads = %v. Deleting the daemon_worker wiring must fail HERE", grants.reads)
	}
	if _, statErr := os.Stat(filepath.Join(staged, "bin", "go")); statErr != nil {
		t.Fatalf("the granted read %q is not a real installation: %v", staged, statErr)
	}

	// 2. it is NEVER writable. A seat that can rewrite its own go binary would
	//    reproduce the defect this whole shape exists to remove.
	for _, write := range grants.writes {
		if pathWithin(staged, write) || pathWithin(write, staged) {
			t.Errorf("staged toolchain %q overlaps seat-writable %q", staged, write)
		}
	}
	if err := validateStagedToolchainPlacement(staged, grants.writes); err != nil {
		t.Errorf("production placement is unsafe: %v", err)
	}

	// 3. the seat is actually pointed at it, or the grant is inert.
	for _, want := range []string{
		"GOROOT=" + staged,
		"GOTOOLCHAIN=local",
	} {
		if !containsString(grants.env, want) {
			t.Errorf("env %v does not export %q, so the seat would not use the staged copy", grants.env, want)
		}
	}
	pathSet := false
	for _, entry := range grants.env {
		if strings.HasPrefix(entry, "PATH=") && strings.Contains(entry, filepath.Join(staged, "bin")) {
			pathSet = true
		}
	}
	if !pathSet {
		t.Errorf("env %v does not put the staged bin on PATH", grants.env)
	}

	// 4. a staging miss must NOT become a job event. Three CI race shards caught
	//    that as a host-dependent event stream.
	for _, dropped := range grants.dropped {
		if strings.Contains(strings.ToLower(dropped), "toolchain") {
			t.Errorf("a toolchain diagnostic reached grants.dropped (%q), which is evented as a config narrowing", dropped)
		}
	}
}

// TestReadOnlySeatEnvKeepsRuntimeBinariesResolvable is #1918 AS A TEST.
//
// #1879 replaced the seat's PATH with a fixed list instead of extending the
// inherited one, and every claude and kimi read-only seat stopped launching:
// sandbox-exec resolves argv[0] with exec.LookPath BEFORE any Landlock rule is
// applied (internal/sandbox/exec_linux.go), and the runtime binaries live in
// neither /usr/local/bin nor /usr/bin. Measured boundary: gm-review-opus was
// 11-for-11 before the deploy and 0-for-2 after.
//
// IT ASSERTS RESOLUTION, NOT THE PATH STRING. A test that greps the PATH value
// for a directory passes on any list that happens to contain the substring,
// including one whose entries do not hold the binaries; the failure this
// reproduces is exec.LookPath returning ErrNotFound, so that is what is
// exercised. The go arm is the other half of the boundary: extending PATH must
// not cost the toolchain pin, so `go` must still resolve INSIDE the staged copy
// even though a different go sits earlier on the inherited PATH.
func TestReadOnlySeatEnvKeepsRuntimeBinariesResolvable(t *testing.T) {
	home := t.TempDir()
	live := config.PathsForHome(home)

	install := filepath.Join(t.TempDir(), "go1.26.4")
	installBin := filepath.Join(install, "bin")
	if err := os.MkdirAll(installBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installBin, "go"), []byte("#!/bin/sh\necho go1.26.4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "go.env"), []byte("GOTOOLCHAIN=local\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Three separate runtime locations model profile/package installations.
	// None may survive as a recursive sandbox grant: production must copy each
	// executable into the engine-owned runtime root and resolve the shim there.
	runtimeDirs := map[string]string{}
	pathEntries := []string{installBin}
	for _, name := range []string{"claude", "kimi", "codex"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		runtimeDirs[name] = dir
		pathEntries = append(pathEntries, dir)
	}
	pathEntries = append(pathEntries, "/usr/bin", "/bin")
	t.Setenv("PATH", strings.Join(pathEntries, string(os.PathListSeparator)))

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")

	agent := runtime.Agent{
		Name: "seat", Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true,
		RepoScope: "gitmoot/gitmoot",
	}
	grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, "gitmoot/gitmoot", true)
	if err != nil {
		t.Fatalf("readOnlyRuntimeSandboxGrants: %v", err)
	}

	// The seat's effective PATH is the LAST PATH= in the env, because
	// exec.Cmd dedups cmd.Env keeping the last occurrence of each key and
	// grants.env is appended to os.Environ() by the subprocess runners.
	seatPath := ""
	for _, entry := range grants.env {
		if strings.HasPrefix(entry, "PATH=") {
			seatPath = strings.TrimPrefix(entry, "PATH=")
		}
	}
	if seatPath == "" {
		t.Fatalf("production set no PATH for the seat; env = %v", grants.env)
	}
	if !strings.Contains(seatPath, filepath.Join(toolchain.Root(live.Home))) {
		t.Fatalf("seat PATH %q does not carry the staged toolchain, so this test is measuring the wrong environment", seatPath)
	}
	t.Setenv("PATH", seatPath)

	for _, binary := range []string{"claude", "kimi", "codex"} {
		resolved, lookErr := exec.LookPath(binary)
		if lookErr != nil {
			t.Errorf("seat PATH cannot resolve %q: %v\nsandbox-exec resolves argv[0] with exec.LookPath, so this is exactly the launch failure in #1918.\nseat PATH = %q", binary, lookErr, seatPath)
			continue
		}
		if !pathWithin(resolved, toolchain.RuntimeRoot(live.Home)) {
			t.Errorf("%q resolved to host path %q, outside engine-owned runtime root %q", binary, resolved, toolchain.RuntimeRoot(live.Home))
			continue
		}
		// Every installed runtime must LAUNCH from its staged copy, not merely
		// resolve: an unrunnable copy is the #1918 regression with extra steps.
		if output, runErr := exec.Command(resolved).CombinedOutput(); runErr != nil {
			t.Errorf("staged runtime %q does not launch: %v: %s", binary, runErr, output)
		}
	}
	for name, sourceDir := range runtimeDirs {
		for _, granted := range grants.reads {
			if pathWithin(granted, sourceDir) || pathWithin(sourceDir, granted) {
				t.Errorf("host runtime %q source %q escaped into recursive read grant %q", name, sourceDir, granted)
			}
		}
	}

	// The pin must survive the widening: `go` resolves to the staged copy even
	// though the operator's own installation sits earlier on the inherited PATH.
	resolvedGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("seat PATH cannot resolve go: %v (PATH = %q)", err, seatPath)
	}
	if !pathWithin(resolvedGo, toolchain.Root(live.Home)) {
		t.Errorf("go resolved to %q, outside the staged toolchain root %q: extending PATH must not cost the toolchain pin", resolvedGo, toolchain.Root(live.Home))
	}
}

// TestStageSeatRuntimesCopiesInstalledAndShadowsAbsentRuntimes pins both halves
// of ruling 122157's availability/containment trade: an installed runtime must
// LAUNCH from the engine's own copy, and an absent one must FAIL EXPLICITLY
// rather than fall through to whatever the inherited PATH offers later.
func TestStageSeatRuntimesCopiesInstalledAndShadowsAbsentRuntimes(t *testing.T) {
	// One runtime is deliberately left uninstalled, so the absent arm is
	// measured rather than assumed.
	absent := seatRuntimeNames[len(seatRuntimeNames)-1]
	var pathEntries []string
	for _, name := range seatRuntimeNames {
		if name == absent {
			continue
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf '"+name+"-ran\\n'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		pathEntries = append(pathEntries, dir)
	}
	// FIXTURES ONLY, deliberately: /usr/bin holds a real codex on this host, so
	// appending the system directories made the "absent" arm resolve the
	// operator's own installation and measure nothing (observed).
	t.Setenv("PATH", strings.Join(pathEntries, string(os.PathListSeparator)))

	paths := config.PathsForHome(t.TempDir())
	commands, _, diagnostics, err := stageSeatRuntimes(paths, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("stageSeatRuntimes diagnostics = %v", diagnostics)
	}
	if len(commands) != len(seatRuntimeNames) {
		t.Fatalf("stageSeatRuntimes returned %d commands for %d runtime classes: %v", len(commands), len(seatRuntimeNames), commands)
	}
	for _, command := range commands {
		name := filepath.Base(command)
		if !pathWithin(command, toolchain.RuntimeRoot(paths.Home)) {
			t.Errorf("%q is outside the engine runtime root: %q", name, command)
		}
		output, runErr := exec.Command(command).CombinedOutput()
		if name == absent {
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 126 || !strings.Contains(string(output), "runtime unavailable") {
				t.Errorf("absent runtime %q did not fail explicitly: err=%v output=%q", name, runErr, output)
			}
			continue
		}
		if runErr != nil || strings.TrimSpace(string(output)) != name+"-ran" {
			t.Errorf("installed runtime %q did not launch from its staged copy: err=%v output=%q", name, runErr, output)
		}
	}
}

func TestStageSeatRuntimesAddsOmpOnlyWhenSelected(t *testing.T) {
	runtimeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runtimeDir, runtime.OmpRuntime), []byte("#!/bin/sh\nprintf 'omp-ran\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", runtimeDir)

	without, _, _, err := stageSeatRuntimes(config.PathsForHome(t.TempDir()), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range without {
		if filepath.Base(command) == runtime.OmpRuntime {
			t.Fatalf("unselected OMP was published to a provider seat: %v", without)
		}
	}

	paths := config.PathsForHome(t.TempDir())
	with, _, diagnostics, err := stageSeatRuntimes(paths, runtime.OmpRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("selected OMP staging diagnostics = %v", diagnostics)
	}
	var ompCommand string
	for _, command := range with {
		if filepath.Base(command) == runtime.OmpRuntime {
			ompCommand = command
			break
		}
	}
	if ompCommand == "" || !pathWithin(ompCommand, toolchain.RuntimeRoot(paths.Home)) {
		t.Fatalf("selected OMP command = %q, want daemon-owned staged path", ompCommand)
	}
	output, err := exec.Command(ompCommand).CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "omp-ran" {
		t.Fatalf("staged OMP command failed: err=%v output=%q", err, output)
	}
}

// TestSeatPathRefusesAStagedPathHoldingAListSeparator is the #1921 review P3.
//
// `--home` may legally contain a colon, and a PATH entry may not: the entry
// would SPLIT, so the staged bin directory would stop being one path and `go`
// would resolve to whatever came next — silently unpinning the seat while every
// other signal still claimed a staged toolchain.
//
// The refusal is asserted through stageSeatToolchain's own contract rather than
// seatPath alone where it can be: production must ship a diagnostic AND no env,
// because returning GOROOT with a PATH that does not point at the staged copy
// would claim a pin this shape cannot hold.
func TestSeatPathRefusesAStagedPathHoldingAListSeparator(t *testing.T) {
	separator := string(os.PathListSeparator)

	path, diagnostic := seatPath(filepath.Join("/tmp", "home"+separator+"colon", "toolchains", "go1.26.4"))
	if path != "" {
		t.Errorf("seatPath returned PATH %q for a staged path holding %q; the first entry would be a fragment", path, separator)
	}
	if !strings.Contains(diagnostic, separator) {
		t.Errorf("diagnostic %q does not name the separator that caused the refusal; an operator cannot act on it", diagnostic)
	}

	// An ordinary staged path still yields a PATH and no diagnostic — a guard
	// that also refuses the normal case would disable staging everywhere.
	ordinary, ordinaryDiagnostic := seatPath(filepath.Join(t.TempDir(), "toolchains", "go1.26.4"))
	if ordinaryDiagnostic != "" {
		t.Errorf("a separator-free staged path was refused: %q", ordinaryDiagnostic)
	}
	if ordinary == "" {
		t.Error("a separator-free staged path produced no PATH")
	}
}

// writeSeatToolchainFixture builds a minimal Go installation: selection reads
// only bin/go and VERSION.
func writeSeatToolchainFixture(t *testing.T, root, version string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	return root
}

// TestStageSeatToolchainStagesWhatTheWorkspaceNeeds is #2143's production
// shape at the seat boundary.
//
// The launcher is FIRST on PATH and does not satisfy the workspace, which is
// exactly the host that produced the defect: exec.LookPath returned the 1.22
// launcher, staging copied it, and the seat could not build a go.mod saying
// 1.26. The ordering is load-bearing - a fixture where the first PATH entry
// already satisfies go.mod passes without exercising anything, which is the
// population failure this repository has repeatedly shipped.
func TestStageSeatToolchainStagesWhatTheWorkspaceNeeds(t *testing.T) {
	base := t.TempDir()
	launcher := writeSeatToolchainFixture(t, filepath.Join(base, "distro"), "go1.22.2")
	satisfying := writeSeatToolchainFixture(t, filepath.Join(base, "pinned"), "go1.26.4")

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module x\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{
		filepath.Join(launcher, "bin"),
		filepath.Join(satisfying, "bin"),
	}, string(os.PathListSeparator)))

	paths := config.PathsForHome(t.TempDir())
	staged, env, diagnostic, err := stageSeatToolchain(paths, workspace)
	if err != nil {
		t.Fatalf("stageSeatToolchain: %v", err)
	}
	if diagnostic != "" {
		t.Fatalf("diagnostic = %q, want none", diagnostic)
	}
	if !strings.Contains(filepath.Base(staged), "go1.26.4") {
		t.Errorf("staged %q, want the go1.26.4 tree: the first PATH entry must not decide", staged)
	}
	var sawGoroot, sawSelector bool
	for _, entry := range env {
		if entry == "GOROOT="+staged {
			sawGoroot = true
		}
		if entry == "GOTOOLCHAIN=local" {
			sawSelector = true
		}
	}
	if !sawGoroot {
		t.Errorf("env = %q, want GOROOT pointing at the staged copy", env)
	}
	if !sawSelector {
		t.Error("GOTOOLCHAIN=local missing: a seat that downloads a compiler mid-review is worse than one that fails")
	}
}

func TestStageSeatToolchainFallsBackFromAnUnstageableSatisfyingInstallation(t *testing.T) {
	base := t.TempDir()
	broken := writeSeatToolchainFixture(t, filepath.Join(base, "broken"), "go1.26.0")
	if err := os.MkdirAll(filepath.Join(broken, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir broken pkg: %v", err)
	}
	if err := os.Symlink("outside", filepath.Join(broken, "pkg", "include")); err != nil {
		t.Fatalf("symlink broken include: %v", err)
	}
	usable := writeSeatToolchainFixture(t, filepath.Join(base, "usable"), "go1.26.4")

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module x\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{
		filepath.Join(broken, "bin"),
		filepath.Join(usable, "bin"),
	}, string(os.PathListSeparator)))

	paths := config.PathsForHome(t.TempDir())
	staged, _, diagnostic, err := stageSeatToolchain(paths, workspace)
	if err != nil {
		t.Fatalf("stageSeatToolchain: %v", err)
	}
	if !strings.Contains(filepath.Base(staged), "go1.26.4") {
		t.Errorf("staged %q, want the usable higher installation", staged)
	}
	for _, want := range []string{broken, "pkg/include"} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("diagnostic = %q, want refused lower installation detail %q", diagnostic, want)
		}
	}
}

func TestStageSeatToolchainAggregatesEverySatisfyingStagingRefusal(t *testing.T) {
	base := t.TempDir()
	first := writeSeatToolchainFixture(t, filepath.Join(base, "first"), "go1.26.0")
	second := writeSeatToolchainFixture(t, filepath.Join(base, "second"), "go1.26.4")
	for _, root := range []string{first, second} {
		if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
			t.Fatalf("mkdir %s pkg: %v", root, err)
		}
		if err := os.Symlink("outside", filepath.Join(root, "pkg", "include")); err != nil {
			t.Fatalf("symlink %s include: %v", root, err)
		}
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module x\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{
		filepath.Join(first, "bin"),
		filepath.Join(second, "bin"),
	}, string(os.PathListSeparator)))

	paths := config.PathsForHome(t.TempDir())
	staged, _, diagnostic, err := stageSeatToolchain(paths, workspace)
	if err != nil {
		t.Fatalf("stageSeatToolchain: %v", err)
	}
	if !strings.HasSuffix(filepath.Base(staged), toolchain.UnavailableRuntimeSuffix) {
		t.Errorf("staged %q, want the published-unavailable command", staged)
	}
	for _, want := range []string{first, second, "pkg/include"} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("diagnostic = %q, want refusal detail %q", diagnostic, want)
		}
	}
}

func TestStageSeatToolchainContainsWorkspaceGoModRead(t *testing.T) {
	installation := writeSeatToolchainFixture(t, filepath.Join(t.TempDir(), "go"), "go1.26.4")
	t.Setenv("PATH", filepath.Join(installation, "bin"))

	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "outside symlink",
			setup: func(t *testing.T, workspace string) {
				outside := filepath.Join(t.TempDir(), "outside.mod")
				if err := os.WriteFile(outside, []byte("go 99.0\n"), 0o644); err != nil {
					t.Fatalf("write outside go.mod: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(workspace, "go.mod")); err != nil {
					t.Fatalf("symlink go.mod: %v", err)
				}
			},
		},
		{
			name: "non regular",
			setup: func(t *testing.T, workspace string) {
				if err := os.Mkdir(filepath.Join(workspace, "go.mod"), 0o755); err != nil {
					t.Fatalf("mkdir go.mod: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			test.setup(t, workspace)
			paths := config.PathsForHome(t.TempDir())
			staged, _, diagnostic, err := stageSeatToolchain(paths, workspace)
			if err != nil {
				t.Fatalf("stageSeatToolchain: %v", err)
			}
			if strings.HasSuffix(filepath.Base(staged), toolchain.UnavailableRuntimeSuffix) {
				t.Errorf("staged %q, want the safe local installation", staged)
			}
			if diagnostic != "" {
				t.Errorf("diagnostic = %q, want none", diagnostic)
			}
		})
	}
}

// TestStageSeatToolchainRefusesWhenNothingSatisfiesTheWorkspace pins the loud
// failure. A seat that cannot run the gate must publish the exit-126 shim so
// the capability preflight blocks the review; the outcome that must never
// return is a silent downgrade to a toolchain too old to build the repository,
// which surfaces later as "go.mod requires go >= 1.26" and reads as a
// repository problem rather than a staging one.
func TestStageSeatToolchainRefusesWhenNothingSatisfiesTheWorkspace(t *testing.T) {
	launcher := writeSeatToolchainFixture(t, filepath.Join(t.TempDir(), "distro"), "go1.22.2")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module x\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	t.Setenv("PATH", filepath.Join(launcher, "bin"))

	paths := config.PathsForHome(t.TempDir())
	staged, _, diagnostic, err := stageSeatToolchain(paths, workspace)
	if err != nil {
		t.Fatalf("stageSeatToolchain: %v", err)
	}
	if !strings.HasSuffix(filepath.Base(staged), toolchain.UnavailableRuntimeSuffix) {
		t.Errorf("staged %q, want the published-unavailable command so the preflight blocks the review", staged)
	}
	for _, want := range []string{"go1.26", launcher, "go1.22.2"} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("diagnostic = %q, want it to name %q", diagnostic, want)
		}
	}
}

// TestStageSeatToolchainWithoutAWorkspaceTakesTheNewest covers a seat with no
// module context, which must still stage rather than refuse: not every
// repository a seat reviews is a Go module.
func TestStageSeatToolchainWithoutAWorkspaceTakesTheNewest(t *testing.T) {
	base := t.TempDir()
	older := writeSeatToolchainFixture(t, filepath.Join(base, "older"), "go1.22.2")
	newer := writeSeatToolchainFixture(t, filepath.Join(base, "newer"), "go1.26.4")
	t.Setenv("PATH", strings.Join([]string{
		filepath.Join(older, "bin"),
		filepath.Join(newer, "bin"),
	}, string(os.PathListSeparator)))

	paths := config.PathsForHome(t.TempDir())
	staged, _, diagnostic, err := stageSeatToolchain(paths, "")
	if err != nil {
		t.Fatalf("stageSeatToolchain: %v", err)
	}
	if diagnostic != "" {
		t.Fatalf("diagnostic = %q, want none", diagnostic)
	}
	if !strings.Contains(filepath.Base(staged), "go1.26.4") {
		t.Errorf("staged %q, want the newest installation when no module asks for one", staged)
	}
}
