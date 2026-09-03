package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestReadOnlySeatStagesSymlinkedInputsAndCredentials pins the succeed path.
//
// A dotfiles manager leaves a SYMLINK where the runtime looks and keeps the
// real file in its own tree. Rejecting that made a valid kimi profile
// unlaunchable. Both staging sites had the defect - the input path AND the
// credential path - so both are pinned here; the reviewer named only the first.
func TestReadOnlySeatStagesSymlinkedInputsAndCredentials(t *testing.T) {
	home := t.TempDir()
	real := t.TempDir()

	// The real files live outside the runtime's state dir, as stow would keep them.
	realConfig := filepath.Join(real, "config.toml")
	if err := os.WriteFile(realConfig, []byte("model = \"kimi-k2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	realCredential := filepath.Join(real, "kimi-code.json")
	if err := os.WriteFile(realCredential, []byte(`{"token":"kimi-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(home, "dotfiles-kimi")
	if err := os.MkdirAll(filepath.Join(source, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realConfig, filepath.Join(source, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realCredential, filepath.Join(source, "credentials", "kimi-code.json")); err != nil {
		t.Fatal(err)
	}

	agent := runtime.Agent{Runtime: runtime.KimiRuntime, RuntimeConfigDir: source}
	stateDir, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err != nil {
		t.Fatalf("a symlinked profile must be stageable, got: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(stateDir, "config.toml"))
	if err != nil {
		t.Fatalf("read staged config: %v", err)
	}
	if !strings.Contains(string(staged), "kimi-k2") {
		t.Errorf("staged config = %q, want the symlink target's content", staged)
	}
	credential, err := os.ReadFile(filepath.Join(stateDir, "credentials", "kimi-code.json"))
	if err != nil {
		t.Fatalf("read staged credential: %v", err)
	}
	if !strings.Contains(string(credential), "kimi-token") {
		t.Errorf("staged credential = %q, want the symlink target's content", credential)
	}
	// The staged copy must be a real file in the seat, not a symlink pointing
	// back out of the sandbox at a path the seat cannot read.
	info, err := os.Lstat(filepath.Join(stateDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("staged input is itself a symlink; the seat would follow it out of the sandbox")
	}
}

// TestReadOnlySeatStillRefusesInputsThatAreNotFiles keeps the bound honest:
// following symlinks must not turn into accepting anything.
func TestReadOnlySeatStillRefusesInputsThatAreNotFiles(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".codex")
	if err := os.MkdirAll(filepath.Join(source, "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeConfigDir: source}
	_, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err == nil {
		t.Fatal("a directory named config.toml was accepted as a runtime input")
	}
	if !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("error = %v, want it to say the input is not a regular file", err)
	}

	// A symlink TO a directory is the same refusal, reached through resolution.
	linked := filepath.Join(home, ".codex-linked")
	if err := os.MkdirAll(linked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(linked, "config.toml")); err != nil {
		t.Fatal(err)
	}
	agent = runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeConfigDir: linked}
	if _, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false); err == nil {
		t.Fatal("a symlink to a directory was accepted as a runtime input")
	}
}

// TestReadOnlySeatTreatsADanglingSymlinkAsMissing pins that a broken link takes
// the missing-file path rather than a third outcome: skipped when the input is
// optional, and NAMED when it is required.
func TestReadOnlySeatTreatsADanglingSymlinkAsMissing(t *testing.T) {
	home := t.TempDir()

	// codex config.toml is OPTIONAL: a dangling link is skipped, seat launches.
	codexSource := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "gone.toml"), filepath.Join(codexSource, "config.toml")); err != nil {
		t.Fatal(err)
	}
	stateDir, _, err := prepareReadOnlyRuntimeState(runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeConfigDir: codexSource}, t.TempDir(), false)
	if err != nil {
		t.Fatalf("a dangling optional input must be skipped, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "config.toml")); !os.IsNotExist(statErr) {
		t.Errorf("a dangling optional input was staged anyway: %v", statErr)
	}

	// kimi config.toml is REQUIRED: the failure must name the file.
	kimiSource := filepath.Join(home, ".kimi-code")
	if err := os.MkdirAll(kimiSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "gone.toml"), filepath.Join(kimiSource, "config.toml")); err != nil {
		t.Fatal(err)
	}
	_, _, err = prepareReadOnlyRuntimeState(runtime.Agent{Runtime: runtime.KimiRuntime, RuntimeConfigDir: kimiSource}, t.TempDir(), false)
	if err == nil {
		t.Fatal("a dangling REQUIRED input was accepted")
	}
	if !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("error = %v, want it to name config.toml rather than leave the runtime to explain", err)
	}
}

// TestStagedInputNamesCannotEscapeTheSeatStateDir pins the containment check.
// Every real name is a separator-free constant, so this is a guard against a
// future policy edit, not a live bug - which is exactly why it needs a test
// rather than a convention.
func TestStagedInputNamesCannotEscapeTheSeatStateDir(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{"..", filepath.Join("..", "escaped.toml"), filepath.Join("a", "..", "..", "escaped.toml")} {
		if _, err := containedStagePath(stateDir, name); err == nil {
			t.Errorf("containedStagePath(%q) allowed a write outside the seat state dir", name)
		}
	}
	// The shapes in use must still be allowed, including the nested credential.
	for _, name := range []string{"config.toml", filepath.Join("credentials", "kimi-code.json")} {
		got, err := containedStagePath(stateDir, name)
		if err != nil {
			t.Errorf("containedStagePath(%q) refused a name in use: %v", name, err)
			continue
		}
		if !strings.HasPrefix(got, stateDir) {
			t.Errorf("containedStagePath(%q) = %q, want it under %q", name, got, stateDir)
		}
	}
}
