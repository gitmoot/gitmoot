package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	probeForceUnsupportedEnv = "GITMOOT_SANDBOX_PROBE_FORCE_UNSUPPORTED"
	probeExecutableEnv       = "GITMOOT_SANDBOX_PROBE_EXECUTABLE"
)

// ProbeResult is the cached, definitive capability result used by operators and
// produce dispatch. Supported is true only after the real shim allowed an
// in-root write and denied an out-of-root write.
type ProbeResult struct {
	Supported bool
	ABI       int
	Err       error
}

var (
	probeOnce   sync.Once
	probeResult ProbeResult
)

// SandboxProbe checks both kernel capability and effective enforcement by
// executing the current Gitmoot binary's hidden sandbox-exec shim. Results are
// cached for the lifetime of this process.
func SandboxProbe() ProbeResult {
	probeOnce.Do(func() { probeResult = runProbe() })
	return probeResult
}

func runProbe() ProbeResult {
	if strings.TrimSpace(os.Getenv(probeForceUnsupportedEnv)) != "" {
		return ProbeResult{Err: errors.New("forced unsupported by test override")}
	}
	if runtime.GOOS != "linux" {
		return ProbeResult{Err: fmt.Errorf("Landlock sandboxing is unsupported on %s", runtime.GOOS)}
	}
	abi, err := ABI()
	if err != nil {
		return ProbeResult{Err: fmt.Errorf("query Landlock ABI: %w", err)}
	}
	result := ProbeResult{ABI: abi}
	if abi < MinimumABI {
		result.Err = fmt.Errorf("Landlock ABI v%d is too old; v%d or newer is required", abi, MinimumABI)
		return result
	}

	// Put the sibling fixtures under the caller's current directory rather than
	// os.TempDir: /tmp is intentionally writable inside the sandbox, so a sibling
	// there could not prove denial. The child itself runs with inside as its cwd,
	// therefore only inside (not this parent directory) receives the workdir grant.
	base, err := os.MkdirTemp(".", ".gitmoot-landlock-probe-*")
	if err != nil {
		result.Err = fmt.Errorf("create Landlock probe directory: %w", err)
		return result
	}
	base, err = filepath.Abs(base)
	if err != nil {
		result.Err = fmt.Errorf("resolve Landlock probe directory: %w", err)
		return result
	}
	defer os.RemoveAll(base)
	inside := filepath.Join(base, "inside")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		result.Err = fmt.Errorf("create allowed probe directory: %w", err)
		return result
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		result.Err = fmt.Errorf("create denied probe directory: %w", err)
		return result
	}

	executable := strings.TrimSpace(os.Getenv(probeExecutableEnv))
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			result.Err = fmt.Errorf("resolve Gitmoot executable: %w", err)
			return result
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	insideFile := filepath.Join(inside, "allowed")
	allowed := exec.CommandContext(ctx, executable, "sandbox-exec", "--write", inside, "--", "sh", "-c",
		`printf allowed > "$0" && cat /etc/os-release >/dev/null && /bin/true`, insideFile)
	allowed.Dir = inside
	if output, err := allowed.CombinedOutput(); err != nil {
		result.Err = fmt.Errorf("allowed Landlock probe write failed: %w: %s", err, strings.TrimSpace(string(output)))
		return result
	}
	if data, err := os.ReadFile(insideFile); err != nil || string(data) != "allowed" {
		result.Err = fmt.Errorf("allowed Landlock probe write was not persisted")
		return result
	}

	outsideFile := filepath.Join(outside, "denied")
	denied := exec.CommandContext(ctx, executable, "sandbox-exec", "--write", inside, "--", "sh", "-c",
		`printf denied > "$0"`, outsideFile)
	denied.Dir = inside
	if output, err := denied.CombinedOutput(); err == nil {
		result.Err = errors.New("Landlock probe wrote outside the allowed roots")
		return result
	} else if ctx.Err() != nil {
		result.Err = fmt.Errorf("Landlock denial probe timed out: %w", ctx.Err())
		return result
	} else if _, statErr := os.Stat(outsideFile); !errors.Is(statErr, os.ErrNotExist) {
		result.Err = fmt.Errorf("Landlock denial probe left an outside file: %v (%s)", statErr, strings.TrimSpace(string(output)))
		return result
	}

	result.Supported = true
	return result
}
