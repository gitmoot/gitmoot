package runtime

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

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

// envFakeRunner is a fakeRunner that also implements subprocess.EnvRunner,
// recording the extra environment passed to RunEnv so a test can prove the
// daemon's actual credential is what gets injected into the validation probe.
type envFakeRunner struct {
	fakeRunner
	gotEnv []string
}

func (f *envFakeRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	f.gotEnv = append([]string(nil), env...)
	return f.fakeRunner.Run(ctx, dir, command, args...)
}

// TestClaudeClassifyProbe486 is the tri-state core of the #486 fix: a live-probe
// outcome must map to valid / invalid / unknown so doctor can stop reporting a
// set-but-unvalidated token as ok, while never flipping a transient/network blip
// (or a missing binary) to a hard "invalid".
func TestClaudeClassifyProbe486(t *testing.T) {
	authErr := ClassifyClaudeCommandError(
		subprocess.Result{Stderr: "401 Invalid authentication credentials"},
		errors.New("exit 1"),
	)
	for _, tt := range []struct {
		name string
		err  error
		want ClaudeTokenStatus
	}{
		{name: "nil is valid", err: nil, want: ClaudeTokenValid},
		{name: "classified auth failure is invalid", err: authErr, want: ClaudeTokenInvalid},
		{name: "transient/network is unknown not invalid", err: errors.New("network is unreachable"), want: ClaudeTokenUnknown},
		{name: "missing binary is unknown", err: &exec.Error{Name: "claude", Err: exec.ErrNotFound}, want: ClaudeTokenUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClaudeClassifyProbe(tt.err); got != tt.want {
				t.Fatalf("ClaudeClassifyProbe(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
	if !errors.Is(authErr, ErrClaudeAuthFailed) {
		t.Fatal("a classified auth failure must satisfy errors.Is(err, ErrClaudeAuthFailed)")
	}
	if errors.Is(errors.New("boom"), ErrClaudeAuthFailed) {
		t.Fatal("a plain error must not satisfy errors.Is(err, ErrClaudeAuthFailed)")
	}
}

// TestClaudeLiveCheckEnvInjectsCredential proves the daemon-token validation seam:
// ClaudeLiveCheckEnv passes the supplied credential env to an EnvRunner probe (so
// doctor validates the daemon's own token, not the doctor process's), and a 401
// from that probe classifies Invalid.
func TestClaudeLiveCheckEnvInjectsCredential(t *testing.T) {
	runner := &envFakeRunner{
		fakeRunner: fakeRunner{
			results: []subprocess.Result{{Stderr: "401 Invalid authentication credentials"}},
			errs:    []error{errors.New("exit 1")},
		},
	}
	cred := []string{ClaudeOAuthTokenEnv + "=daemon-secret"}
	err := ClaudeLiveCheckEnv(context.Background(), runner, "", cred)
	if ClaudeClassifyProbe(err) != ClaudeTokenInvalid {
		t.Fatalf("ClaudeLiveCheckEnv error = %v, want classified invalid", err)
	}
	if len(runner.gotEnv) != 1 || runner.gotEnv[0] != cred[0] {
		t.Fatalf("probe env = %v, want the injected daemon credential %v", runner.gotEnv, cred)
	}
	runner.want(t, 0, "claude", "-p", "--output-format", "json", "--", ClaudeLiveCheckPrompt)
}

// TestClaudeLiveCheckClassifiesSocketClosed401AsInvalid486 is the #486 regression
// for the DOCUMENTED invalid-token symptom: an invalid CLAUDE_CODE_OAUTH_TOKEN
// manifests as `Failed to authenticate. API Error: 401 The socket connection was
// closed unexpectedly`. That string carries "authenticate" and "401" but NOT
// "authentication", so isClaudeAuthFailure misses it and the prior probe returned
// Unknown — which left `gitmoot doctor` reporting OK:true for the exact scenario
// the fix exists to catch. The isolated probe retries the 401-socket-closed
// signature once and, when it PERSISTS, must classify Invalid.
func TestClaudeLiveCheckClassifiesSocketClosed401AsInvalid486(t *testing.T) {
	const symptom = "Failed to authenticate. API Error: 401 The socket connection was closed unexpectedly"
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: symptom}, {Stderr: symptom}},
		errs:    []error{errors.New("exit 1"), errors.New("exit 1")},
	}
	err := ClaudeLiveCheck(context.Background(), runner, "/repo")
	if ClaudeClassifyProbe(err) != ClaudeTokenInvalid {
		t.Fatalf("socket-closed 401 classified %v, want invalid\nerr=%v", ClaudeClassifyProbe(err), err)
	}
	if !errors.Is(err, ErrClaudeAuthFailed) {
		t.Fatalf("persistent socket-closed 401 must satisfy errors.Is(err, ErrClaudeAuthFailed):\n%v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("probe made %d calls, want 2 (one retry of the 401-socket-closed)", len(runner.calls))
	}
}

// TestClaudeLiveCheckSocketClosed401RetryClears486 guards the other side: a one-off
// 401-socket-closed that CLEARS on the byte-identical retry is the concurrency
// transient the daemon path tolerates, so the isolated probe must report Valid, not
// flip a healthy token to Invalid.
func TestClaudeLiveCheckSocketClosed401RetryClears486(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "API Error: 401 The socket connection was closed unexpectedly"},
			{Stdout: `{"result":"OK"}`},
		},
		errs: []error{errors.New("exit 1"), nil},
	}
	if err := ClaudeLiveCheck(context.Background(), runner, "/repo"); err != nil {
		t.Fatalf("a cleared socket-closed retry must be valid, got %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("probe made %d calls, want 2 (the cleared retry)", len(runner.calls))
	}
}

// blockingRunner blocks until the context is cancelled, simulating a hung/slow
// `claude -p`, so a test can prove the probe bounds an unbounded caller.
type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ string, _ string, _ ...string) (subprocess.Result, error) {
	<-ctx.Done()
	return subprocess.Result{}, ctx.Err()
}

func (blockingRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }

// TestClaudeLiveCheckBoundsUnboundedContext486 proves the probe bounds an unbounded
// (context.Background) caller — the regression #486 introduced by always probing in
// doctor — so a stalled claude can't block forever. With a hung runner the probe
// returns within its own timeout, and the timeout maps to Unknown (never Invalid),
// so a SET token degrades to set-but-unvalidated rather than flipping doctor red.
func TestClaudeLiveCheckBoundsUnboundedContext486(t *testing.T) {
	old := ClaudeLiveProbeTimeout
	ClaudeLiveProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { ClaudeLiveProbeTimeout = old })
	done := make(chan error, 1)
	go func() { done <- ClaudeLiveCheck(context.Background(), blockingRunner{}, "/repo") }()
	select {
	case err := <-done:
		if ClaudeClassifyProbe(err) != ClaudeTokenUnknown {
			t.Fatalf("timed-out probe classified %v, want unknown\nerr=%v", ClaudeClassifyProbe(err), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ClaudeLiveCheck did not honor its bounded timeout (hung)")
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
