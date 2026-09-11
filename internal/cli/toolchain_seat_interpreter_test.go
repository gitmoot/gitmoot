package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/toolchain"
)

// TestStageSeatRuntimesStagesAScriptRuntimesInterpreter is the codex shape, and
// it is a hole no launch probe outside Landlock could have found.
//
// MEASURED ON THIS HOST: codex resolves to
// /root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/bin/codex.js,
// whose first line is "#!/usr/bin/env node", and that node resolves to
// /root/.nvm/versions/node/v22.23.2/bin/node - INSIDE the operator's home, the
// exact root class this change refuses to grant. Copying the script alone
// therefore produced a staged entrypoint that can only run by reaching an
// ungranted operator path. The earlier availability probe passed because it
// executed the shim in the test process, where the host node is reachable; the
// sandbox is where it would have failed.
//
// The fixture uses a private interpreter name so the assertion cannot be
// satisfied by the host's real node.
func TestStageSeatRuntimesStagesAScriptRuntimesInterpreter(t *testing.T) {
	interpreterDir := t.TempDir()
	interpreter := filepath.Join(interpreterDir, "probenode")
	// A real interpreter: it prints the script path it was handed, so the test
	// can prove the STAGED interpreter ran the STAGED script.
	if err := os.WriteFile(interpreter, []byte("#!/bin/sh\nprintf 'ran %s\\n' \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A packaged script runtime, shaped like codex: package root, bin/<name>.js
	// entrypoint, and a shebang that goes through /usr/bin/env.
	pkgBase := t.TempDir()
	pkg := filepath.Join(pkgBase, "lib", "node_modules", "@openai", "codex")
	pkgBin := filepath.Join(pkg, "bin")
	if err := os.MkdirAll(pkgBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@openai/codex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(pkgBin, "codex.js")
	if err := os.WriteFile(entrypoint, []byte("#!/usr/bin/env probenode\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launcherDir := t.TempDir()
	if err := os.Symlink(entrypoint, filepath.Join(launcherDir, "codex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{launcherDir, interpreterDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)))

	paths := config.PathsForHome(t.TempDir())
	commands, _, diagnostics, err := stageSeatRuntimes(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}

	var stagedCodex string
	for _, command := range commands {
		if filepath.Base(command) == "codex" {
			stagedCodex = command
		}
	}
	if stagedCodex == "" {
		t.Fatalf("the script runtime was not staged; commands = %v", commands)
	}
	if !pathWithin(stagedCodex, toolchain.RuntimeRoot(paths.Home)) {
		t.Errorf("staged runtime %q is outside the engine runtime root", stagedCodex)
	}

	// THE INTERPRETER IS NO LONGER A SEPARATE SEAT COMMAND, and that is the
	// point of directive 122816: the launcher EXECS the staged interpreter by
	// its staged absolute path, so nothing has to be resolvable on the seat's
	// PATH and the kernel never resolves the entrypoint's original shebang.
	//
	// So the assertion is the strongest available: launch with the host
	// interpreter directory ABSENT from PATH entirely. Under the old shape this
	// could only work if something resolved "probenode" for /usr/bin/env; now it
	// works because the launcher names the staged copy outright.
	seatPath := strings.Join([]string{filepath.Dir(stagedCodex), "/usr/bin", "/bin"}, string(os.PathListSeparator))
	command := exec.Command(stagedCodex)
	command.Env = append(os.Environ(), "PATH="+seatPath)
	output, runErr := command.CombinedOutput()
	if runErr != nil {
		t.Fatalf("the staged script did not launch with its host interpreter directory absent from PATH: %v\noutput=%s", runErr, output)
	}
	// The probe interpreter prints the script path it was handed, so this proves
	// the STAGED interpreter ran the STAGED entrypoint.
	if !strings.Contains(string(output), toolchain.RuntimeRoot(paths.Home)) {
		t.Errorf("staged script ran but not from the engine root: output=%q", output)
	}
}

// TestStageSeatRuntimesLeavesSystemInterpretersAlone is the control for the
// test above, and it exists because the first version of that fix had no
// boundary: every "#!/bin/sh" runtime staged a copy of /bin/sh as a runtime
// command, which my own count assertion caught. The sandbox grants the fixed
// system roots unconditionally, so a system interpreter needs no copy - and
// copying it would put an engine-owned shell ahead of the real one on the
// seat's PATH for no gain.
func TestStageSeatRuntimesLeavesSystemInterpretersAlone(t *testing.T) {
	launcherDir := t.TempDir()
	// A shell script whose interpreter is a SYSTEM binary.
	if err := os.WriteFile(filepath.Join(launcherDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{launcherDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)))

	paths := config.PathsForHome(t.TempDir())
	commands, _, diagnostics, err := stageSeatRuntimes(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	for _, command := range commands {
		if base := filepath.Base(command); base == "sh" || base == "dash" || base == "bash" {
			t.Errorf("a system interpreter was staged as runtime command %q; it is already inside the sandbox's fixed read set", command)
		}
	}
	// CONTROL ON THE CONTROL: the runtime itself is still staged and runnable,
	// so this cannot pass by staging nothing.
	var stagedClaude string
	for _, command := range commands {
		if filepath.Base(command) == "claude" {
			stagedClaude = command
		}
	}
	if stagedClaude == "" || !pathWithin(stagedClaude, toolchain.RuntimeRoot(paths.Home)) {
		t.Fatalf("the script runtime was not staged into the engine root: %q", stagedClaude)
	}
	if output, runErr := exec.Command(stagedClaude).CombinedOutput(); runErr != nil {
		t.Fatalf("staged script runtime does not run: %v: %s", runErr, output)
	}
}

// TestStageSeatToolchainResolvesASymlinkedSystemGo is the second half of the
// same class, in the Go arm.
//
// MEASURED: /usr/bin/go on this host is a symlink to /usr/lib/go-1.22/bin/go.
// stageSeatToolchain classified the RAW LookPath result, and "/usr/bin/go" has
// parent "bin", so toolchainInstallRoot returned "/usr" - staging would have
// tried to copy the entire system tree as if it were a Go installation.
// StageRuntime already resolved symlinks for precisely this reason; the Go arm
// did not.
func TestStageSeatToolchainResolvesASymlinkedSystemGo(t *testing.T) {
	base := t.TempDir()
	install := filepath.Join(base, "lib", "go-1.26.4")
	installBin := filepath.Join(install, "bin")
	if err := os.MkdirAll(installBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installBin, "go"), []byte("#!/bin/sh\necho go1.26.4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"VERSION": "go1.26.4\n", "go.env": "GOTOOLCHAIN=local\n"} {
		if err := os.WriteFile(filepath.Join(install, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The launcher directory reproduces /usr/bin: a "bin" directory holding a
	// symlink into the real installation.
	launcherDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(launcherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(installBin, "go"), filepath.Join(launcherDir, "go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{launcherDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)))

	paths := config.PathsForHome(t.TempDir())
	staged, env, diagnostic, err := stageSeatToolchain(paths)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic != "" {
		t.Fatalf("a symlinked installation was refused: %q", diagnostic)
	}
	// The staged copy must be a TOOLCHAIN, so bin/go exists inside it. Before
	// the fix the classified source was `base` (the symlink's grandparent), and
	// staging either failed or copied the wrong tree.
	if _, statErr := os.Stat(filepath.Join(staged, "bin", "go")); statErr != nil {
		t.Fatalf("staged root %q is not a Go installation: %v", staged, statErr)
	}
	var goroot string
	for _, entry := range env {
		if strings.HasPrefix(entry, "GOROOT=") {
			goroot = strings.TrimPrefix(entry, "GOROOT=")
		}
	}
	if goroot != staged {
		t.Errorf("GOROOT = %q, want the staged root %q", goroot, staged)
	}
	if !pathWithin(staged, toolchain.Root(paths.Home)) {
		t.Errorf("staged toolchain %q is outside the engine toolchain root", staged)
	}
}
