package toolchain

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStageRuntimeStagesASelfContainedExecutableWithoutItsProfile is the kimi
// shape, which is what made #1918's availability fix a security finding: a
// self-contained executable alone in ~/.kimi-code/bin, whose parent also holds
// credentials/, oauth/ and a secret-bearing config.toml.
//
// The staged tree must contain the EXECUTABLE and NOTHING ELSE from that profile.
// Granting the profile is what the three failed guard attempts did; copying only
// the artifact removes the outside root entirely, so there is nothing left to
// contain and no later descendant to appear.
func TestStageRuntimeStagesASelfContainedExecutableWithoutItsProfile(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(t.TempDir(), ".kimi-code")
	profileBin := filepath.Join(profile, "bin")
	credentials := filepath.Join(profile, "credentials")
	for _, dir := range []string{profileBin, credentials} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(profileBin, "kimi")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'kimi-ran\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Two secrets the old rule leaked: one whose NAME was on the denylist and one
	// whose name never was. Neither may appear in the staged copy.
	if err := os.WriteFile(filepath.Join(credentials, "kimi-code.json"), []byte(`{"access_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "config.toml"), []byte("api_key = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	staged, _, err := StageRuntime(home, "kimi", executable)
	if err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}
	if !strings.HasPrefix(staged, RuntimeRoot(home)) {
		t.Fatalf("staged executable %q is outside the daemon-owned root %q", staged, RuntimeRoot(home))
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged executable is not present: %v", err)
	}

	// AVAILABILITY: the copy must RUN, or this trades a credential leak for a
	// launch failure - the trade the ruling explicitly refuses.
	output, err := exec.Command(staged).CombinedOutput()
	if err != nil {
		t.Fatalf("the staged copy does not run: %v\noutput=%s", err, output)
	}
	if strings.TrimSpace(string(output)) != "kimi-ran" {
		t.Fatalf("staged copy output = %q, want %q", strings.TrimSpace(string(output)), "kimi-ran")
	}

	// CONTAINMENT: walk the whole published tree and prove no profile member came
	// with it. The generated .bin/kimi symlink is engine metadata, so count only
	// regular files copied from the source.
	published, err := StagedRuntimeRoot(home, staged)
	if err != nil {
		t.Fatal(err)
	}
	var copied []string
	if err := filepath.WalkDir(published, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// The generated .bin shim and the recorded entrypoint digest are ENGINE
		// metadata, not copied source, so they are excluded exactly as the shim
		// already was. Anything else under here came from the operator's tree.
		relative, relErr := filepath.Rel(published, path)
		if relErr != nil {
			return relErr
		}
		engineMetadata := relative == entrypointDigestName || strings.HasPrefix(relative, launcherDirName+string(filepath.Separator))
		if entry.Type().IsRegular() && !engineMetadata {
			copied = append(copied, filepath.Base(path))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || copied[0] != "kimi" {
		t.Fatalf("staged tree holds regular files %v, want exactly [kimi]; any other member is operator profile material the seat must not read", copied)
	}
}

// TestStageRuntimeStagesANodePackageBoundary is the codex shape: the executable
// is <node>/lib/node_modules/@openai/codex/bin/codex.js and cannot run without
// the files beside its bin dir, so the boundary is the PACKAGE ROOT.
//
// This is the direction a fix that merely stopped copying would break, which is
// why it is asserted beside the containment case rather than after it.
func TestStageRuntimeStagesANodePackageBoundary(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	pkg := filepath.Join(base, "lib", "node_modules", "@openai", "codex")
	pkgBin := filepath.Join(pkg, "bin")
	if err := os.MkdirAll(pkgBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@openai/codex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "dist.js"), []byte("module.exports=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(pkgBin, "codex.js")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'codex-ran\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	staged, _, err := StageRuntime(home, "codex", executable)
	if err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}
	if filepath.Base(staged) != "codex" || filepath.Base(filepath.Dir(staged)) != ".bin" {
		t.Fatalf("staged shim %q does not preserve the configured runtime name in a fingerprint-local .bin directory", staged)
	}
	packageRoot, err := StagedRuntimeRoot(home, staged)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"package.json", "dist.js"} {
		if _, err := os.Stat(filepath.Join(packageRoot, member)); err != nil {
			t.Errorf("package member %q is missing from the staged tree, so a node-packaged runtime cannot run: %v", member, err)
		}
	}
	output, err := exec.Command(staged).CombinedOutput()
	if err != nil {
		t.Fatalf("the staged package copy does not run: %v\noutput=%s", err, output)
	}
	if strings.TrimSpace(string(output)) != "codex-ran" {
		t.Fatalf("staged copy output = %q, want %q", strings.TrimSpace(string(output)), "codex-ran")
	}
}

// TestStageRuntimeRepublishesWhenTheSourceChanges proves identity is by CONTENT.
// A runtime upgrade must publish a new tree rather than silently reuse the
// previous one, which is the failure mode a name-keyed cache would have.
func TestStageRuntimeRepublishesWhenTheSourceChanges(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	executable := filepath.Join(dir, "claude")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'v1\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, _, err := StageRuntime(home, "claude", executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'v2\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, _, err := StageRuntime(home, "claude", executable)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("an upgraded runtime republished to the same path, so the seat would run the previous version")
	}
	output, err := exec.Command(second).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(output)) != "v2" {
		t.Fatalf("staged copy ran %q, want the upgraded v2", strings.TrimSpace(string(output)))
	}
}

// TestStageRuntimeStagesTheResolvedTargetOfASymlinkedPathEntry is the claude
// shape on this host: /root/.local/bin/claude is a symlink into
// /root/.local/share/claude/versions/<v>, so copying the LINK stages nothing
// runnable. The resolved target is what must be copied.
func TestStageRuntimeStagesTheResolvedTargetOfASymlinkedPathEntry(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	versions := filepath.Join(base, "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(versions, "2.1.252")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nprintf 'claude-ran\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "claude")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	staged, _, err := StageRuntime(home, "claude", link)
	if err != nil {
		t.Fatalf("StageRuntime on a symlinked PATH entry: %v", err)
	}
	output, err := exec.Command(staged).CombinedOutput()
	if err != nil {
		t.Fatalf("the staged copy of a symlinked entry does not run: %v\noutput=%s", err, output)
	}
	if strings.TrimSpace(string(output)) != "claude-ran" {
		t.Fatalf("staged copy output = %q, want %q", strings.TrimSpace(string(output)), "claude-ran")
	}
}

func TestStageUnavailableRuntimeFailsExplicitly(t *testing.T) {
	home := t.TempDir()
	command, err := StageUnavailableRuntime(home, "kimi")
	if err != nil {
		t.Fatal(err)
	}
	reused, err := StageUnavailableRuntime(home, "kimi")
	if err != nil {
		t.Fatalf("reusing unavailable command: %v", err)
	}
	if reused != command {
		t.Fatalf("reused unavailable command = %q, want %q", reused, command)
	}
	output, runErr := exec.Command(command).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 126 {
		t.Fatalf("unavailable command error = %v, output = %q; want exit 126", runErr, output)
	}
	if !strings.Contains(string(output), "runtime unavailable") {
		t.Fatalf("unavailable command output = %q, want explicit availability error", output)
	}
	if _, err := StagedRuntimeRoot(home, command); err != nil {
		t.Fatalf("unavailable command is not under the engine runtime root: %v", err)
	}
}

func TestStageRuntimeRefusesCorruptedPublishedCopy(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged, _, err := StageRuntime(home, "claude", source)
	if err != nil {
		t.Fatal(err)
	}
	published, err := StagedRuntimeRoot(home, staged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(published, "claude")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StageRuntime(home, "claude", source); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("restaging a corrupted published copy returned %v, want ErrRuntimeNotStageable", err)
	}
}

// TestStageRuntimeReusesAPublishedBinaryRuntime is #1974, and the shape is the
// whole defect: writeLauncher publishes a BINARY runtime's launcher as a
// relative symlink and a SCRIPT runtime's as a generated file, and only the
// second shape was ever revalidated.
//
// The first staging publishes without validating, so it passed. Every REUSE ran
// publishedMembersDigest over the published tree, whose traversal refuses a
// symlink at any component, so it returned ELOOP and stageSeatRuntimes
// published the runtime as the exit-126 unavailable shim - permanently, because
// the published name is fingerprint-derived and the same root is chosen every
// time. Measured on this host as claude and kimi unusable for every read-only
// seat while codex, a node script, kept working.
//
// Every other reuse test in this file uses a "#!/bin/sh" source, which is why
// the reuse path was covered only for the launcher shape that happens to work.
func TestStageRuntimeReusesAPublishedBinaryRuntime(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "claude")
	// No shebang: RuntimeInterpreter reports ok=false, so the runtime stages as
	// a binary and its launcher is a symlink rather than an exec script.
	if err := os.WriteFile(source, []byte("\x7fELF\x02\x01\x01 not a script\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, _, err := StageRuntime(home, "claude", source)
	if err != nil {
		t.Fatalf("first StageRuntime: %v", err)
	}
	// PIN THE MECHANISM. If the launcher ever stops being a symlink this test
	// still passes while covering nothing, so it must say so out loud.
	info, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("launcher %q has mode %s, want a symlink: this regression covers the symlink-launcher reuse path and a regular launcher no longer exercises it", first, info.Mode())
	}
	second, _, err := StageRuntime(home, "claude", source)
	if err != nil {
		t.Fatalf("reusing the published binary runtime: %v", err)
	}
	if second != first {
		t.Fatalf("reuse published a different launcher %q, want the existing %q", second, first)
	}
}

// TestStageRuntimeDigestCoversANestedLauncherDirectory bounds the exclusion the
// fix above introduces. Only the TOP-LEVEL launcher directory is engine
// metadata; a ".bin" directory that came from the source package is payload and
// must still be hashed, or a member could be swapped without changing the
// digest. A nested node_modules/.bin is the ordinary shape of the packaged
// runtime this engine stages, so the widened exclusion is a live hazard rather
// than a hypothetical one.
func TestStageRuntimeDigestCoversANestedLauncherDirectory(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	pkg := filepath.Join(base, "lib", "node_modules", "@openai", "codex")
	nested := filepath.Join(pkg, "node_modules", launcherDirName)
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@openai/codex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "helper"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(pkg, "bin", "codex.js")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged, _, err := StageRuntime(home, "codex", source)
	if err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}
	published, err := StagedRuntimeRoot(home, staged)
	if err != nil {
		t.Fatal(err)
	}
	publishedHelper := filepath.Join(published, "node_modules", launcherDirName, "helper")
	if _, err := os.Stat(publishedHelper); err != nil {
		t.Fatalf("a nested %s member was not staged, so the packaged runtime is incomplete: %v", launcherDirName, err)
	}
	if _, _, err := StageRuntime(home, "codex", source); err != nil {
		t.Fatalf("reusing a package that contains a nested %s directory: %v", launcherDirName, err)
	}
	// The engine tree is published read-only, so the tamper needs the directory
	// writable first. This is the operator-corruption case, not a seat one.
	if err := os.Chmod(filepath.Dir(publishedHelper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(publishedHelper); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StageRuntime(home, "codex", source); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("removing a nested %s member left the published digest valid (err = %v); the launcher exclusion must apply to the top level only", launcherDirName, err)
	}
}

func TestStagedRuntimeRootRefusesAnotherHome(t *testing.T) {
	firstHome := t.TempDir()
	command, err := StageUnavailableRuntime(firstHome, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StagedRuntimeRoot(t.TempDir(), command); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("StagedRuntimeRoot accepted another home's command: %v", err)
	}
}
