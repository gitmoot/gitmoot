package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

type contractProbeRunner struct {
	path string

	mu               sync.Mutex
	helpCalls        int
	firstHelpTimeout bool
}

func (r *contractProbeRunner) LookPath(string) (string, error) {
	if r.path == "" {
		return "", errors.New("binary missing")
	}
	return r.path, nil
}

func (r *contractProbeRunner) Run(ctx context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	content, err := os.ReadFile(command)
	if err != nil {
		return subprocess.Result{}, err
	}
	mode := string(content)
	if len(args) == 1 && args[0] == "--version" {
		return subprocess.Result{Stdout: "stub 1.2.3\n"}, nil
	}
	if len(args) == 1 && args[0] == "--help" {
		r.mu.Lock()
		r.helpCalls++
		helpCall := r.helpCalls
		firstHelpTimeout := r.firstHelpTimeout
		r.mu.Unlock()
		switch {
		case firstHelpTimeout && helpCall == 1:
			<-ctx.Done()
			return subprocess.Result{}, ctx.Err()
		case strings.Contains(mode, "unparseable"):
			return subprocess.Result{Stdout: "???\n"}, nil
		case strings.Contains(mode, "unsupported"):
			return subprocess.Result{Stdout: "Usage: kimi [options]\n  --prompt\n  --output-format\n"}, nil
		default:
			return subprocess.Result{Stdout: "Usage: kimi [options]\n  --print\n  -p, --prompt\n  --output-format\n"}, nil
		}
	}
	return subprocess.Result{}, errors.New("unexpected probe")
}

func (r *contractProbeRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.helpCalls
}

func newContractCheckerForTest(t *testing.T, content string) (*RuntimeContractChecker, *contractProbeRunner, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kimi")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &contractProbeRunner{path: path}
	checker := NewRuntimeContractChecker(runner, BuiltinRuntimeRegistry())
	return checker, runner, path
}

func TestRuntimePreflightUnknownNeverBlocks(t *testing.T) {
	checker, _, _ := newContractCheckerForTest(t, "unparseable")
	agent := Agent{Name: "legacy", Runtime: KimiCLIRuntime, AutonomyPolicy: AutonomyPolicyAuto}
	result := checker.Check(context.Background(), agent)
	if result.State != RuntimeContractUnknown {
		t.Fatalf("state = %q, want unknown", result.State)
	}
	if err := RuntimeContractDispatchError(agent, result); err != nil {
		t.Fatalf("unknown preflight blocked dispatch: %v", err)
	}
}

func TestRuntimePreflightUnsupportedErrorNamesRequiredFlag(t *testing.T) {
	checker, _, _ := newContractCheckerForTest(t, "unsupported")
	agent := Agent{Name: "legacy", Runtime: KimiCLIRuntime, AutonomyPolicy: AutonomyPolicyAuto}
	result := checker.Check(context.Background(), agent)
	if result.State != RuntimeContractUnsupported {
		t.Fatalf("state = %q, want unsupported", result.State)
	}
	err := RuntimeContractDispatchError(agent, result)
	if err == nil {
		t.Fatal("unsupported preflight did not block")
	}
	for _, want := range []string{"kimi-cli", "--print", "stub 1.2.3", "remedy", "kimiCLIPromptArgs"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestRuntimePreflightReprobesChangedBinaryIdentity(t *testing.T) {
	checker, runner, path := newContractCheckerForTest(t, "supported")
	agent := Agent{Name: "legacy", Runtime: KimiCLIRuntime}
	if got := checker.Check(context.Background(), agent).State; got != RuntimeContractSupported {
		t.Fatalf("initial state = %q, want supported", got)
	}
	if err := os.WriteFile(path, []byte("unsupported-and-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := checker.Check(context.Background(), agent).State; got != RuntimeContractUnsupported {
		t.Fatalf("updated state = %q, want unsupported", got)
	}
	if got := runner.calls(); got != 2 {
		t.Fatalf("help probes = %d, want 2 after binary identity changed", got)
	}
}

func TestRuntimePreflightCachesUnchangedBinaryIdentity(t *testing.T) {
	checker, runner, _ := newContractCheckerForTest(t, "supported")
	agent := Agent{Name: "legacy", Runtime: KimiCLIRuntime}
	for i := 0; i < 2; i++ {
		result := checker.Check(context.Background(), agent)
		if result.State != RuntimeContractSupported || result.Instrument != "binary-help" {
			t.Fatalf("check %d result = %#v, want supported from binary-help", i+1, result)
		}
	}
	if got := runner.calls(); got != 1 {
		t.Fatalf("help probes = %d, want one cached probe", got)
	}
}

func TestRuntimePreflightDoesNotCacheUnknownProbe(t *testing.T) {
	checker, runner, _ := newContractCheckerForTest(t, "supported")
	runner.firstHelpTimeout = true
	checker.Timeout = time.Millisecond
	agent := Agent{Name: "legacy", Runtime: KimiCLIRuntime}
	if got := checker.Check(context.Background(), agent).State; got != RuntimeContractUnknown {
		t.Fatalf("first state = %q, want unknown", got)
	}
	if got := checker.Check(context.Background(), agent).State; got != RuntimeContractSupported {
		t.Fatalf("second state = %q, want supported after retry", got)
	}
	if got := runner.calls(); got != 2 {
		t.Fatalf("help probes = %d, want 2 after unknown result", got)
	}
}

func TestRuntimePreflightHonorsDeclaredPrecondition(t *testing.T) {
	checker, _, _ := newContractCheckerForTest(t, "supported")
	checker.EffectiveUID = func() (int, bool) { return 0, true }
	// Point the shared-binary fixture at Claude metadata; its help is parseable and
	// lists --permission-mode for this test.
	checker.Registry = registryWithContractForTest(ClaudeRuntime, RuntimeContract{
		Binary: "claude",
		Requirements: []RuntimeRequirement{{
			Kind: RuntimeRequirementNonRootEUID, Name: "precondition effective uid != 0 for --permission-mode bypassPermissions",
			Flag: "--permission-mode bypassPermissions", Source: "internal/runtime/adapter.go::claudePermissionArgs",
			Remedy: "run as non-root", Policies: []string{AutonomyPolicyDangerFullAccess}, Instrument: "effective-uid",
		}},
	})
	agent := Agent{Name: "root-claude", Runtime: ClaudeRuntime, AutonomyPolicy: AutonomyPolicyDangerFullAccess}
	result := checker.Check(context.Background(), agent)
	if result.State != RuntimeContractUnsupported || result.Instrument != "effective-uid" {
		t.Fatalf("precondition result = %#v, want unsupported from effective-uid", result)
	}
	if err := RuntimeContractDispatchError(agent, result); err == nil || !strings.Contains(err.Error(), "--permission-mode bypassPermissions") {
		t.Fatalf("precondition error = %v, want exact argv", err)
	}
	checker.EffectiveUID = func() (int, bool) { return 0, false }
	result = checker.Check(context.Background(), agent)
	if result.State != RuntimeContractUnknown || result.Instrument != "effective-uid" {
		t.Fatalf("undecidable precondition result = %#v, want unknown from effective-uid", result)
	}
	if err := RuntimeContractDispatchError(agent, result); err != nil {
		t.Fatalf("undecidable precondition blocked dispatch: %v", err)
	}
}

func registryWithContractForTest(name string, contract RuntimeContract) Registry {
	meta := RuntimeMetadata{Name: name, Dispatchable: true, Contract: contract}
	return Registry{order: []string{name}, entries: map[string]RuntimeMetadata{name: meta}}
}
