package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func TestImplementWritePolicyError(t *testing.T) {
	implement := []string{"ask", "review", "implement"}
	readOnlyCaps := []string{"ask", "review"}
	for _, tc := range []struct {
		name         string
		capabilities []string
		policy       string
		wantErr      bool
	}{
		{"implement + empty refused", implement, "", true},
		{"implement + auto refused", implement, AutonomyPolicyAuto, true},
		{"implement + read-only refused", implement, AutonomyPolicyReadOnly, true},
		{"implement + workspace-write allowed", implement, AutonomyPolicyWorkspaceWrite, false},
		{"implement + danger-full-access allowed", implement, AutonomyPolicyDangerFullAccess, false},
		{"no implement + auto allowed", readOnlyCaps, AutonomyPolicyAuto, false},
		{"no implement + read-only allowed", readOnlyCaps, AutonomyPolicyReadOnly, false},
		{"no implement + empty allowed", readOnlyCaps, "", false},
	} {
		err := ImplementWritePolicyError(tc.capabilities, tc.policy)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: expected an error, got nil", tc.name)
			}
			for _, fragment := range []string{"danger-full-access", "workspace-write", "implement"} {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("%s: error %q must mention %q", tc.name, err.Error(), fragment)
				}
			}
		} else if err != nil {
			t.Fatalf("%s: expected no error, got %v", tc.name, err)
		}
	}
}

func TestPolicyGrantsImplementWrite(t *testing.T) {
	for policy, want := range map[string]bool{
		"":                             false,
		AutonomyPolicyAuto:             false,
		AutonomyPolicyReadOnly:         false,
		AutonomyPolicyWorkspaceWrite:   true,
		AutonomyPolicyDangerFullAccess: true,
		"bogus":                        false,
	} {
		if got := PolicyGrantsImplementWrite(policy); got != want {
			t.Fatalf("PolicyGrantsImplementWrite(%q) = %v, want %v", policy, got, want)
		}
	}
}

func TestValidateAgent(t *testing.T) {
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: OmpRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000", RepoScope: "gitmoot/gitmoot"}
	if err := ValidateAgent(agent); err != nil {
		t.Fatalf("ValidateAgent returned error: %v", err)
	}
	for _, policy := range []string{AutonomyPolicyAuto, AutonomyPolicyReadOnly, AutonomyPolicyWorkspaceWrite, AutonomyPolicyDangerFullAccess} {
		agent.AutonomyPolicy = policy
		if err := ValidateAgent(agent); err != nil {
			t.Fatalf("ValidateAgent rejected policy %q: %v", policy, err)
		}
	}
	agent.AutonomyPolicy = "manual"
	if err := ValidateAgent(agent); err == nil {
		t.Fatal("ValidateAgent accepted unsupported autonomy policy")
	}
	agent.AutonomyPolicy = ""
	agent.RepoScope = ""
	if err := ValidateAgent(agent); err != nil {
		t.Fatalf("ValidateAgent rejected global agent without repo scope: %v", err)
	}
	agent.RepoScope = "gitmoot/gitmoot"

	agent.Runtime = "unknown"
	if err := ValidateAgent(agent); err == nil {
		t.Fatal("ValidateAgent accepted unsupported runtime")
	}

	for _, removed := range []string{"codex", "claude", "kimi"} {
		agent := Agent{Name: "legacy", Role: "reviewer", Runtime: removed, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000", RepoScope: "gitmoot/gitmoot"}
		err := ValidateAgent(agent)
		if !errors.Is(err, ErrRemovedRuntime) || !strings.Contains(err.Error(), "requires omp") {
			t.Fatalf("ValidateAgent(%s) = %v, want a removed-runtime refusal naming omp", removed, err)
		}
	}

	for _, scope := range []string{"owner/", "/repo", "owner/repo/extra"} {
		agent := Agent{Name: "audit", Role: "reviewer", Runtime: OmpRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000", RepoScope: scope}
		if err := ValidateAgent(agent); err == nil {
			t.Fatalf("ValidateAgent accepted malformed repo scope %q", scope)
		}
	}
}

func TestAdapterValidateRejectsRuntimeMismatch(t *testing.T) {
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000", RepoScope: "gitmoot/gitmoot"}
	if err := (OmpAdapter{}).Validate(context.Background(), agent); err == nil {
		t.Fatal("OmpAdapter accepted a shell agent")
	}
}

// TestEffectiveModelResolutionOrder locks the exact #652 precedence directly at the
// resolution function: job --model > agent --model > runtime default_model > "".
func TestEffectiveModelResolutionOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agentModel string
		jobModel   string
		rtDefault  string
		want       string
	}{
		{"job wins over all", "sonnet", "opus", "gpt-5.5", "opus"},
		{"agent wins over rt default", "sonnet", "", "gpt-5.5", "sonnet"},
		{"rt default when neither pins", "", "", "gpt-5.5", "gpt-5.5"},
		{"empty when nothing set (byte-identical)", "", "", "", ""},
		{"rt default trimmed", "", "", "  gpt-5.5  ", "gpt-5.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveModel(Agent{Model: tc.agentModel}, Job{Model: tc.jobModel, RuntimeDefaultModel: tc.rtDefault})
			if got != tc.want {
				t.Fatalf("EffectiveModel(agent=%q, job=%q, rtDefault=%q) = %q, want %q", tc.agentModel, tc.jobModel, tc.rtDefault, got, tc.want)
			}
		})
	}
}

