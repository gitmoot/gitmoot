package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
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
		// An empty source resolves through the HOST DEFAULT by design (that is
		// this PR's fix for a silently-dead default profile), so this subtest is
		// only meaningful with the host pinned. Unpinned it read the real
		// ~/.claude and started failing the moment that credential expired -
		// the assertion was measuring the box, not the code.
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, "  ", false, true, time.Now().UTC()); diagnosis != "" {
			t.Fatalf("diagnosis=%q, want silence when the host default profile does not exist", diagnosis)
		}
	})
	t.Run("no source dir resolves the host default when one exists", func(t *testing.T) {
		// The other half of the same contract, pinned rather than assumed: an
		// empty source is NOT ignored, it resolves to ~/.claude, and an expired
		// profile there is diagnosed instead of passing silently.
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		hostDefault := filepath.Join(home, ".claude")
		if err := os.MkdirAll(hostDefault, 0o700); err != nil {
			t.Fatal(err)
		}
		writeClaudeCredential(t, hostDefault, 0, "")
		agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, ReadOnlySeat: true}
		if diagnosis := readOnlySeatCredentialPreflight(agent, "  ", false, false, time.Now().UTC()); diagnosis == "" {
			t.Fatal("an empty source must resolve the host default profile, not pass silently")
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

// FAIL-OPEN is the property that keeps a broken probe from stranding every seat
// job, and round 3 of the #1810 review showed it unpinned: a mutant flipping all
// six early returns to authProbeInvalid survived the whole scoped suite. A probe
// that cannot decide must release the job, never hold it.
func TestSeatAuthProbeFailsOpenOnEveryUndecidableOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"binary missing", exec.ErrNotFound},
		{"probe timed out", context.DeadlineExceeded},
		{"empty stdout", errors.New("claude auth probe produced no output")},
		{"unparseable output", errors.New("invalid character 'x' looking for beginning of value")},
		{"sandbox denied the probe", errors.New("fork/exec: permission denied")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			configured := t.TempDir()
			writeClaudeCredential(t, configured, time.Now().Add(time.Hour).UnixMilli(), "refresh")
			t.Setenv("CLAUDE_CONFIG_DIR", configured)
			original := claudeAuthLiveCheck
			t.Cleanup(func() { claudeAuthLiveCheck = original })
			claudeAuthLiveCheck = func(context.Context, subprocess.Runner, string, []string) error {
				return tc.err
			}
			worker := jobWorker{ConfigHome: home}
			verdict := worker.probeReadOnlySeatClaudeAuth(context.Background(),
				runtime.Agent{Name: "reviewer", Runtime: runtime.ClaudeRuntime},
				workflow.JobPayload{RuntimeConfigDir: configured})
			if verdict != authProbeUnknown {
				t.Fatalf("verdict = %v, want authProbeUnknown: an undecidable probe must not hold the job", verdict)
			}
			if verdict == authProbeInvalid {
				t.Fatalf("verdict = authProbeInvalid; an undecidable probe would hold the job and strand every seat run")
			}
		})
	}
}

// The overlay is claude-only BY POLICY, not by "codex happens to be excluded".
// Round 3 measured a mutant widening it to kimi surviving, because the test
// named codex instead of enumerating the runtimes.
func TestSeatRuntimeAuthOverlayIsClaudeOnlyAcrossEverySupportedRuntime(t *testing.T) {
	home := t.TempDir()
	configured := t.TempDir()
	writeClaudeCredential(t, configured, time.Now().Add(time.Hour).UnixMilli(), "refresh")
	t.Setenv("CLAUDE_CONFIG_DIR", configured)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "seat-overlay-token")
	supported := runtime.SupportedRuntimes()
	if len(supported) < 2 {
		t.Fatalf("SupportedRuntimes() = %v, expected the full runtime set", supported)
	}
	sawClaude := false
	for _, name := range supported {
		env, err := readOnlySeatRuntimeAuthEnv(home, name, false)
		if err != nil {
			t.Fatalf("readOnlySeatRuntimeAuthEnv(%s): %v", name, err)
		}
		if name == runtime.ClaudeRuntime {
			sawClaude = true
			if len(env) == 0 {
				t.Fatalf("claude lost its runtime-auth overlay: %v", env)
			}
			continue
		}
		if len(env) != 0 {
			t.Fatalf("runtime %q received a claude overlay (%d entries): the overlay is claude-only by policy", name, len(env))
		}
	}
	if !sawClaude {
		t.Fatalf("SupportedRuntimes() = %v, missing claude; the policy assertion measured nothing", supported)
	}
	// Gateway mode withholds it from claude too.
	gatewayEnv, err := readOnlySeatRuntimeAuthEnv(home, runtime.ClaudeRuntime, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(gatewayEnv) != 0 {
		t.Fatalf("gateway-mode claude seat received an overlay: %v", redactEnvNames(gatewayEnv))
	}
}

