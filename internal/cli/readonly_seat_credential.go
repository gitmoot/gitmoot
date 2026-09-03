package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// A read-only seat stages a SNAPSHOT of the host credential into a disposable
// isolated state dir. A snapshot of an expiring OAuth credential is only good
// until it expires, and gitmoot launched seats without looking at the expiry it
// had already copied.
//
// Measured 2026-09-03 with the daemon on CLAUDE_CONFIG_DIR=/root/.claude-13,
// whose credential expired at 06:31:46.182Z. Three seat reviews succeeded, the
// last finishing 06:30:08. The next job was created 06:31:36 and failed, and so
// did the two after it, each reporting the runtime's own sentence:
//
//	claude: Failed to authenticate: OAuth session expired and could not be refreshed
//
// That sentence names OAuth, not the staged input, so it reads as an owner
// problem. Two states are worth separating, and only one of them is certain:
//
//   - EXPIRED WITH NO REFRESH TOKEN is provably unusable. There is no valid
//     access token and no way to obtain one, so launching can only fail. This
//     refuses BEFORE the runtime starts, with an error naming the source dir and
//     the expiry. `/root/.claude/.credentials.json` has been in exactly this
//     state since 2026-08-31 (expiresAt 0, refreshToken absent), which is what
//     forced the daemon onto a different account.
//   - EXPIRED WITH A REFRESH TOKEN may still work: refreshing is the runtime's
//     job and normally succeeds. This does NOT refuse, because refusing would
//     break every host whose credential refreshes fine. It records a diagnosis
//     so that if the runtime then fails, the job already carries the fact that
//     gitmoot staged an expired credential and from where.
type readOnlySeatCredentialState struct {
	// Source is the host file the seat staged from.
	Source string
	// ExpiresAt is the credential expiry, zero when the file declares none.
	ExpiresAt time.Time
	// Refreshable reports whether a non-empty refresh token accompanied it.
	Refreshable bool
}

// Expired reports whether the credential cannot be used as-is. A zero expiry
// counts as expired: the field exists in this format and a zero value is what a
// FAILED refresh writes back, so it is a positive statement that the token is
// dead rather than an absent one.
func (s readOnlySeatCredentialState) Expired(now time.Time) bool {
	return s.ExpiresAt.IsZero() || !s.ExpiresAt.After(now)
}

// readOnlySeatCredentialStatePath is the credential a seat stages, per runtime.
// Only claude declares one today: its file carries a documented expiry field.
// A runtime whose staged credential has no readable expiry returns "", and
// preflight then has nothing to assert, which is the honest outcome rather than
// a guess about a format gitmoot has not measured.
func readOnlySeatCredentialStatePath(runtimeName string, sourceDir string) string {
	sourceDir = strings.TrimSpace(sourceDir)
	if sourceDir == "" {
		return ""
	}
	// Resolve exactly as prepareReadOnlyRuntimeState does before staging:
	// absolute, then symlinks evaluated. Reading a DIFFERENT path from the one
	// that gets staged would let this report on a file the seat never uses, which
	// the #1810 review flagged.
	if absolute, err := filepath.Abs(sourceDir); err == nil {
		sourceDir = absolute
	}
	if resolved, err := filepath.EvalSymlinks(sourceDir); err == nil {
		sourceDir = resolved
	}
	switch runtimeName {
	case runtime.ClaudeRuntime:
		return filepath.Join(sourceDir, ".credentials.json")
	default:
		return ""
	}
}