func TestEffectiveEffortResolutionOrder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		agentEffort string
		jobEffort   string
		rtDefault   string
		want        string
	}{
		{"job wins over all", "medium", "xhigh", "low", "xhigh"},
		{"agent wins over runtime default", "high", "", "low", "high"},
		{"runtime default when neither pins", "", "", "medium", "medium"},
		{"empty when nothing set", "", "", "", ""},
		{"runtime default trimmed", "", "", "  high  ", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveEffort(Agent{Effort: tc.agentEffort}, Job{Effort: tc.jobEffort, RuntimeDefaultEffort: tc.rtDefault})
			if got != tc.want {
				t.Fatalf("effectiveEffort(agent=%q, job=%q, rtDefault=%q) = %q, want %q", tc.agentEffort, tc.jobEffort, tc.rtDefault, got, tc.want)
			}
		})
	}
}

func TestShellDeliverCommand(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "ok\n"}}}
	adapter := ShellAdapter{Runner: runner}
	agent := Agent{Name: "custom", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "hello"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "ok" {
		t.Fatalf("summary = %q", result.Summary)
	}
	runner.want(t, 0, "sh", "-c", "printf ok", "gitmoot", "hello")
}

// envFakeRunner is a fakeRunner that also implements subprocess.EnvRunner,
// recording the extra environment passed to RunEnv.
type envFakeRunner struct {
	fakeRunner
	gotEnv []string
}

func (f *envFakeRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	f.gotEnv = append([]string(nil), env...)
	return f.fakeRunner.Run(ctx, dir, command, args...)
}

