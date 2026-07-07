package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/subprocess"
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
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000", RepoScope: "jerryfane/gitmoot"}
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
	agent.RepoScope = "jerryfane/gitmoot"

	agent.Runtime = "unknown"
	if err := ValidateAgent(agent); err == nil {
		t.Fatal("ValidateAgent accepted unsupported runtime")
	}

	agent = Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "session-name", RepoScope: "jerryfane/gitmoot"}
	if err := ValidateAgent(agent); err != nil {
		t.Fatalf("ValidateAgent rejected Codex runtime name: %v", err)
	}

	agent = Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "session-name", RepoScope: "jerryfane/gitmoot"}
	if err := ValidateAgent(agent); err == nil {
		t.Fatal("ValidateAgent accepted non-UUID Claude runtime ref")
	}

	for _, scope := range []string{"owner/", "/repo", "owner/repo/extra"} {
		agent := Agent{Name: "audit", Role: "reviewer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000", RepoScope: scope}
		if err := ValidateAgent(agent); err == nil {
			t.Fatalf("ValidateAgent accepted malformed repo scope %q", scope)
		}
	}
}

func TestAdapterValidateRejectsRuntimeMismatch(t *testing.T) {
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000", RepoScope: "jerryfane/gitmoot"}
	if err := (CodexAdapter{}).Validate(context.Background(), agent); err == nil {
		t.Fatal("CodexAdapter accepted a Claude agent")
	}
}

func TestCodexDeliverCommand(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review this"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Raw != "done" {
		t.Fatalf("raw = %q", result.Raw)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", "review this")
}

func TestCodexDeliverCommandUsesJobModel(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", Model: "agent-default"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review this", Model: "opus"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "--model", "opus", "550e8400-e29b-41d4-a716-446655440001", "--", "review this")
}

func TestCodexDeliverCommandFallsBackToAgentModel(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", Model: "sonnet"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review this"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "--model", "sonnet", "550e8400-e29b-41d4-a716-446655440001", "--", "review this")
}

// TestCodexDeliverCommandFallsBackToRuntimeDefaultModel proves the #652 behavioral
// registry default: with NO agent --model and NO job --model, a delivered job runs
// on Job.RuntimeDefaultModel (the runtime's configured default_model, threaded in
// by the dispatch layer). This is the real resolution seam — the --model arg the
// runtime CLI actually receives.
func TestCodexDeliverCommandFallsBackToRuntimeDefaultModel(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review this", RuntimeDefaultModel: "gpt-5.5"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "--model", "gpt-5.5", "550e8400-e29b-41d4-a716-446655440001", "--", "review this")
}

// TestCodexDeliverCommandJobModelWinsOverRuntimeDefault proves a job --model pin
// still WINS over the registry default_model (#652 resolution order).
func TestCodexDeliverCommandJobModelWinsOverRuntimeDefault(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review this", Model: "opus", RuntimeDefaultModel: "gpt-5.5"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "--model", "opus", "550e8400-e29b-41d4-a716-446655440001", "--", "review this")
}

// TestCodexDeliverCommandAgentModelWinsOverRuntimeDefault proves an agent --model
// pin still WINS over the registry default_model when the job pins none (#652).
func TestCodexDeliverCommandAgentModelWinsOverRuntimeDefault(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", Model: "sonnet"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review this", RuntimeDefaultModel: "gpt-5.5"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "--model", "sonnet", "550e8400-e29b-41d4-a716-446655440001", "--", "review this")
}

// TestCodexDeliverCommandNoRuntimeDefaultIsByteIdentical proves the byte-identical
// default: an EMPTY RuntimeDefaultModel (the built-in default for every runtime, and
// the value when no config sets it) forces NO --model arg — exactly as before #652.
func TestCodexDeliverCommandNoRuntimeDefaultIsByteIdentical(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review this", RuntimeDefaultModel: ""}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", "review this")
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
			got := effectiveModel(Agent{Model: tc.agentModel}, Job{Model: tc.jobModel, RuntimeDefaultModel: tc.rtDefault})
			if got != tc.want {
				t.Fatalf("effectiveModel(agent=%q, job=%q, rtDefault=%q) = %q, want %q", tc.agentModel, tc.jobModel, tc.rtDefault, got, tc.want)
			}
		})
	}
}

func TestCodexStartCommandUsesAgentModel(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440009"}` + "\n"}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RepoScope: "jerryfane/gitmoot", Model: "opus"}

	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--model", "opus", "--json", "--", "initialize")
}

func TestCodexStartCommandParsesThreadID(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "not json\n" + `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440009"}` + "\n"}}}
	adapter := CodexAdapter{Runner: runner, Dir: "/repo"}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"})

	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if result.RuntimeRef != "550e8400-e29b-41d4-a716-446655440009" {
		t.Fatalf("runtime ref = %q", result.RuntimeRef)
	}
	runner.want(t, 0, "codex", "exec", "--json", "--", "initialize")
}

func TestCodexStartCommandAppliesAutonomyPolicy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		policy  string
		sandbox string
	}{
		{name: "read_only", policy: AutonomyPolicyReadOnly, sandbox: "read-only"},
		{name: "workspace_write", policy: AutonomyPolicyWorkspaceWrite, sandbox: "workspace-write"},
		{name: "danger_full_access", policy: AutonomyPolicyDangerFullAccess, sandbox: "danger-full-access"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: "not json\n" + `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440009"}` + "\n"}}}
			adapter := CodexAdapter{Runner: runner}
			agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RepoScope: "jerryfane/gitmoot", AutonomyPolicy: tt.policy}

			if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err != nil {
				t.Fatalf("Start returned error: %v", err)
			}
			runner.want(t, 0, "codex", "exec", "--sandbox", tt.sandbox, "--json", "--", "initialize")
		})
	}
}

