package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func buildGitmootBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Landlock kernel E2E requires Linux")
	}
	path := filepath.Join(t.TempDir(), "gitmoot")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", path, "./cmd/gitmoot")
	cmd.Dir = filepath.Join("..", "..")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gitmoot test binary: %v\n%s", err, output)
	}
	return path
}

func requireLandlockABI(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Landlock requires Linux")
	}
	abi, err := ABI()
	if err != nil || abi < MinimumABI {
		t.Skipf("Landlock ABI v%d unavailable (need v%d): %v", abi, MinimumABI, err)
	}
	return abi
}

func TestSandboxExecKernelE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)
	base, err := os.MkdirTemp(".", ".gitmoot-sandbox-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	inside := filepath.Join(base, "inside")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	insideFile := filepath.Join(inside, "artifact")
	allowed := exec.Command(gitmoot, "sandbox-exec", "--write", inside, "--", "/bin/sh", "-c",
		`printf ok > "$0" && cat /etc/os-release >/dev/null && /bin/true`, insideFile)
	allowed.Dir = inside
	if output, err := allowed.CombinedOutput(); err != nil {
		t.Fatalf("allowed write/read/exec failed: %v\n%s", err, output)
	}
	if data, err := os.ReadFile(insideFile); err != nil || string(data) != "ok" {
		t.Fatalf("inside artifact = %q, err=%v", data, err)
	}

	outsideFile := filepath.Join(outside, "escape")
	denied := exec.Command(gitmoot, "sandbox-exec", "--write", inside, "--", "/bin/sh", "-c", `printf no > "$0"`, outsideFile)
	denied.Dir = inside
	if output, err := denied.CombinedOutput(); err == nil {
		t.Fatalf("outside write unexpectedly succeeded: %s", output)
	}
	if _, err := os.Stat(outsideFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file exists or stat failed unexpectedly: %v", err)
	}
}

