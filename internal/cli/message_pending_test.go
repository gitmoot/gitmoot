package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

type turnHookTestOutput struct {
	Decision           string `json:"decision"`
	Reason             string `json:"reason"`
	HookSpecificOutput *struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// runTurnHook invokes the command exactly as a generated plugin hook does: the
// acting role comes from the session environment and the runtime's hook input
// arrives on stdin.
func runTurnHook(t *testing.T, home, role, runtimeName, event, stdin string) (string, string) {
	t.Helper()
	t.Setenv("GITMOOT_ORG_ROLE", role)
	t.Setenv("HERDR_PANE_ID", "")
	previous := turnHookInput
	turnHookInput = strings.NewReader(stdin)
	t.Cleanup(func() { turnHookInput = previous })
	var stdout, stderr bytes.Buffer
	code := Run([]string{"message", "pending", "--claim", "--hook", event, "--runtime", runtimeName, "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("hook exit = %d, want 0 (a hook must never fail the turn); stderr=%s", code, stderr.String())
	}
	return stdout.String(), stderr.String()
}

func sendTestMessage(t *testing.T, home, from, to, body string) int64 {
	t.Helper()
	var sent messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, from, "send", to, body), &sent); err != nil {
		t.Fatal(err)
	}
	return sent.ID
}

func showTestMessage(t *testing.T, home, role string, id int64) messageTestResult {
	t.Helper()
	var shown messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, role, "show", fmt.Sprint(id)), &shown); err != nil {
		t.Fatal(err)
	}
	return shown
}

func turnHookWakeReceipts(t *testing.T, home, role string) map[string]string {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.ListWakeOutbox(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		if entry.TargetRole == role {
			out[entry.SourceID] = entry.State + "|" + entry.LastError
		}
	}
	return out
}

func decodeTurnHookOutput(t *testing.T, stdout string) turnHookTestOutput {
	t.Helper()
	var output turnHookTestOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("hook stdout is not one JSON object: %v\n%s", err, stdout)
	}
	return output
}

func TestMessagePendingClaimShowsOwnMailOnceAndLeavesOtherRoles(t *testing.T) {
	home := messageTestHome(t)
	first := sendTestMessage(t, home, "gm-omp-nag", "deimos", "Please inspect the failing build.")
	second := sendTestMessage(t, home, "jarvis", "deimos", "Rebase onto main before merging.")
	other := sendTestMessage(t, home, "gm-omp-nag", "jarvis", "Private note for jarvis only.")

	stdout, _ := runTurnHook(t, home, "deimos", "claude", "UserPromptSubmit",
		`{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"continue"}`)
	output := decodeTurnHookOutput(t, stdout)
	if output.Decision != "" || output.HookSpecificOutput == nil || output.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Fatalf("claude UserPromptSubmit output = %s", stdout)
	}
	text := output.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		fmt.Sprintf("message %d [message] from gm-omp-nag: Please inspect the failing build.", first),
		fmt.Sprintf("gitmoot message show %d", first),
		fmt.Sprintf("message %d [message] from jarvis: Rebase onto main before merging.", second),
		fmt.Sprintf("gitmoot message show %d", second),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("additionalContext missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, fmt.Sprint(other)) || strings.Contains(text, "Private note") {
		t.Fatalf("deimos's hook showed jarvis's mail:\n%s", text)
	}

	again, _ := runTurnHook(t, home, "deimos", "claude", "PostToolUse",
		`{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"Bash"}`)
	if again != "" {
		t.Fatalf("second hook re-delivered claimed mail: %q", again)
	}
	if got := showTestMessage(t, home, "deimos", first).Status; got != "submitted" {
		t.Fatalf("claimed notification status = %q, want submitted", got)
	}
	if got := showTestMessage(t, home, "jarvis", other).Status; got != "queued" {
		t.Fatalf("another role's notification status = %q, want queued", got)
	}
	receipts := turnHookWakeReceipts(t, home, "deimos")
	for _, id := range []int64{first, second} {
		if got := receipts[fmt.Sprint(id)]; got != "delivered|turn-hook:claude:UserPromptSubmit" {
			t.Fatalf("wake for message %d = %q, want delivered turn-hook receipt", id, got)
		}
	}

	// Mail that arrives mid-turn is claimed by the next tool boundary, alone.
	third := sendTestMessage(t, home, "jarvis", "deimos", "One more thing.")
	stdout, _ = runTurnHook(t, home, "deimos", "claude", "PostToolUse",
		`{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"Bash"}`)
	output = decodeTurnHookOutput(t, stdout)
	text = output.HookSpecificOutput.AdditionalContext
	if output.HookSpecificOutput.HookEventName != "PostToolUse" || !strings.Contains(text, fmt.Sprintf("message %d ", third)) ||
		strings.Contains(text, fmt.Sprintf("message %d ", first)) {
		t.Fatalf("mid-turn delivery = %s", stdout)
	}
}