func TestCodexStartRejectsMissingThreadID(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"type":"turn.completed"}` + "\n"}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err == nil {
		t.Fatal("Start accepted output without thread id")
	}
	runner.want(t, 0, "codex", "exec", "--json", "--", "initialize")
}

// TestCodexStartUnwrapsJSONTranscript pins the #724 fix: codex Start runs `codex
// exec --json` (banner + thread.started + turn events + agent_message JSONL) and
// must surface the agent_message .text as Raw — NOT the whole transcript — so
// forked-session consumers that parse Raw as JSON (skillopt synth/ab) see the
// assistant's actual answer. Streams with no agent_message (older CLI, plain-text
// fallback) fail-open to the full stdout. RuntimeRef stays the parsed thread id
// in every case.
func TestCodexStartUnwrapsJSONTranscript(t *testing.T) {
	const threadID = "019f3041-cfed-7e82-8766-b5ca75cf92da"
	started := `{"type":"thread.started","thread_id":"` + threadID + `"}`
	// The assistant's final message is itself a JSON object — the shape skillopt
	// synth's challenger returns.
	inner := `{"context":"...","question":"2+2?","rubric":"exact match"}`
	// A realistic codex exec --json transcript: thread banner, turn start,
	// reasoning noise, the agent_message, and the turn.completed usage line.
	transcript := started + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"thinking hard about arithmetic"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":` + strconv.Quote(inner) + `}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":34}}`

	// A transcript that never emits an agent_message (only the thread.started the
	// thread-id parser needs) must fail-open to the full stdout.
	noMessage := started + "\n" + `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}`

	for _, tt := range []struct {
		name    string
		stdout  string
		wantRaw string
	}{
		{name: "unwraps agent_message text", stdout: transcript, wantRaw: inner},
		{name: "joins multiple agent_messages", stdout: started + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}` + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","text":"second"}}`,
			wantRaw: "first\n\nsecond"},
		{name: "no agent_message falls back to stdout", stdout: noMessage, wantRaw: noMessage},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: tt.stdout}}}
			adapter := CodexAdapter{Runner: runner}
			agent := Agent{Name: "challenger", Role: "reviewer", Runtime: CodexRuntime, RepoScope: "jerryfane/gitmoot"}

			result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "generate"})
			if err != nil {
				t.Fatalf("Start returned error: %v", err)
			}
			if result.Raw != tt.wantRaw {
				t.Fatalf("Raw = %q, want %q", result.Raw, tt.wantRaw)
			}
			// The unwrap never disturbs the parsed thread id.
			if result.RuntimeRef != threadID {
				t.Fatalf("RuntimeRef = %q, want %q", result.RuntimeRef, threadID)
			}
		})
	}
}

func TestCodexDeliverLastSessionCommand(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "continue"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "--last", "--", "continue")
}

func TestCodexDeliverCommandAppliesAutonomyPolicy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		policy  string
		sandbox string
	}{
		{name: "read_only", policy: AutonomyPolicyReadOnly, sandbox: "read-only"},
		{name: "workspace_write", policy: AutonomyPolicyWorkspaceWrite, sandbox: "workspace-write"},
		{name: "danger_full_access", policy: AutonomyPolicyDangerFullAccess, sandbox: "danger-full-access"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
			adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
			agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: tt.policy}

			if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
				t.Fatalf("Deliver returned error: %v", err)
			}
			runner.want(t, 0, "codex", "exec", "--sandbox", tt.sandbox, "--json", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", "review")
		})
	}
}

func TestCodexDeliverVerifiesNamedSession(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "review-thread", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "review-thread", "--", "review")
}

func TestCodexDeliverRejectsMissingNamedSession(t *testing.T) {
	runner := &fakeRunner{}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "missing-thread", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err == nil {
		t.Fatal("Deliver accepted missing codex session")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called before session verification: %v", runner.calls)
	}
}

func TestCodexDeliverAllowsMissingUUIDSessionToReachCodex(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", "review")
}

func TestCodexHealthUsesRegisteredSession(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "OK"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", healthPrompt)
}

func TestCodexHealthRejectsBrokenSession(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit 1")}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err == nil {
		t.Fatal("Health accepted broken codex session")
	}
	runner.want(t, 0, "codex", "exec", "--json", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", healthPrompt)
}

func TestClaudeDeliverCommand(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"done"}`}}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("summary = %q", result.Summary)
	}
	runner.want(t, 0, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
}

func TestClaudeDeliverCommandUsesJobModel(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"done"}`}}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot", Model: "agent-default"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review", Model: "opus"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "claude", "--model", "opus", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
}

func TestClaudeDeliverCommandFallsBackToAgentModel(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"done"}`}}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot", Model: "sonnet"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "claude", "--model", "sonnet", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
}

func TestIsClaudeSessionMissing(t *testing.T) {
	missing := subprocess.Result{Stderr: "No conversation found with session ID: 550e8400-e29b-41d4-a716-446655440002"}
	if !isClaudeSessionMissing(missing) {
		t.Fatal("expected dead-session stderr to be classified as session-missing")
	}
	// Pin the disjointness invariant in both directions: the canonical
	// session-missing sample must NOT be classified as an auth failure, so the
	// self-heal path can never be entered for (or suppressed by) an auth error.
	if isClaudeAuthFailure(missing) {
		t.Fatal("session-missing sample must NOT be classified as an auth failure")
	}
	for name, result := range map[string]subprocess.Result{
		"auth via stderr 401": {Stderr: `{"error":{"type":"authentication_error","message":"401 Invalid authentication credentials"}}`},
		"auth via type":       {Stderr: "authentication_error"},
		"generic non-zero":    {Stderr: "fatal: some other failure", Stdout: "partial work"},
		"empty":               {},
	} {
		if isClaudeSessionMissing(result) {
			t.Fatalf("%s must NOT be classified as session-missing", name)
		}
		// Guard the invariant that auth and session-missing are disjoint classes.
		if name == "auth via stderr 401" && !isClaudeAuthFailure(result) {
			t.Fatalf("%s should still be an auth failure", name)
		}
	}
}

func TestClaudeDeliverSelfHealsDeadSession(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "No conversation found with session ID: 550e8400-e29b-41d4-a716-446655440002"},
			{Stdout: `{"result":"done"}`},
		},
		errs: []error{errors.New("exit 1"), nil},
	}
	adapter := ClaudeAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440099", nil
		},
	}
	agent := Agent{Name: "shipper", Role: "implementer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if result.RefreshedRuntimeRef != "550e8400-e29b-41d4-a716-446655440099" {
		t.Fatalf("RefreshedRuntimeRef = %q, want the fresh UUID", result.RefreshedRuntimeRef)
	}
	// A #443 dead-session self-heal re-pin is NOT ephemeral (#665): the mailbox
	// MUST persist it — the old pinned session is genuinely gone.
	if result.SessionEphemeral {
		t.Fatalf("SessionEphemeral = true, want false for a #443 self-heal re-pin (it must be persisted)")
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected exactly 2 runner calls (single bounded retry), got %d: %v", len(runner.calls), runner.calls)
	}
	runner.want(t, 0, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
	runner.want(t, 1, "claude", "--session-id", "550e8400-e29b-41d4-a716-446655440099", "-p", "--output-format", "json", "--", "review")
}

func TestClaudeDeliverSelfHealUnrecoverable(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "No conversation found with session ID: 550e8400-e29b-41d4-a716-446655440002"},
			{Stderr: "claude: internal error starting session"},
		},
		errs: []error{errors.New("exit 1"), errors.New("exit 1")},
	}
	adapter := ClaudeAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440099", nil
		},
	}
	agent := Agent{Name: "shipper", Role: "implementer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	_, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})

	if err == nil {
		t.Fatal("Deliver accepted an unrecoverable dead session")
	}
	errText := err.Error()
	for _, want := range []string{`agent "shipper"`, "gitmoot agent restart", "--session last"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("actionable error missing %q:\n%s", want, errText)
		}
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected exactly 2 runner calls (single bounded retry), got %d: %v", len(runner.calls), runner.calls)
	}
}

func TestClaudeDeliverSelfHealAuthFailureStaysAuth(t *testing.T) {
	// If the fresh start fails for an auth reason, surface it as auth — never mask
	// a genuine auth failure behind the stale-session remediation.
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "No conversation found with session ID: 550e8400-e29b-41d4-a716-446655440002"},
			{Stderr: `{"error":{"type":"authentication_error","message":"401 Invalid authentication credentials"}}`},
		},
		errs: []error{errors.New("exit 1"), errors.New("exit 1")},
	}
	adapter := ClaudeAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440099", nil
		},
	}
	agent := Agent{Name: "shipper", Role: "implementer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	_, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err == nil {
		t.Fatal("Deliver accepted an auth failure on the fresh start")
	}
	if !strings.Contains(err.Error(), "Claude Code authentication failed") {
		t.Fatalf("fresh-start auth failure should surface as auth, got:\n%s", err.Error())
	}
}

func TestClaudeDeliverDoesNotHealLastSession(t *testing.T) {
	// A "last" (--continue) agent must never trigger a fresh --session-id start,
	// even on a generic failure.
	called := false
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "No conversation found with session ID: anything"}},
		errs:    []error{errors.New("exit 1")},
	}
	adapter := ClaudeAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			called = true
			return "550e8400-e29b-41d4-a716-446655440099", nil
		},
	}
	agent := Agent{Name: "researcher", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	_, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err == nil {
		t.Fatal("expected the underlying failure to surface")
	}
	if called {
		t.Fatal("self-heal must not mint a fresh session for a last/--continue agent")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly 1 runner call, got %d: %v", len(runner.calls), runner.calls)
	}
	runner.want(t, 0, "claude", "--continue", "-p", "--output-format", "json", "--", "review")
}

func TestClaudeStartCommandUsesAgentModel(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"ready"}`}}}
	adapter := ClaudeAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440010", nil
		},
	}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RepoScope: "jerryfane/gitmoot", Model: "opus"}

	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	runner.want(t, 0, "claude", "--model", "opus", "--session-id", "550e8400-e29b-41d4-a716-446655440010", "-p", "--output-format", "json", "--", "initialize")
}

