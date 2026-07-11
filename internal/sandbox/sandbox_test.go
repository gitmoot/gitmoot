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
