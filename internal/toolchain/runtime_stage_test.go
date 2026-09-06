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

	staged, err := StageRuntime(home, "kimi", executable)
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

	staged, err := StageRuntime(home, "codex", executable)
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
	first, err := StageRuntime(home, "claude", executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'v2\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := StageRuntime(home, "claude", executable)
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

	staged, err := StageRuntime(home, "claude", link)
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
	staged, err := StageRuntime(home, "claude", source)
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
	if _, err := StageRuntime(home, "claude", source); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("restaging a corrupted published copy returned %v, want ErrRuntimeNotStageable", err)
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