func TestClaudeStartCommandUsesGeneratedSessionID(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"ready"}`}}}
	adapter := ClaudeAdapter{
		Runner: runner,
		Dir:    "/repo",
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440010", nil
		},
	}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"})

	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if result.RuntimeRef != "550e8400-e29b-41d4-a716-446655440010" {
		t.Fatalf("runtime ref = %q", result.RuntimeRef)
	}
	runner.want(t, 0, "claude", "--session-id", "550e8400-e29b-41d4-a716-446655440010", "-p", "--output-format", "json", "--", "initialize")
}

// TestClaudeStartUnwrapsJSONEnvelope pins the #721 fix: Start must surface the
// envelope's inner "result" text as Raw (not the whole CLI JSON envelope) so
// forked-session consumers that parse Raw as JSON (skillopt synth/ab) see the
// assistant's actual answer, while corrupt/non-JSON/empty envelopes fail-open to
// the raw stdout.
func TestClaudeStartUnwrapsJSONEnvelope(t *testing.T) {
	// A realistic claude --output-format json envelope whose inner result is
	// itself a JSON object (the shape skillopt synth's challenger returns).
	inner := `{"context":"...","question":"2+2?","rubric":"exact match"}`
	envelope := `{"type":"result","subtype":"success","is_error":false,` +
		`"session_id":"550e8400-e29b-41d4-a716-446655440010",` +
		`"result":` + strconv.Quote(inner) + `,` +
		`"usage":{"input_tokens":12,"output_tokens":34}}`

	for _, tt := range []struct {
		name    string
		stdout  string
		wantRaw string
	}{
		{name: "unwraps inner result", stdout: envelope, wantRaw: inner},
		{name: "plain answer envelope", stdout: `{"type":"result","result":"the answer"}`, wantRaw: "the answer"},
		{name: "non-json falls back to stdout", stdout: "not json at all", wantRaw: "not json at all"},
		{name: "empty result field falls back to stdout", stdout: `{"type":"result","result":""}`, wantRaw: `{"type":"result","result":""}`},
		{name: "envelope without result field falls back", stdout: `{"type":"result","subtype":"error"}`, wantRaw: `{"type":"result","subtype":"error"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: tt.stdout}}}
			adapter := ClaudeAdapter{
				Runner: runner,
				NewRuntimeRef: func() (string, error) {
					return "550e8400-e29b-41d4-a716-446655440010", nil
				},
			}
			agent := Agent{Name: "challenger", Role: "reviewer", Runtime: ClaudeRuntime, RepoScope: "jerryfane/gitmoot"}

			result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "generate"})
			if err != nil {
				t.Fatalf("Start returned error: %v", err)
			}
			if result.Raw != tt.wantRaw {
				t.Fatalf("Raw = %q, want %q", result.Raw, tt.wantRaw)
			}
			// RuntimeRef behavior stays identical regardless of unwrap outcome.
			if result.RuntimeRef != "550e8400-e29b-41d4-a716-446655440010" {
				t.Fatalf("RuntimeRef = %q, want the generated ref", result.RuntimeRef)
			}
		})
	}
}

func TestClaudeStartCommandAppliesAutonomyPolicy(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy string
		mode   string
	}{
		{name: "read_only", policy: AutonomyPolicyReadOnly, mode: "plan"},
		{name: "workspace_write", policy: AutonomyPolicyWorkspaceWrite, mode: "acceptEdits"},
		{name: "danger_full_access", policy: AutonomyPolicyDangerFullAccess, mode: "bypassPermissions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"ready"}`}}}
			adapter := ClaudeAdapter{
				Runner: runner,
				NewRuntimeRef: func() (string, error) {
					return "550e8400-e29b-41d4-a716-446655440010", nil
				},
			}
			agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RepoScope: "jerryfane/gitmoot", AutonomyPolicy: tt.policy}

			if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err != nil {
				t.Fatalf("Start returned error: %v", err)
			}
			runner.want(t, 0, "claude", "--permission-mode", tt.mode, "--session-id", "550e8400-e29b-41d4-a716-446655440010", "-p", "--output-format", "json", "--", "initialize")
		})
	}
}