// Round 3 of the #1810 review reproduced BOTH directions of dishonesty in the
// operator-facing checks: a false RED in gateway mode and with a live overlay,
// where every seat job succeeds, and a hard exit driven by a file read that
// cannot see the per-job override which actually decides the seat's source.
func TestSeatCredentialDoctorCheckReservesTheHardFailForWhatItProves(t *testing.T) {
	dead := time.Now().Add(-24 * time.Hour).UnixMilli()
	for _, tc := range []struct {
		name         string
		gateway      bool
		overlayToken string
		wantOK       bool
		wantRequired bool
		wantDetail   string
	}{
		{name: "gateway mode asserts nothing", gateway: true, wantOK: true, wantRequired: false, wantDetail: "model gateway"},
		{name: "overlay present is untidy not broken", overlayToken: "live-overlay-token", wantOK: false, wantRequired: false, wantDetail: "overlay is present"},
		{name: "no gateway no overlay is the proven failure", wantOK: false, wantRequired: true, wantDetail: "UNUSABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			paths := seatDoctorTestPaths(t, home, tc.gateway)
			configured := t.TempDir()
			writeClaudeCredential(t, configured, dead, "")
			t.Setenv("CLAUDE_CONFIG_DIR", configured)
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", tc.overlayToken)
			check, ok := seatCredentialDoctorCheck(paths)
			if !ok {
				t.Fatal("seatCredentialDoctorCheck declined to report")
			}
			if check.OK != tc.wantOK || check.Required != tc.wantRequired {
				t.Fatalf("check = OK %v Required %v, want OK %v Required %v: %s", check.OK, check.Required, tc.wantOK, tc.wantRequired, check.Detail)
			}
			if !strings.Contains(check.Detail, tc.wantDetail) {
				t.Fatalf("detail = %q, want it to mention %q", check.Detail, tc.wantDetail)
			}
			// Every verdict must name the source it measured and admit the
			// per-job override it cannot see.
			if !strings.Contains(check.Detail, "runtime_config_dir") {
				t.Fatalf("detail = %q, want it to disclose the per-job override blind spot", check.Detail)
			}
		})
	}
}

