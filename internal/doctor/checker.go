package doctor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/presence"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

type Check struct {
	Name     string
	OK       bool
	Required bool
	Detail   string
}

type BuildInfo struct {
	Version string
	Commit  string
}

func BuildInfoFromValues(version, commit string) BuildInfo {
	return BuildInfo{Version: strings.TrimSpace(version), Commit: strings.TrimSpace(commit)}
}

// BuildStatus compares what the daemon PROCESS is running against the binary it
// would load if restarted. Those are the only two builds the operator can act
// on; the build of whatever binary happens to be invoking `doctor` is not one of
// them (it may not even be the daemon's binary), so it is deliberately absent.
type BuildStatus struct {
	// Daemon is the build recorded by the running daemon at startup — what it is
	// actually executing. Zero when the daemon predates build stamping.
	Daemon BuildInfo
	// OnDisk is the build of the binary now sitting at the daemon's executable
	// path — what a restart would pick up. Zero when it cannot be resolved.
	OnDisk        BuildInfo
	OnDiskPath    string
	DaemonRunning bool
}

type Checker struct {
	Dir    string
	Runner subprocess.Runner
	// ClaudeProbeRunner and ClaudeAuthLookup let the one-shot CLI doctor probe
	// the exact environment used for Claude deliveries. Other callers leave
	// these unset and retain the inexpensive ambient-presence behavior.
	ClaudeProbeRunner subprocess.Runner
	ClaudeAuthLookup  func(string) (string, bool)
	ClaudeAuthSource  string
	ClaudeAuthError   error
	SkipDaemonAuth    bool
	// LiveProbe opts the claude auth check into a live `claude -p` probe when the
	// auth env is not Ready, instead of false-reporting OK:false on a cached-creds
	// box. It is opt-in because GlobalChecks is re-run on every dashboard refresh
	// (dashboard_tui.go) and an unconditional probe would spawn claude each time.
	// The one-shot `gitmoot doctor` (root.go) sets this true; the dashboard leaves
	// it false so a refresh never spawns claude.
	LiveProbe bool
	// Paths locates the running daemon for the daemon-aware claude auth check
	// (issue #427). When unset, the daemon check is skipped and only the
	// shell-local check is reported. The signal that actually matters for Claude
	// background jobs is the daemon's own environment, not the shell that ran
	// `gitmoot doctor`.
	Paths config.Paths
	// Build is supplied by the CLI so this package stays independent of both
	// buildinfo and daemon-state persistence. Nil omits the check for callers
	// such as the continuously refreshed terminal dashboard.
	Build *BuildStatus
}

// Run returns the global (cwd-independent) checks followed by the per-repo
// checks for the Checker's Dir, treated as a single checkout. This is the
// `gitmoot doctor --repo <dir>` view.
func (c Checker) Run(ctx context.Context) []Check {
	checks := c.GlobalChecks(ctx)
	return append(checks, c.RepoChecks(ctx, c.Dir)...)
}

// GlobalChecks returns the diagnostics that do not depend on any repo checkout:
// required/optional runtime binaries and the auth checks. They can run from
// anywhere, so the dashboard renders them once.
func (c Checker) GlobalChecks(ctx context.Context) []Check {
	runner := c.runner()
	checks := []Check{
		c.command(ctx, runner, "git", true, "--version"),
		c.command(ctx, runner, "gh", true, "--version"),
		c.command(ctx, runner, "codex", true, "--version"),
		c.command(ctx, runner, "claude", false, "--help"),
		c.command(ctx, runner, "kimi", false, "--version"),
	}
	// The daemon is what actually runs Claude background jobs, so report its auth
	// state first when it can be detected (issue #427). The shell-local check
	// follows, clearly labeled, so a warn in a terminal can't be mistaken for "the
	// daemon is broken".
	if !c.SkipDaemonAuth {
		if daemon, ok := c.claudeAuthDaemon(ctx); ok {
			checks = append(checks, daemon)
		}
	}
	checks = append(checks, c.claudeAuthEnv(ctx), c.ghAuth(ctx, runner))
	if c.Build != nil {
		checks = append(checks, CheckBuild(*c.Build))
	}
	return checks
}