func TestSandboxExecReadOnlyWorkdirE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)
	base := t.TempDir()
	workdir := filepath.Join(base, "review-worktree")
	cacheDir := filepath.Join(base, "review-cache")
	gitMetadataDir := filepath.Join(base, "linked-gitdir")
	outsideDir := filepath.Join(base, "outside")
	for _, dir := range []string{workdir, cacheDir, filepath.Join(cacheDir, "tmp"), gitMetadataDir, outsideDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cleanupProbeDir := filepath.Join(cacheDir, "go-mod", ".cleanup-probe")
	// The go command installs downloaded toolchains as read-only modules. Make
	// their directories removable before testing.TempDir cleans the sandbox.
	t.Cleanup(func() {
		err := filepath.Walk(cacheDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return nil
		})
		if err != nil {
			t.Errorf("make sandbox tool cache removable: %v", err)
			return
		}
		if info, statErr := os.Stat(cleanupProbeDir); statErr == nil {
			if info.Mode().Perm()&0o200 == 0 {
				t.Errorf("sandbox tool cache cleanup left probe directory read-only: %v", info.Mode().Perm())
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("inspect sandbox tool cache cleanup probe: %v", statErr)
		}
	})
	source := filepath.Join(workdir, "source.txt")
	if err := os.WriteFile(source, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataFile := filepath.Join(gitMetadataDir, "index")
	if err := os.WriteFile(metadataFile, []byte("metadata-unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outsideDir, "credential")
	if err := os.WriteFile(outsideFile, []byte("must-stay-hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "go.mod"), []byte("module reviewtest\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "review_test.go"), []byte("package reviewtest\n\nimport \"testing\"\n\nfunc TestReviewerCanRunTests(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cacheArtifact := filepath.Join(cacheDir, "test-result")
	script := `set -eu
go test -count=1 .
printf passed > "$1"
if { printf mutated > "$2"; } 2>/dev/null; then exit 41; fi
if { printf metadata-mutated > "$3"; } 2>/dev/null; then exit 42; fi
if cat "$4" >/dev/null 2>&1; then exit 43; fi
`
	command := exec.Command(gitmoot, "sandbox-exec", "--read-only-workdir", "--read", workdir, "--read", gitMetadataDir, "--write", cacheDir, "--", "/bin/sh", "-c", script, "gitmoot-test", cacheArtifact, source, metadataFile, outsideFile)
	command.Dir = workdir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(cacheDir, "home"),
		"GOTOOLCHAIN=go1.26.0",
		"GOFLAGS=" + os.Getenv("GOFLAGS"),
		"GOCACHE=" + filepath.Join(cacheDir, "go-build"),
		"GOMODCACHE=" + filepath.Join(cacheDir, "go-mod"),
		"GOPATH=" + filepath.Join(cacheDir, "gopath"),
		"TMPDIR=" + filepath.Join(cacheDir, "tmp"),
	}
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("executable read-only review sandbox failed: %v\n%s", err, combined)
	}
	if data, err := os.ReadFile(cacheArtifact); err != nil || string(data) != "passed" {
		t.Fatalf("cache artifact = %q, err=%v", data, err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "unchanged" {
		t.Fatalf("review worktree changed to %q, err=%v", data, err)
	}
	if data, err := os.ReadFile(metadataFile); err != nil || string(data) != "metadata-unchanged" {
		t.Fatalf("linked git metadata changed to %q, err=%v", data, err)
	}
	// Exercise the cleanup contract even when the host already has the requested
	// Go toolchain and therefore does not download a read-only toolchain module.
	if err := os.MkdirAll(cleanupProbeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleanupProbeDir, "artifact"), []byte("probe"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cleanupProbeDir, 0o500); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxExecReadOnlyInputE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)
	base, err := os.MkdirTemp(".", ".gitmoot-sandbox-read-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	workdir := filepath.Join(base, "worktree")
	readDir := filepath.Join(base, "input")
	writeDir := filepath.Join(base, "output")
	outsideDir := filepath.Join(base, "outside")
	for _, dir := range []string{workdir, readDir, writeDir, outsideDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(readDir, "source.txt")
	outside := filepath.Join(outsideDir, "hidden.txt")
	output := filepath.Join(writeDir, "result.txt")
	if err := os.WriteFile(input, []byte("source-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `set -eu
cat "$1" > "$2"
if { printf denied > "$1"; } 2>/dev/null; then exit 41; fi
if cat "$3" >/dev/null 2>&1; then exit 42; fi
`
	command := exec.Command(gitmoot, "sandbox-exec", "--read", readDir, "--write", writeDir, "--", "/bin/sh", "-c", script, "gitmoot-test", input, output, outside)
	command.Dir = workdir
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("read-only produce sandbox failed: %v\n%s", err, combined)
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "source-data" {
		t.Fatalf("output = %q, err=%v", data, err)
	}
	if data, err := os.ReadFile(input); err != nil || string(data) != "source-data" {
		t.Fatalf("read-only input changed to %q, err=%v", data, err)
	}
}

func resetProbeCache(t *testing.T) {
	t.Helper()
	probeOnce = sync.Once{}
	probeResult = ProbeResult{}
	t.Cleanup(func() {
		probeOnce = sync.Once{}
		probeResult = ProbeResult{}
	})
}

func TestSandboxProbeSupported(t *testing.T) {
	abi := requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)
	resetProbeCache(t)
	t.Setenv(probeExecutableEnv, gitmoot)
	t.Setenv(probeForceUnsupportedEnv, "")
	result := SandboxProbe()
	if !result.Supported || result.ABI != abi || result.Err != nil {
		t.Fatalf("SandboxProbe = %+v, want supported ABI v%d", result, abi)
	}
	if cached := SandboxProbe(); cached.Supported != result.Supported || cached.ABI != result.ABI {
		t.Fatalf("cached SandboxProbe = %+v, want %+v", cached, result)
	}
}

func TestSandboxProbeForcedUnsupported(t *testing.T) {
	resetProbeCache(t)
	t.Setenv(probeForceUnsupportedEnv, "1")
	result := SandboxProbe()
	if result.Supported || result.Err == nil || !strings.Contains(result.Err.Error(), "forced unsupported") {
		t.Fatalf("SandboxProbe forced result = %+v", result)
	}
}

// TestSandboxExecStrictReadModeReachesProcfsE2E is the REAL-SUBPROCESS half of
// the procfs grant. TestReadableRootsGrantsProcfs only asserts list membership,
// which would stay green if the rule stopped being applied; this runs a genuine
// Landlock-confined child in STRICT read mode (reads declared, the read-only
// seat's shape) and reads the exact file whose denial broke every reviewer:
// codex's bwrap died on /proc/sys/kernel/overflowuid and the Bun-based Claude
// binary aborted with no message at all.
func TestSandboxExecStrictReadModeReachesProcfsE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	cacheDir := filepath.Join(base, "cache")
	for _, dir := range []string{workdir, cacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Strict read mode is selected by declaring reads; that is the mode the
	// grant regressed in. The legacy no-reads mode never lost procfs.
	command := exec.Command(gitmoot, "sandbox-exec", "--read-only-workdir",
		"--read", workdir, "--write", cacheDir, "--",
		"/bin/sh", "-c", `cat /proc/sys/kernel/overflowuid && cat /proc/self/status >/dev/null`)
	command.Dir = workdir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("strict-read-mode sandbox could not read procfs: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "65534") {
		t.Fatalf("overflowuid read returned %q, want the kernel value; procfs is not genuinely readable", output)
	}
}