func TestShellDeliverInjectsJobEnvironment(t *testing.T) {
	runner := &envFakeRunner{fakeRunner: fakeRunner{results: []subprocess.Result{{Stdout: "ok\n"}}}}
	adapter := ShellAdapter{Runner: runner}
	agent := Agent{Name: "custom", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot"}
	env := []string{"GITMOOT_TRIGGER_BODY=first\n第二", "GITMOOT_TRIGGER_SUBJECT=Snowman ☃"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "hello", ShellEnv: env}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if !reflect.DeepEqual(runner.gotEnv, env) {
		t.Fatalf("RunEnv environment = %#v, want %#v", runner.gotEnv, env)
	}
	runner.want(t, 0, "sh", "-c", "printf ok", "gitmoot", "hello")
}

func TestShellStartUnsupported(t *testing.T) {
	adapter := ShellAdapter{}
	agent := Agent{Name: "custom", Role: "reviewer", Runtime: ShellRuntime, RepoScope: "gitmoot/gitmoot"}

	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err == nil {
		t.Fatal("Start accepted shell runtime")
	}
}

func TestShellHealthRunsProbe(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit 1")}}
	adapter := ShellAdapter{Runner: runner}
	agent := Agent{Name: "custom", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: "exit 1", RepoScope: "gitmoot/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err == nil {
		t.Fatal("Health accepted failing shell command")
	}
	runner.want(t, 0, "sh", "-c", "exit 1", "gitmoot-health", healthPrompt)
}

type fakeRunner struct {
	results []subprocess.Result
	errs    []error
	calls   [][]string
}

type agentEnvRunner struct {
	results  []subprocess.Result
	errs     []error
	runCalls [][]string
	envCalls []struct {
		env  []string
		argv []string
	}
}

func (r *agentEnvRunner) result(command string, args []string, index int) (subprocess.Result, error) {
	result := subprocess.Result{Command: command, Args: args}
	if index < len(r.results) {
		result = r.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(r.errs) {
		err = r.errs[index]
	}
	return result, err
}

func (r *agentEnvRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	argv := append([]string{command}, args...)
	r.runCalls = append(r.runCalls, argv)
	return r.result(command, args, len(r.runCalls)+len(r.envCalls)-1)
}

func (r *agentEnvRunner) RunEnv(_ context.Context, _ string, env []string, command string, args ...string) (subprocess.Result, error) {
	argv := append([]string{command}, args...)
	r.envCalls = append(r.envCalls, struct {
		env  []string
		argv []string
	}{env: append([]string(nil), env...), argv: argv})
	return r.result(command, args, len(r.runCalls)+len(r.envCalls)-1)
}

func (r *agentEnvRunner) LookPath(file string) (string, error) {
	return "/usr/bin/" + file, nil
}

func TestManagedRuntimeDeliveriesThreadAgentEnv(t *testing.T) {
	const session = "550e8400-e29b-41d4-a716-446655440002"
	wantEnv := []string{"PROXY_KEY=gitmoot-kc-agent-test", "GITMOOT_PROXY_PROXY_KEY_URL=http://127.0.0.1:1234/lease"}
	tests := []struct {
		name   string
		output string
		agent  Agent
		build  func(subprocess.Runner) Adapter
	}{
		{
			name: "omp", output: ompStreamOK,
			agent: Agent{Name: "seat", Role: "reviewer", Runtime: OmpRuntime, RuntimeRef: session, RepoScope: "gitmoot/gitmoot"},
			build: func(r subprocess.Runner) Adapter { return OmpAdapter{Runner: r} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withEnv := &agentEnvRunner{results: []subprocess.Result{{Stdout: tt.output}}}
			if _, err := tt.build(withEnv).Deliver(context.Background(), tt.agent, Job{Prompt: "work", AgentEnv: wantEnv}); err != nil {
				t.Fatalf("Deliver with AgentEnv: %v", err)
			}
			if len(withEnv.runCalls) != 0 || len(withEnv.envCalls) != 1 {
				t.Fatalf("runner calls: Run=%#v RunEnv=%#v", withEnv.runCalls, withEnv.envCalls)
			}
			if !reflect.DeepEqual(withEnv.envCalls[0].env, wantEnv) {
				t.Fatalf("RunEnv env = %#v, want %#v", withEnv.envCalls[0].env, wantEnv)
			}

			withoutEnv := &agentEnvRunner{results: []subprocess.Result{{Stdout: tt.output}}}
			if _, err := tt.build(withoutEnv).Deliver(context.Background(), tt.agent, Job{Prompt: "work"}); err != nil {
				t.Fatalf("Deliver without AgentEnv: %v", err)
			}
			if len(withoutEnv.runCalls) != 1 || len(withoutEnv.envCalls) != 0 {
				t.Fatalf("empty AgentEnv changed runner path: Run=%#v RunEnv=%#v", withoutEnv.runCalls, withoutEnv.envCalls)
			}
		})
	}
}

type pidFakeRunner struct {
	*fakeRunner
	pid int
}

func (f *pidFakeRunner) RunWithPID(ctx context.Context, dir string, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	if onPID != nil {
		onPID(f.pid)
	}
	return f.Run(ctx, dir, command, args...)
}

func TestDeliverReportsPIDWhenRunnerSupportsCapability(t *testing.T) {
	const wantPID = 424242
	tests := []struct {
		name    string
		adapter Adapter
		agent   Agent
	}{
		{
			name:    "omp",
			adapter: OmpAdapter{Runner: &pidFakeRunner{fakeRunner: &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}, pid: wantPID}},
			agent:   Agent{Name: "omp", Role: "implementer", Runtime: OmpRuntime, RuntimeRef: FreshRefForJob("pid-omp"), RepoScope: "gitmoot/gitmoot"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPID := 0
			if _, err := tc.adapter.Deliver(context.Background(), tc.agent, Job{
				Prompt: "work",
				OnPID:  func(pid int) { gotPID = pid },
			}); err != nil {
				t.Fatalf("Deliver returned error: %v", err)
			}
			if gotPID != wantPID {
				t.Fatalf("captured PID = %d, want %d", gotPID, wantPID)
			}
		})
	}
}

func TestDeliverWithoutPIDCapabilityPreservesBehavior(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
	adapter := OmpAdapter{Runner: runner}
	agent := Agent{Name: "omp", Role: "implementer", Runtime: OmpRuntime, RuntimeRef: FreshRefForJob("no-pid-omp"), RepoScope: "gitmoot/gitmoot"}
	gotPID := 0
	result, err := adapter.Deliver(context.Background(), agent, Job{
		Prompt: "work",
		OnPID:  func(pid int) { gotPID = pid },
	})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "done" || gotPID != 0 {
		t.Fatalf("Deliver result = %+v, captured PID = %d; want unchanged result and no PID", result, gotPID)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
}

func (f *fakeRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	call := append([]string{command}, args...)
	f.calls = append(f.calls, call)
	index := len(f.calls) - 1
	result := subprocess.Result{Command: command, Args: args}
	if index < len(f.results) {
		result = f.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return result, err
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	if file == "" {
		return "", errors.New("empty file")
	}
	return "/usr/bin/" + file, nil
}

func (f *fakeRunner) want(t *testing.T, index int, want ...string) {
	t.Helper()
	if index >= len(f.calls) {
		t.Fatalf("missing call %d; calls=%v", index, f.calls)
	}
	if !reflect.DeepEqual(f.calls[index], want) {
		t.Fatalf("call %d = %s\nwant %s", index, strings.Join(f.calls[index], " "), strings.Join(want, " "))
	}
}