// inspectReadOnlySeatCredential reads the expiry a seat is about to stage. A
// missing, unreadable, or expiry-less file returns ok=false: absence is the
// #1806 class (handled where inputs are staged), and a file that declares no
// expiry gives this check nothing to assert.
func inspectReadOnlySeatCredential(runtimeName string, sourceDir string) (readOnlySeatCredentialState, bool) {
	path := readOnlySeatCredentialStatePath(runtimeName, sourceDir)
	if path == "" {
		return readOnlySeatCredentialState{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return readOnlySeatCredentialState{}, false
	}
	var file struct {
		ClaudeAIOauth struct {
			// A POINTER, because an absent expiresAt and an explicit 0 mean
			// different things and Go decodes both to the same value otherwise.
			// Absent: the file declares no expiry and this check must assert
			// nothing. Explicit 0: what a FAILED refresh writes back, i.e. a
			// positive statement that the token is dead. Conflating them made
			// three existing seat tests fail, whose fixtures legitimately carry
			// only an accessToken.
			ExpiresAt    *int64 `json:"expiresAt"`
			RefreshToken string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return readOnlySeatCredentialState{}, false
	}
	if file.ClaudeAIOauth.ExpiresAt == nil {
		return readOnlySeatCredentialState{}, false
	}
	state := readOnlySeatCredentialState{
		Source:      path,
		Refreshable: strings.TrimSpace(file.ClaudeAIOauth.RefreshToken) != "",
	}
	if *file.ClaudeAIOauth.ExpiresAt > 0 {
		state.ExpiresAt = time.UnixMilli(*file.ClaudeAIOauth.ExpiresAt).UTC()
	}
	return state, true
}

// readOnlySeatCredentialPreflight returns a DIAGNOSIS when the credential a seat
// is about to stage is already expired, and never refuses.
//
// It refused in the first version of this change, and the exact-head review of
// #1810 (job local-review-gm-review-opus-18d1be80ca9b36c9) showed both halves of
// that were wrong:
//
//   - the refusal contradicted the engine's own policy for this class.
//     classifyOperationalBlocker maps a runtime-auth rejection to
//     blockerClassRuntimeAuth with authBlockerRetryDelay, and docs/events.md
//     documents job.deferred emitted INSTEAD of job.failed, so a run self-heals
//     after a re-login with no job.failed and no PR comment. Failing at
//     preflight turned a deferrable blocker into a terminal one;
//   - after the auth-overlay injection in this same commit, an expired staged
//     snapshot is no longer decisive: the seat authenticates with the resolved
//     runtime auth, so the refusal could hard-fail a job the same commit had
//     just made authenticable.
//
// What survives is the reporting half, which is the part the outage actually
// needed: when a job later fails to authenticate, the record already says that
// gitmoot staged an expired credential and from where, instead of leaving the
// reader with the runtime's own "OAuth session expired" wording. Gateway mode
// stages no credential file at all, so there is nothing to diagnose there.
func readOnlySeatCredentialPreflight(agent runtime.Agent, sourceDir string, gatewayMode bool, now time.Time) string {
	if !agent.ReadOnlySeat || gatewayMode {
		return ""
	}
	state, ok := inspectReadOnlySeatCredential(agent.Runtime, sourceDir)
	if !ok || !state.Expired(now) {
		return ""
	}
	expiry := "no expiry recorded (a failed refresh writes this)"
	if !state.ExpiresAt.IsZero() {
		expiry = state.ExpiresAt.Format(time.RFC3339)
	}
	refresh := "and carries no refresh token, so it cannot authenticate on its own"
	if state.Refreshable {
		refresh = "and must be refreshed by the runtime to work on its own"
	}
	return fmt.Sprintf(
		"read-only seat staged an expired %s credential (expired %s) %s; the seat also carries the resolved runtime auth, so this is context for any later auth failure rather than a refusal",
		agent.Runtime, expiry, refresh,
	)
}

// readOnlySeatRuntimeAuthEnv resolves the runtime auth a seat must carry. It is
// the same overlay runtimeJobRunnerWithAuth injects for every other job; the
// seat path rebuilt its environment and silently dropped it.
func readOnlySeatRuntimeAuthEnv(home string, runtimeName string, gatewayMode bool) ([]string, error) {
	if gatewayMode || runtimeName != runtime.ClaudeRuntime {
		return nil, nil
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		return nil, fmt.Errorf("resolve read-only seat runtime auth paths: %w", err)
	}
	state, err := loadRuntimeAuthFile(paths.Home)
	if err != nil {
		return nil, fmt.Errorf("load read-only seat runtime auth: %w", err)
	}
	return runtimeAuthInjectionEnv(state), nil
}

// writeSeatCredentialProbe reports the credential a READ-ONLY SEAT would stage,
// which is a different credential from the ambient one every probe measured
// until now. Measured 2026-09-03: `gitmoot auth probe claude` and `gitmoot
// doctor` were green for hours while every claude review job failed, because
// the seat staged /root/.claude/.credentials.json (expiresAt 0, refresh
// rejected) and the probe never looked at it. A green that cannot see the
// credential under test is the false green this reports on.
func writeSeatCredentialProbe(stdout io.Writer, home string) {
	sourceDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	origin := "CLAUDE_CONFIG_DIR"
	if sourceDir == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			writeLine(stdout, "read-only seat credential: unknown (cannot resolve the host home: %v)", err)
			return
		}
		sourceDir = filepath.Join(userHome, ".claude")
		origin = "host default"
	}
	state, ok := inspectReadOnlySeatCredential(runtime.ClaudeRuntime, sourceDir)
	if !ok {
		writeLine(stdout, "read-only seat credential (%s %s): no readable expiry; a seat stages what is there and the runtime decides", origin, sourceDir)
		return
	}
	if !state.Expired(time.Now().UTC()) {
		writeLine(stdout, "read-only seat credential (%s): declares expiry %s, not yet reached; this reads a field and does not prove the credential authenticates", state.Source, state.ExpiresAt.Format(time.RFC3339))
		return
	}
	expiry := "no expiry recorded (a failed refresh writes this)"
	if !state.ExpiresAt.IsZero() {
		expiry = state.ExpiresAt.Format(time.RFC3339)
	}
	if state.Refreshable {
		writeLine(stdout, "read-only seat credential (%s): EXPIRED %s, refresh token present; a seat must refresh it inside a disposable home, and the rotated token is discarded with the job", state.Source, expiry)
		return
	}
	writeLine(stdout, "read-only seat credential (%s): UNUSABLE, expired %s with no refresh token; every read-only seat job on this runtime will fail until that account is re-logged in", state.Source, expiry)
}

// seatModelGatewayMode reports whether claude deliveries run through the local
// model gateway, resolved the same way runtimeJobRunnerWithAuth resolves it. In
// gateway mode no credential file is staged for a seat, so both the auth overlay
// and the staged-credential diagnosis are deliberately silent there.
func seatModelGatewayMode(home string) bool {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return false
	}
	cfg, err := config.LoadCredentialsConfig(paths)
	if err != nil {
		return false
	}
	return cfg.ModelGateway
}
