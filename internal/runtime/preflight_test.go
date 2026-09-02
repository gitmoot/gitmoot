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
		case strings.Contains(mode, "omp-without-plan"):
			return subprocess.Result{Stdout: "Usage: omp [options]\n  -p, --print\n  --mode=<value>\n  --approval-mode=<value>\n  --no-session\n"}, nil
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
	agent := Agent{Name: "seat", Runtime: KimiRuntime, AutonomyPolicy: AutonomyPolicyAuto}
	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})
	if result.State != RuntimeContractUnknown {
		t.Fatalf("state = %q, want unknown", result.State)
	}
	if err := RuntimeContractDispatchError(agent, result); err != nil {
		t.Fatalf("unknown preflight blocked dispatch: %v", err)
	}
}

func TestRuntimePreflightUnsupportedErrorNamesRequiredFlag(t *testing.T) {
	checker, _, _ := newContractCheckerForTest(t, "unsupported")
	agent := Agent{Name: "seat", Runtime: KimiRuntime, AutonomyPolicy: AutonomyPolicyAuto}
	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})
	if result.State != RuntimeContractUnsupported {
		t.Fatalf("state = %q, want unsupported", result.State)
	}
	err := RuntimeContractDispatchError(agent, result)
	if err == nil {
		t.Fatal("unsupported preflight did not block")
	}
	for _, want := range []string{"kimi", "flag -p", "stub 1.2.3", "remedy", "KimiAdapter.Deliver"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestRuntimePreflightScopesOmpPlanFlagsToPlanJobs(t *testing.T) {
	checker, _, _ := newContractCheckerForTest(t, "omp-without-plan")
	agent := Agent{Name: "planner", Runtime: OmpRuntime, AutonomyPolicy: AutonomyPolicyAuto}

	normal := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})
	if normal.State != RuntimeContractSupported {
		t.Fatalf("normal job state = %q, want supported without optional plan flags: %#v", normal.State, normal)
	}
	for _, requirement := range normal.Requirements {
		if strings.HasPrefix(requirement.Flag, "--plan-yolo") {
			t.Fatalf("normal job evaluated request-scoped requirement %#v", requirement)
		}
	}

	plan := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{Plan: true})
	if plan.State != RuntimeContractUnsupported {
		t.Fatalf("plan job state = %q, want unsupported when plan flags are absent: %#v", plan.State, plan)
	}
	err := RuntimeContractDispatchError(agent, plan)
	if err == nil || !strings.Contains(err.Error(), "--plan-yolo") {
		t.Fatalf("plan job error = %v, want missing plan flag named", err)
	}
}

func TestRuntimePreflightReprobesChangedBinaryIdentity(t *testing.T) {
	checker, runner, path := newContractCheckerForTest(t, "supported")
	agent := Agent{Name: "seat", Runtime: KimiRuntime}
	if got := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{}).State; got != RuntimeContractSupported {
		t.Fatalf("initial state = %q, want supported", got)
	}
	if err := os.WriteFile(path, []byte("unsupported-and-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{}).State; got != RuntimeContractUnsupported {
		t.Fatalf("updated state = %q, want unsupported", got)
	}
	if got := runner.calls(); got != 2 {
		t.Fatalf("help probes = %d, want 2 after binary identity changed", got)
	}
}

func TestRuntimePreflightCachesUnchangedBinaryIdentity(t *testing.T) {
	checker, runner, _ := newContractCheckerForTest(t, "supported")
	now := time.Unix(1_700_000_000, 0)
	checker.now = func() time.Time { return now }
	agent := Agent{Name: "seat", Runtime: KimiRuntime}
	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})
	if result.State != RuntimeContractSupported || result.Instrument != "binary-help" {
		t.Fatalf("first result = %#v, want supported from binary-help", result)
	}
	now = now.Add(2 * unknownBinaryProbeTTL)
	result = checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})
	if result.State != RuntimeContractSupported || result.Instrument != "binary-help" {
		t.Fatalf("second result = %#v, want supported from binary-help", result)
	}
	if got := runner.calls(); got != 1 {
		t.Fatalf("help probes = %d, want successful probe cached without TTL", got)
	}
}