func TestClaudeStartDoesNotStoreSessionIDWhenCommandFails(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "failed"}},
		errs:    []error{errors.New("exit 1")},
	}
	adapter := ClaudeAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440010", nil
		},
	}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err == nil {
		t.Fatal("Start accepted failed claude command")
	}
	runner.want(t, 0, "claude", "--session-id", "550e8400-e29b-41d4-a716-446655440010", "-p", "--output-format", "json", "--", "initialize")
}

func TestClaudeStartClassifiesAuthFailure(t *testing.T) {
	raw := `{"error":{"type":"authentication_error","message":"401 Invalid authentication credentials"}}`
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: raw}},
		errs:    []error{errors.New("exit 1")},
	}
	adapter := ClaudeAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440010", nil
		},
	}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"})

	if err == nil {
		t.Fatal("Start accepted auth failure")
	}
	errText := err.Error()
	for _, want := range []string{"Claude Code authentication failed", ClaudeSessionAuthFailedMessage, "401 Invalid authentication credentials"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("error missing %q:\n%s", want, errText)
		}
	}
	if strings.Contains(errText, ClaudeBackgroundTokenMessage) {
		t.Fatalf("real subprocess auth failure must not reuse the background-token caveat:\n%s", errText)
	}
	if result.Raw != raw {
		t.Fatalf("raw = %q, want %q", result.Raw, raw)
	}
}

func TestClaudeDeliverLastSessionCommand(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"done"}`}}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "claude", "--continue", "-p", "--output-format", "json", "--", "review")
}

func TestClaudeTemplateAgentIgnoresLastSession(t *testing.T) {
	minted := "550e8400-e29b-41d4-a716-446655440099"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"coordinated"}`}}}
	adapter := ClaudeAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return minted, nil
		},
	}
	agent := Agent{
		Name:       "lead",
		Role:       "agent",
		Runtime:    ClaudeRuntime,
		RuntimeRef: LastRef,
		RepoScope:  "jerryfane/gitmoot",
		TemplateID: "coordinator",
		Model:      "opus",
	}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "coordinate"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "coordinated" {
		t.Fatalf("Summary = %q, want coordinated", result.Summary)
	}
	// The minted session is ephemeral by design (#665): the mailbox adopts it for
	// same-job repair but must never persist it onto the "last" registration.
	if !result.SessionEphemeral {
		t.Fatalf("SessionEphemeral = false, want true for a last+template coordinator session")
	}
	if result.RefreshedRuntimeRef != minted {
		t.Fatalf("RefreshedRuntimeRef = %q, want the minted session %q", result.RefreshedRuntimeRef, minted)
	}
	runner.want(t, 0, "claude", "--model", "opus", "--session-id", minted, "-p", "--output-format", "json", "--", "coordinate")
	for _, arg := range runner.calls[0] {
		if arg == "--continue" || arg == "--resume" {
			t.Fatalf("template-backed Claude last agent must use an isolated session, got %v", runner.calls[0])
		}
	}
}

func TestClaudeDeliverCommandAppliesAutonomyPolicy(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy string
		mode   string
	}{
		{name: "read_only", policy: AutonomyPolicyReadOnly, mode: "plan"},
		{name: "workspace_write", policy: AutonomyPolicyWorkspaceWrite, mode: "acceptEdits"},
		{name: "danger_full_access", policy: AutonomyPolicyDangerFullAccess, mode: "bypassPermissions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"done"}`}}}
			adapter := ClaudeAdapter{Runner: runner}
			agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: tt.policy}

			if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
				t.Fatalf("Deliver returned error: %v", err)
			}
			runner.want(t, 0, "claude", "--permission-mode", tt.mode, "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
		})
	}
}

func TestClaudeDeliverFallsBackToText(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "unknown option '--output-format'"},
			{Stdout: "plain response\n"},
		},
		errs: []error{errors.New("exit 1"), nil},
	}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "plain response" {
		t.Fatalf("summary = %q", result.Summary)
	}
	runner.want(t, 0, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
	runner.want(t, 1, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--", "review")
}

func TestClaudeDeliverClassifiesAuthFailure(t *testing.T) {
	raw := "401 Invalid authentication credentials"
	runner := &fakeRunner{
		results: []subprocess.Result{{Stdout: raw}},
		errs:    []error{errors.New("exit 1")},
	}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})

	if err == nil {
		t.Fatal("Deliver accepted auth failure")
	}
	errText := err.Error()
	for _, want := range []string{"Claude Code authentication failed", ClaudeSessionAuthFailedMessage, raw} {
		if !strings.Contains(errText, want) {
			t.Fatalf("error missing %q:\n%s", want, errText)
		}
	}
	if strings.Contains(errText, ClaudeBackgroundTokenMessage) {
		t.Fatalf("real subprocess auth failure must not reuse the background-token caveat:\n%s", errText)
	}
	if result.Raw != raw {
		t.Fatalf("raw = %q, want %q", result.Raw, raw)
	}
}

func TestClaudeDeliverRetriesTransientSocketError(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "API Error: 401 The socket connection was closed unexpectedly"},
			{Stdout: `{"result":"recovered"}`},
		},
		errs: []error{errors.New("exit 1"), nil},
	}
	adapter := ClaudeAdapter{Runner: runner, RetryBackoff: time.Nanosecond}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "recovered" {
		t.Fatalf("summary = %q, want %q", result.Summary, "recovered")
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner called %d times, want exactly 2 (one retry): %v", len(runner.calls), runner.calls)
	}
	runner.want(t, 0, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
	runner.want(t, 1, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
}

func TestClaudeDeliverDoesNotRetryPermanentError(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "401 Invalid authentication credentials"}},
		errs:    []error{errors.New("exit 1")},
	}
	adapter := ClaudeAdapter{Runner: runner, RetryBackoff: time.Nanosecond}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err == nil {
		t.Fatal("Deliver accepted permanent error")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner called %d times, want exactly 1 (no retry on permanent error): %v", len(runner.calls), runner.calls)
	}
}

