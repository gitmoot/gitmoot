package toolchain

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stagedPackageFixture builds a clean node-package boundary and returns its root
// and entrypoint. Every boundary test starts from the same shape, so a
// difference in outcome is attributable to the member under test.
func stagedPackageFixture(t *testing.T) (pkg string, entrypoint string) {
	t.Helper()
	pkg = filepath.Join(t.TempDir(), "node_modules", "@vendor", "runtime")
	pkgBin := filepath.Join(pkg, "bin")
	if err := os.MkdirAll(pkgBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@vendor/runtime"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entrypoint = filepath.Join(pkgBin, "tool")
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\nprintf 'tool-ran\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return pkg, entrypoint
}

// stageCleanFixtureRuns is the PASSING CONTROL every refusal test below shares:
// a clean tree must stage and run. Without it, "refused" could mean "refuses
// everything", which is the failure mode a security guard slides into.
func stageCleanFixtureRuns(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	_, entrypoint := stagedPackageFixture(t)
	launcher, _, err := StageRuntime(home, "tool", entrypoint)
	if err != nil {
		t.Fatalf("a clean package boundary was refused: %v", err)
	}
	output, runErr := exec.Command(launcher).CombinedOutput()
	if runErr != nil || strings.TrimSpace(string(output)) != "tool-ran" {
		t.Fatalf("clean staged runtime did not run: err=%v output=%q", runErr, output)
	}
}

// TestStageRefusesATreeWithAPrivateAncestor is bridge F1 under the accepted
// boundary (plan 122812, directive 122816): an ambiguous tree is REFUSED WHOLE
// rather than filtered.
//
// The measured defect was a 0700 credentials/ directory holding a mode-0644
// token being copied, because only the leaf's own bits were consulted. Filtering
// the subtree was the first fix and the panel showed why it is not enough: mode
// bits cannot classify secrets at all, so the safe answer is to refuse a runtime
// whose package carries operator-private state and publish it unavailable.
func TestStageRefusesATreeWithAPrivateAncestor(t *testing.T) {
	home := t.TempDir()
	pkg, entrypoint := stagedPackageFixture(t)
	private := filepath.Join(pkg, "credentials")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "token.json"), []byte(`{"access_token":"seat-must-not-read-this"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	launcher, _, err := StageRuntime(home, "tool", entrypoint)
	if !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("a tree containing a 0700 directory staged anyway: launcher=%q err=%v", launcher, err)
	}
	if !strings.Contains(err.Error(), "world-traversable") {
		t.Errorf("refusal does not name the private ancestor: %v", err)
	}
	assertNoStagedBytes(t, home, "seat-must-not-read-this")
	stageCleanFixtureRuns(t)
}

// TestStageRefusesANonWorldReadableMember is the same rule at the leaf.
func TestStageRefusesANonWorldReadableMember(t *testing.T) {
	home := t.TempDir()
	pkg, entrypoint := stagedPackageFixture(t)
	if err := os.WriteFile(filepath.Join(pkg, "config.toml"), []byte("api_key = \"seat-must-not-read-this\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := StageRuntime(home, "tool", entrypoint); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("a tree containing a mode-0600 member staged anyway: %v", err)
	}
	assertNoStagedBytes(t, home, "seat-must-not-read-this")
	stageCleanFixtureRuns(t)
}

// TestStageRefusesASymlinkedMember covers the static half of class 1.
func TestStageRefusesASymlinkedMember(t *testing.T) {
	home := t.TempDir()
	pkg, entrypoint := stagedPackageFixture(t)
	if err := os.WriteFile(filepath.Join(pkg, "secret.toml"), []byte("api_key = \"seat-must-not-read-this\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secret.toml", filepath.Join(pkg, "payload.json")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := StageRuntime(home, "tool", entrypoint); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("a tree containing an in-root symlink staged anyway: %v", err)
	}
	assertNoStagedBytes(t, home, "seat-must-not-read-this")
	stageCleanFixtureRuns(t)
}

// TestStageRefusesASymlinkSwappedBetweenEnumerationAndOpen is class 1's RACE,
// made DETERMINISTIC as directive 122816 requires.
//
// A high-iteration race loop was refused as proof, and rightly: it passes on a
// slow machine for the wrong reason and proves only the author's patience. The
// swapBarrier seam fires in exactly the window the attack needs - after the name
// is enumerated, before it is opened - so the substitution happens every time.
//
// THE ASSERTION IS ON BYTES, NOT NAMES: the private content must not appear
// anywhere under the engine root, whatever a staged file is called. os.Root with
// O_NOFOLLOW followed such a link (measured), and a panel lens landed exactly
// this race against the previous implementation at attempt 54; openat2 with
// RESOLVE_NO_SYMLINKS refuses it in the kernel, in the same syscall as the open.
func TestStageRefusesASymlinkSwappedBetweenEnumerationAndOpen(t *testing.T) {
	home := t.TempDir()
	pkg, entrypoint := stagedPackageFixture(t)
	const secret = "seat-must-not-read-this"
	if err := os.WriteFile(filepath.Join(pkg, "secret.toml"), []byte("api_key = \""+secret+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(pkg, "payload.json")
	if err := os.WriteFile(payload, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	swapped := false
	swapBarrier = func(relative string) {
		if relative != "payload.json" || swapped {
			return
		}
		swapped = true
		if err := os.Remove(payload); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("secret.toml", payload); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { swapBarrier = nil })

	_, _, err := StageRuntime(home, "tool", entrypoint)
	if !swapped {
		t.Fatal("the barrier never fired, so this test did not exercise the substitution window")
	}
	if !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("staging accepted a member replaced by a symlink mid-walk: %v", err)
	}
	assertNoStagedBytes(t, home, secret)

	// CONTROL: the same barrier replacing the member with a REGULAR file of
	// different content is accepted, so the refusal is about symlinks and not
	// about "anything changed".
	controlHome := t.TempDir()
	controlPkg, controlEntry := stagedPackageFixture(t)
	controlPayload := filepath.Join(controlPkg, "payload.json")
	if err := os.WriteFile(controlPayload, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replaced := false
	swapBarrier = func(relative string) {
		if relative != "payload.json" || replaced {
			return
		}
		replaced = true
		if err := os.WriteFile(controlPayload, []byte("{\"replaced\":true}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	launcher, _, controlErr := StageRuntime(controlHome, "tool", controlEntry)
	if !replaced {
		t.Fatal("the control barrier never fired")
	}
	if controlErr != nil {
		t.Fatalf("a regular-file replacement was refused, so the symlink assertion above proves nothing: %v", controlErr)
	}
	if output, runErr := exec.Command(launcher).CombinedOutput(); runErr != nil {
		t.Fatalf("control staged runtime did not run: %v: %s", runErr, output)
	}
}

// TestStageRefusesReuseWhenPublishedContentChanged is class 3 plus bridge F3.
func TestStageRefusesReuseWhenPublishedContentChanged(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nprintf 'v1\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launcher, _, err := StageRuntime(home, "tool", source)
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL FIRST: an untouched published tree is reused and returns the same
	// launcher, so the refusal below cannot mean "never reuses anything".
	reused, _, err := StageRuntime(home, "tool", source)
	if err != nil {
		t.Fatalf("an untouched published tree was not reused: %v", err)
	}
	if reused != launcher {
		t.Fatalf("reuse returned %q, want %q", reused, launcher)
	}

	published, err := StagedRuntimeRoot(home, launcher)
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(published, "tool")
	if err := os.Chmod(entrypoint, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\nprintf 'substituted\\n'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StageRuntime(home, "tool", source); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("reuse of a published tree whose bytes changed returned %v, want ErrRuntimeNotStageable", err)
	}
}

// assertNoStagedBytes fails if any file under the engine root contains needle.
// It asserts on CONTENT because a refusal that merely renames the leak is not a
// refusal.
func assertNoStagedBytes(t *testing.T, home string, needle string) {
	t.Helper()
	root := RuntimeRoot(home)
	if _, err := os.Stat(root); err != nil {
		return
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(content), needle) {
			t.Errorf("staged file %q carries private bytes %q", path, needle)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
