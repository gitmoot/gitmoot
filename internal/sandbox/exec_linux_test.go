//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// goInstall builds a directory satisfying the Go-installation signature with the
// member directories a real installation exposes. Fixtures are real rather than
// named: the signature is a content proof, so a row that only names a path
// asserts nothing about it.
func goInstall(t *testing.T, root string) string {
	t.Helper()
	for _, dir := range []string{"bin", "pkg", "src", "lib"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.4\ntime 2026-05-29T15:26:39Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.env"), []byte("GOTOOLCHAIN=local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// homeFixture returns a scratch directory created directly under the operator's
// home. Fixtures nest inside it, so a case that must sit exactly one segment
// below home cannot be built here - TestOperatorToolchainRootContainment covers
// that boundary with pure paths instead.
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

// TestOperatorToolchainRootContainment pins the home boundary alone.
//
// RESTORED BY NAME, because round 2 deleted these rather than restating them:
// the sdk, GOPATH, ~/opt and ~/.cache layouts appear here and again as real
// fixtures below. The one-segment row matters because every filesystem fixture
// nests inside a scratch directory and is therefore two or more segments down,
// which would leave a reintroduced depth-two minimum invisible.
func TestOperatorToolchainRootContainment(t *testing.T) {
	const home = "/home/operator"
	tests := []struct {
		name string
		root string
		home string
		want bool
	}{
		{name: "one segment below home", root: home + "/go", home: home, want: true},
		{name: "go official sdk layout", root: home + "/sdk/go1.26.4", home: home, want: true},
		{name: "gopath toolchain", root: home + "/go/toolchains/go1.26.4", home: home, want: true},
		{name: "opt layout", root: home + "/opt/go", home: home, want: true},
		{name: "cache layout", root: home + "/.cache/toolchains/go", home: home, want: true},
		{name: "local toolchains layout", root: home + "/.local/toolchains/go1.26.4", home: home, want: true},

		{name: "home itself", root: home, home: home},
		{name: "parent of home", root: "/home", home: home},
		{name: "outside home", root: "/tmp/toolchain", home: home},
		{name: "sibling prefix collision", root: "/home/operator2/go", home: home},
		{name: "root-valued home", root: "/var/secrets", home: "/"},
		{name: "empty home", root: home + "/go", home: ""},
		{name: "relative home", root: home + "/go", home: "relative"},
		{name: "relative root", root: "go", home: home},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := operatorToolchainRoot(test.root, test.home); got != test.want {
				t.Fatalf("operatorToolchainRoot(%q, %q) = %v, want %v", test.root, test.home, got, test.want)
			}
		})
	}
}

