package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
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
//     access token and no way to obtain one. The worker records that fact before
//     delivery; a later runtime-auth rejection follows the normal deferrable
//     blocker path instead of becoming a terminal preflight failure.
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
	sourceDir, err := resolveRuntimeConfigDir(runtimeName, sourceDir)
	if err != nil || sourceDir == "" {
		return ""
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
func readOnlySeatCredentialPreflight(agent runtime.Agent, sourceDir string, gatewayMode bool, haveOverlay bool, now time.Time) string {
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
	// Only claim the overlay when one was actually resolved. Asserting it
	// unconditionally was false on any host with no runtime-auth.env and no
	// ambient credentials, which is precisely the host this diagnosis matters on
	// (#1810 review, round 2).
	fallback := "and NO resolved runtime auth was available to the seat, so this staged credential is all it has"
	if haveOverlay {
		fallback = "and the seat also carries the resolved runtime auth, so this is context for any later auth failure rather than a refusal"
	}
	return fmt.Sprintf(
		"read-only seat staged an expired %s credential (expired %s) %s; %s",
		agent.Runtime, expiry, refresh, fallback,
	)
}

// readOnlySeatRuntimeAuthEnv resolves the runtime auth a seat must carry: the
// same overlay runtimeJobRunnerWithAuth injects for every other job, which the
// seat path rebuilt its environment without.
//
// It bootstraps first, exactly as runtimeJobRunnerWithAuth does. Reading
// runtime-auth.env alone returned an empty overlay on a host that authenticates
// from ambient credentials and had never written that file.
func readOnlySeatRuntimeAuthEnv(home string, runtimeName string, gatewayMode bool) ([]string, error) {
	if gatewayMode || runtimeName != runtime.ClaudeRuntime {
		return nil, nil
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		return nil, fmt.Errorf("resolve read-only seat runtime auth paths: %w", err)
	}
	return readOnlySeatRuntimeAuthEnvForPaths(paths, runtimeName, gatewayMode)
}

// readOnlySeatRuntimeAuthEnvForPaths is the same lookup for a caller that has
// already resolved its paths. Handing paths.Home to the flag-taking variant
// re-resolves a DIFFERENT home, which is the mistake that made the operator
// checks read the wrong overlay (#1810 review, round 3).
func readOnlySeatRuntimeAuthEnvForPaths(paths config.Paths, runtimeName string, gatewayMode bool) ([]string, error) {
	if gatewayMode || runtimeName != runtime.ClaudeRuntime {
		return nil, nil
	}
	if _, err := bootstrapRuntimeAuth(paths.Home, runtimeAuthEnvLookup, runtimeAuthLogf); err != nil {
		return nil, fmt.Errorf("bootstrap read-only seat runtime auth: %w", err)
	}
	state, err := loadRuntimeAuthFile(paths.Home)
	if err != nil {
		return nil, fmt.Errorf("load read-only seat runtime auth: %w", err)
	}
	return runtimeAuthInjectionEnv(state), nil
}

const readOnlySeatCredentialExpiredEvent = "readonly_seat_credential_expired"

func (w jobWorker) recordReadOnlySeatCredentialDiagnosis(ctx context.Context, jobID, diagnosis string) error {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Kind == readOnlySeatCredentialExpiredEvent {
			return nil
		}
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{
		JobID: jobID, Kind: readOnlySeatCredentialExpiredEvent, Message: diagnosis,
	})
}

