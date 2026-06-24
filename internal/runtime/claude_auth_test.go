package runtime

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestInspectClaudeAuthEnvMasksReadiness(t *testing.T) {
	auth := InspectClaudeAuthEnv(func(name string) (string, bool) {
		switch name {
		case ClaudeOAuthTokenEnv:
			return "secret-token", true
		default:
			return "", false
		}
	})

	if !auth.Ready() {
		t.Fatal("auth env was not ready despite OAuth token")
	}
	detail := auth.MaskedDetail()
	if !strings.Contains(detail, ClaudeOAuthTokenEnv+"=set") || strings.Contains(detail, "secret-token") {
		t.Fatalf("masked detail = %q", detail)
	}
	if warning := auth.Warning(); warning != "" {
		t.Fatalf("warning = %q, want none", warning)
	}
}

func TestInspectClaudeAuthEnvWarnsForMissingCredentials(t *testing.T) {
	auth := InspectClaudeAuthEnv(func(string) (string, bool) { return "", false })

	if auth.Ready() {
		t.Fatal("auth env is ready despite no credentials")
	}
	if !strings.Contains(auth.Warning(), "claude setup-token") {
		t.Fatalf("warning = %q, want setup-token guidance", auth.Warning())
	}
}

func TestInspectClaudeAuthEnvWarnsForAPIKeyPrecedence(t *testing.T) {
	auth := InspectClaudeAuthEnv(func(name string) (string, bool) {
		if name == AnthropicAPIKeyEnv {
			return "secret-key", true
		}
		return "", false
	})

	if !auth.Ready() {
		t.Fatal("auth env was not ready despite API key")
	}
	if !strings.Contains(auth.Warning(), "API-key billing") {
		t.Fatalf("warning = %q, want API key warning", auth.Warning())
	}
}

func TestClaudeLiveCheckRunsPrintModeSmoke(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"result":"OK"}`}}}

	if err := ClaudeLiveCheck(context.Background(), runner, "/repo"); err != nil {
		t.Fatalf("ClaudeLiveCheck returned error: %v", err)
	}

	runner.want(t, 0, "claude", "-p", "--output-format", "json", "--", ClaudeLiveCheckPrompt)
}

func TestClaudeLiveCheckClassifiesAuthFailure(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "401 Invalid authentication credentials"}},
		errs:    []error{errors.New("exit 1")},
	}

	err := ClaudeLiveCheck(context.Background(), runner, "/repo")

	if err == nil {
		t.Fatal("ClaudeLiveCheck accepted auth failure")
	}
	// A real subprocess auth/session failure must surface the session-failure
	// message (refresh + rebind), not the background-token caveat.
	if !strings.Contains(err.Error(), ClaudeSessionAuthFailedMessage) {
		t.Fatalf("error missing session-failure message:\n%s", err)
	}
	if strings.Contains(err.Error(), ClaudeBackgroundTokenMessage) {
		t.Fatalf("error must not reuse the background-token caveat for a real auth failure:\n%s", err)
	}
}

// (F) The two messages must be distinct, and a classified subprocess auth/session
// failure (the path the adapter uses) must wrap the session message — never the
// background-token caveat.
func TestClaudeAuthMessagesAreDistinct(t *testing.T) {
	if ClaudeBackgroundTokenMessage == ClaudeSessionAuthFailedMessage {
		t.Fatal("background-token and session-failure messages must differ")
	}
	if !strings.Contains(ClaudeBackgroundTokenMessage, "background") {
		t.Fatalf("background-token message lost its background-job framing:\n%s", ClaudeBackgroundTokenMessage)
	}
	if !strings.Contains(ClaudeSessionAuthFailedMessage, "session") {
		t.Fatalf("session-failure message lost its session framing:\n%s", ClaudeSessionAuthFailedMessage)
	}
	err := ClassifyClaudeCommandError(
		subprocess.Result{Stderr: "401 Invalid authentication credentials"},
		errors.New("exit 1"),
	)
	if err == nil || !strings.Contains(err.Error(), ClaudeSessionAuthFailedMessage) {
		t.Fatalf("ClassifyClaudeCommandError must wrap the session message:\n%v", err)
	}
}

// A missing/unexecutable claude binary is "probe unavailable", not an auth
// failure — ClaudeProbeUnavailable distinguishes it so doctor never false-fails.
func TestClaudeProbeUnavailableClassifiesMissingBinary(t *testing.T) {
	runner := &fakeRunner{
		errs: []error{&exec.Error{Name: "claude", Err: exec.ErrNotFound}},
	}
	err := ClaudeLiveCheck(context.Background(), runner, "/repo")
	if err == nil {
		t.Fatal("ClaudeLiveCheck accepted a missing binary")
	}
	if !ClaudeProbeUnavailable(err) {
		t.Fatalf("missing binary not classified as probe-unavailable:\n%v", err)
	}
	authErr := ClassifyClaudeCommandError(
		subprocess.Result{Stderr: "401 authentication_error"},
		errors.New("exit 1"),
	)
	if ClaudeProbeUnavailable(authErr) {
		t.Fatalf("auth failure must NOT be classified as probe-unavailable:\n%v", authErr)
	}
	if ClaudeProbeUnavailable(nil) {
		t.Fatal("nil error must not be probe-unavailable")
	}
}

func TestClaudeLiveCheckFallsBackToText(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "unknown option '--output-format'"},
			{Stdout: "OK\n"},
		},
		errs: []error{errors.New("exit 1"), nil},
	}

	if err := ClaudeLiveCheck(context.Background(), runner, "/repo"); err != nil {
		t.Fatalf("ClaudeLiveCheck returned error: %v", err)
	}

	runner.want(t, 0, "claude", "-p", "--output-format", "json", "--", ClaudeLiveCheckPrompt)
	runner.want(t, 1, "claude", "-p", "--", ClaudeLiveCheckPrompt)
}

func TestClaudeLiveCheckFallbackRejectsStderrOnlySuccess(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "unknown option '--output-format'"},
			{Stderr: "diagnostic only"},
		},
		errs: []error{errors.New("exit 1"), nil},
	}

	err := ClaudeLiveCheck(context.Background(), runner, "/repo")

	if err == nil {
		t.Fatal("ClaudeLiveCheck accepted stderr-only fallback output")
	}
	if !strings.Contains(err.Error(), "no stdout response") {
		t.Fatalf("error = %q, want no stdout response", err)
	}
}

func TestClaudeLiveCheckRejectsStderrOnlySuccess(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stderr: "diagnostic only"}}}

	err := ClaudeLiveCheck(context.Background(), runner, "/repo")

	if err == nil {
		t.Fatal("ClaudeLiveCheck accepted stderr-only output")
	}
	if !strings.Contains(err.Error(), "no stdout response") {
		t.Fatalf("error = %q, want no stdout response", err)
	}
}