// TestDescribeToolchainRequiresAGoInstallation asserts the signature, the
// enumeration and the narrowing through a real descriptor - the only object the
// decision may consult.
func TestDescribeToolchainRequiresAGoInstallation(t *testing.T) {
	base := homeFixture(t)

	valid := goInstall(t, filepath.Join(base, "toolchains", "go1.26.4"))
	sdk := goInstall(t, filepath.Join(base, "sdk", "go1.26.4"))
	gopath := goInstall(t, filepath.Join(base, "go", "toolchains", "go1.26.4"))
	optLayout := goInstall(t, filepath.Join(base, "opt", "go"))
	cacheLayout := goInstall(t, filepath.Join(base, ".cache", "toolchains", "go"))
	asdfVersion := filepath.Join(base, ".asdf", "installs", "golang", "1.26.4")
	asdfGoroot := goInstall(t, filepath.Join(asdfVersion, "go"))

	// a MINIMAL layout: only bin, VERSION and go.env. Absent members must skip.
	minimal := filepath.Join(base, "minimal")
	if err := os.MkdirAll(filepath.Join(minimal, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(minimal, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(minimal, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	credentials := filepath.Join(base, ".kimi-code", "credentials")
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "kimi-code.json"), []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// the round-2 P1: a credential directory that ALSO carries the signature
	credentialWithSignature := goInstall(t, filepath.Join(base, ".ssh"))
	if err := os.WriteFile(filepath.Join(credentialWithSignature, "id_ed25519"), []byte("PRIVATE-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	binOnly := filepath.Join(base, "bin-only")
	if err := os.MkdirAll(filepath.Join(binOnly, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binOnly, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	versionOnly := filepath.Join(base, "version-only")
	if err := os.MkdirAll(versionOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionOnly, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wrongVersion := goInstall(t, filepath.Join(base, "wrong-version"))
	if err := os.WriteFile(filepath.Join(wrongVersion, "VERSION"), []byte("1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	notExecutable := goInstall(t, filepath.Join(base, "not-executable"))
	if err := os.Chmod(filepath.Join(notExecutable, "bin", "go"), 0o644); err != nil {
		t.Fatal(err)
	}

	// VERSION as a symlink to a regular file starting with "go": O_NOFOLLOW must
	// refuse rather than accept the target's content.
	symlinkedVersion := goInstall(t, filepath.Join(base, "symlinked-version"))
	decoy := filepath.Join(base, "decoy-version")
	if err := os.WriteFile(decoy, []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(symlinkedVersion, "VERSION")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, filepath.Join(symlinkedVersion, "VERSION")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		root      string
		want      bool
		wantFiles int
	}{
		{name: "real installation", root: valid, want: true, wantFiles: 2},
		{name: "go official sdk layout", root: sdk, want: true, wantFiles: 2},
		{name: "gopath toolchain", root: gopath, want: true, wantFiles: 2},
		{name: "opt layout", root: optLayout, want: true, wantFiles: 2},
		{name: "cache layout", root: cacheLayout, want: true, wantFiles: 2},
		{name: "asdf GOROOT one level deeper", root: asdfGoroot, want: true, wantFiles: 2},
		{name: "minimal layout skips absent members", root: minimal, want: true, wantFiles: 1},
		{name: "credential dir carrying the signature is granted NARROWLY", root: credentialWithSignature, want: true, wantFiles: 2},

		{name: "asdf version directory is not the installation", root: asdfVersion},
		{name: "credential store", root: credentials},
		{name: "bin/go without VERSION", root: binOnly},
		{name: "VERSION without bin/go", root: versionOnly},
		{name: "VERSION that does not name Go", root: wrongVersion},
		{name: "bin/go not executable", root: notExecutable},
		{name: "VERSION is a symlink", root: symlinkedVersion},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handle, err := os.Open(test.root)
			if err != nil {
				t.Fatalf("open %q: %v; every row must reach the predicate or it asserts nothing", test.root, err)
			}
			grant := describeToolchain(handle)
			if (grant != nil) != test.want {
				if grant != nil {
					grant.close()
				} else {
					handle.Close()
				}
				t.Fatalf("describeToolchain(%q) granted=%v, want %v", test.root, grant != nil, test.want)
			}
			if grant == nil {
				handle.Close()
				return
			}
			defer grant.close()

			// EVERY installed path must be a descriptor path, never a name.
			for _, path := range append(append([]string{}, grant.dirs...), grant.files...) {
				if !strings.HasPrefix(path, "/proc/self/fd/") {
					t.Fatalf("grant installs %q by NAME; every member must be its own descriptor", path)
				}
			}
			// and the installation ROOT must never be granted
			root := procFdPath(handle)
			for _, dir := range grant.dirs {
				if dir == root {
					t.Fatalf("grant includes the installation root %q; only members may be granted", root)
				}
			}
			if len(grant.files) != test.wantFiles {
				t.Fatalf("grant names %d root files, want %d", len(grant.files), test.wantFiles)
			}
			// the sibling secret must not be reachable through any granted path
			for _, path := range append(append([]string{}, grant.dirs...), grant.files...) {
				resolved, err := filepath.EvalSymlinks(path)
				if err != nil {
					continue
				}
				if filepath.Base(resolved) == "id_ed25519" {
					t.Fatalf("grant reaches the sibling secret at %q", resolved)
				}
			}
		})
	}
}

// TestDescribeToolchainRefusesASymlinkLeavingHome pins the policy the round-2
// doc comment DENIED. That comment claimed a ~/.local symlinked to a data volume
// still works; it does not, because the resolved path leaves home.
func TestDescribeToolchainRefusesASymlinkLeavingHome(t *testing.T) {
	base := homeFixture(t)
	outside, err := os.MkdirTemp("/tmp", "gm-sandbox-outside-")
	if err != nil {
		t.Skipf("cannot create a fixture outside the operator home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })
	away := goInstall(t, filepath.Join(outside, "go1.26.4"))

	link := filepath.Join(base, "volume")
	if err := os.Symlink(away, link); err != nil {
		t.Fatal(err)
	}
	if grant := grantableToolchain(filepath.Join(link, "bin", "go")); grant != nil {
		grant.close()
		t.Fatal("a toolchain whose resolved path leaves the operator home was granted; containment is on the resolved path")
	}

	// CONTROL: the same shape reached by a path that stays inside home IS
	// granted, or this test would pass with the arm broken entirely.
	inside := goInstall(t, filepath.Join(base, "inside", "go1.26.4"))
	insideLink := filepath.Join(base, "pointer")
	if err := os.Symlink(inside, insideLink); err != nil {
		t.Fatal(err)
	}
	grant := grantableToolchain(filepath.Join(insideLink, "bin", "go"))
	if grant == nil {
		t.Fatal("control failed: a symlink that stays inside home must still be granted")
	}
	grant.close()
}

// TestGoInstallationAtRefusesAFifoWithoutBlocking pins the availability half of
// round 2's P2: VERSION was opened before its type was proven, so a FIFO with no
// writer would BLOCK SANDBOX STARTUP rather than be refused.
func TestGoInstallationAtRefusesAFifoWithoutBlocking(t *testing.T) {
	base := homeFixture(t)
	root := goInstall(t, filepath.Join(base, "fifo-version"))
	if err := os.Remove(filepath.Join(root, "VERSION")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "VERSION"), 0o644); err != nil {
		t.Skipf("cannot create a FIFO fixture: %v", err)
	}
	handle, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	done := make(chan bool, 1)
	go func() { done <- goInstallationAt(handle) }()
	select {
	case granted := <-done:
		if granted {
			t.Fatal("a FIFO VERSION was accepted; type must be proven before content is read")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("goInstallationAt BLOCKED on a FIFO with no writer; sandbox startup would hang")
	}
}

// TestGrantableToolchainFailsClosed pins that any resolution failure yields no
// grant. The over-long row is the discriminating one: the symlink rows are
// refused anyway because the lexical root cannot be opened, so only a case where
// resolution fails OVER A GENUINE INSTALLATION separates fail-closed from
// happens-to-be-refused-later.
func TestGrantableToolchainFailsClosed(t *testing.T) {
	base := homeFixture(t)
	loop := filepath.Join(base, "current")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	realInstall := goInstall(t, filepath.Join(base, "real", "go1.26.4"))
	overlong := filepath.Join(realInstall, "bin", strings.Repeat("x", 300))

	for _, test := range []struct{ name, executable string }{
		{name: "self-looping symlink", executable: filepath.Join(loop, "bin", "go")},
		{name: "dangling symlink", executable: filepath.Join(dangling, "bin", "go")},
		{name: "absent path", executable: filepath.Join(base, "absent", "bin", "go")},
		{name: "unresolvable name over a valid installation", executable: overlong},
		{name: "empty path", executable: ""},
		{name: "not a bin directory", executable: filepath.Join(realInstall, "go")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if grant := grantableToolchain(test.executable); grant != nil {
				grant.close()
				t.Fatalf("grantableToolchain(%q) returned a grant; resolution failures must fail closed", test.executable)
			}
		})
	}
}

// TestDescribeToolchainRefusesAnUnlinkedRoot asserts that an unlinked root is
// refused. It does NOT pin the " (deleted)" check specifically, and saying so
// matters: measured, RemoveAll takes the contents with the directory, so the
// signature check refuses this same input one step later and a mutant deleting
// the suffix refusal survives. The property asserted here is the outcome; the
// suffix check itself is defence in depth for a racing swap no deterministic
// test can construct.
func TestDescribeToolchainRefusesAnUnlinkedRoot(t *testing.T) {
	base := homeFixture(t)
	root := goInstall(t, filepath.Join(base, "doomed", "go1.26.4"))
	handle, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if grant := describeToolchain(handle); grant == nil {
		t.Fatal("control failed: the fixture must be grantable before it is unlinked")
	} else {
		grant.close()
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if grant := describeToolchain(handle); grant != nil {
		grant.close()
		t.Fatal("an unlinked root was classified; readlink reports it as deleted and it must be refused")
	}
}

// TestReadableRootsInstallsTheToolchainGrant is the PRODUCTION-PATH assertion:
// it enters readableRoots and proves the narrow grant ARRIVES and the
// installation root DOES NOT. Deleting the hook fails here.
func TestReadableRootsInstallsTheToolchainGrant(t *testing.T) {
	base := homeFixture(t)
	root := goInstall(t, filepath.Join(base, "toolchains", "go1.26.4"))
	t.Setenv("PATH", filepath.Join(root, "bin"))
	if resolved, err := exec.LookPath("go"); err != nil || resolved != filepath.Join(root, "bin", "go") {
		t.Fatalf("LookPath resolved %q err=%v, want the fixture; the assertion below would be vacuous", resolved, err)
	}

	roots, files, held, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
	if err != nil {
		t.Fatalf("readableRoots returned error: %v", err)
	}
	defer func() {
		for _, handle := range held {
			handle.Close()
		}
	}()
	if len(held) < 2 {
		t.Fatalf("readableRoots held %d descriptors, want the root plus one per member", len(held))
	}

	// resolve every granted path and check what it actually names
	granted := map[string]bool{}
	for _, candidate := range append(append([]string{}, roots...), files...) {
		if !strings.HasPrefix(candidate, "/proc/self/fd/") {
			continue
		}
		resolved, err := os.Readlink(candidate)
		if err != nil {
			continue
		}
		if resolved == root {
			t.Fatalf("readableRoots granted the installation ROOT %q; only members may be granted", resolved)
		}
		granted[resolved] = true
	}
	for _, member := range []string{"bin", "pkg", "src", "lib", "VERSION", "go.env"} {
		if !granted[filepath.Join(root, member)] {
			t.Fatalf("member %q was not granted; granted=%v", member, granted)
		}
	}
}

// TestReadableRootsWithholdsOperatorCredentialDirectories is the containment
// half. A positive assertion alone cannot catch over-granting, so both are kept.
func TestReadableRootsWithholdsOperatorCredentialDirectories(t *testing.T) {
	home := operatorHomeDir()
	if home == "" || home == "/" {
		t.Skip("operator home is unavailable or /, so there is no credential boundary to assert")
	}
	roots, _, held, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
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
			resolved, err := os.Readlink(clean)
			if err != nil {
				continue
			}
			clean = resolved
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
// exactly the process it exists to serve.
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
// read-path mode dropped. It is doubly load-bearing now: every toolchain rule is
// installed through a /proc/self/fd path.
func TestReadableRootsGrantsProcfs(t *testing.T) {
	roots, _, held, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
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

// TestToolchainMemberGrantIsInodePinnedE2E decides whether descriptor pinning may
// be relied on, and grants NO ANCESTOR of the installation.
//
// Round 2's version granted the fixture's parent read-write, so post-rename reads
// passed through ancestor rules whether or not the descriptor path was installed
// - it could not fail. Here the only route is the /proc/self/fd rule, and the
// MEMBER is renamed rather than the root, which is what discriminates per-member
// descriptor pinning from name resolution below a pinned root.
func TestToolchainMemberGrantIsInodePinnedE2E(t *testing.T) {
	if os.Getenv("ZZ_FD_CHILD") == "1" {
		toolchainGrantChild()
		return
	}
	for _, mode := range []struct {
		name    string
		install string
		wantOK  bool
	}{
		{name: "member descriptor path", install: "fd", wantOK: true},
		{name: "member original name", install: "name"},
		{name: "no member grant", install: "none"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			base := t.TempDir()
			root := goInstall(t, filepath.Join(base, "toolchains", "go1.26.4"))
			cmd := exec.Command(os.Args[0], "-test.run", "^TestToolchainMemberGrantIsInodePinnedE2E$")
			cmd.Env = append(os.Environ(),
				"ZZ_FD_CHILD=1", "ZZ_FD_ROOT="+root, "ZZ_FD_MODE="+mode.install)
			out, err := cmd.CombinedOutput()
			ok := err == nil && strings.Contains(string(out), "GRANT-SURVIVED")
			if ok != mode.wantOK {
				t.Fatalf("install=%s survived=%v want %v\n%s", mode.install, ok, mode.wantOK, out)
			}
		})
	}
}

func toolchainGrantChild() {
	root := os.Getenv("ZZ_FD_ROOT")
	mode := os.Getenv("ZZ_FD_MODE")
	member := filepath.Join(root, "src")
	if err := os.WriteFile(filepath.Join(member, "marker"), []byte("present"), 0o644); err != nil {
		fmt.Println("CHILD marker failed:", err)
		os.Exit(3)
	}
	handle, err := os.Open(member)
	if err != nil {
		fmt.Println("CHILD open member failed:", err)
		os.Exit(3)
	}
	// THE SWAP WINDOW, reproduced faithfully: the member is renamed AFTER the
	// descriptor is taken and BEFORE the rule is installed, which is exactly the
	// interval condition 1 is about. It happens here, outside any sandbox,
	// because inside the domain no ancestor of the installation is writable -
	// which is the point of granting members only.
	moved := filepath.Join(root, "src-moved")
	if err := os.Rename(member, moved); err != nil {
		fmt.Println("CHILD rename member failed:", err)
		os.Exit(4)
	}
	scratch, err := os.MkdirTemp("/tmp", "gm-fd-child-")
	if err != nil {
		fmt.Println("CHILD scratch failed:", err)
		os.Exit(3)
	}
	reads := []string{"/proc", "/bin", "/usr/bin", "/lib", "/lib64"}
	switch mode {
	case "fd":
		reads = append(reads, procFdPath(handle))
	case "name":
		// the pre-swap NAME, which is what a name-based install would carry
		reads = append(reads, member)
	}
	if err := landlock.V3.RestrictPaths(
		landlock.RODirs(reads...),
		landlock.RWDirs(scratch).WithRefer(),
	); err != nil {
		fmt.Println("CHILD RestrictPaths rejected the rules:", err)
		os.Exit(5)
	}
	// the installer's descriptor goes away, as it does before syscall.Exec
	_ = handle.Close()
	if _, err := os.ReadFile(filepath.Join(moved, "marker")); err != nil {
		fmt.Println("CHILD read after swap+close failed:", err)
		os.Exit(7)
	}
	fmt.Println("GRANT-SURVIVED")
	os.Exit(0)
}
