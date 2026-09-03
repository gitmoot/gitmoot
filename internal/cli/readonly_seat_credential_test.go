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

// The refusal is GONE, deliberately. A preflight refusal converts the
// DEFERRABLE runtime-auth blocker into a terminal failure plus a PR comment.
// A resolved auth overlay may also make an expired snapshot non-decisive. What
// survives is the report: an expired snapshot must be RECORDED, never refused,
// and must say whether an overlay was actually available.
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

			diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, true, time.Now().UTC())
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
	if diagnosis := readOnlySeatCredentialPreflight(agent, dir, true, true, time.Now().UTC()); diagnosis != "" {
		t.Fatalf("gateway mode diagnosis=%q, want silence", diagnosis)
	}
}

// A live credential must pass silently. A check that fires on a valid account
// would be worse than no check: it would refuse working reviews.
func TestReadOnlySeatCredentialPreflightPassesALiveCredential(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, time.Now().UTC().Add(8*time.Hour).UnixMilli(), "refresh-token")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

	if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, true, time.Now().UTC()); diagnosis != "" {
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
		if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, true, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
		}
	})
	t.Run("runtime with no readable expiry", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.CodexRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, true, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
		}
	})
	t.Run("absent credential file", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, t.TempDir(), false, true, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
		}
	})
	t.Run("unparseable credential file", func(t *testing.T) {
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, ".credentials.json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, broken, false, true, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence", diagnosis)
		}
	})
	t.Run("no source dir", func(t *testing.T) {
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, "  ", false, true, time.Now().UTC()); diagnosis != "" {
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
	if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, true, time.Now().UTC()); diagnosis != "" {
		t.Fatalf("absent expiry: diagnosis=%q, want silence", diagnosis)
	}

	// ...while an EXPLICIT zero is still reported, because that value is what a
	// failed refresh writes back rather than an absent field.
	writeClaudeCredential(t, dir, 0, "")
	if diagnosis := readOnlySeatCredentialPreflight(agent, dir, false, true, time.Now().UTC()); diagnosis == "" {
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

// The overlay was resolved by READING runtime-auth.env only, so on a host that
// authenticates from ambient credentials and never wrote that file the seat
// received nothing and the whole fix was inert (#1810 review F1). The seat path
// must bootstrap exactly as runtimeJobRunnerWithAuth does.
func TestReadOnlySeatRuntimeAuthEnvBootstrapsAmbientCredentials(t *testing.T) {
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	// No runtime-auth.env exists; the credential lives only in the environment.
	if _, err := os.Stat(runtimeAuthFilePath(paths.Home)); !os.IsNotExist(err) {
		t.Fatalf("fixture already has a runtime auth file: %v", err)
	}
	original := runtimeAuthEnvLookup
	runtimeAuthEnvLookup = func(name string) (string, bool) {
		if name == runtime.ClaudeOAuthTokenEnv {
			return "ambient-seat-token-long-enough", true
		}
		return "", false
	}
	t.Cleanup(func() { runtimeAuthEnvLookup = original })

	env, err := readOnlySeatRuntimeAuthEnv(home, runtime.ClaudeRuntime, false)
	if err != nil {
		t.Fatalf("readOnlySeatRuntimeAuthEnv: %v", err)
	}
	if !containsEnv(env, runtime.ClaudeOAuthTokenEnv+"=ambient-seat-token-long-enough") {
		t.Fatalf("seat overlay is empty on an ambient-auth host: %v", redactEnvNames(env))
	}
}

// A host authenticated only through Claude's credential store has no managed
// environment value to bootstrap. The overlay must remain empty, and callers
// must not claim it repaired the staged snapshot.
func TestReadOnlySeatRuntimeAuthEnvDoesNotInventCredentialStoreOverlay(t *testing.T) {
	home := t.TempDir()
	original := runtimeAuthEnvLookup
	runtimeAuthEnvLookup = func(string) (string, bool) { return "", false }
	t.Cleanup(func() { runtimeAuthEnvLookup = original })

	env, err := readOnlySeatRuntimeAuthEnv(home, runtime.ClaudeRuntime, false)
	if err != nil {
		t.Fatalf("readOnlySeatRuntimeAuthEnv: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("credential-store host produced invented overlay names: %v", redactEnvNames(env))
	}
	if _, err := os.Stat(runtimeAuthFilePath(home)); !os.IsNotExist(err) {
		t.Fatalf("credential-store bootstrap unexpectedly wrote runtime auth: %v", err)
	}
}

// Produce jobs authenticate as the account selected by CLAUDE_CONFIG_DIR, but
// their Landlock grant must never make that live operator profile writable.
// Mutable runtime state stays under a job-private root.
func TestProduceRuntimeSandboxGrantsReadButDoNotWriteConfiguredClaudeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configured := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configured)
	stateRoot := t.TempDir()
	reads, _, writes, env, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, stateRoot, nil, nil, nil)
	if err != nil {
		t.Fatalf("produceRuntimeSandboxGrants: %v", err)
	}
	writeState := filepath.Join(stateRoot, ".claude")
	if !containsEnv(env, "CLAUDE_CONFIG_DIR="+configured) {
		t.Fatalf("produce env = %v, want configured account %q", env, configured)
	}
	if !containsPath(reads, configured) {
		t.Fatalf("produce reads = %v, want configured account %q readable", reads, configured)
	}
	if !containsPath(writes, writeState) {
		t.Fatalf("produce writes = %v, want mutable state under %q", writes, writeState)
	}
	if containsPath(writes, configured) {
		t.Fatalf("produce writes = %v, must not grant live configured dir %q", writes, configured)
	}

	for _, invalid := range []string{"relative-claude", "~someone/claude"} {
		t.Setenv("CLAUDE_CONFIG_DIR", invalid)
		if _, _, _, _, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, stateRoot, nil, nil, nil); err == nil {
			t.Fatalf("CLAUDE_CONFIG_DIR=%q must be refused by the produce path", invalid)
		}
	}
}

