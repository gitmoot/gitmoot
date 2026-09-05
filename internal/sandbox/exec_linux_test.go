//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// goInstall builds a directory that satisfies the Go-installation signature.
// Every should-succeed fixture uses this rather than assuming the signature,
// because the previous round's table pinned a depth heuristic and its rows were
// never proof that a real installation passes.
func goInstall(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.4\ntime 2026-05-29T15:26:39Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// homeFixture returns a scratch directory under the OPERATOR's real home,
// which is where the home arm can accept a root at all.
func homeFixture(t *testing.T) string {
	t.Helper()
	home := operatorHomeDir()
	if home == "" || home == "/" {
		t.Skip("operator home is unavailable or /, so the home arm cannot be exercised")
	}
	dir, err := os.MkdirTemp(home, "gm-sandbox-test-")
	if err != nil {
		t.Skipf("cannot create a fixture under the operator home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestSystemToolchainRoot(t *testing.T) {
	tests := []struct {
		name string
		root string
		want bool
	}{
		{name: "hosted Go", root: "/opt/hostedtoolcache/go/1.26.0/x64", want: true},
		{name: "local Go", root: "/usr/local/go", want: true},
		{name: "Nix Go", root: "/nix/store/hash-go", want: true},
		{name: "snap Go", root: "/snap/go/current", want: true},
		{name: "home tool", root: "/root/.local/bin"},
		{name: "temporary tool", root: "/tmp/toolchain"},
		{name: "home directory", root: "/home/user"},
		{name: "sibling prefix collision", root: "/optional/go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := systemToolchainRoot(test.root); got != test.want {
				t.Fatalf("systemToolchainRoot(%q) = %v, want %v", test.root, got, test.want)
			}
		})
	}
}

// TestOperatorToolchainRootRequiresAGoInstallation is the home arm's boundary,
// asserted with real directories because the signature is a content proof.
//
// EXPECTATION DRIFT FROM THE DEPTH RULE, stated rather than quietly rewritten.
// The previous round's should-succeed rows were written for a depth heuristic
// and two of them change meaning here:
//
//   - ~/.asdf/installs/golang/1.26.4 was GRANTED by depth. It is now REFUSED at
//     that level, because an asdf golang install keeps GOROOT one level deeper;
//     the nested go/ directory is what holds bin/go and VERSION and it is
//     granted instead. The new outcome is correct: the old row granted a
//     directory that is not a Go installation.
//   - ~/.cache/toolchains/go and ~/opt/go were GRANTED by depth alone. They are
//     now granted only when they actually contain an installation, which is
//     what the fixtures below construct.
//
// Depth is gone entirely, so ~/x/y (depth two, no installation) is refused
// while ~/go (depth one, real installation) is granted - the opposite of the
// old rule in both directions.
func TestOperatorToolchainRootRequiresAGoInstallation(t *testing.T) {
	base := homeFixture(t)
	home := operatorHomeDir()

	valid := goInstall(t, filepath.Join(base, "toolchains", "go1.26.4"))
	depthOne := goInstall(t, filepath.Join(base, "go"))

	// an asdf-shaped layout: the version directory is NOT the installation
	asdfVersion := filepath.Join(base, ".asdf", "installs", "golang", "1.26.4")
	asdfGoroot := goInstall(t, filepath.Join(asdfVersion, "go"))

	// a credential-store shape: exists, is deep, holds secrets, no installation
	credentials := filepath.Join(base, ".kimi-code", "credentials")
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "kimi-code.json"), []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// bin/go present, VERSION absent
	binOnly := filepath.Join(base, "bin-only")
	if err := os.MkdirAll(filepath.Join(binOnly, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binOnly, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// VERSION present, bin/go absent
	versionOnly := filepath.Join(base, "version-only")
	if err := os.MkdirAll(versionOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionOnly, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// VERSION that does not name Go
	wrongVersion := goInstall(t, filepath.Join(base, "wrong-version"))
	if err := os.WriteFile(filepath.Join(wrongVersion, "VERSION"), []byte("1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// bin/go present but not executable
	notExecutable := goInstall(t, filepath.Join(base, "not-executable"))
	if err := os.Chmod(filepath.Join(notExecutable, "bin", "go"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A REAL installation OUTSIDE the operator home. Constructed rather than
	// named, because a row naming a path that does not exist asserts nothing:
	// the previous version of this table listed /tmp/toolchain, os.Open failed,
	// and the subtest returned without ever calling the predicate. A mutant that
	// deleted the home boundary entirely survived because of it.
	outside, err := os.MkdirTemp("/tmp", "gm-sandbox-outside-")
	if err != nil {
		t.Skipf("cannot create a fixture outside the operator home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })
	outsideInstall := goInstall(t, filepath.Join(outside, "go1.26.4"))

	// a symlinked intermediate: accepted on purpose (see operatorToolchainRoot)
	linkedParent := filepath.Join(base, "linked")
	if err := os.Symlink(filepath.Join(base, "toolchains"), linkedParent); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		root string
		home string
		want bool
	}{
		{name: "real installation", root: valid, home: home, want: true},
		{name: "installation one segment below home", root: depthOne, home: home, want: true},
		{name: "asdf GOROOT one level deeper", root: asdfGoroot, home: home, want: true},
		{name: "through a symlinked intermediate", root: filepath.Join(linkedParent, "go1.26.4"), home: home, want: true},

		{name: "asdf version directory is not the installation", root: asdfVersion, home: home},
		{name: "credential store", root: credentials, home: home},
		{name: "bin/go without VERSION", root: binOnly, home: home},
		{name: "VERSION without bin/go", root: versionOnly, home: home},
		{name: "VERSION that does not name Go", root: wrongVersion, home: home},
		{name: "bin/go not executable", root: notExecutable, home: home},

		{name: "root-valued home", root: valid, home: "/"},
		{name: "empty home", root: valid, home: ""},
		{name: "relative home", root: valid, home: "relative"},
		{name: "home itself", root: home, home: home},
		{name: "real installation outside home", root: outsideInstall, home: home},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Every row must reach the predicate. An unopenable path used to
			// return early here, which turned refusal rows into assertions
			// about nothing.
			handle, err := os.Open(test.root)
			if err != nil {
				t.Fatalf("open %q: %v; every row must reach the predicate or it asserts nothing", test.root, err)
			}
			defer handle.Close()
			if got := operatorToolchainRoot(test.root, test.home, handle); got != test.want {
				t.Fatalf("operatorToolchainRoot(%q, %q) = %v, want %v", test.root, test.home, got, test.want)
			}
		})
	}
}

// TestGrantableToolchainRootHandleFailsClosed pins the property the round-2 P1-1
// construction depended on: the previous code fell back to the LEXICAL path when
// EvalSymlinks failed, so a self-looping symlink produced a clean-looking root
// that passed every check. Any resolution failure must now yield no grant.
func TestGrantableToolchainRootHandleFailsClosed(t *testing.T) {
	base := homeFixture(t)
	loop := filepath.Join(base, "current")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	// THE DISCRIMINATING ROW. The three symlink rows below are refused even with
	// the fallback restored, because the lexical root cannot be opened - so they
	// do not actually pin fail-closed. This one does: the final component is
	// longer than NAME_MAX, so EvalSymlinks errors while the LEXICAL root is a
	// genuine Go installation that would otherwise be granted. It is the only
	// arm that separates "fails closed" from "happens to be refused later".
	realInstall := goInstall(t, filepath.Join(base, "real", "go1.26.4"))
	overlong := filepath.Join(realInstall, "bin", strings.Repeat("x", 300))
	for _, test := range []struct{ name, executable string }{
		{name: "self-looping symlink", executable: filepath.Join(loop, "bin", "go")},
		{name: "dangling symlink", executable: filepath.Join(dangling, "bin", "go")},
		{name: "absent path", executable: filepath.Join(base, "absent", "bin", "go")},
		{name: "unresolvable name over a valid installation", executable: overlong},
		{name: "empty path", executable: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if handle := grantableToolchainRootHandle(test.executable); handle != nil {
				handle.Close()
				t.Fatalf("grantableToolchainRootHandle(%q) returned a grant; unresolvable paths must fail closed", test.executable)
			}
		})
	}
}

// TestReadableRootsInstallsTheToolchainGrant is the PRODUCTION-PATH assertion.
//
// The previous round had none: every positive assertion pinned a helper, and
// deleting the hook in readableRoots left the whole package green. This enters
// readableRoots and proves the grant ARRIVES, so deleting that hook fails here.
func TestReadableRootsInstallsTheToolchainGrant(t *testing.T) {
	base := homeFixture(t)
	root := goInstall(t, filepath.Join(base, "toolchains", "go1.26.4"))
	t.Setenv("PATH", filepath.Join(root, "bin"))
	if resolved, err := exec.LookPath("go"); err != nil || resolved != filepath.Join(root, "bin", "go") {
		t.Fatalf("LookPath resolved %q err=%v, want the fixture; the assertion below would be vacuous", resolved, err)
	}

	roots, held, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
	if err != nil {
		t.Fatalf("readableRoots returned error: %v", err)
	}
	defer func() {
		for _, handle := range held {
			handle.Close()
		}
	}()
	if len(held) != 1 {
		t.Fatalf("readableRoots held %d descriptors, want exactly 1 for the toolchain grant", len(held))
	}

	want := procFdPath(held[0])
	var found bool
	for _, candidate := range roots {
		if candidate == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("readableRoots = %v, want the toolchain descriptor path %q among them", roots, want)
	}
	// and the descriptor must name the fixture, not merely exist
	target, err := os.Readlink(want)
	if err != nil {
		t.Fatalf("readlink %q: %v", want, err)
	}
	if target != root {
		t.Fatalf("toolchain descriptor points at %q, want the fixture %q", target, root)
	}
}

// TestReadableRootsWithholdsOperatorCredentialDirectories is the containment
// half. A positive assertion alone cannot catch over-granting, so both are kept.
func TestReadableRootsWithholdsOperatorCredentialDirectories(t *testing.T) {
	home := operatorHomeDir()
	if home == "" || home == "/" {
		t.Skip("operator home is unavailable or /, so there is no credential boundary to assert")
	}
	roots, held, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
	if err != nil {
		t.Fatalf("readableRoots returned error: %v", err)
	}
	defer func() {
		for _, handle := range held {
			handle.Close()
		}
	}()
	if len(roots) == 0 {
		t.Fatal("readableRoots returned no roots, so this assertion would pass vacuously")
	}
	withheld := []string{home}
	for _, dir := range []string{".gitmoot", ".codex", ".claude", ".kimi-code", ".config", ".ssh", ".npm", ".local/share"} {
		withheld = append(withheld, filepath.Join(home, dir))
	}
	for _, root := range roots {
		clean := filepath.Clean(root)
		if strings.HasPrefix(clean, "/proc/self/fd/") {
			target, err := os.Readlink(clean)
			if err != nil {
				t.Fatalf("readlink %q: %v", clean, err)
			}
			clean = target
		}
		for _, deny := range withheld {
			if clean == deny {
				t.Fatalf("readableRoots granted %q, which a read-only seat must never read; roots=%v", clean, roots)
			}
		}
		if rel, relErr := filepath.Rel(clean, home); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("readableRoots granted %q, an ancestor of the operator home %q; roots=%v", clean, home, roots)
		}
	}
}

// TestOperatorHomeDirIgnoresARewrittenHOME defends the property that makes the
// grant work AT ALL inside the process that needs it.
//
// MEASURED: a read-only seat rewrites HOME to its own throwaway cache root and
// sandbox-exec inherits that env. With HOME pointed at a scratch directory,
// os.UserHomeDir() returns the scratch path while user.Current().HomeDir still
// returns the operator's home. Written against os.UserHomeDir(), the home arm
// would never match the operator's toolchain and this fix would be INERT in
// exactly the process it exists to serve - a green unit test and an unchanged
// exit 126 in production.
func TestOperatorHomeDirIgnoresARewrittenHOME(t *testing.T) {
	real := operatorHomeDir()
	if real == "" {
		t.Skip("passwd lookup is unavailable, so there is nothing to compare against")
	}
	seatHome := t.TempDir()
	t.Setenv("HOME", seatHome)
	if envHome, err := os.UserHomeDir(); err != nil || envHome != seatHome {
		t.Fatalf("os.UserHomeDir() = %q err=%v, want the rewritten %q; the premise of this test no longer holds", envHome, err, seatHome)
	}
	if got := operatorHomeDir(); got != real {
		t.Fatalf("operatorHomeDir() = %q under a rewritten HOME, want the operator's %q; the toolchain grant would be inert in a seat", got, real)
	}
}

func TestRuntimeHostReadFilesIncludesOpenSSLConfig(t *testing.T) {
	for _, path := range runtimeHostReadFiles {
		if path == "/etc/ssl/openssl.cnf" {
			return
		}
	}
	t.Fatal("runtime host file grants omit /etc/ssl/openssl.cnf; Node-based review runtimes cannot initialize TLS")
}

// TestReadableRootsGrantsProcfs pins the runtime BOOTSTRAP grant that strict
// read-path mode dropped. Without it the Bun-based Claude/Kimi binaries abort
// and codex's bwrap cannot read /proc/sys/kernel/overflowuid, so every read-only
// review dies before doing any work. It is now doubly load-bearing: the
// toolchain rule is installed through a /proc/self/fd path.
func TestReadableRootsGrantsProcfs(t *testing.T) {
	roots, held, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
	if err != nil {
		t.Fatalf("readableRoots returned error: %v", err)
	}
	defer func() {
		for _, handle := range held {
			handle.Close()
		}
	}()
	for _, root := range roots {
		if root == "/proc" {
			return
		}
	}
	t.Fatalf("readableRoots = %v, want /proc among them; review runtimes cannot bootstrap without procfs", roots)
}

// TestToolchainGrantSurvivesDescriptorCloseE2E is the arm that decides whether
// descriptor pinning may be relied on at all.
//
// The installer is not the process that keeps running: RestrictPaths is followed
// by syscall.Exec, so the descriptor is gone by the time the runtime executes.
// This proves the kernel keeps the rule against the INODE - the grant survives
// both closing the descriptor and renaming the original name away, which is the
// discriminator that separates inode pinning from name resolution.
func TestToolchainGrantSurvivesDescriptorCloseE2E(t *testing.T) {
	if os.Getenv("ZZ_FD_CHILD") == "1" {
		toolchainGrantChild()
		return
	}
	base := t.TempDir()
	root := goInstall(t, filepath.Join(base, "toolchains", "go1.26.4"))
	cmd := exec.Command(os.Args[0], "-test.run", "^TestToolchainGrantSurvivesDescriptorCloseE2E$")
	cmd.Env = append(os.Environ(), "ZZ_FD_CHILD=1", "ZZ_FD_ROOT="+root, "ZZ_FD_BASE="+base)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "GRANT-SURVIVED") {
		t.Fatalf("child did not confirm the grant survived:\n%s", out)
	}
}

func toolchainGrantChild() {
	root := os.Getenv("ZZ_FD_ROOT")
	base := os.Getenv("ZZ_FD_BASE")
	handle, err := os.Open(root)
	if err != nil {
		fmt.Println("CHILD open failed:", err)
		os.Exit(3)
	}
	readable, _, err := readableRoots([]string{base}, "/bin/sh")
	if err != nil {
		fmt.Println("CHILD readableRoots failed:", err)
		os.Exit(4)
	}
	if err := restrictForChild(append(readable, procFdPath(handle)), []string{base, os.TempDir()}); err != nil {
		fmt.Println("CHILD RestrictPaths rejected the descriptor path:", err)
		os.Exit(5)
	}
	// the installer's descriptor goes away, exactly as it does before Exec
	_ = handle.Close()
	moved := filepath.Join(base, "moved-away")
	if err := os.Rename(root, moved); err != nil {
		fmt.Println("CHILD rename failed:", err)
		os.Exit(6)
	}
	if _, err := os.ReadFile(filepath.Join(moved, "VERSION")); err != nil {
		fmt.Println("CHILD read after close+rename failed:", err)
		os.Exit(7)
	}
	fmt.Println("GRANT-SURVIVED")
	os.Exit(0)
}

// restrictForChild installs a minimal read-only domain plus the descriptor path
// under test. Separate from execSandbox because that function ends in
// syscall.Exec, which a test cannot survive.
//
// WithRefer mirrors execSandbox. Without it Landlock refuses rename across its
// own rule boundaries with EXDEV, which the first version of this test hit and
// which reads exactly like the grant having been revoked - it is not.
func restrictForChild(readable, writable []string) error {
	return landlock.V3.RestrictPaths(
		landlock.RODirs(readable...),
		landlock.RWDirs(writable...).WithRefer(),
	)
}
