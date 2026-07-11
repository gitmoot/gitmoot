package cli

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/sandbox"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
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
	dir     string
	command string
	args    []string
	env     []string
}

func (r *sandboxAdapterCaptureRunner) Run(_ context.Context, dir, command string, args ...string) (subprocess.Result, error) {
	r.dir = dir
	r.command = command
	r.args = append([]string(nil), args...)
	return subprocess.Result{Command: command, Args: args, Stdout: r.stdout}, nil
}

func (r *sandboxAdapterCaptureRunner) RunEnv(_ context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	r.env = append([]string(nil), env...)
	return r.Run(context.Background(), dir, command, args...)
}

func (r *sandboxAdapterCaptureRunner) LookPath(file string) (string, error) { return file, nil }

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
			agent := runtime.Agent{Name: "p", Role: "producer", Runtime: tc.runtime, RuntimeRef: ref, RepoScope: "owner/repo", AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite, WritablePaths: []string{"/data/out"}}
			wrapped, err := wrapProduceSandboxAdapter("produce", agent, tc.adapter(capture))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wrapped.Deliver(context.Background(), agent, runtime.Job{Prompt: "write"}); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			wantPrefix := []string{"sandbox-exec", "--write", "/data/out"}
			if tc.runtime == runtime.ClaudeRuntime {
				wantPrefix = append(wantPrefix, "--write", home+"/.claude", "--write", home+"/.cache/claude-cli-nodejs")
				if !reflect.DeepEqual(capture.env, []string{"CLAUDE_CONFIG_DIR=" + home + "/.claude"}) {
					t.Fatalf("Claude sandbox env = %v", capture.env)
				}
			} else {
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
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, WritablePaths: []string{"/data"}}
	base := runtime.ClaudeAdapter{Runner: subprocess.TeeRunner{Inner: subprocess.GroupRunner{}}}
	wrapped, err := wrapProduceSandboxAdapter("produce", agent, base)
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
	wantPaths := []string{"/data", home + "/.claude", home + "/.cache/claude-cli-nodejs"}
	if !reflect.DeepEqual(shim.WritablePaths, wantPaths) || !reflect.DeepEqual(shim.Env, []string{"CLAUDE_CONFIG_DIR=" + home + "/.claude"}) {
		t.Fatalf("Claude shim = paths %v env %v, want %v + config env", shim.WritablePaths, shim.Env, wantPaths)
	}

	nonProduce, err := wrapProduceSandboxAdapter("ask", agent, base)
	if err != nil || !reflect.DeepEqual(nonProduce, base) {
		t.Fatalf("non-produce adapter changed: %T %+v, err=%v", nonProduce, nonProduce, err)
	}
	codexBase := runtime.CodexAdapter{Runner: subprocess.GroupRunner{}}
	codex, err := wrapProduceSandboxAdapter("produce", runtime.Agent{Runtime: runtime.CodexRuntime, WritablePaths: []string{"/data"}}, codexBase)
	if err != nil || !reflect.DeepEqual(codex, codexBase) {
		t.Fatalf("Codex adapter changed: %T %+v, err=%v", codex, codex, err)
	}
}
