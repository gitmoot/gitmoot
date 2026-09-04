//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
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
	return root
}

// TestSandboxExecGrantsThePinnedToolchainE2E is #1839's behavioral guard for
// the toolchain half. It EXECUTES a sandboxed command that runs the Go
// toolchain resolved from PATH, exactly as a read-only review seat does.
//
// Requirement: a review seat must be able to execute the toolchain required by
// the checked-out repository. Asserting on a grant slice cannot prove that -
// the kernel decides - so this drives the real gitmoot sandbox-exec wrapper and
// reads the marker the toolchain itself printed.
//
// The control arm is the same command with a toolchain the classifier must NOT
// grant, which is the state this box was actually in: exec denied, no marker.
func TestSandboxExecGrantsThePinnedToolchainE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)

	base := t.TempDir()
	workdir := filepath.Join(base, "worktree")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}

	// A pinned toolchain in the shape this box uses: go-named leaf, outside
	// every package root. Before the fix this was silently not granted.
	pinned := makeToolchain(t, filepath.Join(base, "pinned"), "go1.26.4")
	// A tree the classifier must refuse: not go-named, not under a package
	// root. This is the control that makes the grant observable.
	unclassified := makeToolchain(t, filepath.Join(base, "vendor"), "mytools")

	run := func(t *testing.T, toolchain string) (string, error) {
		t.Helper()
		// --read is load-bearing: with no read grant at all, sandbox-exec
		// preserves the legacy produce contract and makes the whole
		// filesystem readable, so nothing would be confined and both arms
		// would pass. A read-only review seat always carries read grants.
		cmd := exec.Command(gitmoot, "sandbox-exec",
			"--read", workdir, "--write", workdir, "--", "/bin/sh", "-c", "go")
		cmd.Dir = workdir
		cmd.Env = []string{
			"PATH=" + filepath.Join(toolchain, "bin") + ":/bin:/usr/bin",
			"HOME=" + base,
		}
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	granted, err := run(t, pinned)
	if err != nil {
		t.Fatalf("the pinned toolchain could not execute under the sandbox: %v\n%s\n"+
			"this is the #1839 defect: a review seat cannot run the toolchain its repository requires", err, granted)
	}
	if !strings.Contains(granted, "toolchain-ran") {
		t.Fatalf("pinned toolchain output = %q, want the marker: the command must actually have run", granted)
	}

	refused, err := run(t, unclassified)
	if err == nil || strings.Contains(refused, "toolchain-ran") {
		t.Fatalf("an unclassified tree executed anyway (err=%v, output=%q): the grant is not what made the "+
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
