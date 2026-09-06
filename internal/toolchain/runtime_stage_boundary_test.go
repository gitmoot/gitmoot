package toolchain

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stagedPackageFixture builds a node-package boundary and returns its root and
// entrypoint. Every boundary test below starts from the same shape, so a
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

func stagedMembers(t *testing.T, root string) map[string]bool {
	t.Helper()
	members := map[string]bool{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		members[rel] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return members
}

// TestStageRuntimeSkipsAPrivateAncestorsReadableChild is bridge finding F1.
//
// The filter I first shipped tested only a member's OWN permission bits, so a
// 0700 credentials/ directory holding a mode-0644 token was copied: the leaf
// looked public because its private DIRECTORY was never consulted. Privacy is a
// property of the ancestor chain, not of the leaf.
func TestStageRuntimeSkipsAPrivateAncestorsReadableChild(t *testing.T) {
	home := t.TempDir()
	pkg, entrypoint := stagedPackageFixture(t)

	// The exact shape of the finding: a private directory whose child is
	// world-readable, so the child alone cannot reveal that it is a secret.
	private := filepath.Join(pkg, "credentials")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "token.json"), []byte(`{"access_token":"seat-must-not-read-this"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// CONTROL, in the same fixture so the two cannot differ by accident: a
	// PUBLIC directory with an identically-permissioned payload must be copied.
	public := filepath.Join(pkg, "lib")
	if err := os.MkdirAll(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(public, "payload.js"), []byte("module.exports=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged, err := StageRuntime(home, "tool", entrypoint)
	if err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}
	root, err := StagedRuntimeRoot(home, staged)
	if err != nil {
		t.Fatal(err)
	}
	members := stagedMembers(t, root)
	if members[filepath.Join("credentials", "token.json")] {
		t.Error("a mode-0644 child of a 0700 directory was copied; privacy must be decided by the ancestor chain, not the leaf")
	}
	if members["credentials"] {
		t.Error("the private directory itself was reproduced in the staged tree")
	}
	if !members[filepath.Join("lib", "payload.js")] {
		t.Error("a mode-0644 child of a 0755 directory was NOT copied, so the rule is refusing everything rather than refusing privacy")
	}
	if output, runErr := exec.Command(staged).CombinedOutput(); runErr != nil || strings.TrimSpace(string(output)) != "tool-ran" {
		t.Fatalf("staged runtime does not run: err=%v output=%q", runErr, output)
	}
}

// TestStageRuntimeDoesNotFollowAnInRootSymlinkToAPrivateSibling is bridge
// finding F2.
//
// os.Root refuses escapes OUT of the root; it does NOT refuse redirection
// WITHIN it. The copier decided from readdir metadata and then opened by NAME,
// so a link named like a payload delivered a private sibling's bytes into the
// staged tree. The fix decides from the open descriptor, with O_NOFOLLOW as the
// authoritative check.
//
// THE FIXTURE IS A COMPOSITE ON PURPOSE, and my first version was not. It
// pointed the link at a mode-0600 file, so the LEAF-PRIVACY rule skipped it and
// the test passed with the symlink rule deleted - measured by mutation. The
// target is now a mode-0644 file under a 0700 directory: leaf privacy says
// "public", the ancestor rule never sees it because the walked name's own parent
// is public, and only refusing to FOLLOW the link keeps those bytes out.
func TestStageRuntimeDoesNotFollowAnInRootSymlinkToAPrivateSibling(t *testing.T) {
	home := t.TempDir()
	pkg, entrypoint := stagedPackageFixture(t)

	secretBytes := []byte("api_key = \"seat-must-not-read-this\"\n")
	private := filepath.Join(pkg, "credentials")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "token.json"), secretBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	// A link that LOOKS like ordinary package payload and points, inside the
	// same root, at a readable file whose PRIVACY comes only from its directory.
	if err := os.Symlink(filepath.Join("credentials", "token.json"), filepath.Join(pkg, "payload.json")); err != nil {
		t.Fatal(err)
	}

	staged, err := StageRuntime(home, "tool", entrypoint)
	if err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}
	root, err := StagedRuntimeRoot(home, staged)
	if err != nil {
		t.Fatal(err)
	}
	if members := stagedMembers(t, root); members["payload.json"] {
		t.Error("an in-root symlink was reproduced or followed into the staged tree")
	}
	// The bytes are what matter, not the name: assert no staged file carries the
	// secret, whatever it is called.
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return walkErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(content), "seat-must-not-read-this") {
			t.Errorf("staged file %q carries the private sibling's bytes", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// CONTROL: a REAL regular member of the same name is copied, so the rule
	// refuses links rather than refusing that filename.
	controlHome := t.TempDir()
	controlPkg, controlEntry := stagedPackageFixture(t)
	if err := os.WriteFile(filepath.Join(controlPkg, "payload.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	controlStaged, err := StageRuntime(controlHome, "tool", controlEntry)
	if err != nil {
		t.Fatal(err)
	}
	controlRoot, err := StagedRuntimeRoot(controlHome, controlStaged)
	if err != nil {
		t.Fatal(err)
	}
	if !stagedMembers(t, controlRoot)["payload.json"] {
		t.Error("a regular payload.json was not copied, so the F2 assertion above passes for the wrong reason")
	}
}

// TestStageRuntimeRefusesReuseWhenPublishedContentChanged is bridge finding F3.
//
// The name is content-addressed from a FIRST read while the copy comes from a
// SECOND, so the published name could describe a tree that was never published,
// and reuse only checked that a file and a shim existed. Reuse now re-proves the
// entrypoint's CONTENT.
func TestStageRuntimeRefusesReuseWhenPublishedContentChanged(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nprintf 'v1\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged, err := StageRuntime(home, "tool", source)
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL FIRST: an untouched published tree is reused, returning the same
	// path. Without this, the refusal below could mean "never reuses anything".
	reused, err := StageRuntime(home, "tool", source)
	if err != nil {
		t.Fatalf("an untouched published tree was not reused: %v", err)
	}
	if reused != staged {
		t.Fatalf("reuse returned %q, want %q", reused, staged)
	}

	root, err := StagedRuntimeRoot(home, staged)
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(root, "tool")
	if err := os.Chmod(entrypoint, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\nprintf 'substituted\\n'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StageRuntime(home, "tool", source); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("reuse of a published tree whose entrypoint bytes changed returned %v, want ErrRuntimeNotStageable", err)
	}
}

// TestRuntimeInterpreterReportsAnUnresolvableInterpreterAsRequired is bridge
// finding F4 at the unit boundary.
//
// A shebang naming an interpreter that cannot be resolved must be reported as
// REQUIRED-but-unstageable, not as "no interpreter needed". The latter leaves a
// runnable shim whose script can only fall back to the host or fail opaquely.
func TestRuntimeInterpreterReportsAnUnresolvableInterpreterAsRequired(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tool")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env definitely-not-installed-anywhere\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	name, target, required := RuntimeInterpreter(script)
	if !required {
		t.Fatal("an unresolvable interpreter was reported as not required; the runtime would stage as a script that cannot run")
	}
	if target != "" {
		t.Errorf("target = %q, want empty so StageRuntime refuses", target)
	}
	if name != "definitely-not-installed-anywhere" {
		t.Errorf("name = %q, want the declared interpreter so the diagnostic can name it", name)
	}
	// Staging that empty target must refuse rather than succeed quietly.
	if _, err := StageRuntime(t.TempDir(), name, target); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("StageRuntime with an empty interpreter path returned %v, want ErrRuntimeNotStageable", err)
	}

	// CONTROL: a resolvable non-system interpreter is still reported required
	// WITH a path, so this test cannot pass by calling everything unresolvable.
	interpreterDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(interpreterDir, "probenode"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolvableScript := filepath.Join(dir, "tool2")
	if err := os.WriteFile(resolvableScript, []byte("#!/usr/bin/env probenode\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{dir, interpreterDir}, string(os.PathListSeparator)))
	name, target, required = RuntimeInterpreter(resolvableScript)
	if !required || target == "" || name != "probenode" {
		t.Fatalf("resolvable interpreter reported name=%q target=%q required=%v", name, target, required)
	}
}
