package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxExecScriptRuntimeNeedsItsInterpreterGrantedKernelE2E closes the
// gap that let a real defect through acceptance on #1921.
//
// THE GAP: the availability probe for contract item 5 executes staged shims in
// the TEST PROCESS, where every host path is reachable. It therefore proved
// staging and launchability but could not prove the SANDBOX case, and a codex
// seat was broken while that probe stayed green - codex's entrypoint is
// "#!/usr/bin/env node" and its node lives in the operator's home, which is the
// one root this design refuses to grant. Verifying the pipe is not verifying
// the picture, so this test verifies the picture: a real Landlock domain, a
// script runtime, and its interpreter.
//
// TWO ARMS, because one alone proves nothing:
//
//   - interpreter NOT granted -> the seat cannot run the script at all. This is
//     the state 4eb38221 shipped.
//   - interpreter granted the way a staged copy is -> it runs.
//
// The arms are discriminated by the script's own output, not by an exit code,
// because an exit code cannot distinguish "interpreter unreachable" from "the
// script ran and failed".
func TestSandboxExecScriptRuntimeNeedsItsInterpreterGrantedKernelE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)

	// NOT t.TempDir(): writableRoots grants os.TempDir() and /tmp implicitly, so
	// a fixture under /tmp would be readable whatever the rules say and BOTH
	// arms would pass. Same reasoning as the sibling profile-grant E2Es.
	base, err := os.MkdirTemp(".", ".gitmoot-script-interp-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}

	// The "operator" side: an interpreter outside any engine-owned tree, exactly
	// where node sits on this host.
	operatorBin := filepath.Join(base, "operator-profile", "bin")
	// TWO SEPARATE STAGED ROOTS, and the separation is the whole point. My first
	// version wrote the interpreter into BOTH directories, so arm A's env found
	// the staged copy and the arm passed while measuring nothing - a fixture
	// that cannot fail. Arm A's root holds the SCRIPT ONLY.
	unstagedRoot := filepath.Join(base, "engine-staged-script-only")
	unstagedBin := filepath.Join(unstagedRoot, ".bin")
	stagedRoot := filepath.Join(base, "engine-staged-with-interpreter")
	stagedBin := filepath.Join(stagedRoot, ".bin")
	workdir := filepath.Join(base, "seat-worktree")
	for _, dir := range []string{operatorBin, unstagedBin, stagedBin, workdir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	interpreter := []byte("#!/bin/sh\nprintf 'SCRIPT_RAN\\n'\n")
	if err := os.WriteFile(filepath.Join(operatorBin, "probenode"), interpreter, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the arm-B root gets a staged interpreter beside the script.
	if err := os.WriteFile(filepath.Join(stagedBin, "probenode"), interpreter, 0o755); err != nil {
		t.Fatal(err)
	}
	// The runtime entrypoint is a SCRIPT whose shebang goes through
	// /usr/bin/env, which is the codex shape.
	script := []byte("#!/usr/bin/env probenode\n")
	for _, dir := range []string{unstagedBin, stagedBin} {
		if err := os.WriteFile(filepath.Join(dir, "coderuntime"), script, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	run := func(t *testing.T, readRoots []string, pathEntries []string) (string, error) {
		t.Helper()
		args := []string{"sandbox-exec", "--read-only-workdir"}
		for _, root := range append([]string{workdir}, readRoots...) {
			args = append(args, "--read", root)
		}
		args = append(args, "--write", workdir, "--", "coderuntime")
		command := exec.Command(gitmoot, args...)
		command.Dir = workdir
		command.Env = []string{
			"PATH=" + strings.Join(pathEntries, string(os.PathListSeparator)),
			"HOME=" + workdir,
		}
		output, err := command.CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}

	// ARM A: only the script's own directory is granted, and the interpreter is
	// resolvable on PATH only from the operator directory. This reproduces
	// 4eb38221: the script is staged, its interpreter is not.
	denied, deniedErr := run(t, []string{unstagedRoot}, []string{unstagedBin, operatorBin, "/usr/bin", "/bin"})
	if deniedErr == nil && strings.Contains(denied, "SCRIPT_RAN") {
		t.Fatalf("the script ran with its interpreter reachable ONLY through the operator directory (%q).\nEither that root is granted - the exposure this design removes - or this test is not measuring the sandbox.\noutput=%q", operatorBin, denied)
	}

	// ARM B: the interpreter is staged beside the script, the way production now
	// stages it, and the operator directory is absent from PATH entirely. This
	// must run, or the fix has bought containment at the cost of availability -
	// the trade ruling 122157 explicitly refuses.
	allowed, allowedErr := run(t, []string{stagedRoot}, []string{stagedBin, "/usr/bin", "/bin"})
	if allowedErr != nil {
		t.Fatalf("a script runtime with its interpreter staged beside it did not run inside the sandbox: %v\noutput=%q\nThis is #1918's failure with the fix in place, which would mean staging the interpreter is not sufficient.", allowedErr, allowed)
	}
	if !strings.Contains(allowed, "SCRIPT_RAN") {
		t.Fatalf("staged arm output = %q, want SCRIPT_RAN", allowed)
	}
}
