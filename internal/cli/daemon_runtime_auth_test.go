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
}
