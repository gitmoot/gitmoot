package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/toolchain"
)

// stageScriptRuntimeFixture builds the codex shape: a packaged script runtime
// whose entrypoint is executed by a SEPARATE interpreter, so staging publishes
// two sibling roots. It returns the interpreter's own directory name so the
// assertion cannot be satisfied by the host's real node.
func stageScriptRuntimeFixture(t *testing.T, name string) {
	t.Helper()
	interpreterDir := t.TempDir()
	interpreter := filepath.Join(interpreterDir, "probenode")
	if err := os.WriteFile(interpreter, []byte("#!/bin/sh\nprintf 'ran %s\\n' \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "lib", "node_modules", "@openai", name)
	pkgBin := filepath.Join(pkg, "bin")
	if err := os.MkdirAll(pkgBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@openai/`+name+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(pkgBin, name+".js")
	if err := os.WriteFile(entrypoint, []byte("#!/usr/bin/env probenode\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launcherDir := t.TempDir()
	if err := os.Symlink(entrypoint, filepath.Join(launcherDir, name)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{launcherDir, interpreterDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)))
}

// TestSeatGrantsCoverTheInterpreterAScriptRuntimeExecs pins #2141.
//
// A staged script runtime's launcher is a two-line shell script that execs the
// STAGED interpreter from its own published root - a SIBLING of the launcher's
// root. Seat setup granted each launcher's own root and nothing else, so the
// engine handed the seat a command it had itself forbidden: every codex review
// on this host died with
//
//	.bin/codex: exec: .../runtimes/node-<fp>/.bin/node: Permission denied  (126)
//
// and produced no finding, so the failure read as a broken reviewer rather than
// as a missing grant.
//
// This drives the production entry, readOnlyRuntimeSandboxGrants, because that
// is the function that decides what the seat may execute. A test that only
// asserted stageSeatRuntimes returned an interpreter would pass while the grant
// list stayed short - the shape of guard this repo has been convicting all
// week: a population that excludes the case that motivated it.
func TestSeatGrantsCoverTheInterpreterAScriptRuntimeExecs(t *testing.T) {
	stageScriptRuntimeFixture(t, "codex")
	checkout := seatFixtureCheckout(t)
	home := t.TempDir()
	agent := runtime.Agent{Name: "gm-review-codex", Runtime: runtime.CodexRuntime, ReadOnlySeat: true}

	grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, "gitmoot/gitmoot", true)
	if err != nil {
		t.Fatalf("seat setup: %v", err)
	}

	// Locate the launcher and the interpreter root independently of the grant
	// list, so the assertion is against the filesystem rather than against the
	// value under test.
	seatPaths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatalf("resolve seat paths: %v", err)
	}
	runtimeRoot := toolchain.RuntimeRoot(seatPaths.Home)
	entries, err := os.ReadDir(runtimeRoot)
	if err != nil {
		t.Fatalf("read staged runtime root: %v", err)
	}
	var codexRoot, interpreterRoot string
	for _, entry := range entries {
		switch {
		case strings.HasPrefix(entry.Name(), "codex-"):
			codexRoot = filepath.Join(runtimeRoot, entry.Name())
		case strings.HasPrefix(entry.Name(), "probenode-"):
			interpreterRoot = filepath.Join(runtimeRoot, entry.Name())
		}
	}
	if codexRoot == "" {
		t.Fatalf("fixture did not stage a codex runtime under %s", runtimeRoot)
	}
	if interpreterRoot == "" {
		t.Fatalf("fixture did not stage the interpreter under %s; the two-root shape this test exists for was not produced", runtimeRoot)
	}
	if interpreterRoot == codexRoot {
		t.Fatal("interpreter and runtime share a root, so this fixture cannot detect the defect")
	}

	// The launcher must actually reference the interpreter root, or the grant
	// below would be gratuitous rather than required.
	launcher, err := os.ReadFile(filepath.Join(codexRoot, ".bin", "codex"))
	if err != nil {
		t.Fatalf("read staged launcher: %v", err)
	}
	if !strings.Contains(string(launcher), interpreterRoot) {
		t.Fatalf("staged launcher does not exec the staged interpreter root %s:\n%s", interpreterRoot, launcher)
	}

	granted := func(root string) bool {
		for _, read := range grants.reads {
			if read == root {
				return true
			}
		}
		return false
	}
	if !granted(codexRoot) {
		t.Errorf("the runtime's own root %s is not granted", codexRoot)
	}
	if !granted(interpreterRoot) {
		t.Fatalf("the interpreter root %s is NOT granted, so the seat cannot exec the launcher the engine staged for it.\ngranted reads: %v", interpreterRoot, grants.reads)
	}

	// The interpreter must not become PATH-resolvable: granting the exec
	// closure is not the same as offering the seat a second interpreter by
	// name, which no dispatch decision chose.
	for _, env := range grants.env {
		if strings.HasPrefix(env, "PATH=") && strings.Contains(env, interpreterRoot) {
			t.Errorf("interpreter root leaked onto the seat PATH: %s", env)
		}
	}
}

// TestSeatGrantsCoverATRANSITIVEInterpreterChain pins the P1 from #2141's
// review: an interpreter can ITSELF be a script needing a further interpreter.
//
// The first version of this fix returned only the IMMEDIATE interpreter, so a
// three-link chain granted two roots out of three and the seat still exited
// 126 on the third. The reviewer reproduced that with an executed test rather
// than arguing it. StageRuntime now returns the full transitive closure.
//
// The chain built here is runtime -> mid (a script) -> base (a real binary),
// which is the shape a packaged interpreter takes when it ships as a wrapper.
func TestSeatGrantsCoverATRANSITIVEInterpreterChain(t *testing.T) {
	baseDir := t.TempDir()
	base := filepath.Join(baseDir, "probebase")
	if err := os.WriteFile(base, []byte("#!/bin/sh\nprintf 'base ran %s\\n' \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// mid is a SCRIPT interpreter: it needs probebase to run, so staging mid
	// must in turn stage probebase.
	midDir := t.TempDir()
	mid := filepath.Join(midDir, "probemid")
	if err := os.WriteFile(mid, []byte("#!/usr/bin/env probebase\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "lib", "node_modules", "@openai", "codex")
	pkgBin := filepath.Join(pkg, "bin")
	if err := os.MkdirAll(pkgBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@openai/codex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(pkgBin, "codex.js")
	if err := os.WriteFile(entrypoint, []byte("#!/usr/bin/env probemid\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launcherDir := t.TempDir()
	if err := os.Symlink(entrypoint, filepath.Join(launcherDir, "codex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{launcherDir, midDir, baseDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)))

	home := t.TempDir()
	grants, err := readOnlyRuntimeSandboxGrants(home, runtime.Agent{
		Name: "gm-review-codex", Runtime: runtime.CodexRuntime, ReadOnlySeat: true,
	}, seatFixtureCheckout(t), "gitmoot/gitmoot", true)
	if err != nil {
		t.Fatalf("seat setup: %v", err)
	}
	seatPaths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := toolchain.RuntimeRoot(seatPaths.Home)
	entries, err := os.ReadDir(runtimeRoot)
	if err != nil {
		t.Fatalf("read staged runtime root: %v", err)
	}
	want := map[string]string{"codex-": "", "probemid-": "", "probebase-": ""}
	for _, entry := range entries {
		for prefix := range want {
			if strings.HasPrefix(entry.Name(), prefix) {
				want[prefix] = filepath.Join(runtimeRoot, entry.Name())
			}
		}
	}
	for prefix, root := range want {
		if root == "" {
			t.Fatalf("the fixture did not stage a %s* root under %s; the three-link chain this test exists for was not produced (staged: %v)",
				prefix, runtimeRoot, entries)
		}
	}
	granted := func(root string) bool {
		for _, read := range grants.reads {
			if read == root {
				return true
			}
		}
		return false
	}
	for prefix, root := range want {
		if !granted(root) {
			t.Errorf("root %s (%s) is NOT granted, so the seat cannot exec the chain the engine staged: granted reads %v", prefix, root, grants.reads)
		}
	}
}
