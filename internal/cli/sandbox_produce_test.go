package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/sandbox"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestWorkerProducePreflightProbePassAndFail(t *testing.T) {
	claude := runtime.Agent{Name: "p", Runtime: runtime.ClaudeRuntime, AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}
	pass := jobWorker{SandboxProbe: func() sandbox.ProbeResult { return sandbox.ProbeResult{Supported: true, ABI: 5} }}
	if err := pass.produceDispatchError("produce", claude); err != nil {
		t.Fatalf("Claude produce probe-pass = %v", err)
	}
	kimi := claude
	kimi.Runtime = runtime.KimiRuntime
	if err := pass.produceDispatchError("produce", kimi); err != nil {
		t.Fatalf("Kimi produce probe-pass = %v", err)
	}

	fail := jobWorker{SandboxProbe: func() sandbox.ProbeResult { return sandbox.ProbeResult{ABI: 2, Err: errors.New("no Landlock")} }}
	want := `produce stages require the codex runtime; agent "p" uses runtime "claude"`
	if err := fail.produceDispatchError("produce", claude); err == nil || err.Error() != want {
		t.Fatalf("Claude probe-fail error = %v, want %q", err, want)
	}
}

func TestWorkerProduceProbeFailureRecordsSeparateDiagnosticEvent(t *testing.T) {
	store := pipelineAdvanceStore(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "produce-probe-fail", Agent: "p", Type: "produce", State: "queued"}); err != nil {
		t.Fatal(err)
	}
	w := jobWorker{Store: store, SandboxProbe: func() sandbox.ProbeResult {
		return sandbox.ProbeResult{ABI: 2, Err: errors.New("no Landlock")}
	}}
	w.recordProduceSandboxDiagnostic(ctx, "produce-probe-fail", "produce", runtime.Agent{
		Name: "p", Runtime: runtime.ClaudeRuntime, AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite,
	})
	events, err := store.ListJobEvents(ctx, "produce-probe-fail")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "produce_sandbox_unsupported" {
			want := "Gitmoot Landlock sandbox unavailable for claude produce: Landlock ABI v2: no Landlock; run gitmoot sandbox probe"
			if event.Message != want {
				t.Fatalf("diagnostic event = %q, want %q", event.Message, want)
			}
			return
		}
	}
	t.Fatalf("produce_sandbox_unsupported event missing: %+v", events)
}

func TestWorkerProducePreflightCodexAndNonProduceNeverProbe(t *testing.T) {
	probes := 0
	w := jobWorker{SandboxProbe: func() sandbox.ProbeResult {
		probes++
		return sandbox.ProbeResult{Err: errors.New("must not run")}
	}}
	codex := runtime.Agent{Name: "p", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}
	if err := w.produceDispatchError("produce", codex); err != nil {
		t.Fatalf("Codex produce changed: %v", err)
	}
	claude := codex
	claude.Runtime = runtime.ClaudeRuntime
	if err := w.produceDispatchError("ask", claude); err != nil {
		t.Fatalf("non-produce changed: %v", err)
	}
	if probes != 0 {
		t.Fatalf("probe called %d times, want 0", probes)
	}
}

type sandboxAdapterCaptureRunner struct {
	stdout  string
	err     error
	dir     string
	command string
	args    []string
	env     []string
}

func (r *sandboxAdapterCaptureRunner) Run(_ context.Context, dir, command string, args ...string) (subprocess.Result, error) {
	r.dir = dir
	r.command = command
	r.args = append([]string(nil), args...)
	return subprocess.Result{Command: command, Args: args, Stdout: r.stdout}, r.err
}

func (r *sandboxAdapterCaptureRunner) RunEnv(_ context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	r.env = append([]string(nil), env...)
	return r.Run(context.Background(), dir, command, args...)
}

func (r *sandboxAdapterCaptureRunner) LookPath(file string) (string, error) { return file, nil }

type repairStateRunner struct {
	calls               int
	stateDir            string
	repairStateObserved bool
	toolCacheObserved   bool
}

func (r *repairStateRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{}, errors.New("repair-state runner requires explicit environment")
}