func TestClaudeDeliverSucceedsWithoutRetry(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"done"}`}}}
	adapter := ClaudeAdapter{Runner: runner, RetryBackoff: time.Nanosecond}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner called %d times, want exactly 1: %v", len(runner.calls), runner.calls)
	}
}

func TestClaudeDeliverRetriesExhausted(t *testing.T) {
	// Every attempt hits the transient, INCLUDING the final fresh-session
	// escalation (#509): the bounds must stop after exactly
	// claudeDeliveryMaxAttempts re-resume attempts plus one fresh-session attempt
	// and surface the final error, never loop.
	results := make([]subprocess.Result, claudeDeliveryMaxAttempts+1)
	errs := make([]error, claudeDeliveryMaxAttempts+1)
	for i := range results {
		results[i] = subprocess.Result{Stderr: "401 The socket connection was closed unexpectedly"}
		errs[i] = errors.New("exit 1")
	}
	runner := &fakeRunner{results: results, errs: errs}
	adapter := ClaudeAdapter{
		Runner:        runner,
		RetryBackoff:  time.Nanosecond,
		NewRuntimeRef: func() (string, error) { return "550e8400-e29b-41d4-a716-446655440099", nil },
	}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err == nil {
		t.Fatal("Deliver accepted a transient that persisted across all attempts")
	}
	if len(runner.calls) != claudeDeliveryMaxAttempts+1 {
		t.Fatalf("runner called %d times, want exactly %d (retries + one fresh-session escalation): %v", len(runner.calls), claudeDeliveryMaxAttempts+1, runner.calls)
	}
}

func TestClaudeDeliverEscalatesToFreshSessionOnPersistentTransient(t *testing.T) {
	// Regression for #509: a byte-identical --resume retry empirically does NOT
	// clear the intermittent socket-closed 401 (every in-call attempt fails
	// together), but a fresh, later delivery does. So after the in-call retries
	// exhaust on the transient, Deliver must make ONE final attempt on a
	// brand-new --session-id to get this delivery past the flake.
	//
	// CRUCIALLY, the transient (#509) session is still ALIVE (a later --resume of
	// the same ref recovers), so the agent must NOT be permanently re-pinned:
	// RefreshedRuntimeRef must stay EMPTY so the agent keeps its original,
	// context-bearing session and the next delivery resumes it. (Re-pinning is
	// reserved for the dead-session class (#443) — see TestClaudeDeliverSelfHealsDeadSession.)
	results := make([]subprocess.Result, claudeDeliveryMaxAttempts+1)
	errs := make([]error, claudeDeliveryMaxAttempts+1)
	for i := 0; i < claudeDeliveryMaxAttempts; i++ {
		results[i] = subprocess.Result{Stderr: "API Error: 401 The socket connection was closed unexpectedly"}
		errs[i] = errors.New("exit 1")
	}
	results[claudeDeliveryMaxAttempts] = subprocess.Result{Stdout: `{"result":"recovered"}`}
	runner := &fakeRunner{results: results, errs: errs}
	adapter := ClaudeAdapter{
		Runner:        runner,
		RetryBackoff:  time.Nanosecond,
		NewRuntimeRef: func() (string, error) { return "550e8400-e29b-41d4-a716-446655440099", nil },
	}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "recovered" {
		t.Fatalf("summary = %q, want %q", result.Summary, "recovered")
	}
	// The transient session is still alive, so the agent stays pinned to it: a
	// transport blip must never silently discard accumulated conversation context.
	if result.RefreshedRuntimeRef != "" {
		t.Fatalf("RefreshedRuntimeRef = %q, want empty (a still-alive transient session must NOT be re-pinned away, or the agent loses its context)", result.RefreshedRuntimeRef)
	}
	if len(runner.calls) != claudeDeliveryMaxAttempts+1 {
		t.Fatalf("runner called %d times, want %d (retries + one fresh-session escalation): %v", len(runner.calls), claudeDeliveryMaxAttempts+1, runner.calls)
	}
	// The in-call retries re-resume the wedged session...
	runner.want(t, 0, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", "review")
	// ...and the final escalation uses a brand-new --session-id, never --resume.
	runner.want(t, claudeDeliveryMaxAttempts, "claude", "--session-id", "550e8400-e29b-41d4-a716-446655440099", "-p", "--output-format", "json", "--", "review")
}

func TestClaudeDeliverDoesNotEscalateLastSessionOnTransient(t *testing.T) {
	// A "last" (--continue) agent shares the rolling session and must never be
	// re-pinned onto a fresh --session-id, even when the transient persists across
	// every attempt — escalation is reserved for pinned-UUID sessions.
	results := make([]subprocess.Result, claudeDeliveryMaxAttempts)
	errs := make([]error, claudeDeliveryMaxAttempts)
	for i := range results {
		results[i] = subprocess.Result{Stderr: "401 The socket connection was closed unexpectedly"}
		errs[i] = errors.New("exit 1")
	}
	runner := &fakeRunner{results: results, errs: errs}
	adapter := ClaudeAdapter{
		Runner:        runner,
		RetryBackoff:  time.Nanosecond,
		NewRuntimeRef: func() (string, error) { return "550e8400-e29b-41d4-a716-446655440099", nil },
	}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err == nil {
		t.Fatal("Deliver accepted a transient that persisted across all attempts")
	}
	if result.RefreshedRuntimeRef != "" {
		t.Fatalf("RefreshedRuntimeRef = %q, want empty (a last/--continue session must never be re-pinned)", result.RefreshedRuntimeRef)
	}
	if len(runner.calls) != claudeDeliveryMaxAttempts {
		t.Fatalf("runner called %d times, want exactly %d (no fresh-session escalation on --continue): %v", len(runner.calls), claudeDeliveryMaxAttempts, runner.calls)
	}
	for i, call := range runner.calls {
		for _, arg := range call {
			if arg == "--session-id" {
				t.Fatalf("call %d minted a fresh --session-id for a last/--continue agent: %v", i, call)
			}
		}
	}
}

func TestClaudeDeliverSucceedsOnFinalAttempt(t *testing.T) {
	// Transient on the first claudeDeliveryMaxAttempts-1 attempts, then a JSON
	// success on the last: Deliver must recover and call exactly the bound times.
	results := make([]subprocess.Result, claudeDeliveryMaxAttempts)
	errs := make([]error, claudeDeliveryMaxAttempts)
	for i := 0; i < claudeDeliveryMaxAttempts-1; i++ {
		results[i] = subprocess.Result{Stderr: "401 The socket connection was closed unexpectedly"}
		errs[i] = errors.New("exit 1")
	}
	results[claudeDeliveryMaxAttempts-1] = subprocess.Result{Stdout: `{"result":"recovered"}`}
	runner := &fakeRunner{results: results, errs: errs}
	adapter := ClaudeAdapter{Runner: runner, RetryBackoff: time.Nanosecond}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "recovered" {
		t.Fatalf("summary = %q, want %q", result.Summary, "recovered")
	}
	if len(runner.calls) != claudeDeliveryMaxAttempts {
		t.Fatalf("runner called %d times, want exactly %d: %v", len(runner.calls), claudeDeliveryMaxAttempts, runner.calls)
	}
}

func TestClaudeDeliverStopsRetryOnContextCancelledDuringBackoff(t *testing.T) {
	// A persistent transient with a large backoff would hang if cancellation were
	// ignored mid-backoff; cancelling after the first call must stop promptly.
	results := make([]subprocess.Result, claudeDeliveryMaxAttempts)
	errs := make([]error, claudeDeliveryMaxAttempts)
	for i := range results {
		results[i] = subprocess.Result{Stderr: "401 The socket connection was closed unexpectedly"}
		errs[i] = errors.New("exit 1")
	}
	runner := &fakeRunner{results: results, errs: errs}
	adapter := ClaudeAdapter{Runner: runner, RetryBackoff: time.Hour}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel shortly after delivery starts so the first backoff is interrupted.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := adapter.Deliver(ctx, agent, Job{Prompt: "review"}); err == nil {
			t.Error("Deliver accepted transient error under cancelled context")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Deliver hung instead of honouring context cancellation during backoff")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner called %d times, want exactly 1 (cancel during backoff must stop): %v", len(runner.calls), runner.calls)
	}
}

func TestClaudeRetryBackoffExponentialCapped(t *testing.T) {
	a := ClaudeAdapter{RetryBackoff: time.Second}
	for _, tt := range []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 3, want: 4 * time.Second},
		{attempt: 4, want: 8 * time.Second},
		{attempt: 10, want: maxClaudeRetryBackoff}, // capped
	} {
		if got := a.retryBackoff(tt.attempt); got != tt.want {
			t.Fatalf("retryBackoff(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
	// Defaults to defaultClaudeRetryBackoff when RetryBackoff is unset.
	if got := (ClaudeAdapter{}).retryBackoff(1); got != defaultClaudeRetryBackoff {
		t.Fatalf("default retryBackoff(1) = %v, want %v", got, defaultClaudeRetryBackoff)
	}
}

func TestClaudeDeliverStopsRetryOnContextCancellation(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "401 The socket connection was closed unexpectedly"}},
		errs:    []error{errors.New("exit 1")},
	}
	// A large backoff would hang for an hour if cancellation were ignored.
	adapter := ClaudeAdapter{Runner: runner, RetryBackoff: time.Hour}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := adapter.Deliver(ctx, agent, Job{Prompt: "review"}); err == nil {
		t.Fatal("Deliver accepted transient error under cancelled context")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner called %d times, want exactly 1 (cancelled ctx must skip backoff/retry): %v", len(runner.calls), runner.calls)
	}
}

func TestIsTransientClaudeDeliveryError(t *testing.T) {
	runErr := errors.New("exit 1")
	for _, tt := range []struct {
		name   string
		result subprocess.Result
		err    error
		want   bool
	}{
		{name: "socket_was_closed_on_stderr", result: subprocess.Result{Stderr: "401 The socket connection was closed unexpectedly"}, err: runErr, want: true},
		{name: "socket_closed_alt_wording_on_stdout", result: subprocess.Result{Stdout: "API Error: 401 socket connection closed unexpectedly"}, err: runErr, want: true},
		{name: "mixed_case", result: subprocess.Result{Stderr: "401 Socket Connection Was Closed Unexpectedly"}, err: runErr, want: true},
		{name: "socket_closed_without_401_mid_stream", result: subprocess.Result{Stderr: "socket connection was closed unexpectedly"}, err: runErr, want: false},
		{name: "permanent_auth_error", result: subprocess.Result{Stderr: "401 Invalid authentication credentials"}, err: runErr, want: false},
		// A genuine auth failure (#486) must never be retried as a transient even
		// if its message also carries a socket-closed phrase: retrying an invalid
		// token wastes minutes and masks the real "fix your token" signal.
		{name: "auth_error_with_socket_noise_not_transient", result: subprocess.Result{Stderr: `{"error":{"type":"authentication_error"}} 401 The socket connection was closed unexpectedly`}, err: runErr, want: false},
		{name: "nil_error_even_with_signature", result: subprocess.Result{Stderr: "401 socket connection was closed unexpectedly"}, err: nil, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientClaudeDeliveryError(tt.result, tt.err); got != tt.want {
				t.Fatalf("isTransientClaudeDeliveryError = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudeHealthUsesRegisteredSession(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"OK"}`}}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	runner.want(t, 0, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", healthPrompt)
}