func TestJobProduceRuntimeStateDirIsPrivatePerJob(t *testing.T) {
	home := t.TempDir()
	first, err := jobProduceRuntimeStateDir(home, "job-a", runtime.ClaudeRuntime)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := jobProduceRuntimeStateDir(home, "job-a", runtime.ClaudeRuntime)
	if err != nil {
		t.Fatal(err)
	}
	second, err := jobProduceRuntimeStateDir(home, "job-b", runtime.ClaudeRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if first != repeated {
		t.Fatalf("same job resolved to %q then %q", first, repeated)
	}
	if first == second {
		t.Fatalf("distinct jobs share runtime state %q", first)
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(paths.Home, "cache", "produce-runtime")
	if !strings.HasPrefix(first, wantRoot+string(filepath.Separator)) {
		t.Fatalf("job state %q is outside private root %q", first, wantRoot)
	}
}

// writeFileForTest writes a raw .credentials.json body for cases that need a
// shape writeClaudeCredential cannot express.
func writeClaudeCredentialBody(dir, body string) error {
	return os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600)
}

// The message must not claim an overlay the seat does not have. On a host with
// no runtime-auth.env and no ambient credentials the overlay is empty, and
// asserting otherwise misdirects the reader on exactly the host where this
// diagnosis matters (#1810 review, round 2).
func TestReadOnlySeatCredentialPreflightTellsTheTruthAboutTheOverlay(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, 0, "")
	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}

	withOverlay := readOnlySeatCredentialPreflight(agent, dir, false, true, time.Now().UTC())
	if !strings.Contains(withOverlay, "also carries the resolved runtime auth") {
		t.Fatalf("with an overlay the message must say so: %q", withOverlay)
	}
	without := readOnlySeatCredentialPreflight(agent, dir, false, false, time.Now().UTC())
	if !strings.Contains(without, "NO resolved runtime auth") {
		t.Fatalf("without an overlay the message must say so: %q", without)
	}
	if strings.Contains(without, "also carries the resolved runtime auth") {
		t.Fatalf("message claims an overlay that does not exist: %q", without)
	}
}

// A seat with no configured dir stages from the HOST DEFAULT, so every caller
// that inspects or grants a runtime profile must resolve the same way the
// staging code does: host default, ~ expansion, absolute, symlinks resolved.
func TestResolveRuntimeConfigDirMatchesTheStagingContract(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for runtimeName, want := range map[string]string{
		runtime.ClaudeRuntime: filepath.Join(home, ".claude"),
		runtime.CodexRuntime:  filepath.Join(home, ".codex"),
		runtime.KimiRuntime:   filepath.Join(home, ".kimi-code"),
	} {
		got, err := resolveRuntimeConfigDir(runtimeName, "")
		if err != nil || got != want {
			t.Fatalf("%s default = %q err=%v, want %q", runtimeName, got, err, want)
		}
	}
	if got, err := resolveRuntimeConfigDir(runtime.ShellRuntime, ""); err != nil || got != "" {
		t.Fatalf("shell default = %q err=%v, want empty", got, err)
	}

	// A ~ path belongs to the CURRENT user; a ~user path is refused rather than
	// silently mapped under this user's home, which would grant and inspect the
	// wrong account.
	if got, err := resolveRuntimeConfigDir(runtime.ClaudeRuntime, "~/claude-13"); err != nil || got != filepath.Join(home, "claude-13") {
		t.Fatalf("~ expansion = %q err=%v", got, err)
	}
	if _, err := resolveRuntimeConfigDir(runtime.ClaudeRuntime, "~someone/claude"); err == nil {
		t.Fatal("~user expansion must be refused, not mapped under this user's home")
	}

	// A relative path is REFUSED, not repaired: resolving it against the daemon's
	// working directory both grants the wrong profile and creates a stray one
	// there. Nothing must be created as a side effect of the refusal.
	if _, err := resolveRuntimeConfigDir(runtime.ClaudeRuntime, "relative-claude"); err == nil {
		t.Fatal("a relative runtime state directory must be refused")
	}
	if _, err := os.Stat("relative-claude"); err == nil {
		t.Fatal("refusing a relative dir still created it in the working directory")
	}

	// Symlinks are resolved so the inspected file is the staged file. Without
	// this, a symlinked profile reports on a path the seat never reads.
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "profile-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveRuntimeConfigDir(runtime.ClaudeRuntime, link)
	if err != nil || got != resolvedTarget {
		t.Fatalf("symlinked dir = %q err=%v, want %q", got, err, resolvedTarget)
	}
	writeClaudeCredential(t, target, 0, "")
	if path := readOnlySeatCredentialStatePath(runtime.ClaudeRuntime, link); path != filepath.Join(resolvedTarget, ".credentials.json") {
		t.Fatalf("state path = %q, want the resolved staged file", path)
	}
}