// CheckBuild reports whether the running daemon is executing stale code: the
// build it started from, versus the build now sitting at its executable path
// (what a restart would load). Those are the only two builds the operator can
// act on — the build of whatever binary happens to be invoking `doctor` is not
// necessarily either of them, so it is deliberately not part of the comparison.
//
// Every unknown is neutral. Inferring "skew" from missing information would fire
// on every daemon started by an older gitmoot; inferring "same" would hide the
// staleness this check exists to catch. So an unidentifiable build on either
// side yields a skipped comparison, never a verdict.
func CheckBuild(status BuildStatus) Check {
	check := Check{Name: "build", OK: true, Required: false}
	if !status.DaemonRunning {
		check.Detail = "daemon not running; build comparison skipped"
		return check
	}
	if !identifiableBuild(status.Daemon) {
		check.Detail = "running daemon build unknown (older gitmoot, or an unstamped build); comparison skipped"
		return check
	}
	if !identifiableBuild(status.OnDisk) {
		check.Detail = "build at the daemon's binary path is unknown (unstamped build); comparison skipped"
		return check
	}

	versionsDiffer := status.Daemon.Version != status.OnDisk.Version
	commitsDiffer := knownBuildCommit(status.Daemon.Commit) && knownBuildCommit(status.OnDisk.Commit) && status.Daemon.Commit != status.OnDisk.Commit
	if !versionsDiffer && !commitsDiffer {
		check.Detail = "daemon is running the binary on disk (" + buildDisplay(status.OnDisk) + ")"
		return check
	}

	check.OK = false
	target := "the binary on disk"
	if path := strings.TrimSpace(status.OnDiskPath); path != "" {
		target = path
	}
	check.Detail = fmt.Sprintf("daemon running %s; %s is %s — restart the daemon to pick it up",
		buildDisplay(status.Daemon), target, buildDisplay(status.OnDisk))
	return check
}

// identifiableBuild mirrors buildinfo.Info.Identifiable, without importing it —
// this package stays free of the build-stamp dependency and the CLI supplies the
// values. An unstamped build with no VCS revision reports version "dev" and no
// commit; every such build is the same anonymous "dev", so comparing two of them
// tells you nothing and must not be treated as a verdict either way.
func identifiableBuild(build BuildInfo) bool {
	if !knownBuildVersion(build.Version) {
		return false
	}
	if build.Version != "dev" {
		return true
	}
	return knownBuildCommit(build.Commit)
}

func knownBuildVersion(version string) bool {
	version = strings.TrimSpace(version)
	return version != "" && !strings.EqualFold(version, "unknown")
}

func knownBuildCommit(commit string) bool {
	commit = strings.TrimSpace(commit)
	return commit != "" && !strings.EqualFold(commit, "unknown")
}

func buildDisplay(build BuildInfo) string {
	if knownBuildCommit(build.Commit) {
		commit := build.Commit
		if len(commit) > 8 {
			commit = commit[:8]
		}
		return fmt.Sprintf("%s (%s)", build.Version, commit)
	}
	return build.Version
}

// RepoChecks returns the per-repo diagnostics (origin remote resolves, base
// branch present) run against checkoutPath. A repo that has no checkout path yet
// (subscribed but never delivered to) reports a single non-required "no checkout"
// check rather than failing the git-dependent checks.
func (c Checker) RepoChecks(ctx context.Context, checkoutPath string) []Check {
	if strings.TrimSpace(checkoutPath) == "" {
		return []Check{{Name: "checkout", OK: false, Required: false, Detail: "no checkout yet"}}
	}
	runner := c.runner()
	return []Check{
		c.repoRemote(ctx, runner, checkoutPath),
		c.baseBranch(ctx, runner, checkoutPath),
	}
}

func (c Checker) runner() subprocess.Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return subprocess.ExecRunner{}
}

func (c Checker) command(ctx context.Context, runner subprocess.Runner, name string, required bool, args ...string) Check {
	if _, err := runner.LookPath(name); err != nil {
		return Check{Name: name, Required: required, Detail: err.Error()}
	}
	result, err := runner.Run(ctx, "", name, args...)
	if err != nil {
		return Check{Name: name, Required: required, Detail: strings.TrimSpace(result.Stderr)}
	}
	return Check{Name: name, OK: true, Required: required, Detail: firstLine(result.Stdout, result.Stderr)}
}

// GHAuthRemediation is the actionable hint appended when `gh auth status` fails.
// A job that pushes branches or opens PRs needs valid gh credentials in the
// environment that RUNS it — which for background jobs is the daemon's, not the
// invoking shell's (issue #552 point 4: an integrate leg failed on invalid gh
// auth even though interactive `gh auth status` was valid, because the daemon did
// not inherit the shell's credentials).
const GHAuthRemediation = "run `gh auth login` and ensure the daemon inherits valid gh credentials before dispatching jobs that push or open PRs"

func (c Checker) ghAuth(ctx context.Context, runner subprocess.Runner) Check {
	result, err := runner.Run(ctx, "", "gh", "auth", "status")
	if err != nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(err.Error())
		}
		if detail != "" {
			detail += "; "
		}
		detail += GHAuthRemediation
		return Check{Name: "gh auth", Required: true, Detail: detail}
	}
	return Check{Name: "gh auth", OK: true, Required: true, Detail: firstLine(result.Stdout, result.Stderr)}
}