func TestClaudeHealthClassifiesAuthFailure(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "401 Invalid authentication credentials"}},
		errs:    []error{errors.New("exit 1")},
	}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	err := adapter.Health(context.Background(), agent)

	if err == nil {
		t.Fatal("Health accepted auth failure")
	}
	errText := err.Error()
	for _, want := range []string{"Claude Code authentication failed", ClaudeSessionAuthFailedMessage} {
		if !strings.Contains(errText, want) {
			t.Fatalf("error missing %q:\n%s", want, errText)
		}
	}
	if strings.Contains(errText, ClaudeBackgroundTokenMessage) {
		t.Fatalf("real subprocess auth failure must not reuse the background-token caveat:\n%s", errText)
	}
}

func TestClaudeHealthRejectsBrokenSession(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit 1")}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err == nil {
		t.Fatal("Health accepted broken claude session")
	}
	runner.want(t, 0, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", healthPrompt)
}

func TestShellDeliverCommand(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "ok\n"}}}
	adapter := ShellAdapter{Runner: runner}
	agent := Agent{Name: "custom", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "hello"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "ok" {
		t.Fatalf("summary = %q", result.Summary)
	}
	runner.want(t, 0, "sh", "-c", "printf ok", "gitmoot", "hello")
}

func TestShellStartUnsupported(t *testing.T) {
	adapter := ShellAdapter{}
	agent := Agent{Name: "custom", Role: "reviewer", Runtime: ShellRuntime, RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err == nil {
		t.Fatal("Start accepted shell runtime")
	}
}

func TestShellHealthRunsProbe(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit 1")}}
	adapter := ShellAdapter{Runner: runner}
	agent := Agent{Name: "custom", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: "exit 1", RepoScope: "jerryfane/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err == nil {
		t.Fatal("Health accepted failing shell command")
	}
	runner.want(t, 0, "sh", "-c", "exit 1", "gitmoot-health", healthPrompt)
}

func TestCodexSessionIndexFindsIDAndThreadName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_index.jsonl")
	content := `{"id":"550e8400-e29b-41d4-a716-446655440001","thread_name":"review-thread","updated_at":"2026-05-20T00:00:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	index := CodexSessionIndex{Path: path}

	for _, ref := range []string{"550e8400-e29b-41d4-a716-446655440001", "review-thread"} {
		exists, err := index.Exists(context.Background(), ref)
		if err != nil {
			t.Fatalf("Exists returned error for %q: %v", ref, err)
		}
		if !exists {
			t.Fatalf("Exists(%q) = false, want true", ref)
		}
	}
}

func TestCodexSessionIndexFindsIDFromShellSnapshot(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), nil, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	snapshots := filepath.Join(home, "shell_snapshots")
	if err := os.MkdirAll(snapshots, 0o700); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	sessionID := "550e8400-e29b-41d4-a716-446655440003"
	snapshot := filepath.Join(snapshots, sessionID+".1779518064381155930.sh")
	if err := os.WriteFile(snapshot, []byte("# snapshot\n"), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	exists, err := (CodexSessionIndex{Home: home}).Exists(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Exists returned error: %v", err)
	}
	if !exists {
		t.Fatal("Exists returned false for shell snapshot session")
	}
}

func TestCodexSessionIndexUsesCodexHomeBeforeDefaultHome(t *testing.T) {
	codexHome := t.TempDir()
	sessionID := "550e8400-e29b-41d4-a716-446655440004"
	content := `{"id":"` + sessionID + `","thread_name":"codex-home-thread","updated_at":"2026-05-20T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	exists, err := (CodexSessionIndex{}).Exists(context.Background(), "codex-home-thread")
	if err != nil {
		t.Fatalf("Exists returned error: %v", err)
	}
	if !exists {
		t.Fatal("Exists returned false for CODEX_HOME thread")
	}
}

func TestCodexSessionIndexDoesNotMatchThreadNameFromSnapshot(t *testing.T) {
	home := t.TempDir()
	snapshots := filepath.Join(home, "shell_snapshots")
	if err := os.MkdirAll(snapshots, 0o700); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapshots, "review-thread.1779518064381155930.sh"), []byte("# snapshot\n"), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	exists, err := (CodexSessionIndex{Home: home}).Exists(context.Background(), "review-thread")
	if err != nil {
		t.Fatalf("Exists returned error: %v", err)
	}
	if exists {
		t.Fatal("Exists returned true for non-UUID snapshot thread name")
	}
}

