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

// The refusal is GONE, deliberately. The #1810 exact-head review showed a
// preflight refusal was wrong twice over: it converted a DEFERRABLE
// runtime-auth blocker into a terminal failure plus a PR comment, against the
// engine's own classifyOperationalBlocker/job.deferred policy, and the auth
// overlay injected by the same commit means an expired staged snapshot no
// longer decides whether the seat can authenticate. What survives is the
// report: an expired snapshot must be RECORDED, never refused.
func TestReadOnlySeatCredentialPreflightReportsAndNeverRefuses(t *testing.T) {
	for name, test := range map[string]struct {
		expiresAt    int64
		refreshToken string
		wantPhrase   string
	}{
		"expired with no refresh token": {
			expiresAt: 0, refreshToken: "",
			wantPhrase: "carries no refresh token",
		},
		"expired but refreshable": {
			expiresAt: time.Now().UTC().Add(-time.Hour).UnixMilli(), refreshToken: "r",
			wantPhrase: "must be refreshed by the runtime",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeClaudeCredential(t, dir, test.expiresAt, test.refreshToken)
			agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

			diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, time.Now().UTC())
			if diagnosis == "" {
				t.Fatal("an expired staged credential must be recorded")
			}
			if !strings.Contains(diagnosis, test.wantPhrase) {
				t.Fatalf("diagnosis %q must contain %q", diagnosis, test.wantPhrase)
			}
			if !strings.Contains(diagnosis, "rather than a refusal") {
				t.Fatalf("diagnosis %q must say it is not a refusal", diagnosis)
			}
			// The host credential path is NOT published: this string reaches a job
			// event and previously reached a PR comment.
			if strings.Contains(diagnosis, dir) {
				t.Fatalf("diagnosis %q leaks the absolute host credential path", diagnosis)
			}
		})
	}
}

// Gateway mode stages no credential file for a seat, so there is nothing to
// diagnose and the check must stay silent.
func TestReadOnlySeatCredentialPreflightSilentInGatewayMode(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, 0, "")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
	if diagnosis := readOnlySeatCredentialPreflight(agent, dir, true, time.Now().UTC()); diagnosis != "" {
		t.Fatalf("gateway mode diagnosis=%q, want silence", diagnosis)
	}
}

// A live credential must pass silently. A check that fires on a valid account
// would be worse than no check: it would refuse working reviews.
func TestReadOnlySeatCredentialPreflightPassesALiveCredential(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, time.Now().UTC().Add(8*time.Hour).UnixMilli(), "refresh-token")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

	if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, time.Now().UTC()); diagnosis != "" {
		t.Fatalf("live credential: diagnosis=%q, want silence", diagnosis)
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
		if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
		}
	})
	t.Run("runtime with no readable expiry", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.CodexRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
		}
	})
	t.Run("absent credential file", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, t.TempDir(), false, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
		}
	})
	t.Run("unparseable credential file", func(t *testing.T) {
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, ".credentials.json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, broken, false, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
		}
	})
	t.Run("no source dir", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, "  ", false, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
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
	if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, time.Now().UTC()); diagnosis != "" {
		t.Fatalf("absent expiry: diagnosis=%q, want silence", diagnosis)
	}

	// ...while an EXPLICIT zero is still reported, because that value is what a
	// failed refresh writes back rather than an absent field.
	writeClaudeCredential(t, dir, 0, "")
	if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, time.Now().UTC()); diagnosis == "" {
		t.Fatal("an explicit expiresAt of 0 must still be reported")
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

// writeFileForTest writes a raw .credentials.json body for cases that need a
// shape writeClaudeCredential cannot express.
func writeFileForTest(dir, body string) error {
	return os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600)
}