func (r *repairStateRunner) RunEnv(_ context.Context, _ string, env []string, command string, args ...string) (subprocess.Result, error) {
	r.calls++
	stateDir := envValue(env, "CLAUDE_CONFIG_DIR")
	if stateDir == "" {
		return subprocess.Result{}, errors.New("missing isolated CLAUDE_CONFIG_DIR")
	}
	credential := filepath.Join(stateDir, ".credentials.json")
	if _, err := os.ReadFile(credential); err != nil {
		return subprocess.Result{}, err
	}
	if cache := envValue(env, "GOCACHE"); strings.Contains(cache, filepath.Join("cache", "tools", "read-only")) {
		r.toolCacheObserved = true
	}
	marker := filepath.Join(stateDir, "repair.marker")
	switch r.calls {
	case 1:
		r.stateDir = stateDir
		if err := os.WriteFile(marker, []byte("first-delivery"), 0o600); err != nil {
			return subprocess.Result{}, err
		}
		return subprocess.Result{Command: command, Args: args, Stdout: `{"result":"review complete, no json"}`}, nil
	case 2:
		if stateDir != r.stateDir {
			return subprocess.Result{}, errors.New("repair delivery changed isolated state directory")
		}
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != "first-delivery" {
			return subprocess.Result{}, errors.New("repair delivery lost first-delivery state")
		}
		r.repairStateObserved = true
		return subprocess.Result{Command: command, Args: args, Stdout: `{"result":"{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"clean after repair\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[]}}"}`}, nil
	default:
		return subprocess.Result{}, errors.New("unexpected extra repair delivery")
	}
}

func (r *repairStateRunner) LookPath(file string) (string, error) { return file, nil }

type kimiHomeStateRunner struct {
	home               string
	credentialObserved bool
}

func (r *kimiHomeStateRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{}, errors.New("kimi-home runner requires explicit environment")
}

func (r *kimiHomeStateRunner) RunEnv(_ context.Context, _ string, env []string, command string, args ...string) (subprocess.Result, error) {
	r.home = envValue(env, "HOME")
	if r.home == "" {
		return subprocess.Result{}, errors.New("missing isolated HOME")
	}
	credential := filepath.Join(r.home, ".kimi-code", "credentials", "kimi-code.json")
	if _, err := os.ReadFile(credential); err != nil {
		return subprocess.Result{}, fmt.Errorf("read Kimi credential under HOME: %w", err)
	}
	r.credentialObserved = true
	return subprocess.Result{
		Command: command,
		Args:    args,
		Stdout:  `{"role":"assistant","content":"{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"Kimi profile observed\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[]}}"}` + "\n",
	}, nil
}

func (r *kimiHomeStateRunner) LookPath(file string) (string, error) { return file, nil }

type renameRuntimeStateRunner struct {
	savedState string
}

func (r *renameRuntimeStateRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{}, errors.New("rename-state runner requires explicit environment")
}

func (r *renameRuntimeStateRunner) RunEnv(_ context.Context, _ string, env []string, command string, args ...string) (subprocess.Result, error) {
	stateDir := envValue(env, "CLAUDE_CONFIG_DIR")
	if stateDir == "" {
		return subprocess.Result{}, errors.New("missing isolated CLAUDE_CONFIG_DIR")
	}
	stateRoot := filepath.Dir(stateDir)
	cacheRoot := filepath.Dir(stateRoot)
	r.savedState = filepath.Join(cacheRoot, "saved-runtime-state")
	if err := os.Rename(stateRoot, r.savedState); err != nil {
		return subprocess.Result{}, fmt.Errorf("move runtime state inside writable cache: %w", err)
	}
	return subprocess.Result{
		Command: command,
		Args:    args,
		Stdout:  `{"result":"{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"state moved\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[]}}"}`,
	}, nil
}

func (r *renameRuntimeStateRunner) LookPath(file string) (string, error) { return file, nil }