type staticCodexSessions struct {
	exists bool
	err    error
}

func (s staticCodexSessions) Exists(context.Context, string) (bool, error) {
	return s.exists, s.err
}

type fakeRunner struct {
	results []subprocess.Result
	errs    []error
	calls   [][]string
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

func TestCodexCommandError(t *testing.T) {
	runErr := errors.New("exit status 1")

	t.Run("surfaces the --json error event over stderr noise", func(t *testing.T) {
		result := subprocess.Result{
			Stderr: "Reading additional input from stdin...",
			Stdout: `{"type":"thread.started","thread_id":"t1"}` + "\n" +
				`{"type":"turn.started"}` + "\n" +
				`{"type":"error","message":"You've hit your usage limit. try again at Jun 14th"}` + "\n",
		}
		got := codexCommandError(result, runErr).Error()
		if !strings.Contains(got, "You've hit your usage limit") {
			t.Fatalf("want the usage-limit message, got %q", got)
		}
		if strings.Contains(got, "Reading additional input from stdin") {
			t.Fatalf("must not surface the stdin noise line, got %q", got)
		}
	})

	t.Run("uses a turn.failed error.message", func(t *testing.T) {
		result := subprocess.Result{
			Stdout: `{"type":"turn.failed","error":{"message":"sandbox denied write"}}` + "\n",
		}
		if got := codexCommandError(result, runErr).Error(); !strings.Contains(got, "sandbox denied write") {
			t.Fatalf("want the turn.failed message, got %q", got)
		}
	})

	t.Run("falls back to stderr when stdout has no json error", func(t *testing.T) {
		result := subprocess.Result{Stderr: "boom on stderr", Stdout: "plain non-json output"}
		if got := codexCommandError(result, runErr).Error(); !strings.Contains(got, "boom on stderr") {
			t.Fatalf("want the stderr fallback, got %q", got)
		}
	})
}

func TestValidateAgentAcceptsKimiRuntimes(t *testing.T) {
	for _, runtimeName := range []string{KimiRuntime, KimiCLIRuntime} {
		agent := Agent{Name: "audit", Role: "reviewer", Runtime: runtimeName, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440000", RepoScope: "jerryfane/gitmoot"}
		if err := ValidateAgent(agent); err != nil {
			t.Fatalf("ValidateAgent rejected valid %s agent: %v", runtimeName, err)
		}
	}
}

func TestValidateAgentRejectsInvalidKimiRef(t *testing.T) {
	for _, runtimeName := range []string{KimiRuntime, KimiCLIRuntime} {
		agent := Agent{Name: "audit", Role: "reviewer", Runtime: runtimeName, RuntimeRef: "not-a-session", RepoScope: "jerryfane/gitmoot"}
		if err := ValidateAgent(agent); err == nil {
			t.Fatalf("ValidateAgent accepted invalid %s runtime ref", runtimeName)
		}
	}
}

func TestKimiAdapterValidateRejectsRuntimeMismatch(t *testing.T) {
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440000", RepoScope: "jerryfane/gitmoot"}
	if err := (KimiAdapter{}).Validate(context.Background(), agent); err == nil {
		t.Fatal("KimiAdapter accepted a Claude agent")
	}
	if err := (KimiCLIAdapter{}).Validate(context.Background(), agent); err == nil {
		t.Fatal("KimiCLIAdapter accepted a Claude agent")
	}
}

func TestKimiStartCommandParsesSessionID(t *testing.T) {
	stdout := `{"role":"assistant","content":"ready"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session_550e8400-e29b-41d4-a716-446655440000","command":"kimi -r session_550e8400-e29b-41d4-a716-446655440000","content":"To resume this session: kimi -r session_550e8400-e29b-41d4-a716-446655440000"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{Runner: runner, Dir: "/repo"}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: KimiRuntime, RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly}

	result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if result.RuntimeRef != "session_550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("runtime ref = %q", result.RuntimeRef)
	}
	if result.Raw != "ready" {
		t.Fatalf("raw = %q", result.Raw)
	}
	runner.want(t, 0, "kimi", "-p", "initialize", "--output-format", "stream-json")
}

func TestKimiStartCommandDoesNotPassPermissionFlags(t *testing.T) {
	stdout := `{"role":"assistant","content":"ready"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session_550e8400-e29b-41d4-a716-446655440000","command":"kimi -r session_550e8400-e29b-41d4-a716-446655440000","content":"To resume this session: kimi -r session_550e8400-e29b-41d4-a716-446655440000"}` + "\n"

	for _, tt := range []struct {
		name   string
		policy string
	}{
		{name: "read_only", policy: AutonomyPolicyReadOnly},
		{name: "auto", policy: AutonomyPolicyAuto},
		{name: "workspace_write", policy: AutonomyPolicyWorkspaceWrite},
		{name: "danger_full_access", policy: AutonomyPolicyDangerFullAccess},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
			adapter := KimiAdapter{
				Runner: runner,
				NewRuntimeRef: func() (string, error) {
					return "550e8400-e29b-41d4-a716-446655440010", nil
				},
			}
			agent := Agent{Name: "lead", Role: "implementer", Runtime: KimiRuntime, RepoScope: "jerryfane/gitmoot", AutonomyPolicy: tt.policy}

			if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err != nil {
				t.Fatalf("Start returned error: %v", err)
			}
			runner.want(t, 0, "kimi", "-p", "initialize", "--output-format", "stream-json")
		})
	}
}

func TestKimiStartFallsBackToGeneratedRefWhenSessionIDMissing(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"role":"assistant","content":"ready"}` + "\n"}}}
	adapter := KimiAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440010", nil
		},
	}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: KimiRuntime, RepoScope: "jerryfane/gitmoot"}

	result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if result.RuntimeRef != "550e8400-e29b-41d4-a716-446655440010" {
		t.Fatalf("runtime ref = %q", result.RuntimeRef)
	}
}

func TestKimiDeliverCommandStartsFreshSession(t *testing.T) {
	stdout := `{"role":"assistant","content":"done"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session_550e8400-e29b-41d4-a716-446655440001","command":"kimi -r session_550e8400-e29b-41d4-a716-446655440001","content":"To resume this session: kimi -r session_550e8400-e29b-41d4-a716-446655440001"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if result.Raw != "done" {
		t.Fatalf("raw = %q", result.Raw)
	}
	runner.want(t, 0, "kimi", "-p", "review", "--output-format", "stream-json")
}