func TestMessagePendingStopContinuesOnlyOncePerRuntimeContract(t *testing.T) {
	home := messageTestHome(t)
	id := sendTestMessage(t, home, "jarvis", "deimos", "Status?")

	// A Stop that is already a stop-hook continuation must not extend the loop
	// and must not consume the mail it would not show.
	stdout, _ := runTurnHook(t, home, "deimos", "codex", "Stop",
		`{"session_id":"s1","turn_id":"t1","hook_event_name":"Stop","stop_hook_active":true,"last_assistant_message":"done"}`)
	if stdout != "" {
		t.Fatalf("continued Stop produced output: %q", stdout)
	}
	if got := showTestMessage(t, home, "deimos", id).Status; got != "queued" {
		t.Fatalf("continued Stop consumed mail: status %q", got)
	}

	stdout, _ = runTurnHook(t, home, "deimos", "codex", "Stop",
		`{"session_id":"s1","turn_id":"t1","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"done"}`)
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("codex Stop output: %v\n%s", err, stdout)
	}
	// Codex's Stop output schema has no hookSpecificOutput; additionalContext
	// there is rejected as invalid output, so continuation is decision=block.
	if _, ok := raw["hookSpecificOutput"]; ok || raw["decision"] != "block" ||
		!strings.Contains(fmt.Sprint(raw["reason"]), fmt.Sprintf("gitmoot message show %d", id)) {
		t.Fatalf("codex Stop output = %s", stdout)
	}

	second := sendTestMessage(t, home, "jarvis", "deimos", "Also this.")
	stdout, _ = runTurnHook(t, home, "deimos", "claude", "Stop",
		`{"session_id":"s2","hook_event_name":"Stop","stop_hook_active":false}`)
	output := decodeTurnHookOutput(t, stdout)
	if output.Decision != "" || output.HookSpecificOutput == nil || output.HookSpecificOutput.HookEventName != "Stop" ||
		!strings.Contains(output.HookSpecificOutput.AdditionalContext, fmt.Sprintf("gitmoot message show %d", second)) {
		t.Fatalf("claude Stop output = %s", stdout)
	}
	if again, _ := runTurnHook(t, home, "deimos", "claude", "Stop", `{"hook_event_name":"Stop"}`); again != "" {
		t.Fatalf("Stop with no unclaimed mail continued the turn: %q", again)
	}
}

