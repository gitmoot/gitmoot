package doctor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

type Check struct {
	Name     string
	OK       bool
	Required bool
	Detail   string
	// State is populated when a check has a structured domain state whose
	// distinctions are not represented by OK/status alone.
	State string
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

type StuckJobProcess struct {
	JobID string
	PID   int
	Live  bool
	Known bool
}

type StuckJobsStatus struct {
	Jobs []StuckJobProcess
}

// DaemonLogStatus records the evidence needed to decide whether the running
// daemon is still writing the log path advertised by `daemon status`.
type DaemonLogStatus struct {
	DaemonRunning bool
	Determined    bool
	Stale         bool
	Missing       bool
	LogPath       string
	StartedAt     time.Time
	LastWrite     time.Time
}

type Checker struct {
	Dir    string
	Runner subprocess.Runner
	// Paths locates the gitmoot home for checks that read its config (the org
	// registry's herdr version check). Unset skips them.
	Paths config.Paths
	// Build is supplied by the CLI so this package stays independent of both
	// buildinfo and daemon-state persistence. Nil omits the check for callers
	// such as the continuously refreshed terminal dashboard.
	Build *BuildStatus
	// StuckJobs is supplied by the CLI, which owns store access and process
	// probing. Nil omits the check for callers such as the dashboard.
	StuckJobs *StuckJobsStatus
	// RuntimeServing is supplied by the CLI, which owns store access, and names
	// the runtimes the engine is currently holding off a provider refusal
	// (#1558). Nil omits the check: a dashboard that cannot resolve holds must
	// not imply every runtime is serving.
	RuntimeServing *RuntimeServingStatus
	// LogStatus is supplied by the one-shot CLI. Nil omits the check for callers
	// such as the continuously refreshed terminal dashboard.
	LogStatus *DaemonLogStatus
	// RuntimeContracts are supplied by the one-shot CLI after its lazy installed
	// binary probes. Dashboard callers leave this empty and spawn no probes.
	RuntimeContracts []runtime.RuntimeContractResult
}

// Run returns the global (cwd-independent) checks followed by the per-repo
// checks for the Checker's Dir, treated as a single checkout. This is the
// `gitmoot doctor --repo <dir>` view.
func (c Checker) Run(ctx context.Context) []Check {
	checks := c.GlobalChecks(ctx)
	return append(checks, c.RepoChecks(ctx, c.Dir)...)
}

// OmpRemediation is appended when the omp binary is missing or broken: omp is
// the only job runtime (#2587), so every review, ask and implement job needs it.
const OmpRemediation = "gitmoot requires omp to run jobs; install omp (oh-my-pi) and make sure it is on the daemon's PATH"

// GlobalChecks returns the diagnostics that do not depend on any repo checkout:
// the required binaries (git, gh and the omp job runtime) and gh auth. They can
// run from anywhere, so the dashboard renders them once.
func (c Checker) GlobalChecks(ctx context.Context) []Check {
	runner := c.runner()
	omp := c.command(ctx, runner, "omp", true, "--version")
	if !omp.OK {
		omp.Detail = strings.TrimPrefix(strings.TrimSpace(omp.Detail)+"; "+OmpRemediation, "; ")
	}
	checks := []Check{
		c.command(ctx, runner, "git", true, "--version"),
		c.command(ctx, runner, "gh", true, "--version"),
		omp,
	}
	checks = append(checks, c.ghAuth(ctx, runner))
	if c.Build != nil {
		checks = append(checks, CheckBuild(*c.Build))
	}
	if c.StuckJobs != nil {
		checks = append(checks, CheckStuckJobs(*c.StuckJobs))
	}
	if c.RuntimeServing != nil {
		checks = append(checks, CheckRuntimeServing(*c.RuntimeServing))
	}
	if c.LogStatus != nil {
		// Preserve the existing doctor surface unless staleness is positively
		// established. Fresh, stopped, and indeterminate states are neutral.
		if check := CheckDaemonLog(*c.LogStatus); !check.OK {
			checks = append(checks, check)
		}
	}
	for _, result := range c.RuntimeContracts {
		checks = append(checks, CheckRuntimeContract(result))
	}
	if strings.TrimSpace(c.Paths.ConfigFile) != "" {
		cfg, err := config.LoadOrg(c.Paths)
		if err != nil {
			checks = append(checks, Check{Name: "org registry", Required: true, Detail: fmt.Sprintf("load org registry: %v", err)})
		} else if cfg.Enabled() {
			checks = append(checks, CheckHerdrVersion(ctx, runner, OrgMinimumHerdrVersion))
		}
	}
	return checks
}

// CheckRuntimeContract surfaces all three preflight states. Unsupported and
// unknown are warnings because the runtimes are optional doctor dependencies;
// dispatch itself blocks only unsupported.
func CheckRuntimeContract(result runtime.RuntimeContractResult) Check {
	check := Check{Name: "contract " + result.Runtime, OK: result.State == runtime.RuntimeContractSupported, State: string(result.State)}
	switch result.State {
	case runtime.RuntimeContractSupported:
		check.Detail = fmt.Sprintf("supported (version %s; instrument %s)", result.Version, result.Instrument)
	case runtime.RuntimeContractUnsupported:
		check.Detail = fmt.Sprintf("contract-violated (version %s; instrument %s)", result.Version, result.Instrument)
		for _, requirement := range result.Requirements {
			if requirement.State == runtime.RuntimeContractUnsupported {
				check.Detail += ": " + requirement.Name + " required by " + requirement.Source
				break
			}
		}
	case runtime.RuntimeContractUnknown:
		check.Detail = fmt.Sprintf("unknown (version %s; instrument %s); dispatch remains fail-open", result.Version, result.Instrument)
	default:
		check.Detail = "unknown contract state; dispatch remains fail-open"
	}
	return check
}

// CheckStuckJobs reports running jobs whose directly recorded runtime process
// is confirmably dead. Unknowns are neutral: a legacy job with no PID, an
// unavailable process table, or an identity that could not be captured never
// produces a finding.
func CheckStuckJobs(status StuckJobsStatus) Check {
	check := Check{
		Name:     "stuck runs",
		OK:       true,
		Required: true,
		Detail:   "no active runs with a dead recorded runtime process",
	}
	var dead []string
	for _, job := range status.Jobs {
		if !job.Known || job.Live || job.PID <= 0 {
			continue
		}
		dead = append(dead, fmt.Sprintf("%s (pid %d)", job.JobID, job.PID))
	}
	if len(dead) == 0 {
		return check
	}
	check.OK = false
	check.Detail = fmt.Sprintf("%d active run(s) have a dead recorded runtime process: %s", len(dead), strings.Join(dead, ", "))
	return check
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

// CheckDaemonLog warns only when a running daemon's advertised log is
// definitely missing or predates that daemon's recorded start. Every unknown is
// neutral, matching CheckBuild's conservative daemon-state posture.
func CheckDaemonLog(status DaemonLogStatus) Check {
	check := Check{Name: "daemon log", OK: true, Required: false}
	if !status.DaemonRunning || !status.Determined || !status.Stale {
		return check
	}

	check.OK = false
	lastWrite := "missing (no timestamp)"
	if !status.Missing && !status.LastWrite.IsZero() {
		lastWrite = status.LastWrite.UTC().Format(time.RFC3339)
	}
	check.Detail = fmt.Sprintf(
		"advertised log %s is stale (last write: %s; daemon started: %s); if running under systemd, follow logs with journalctl --user -u gitmoot-daemon -f",
		status.LogPath,
		lastWrite,
		status.StartedAt.UTC().Format(time.RFC3339),
	)
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

// RuntimeServingBlock is one runtime the ENGINE is currently holding off a
// provider refusal (#1558). It is inert data: the CLI resolves it from the
// store and doctor renders it, so this package keeps its store independence.
type RuntimeServingBlock struct {
	// Runtime is the runtime the blocked job would have run on, resolved by the
	// caller. A job whose runtime cannot be attributed is NOT reported at all.
	Runtime string
	// Class is the blocker class the classifier recorded (runtime_quota or
	// runtime_auth).
	Class string
	// JobID and Detail name the evidence so the row is actionable rather than
	// merely red.
	JobID  string
	Detail string
	// RetryAt is the engine's own forward-looking hold: the moment it intends to
	// retry. The caller reports a block ONLY while this is in the future, so the
	// verdict is the engine's current belief rather than a guess from a clock.
	RetryAt time.Time
}

// RuntimeServingStatus records the provider-refusal holds the engine is sitting
// on right now.
type RuntimeServingStatus struct {
	Blocks []RuntimeServingBlock
}

// CheckRuntimeServing distinguishes "installed and contract-valid" from
// "serving" (#1558).
//
// The false green this replaces: `doctor` reported kimi `ok 0.29.2 / contract
// ok` while the daemon had, eleven minutes earlier, deferred two review jobs on
// `provider.api_error: 403 You've reached your usage limit for this billing
// cycle`. Capability present, service unavailable - the same shape AGENTS.md
// already records for CLAUDE_CODE_OAUTH_TOKEN, one layer out. It cost a real
// decision: kimi went on a review panel on doctor's word and its legs deferred
// indefinitely.
//
// WHAT THIS CHECK DELIBERATELY IS NOT: a second opinion about the provider. It
// reports only what the ENGINE currently believes, keyed on the forward-looking
// BlockerRetryAt the classifier wrote when it deferred the job. A verdict
// derived from "the store says 403 sometime" would be a stale row read as
// current - and replacing a false green with a false red is the same defect with
// the sign flipped. The caller therefore filters to holds that have not expired,
// and every state it cannot interpret is neutral.
//
// It is a WARNING, not a failure: the runtime is installed and its contract is
// valid, the provider is refusing today, and the operator's decision is which
// runtime to put on a panel rather than whether the box is broken.
func CheckRuntimeServing(status RuntimeServingStatus) Check {
	check := Check{
		Name:   "runtime serving",
		OK:     true,
		Detail: "no runtime is held on a provider refusal the engine is still waiting out",
	}
	if len(status.Blocks) == 0 {
		return check
	}
	details := make([]string, 0, len(status.Blocks))
	for _, block := range status.Blocks {
		detail := fmt.Sprintf("%s: %s until %s (job %s)", block.Runtime, block.Class,
			block.RetryAt.UTC().Format(time.RFC3339), block.JobID)
		if strings.TrimSpace(block.Detail) != "" {
			detail += ": " + strings.TrimSpace(block.Detail)
		}
		details = append(details, detail)
	}
	sort.Strings(details)
	check.OK = false
	check.State = "warn"
	check.Detail = fmt.Sprintf("%d runtime(s) installed and contract-valid but NOT serving - the engine is holding a provider refusal: %s",
		len(status.Blocks), strings.Join(details, "; "))
	return check
}
