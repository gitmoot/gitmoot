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

// Produce jobs authenticate as the account selected by CLAUDE_CONFIG_DIR, and
// the Claude CLI writes inside its own config dir during normal operation, so
// a read-only grant on the operator profile denies writes the runtime needs
// (#1810 review round 4, P1). The contract is therefore: stage the configured
// account into a job-private profile, point the runtime at that writable copy,
// and keep the operator profile out of the sandbox entirely.
func TestProduceRuntimeSandboxGrantsStageConfiguredAccountIntoWritableJobPrivateProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configured := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configured)
	if err := os.WriteFile(filepath.Join(configured, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"operator-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	reads, _, writes, env, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, stateRoot, nil, nil, nil)
	if err != nil {
		t.Fatalf("produceRuntimeSandboxGrants: %v", err)
	}
	writeState := filepath.Join(stateRoot, ".claude")
	cacheHome := filepath.Join(stateRoot, "xdg-cache")
	if !containsEnv(env, "CLAUDE_CONFIG_DIR="+writeState) {
		t.Fatalf("produce env = %v, want the runtime pointed at writable %q", env, writeState)
	}
	if !containsEnv(env, "XDG_CACHE_HOME="+cacheHome) {
		t.Fatalf("produce env = %v, want the node cache redirected to %q", env, cacheHome)
	}
	if !containsPath(writes, writeState) || !containsPath(writes, cacheHome) {
		t.Fatalf("produce writes = %v, want both %q and %q writable", writes, writeState, cacheHome)
	}
	if containsPath(writes, configured) {
		t.Fatalf("produce writes = %v, must not grant live configured dir %q", writes, configured)
	}
	if containsPath(reads, configured) {
		t.Fatalf("produce reads = %v, must not expose live configured dir %q to the sandbox", reads, configured)
	}
	// The account must be the CONFIGURED one: staging content, not just a path.
	staged, err := os.ReadFile(filepath.Join(writeState, ".credentials.json"))
	if err != nil {
		t.Fatalf("read staged credential: %v", err)
	}
	if !strings.Contains(string(staged), "operator-account") {
		t.Fatalf("staged credential = %s, want the configured account", staged)
	}

	for _, invalid := range []string{"relative-claude", "~someone/claude"} {
		t.Setenv("CLAUDE_CONFIG_DIR", invalid)
		if _, _, _, _, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, stateRoot, nil, nil, nil); err == nil {
			t.Fatalf("CLAUDE_CONFIG_DIR=%q must be refused by the produce path", invalid)
		}
	}
}

// A profile that does not exist yet is a VALID configuration - a fresh host, or
// a configured dir the operator has not created. Round 4 of the #1810 review
// reproduced a hard failure here ("sandbox read path ...: no such file or
// directory") for both the configured and the default case.
func TestProduceRuntimeSandboxGrantsAcceptMissingProfile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(t *testing.T, home string)
	}{
		{name: "configured-missing", configure: func(t *testing.T, home string) {
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "never-created-claude"))
		}},
		{name: "unset-host-default-missing", configure: func(t *testing.T, _ string) {
			t.Setenv("CLAUDE_CONFIG_DIR", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			tc.configure(t, home)
			stateRoot := t.TempDir()
			reads, _, writes, env, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, stateRoot, nil, nil, nil)
			if err != nil {
				t.Fatalf("a missing profile must not fail the job: %v", err)
			}
			writeState := filepath.Join(stateRoot, ".claude")
			if !containsEnv(env, "CLAUDE_CONFIG_DIR="+writeState) || !containsPath(writes, writeState) {
				t.Fatalf("grants = reads %v writes %v env %v, want a writable job-private profile", reads, writes, env)
			}
			if _, err := os.Stat(writeState); err != nil {
				t.Fatalf("job-private profile was not created: %v", err)
			}
		})
	}
}

// M7: the guard that refuses an unplumbed caller. A produce job with no
// job-private root would otherwise fall back to granting shared state.
func TestProduceRuntimeSandboxGrantsRefuseMissingStateRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, root := range []string{"", "   "} {
		_, _, _, _, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, root, nil, nil, nil)
		if err == nil {
			t.Fatalf("state root %q must be refused", root)
		}
		if !strings.Contains(err.Error(), "job-private runtime state root") {
			t.Fatalf("error = %v, want it to name the missing job-private root", err)
		}
	}
}

// M8: each dispatch resets its own state root, so a previous run's runtime
// state - including a stale credential copy - cannot survive into the next.
func TestProduceRuntimeSandboxGrantsResetStaleRuntimeState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stateRoot := t.TempDir()
	stale := filepath.Join(stateRoot, ".claude", "stale-session.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(`{"from":"previous run"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, stateRoot, nil, nil, nil); err != nil {
		t.Fatalf("produceRuntimeSandboxGrants: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale runtime state survived the reset: %v", err)
	}
}

