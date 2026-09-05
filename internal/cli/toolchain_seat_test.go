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

// TestPinnedToolchainRootRefusesSystemPrefixes is the scope boundary jarvis set:
// system-prefix toolchains keep their pre-existing location-only grant inside
// internal/sandbox and are NOT copied. If this path ever started returning them,
// two owners would be competing for one decision.
func TestPinnedToolchainRootRefusesSystemPrefixes(t *testing.T) {
	for _, refused := range []string{
		"/opt/go/bin/go",
		"/opt/nested/deeper/go/bin/go",
		"/usr/local/go/bin/go",
		"/usr/local/bin/go",
		"/nix/store/abc-go-1.26.4/bin/go",
		"/snap/go/current/bin/go",
	} {
		if root, ok := pinnedToolchainRoot(refused); ok {
			t.Fatalf("pinnedToolchainRoot(%q) = %q, want refused: system prefixes belong to internal/sandbox", refused, root)
		}
	}

	// CONTROL: an operator-pinned path IS returned, so the refusals above are
	// about the prefixes rather than about the function refusing everything.
	for path, want := range map[string]string{
		"/root/.local/toolchains/go1.26.4/bin/go": "/root/.local/toolchains/go1.26.4",
		"/home/op/sdk/go1.26.4/bin/go":            "/home/op/sdk/go1.26.4",
		"/opt-not-a-prefix/go/bin/go":             "/opt-not-a-prefix/go",
		"/usr/localish/go/bin/go":                 "/usr/localish/go",
	} {
		root, ok := pinnedToolchainRoot(path)
		if !ok || root != want {
			t.Fatalf("pinnedToolchainRoot(%q) = %q,%v; want %q,true", path, root, ok, want)
		}
	}

	// and a path that is not an installation layout at all
	if _, ok := pinnedToolchainRoot("/somewhere/go"); ok {
		t.Fatal("a go NOT under bin/ or sbin/ was accepted as an installation")
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
	// prepend so LookPath finds the fixture, while git and friends still resolve
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

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
		if strings.HasPrefix(granted, stagedRoot+string(filepath.Separator)) || granted == stagedRoot {
			staged = granted
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

	// Two SEPARATE directories, because that is the shape on the live host:
	// claude sits in /root/.local/bin and kimi in /root/.kimi-code/bin, so a fix
	// that rescues one hardcoded directory would still strand the other.
	claudeDir := t.TempDir()
	kimiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(claudeDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiDir, "kimi"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{installBin, claudeDir, kimiDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)))

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
	for _, binary := range []string{"claude", "kimi"} {
		resolved, lookErr := exec.LookPath(binary)
		if lookErr != nil {
			t.Errorf("seat PATH cannot resolve %q: %v\nsandbox-exec resolves argv[0] with exec.LookPath, so this is exactly the launch failure in #1918.\nseat PATH = %q", binary, lookErr, seatPath)
			continue
		}
		if _, statErr := os.Stat(resolved); statErr != nil {
			t.Errorf("resolved %q to %q, which does not exist: %v", binary, resolved, statErr)
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