func seatDoctorTestPaths(t *testing.T, home string, gateway bool) config.Paths {
	t.Helper()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	body := config.DefaultConfig(paths)
	if gateway {
		body += "\n[credentials]\nmodel_gateway = true\n"
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths
}

// The seat path has always narrowed its staged credential to the OAuth section.
// Round 3 measured produce copying the file VERBATIM, so a sibling secret in the
// operator's credential file rode into the job-private profile.
func TestProduceStagesOnlyTheOAuthSectionLikeTheSeatPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configured := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configured)
	body := `{"claudeAiOauth":{"accessToken":"account-token","refreshToken":"ref"},"bedrockApiKey":"SIBLING-SECRET-XYZ"}`
	if err := os.WriteFile(filepath.Join(configured, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	if _, _, _, _, err := produceRuntimeSandboxGrants(runtime.ClaudeRuntime, stateRoot, nil, nil, nil); err != nil {
		t.Fatalf("produceRuntimeSandboxGrants: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(stateRoot, ".claude", ".credentials.json"))
	if err != nil {
		t.Fatalf("read staged credential: %v", err)
	}
	if !strings.Contains(string(staged), "account-token") {
		t.Fatalf("staged credential lost the account: %s", staged)
	}
	if strings.Contains(string(staged), "SIBLING-SECRET-XYZ") {
		t.Fatalf("a sibling secret rode into the job-private profile: %s", staged)
	}
}

// The live-check stub above exercises only the LAST return in the probe. The
// six EARLY returns - temp-dir creation, staging, overlay resolution, probe-home
// mkdir, an empty state dir, and backend resolution - are the ones a mutant
// flipped to fail-closed and survived (#1810 review round 3, M9). Each of these
// drives a real early return, so flipping any of them to authProbeInvalid fails
// here.
func TestSeatAuthProbeFailsOpenOnSetupFailuresNotOnlyOnTheLiveCheck(t *testing.T) {
	original := claudeAuthLiveCheck
	t.Cleanup(func() { claudeAuthLiveCheck = original })
	claudeAuthLiveCheck = func(context.Context, subprocess.Runner, string, []string) error {
		t.Fatal("probe reached the live check; this test must fail EARLIER, or it is not measuring the early returns")
		return nil
	}
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T) (runtime.Agent, workflow.JobPayload)
	}{
		{
			// os.MkdirTemp("", ...) honours TMPDIR, so an unusable TMPDIR fails
			// the very first step.
			name: "temp state root cannot be created",
			prepare: func(t *testing.T) (runtime.Agent, workflow.JobPayload) {
				t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
				configured := t.TempDir()
				writeClaudeCredential(t, configured, time.Now().Add(time.Hour).UnixMilli(), "r")
				return runtime.Agent{Name: "reviewer", Runtime: runtime.ClaudeRuntime}, workflow.JobPayload{RuntimeConfigDir: configured}
			},
		},
		{
			// A relative config dir is refused by resolveRuntimeConfigDir, so
			// staging fails before anything is written.
			name: "staging refuses the configured dir",
			prepare: func(t *testing.T) (runtime.Agent, workflow.JobPayload) {
				return runtime.Agent{Name: "reviewer", Runtime: runtime.ClaudeRuntime}, workflow.JobPayload{RuntimeConfigDir: "relative-not-absolute"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, payload := tc.prepare(t)
			worker := jobWorker{ConfigHome: t.TempDir()}
			if verdict := worker.probeReadOnlySeatClaudeAuth(context.Background(), agent, payload); verdict != authProbeUnknown {
				t.Fatalf("verdict = %v, want authProbeUnknown: a probe that cannot set itself up must release the job, not hold it", verdict)
			}
		})
	}
}

// P0 REGRESSION (#1810): runtime-auth.env is a CREDENTIAL FILE, and its path is
// filepath.Join(home, name) - so an empty or relative home resolves to a
// relative path and the credential is written into the process's working
// directory. During a test run that directory is the package source tree, which
// is how a live token became a tracked file and reached a public remote. No
// caller may write a credential anywhere but an absolute home.
func TestRuntimeAuthNeverWritesACredentialIntoTheWorkingDirectory(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-only-placeholder-not-a-token")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(cwd, runtimeAuthFileName)
	if _, err := os.Stat(stray); err == nil {
		t.Fatalf("%s already exists in the package directory before this test ran; a credential is sitting in the work tree", stray)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(stray); err == nil {
			_ = os.Remove(stray)
			t.Errorf("a credential file was written into the work tree at %s", stray)
		}
	})

	for _, home := range []string{"", "   ", "relative/home", "."} {
		if _, err := bootstrapRuntimeAuth(home, runtimeAuthEnvLookup, runtimeAuthLogf); err == nil {
			t.Fatalf("bootstrapRuntimeAuth(%q) accepted a non-absolute home; a credential must only be written to a deliberate absolute path", home)
		}
	}

	// The operator-facing checks reach the same primitive, and pre-existing
	// callers pass a zero config.Paths. They must not create anything either.
	dir := t.TempDir()
	writeClaudeCredential(t, dir, 0, "")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if _, ok := seatCredentialDoctorCheck(config.Paths{}); !ok {
		t.Fatal("doctor check must still be emitted with a zero config.Paths")
	}
	var probeOut strings.Builder
	writeSeatCredentialProbe(&probeOut, config.Paths{})
	if _, err := os.Stat(stray); err == nil {
		t.Fatalf("the operator-facing checks wrote a credential into the work tree at %s", stray)
	}
}
