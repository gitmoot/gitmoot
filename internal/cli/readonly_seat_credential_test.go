package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

func writeClaudeCredential(t *testing.T, dir string, expiresAtMillis int64, refreshToken string) {
	t.Helper()
	body := `{"claudeAiOauth":{"accessToken":"a","expiresAt":` +
		strconv.FormatInt(expiresAtMillis, 10) + `,"refreshToken":"` + refreshToken + `"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A credential that expired with NO refresh token cannot authenticate: no valid
// access token and no way to get one. Launching a runtime on it can only fail,
// so the seat refuses first, and the error names the file and the expiry rather
// than leaving the reader with the runtime's "OAuth session expired" wording.
// This is the state /root/.claude has been in since 2026-08-31.
func TestReadOnlySeatCredentialPreflightRefusesTheUnusableCase(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, 0, "")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

	diagnosis, err := readOnlySeatCredentialPreflight(agent, dir, time.Now().UTC())
	if err == nil {
		t.Fatal("an expired credential with no refresh token must be refused before launch")
	}
	if diagnosis != "" {
		t.Fatalf("refusal must not also return a diagnosis, got %q", diagnosis)
	}
	for _, want := range []string{".credentials.json", "no refresh token", "re-login"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %q", err.Error(), want)
		}
	}
}

// Expired WITH a refresh token is the state the daemon's account was in at
// 06:31:46Z. Refreshing is the runtime's job and normally succeeds, so refusing
// would break every host whose credential refreshes fine. Record the fact
// instead, naming the source and the expiry.
func TestReadOnlySeatCredentialPreflightDiagnosesTheRefreshableCase(t *testing.T) {
	dir := t.TempDir()
	expiry := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeClaudeCredential(t, dir, expiry.UnixMilli(), "refresh-token")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

	diagnosis, err := readOnlySeatCredentialPreflight(agent, dir, time.Now().UTC())
	if err != nil {
		t.Fatalf("a refreshable credential must not be refused: %v", err)
	}
	if diagnosis == "" {
		t.Fatal("an expired refreshable credential must be recorded, not silently staged")
	}
	for _, want := range []string{dir, expiry.Format(time.RFC3339), "staged snapshot"} {
		if !strings.Contains(diagnosis, want) {
			t.Fatalf("diagnosis %q must name %q", diagnosis, want)
		}
	}
}

// A live credential must pass silently. A check that fires on a valid account
// would be worse than no check: it would refuse working reviews.
func TestReadOnlySeatCredentialPreflightPassesALiveCredential(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, time.Now().UTC().Add(8*time.Hour).UnixMilli(), "refresh-token")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

	diagnosis, err := readOnlySeatCredentialPreflight(agent, dir, time.Now().UTC())
	if err != nil || diagnosis != "" {
		t.Fatalf("live credential: diagnosis=%q err=%v, want silence", diagnosis, err)
	}
}

// Only a seat is checked, and only where an expiry is actually readable. A
// non-seat job, another runtime, an absent file and an unparseable file all pass
// through: a missing input is the separate staging concern, and asserting on a
// credential format gitmoot has not measured would be a guess.
func TestReadOnlySeatCredentialPreflightStaysSilentWhereItCannotAssert(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, 0, "")

	t.Run("non-seat job", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime}
		if diagnosis, err := readOnlySeatCredentialPreflight(agent, dir, time.Now().UTC()); err != nil || diagnosis != "" {
			t.Fatalf("diagnosis=%q err=%v, want silence", diagnosis, err)
		}
	})
	t.Run("runtime with no readable expiry", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.CodexRuntime, ReadOnlySeat: true}
		if diagnosis, err := readOnlySeatCredentialPreflight(agent, dir, time.Now().UTC()); err != nil || diagnosis != "" {
			t.Fatalf("diagnosis=%q err=%v, want silence", diagnosis, err)
		}
	})
	t.Run("absent credential file", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis, err := readOnlySeatCredentialPreflight(agent, t.TempDir(), time.Now().UTC()); err != nil || diagnosis != "" {
			t.Fatalf("diagnosis=%q err=%v, want silence", diagnosis, err)
		}
	})
	t.Run("unparseable credential file", func(t *testing.T) {
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, ".credentials.json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis, err := readOnlySeatCredentialPreflight(agent, broken, time.Now().UTC()); err != nil || diagnosis != "" {
			t.Fatalf("diagnosis=%q err=%v, want silence", diagnosis, err)
		}
	})
	t.Run("no source dir", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis, err := readOnlySeatCredentialPreflight(agent, "  ", time.Now().UTC()); err != nil || diagnosis != "" {
			t.Fatalf("diagnosis=%q err=%v, want silence", diagnosis, err)
		}
	})
}

// An absent expiresAt is not evidence of expiry. Three existing seat tests
// legitimately stage a credential carrying only an accessToken, and conflating
// "no expiry declared" with "expired" refused all three jobs. An explicit 0 is
// different: that is what a FAILED refresh writes back.
func TestReadOnlySeatCredentialPreflightIgnoresAnAbsentExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"host"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
	diagnosis, err := readOnlySeatCredentialPreflight(agent, dir, time.Now().UTC())
	if err != nil || diagnosis != "" {
		t.Fatalf("absent expiry: diagnosis=%q err=%v, want silence", diagnosis, err)
	}

	// ...while an EXPLICIT zero still refuses.
	writeClaudeCredential(t, dir, 0, "")
	if _, err := readOnlySeatCredentialPreflight(agent, dir, time.Now().UTC()); err == nil {
		t.Fatal("an explicit expiresAt of 0 must still be refused")
	}
}

// The seat must carry the SAME resolved runtime auth every other job gets. It
// did not, which is why claude reviews failed while `auth probe claude` stayed
// green: the probe reads the resolved auth, the seat never received it.
func TestReadOnlySeatRuntimeAuthEnvCarriesTheResolvedOverlay(t *testing.T) {
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeAuthFilePath(paths.Home),
		[]byte("CLAUDE_CODE_OAUTH_TOKEN=seat-token-long-enough-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env, err := readOnlySeatRuntimeAuthEnv(home, runtime.ClaudeRuntime, false)
	if err != nil {
		t.Fatalf("readOnlySeatRuntimeAuthEnv: %v", err)
	}
	if !containsEnv(env, "CLAUDE_CODE_OAUTH_TOKEN=seat-token-long-enough-value") {
		t.Fatalf("seat auth env = %v, want the resolved token", env)
	}

	// Gateway mode holds the credential itself, so a seat is given none.
	gateway, err := readOnlySeatRuntimeAuthEnv(home, runtime.ClaudeRuntime, true)
	if err != nil || len(gateway) != 0 {
		t.Fatalf("gateway seat auth env = %v err=%v, want none", gateway, err)
	}

	// Other runtimes resolve their auth elsewhere; this must not invent any.
	other, err := readOnlySeatRuntimeAuthEnv(home, runtime.CodexRuntime, false)
	if err != nil || len(other) != 0 {
		t.Fatalf("codex seat auth env = %v err=%v, want none", other, err)
	}
}

// An operator-set CLAUDE_CONFIG_DIR names the ACCOUNT the engine runs as.
// produceRuntimeSandboxGrants hard-coded $HOME/.claude and silently replaced
// it, so a daemon configured onto a live account still pointed produce jobs at
// /root/.claude, which on this host has carried expiresAt 0 with no refresh
// token since 2026-08-31 and yields exactly "OAuth session expired and could
// not be refreshed".
func TestProduceRuntimeSandboxGrantsHonorsConfiguredClaudeDir(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "claude-13")
	t.Setenv("CLAUDE_CONFIG_DIR", configured)

	_, _, writes, env, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, nil, nil, nil)
	if err != nil {
		t.Fatalf("produceRuntimeSandboxGrants: %v", err)
	}
	if !containsEnv(env, "CLAUDE_CONFIG_DIR="+configured) {
		t.Fatalf("produce env = %v, want the configured dir %q", env, configured)
	}
	if !containsPath(writes, configured) {
		t.Fatalf("produce writes = %v, want the configured dir writable", writes)
	}

	// With none set it still falls back to the host default.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, fallbackEnv, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, nil, nil, nil)
	if err != nil {
		t.Fatalf("produceRuntimeSandboxGrants fallback: %v", err)
	}
	if !containsEnv(fallbackEnv, "CLAUDE_CONFIG_DIR="+filepath.Join(filepath.Clean(home), ".claude")) {
		t.Fatalf("fallback env = %v, want $HOME/.claude", fallbackEnv)
	}
}
