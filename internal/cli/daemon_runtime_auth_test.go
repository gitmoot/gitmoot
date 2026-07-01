package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// captureDaemonChildEnv swaps the child-spawn seam to record the extraEnv the
// (re)start body computes for the child, WITHOUT launching a real daemon. The
// child's actual environment is os.Environ()+extraEnv (see startDaemonChild), so
// capturing extraEnv is the load-bearing assertion for #578's recovery path.
func captureDaemonChildEnv(t *testing.T, captured *[]string) {
	t.Helper()
	prev := startDaemonChildFn
	startDaemonChildFn = func(home, poll string, workers int, watchSkillOptReviews, watchIssues bool, scheduler, repo, session string, state daemonState, workDir string, extraEnv []string) (daemonMeta, error) {
		*captured = append([]string{}, extraEnv...)
		return daemonMeta{PID: 777777, LogFile: home + "/daemon.log"}, nil
	}
	t.Cleanup(func() { startDaemonChildFn = prev })
}

// TestDaemonStartPersistsRuntimeAuth0600 (case a) drives the REAL start body
// with a token in the (seam-injected) env and asserts the 0600 file is written
// in the daemon home with mode 0600 and contains the token.
func TestDaemonStartPersistsRuntimeAuth0600(t *testing.T) {
	captureDaemonChildEnv(t, new([]string))
	const token = "sk-ant-oat01-persist-me-9f8e"
	withClaudeAuthLookup(t, map[string]string{runtime.ClaudeOAuthTokenEnv: token})

	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start returned %d; stderr=%q", code, stderr.String())
	}

	path := daemonRuntimeAuthFile(config.PathsForHome(home))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected persisted runtime-auth file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("runtime-auth file mode = %o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), runtime.ClaudeOAuthTokenEnv+"="+token) {
		t.Fatalf("persisted file should contain the token")
	}

	// SECURITY: the token must never leak to stdout/stderr.
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatalf("token leaked to daemon start output; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestDaemonRestartRecoversRuntimeAuthIntoChild (case b) proves recovery: with
// the token ABSENT from the launching env but PRESENT in the 0600 file, the
// computed child environment (captured via the seam) carries the token so the
// restarted daemon keeps Claude auth automatically.
func TestDaemonRestartRecoversRuntimeAuthIntoChild(t *testing.T) {
	const token = "sk-ant-oat01-recover-me-1a2b"
	home := t.TempDir()

	// Seed the 0600 file as if a prior authed start had persisted it.
	path := daemonRuntimeAuthFile(config.PathsForHome(home))
	if err := os.MkdirAll(config.PathsForHome(home).Home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if _, err := runtime.PersistRuntimeAuthEnv(path, func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return token, true
		}
		return "", false
	}); err != nil {
		t.Fatalf("seed persist: %v", err)
	}

	// The launching shell LACKS the token.
	withClaudeAuthLookup(t, map[string]string{})
	var captured []string
	captureDaemonChildEnv(t, &captured)

	var stdout, stderr bytes.Buffer
	code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", true, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("restart returned %d; stderr=%q", code, stderr.String())
	}

	want := runtime.ClaudeOAuthTokenEnv + "=" + token
	found := false
	for _, e := range captured {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("computed child env should recover the token; captured=%v", captured)
	}

	// SECURITY: the recovered token must never appear in the start output.
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatalf("token leaked to restart output; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	// #578 review (finding 1): recovery must NOT silently swallow the auth warning.
	// It re-injects a persisted token that has no expiry/validation, so it emits an
	// informational note that the token may be stale and how to invalidate it —
	// rather than the loud "WARNING" (which would falsely imply no auth) or silence.
	out := stderr.String()
	if strings.Contains(out, "WARNING") {
		t.Fatalf("recovery should not emit the loud auth-DROP WARNING; stderr=%q", out)
	}
	if !strings.Contains(out, "PERSISTED") || !strings.Contains(out, "STALE") {
		t.Fatalf("recovery should note the token is persisted and may be stale; stderr=%q", out)
	}
	if !strings.Contains(out, "--forget-runtime-auth") {
		t.Fatalf("recovery note should point at the invalidation path; stderr=%q", out)
	}
}

// TestDaemonPlainStartDoesNotRecoverRuntimeAuth (finding 2) proves recovery is
// RESTART-ONLY: a plain `daemon start` from a shell with the token intentionally
// unset must NOT re-inject a previously-persisted token, so an operator switching
// to a Codex/Kimi-only daemon has their intended env change honored.
func TestDaemonPlainStartDoesNotRecoverRuntimeAuth(t *testing.T) {
	const token = "sk-ant-oat01-do-not-recover-3c4d"
	home := t.TempDir()

	path := daemonRuntimeAuthFile(config.PathsForHome(home))
	if err := os.MkdirAll(config.PathsForHome(home).Home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if _, err := runtime.PersistRuntimeAuthEnv(path, func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return token, true
		}
		return "", false
	}); err != nil {
		t.Fatalf("seed persist: %v", err)
	}

	// The launching shell intentionally LACKS the token, and this is a plain start.
	withClaudeAuthLookup(t, map[string]string{})
	var captured []string
	captureDaemonChildEnv(t, &captured)

	var stdout, stderr bytes.Buffer
	code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start returned %d; stderr=%q", code, stderr.String())
	}

	for _, e := range captured {
		if strings.Contains(e, token) {
			t.Fatalf("plain start must NOT recover the persisted token into the child; captured=%v", captured)
		}
	}
	// With no auth in env and no recovery, the operator still gets the loud warning.
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Fatalf("plain start without auth should still warn; stderr=%q", stderr.String())
	}
}

// TestDaemonStopForgetRuntimeAuthRemovesFile (findings 1 & 2) proves the explicit
// invalidation path: `daemon stop --forget-runtime-auth` deletes the persisted
// 0600 file so a revoked/stale token is not recovered on the next restart.
func TestDaemonStopForgetRuntimeAuthRemovesFile(t *testing.T) {
	home := t.TempDir()
	path := daemonRuntimeAuthFile(config.PathsForHome(home))
	if err := os.MkdirAll(config.PathsForHome(home).Home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if _, err := runtime.PersistRuntimeAuthEnv(path, func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return "sk-ant-oat01-forget-me-5e6f", true
		}
		return "", false
	}); err != nil {
		t.Fatalf("seed persist: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("precondition: persisted file should exist: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runDaemonStop([]string{"--home", home, "--forget-runtime-auth"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("stop returned %d; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("persisted runtime-auth file should be removed, stat err=%v", err)
	}
	if !strings.Contains(stdout.String(), "removed persisted runtime auth") {
		t.Fatalf("stop should report the removal; stdout=%q", stdout.String())
	}
}