func TestWorkerClaudeKimiProduceDispatchWrappedArgv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		runtime    string
		stdout     string
		adapter    func(subprocess.Runner) workflow.DeliveryAdapter
		commandArg string
	}{
		{name: "claude", runtime: runtime.ClaudeRuntime, stdout: `{"result":"ok"}`, adapter: func(r subprocess.Runner) workflow.DeliveryAdapter {
			return runtime.ClaudeAdapter{Runner: r, Dir: "/work"}
		}, commandArg: "claude"},
		{name: "kimi", runtime: runtime.KimiRuntime, stdout: "{\"role\":\"assistant\",\"content\":\"ok\"}\n", adapter: func(r subprocess.Runner) workflow.DeliveryAdapter {
			return runtime.KimiAdapter{Runner: r, Dir: "/work"}
		}, commandArg: "kimi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", home+"/.cache")
			capture := &sandboxAdapterCaptureRunner{stdout: tc.stdout}
			ref := "last"
			if tc.runtime == runtime.KimiRuntime {
				ref = "session_550e8400-e29b-41d4-a716-446655440000"
			}
			agent := runtime.Agent{Name: "p", Role: "producer", Runtime: tc.runtime, RuntimeRef: ref, RepoScope: "owner/repo", AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite, ReadablePaths: []string{"/data/input"}, WritablePaths: []string{"/data/out"}}
			var claudeConfigDir string
			if tc.runtime == runtime.ClaudeRuntime {
				agent.ReadableFiles = []string{home + "/.claude.json"}
				if err := os.WriteFile(agent.ReadableFiles[0], []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				var resolveErr error
				claudeConfigDir, resolveErr = resolveRuntimeConfigDir(runtime.ClaudeRuntime, os.Getenv("CLAUDE_CONFIG_DIR"))
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
			}
			runtimeStateDir := filepath.Join(home, "job-runtime")
			wrapped, err := wrapProduceSandboxAdapter("produce", agent, tc.adapter(capture), runtimeStateDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wrapped.Deliver(context.Background(), agent, runtime.Job{Prompt: "write"}); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			wantPrefix := []string{"sandbox-exec", "--read", "/data/input"}
			if tc.runtime == runtime.ClaudeRuntime {
				if claudeConfigDir != filepath.Join(runtimeStateDir, ".claude") {
					wantPrefix = append(wantPrefix, "--read", claudeConfigDir)
				}
				wantPrefix = append(wantPrefix, "--read-file", home+"/.claude.json")
				wantPrefix = append(wantPrefix, "--write", "/data/out")
				wantPrefix = append(wantPrefix, "--write", filepath.Join(runtimeStateDir, ".claude"), "--write", filepath.Join(runtimeStateDir, "claude-cli-nodejs"))
				if !reflect.DeepEqual(capture.env, []string{"CLAUDE_CONFIG_DIR=" + claudeConfigDir}) {
					t.Fatalf("Claude sandbox env = %v", capture.env)
				}
			} else {
				wantPrefix = append(wantPrefix, "--write", "/data/out")
				wantPrefix = append(wantPrefix, "--write", home+"/.kimi-code")
				if len(capture.env) != 0 {
					t.Fatalf("Kimi sandbox env = %v, want empty", capture.env)
				}
			}
			wantPrefix = append(wantPrefix, "--", tc.commandArg)
			if capture.command == "" || capture.command != mustExecutable(t) || len(capture.args) < len(wantPrefix) || !reflect.DeepEqual(capture.args[:len(wantPrefix)], wantPrefix) {
				t.Fatalf("wrapped call = %q %v, want executable + prefix %v", capture.command, capture.args, wantPrefix)
			}
		})
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProduceRunnerComposesUnderTeeAndScopesByAction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home+"/.cache")
	stateFile := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(stateFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir, err := resolveRuntimeConfigDir(runtime.ClaudeRuntime, os.Getenv("CLAUDE_CONFIG_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadablePaths: []string{"/input"}, ReadableFiles: []string{stateFile}, WritablePaths: []string{"/data"}}
	base := runtime.ClaudeAdapter{Runner: subprocess.TeeRunner{Inner: subprocess.GroupRunner{}}}
	runtimeStateDir := filepath.Join(home, "job-runtime")
	wrapped, err := wrapProduceSandboxAdapter("produce", agent, base, runtimeStateDir)
	if err != nil {
		t.Fatal(err)
	}
	claude := wrapped.(runtime.ClaudeAdapter)
	tee, ok := claude.Runner.(subprocess.TeeRunner)
	if !ok {
		t.Fatalf("runner = %T, want TeeRunner", claude.Runner)
	}
	shim, ok := tee.Inner.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("tee inner = %T, want WrappingRunner", tee.Inner)
	}
	if _, ok := shim.Inner.(subprocess.GroupRunner); !ok {
		t.Fatalf("shim inner = %T, want GroupRunner", shim.Inner)
	}
	wantReads := []string{"/input"}
	if configDir != filepath.Join(runtimeStateDir, ".claude") {
		wantReads = append(wantReads, configDir)
	}
	wantPaths := []string{"/data", filepath.Join(runtimeStateDir, ".claude"), filepath.Join(runtimeStateDir, "claude-cli-nodejs")}
	if !reflect.DeepEqual(shim.ReadablePaths, wantReads) || !reflect.DeepEqual(shim.ReadableFiles, []string{stateFile}) || !reflect.DeepEqual(shim.WritablePaths, wantPaths) || !reflect.DeepEqual(shim.Env, []string{"CLAUDE_CONFIG_DIR=" + configDir}) {
		t.Fatalf("Claude shim = reads %v writes %v env %v, want reads %v, writes %v + config env", shim.ReadablePaths, shim.WritablePaths, shim.Env, wantReads, wantPaths)
	}

	nonProduce, err := wrapProduceSandboxAdapter("ask", agent, base, runtimeStateDir)
	if err != nil || !reflect.DeepEqual(nonProduce, base) {
		t.Fatalf("non-produce adapter changed: %T %+v, err=%v", nonProduce, nonProduce, err)
	}
	codexBase := runtime.CodexAdapter{Runner: subprocess.GroupRunner{}}
	codex, err := wrapProduceSandboxAdapter("produce", runtime.Agent{Runtime: runtime.CodexRuntime, ReadablePaths: []string{"/input"}, WritablePaths: []string{"/data"}}, codexBase, runtimeStateDir)
	if err != nil || !reflect.DeepEqual(codex, codexBase) {
		t.Fatalf("Codex adapter changed: %T %+v, err=%v", codex, codex, err)
	}
}

func TestWrapReadOnlySandboxAdapterUsesExplicitReadsAndIsolatedState(t *testing.T) {
	configHome := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "review-worktree")
	stateDir := filepath.Join(t.TempDir(), "claude-state")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"secret":"must-stay-hidden"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "settings.json"), []byte(`{"env":{"UNRELATED_SECRET":"must-not-copy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "must-not-reach-review")
	t.Setenv("OPENAI_API_KEY", "must-not-reach-review")
	agent := runtime.Agent{
		Runtime:          runtime.ClaudeRuntime,
		AutonomyPolicy:   runtime.AutonomyPolicyReadOnly,
		ReadOnlySeat:     true,
		RuntimeConfigDir: stateDir,
	}
	wrapped, err := wrapReadOnlySandboxAdapter(configHome, agent, checkout, runtime.ClaudeAdapter{Runner: subprocess.GroupRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	stateAdapter, ok := wrapped.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter = %T, want readOnlyRuntimeAdapter", wrapped)
	}
	stagedPath := filepath.Join(stateAdapter.cleanupRoot, "runtime-state", ".claude", ".credentials.json")
	stagedCredential, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stagedCredential), "mcpOAuth") || strings.Contains(string(stagedCredential), "must-stay-hidden") {
		t.Fatalf("isolated Claude credential leaked unrelated profile credentials: %s", stagedCredential)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(stagedPath), "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated profile copied host settings: %v", err)
	}
	adapter, ok := stateAdapter.Adapter.(runtime.ClaudeAdapter)
	if !ok {
		t.Fatalf("inner adapter = %T, want runtime.ClaudeAdapter", stateAdapter.Adapter)
	}
	runner, ok := adapter.Runner.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("wrapped runner = %T, want subprocess.WrappingRunner", adapter.Runner)
	}
	if !runner.ReadOnlyWorkdir {
		t.Fatal("read-only runner ReadOnlyWorkdir = false")
	}
	if !containsPath(runner.ReadablePaths, checkout) || !containsPath(runner.ReadablePaths, filepath.Join(checkout, ".git")) {
		t.Fatalf("explicit reads %v do not include checkout and git metadata", runner.ReadablePaths)
	}
	if containsPath(runner.ReadablePaths, "/") || containsPath(runner.ReadablePaths, stateDir) {
		t.Fatalf("explicit reads %v expose the host root or source profile", runner.ReadablePaths)
	}
	if len(runner.WritablePaths) != 1 || runner.WritablePaths[0] == stateDir || !strings.Contains(runner.WritablePaths[0], string(filepath.Separator)+"read-only"+string(filepath.Separator)) {
		t.Fatalf("writes = %v, want one per-worktree cache and no source profile", runner.WritablePaths)
	}
	configEnv := envValue(runner.Env, "CLAUDE_CONFIG_DIR")
	if configEnv == "" || configEnv == stateDir || !strings.HasPrefix(configEnv, runner.WritablePaths[0]+string(filepath.Separator)) {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want isolated state under %q", configEnv, runner.WritablePaths[0])
	}
	if !containsEnvPrefix(runner.Env, "GOCACHE=") || !containsEnvPrefix(runner.Env, "TMPDIR=") {
		t.Fatalf("read-only env %v lacks Go cache or temp grants", runner.Env)
	}
	base, ok := runner.Inner.(subprocess.CuratedGroupRunner)
	if !ok {
		t.Fatalf("sandbox inner = %T, want CuratedGroupRunner", runner.Inner)
	}
	if containsEnvPrefix(base.BaseEnv, "GH_TOKEN=") || containsEnvPrefix(base.BaseEnv, "OPENAI_API_KEY=") {
		t.Fatalf("curated base environment leaked credentials: %v", base.BaseEnv)
	}
}

func TestWrapReadOnlySandboxAdapterKeepsModelGatewayCredentialFree(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "review-worktree")
	sourceState := filepath.Join(t.TempDir(), "claude-state")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sourceState, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceState, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"must-not-copy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gatewayRunner := &credgw.Runner{Inner: subprocess.GroupRunner{}}
	adapter := modelGatewayRuntimeAdapter{
		Adapter: runtime.ClaudeAdapter{Runner: gatewayRunner},
		runner:  gatewayRunner,
	}
	wrapped, err := wrapReadOnlySandboxAdapter(t.TempDir(), runtime.Agent{
		Runtime:          runtime.ClaudeRuntime,
		ReadOnlySeat:     true,
		RuntimeConfigDir: sourceState,
	}, checkout, adapter)
	if err != nil {
		t.Fatal(err)
	}
	stateAdapter, ok := wrapped.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter = %T, want readOnlyRuntimeAdapter", wrapped)
	}
	gatewayAdapter, ok := stateAdapter.Adapter.(modelGatewayRuntimeAdapter)
	if !ok {
		t.Fatalf("inner adapter = %T, want modelGatewayRuntimeAdapter", stateAdapter.Adapter)
	}
	if gatewayAdapter.runner.ChildConfigDir == "" || gatewayAdapter.runner.ChildConfigDir == sourceState {
		t.Fatalf("gateway ChildConfigDir = %q, want isolated credential-free state", gatewayAdapter.runner.ChildConfigDir)
	}
	if _, err := os.Stat(filepath.Join(gatewayAdapter.runner.ChildConfigDir, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gateway child profile contains a credential: %v", err)
	}
	claude, ok := gatewayAdapter.Adapter.(runtime.ClaudeAdapter)
	if !ok {
		t.Fatalf("gateway inner adapter = %T", gatewayAdapter.Adapter)
	}
	shim, ok := claude.Runner.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("gateway sandbox runner = %T", claude.Runner)
	}
	gateway, ok := shim.Inner.(*credgw.Runner)
	if !ok {
		t.Fatalf("sandbox inner = %T, want credential gateway", shim.Inner)
	}
	if _, ok := gateway.Inner.(subprocess.CuratedGroupRunner); !ok {
		t.Fatalf("gateway inner base = %T, want curated environment", gateway.Inner)
	}
}

func TestReadOnlyRuntimeStateSurvivesRepairDeliveries(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(stateRoot, "credential")
	if err := os.WriteFile(credential, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &sandboxAdapterCaptureRunner{stdout: `{"result":"done"}`}
	adapter := readOnlyRuntimeAdapter{
		Adapter:     runtime.ClaudeAdapter{Runner: runner},
		cleanupRoot: stateRoot,
	}
	agent := runtime.Agent{
		Name: "reviewer", Role: "reviewer", Runtime: runtime.ClaudeRuntime,
		RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "owner/repo",
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}
	for delivery := 1; delivery <= 2; delivery++ {
		if _, err := adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "review"}); err != nil {
			t.Fatalf("delivery %d: %v", delivery, err)
		}
		if data, err := os.ReadFile(credential); err != nil || string(data) != "secret" {
			t.Fatalf("delivery %d lost repair state: data=%q err=%v", delivery, data, err)
		}
	}
	if err := adapter.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged runtime state survived job cleanup: %v", err)
	}
}

func TestWorkerReadOnlyRuntimeStateSurvivesMailboxRepair(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	sourceDir := t.TempDir()
	const sourceCredential = `{"claudeAiOauth":{"accessToken":"host"}}`
	if err := os.WriteFile(filepath.Join(sourceDir, ".credentials.json"), []byte(sourceCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", sourceDir)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440002", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "repair-review", Agent: "reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true,
	})
	runner := &repairStateRunner{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(agent runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		if agent.RuntimeConfigDir != sourceDir {
			t.Fatalf("worker runtime config dir = %q, want configured profile %q", agent.RuntimeConfigDir, sourceDir)
		}
		return runtime.ClaudeAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, "repair-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) || runner.calls != 2 || !runner.repairStateObserved {
		t.Fatalf("repair job state=%q calls=%d stateObserved=%v payload=%s", stored.State, runner.calls, runner.repairStateObserved, stored.Payload)
	}
	if data, err := os.ReadFile(filepath.Join(sourceDir, ".credentials.json")); err != nil || string(data) != sourceCredential {
		t.Fatalf("shared credential changed to %q, err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Dir(runner.stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated runtime state survived job boundary: %v", err)
	}
}

func TestWorkerKimiReadOnlySeatStagesProfileUnderEffectiveHome(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	sourceDir := t.TempDir()
	credentialDir := filepath.Join(sourceDir, "credentials")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const sourceCredential = `{"access_token":"host"}`
	if err := os.WriteFile(filepath.Join(credentialDir, "kimi-code.json"), []byte(sourceCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "kimi-reviewer", runtime.KimiRuntime,
		"session_550e8400-e29b-41d4-a716-446655440000", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "kimi-review", Agent: "kimi-reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, RuntimeConfigDir: sourceDir,
	})
	runner := &kimiHomeStateRunner{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return runtime.KimiAdapter{Runner: runner, Dir: checkout}, nil
	}
	job, err := store.GetJob(ctx, "kimi-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) || !runner.credentialObserved {
		t.Fatalf("Kimi job state=%q credentialObserved=%v payload=%s", stored.State, runner.credentialObserved, stored.Payload)
	}
	if data, err := os.ReadFile(filepath.Join(credentialDir, "kimi-code.json")); err != nil || string(data) != sourceCredential {
		t.Fatalf("shared Kimi credential changed to %q, err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Dir(runner.home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Kimi job-private cache survived job boundary: %v", err)
	}
}

func TestWorkerReadOnlyCleanupRemovesRenamedRuntimeState(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	sourceDir := t.TempDir()
	const sourceCredential = `{"claudeAiOauth":{"accessToken":"host"}}`
	if err := os.WriteFile(filepath.Join(sourceDir, ".credentials.json"), []byte(sourceCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440002", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "rename-state-review", Agent: "reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, RuntimeConfigDir: sourceDir,
	})
	runner := &renameRuntimeStateRunner{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return runtime.ClaudeAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, "rename-state-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) || runner.savedState == "" {
		t.Fatalf("rename-state job state=%q savedState=%q payload=%s", stored.State, runner.savedState, stored.Payload)
	}
	if _, err := os.Stat(runner.savedState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed runtime state survived job boundary: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(sourceDir, ".credentials.json")); err != nil || string(data) != sourceCredential {
		t.Fatalf("shared credential changed to %q, err=%v", data, err)
	}
}

// streamingReviewRunner is STREAM-capable on purpose: TeeRunner writes through
// the StreamRunner seam, so a buffered-only fake makes the retained transcript
// look empty even when the tee composed correctly — a fixture limitation that
// reads exactly like a lost wrapper.
type streamingReviewRunner struct {
	stdout string
	env    []string
}

func (r *streamingReviewRunner) Run(ctx context.Context, dir, command string, args ...string) (subprocess.Result, error) {
	return r.RunStream(ctx, dir, nil, command, args...)
}

func (r *streamingReviewRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	return r.RunEnvStream(ctx, dir, env, nil, command, args...)
}

func (r *streamingReviewRunner) RunStream(_ context.Context, _ string, out io.Writer, command string, args ...string) (subprocess.Result, error) {
	if out != nil {
		if _, err := io.WriteString(out, r.stdout); err != nil {
			return subprocess.Result{}, err
		}
	}
	return subprocess.Result{Command: command, Args: args, Stdout: r.stdout}, nil
}

func (r *streamingReviewRunner) RunEnvStream(ctx context.Context, dir string, env []string, out io.Writer, command string, args ...string) (subprocess.Result, error) {
	r.env = append([]string(nil), env...)
	return r.RunStream(ctx, dir, out, command, args...)
}

// The read-only review adapter composes a PID-capturing, env-injecting tee, so
// the fixture must serve that combined seam or delivery fails for a reason that
// has nothing to do with the wrappers under test.
func (r *streamingReviewRunner) RunStreamWithPID(ctx context.Context, dir string, out io.Writer, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	if onPID != nil {
		onPID(os.Getpid())
	}
	return r.RunStream(ctx, dir, out, command, args...)
}

func (r *streamingReviewRunner) RunEnvStreamWithPID(ctx context.Context, dir string, env []string, out io.Writer, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	if onPID != nil {
		onPID(os.Getpid())
	}
	return r.RunEnvStream(ctx, dir, env, out, command, args...)
}
func (r *streamingReviewRunner) LookPath(file string) (string, error) { return file, nil }

// TestWorkerReadOnlyReviewRewrapsToolCacheAndTranscript enters through the
// queued worker path: it must retain both post-sandbox wrappers around a
// stateful read-only adapter. Calling either helper directly would not prove
// the production composition order.
func TestWorkerReadOnlyReviewRewrapsToolCacheAndTranscript(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"host"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440002", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	const jobID = "read-only-rewrap-review"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: "reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, RuntimeConfigDir: sourceDir,
	})
	runner := &streamingReviewRunner{
		stdout: `{"result":"{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"reviewer adapter rewrap probe\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[]}}"}`,
	}
	var workerOutput bytes.Buffer
	worker := defaultJobWorker(store, &workerOutput, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return runtime.ClaudeAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) {
		t.Fatalf("review job state=%q payload=%s", stored.State, stored.Payload)
	}
	if cache := envValue(runner.env, "GOCACHE"); !strings.Contains(cache, filepath.Join("cache", "tools", "read-only")) {
		t.Fatalf("GOCACHE=%q, want isolated read-only tool cache", cache)
	}
	if output := workerOutput.String(); strings.Contains(output, "tool cache env inject failed") || strings.Contains(output, "transcript tee build failed") {
		t.Fatalf("worker lost a post-sandbox wrapper: %s", output)
	}
	log, err := os.ReadFile(transcript.JobLogPath(config.PathsForHome(home).Logs, jobID))
	if err != nil {
		t.Fatalf("read retained transcript: %v", err)
	}
	if !strings.Contains(string(log), "reviewer adapter rewrap probe") {
		t.Fatalf("retained transcript missing delivery output: %q", log)
	}
}

