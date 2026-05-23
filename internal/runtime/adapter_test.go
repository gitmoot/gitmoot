package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestValidateAgent(t *testing.T) {
	agent := Agent{Name: "audit", Role: "reviewer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440000", RepoScope: "jerryfane/gitmoot"}
	if err := ValidateAgent(agent); err != nil {
		t.Fatalf("ValidateAgent returned error: %v", err)
	}
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
	runner.want(t, 0, "codex", "exec", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", "review this")
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

func TestCodexStartRejectsMissingThreadID(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"type":"turn.completed"}` + "\n"}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "initialize"}); err == nil {
		t.Fatal("Start accepted output without thread id")
	}
	runner.want(t, 0, "codex", "exec", "--json", "--", "initialize")
}

func TestCodexDeliverLastSessionCommand(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "continue"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "resume", "--last", "--", "continue")
}

func TestCodexDeliverVerifiesNamedSession(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "done"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "review-thread", RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "resume", "review-thread", "--", "review")
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
	runner.want(t, 0, "codex", "exec", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", "review")
}

func TestCodexHealthUsesRegisteredSession(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "OK"}}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	runner.want(t, 0, "codex", "exec", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", healthPrompt)
}

func TestCodexHealthRejectsBrokenSession(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit 1")}}
	adapter := CodexAdapter{Runner: runner, SessionResolver: staticCodexSessions{exists: true}}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: CodexRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440001", RepoScope: "jerryfane/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err == nil {
		t.Fatal("Health accepted broken codex session")
	}
	runner.want(t, 0, "codex", "exec", "resume", "550e8400-e29b-41d4-a716-446655440001", "--", healthPrompt)
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

func TestClaudeDeliverLastSessionCommand(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"done"}`}}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: LastRef, RepoScope: "jerryfane/gitmoot"}

	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "review"}); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	runner.want(t, 0, "claude", "--continue", "-p", "--output-format", "json", "--", "review")
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

func TestClaudeHealthUsesRegisteredSession(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"OK"}`}}}
	adapter := ClaudeAdapter{Runner: runner}
	agent := Agent{Name: "reviewer", Role: "reviewer", Runtime: ClaudeRuntime, RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "jerryfane/gitmoot"}

	if err := adapter.Health(context.Background(), agent); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	runner.want(t, 0, "claude", "--resume", "550e8400-e29b-41d4-a716-446655440002", "-p", "--output-format", "json", "--", healthPrompt)
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