// P3 round 4: two concurrent dispatches of the SAME job id - a lease takeover,
// or a deferral re-dispatch - must not share a state root, because each resets
// its root at grant time and removes it when it finishes.
func TestNewProduceRunStateDirIsUniquePerDispatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "job-root")
	first, err := newProduceRunStateDir(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newProduceRunStateDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("concurrent dispatches share runtime state %q", first)
	}
	for _, dir := range []string{first, second} {
		if filepath.Dir(dir) != filepath.Clean(root) {
			t.Fatalf("run dir %q is outside the job root %q", dir, root)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("run dir %q was not created: %v", dir, err)
		}
	}
	// Removing one dispatch's state must leave a live sibling untouched.
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("sibling dispatch state was destroyed: %v", err)
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

// Round 2 of the #1810 review measured four realistic operator profiles that
// hard-failed EVERY Claude produce job once produce started parsing the profile:
// a symlinked settings.json (chezmoi/stow/yadm), an empty one, one holding a
// JSON array, and a symlinked .credentials.json. Base never opened these files,
// so each was a regression introduced by the staging fix. settings.json is
// configuration the runtime has defaults for, so an unusable one is skipped;
// .credentials.json decides which account runs, so an unusable one still fails -
// but a SYMLINK is resolved rather than rejected in both cases.
func TestProduceStagingToleratesRealisticOperatorProfileShapes(t *testing.T) {
	object := `{"claudeAiOauth":{"accessToken":"operator-account"}}`
	for _, tc := range []struct {
		name        string
		build       func(t *testing.T, configured string)
		wantErr     bool
		wantStaged  []string
		wantSkipped []string
	}{
		{name: "plain-objects", build: func(t *testing.T, configured string) {
			writeProfileFile(t, configured, ".credentials.json", object)
			writeProfileFile(t, configured, "settings.json", `{"model":"opus"}`)
		}, wantStaged: []string{".credentials.json", "settings.json"}},
		{name: "symlinked-settings", build: func(t *testing.T, configured string) {
			writeProfileFile(t, configured, ".credentials.json", object)
			target := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(target, []byte(`{"model":"opus"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(configured, "settings.json")); err != nil {
				t.Fatal(err)
			}
		}, wantStaged: []string{".credentials.json", "settings.json"}},
		{name: "symlinked-credential", build: func(t *testing.T, configured string) {
			target := filepath.Join(t.TempDir(), ".credentials.json")
			if err := os.WriteFile(target, []byte(object), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(configured, ".credentials.json")); err != nil {
				t.Fatal(err)
			}
		}, wantStaged: []string{".credentials.json"}},
		{name: "empty-settings", build: func(t *testing.T, configured string) {
			writeProfileFile(t, configured, ".credentials.json", object)
			writeProfileFile(t, configured, "settings.json", "")
		}, wantStaged: []string{".credentials.json"}, wantSkipped: []string{"settings.json"}},
		{name: "array-settings", build: func(t *testing.T, configured string) {
			writeProfileFile(t, configured, ".credentials.json", object)
			writeProfileFile(t, configured, "settings.json", `["not","an","object"]`)
		}, wantStaged: []string{".credentials.json"}, wantSkipped: []string{"settings.json"}},
		{name: "directory-settings", build: func(t *testing.T, configured string) {
			writeProfileFile(t, configured, ".credentials.json", object)
			if err := os.MkdirAll(filepath.Join(configured, "settings.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, wantStaged: []string{".credentials.json"}, wantSkipped: []string{"settings.json"}},
		{name: "unparseable-credential-still-fails", build: func(t *testing.T, configured string) {
			writeProfileFile(t, configured, ".credentials.json", `["not","an","object"]`)
		}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			configured := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", configured)
			tc.build(t, configured)
			stateRoot := t.TempDir()
			_, _, _, env, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, stateRoot, nil, nil, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("an unusable .credentials.json must fail the dispatch loudly")
				}
				return
			}
			if err != nil {
				t.Fatalf("a realistic operator profile must not fail produce: %v", err)
			}
			writeState := filepath.Join(stateRoot, ".claude")
			if !containsEnv(env, "CLAUDE_CONFIG_DIR="+writeState) {
				t.Fatalf("env = %v, want the job-private profile", env)
			}
			for _, name := range tc.wantStaged {
				if _, err := os.Stat(filepath.Join(writeState, name)); err != nil {
					t.Fatalf("%s was not staged: %v", name, err)
				}
			}
			for _, name := range tc.wantSkipped {
				if _, err := os.Stat(filepath.Join(writeState, name)); !os.IsNotExist(err) {
					t.Fatalf("%s should have been skipped, not staged: %v", name, err)
				}
			}
		})
	}
}

// A file-type refusal must not blame the size, which sends the reader after the
// wrong problem (#1810 review round 2).
func TestProduceStagingErrorsNameTheActualCause(t *testing.T) {
	configured := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configured, ".credentials.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := stageProduceProfileFile(stagedProduceProfileFile{
		source:      filepath.Join(configured, ".credentials.json"),
		destination: filepath.Join(t.TempDir(), ".credentials.json"),
		required:    true,
	})
	if err == nil {
		t.Fatal("a directory in place of the credential must be refused")
	}
	if strings.Contains(err.Error(), "bytes") {
		t.Fatalf("file-type refusal blames the size: %v", err)
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error does not name the file type problem: %v", err)
	}
}

func writeProfileFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
