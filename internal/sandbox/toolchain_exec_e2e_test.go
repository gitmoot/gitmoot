//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makeToolchain builds a GOROOT-shaped tree whose bin/go is a real executable
// that prints a marker, so a test can tell EXECUTED from DENIED rather than
// inferring it from an exit code alone.
func makeToolchain(t *testing.T, parent, leaf string) string {
	t.Helper()
	root := filepath.Join(parent, leaf)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf toolchain-ran\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestSandboxExecGrantsThePinnedToolchainE2E is #1839's behavioral guard for
// the toolchain half, and it runs THE REAL PINNED TOOLCHAIN.
//
// An earlier version of this test used a fixture GOROOT with a shell script
// for bin/go. That was wrong twice over. It could only demonstrate generic
// Landlock read/exec behavior rather than the review toolchain path, and once
// the grant gained a non-user-controlled anchor a fixture under TempDir became
// unreachable from a SUBPROCESS anyway - the test's own package variables do
// not exist in the gitmoot binary being exec'd. So the granted arm is the
// actual toolchain the daemon was built against, running `go version`, which
// cannot succeed unless its GOROOT is genuinely readable.
//
// The refused arm is a COMPLETE planted toolchain - executable bin/go,
// src/runtime, parseable VERSION - somewhere writable and not a home. It must
// stay denied, and it is what makes the granted arm evidence of the grant
// rather than of the sandbox being open.
func TestSandboxExecGrantsThePinnedToolchainE2E(t *testing.T) {
	requireLandlockABI(t)
	pinnedRoot := runtime.GOROOT()
	pinnedGo := filepath.Join(pinnedRoot, "bin", "go")
	if _, err := os.Stat(pinnedGo); err != nil {
		t.Skipf("no build toolchain on disk at %s", pinnedGo)
	}
	gitmoot := buildGitmootBinary(t)

	base := t.TempDir()
	workdir := filepath.Join(base, "worktree")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The planted control tree must live outside every IMPLICITLY granted
	// root. writableRoots appends os.TempDir() AND "/tmp" whenever implicit
	// roots are on (exec_linux.go:248), so a t.TempDir() fixture is writable
	// inside the sandbox by design - which made this control arm pass or fail
	// with TMPDIR rather than with the toolchain rule. MEASURED: green under
	// TMPDIR=/root/... and /var/tmp, red under /tmp, and red in CI for exactly
	// that reason. So the fixture is created beside the package, which is the
	// precedent the existing kernel E2E in this package already uses.
	plantedBase, err := os.MkdirTemp(".", ".gitmoot-1839-planted-*")
	if err != nil {
		t.Fatal(err)
	}
	if plantedBase, err = filepath.Abs(plantedBase); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(plantedBase) })
	planted := makeToolchain(t, plantedBase, "go1.26.4")

	// --read is load-bearing: with no read grant at all, sandbox-exec
	// preserves the legacy produce contract and makes the whole filesystem
	// readable, so nothing would be confined and BOTH arms would pass. That
	// defect was caught by this test's control arm.
	run := func(t *testing.T, toolchain string, args ...string) (string, error) {
		t.Helper()
		argv := append([]string{"sandbox-exec", "--read", workdir, "--write", workdir, "--"}, args...)
		cmd := exec.Command(gitmoot, argv...)
		cmd.Dir = workdir
		cmd.Env = []string{
			"PATH=" + filepath.Join(toolchain, "bin") + ":/bin:/usr/bin",
			"HOME=" + base,
			"GOTOOLCHAIN=local",
			"GOCACHE=" + filepath.Join(base, "gocache"),
			"TMPDIR=" + base,
		}
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	// Both arms exec /bin/sh and let the SHELL resolve `go` from PATH, which
	// is how a seat reaches it. Naming the toolchain binary directly as the
	// command would prove nothing: sandbox-exec must grant read for whatever
	// executable it is asked to run, so a planted binary invoked by absolute
	// path executes regardless of the toolchain rule - measured, and it is
	// what made this control arm pass while the rule was working correctly.
	//
	// `go version` reads files inside GOROOT, so it cannot pass on exec
	// rights alone.
	runOnlyPath := func(t *testing.T, toolchain string, args ...string) (string, error) {
		t.Helper()
		argv := append([]string{"sandbox-exec", "--read", workdir, "--write", workdir, "--"}, args...)
		cmd := exec.Command(gitmoot, argv...)
		cmd.Dir = workdir
		cmd.Env = []string{"PATH=" + filepath.Join(toolchain, "bin"), "HOME=" + base, "GOTOOLCHAIN=local"}
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	granted, err := run(t, pinnedRoot, "/bin/sh", "-c", "go version")
	if err != nil {
		t.Fatalf("the pinned toolchain %s could not run `go version` under the sandbox: %v\n%s\n"+
			"this is the #1839 defect: a review seat cannot run the toolchain its repository requires", pinnedGo, err, granted)
	}
	if !strings.Contains(granted, "go version") {
		t.Fatalf("pinned toolchain output = %q, want a version line: the command must actually have run", granted)
	}

	// A planted tree is refused however complete its shape, so the arm above
	// is evidence about the GRANT.
	// PATH holds ONLY the planted toolchain, so nothing can fall through to a
	// system go and make a refusal read as a success.
	refused, err := runOnlyPath(t, planted, "/bin/sh", "-c", "go && printf ran")
	if err == nil || strings.Contains(refused, "toolchain-ran") || strings.Contains(refused, "ran") {
		t.Fatalf("a planted unanchored toolchain executed anyway (err=%v, output=%q): the grant is not what made the "+
			"pinned arm pass, so this test would not notice the grant being dropped again", err, refused)
	}
}

// TestReadableRootsGrantsTheToolchainRootNotItsParent pins the WIDTH of the
// grant, which the E2E above cannot see.
//
// Measured: a mutant granting filepath.Dir(root) instead of root SURVIVED the
// E2E, because over-granting still lets the pinned arm execute and still
// leaves the unclassified arm ungranted. Fixing an under-grant must not be
// allowed to drift into an over-grant, so the width is asserted here directly.
func TestReadableRootsGrantsTheToolchainRootNotItsParent(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "toolchains")
	root := makeToolchain(t, parent, "go1.26.4")
	t.Setenv("PATH", filepath.Join(root, "bin"))

	oldOwn := daemonBuildGoroot
	daemonBuildGoroot = func() string { return root }
	t.Cleanup(func() { daemonBuildGoroot = oldOwn })

	roots, err := readableRoots([]string{base}, "/bin/sh")
	if err != nil {
		t.Fatalf("readableRoots: %v", err)
	}
	var haveRoot, haveParent bool
	for _, granted := range roots {
		switch granted {
		case root:
			haveRoot = true
		case parent:
			haveParent = true
		}
	}
	if !haveRoot {
		t.Errorf("roots = %v, want the GOROOT %q", roots, root)
	}
	if haveParent {
		t.Errorf("roots = %v include the toolchain PARENT %q: the grant is wider than the toolchain it exists for", roots, parent)
	}
}