func TestForegroundReviewRuntimeStateSurvivesRepairAndCleansAtBoundary(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440002", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime,
		"true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	sourceDir := t.TempDir()
	const sourceCredential = `{"claudeAiOauth":{"accessToken":"host"}}`
	if err := os.WriteFile(filepath.Join(sourceDir, ".credentials.json"), []byte(sourceCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", sourceDir)
	head := readonlyWorktreeHead(t, checkout)
	runner := &repairStateRunner{}

	previousAdapterFactory := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(configHome string, agent runtime.Agent, deliveryCheckout string) (runtime.Adapter, error) {
		delivery, err := wrapReadOnlySandboxAdapter(configHome, agent, deliveryCheckout, runtime.ClaudeAdapter{Runner: runner})
		if err != nil {
			return nil, err
		}
		adapter, ok := delivery.(runtime.Adapter)
		if !ok {
			return nil, errors.New("foreground read-only adapter does not implement runtime.Adapter")
		}
		return adapter, nil
	}
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousAdapterFactory })
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	output, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", LeadAgent: "implementer",
		Action: "review", Instructions: "Review the exact head.", PullRequest: 7,
		Branch: "main", HeadSHA: head, Home: home,
	})
	if err != nil {
		t.Fatalf("foreground review: %v", err)
	}
	if output.Result == nil || output.Result.Decision != "approved" {
		t.Fatalf("foreground review result = %+v, want approved", output.Result)
	}
	// SCOPE, stated because it is easy to misread: toolCacheObserved pins that the
	// Landlock wrapper delivers the isolated cache env on the FOREGROUND path. It
	// does NOT pin the two rewrap switches — reverting those leaves this green,
	// verified by mutation. TestWorkerReadOnlyReviewRewrapsToolCacheAndTranscript
	// is the test that guards them.
	if runner.calls != 2 || !runner.repairStateObserved || !runner.toolCacheObserved {
		t.Fatalf("foreground repair calls=%d stateObserved=%v toolCacheObserved=%v", runner.calls, runner.repairStateObserved, runner.toolCacheObserved)
	}
	if data, err := os.ReadFile(filepath.Join(sourceDir, ".credentials.json")); err != nil || string(data) != sourceCredential {
		t.Fatalf("shared credential changed to %q, err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Dir(runner.stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreground isolated runtime state survived job boundary: %v", err)
	}
}