// writeSeatCredentialProbe reports the credential a READ-ONLY SEAT would stage
// and returns false only when that credential is known to be unusable. Unknown
// expiry data and an expired credential with a refresh token remain non-failing:
// neither proves the runtime will reject the credential.
//
// It resolves the source the same way the doctor check does, preferring the
// DAEMON's own CLAUDE_CONFIG_DIR because the invoking shell may describe an
// account no job uses.
func writeSeatCredentialProbe(stdout io.Writer, paths config.Paths) bool {
	source := resolveSeatCredentialSource(paths)
	if source.dir == "" {
		writeLine(stdout, "read-only seat credential: unknown (cannot resolve the seat config dir)")
		return true
	}
	// Same three inputs as the doctor check, and the same rule: a non-zero exit
	// is reserved for the case that is actually proven broken - no gateway, no
	// overlay, expired with no refresh token (#1810 review, round 3).
	blindSpot := "cannot see a per-job payload runtime_config_dir override"
	gatewayMode := seatModelGatewayModeForPaths(paths)
	if gatewayMode {
		writeLine(stdout, "read-only seat credential: model gateway holds it; no snapshot is staged from %s (%s), so nothing is asserted (%s)", source.dir, source.origin, blindSpot)
		return true
	}
	overlay, overlayErr := readOnlySeatRuntimeAuthEnvForPaths(paths, runtime.ClaudeRuntime, gatewayMode)
	haveOverlay := overlayErr == nil && len(overlay) > 0
	state, ok := inspectReadOnlySeatCredential(runtime.ClaudeRuntime, source.dir)
	if !ok {
		writeLine(stdout, "read-only seat credential (%s, %s): no readable expiry; a seat stages what is there and the runtime decides (%s)", source.dir, source.origin, blindSpot)
		return true
	}
	if !state.Expired(time.Now().UTC()) {
		writeLine(stdout, "read-only seat credential (%s, %s): declares expiry %s, not yet reached; this reads a field and does not prove the credential authenticates (%s)", state.Source, source.origin, state.ExpiresAt.Format(time.RFC3339), blindSpot)
		return true
	}
	expiry := "no expiry recorded (a failed refresh writes this)"
	if !state.ExpiresAt.IsZero() {
		expiry = state.ExpiresAt.Format(time.RFC3339)
	}
	if haveOverlay {
		writeLine(stdout, "read-only seat credential (%s, %s): snapshot expired %s, but a resolved runtime-auth overlay is present and is what a seat authenticates with; clean up the stale snapshot (%s)", state.Source, source.origin, expiry, blindSpot)
		return true
	}
	if state.Refreshable {
		writeLine(stdout, "read-only seat credential (%s, %s): EXPIRED %s, refresh token present; a seat must refresh it inside a disposable home, and the rotated token is discarded with the job (%s)", state.Source, source.origin, expiry, blindSpot)
		return true
	}
	writeLine(stdout, "read-only seat credential (%s, %s): UNUSABLE, expired %s with no refresh token, no model gateway and no runtime-auth overlay; every read-only seat job on this runtime will fail until that account is re-logged in (%s)", state.Source, source.origin, expiry, blindSpot)
	return false
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
	return seatModelGatewayModeForPaths(paths)
}

// seatModelGatewayModeForPaths is the same question asked of ALREADY-RESOLVED
// paths. The operator-facing checks hold a config.Paths, not a --home flag
// value, and re-deriving one from paths.Home silently resolves a DIFFERENT
// config - which read gateway mode as off and produced the false red this
// round was reported for (#1810 review, round 3).
func seatModelGatewayModeForPaths(paths config.Paths) bool {
	cfg, err := config.LoadCredentialsConfig(paths)
	if err != nil {
		return false
	}
	return cfg.ModelGateway
}

// resolveRuntimeConfigDir is the one path contract shared by every caller that
// inspects, stages, or grants a runtime profile: the host default when unset,
// current-user ~ expansion, symlinks resolved when the path exists.
//
// It REFUSES a relative or ~user path instead of repairing it. Both used to be
// silently rewritten — a relative dir against the daemon's arbitrary working
// directory (creating a stray profile there) and ~user under the CURRENT user's
// home — so the engine granted, staged and inspected an account the operator
// never named. That is the failure class this change exists to remove, so it
// fails loudly with the configured value in the message.
func resolveRuntimeConfigDir(runtimeName string, configuredDir string) (string, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve runtime state home: %w", err)
	}
	dir := strings.TrimSpace(configuredDir)
	if dir == "" {
		switch runtimeName {
		case runtime.ClaudeRuntime:
			dir = filepath.Join(userHome, ".claude")
		case runtime.CodexRuntime:
			dir = filepath.Join(userHome, ".codex")
		case runtime.KimiRuntime:
			dir = filepath.Join(userHome, ".kimi-code")
		default:
			return "", nil
		}
	} else {
		switch {
		case dir == "~":
			dir = userHome
		case strings.HasPrefix(dir, "~/"):
			dir = filepath.Join(userHome, strings.TrimPrefix(dir, "~/"))
		case strings.HasPrefix(dir, "~"):
			return "", fmt.Errorf("runtime state directory %q uses unsupported ~user expansion; name an absolute path", configuredDir)
		}
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("runtime state directory %q must be absolute; a relative path resolves against the daemon's working directory", configuredDir)
		}
	}
	if resolved, resolveErr := filepath.EvalSymlinks(dir); resolveErr == nil {
		dir = resolved
	}
	return filepath.Clean(dir), nil
}