func TestKimiDeliverAcceptsArrayContentParts(t *testing.T) {
	stdout := "{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"prefix\\n```json\\n{\\\"gitmoot_result\\\":{\\\"decision\\\":\\\"approved\\\",\\\"summary\\\":\\\"ok\\\",\\\"findings\\\":[],\\\"changes_made\\\":[],\\\"tests_run\\\":[],\\\"needs\\\":[],\\\"delegations\\\":[]}}\\n```\\n\"}]}\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if !strings.Contains(result.Raw, `"gitmoot_result"`) {
		t.Fatalf("raw output missing gitmoot_result: %q", result.Raw)
	}
	runner.want(t, 0, "kimi", "-p", "review", "--output-format", "stream-json")
}

func TestKimiDeliverDoesNotProbePrintFallback(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "unknown option '--print'"}},
		errs:    []error{errors.New("exit 1")},
	}
	adapter := KimiAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err == nil {
		t.Fatal("Deliver accepted command failure")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1 (no probe/fallback)", len(runner.calls))
	}
	runner.want(t, 0, "kimi", "-p", "review", "--output-format", "stream-json")
}

func TestKimiDeliverCommandUsesJobModel(t *testing.T) {
	stdout := `{"role":"assistant","content":"done"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly, Model: "agent-default"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review", Model: "opus"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "kimi", "--model", "opus", "-p", "review", "--output-format", "stream-json")
}

func TestKimiDeliverCommandFallsBackToAgentModel(t *testing.T) {
	stdout := `{"role":"assistant","content":"done"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly, Model: "sonnet"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "kimi", "--model", "sonnet", "-p", "review", "--output-format", "stream-json")
}

func TestKimiStartCommandUsesAgentModel(t *testing.T) {
	stdout := `{"role":"assistant","content":"ready"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{
		Runner: runner,
		NewRuntimeRef: func() (string, error) {
			return "550e8400-e29b-41d4-a716-446655440010", nil
		},
	}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: KimiRuntime, RepoScope: "jerryfane/gitmoot", Model: "opus"}

	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	runner.want(t, 0, "kimi", "--model", "opus", "-p", "initialize", "--output-format", "stream-json")
}

func TestKimiHealthRunsFreshSession(t *testing.T) {
	stdout := `{"role":"assistant","content":"OK"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session_550e8400-e29b-41d4-a716-446655440002","command":"kimi -r session_550e8400-e29b-41d4-a716-446655440002","content":"To resume this session: kimi -r session_550e8400-e29b-41d4-a716-446655440002"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly}

	if err := adapter.Health(context.Background(), agent); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	runner.want(t, 0, "kimi", "-p", KimiLiveCheckPrompt, "--output-format", "stream-json")
}

func TestKimiHealthClassifiesAuthFailure(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "Please run `kimi login` to authenticate."}},
		errs:    []error{errors.New("exit 1")},
	}
	adapter := KimiAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	err := adapter.Health(context.Background(), agent)
	if err == nil {
		t.Fatal("Health accepted auth failure")
	}
	errText := err.Error()
	for _, want := range []string{"Kimi Code authentication required", "kimi login", "restart the Gitmoot daemon"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("error missing %q:\n%s", want, errText)
		}
	}
}

func TestKimiHealthRejectsBrokenSession(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit 1")}}
	adapter := KimiAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly}

	if err := adapter.Health(context.Background(), agent); err == nil {
		t.Fatal("Health accepted broken Kimi session")
	}
	runner.want(t, 0, "kimi", "-p", KimiLiveCheckPrompt, "--output-format", "stream-json")
}

func TestKimiCLIStartCommandUsesPrintRuntime(t *testing.T) {
	stdout := `{"role":"assistant","content":"ready"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session_550e8400-e29b-41d4-a716-446655440003"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiCLIAdapter{Runner: runner, Dir: "/repo"}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: KimiCLIRuntime, RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly}

	result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if result.RuntimeRef != "session_550e8400-e29b-41d4-a716-446655440003" {
		t.Fatalf("runtime ref = %q", result.RuntimeRef)
	}
	runner.want(t, 0, "kimi", "--print", "-p", "initialize", "--output-format", "stream-json")
}

func TestKimiCLIDeliverCommandUsesPrintRuntime(t *testing.T) {
	stdout := `{"role":"assistant","content":"done"}` + "\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := KimiCLIAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: KimiCLIRuntime, RuntimeRef: "session_550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly, Model: "agent-default"}

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review", Model: "opus"})
	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("summary = %q", result.Summary)
	}
	runner.want(t, 0, "kimi", "--model", "opus", "--print", "-p", "review", "--output-format", "stream-json")
}
