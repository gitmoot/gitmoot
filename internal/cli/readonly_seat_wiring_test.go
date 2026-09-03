package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// F3 from the #1810 review: the fix reach is SHAPE-DEPENDENT, and nothing tested
// the wiring. graftRuntimeBaseRunner has no CuratedGroupRunner case, so only the
// execbackend InstanceRunner shape rebuilds a seat env from
// readOnlyRuntimeBaseEnv (this host uses it). This pins that the injected
// overlay reaches the runner the adapter actually calls, for that shape.
func TestWrapReadOnlySandboxAdapterCarriesResolvedAuthIntoTheRunner(t *testing.T) {
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeAuthFilePath(paths.Home),
		[]byte("CLAUDE_CODE_OAUTH_TOKEN=seat-overlay-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")

	recorder := &envRecordingRunner{}
	agent := runtime.Agent{
		Runtime:       runtime.ClaudeRuntime,
		ReadOnlySeat:  true,
		WritablePaths: nil,
	}
	wrapped, err := wrapReadOnlySandboxAdapter(home, agent, checkout, runtime.ClaudeAdapter{Runner: recorder})
	if err != nil {
		t.Fatalf("wrapReadOnlySandboxAdapter: %v", err)
	}
	// The seat wrapper returns readOnlyRuntimeAdapter, which embeds the runtime
	// adapter it wrapped; reach the inner ClaudeAdapter to inspect its runner.
	seat, ok := wrapped.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter type %T, want readOnlyRuntimeAdapter", wrapped)
	}
	t.Cleanup(func() { _ = seat.cleanup() })
	adapter, ok := seat.Adapter.(runtime.ClaudeAdapter)
	if !ok {
		t.Fatalf("inner adapter type %T, want runtime.ClaudeAdapter", seat.Adapter)
	}
	envRunner, ok := adapter.Runner.(subprocess.EnvRunner)
	if !ok {
		t.Fatalf("wrapped seat runner %T does not accept an environment", adapter.Runner)
	}
	if _, err := envRunner.RunEnv(context.Background(), checkout, nil, "/bin/true"); err != nil && recorder.env == nil {
		t.Fatalf("runner never ran: %v", err)
	}
	if !containsEnv(recorder.env, "CLAUDE_CODE_OAUTH_TOKEN=seat-overlay-token-value") {
		t.Fatalf("seat runner env lacks the resolved overlay: %v", redactEnvNames(recorder.env))
	}
}

// envRecordingRunner captures the environment the innermost runner is handed.
type envRecordingRunner struct {
	env []string
}

func (r *envRecordingRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{}, errors.New("env-recording runner requires RunEnv")
}

func (r *envRecordingRunner) LookPath(file string) (string, error) {
	return file, nil
}

func (r *envRecordingRunner) RunEnv(_ context.Context, _ string, env []string, _ string, _ ...string) (subprocess.Result, error) {
	r.env = append([]string(nil), env...)
	return subprocess.Result{}, nil
}

// redactEnvNames returns NAMES only, so a failure message never prints a token.
func redactEnvNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	return names
}
