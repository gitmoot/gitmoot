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

// secretToken is a distinctive value so tests can assert it NEVER leaks into
// stdout/stderr/log while still confirming it lands in the 0600 file / child env.
const secretToken = "sk-ant-oat01-SECRET-DO-NOT-LOG-abcdef0123456789"

func runtimeAuthFileFor(t *testing.T, home string) string {
	t.Helper()
	return daemonRuntimeAuthFilePath(config.PathsForHome(home).Home)
}

// TestDaemonStartPersistsRuntimeAuth_0600 — scenario (a): starting with a token
// in the environment writes the owner-only file with mode 0600 whose contents
// carry the token, and the token never appears on stdout/stderr.
func TestDaemonStartPersistsRuntimeAuth_0600(t *testing.T) {
	withStubbedDaemonChild(t)
	withClaudeAuthLookup(t, map[string]string{runtime.ClaudeOAuthTokenEnv: secretToken})
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("start returned %d; stderr=%q", code, stderr.String())
	}

	path := runtimeAuthFileFor(t, home)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected persisted runtime-auth file: %v", err)
	}
	if got := info.Mode().Perm(); got != daemonRuntimeAuthFilePerm {
		t.Fatalf("runtime-auth file mode = %o, want %o (owner read/write only)", got, daemonRuntimeAuthFilePerm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	if !strings.Contains(string(data), runtime.ClaudeOAuthTokenEnv+"="+secretToken) {
		t.Fatalf("persisted file should carry the token line; got %q", string(data))
	}

	// The token must NEVER reach the world-readable daemon.json meta.
	if meta, err := os.ReadFile(filepath.Join(config.PathsForHome(home).Home, "daemon.json")); err == nil {
		assertNoTokenLeak(t, string(meta))
	}
	assertNoTokenLeak(t, stdout.String(), stderr.String())
}

// TestDaemonStartRecoversRuntimeAuthIntoChildEnv — scenario (b): with the token
// ABSENT from the launching environment but PRESENT in the persisted file, the
// computed child environment (captured via the startDaemonChildFn seam) carries
// the token, and it never appears on stdout/stderr.
func TestDaemonStartRecoversRuntimeAuthIntoChildEnv(t *testing.T) {
	home := t.TempDir()
	// Seed a persisted token as if a prior token-bearing start had written it.
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

	// Launching shell LACKS any runtime auth token.
	withClaudeAuthLookup(t, map[string]string{})

	var capturedEnv []string
	prev := startDaemonChildFn
	startDaemonChildFn = func(h, poll string, workers int, wsor, wi bool, scheduler, repo, session string, state daemonState, workDir string, extraEnv []string) (daemonMeta, error) {
		capturedEnv = extraEnv
		return daemonMeta{PID: 424242, LogFile: filepath.Join(h, "daemon.log")}, nil
	}
	t.Cleanup(func() { startDaemonChildFn = prev })

	var stdout, stderr bytes.Buffer
	if code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("start returned %d; stderr=%q", code, stderr.String())
	}

	want := runtime.ClaudeOAuthTokenEnv + "=" + secretToken
	found := false
	for _, e := range capturedEnv {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("child env should carry the recovered token entry %q; got %v (redacted)", runtime.ClaudeOAuthTokenEnv, redactEnvKeys(capturedEnv))
	}
	// Recovery replaces the #581 warning; the drop warning must NOT fire.
	if strings.Contains(stderr.String(), "WARNING") {
		t.Fatalf("recovered auth should suppress the drop warning; stderr=%q", stderr.String())
	}
	assertNoTokenLeak(t, stdout.String(), stderr.String())
}

// TestPersistDaemonRuntimeAuth_NeverOverwritesGoodTokenWithEmpty — invariant (3):
// an environment with no runtime auth token must not clobber a previously
// persisted good token.
func TestPersistDaemonRuntimeAuth_NeverOverwritesGoodTokenWithEmpty(t *testing.T) {
	dir := t.TempDir()
	// First persist a good token.
	if err := persistDaemonRuntimeAuth(dir, func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return secretToken, true
		}
		return "", false
	}); err != nil {
		t.Fatalf("first persist: %v", err)
	}
	// Now persist with an empty env: the file must be untouched.
	if err := persistDaemonRuntimeAuth(dir, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("empty persist: %v", err)
	}
	got := loadDaemonRuntimeAuthFile(daemonRuntimeAuthFilePath(dir))
	if got[runtime.ClaudeOAuthTokenEnv] != secretToken {
		t.Fatalf("empty env clobbered the persisted token; got %v (redacted keys=%v)", got[runtime.ClaudeOAuthTokenEnv] != "", keysOf(got))
	}
}

// TestPersistDaemonRuntimeAuth_PrefersLiveEnvOverFile — invariant (3): when a var
// is set in both env and file, the live value wins.
func TestPersistDaemonRuntimeAuth_PrefersLiveEnvOverFile(t *testing.T) {
	dir := t.TempDir()
	if err := persistDaemonRuntimeAuth(dir, func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return "old-token", true
		}
		return "", false
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := persistDaemonRuntimeAuth(dir, func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return "new-token", true
		}
		return "", false
	}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got := loadDaemonRuntimeAuthFile(daemonRuntimeAuthFilePath(dir))
	if got[runtime.ClaudeOAuthTokenEnv] != "new-token" {
		t.Fatalf("live env should win over file; got %q", got[runtime.ClaudeOAuthTokenEnv])
	}
}

// TestRecoverDaemonChildAuthEnv_PrefersLiveEnv — a var present in the live env is
// NOT re-injected (the child inherits it), so recovery only fills genuine gaps.
func TestRecoverDaemonChildAuthEnv_PrefersLiveEnv(t *testing.T) {
	dir := t.TempDir()
	if err := persistDaemonRuntimeAuth(dir, func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return secretToken, true
		}
		return "", false
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	live := func(k string) (string, bool) {
		if k == runtime.ClaudeOAuthTokenEnv {
			return "live-value", true
		}
		return "", false
	}
	if got := recoverDaemonChildAuthEnv(dir, live); len(got) != 0 {
		t.Fatalf("live-present var must not be re-injected; got %v (redacted)", redactEnvKeys(got))
	}
}

// TestPersistDaemonRuntimeAuth_NoTokenNoFile — invariant (3): with no token the
// file is not created at all.
func TestPersistDaemonRuntimeAuth_NoTokenNoFile(t *testing.T) {
	dir := t.TempDir()
	if err := persistDaemonRuntimeAuth(dir, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := os.Stat(daemonRuntimeAuthFilePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("no-token persist should not create the file; stat err=%v", err)
	}
}

// assertNoTokenLeak fails if the secret token appears in any captured diagnostic
// stream — the SECURITY-CRITICAL non-leak requirement.
func assertNoTokenLeak(t *testing.T, streams ...string) {
	t.Helper()
	for _, s := range streams {
		if strings.Contains(s, secretToken) {
			t.Fatalf("token leaked into diagnostic output: %q", s)
		}
	}
}

func redactEnvKeys(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if k, _, ok := strings.Cut(e, "="); ok {
			out = append(out, k+"=<redacted>")
		} else {
			out = append(out, "<redacted>")
		}
	}
	return out
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