func TestRuntimePreflightCachesUnknownProbeUntilTTL(t *testing.T) {
	checker, runner, _ := newContractCheckerForTest(t, "supported")
	runner.firstHelpTimeout = true
	checker.Timeout = time.Millisecond
	now := time.Unix(1_700_000_000, 0)
	checker.now = func() time.Time { return now }
	agent := Agent{Name: "seat", Runtime: KimiRuntime}
	if got := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{}).State; got != RuntimeContractUnknown {
		t.Fatalf("first state = %q, want unknown", got)
	}
	now = now.Add(unknownBinaryProbeTTL - time.Second)
	if got := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{}).State; got != RuntimeContractUnknown {
		t.Fatalf("within-TTL state = %q, want cached unknown", got)
	}
	if got := runner.calls(); got != 1 {
		t.Fatalf("within-TTL help probes = %d, want 1", got)
	}
	now = now.Add(time.Second)
	if got := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{}).State; got != RuntimeContractSupported {
		t.Fatalf("second state = %q, want supported after retry", got)
	}
	if got := runner.calls(); got != 2 {
		t.Fatalf("post-TTL help probes = %d, want 2", got)
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
	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})
	if result.State != RuntimeContractUnsupported || result.Instrument != "effective-uid" {
		t.Fatalf("precondition result = %#v, want unsupported from effective-uid", result)
	}
	if err := RuntimeContractDispatchError(agent, result); err == nil || !strings.Contains(err.Error(), "--permission-mode bypassPermissions") {
		t.Fatalf("precondition error = %v, want exact argv", err)
	}
	result = checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{
		EffectiveUID:      996,
		EffectiveUIDKnown: true,
	})
	if result.State != RuntimeContractSupported || len(result.Requirements) != 1 ||
		result.Requirements[0].State != RuntimeContractSupported ||
		result.Requirements[0].Instrument != "effective-uid" ||
		!strings.Contains(result.Requirements[0].Detail, "996") {
		t.Fatalf("configured execution identity result = %#v, want supported effective uid 996", result)
	}
	checker.EffectiveUID = func() (int, bool) { return 0, false }
	result = checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})
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

// TestInspectSkipsPlanScopedRequirements pins doctor's question. Inspect answers
// "can this host run this runtime", and an omp that predates --plan-yolo runs every
// ordinary job fine — the dispatch preflight scopes those flags to plan requests.
// Before this, Inspect evaluated them unconditionally and reported the omp contract
// UNSUPPORTED on such a host, so doctor and the dispatch gate disagreed about the
// same machine and the remedy told operators to upgrade a CLI they did not need.
// A red that is not a defect teaches people to ignore the instrument.
func TestInspectSkipsPlanScopedRequirements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "omp")
	if err := os.WriteFile(path, []byte("omp-without-plan"), 0o755); err != nil {
		t.Fatal(err)
	}
	checker := NewRuntimeContractChecker(&contractProbeRunner{path: path}, BuiltinRuntimeRegistry())

	inspected := checker.Inspect(context.Background(), OmpRuntime)
	if inspected.State != RuntimeContractSupported {
		t.Fatalf("doctor Inspect state = %q on an omp that runs ordinary jobs fine, want supported: doctor must not report an optional-capability gap as a broken runtime", inspected.State)
	}
	for _, req := range inspected.Requirements {
		if strings.HasPrefix(req.Flag, "--plan-yolo") {
			t.Fatalf("Inspect evaluated plan-scoped requirement %q; doctor must skip request-scoped capabilities", req.Flag)
		}
	}
	// The capability gap is still discoverable — at the one place it is required.
	agent := Agent{Name: "seat", Runtime: OmpRuntime, AutonomyPolicy: AutonomyPolicyWorkspaceWrite}
	planned := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{Plan: true})
	if planned.State != RuntimeContractUnsupported {
		t.Fatalf("a PLAN request on the same host = %q, want unsupported: skipping the flag in doctor must not skip it at dispatch", planned.State)
	}
}