func (c Checker) repoRemote(ctx context.Context, runner subprocess.Runner, dir string) Check {
	result, err := runner.Run(ctx, dir, "git", "remote", "get-url", "origin")
	if err != nil {
		return Check{Name: "repo remote", Required: true, Detail: strings.TrimSpace(result.Stderr)}
	}
	remote := strings.TrimSpace(result.Stdout)
	repo, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return Check{Name: "repo remote", Required: true, Detail: err.Error()}
	}

	view, err := runner.Run(ctx, dir, "gh", "repo", "view", repo.String(), "--json", "nameWithOwner")
	if err != nil {
		return Check{Name: "repo remote", Required: true, Detail: strings.TrimSpace(view.Stderr)}
	}
	return Check{Name: "repo remote", OK: true, Required: true, Detail: repo.String()}
}

func (c Checker) baseBranch(ctx context.Context, runner subprocess.Runner, dir string) Check {
	result, err := runner.Run(ctx, dir, "git", "branch", "--show-current")
	if err != nil {
		return Check{Name: "base branch", Required: true, Detail: strings.TrimSpace(result.Stderr)}
	}
	branch := strings.TrimSpace(result.Stdout)
	if branch == "" {
		return Check{Name: "base branch", Required: true, Detail: "detached HEAD"}
	}
	return Check{Name: "base branch", OK: true, Required: true, Detail: branch}
}

// claudeShellAuthLabel prefixes the shell-local claude auth detail so a warn in
// one terminal can't be mistaken for "the daemon is broken": the env-based check
// only ever reflects the shell that ran the command, not the daemon that runs
// background jobs (issue #427).
const claudeShellAuthLabel = "current shell (not the daemon)"

// claudeAuthDaemon reports the running daemon's Claude auth state, best-effort.
// The daemon is what actually runs Claude background jobs, so its environment —
// not the invoking shell — is the signal that matters. It is OS-gated and
// fail-open (Linux /proc only): when the daemon isn't running or its environment
// can't be read it returns ok=false and the caller falls back to the shell-local
// check. Secrets are never printed (masked set/unset only).
func (c Checker) claudeAuthDaemon(ctx context.Context) (Check, bool) {
	if strings.TrimSpace(c.Paths.Home) == "" {
		return Check{}, false
	}
	snap := presence.InspectDaemonClaudeAuth(c.Paths)
	base, ok := claudeAuthDaemonCheck(snap)
	if !ok {
		return Check{}, false
	}
	// Dashboard path (LiveProbe false) or a daemon with no credential: keep the
	// presence-only report. The dashboard must never spawn claude, and an
	// unauthenticated daemon is already a warn — only the one-shot `gitmoot doctor`
	// validates the live token. The prior code reported OK:true for a set-but-
	// invalid daemon token (#486); below it actually probes that exact token.
	if !c.LiveProbe || !snap.Auth.Ready() {
		return base, true
	}
	credEnv, hasCred := presence.DaemonClaudeCredEnv(c.Paths)
	if !hasCred {
		// Booleans were readable but the values are not (non-Linux, hardened /proc):
		// fall back to presence rather than probe the doctor's own credential, which
		// may differ from the daemon's and would give a misleading verdict.
		return base, true
	}
	// Isolate the probe to the injected daemon token: point claude at a throwaway
	// empty CLAUDE_CONFIG_DIR so cached ~/.claude credentials in the doctor's HOME
	// cannot mask a bad daemon token — the decisive token-only test for #486. If the
	// throwaway dir can't be created, fall through with the (already neutralized)
	// credEnv rather than skipping the probe.
	if probeDir, err := os.MkdirTemp("", "gitmoot-claude-probe-"); err == nil {
		defer os.RemoveAll(probeDir)
		credEnv = append(credEnv, runtime.ClaudeConfigDirEnv+"="+probeDir)
	}
	masked := "running daemon (pid " + strconv.Itoa(snap.PID) + "): " + snap.Auth.MaskedDetail()
	probeErr := runtime.ClaudeLiveCheckEnv(ctx, c.runner(), "", credEnv)
	return claudeProbeCheck("claude auth (daemon)", masked, "live token check passed", true, probeErr), true
}

// claudeAuthDaemonCheck builds the daemon-aware claude auth Check from an already
// inspected snapshot. It is split from claudeAuthDaemon so the Detected=true
// branches (Check name/detail, pid prefix, masked set/unset, warn vs ok) are
// testable without a live daemon or readable /proc (issue #427). Secrets never
// reach the detail — only daemon.Auth.MaskedDetail()'s set/unset booleans.
func claudeAuthDaemonCheck(daemon presence.DaemonAuthSnapshot) (Check, bool) {
	if !daemon.Detected {
		return Check{}, false
	}
	masked := daemon.Auth.MaskedDetail()
	detail := "running daemon (pid " + strconv.Itoa(daemon.PID) + "): " + masked
	if daemon.Auth.Ready() {
		if warning := daemon.Auth.Warning(); warning != "" {
			detail += "; " + warning
		}
		return Check{Name: "claude auth (daemon)", OK: true, Required: false, Detail: detail}, true
	}
	detail += "; " + runtime.ClaudeBackgroundTokenMessage
	return Check{Name: "claude auth (daemon)", OK: false, Required: false, Detail: detail}, true
}

