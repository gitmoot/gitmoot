package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// seedPersistedRuntimeAuth writes the 0600 daemon-runtime.env for home as if a
// prior token-bearing start had persisted it, and returns the home dir.
func seedPersistedRuntimeAuth(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(config.PathsForHome(home).Home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if err := persistDaemonRuntimeAuth(config.PathsForHome(home).Home, func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return secretToken, true
		}
		return "", false
	}); err != nil {
		t.Fatalf("seed persist: %v", err)
	}
	return home
}

// captureChildRecoveredEnv swaps the child-spawn seam to record the recoveredEnv
// the (re)start body computes and passes to the child, without launching a real
// daemon. It returns a pointer whose slice is set when the stub fires.
func captureChildRecoveredEnv(t *testing.T) *[]string {
	t.Helper()
	captured := new([]string)
	prev := startDaemonChildFn
	startDaemonChildFn = func(h, poll string, workers int, wsor, wi bool, scheduler, repo, session string, state daemonState, workDir string, extraEnv []string) (daemonMeta, error) {
		*captured = extraEnv
		return daemonMeta{PID: 424242, LogFile: filepath.Join(h, "daemon.log")}, nil
	}
	t.Cleanup(func() { startDaemonChildFn = prev })
	return captured
}

func childEnvHasToken(entries []string) bool {
	want := runtime.ClaudeOAuthTokenEnv + "=" + secretToken
	for _, e := range entries {
		if e == want {
			return true
		}
	}
	return false
}

// TestDaemonRestartOnlyRecovery_E2E is the #588 mutation-proven end-to-end proof
// that persisted runtime-auth recovery is RESTART-ONLY. It drives the real
// (re)start command body (runDaemonStartWithWorkDirRestart) with the child spawn
// stubbed and the auth-readiness seam seeded to an EMPTY live env — i.e. the
// launching shell lacks the token but the 0600 daemon-runtime.env holds it.
//
//   - RESTART (restart=true): the recoveredEnv handed to the child CONTAINS the
//     token AND the informational stale-token note is emitted (no drop warning).
//   - PLAIN START (restart=false): the recoveredEnv does NOT contain the token —
//     the operator's deliberately-unset token is not resurrected; the #581 drop
//     warning fires instead.
//
// The token VALUE must never appear on stdout/stderr in either case.
//
// MUTATION PROOF: in runDaemonStartWithWorkDirRestart, dropping the `if restart`
// gate (calling recoverDaemonChildAuthEnv on the plain-start path too) makes the
// plain-start subtest below go RED — the child env would then carry the token.
func TestDaemonRestartOnlyRecovery_E2E(t *testing.T) {
	t.Run("restart_recovers_token_and_notes_stale", func(t *testing.T) {
		home := seedPersistedRuntimeAuth(t)
		withClaudeAuthLookup(t, map[string]string{}) // launching shell lacks the token
		captured := captureChildRecoveredEnv(t)

		var stdout, stderr bytes.Buffer
		if code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", true, false, &stdout, &stderr); code != 0 {
			t.Fatalf("restart returned %d; stderr=%q", code, stderr.String())
		}

		if !childEnvHasToken(*captured) {
			t.Fatalf("RESTART: child env must carry the recovered token; got %v (redacted)", redactEnvKeys(*captured))
		}
		// Recovery substitutes an informational stale note for the #581 drop warning.
		if strings.Contains(stderr.String(), "WARNING") {
			t.Fatalf("RESTART recovery must not emit a drop WARNING; stderr=%q", stderr.String())
		}
		if !strings.Contains(stderr.String(), "STALE or REVOKED") {
			t.Fatalf("RESTART recovery must emit the stale-token note; stderr=%q", stderr.String())
		}
		// The stale note must steer operators at a LIVE validity probe (`gitmoot
		// doctor`), not at `daemon status`, which only confirms a token is SET
		// (not valid) and would report a revoked recovered token as "ok" — the
		// silent-auth-failure class #588 exists to eliminate.
		if !strings.Contains(stderr.String(), "gitmoot doctor") {
			t.Fatalf("stale note must recommend the live `gitmoot doctor` probe; stderr=%q", stderr.String())
		}
		assertNoTokenLeak(t, stdout.String(), stderr.String())
	})

	t.Run("plain_start_does_not_recover_token", func(t *testing.T) {
		home := seedPersistedRuntimeAuth(t)
		withClaudeAuthLookup(t, map[string]string{}) // launching shell lacks the token
		captured := captureChildRecoveredEnv(t)

		var stdout, stderr bytes.Buffer
		if code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", false, false, &stdout, &stderr); code != 0 {
			t.Fatalf("start returned %d; stderr=%q", code, stderr.String())
		}

		// The mutation-sensitive assertion: a plain start must NOT resurrect the
		// deliberately-unset token. Removing the restart-only gate flips this red.
		if childEnvHasToken(*captured) {
			t.Fatalf("PLAIN START must NOT recover the persisted token into the child env; got %v (redacted)", redactEnvKeys(*captured))
		}
		// With no auth and no recovery, the #581 drop warning must fire.
		if !strings.Contains(stderr.String(), "WARNING") {
			t.Fatalf("PLAIN START without auth should warn; stderr=%q", stderr.String())
		}
		assertNoTokenLeak(t, stdout.String(), stderr.String())
	})
}

// TestDaemonStopForgetRuntimeAuth_E2E proves the #588 `daemon stop
// --forget-runtime-auth` flag deletes the persisted 0600 file, while a plain
// `daemon stop` leaves it intact so a later restart can still recover it. No
// daemon is running (pid==0), which exercises the teardown branch deterministically.
func TestDaemonStopForgetRuntimeAuth_E2E(t *testing.T) {
	t.Run("plain_stop_keeps_file", func(t *testing.T) {
		home := seedPersistedRuntimeAuth(t)
		path := daemonRuntimeAuthFilePath(config.PathsForHome(home).Home)

		var stdout, stderr bytes.Buffer
		if code := runDaemonStop([]string{"--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("stop returned %d; stderr=%q", code, stderr.String())
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("plain stop must LEAVE the persisted file; stat err=%v", err)
		}
		assertNoTokenLeak(t, stdout.String(), stderr.String())
	})

	t.Run("forget_deletes_file", func(t *testing.T) {
		home := seedPersistedRuntimeAuth(t)
		path := daemonRuntimeAuthFilePath(config.PathsForHome(home).Home)

		var stdout, stderr bytes.Buffer
		if code := runDaemonStop([]string{"--home", home, "--forget-runtime-auth"}, &stdout, &stderr); code != 0 {
			t.Fatalf("stop --forget-runtime-auth returned %d; stderr=%q", code, stderr.String())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("--forget-runtime-auth must DELETE the persisted file; stat err=%v", err)
		}
		assertNoTokenLeak(t, stdout.String(), stderr.String())
	})
}