func TestReadOnlyRuntimeAdapterNeverPersistsStagedCredential(t *testing.T) {
	configHome := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "review-worktree")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, ".credentials.json")
	const sourceCredential = `{"claudeAiOauth":{"accessToken":"host"},"mcpOAuth":{"secret":"preserve"}}`
	if err := os.WriteFile(source, []byte(sourceCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{
		Name: "reviewer", Role: "reviewer", Runtime: runtime.ClaudeRuntime,
		RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "owner/repo",
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly, ReadOnlySeat: true,
		RuntimeConfigDir: sourceDir,
	}
	wrapped, err := wrapReadOnlySandboxAdapter(configHome, agent, checkout, runtime.ClaudeAdapter{
		Runner: &sandboxAdapterCaptureRunner{stdout: `{"result":"done"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter, ok := wrapped.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter = %T, want readOnlyRuntimeAdapter", wrapped)
	}
	staged := filepath.Join(adapter.cleanupRoot, "runtime-state", ".claude", ".credentials.json")
	if err := os.WriteFile(staged, []byte(`{"claudeAiOauth":{"accessToken":"attacker-controlled"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "review"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != sourceCredential {
		t.Fatalf("shared credential changed to %q, err=%v", data, err)
	}
	if _, err := os.Stat(adapter.cleanupRoot); err != nil {
		t.Fatalf("staged runtime state removed before job boundary: %v", err)
	}
	if err := adapter.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(adapter.cleanupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged runtime state survived job cleanup: %v", err)
	}
}

func TestStageReadOnlyRuntimeCredentialCopiesOnlyProviderSection(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, ".credentials.json")
	staged := filepath.Join(dir, "isolated", ".credentials.json")
	const sourceCredential = `{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"secret":"preserve"}}`
	if err := os.WriteFile(source, []byte(sourceCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stageReadOnlyRuntimeCredential(source, staged, "claudeAiOauth"); err != nil {
		t.Fatal(err)
	}
	stagedData, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stagedData), "mcpOAuth") || strings.Contains(string(stagedData), "preserve") {
		t.Fatalf("staged credential leaked unrelated provider state: %s", stagedData)
	}
	if !strings.Contains(string(stagedData), "claudeAiOauth") {
		t.Fatalf("staged credential lacks Claude OAuth state: %s", stagedData)
	}
	if err := os.WriteFile(staged, []byte(`{"claudeAiOauth":{"accessToken":"attacker-controlled"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != sourceCredential {
		t.Fatalf("shared credential changed to %q, err=%v", data, err)
	}
}

func TestWrapReadOnlySandboxAdapterRejectsOmpWithoutCredentialBroker(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{Runtime: runtime.OmpRuntime, ReadOnlySeat: true}
	_, err := wrapReadOnlySandboxAdapter(t.TempDir(), agent, checkout, runtime.OmpAdapter{})
	if err == nil || !strings.Contains(err.Error(), "isolated credential broker") {
		t.Fatalf("omp read-only seat error = %v", err)
	}
}

func TestApplyReadOnlySeatClearsInheritedGrants(t *testing.T) {
	for _, test := range []struct {
		name       string
		marked     bool
		wantSeat   bool
		wantConfig string
	}{
		{name: "review seat", marked: true, wantSeat: true, wantConfig: "/profiles/reviewer"},
		{name: "ask seat", marked: true, wantSeat: true, wantConfig: "/profiles/reviewer"},
		{name: "ordinary job", marked: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := runtime.Agent{
				WritablePaths: []string{"/shared/cache"},
				ReadablePaths: []string{"/host"},
				ReadableFiles: []string{"/host/secret"},
			}
			applyReadOnlySeat(test.marked, " /profiles/reviewer ", &agent)
			if agent.ReadOnlySeat != test.wantSeat || agent.RuntimeConfigDir != test.wantConfig {
				t.Fatalf("read-only marker = %v config = %q, want %v %q", agent.ReadOnlySeat, agent.RuntimeConfigDir, test.wantSeat, test.wantConfig)
			}
			if test.wantSeat && (len(agent.WritablePaths) != 0 || len(agent.ReadablePaths) != 0 || len(agent.ReadableFiles) != 0) {
				t.Fatalf("read-only seat retained configured grants: writes=%v readDirs=%v readFiles=%v", agent.WritablePaths, agent.ReadablePaths, agent.ReadableFiles)
			}
		})
	}
}

func TestSelectedRuntimeConfigDirCapturesClaudeDispatchProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/profiles/reviewer")
	if got := selectedRuntimeConfigDir(runtime.ClaudeRuntime); got != "/profiles/reviewer" {
		t.Fatalf("selectedRuntimeConfigDir = %q, want /profiles/reviewer", got)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
func envValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func containsEnvPrefix(env []string, prefix string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