func (c Checker) claudeAuthEnv(ctx context.Context) Check {
	lookup := c.ClaudeAuthLookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	source := strings.TrimSpace(c.ClaudeAuthSource)
	if source == "" {
		source = claudeShellAuthLabel
	}
	if c.ClaudeAuthError != nil {
		return Check{Name: "claude auth", Required: false, Detail: source + ": " + c.ClaudeAuthError.Error()}
	}
	auth := runtime.InspectClaudeAuthEnv(lookup)
	masked := source + ": " + auth.MaskedDetail()
	withWarn := func(detail string) string {
		if warning := auth.Warning(); warning != "" {
			return detail + "; " + warning
		}
		return detail
	}
	// Dashboard path (LiveProbe false): never spawn claude — GlobalChecks reruns
	// on every dashboard refresh. Report the env-presence signal only. A set token
	// reads OK; an unset one warns (foreground Claude may still authenticate via
	// cached ~/.claude credentials, hence non-required).
	if !c.LiveProbe {
		return Check{Name: "claude auth", OK: auth.Ready(), Required: false, Detail: withWarn(masked)}
	}
	// One-shot `gitmoot doctor` (LiveProbe true): a token that is merely SET is not
	// proof it authenticates (#486) — the prior code short-circuited a present token
	// to OK:true and never probed, so an invalid CLAUDE_CODE_OAUTH_TOKEN reported
	// ok. Probe the real dependency (a fresh `claude -p`, the same non-interactive
	// path daemon jobs take) and distinguish valid / invalid / unknown. The probe
	// also covers the cached-creds case (no env token) it always did.
	probeRunner := c.ClaudeProbeRunner
	if probeRunner == nil {
		probeRunner = c.runner()
	}
	probeErr := runtime.ClaudeLiveCheckEnv(ctx, probeRunner, "", nil)
	return claudeProbeCheck("claude auth", masked, runtime.ClaudeBackgroundTokenMessage, auth.Ready(), probeErr)
}

// claudeProbeCheck maps a live-probe outcome to a claude auth Check, distinguishing
// valid / invalid / unknown (#486). It is shared by the shell-local and the
// daemon-aware checks so both VALIDATE the credential instead of trusting that it
// is merely set:
//   - valid   → OK:true,  detail + okCaveat (the caveat is empty when the probe
//     validated the exact credential that matters, e.g. the daemon's own token).
//   - invalid → OK:false, the credential was rejected — the false-green this fixes.
//   - unknown → fail-open so doctor never goes red on a transient/network blip or a
//     missing binary: a binary that could not run is a probe-unavailable warn; a
//     transient error leaves a SET credential reported (OK) but clearly unvalidated,
//     while an UNSET credential stays a warn.
func claudeProbeCheck(name, masked, okCaveat string, ready bool, probeErr error) Check {
	switch runtime.ClaudeClassifyProbe(probeErr) {
	case runtime.ClaudeTokenValid:
		detail := masked
		if okCaveat != "" {
			detail += "; " + okCaveat
		}
		return Check{Name: name, OK: true, Required: false, Detail: detail}
	case runtime.ClaudeTokenInvalid:
		return Check{Name: name, OK: false, Required: false, Detail: masked + "; " + runtime.ClaudeSessionAuthFailedMessage}
	default: // unknown
		if runtime.ClaudeProbeUnavailable(probeErr) {
			// Missing/unrunnable binary is not an auth regression; the claude CLI
			// presence check already covers it. Keep it a non-required warn.
			return Check{Name: name, OK: false, Required: false, Detail: masked + "; " + runtime.ClaudeBackgroundTokenMessage + " (probe unavailable)"}
		}
		if ready {
			// A transient/network error must not flip a SET credential to a failure
			// (that would make doctor flaky); report it as set-but-unvalidated.
			return Check{Name: name, OK: true, Required: false, Detail: masked + "; could not validate the token (transient error); reporting it set-but-unvalidated"}
		}
		return Check{Name: name, OK: false, Required: false, Detail: masked + "; " + runtime.ClaudeBackgroundTokenMessage}
	}
}

func FailedRequired(checks []Check) error {
	var failed []string
	for _, check := range checks {
		if check.Required && !check.OK {
			failed = append(failed, check.Name)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("failed required checks: %s", strings.Join(failed, ", "))
}

func firstLine(values ...string) string {
	for _, value := range values {
		for _, line := range strings.Split(value, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				return line
			}
		}
	}
	return ""
}