func TestMessagePendingFailuresExitZeroWithEmptyOutput(t *testing.T) {
	home := messageTestHome(t)
	id := sendTestMessage(t, home, "jarvis", "deimos", "Keep me pending.")
	cases := []struct {
		name, role, runtime, event, stdin string
	}{
		{"unknown role", "nobody", "claude", "UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit"}`},
		{"unsupported event", "deimos", "claude", "SessionEnd", `{"hook_event_name":"SessionEnd"}`},
		{"unsupported runtime", "deimos", "kimi", "Stop", `{"hook_event_name":"Stop"}`},
		{"mismatched input event", "deimos", "codex", "PostToolUse", `{"hook_event_name":"Stop"}`},
		{"malformed input", "deimos", "codex", "PostToolUse", `{"hook_event_name":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := runTurnHook(t, home, tc.role, tc.runtime, tc.event, tc.stdin)
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "message pending:") {
				t.Fatalf("stderr = %q, want the failure reported there", stderr)
			}
		})
	}
	t.Run("subagent hook", func(t *testing.T) {
		stdout, _ := runTurnHook(t, home, "deimos", "claude", "PostToolUse",
			`{"hook_event_name":"PostToolUse","agent_id":"agent-1","agent_type":"Explore"}`)
		if stdout != "" {
			t.Fatalf("subagent hook claimed seat mail: %q", stdout)
		}
	})
	t.Run("corrupt org config", func(t *testing.T) {
		paths := config.PathsForHome(home)
		original, err := os.ReadFile(paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(paths.ConfigFile, original, 0o600) })
		if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr := runTurnHook(t, home, "deimos", "codex", "UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit"}`)
		if stdout != "" || stderr == "" {
			t.Fatalf("stdout=%q stderr=%q, want empty stdout and a stderr report", stdout, stderr)
		}
	})
	if got := showTestMessage(t, home, "deimos", id).Status; got != "queued" {
		t.Fatalf("failed hooks changed the notification: status %q", got)
	}
}

func TestMessagePendingPreviewIsScrubbedSingleLine(t *testing.T) {
	home := messageTestHome(t)
	body := "first line\n\x1b[31mred\x1b[0m token=ghp_abcdefghijklmnopqrstuvwxyz0123 " + strings.Repeat("long ", 80)
	id := sendTestMessage(t, home, "jarvis", "deimos", body)
	stdout, _ := runTurnHook(t, home, "deimos", "codex", "UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","prompt":"hi"}`)
	text := decodeTurnHookOutput(t, stdout).HookSpecificOutput.AdditionalContext
	var line string
	for _, candidate := range strings.Split(text, "\n") {
		if strings.HasPrefix(candidate, fmt.Sprintf("- message %d ", id)) {
			line = candidate
		}
	}
	if line == "" {
		t.Fatalf("no item line for message %d:\n%s", id, text)
	}
	if strings.Contains(line, "\x1b") || strings.Contains(line, "ghp_") || !strings.Contains(line, "first line") {
		t.Fatalf("preview not scrubbed: %q", line)
	}
	if strings.Count(line, "long") > 40 || !strings.HasSuffix(line, fmt.Sprintf("(read: gitmoot message show %d)", id)) {
		t.Fatalf("preview not bounded: %q", line)
	}
}

func TestMessagePendingBoundsOneInjectionAndKeepsTheRestPending(t *testing.T) {
	home := messageTestHome(t)
	ids := make([]int64, 0, turnHookMaxItems+2)
	for i := range turnHookMaxItems + 2 {
		ids = append(ids, sendTestMessage(t, home, "jarvis", "deimos", fmt.Sprintf("item %d", i)))
	}
	stdout, _ := runTurnHook(t, home, "deimos", "claude", "UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit"}`)
	text := decodeTurnHookOutput(t, stdout).HookSpecificOutput.AdditionalContext
	if !strings.Contains(text, "More mail is waiting") || strings.Contains(text, fmt.Sprintf("message %d ", ids[turnHookMaxItems])) {
		t.Fatalf("first injection not bounded:\n%s", text)
	}
	stdout, _ = runTurnHook(t, home, "deimos", "claude", "PostToolUse", `{"hook_event_name":"PostToolUse"}`)
	text = decodeTurnHookOutput(t, stdout).HookSpecificOutput.AdditionalContext
	for _, id := range ids[turnHookMaxItems:] {
		if !strings.Contains(text, fmt.Sprintf("message %d ", id)) {
			t.Fatalf("remaining mail %d not delivered next:\n%s", id, text)
		}
	}
	if strings.Contains(text, fmt.Sprintf("message %d ", ids[0])) {
		t.Fatalf("already-shown mail repeated:\n%s", text)
	}
}
