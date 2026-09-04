package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// redactEnvNames returns NAMES only, so a failure message never prints a token.
func redactEnvNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	return names
}

// Every runner shape used by the daemon must receive the same curated BaseEnv.
// In particular, the default CuratedGroupRunner and execbackend InstanceRunner
// must both carry the auth overlay and drop ambient GitHub credentials.
func TestWrapReadOnlySandboxAdapterCuratesEveryRunnerShape(t *testing.T) {
	const overlay = "CLAUDE_CODE_OAUTH_TOKEN=seat-shape-token-value"
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeAuthFilePath(paths.Home), []byte(overlay+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An ambient GH_TOKEN must never reach a read-only seat: gh prefers it over
	// the redirected GH_CONFIG_DIR, so inheriting it hands a review a live
	// GitHub credential.
	t.Setenv("GH_TOKEN", "ambient-github-token")
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

	for name, runner := range map[string]subprocess.Runner{
		"nil runner":                    nil,
		"group runner":                  subprocess.GroupRunner{},
		"curated group runner":          subprocess.CuratedGroupRunner{BaseEnv: []string{"GH_TOKEN=ambient-github-token"}},
		"execbackend instance runner":   execbackend.InstanceRunner{BaseEnv: []string{"GH_TOKEN=ambient-github-token"}},
		"tee over curated group runner": subprocess.TeeRunner{Inner: subprocess.CuratedGroupRunner{BaseEnv: []string{"GH_TOKEN=ambient-github-token"}}},
		"env injecting over group":      subprocess.EnvInjectingRunner{Inner: subprocess.GroupRunner{}},
	} {
		t.Run(name, func(t *testing.T) {
			wrapped, _, err := wrapReadOnlySandboxAdapter(home, agent, checkout, runtime.ClaudeAdapter{Runner: runner})
			if err != nil {
				t.Fatalf("wrapReadOnlySandboxAdapter: %v", err)
			}
			seat, ok := wrapped.(readOnlyRuntimeAdapter)
			if !ok {
				t.Fatalf("wrapped adapter %T, want readOnlyRuntimeAdapter", wrapped)
			}
			t.Cleanup(func() { _ = seat.cleanup() })
			inner, ok := seat.Adapter.(runtime.ClaudeAdapter)
			if !ok {
				t.Fatalf("inner adapter %T, want runtime.ClaudeAdapter", seat.Adapter)
			}
			sandboxEnv, baseEnv := seatRunnerEnvironments(t, inner.Runner)
			if !containsEnv(sandboxEnv, overlay) {
				t.Fatalf("sandbox env lacks the overlay: %v", redactEnvNames(sandboxEnv))
			}
			if !containsEnv(baseEnv, overlay) {
				t.Fatalf("runner base env lacks the overlay: %v", redactEnvNames(baseEnv))
			}
			for _, entry := range baseEnv {
				if strings.HasPrefix(entry, "GH_TOKEN=") {
					t.Fatalf("seat base env inherited GH_TOKEN: %v", redactEnvNames(baseEnv))
				}
			}
		})
	}
}

// Gateway mode holds the credential itself, so the seat is deliberately given
// no overlay: injecting one would hand the sandboxed child a real token the
// gateway exists to withhold.
func TestWrapReadOnlySandboxAdapterWithholdsAuthInGatewayMode(t *testing.T) {
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeAuthFilePath(paths.Home),
		[]byte("CLAUDE_CODE_OAUTH_TOKEN=gateway-withheld-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	gatewayRunner := &credgw.Runner{Inner: subprocess.CuratedGroupRunner{}}
	adapter := modelGatewayRuntimeAdapter{
		Adapter: runtime.ClaudeAdapter{Runner: gatewayRunner},
		runner:  gatewayRunner,
	}
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

	wrapped, _, err := wrapReadOnlySandboxAdapter(home, agent, checkout, adapter)
	if err != nil {
		t.Fatalf("wrapReadOnlySandboxAdapter: %v", err)
	}
	seat, ok := wrapped.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter %T, want readOnlyRuntimeAdapter", wrapped)
	}
	t.Cleanup(func() { _ = seat.cleanup() })
	gateway, ok := seat.Adapter.(modelGatewayRuntimeAdapter)
	if !ok {
		t.Fatalf("inner adapter %T, want modelGatewayRuntimeAdapter", seat.Adapter)
	}
	claude, ok := gateway.Adapter.(runtime.ClaudeAdapter)
	if !ok {
		t.Fatalf("gateway inner adapter %T, want runtime.ClaudeAdapter", gateway.Adapter)
	}
	sandboxEnv, baseEnv := seatRunnerEnvironments(t, claude.Runner)
	for _, env := range [][]string{sandboxEnv, baseEnv} {
		for _, entry := range env {
			if strings.HasPrefix(entry, "CLAUDE_CODE_OAUTH_TOKEN=") && !strings.HasSuffix(entry, "=") {
				t.Fatalf("gateway seat received a real token: %v", redactEnvNames(env))
			}
		}
	}
}

// seatRunnerEnvironments returns the sandbox wrapper's env and the innermost
// runner's base env, so a test can tell the grants route from the graft route
// apart instead of passing on either alone.
func seatRunnerEnvironments(t *testing.T, runner subprocess.Runner) (sandboxEnv []string, baseEnv []string) {
	t.Helper()
	for {
		switch r := runner.(type) {
		case subprocess.WrappingRunner:
			sandboxEnv = append(sandboxEnv, r.Env...)
			runner = r.Inner
		case *subprocess.WrappingRunner:
			sandboxEnv = append(sandboxEnv, r.Env...)
			runner = r.Inner
		case subprocess.TeeRunner:
			runner = r.Inner
		case *subprocess.TeeRunner:
			runner = r.Inner
		case subprocess.EnvInjectingRunner:
			sandboxEnv = append(sandboxEnv, r.Env...)
			runner = r.Inner
		case *subprocess.EnvInjectingRunner:
			sandboxEnv = append(sandboxEnv, r.Env...)
			runner = r.Inner
		case *credgw.Runner:
			runner = r.Inner
		case subprocess.CuratedGroupRunner:
			return sandboxEnv, r.BaseEnv
		case *subprocess.CuratedGroupRunner:
			return sandboxEnv, r.BaseEnv
		case execbackend.InstanceRunner:
			return sandboxEnv, r.BaseEnv
		case *execbackend.InstanceRunner:
			return sandboxEnv, r.BaseEnv
		default:
			t.Fatalf("runner shape %T carries no base environment", runner)
			return sandboxEnv, nil
		}
	}
}

// The cockpit log tee REBUILT the adapter from the worker's execution runner and
// assigned over the composed one, discarding the whole read-only seat wrapper:
// no Landlock, no isolated state dir, no auth overlay. With produce grants
// already applied by then, the rebuilt job would even receive a WRITE grant on
// the operator's live credential dir (#1810 review F5). Teeing must ADD a writer
// to the composed adapter instead.
func TestCockpitTeeKeepsTheReadOnlySeatSandbox(t *testing.T) {
	const overlay = "CLAUDE_CODE_OAUTH_TOKEN=seat-cockpit-token-value"
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeAuthFilePath(paths.Home), []byte(overlay+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
	seatAdapter, _, err := wrapReadOnlySandboxAdapter(home, agent, checkout,
		runtime.ClaudeAdapter{Runner: subprocess.CuratedGroupRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	seat, ok := seatAdapter.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter %T, want readOnlyRuntimeAdapter", seatAdapter)
	}
	t.Cleanup(func() { _ = seat.cleanup() })

	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)
	teed, logPath, logFile := worker.cockpitTeeAdapter(seatAdapter, "seat-cockpit-job")
	if logFile == nil {
		t.Fatalf("cockpit tee returned no log file (path %q)", logPath)
	}
	defer logFile.Close()

	teedSeat, ok := teed.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("teed adapter %T discarded the read-only seat wrapper", teed)
	}
	inner, ok := teedSeat.Adapter.(runtime.ClaudeAdapter)
	if !ok {
		t.Fatalf("teed inner adapter %T, want runtime.ClaudeAdapter", teedSeat.Adapter)
	}
	sandboxEnv, baseEnv := seatRunnerEnvironments(t, inner.Runner)
	if !containsEnv(sandboxEnv, overlay) || !containsEnv(baseEnv, overlay) {
		t.Fatalf("teeing dropped the seat auth overlay: sandbox=%v base=%v",
			redactEnvNames(sandboxEnv), redactEnvNames(baseEnv))
	}
	if !containsEnvPrefix(sandboxEnv, "CLAUDE_CONFIG_DIR=") {
		t.Fatalf("teeing dropped the isolated runtime state dir: %v", redactEnvNames(sandboxEnv))
	}
}
