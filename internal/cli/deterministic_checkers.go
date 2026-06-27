package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// Checker names recognized by the deterministic-checker dispatcher (#485). They
// match the [skillopt].deterministic_checkers selector values. diff_size is pure-Go
// and ALWAYS available; the rest are tool-backed and DEGRADE-SKIP when their binary
// or a PR-head checkout is absent.
const (
	checkerDiffSize    = "diff_size"
	checkerDuplication = "duplication"
	checkerLint        = "lint"
	checkerComplexity  = "complexity"
)

// checkerDiffSizeSoftCapLines is the changed-line soft cap the diff-size metric
// normalizes against (#485): a diff at or below it scores ~1.0 (tight, easy to
// review); a larger diff scores lower toward 0 as it grows. It is a coarse,
// host-relative quality prior (a smaller, more contained change is preferable), so
// the optimizer weights this objective-but-soft dimension accordingly.
const checkerDiffSizeSoftCapLines = 400

// diffFileLister is the minimal read the diff-size metric needs (github.Client
// satisfies it): list the PR's changed files + their unified-diff patches at HEAD.
// It is its own narrow interface so the dispatcher is unit-testable with a stub and
// so the read is provably read-only (the same seam the #469 review diff uses).
type diffFileLister interface {
	ListPullRequestFiles(ctx context.Context, repo github.Repository, number int64) ([]github.PullRequestFile, error)
}

// deterministicCheckerDispatcher is the concrete workflow.DeterministicCheckerDispatcher
// (#485): it computes a pure-Go diff-size dimension from the PR file patches and
// runs degrade-graceful external-tool dimensions (duplication via dupl/jscpd, lint
// via golangci-lint, cyclomatic complexity via gocyclo) against a working tree when
// one is available, normalizes each to [0,1], and returns an
// Outcome{Kind:OutcomeReviewed, Objective:true} for the engine to harvest into the
// SAME auto-trace run as a THIRD coexisting OBJECTIVE signal. Every dimension is
// best-effort: a missing tool binary (LookPath miss), no checkout, or a tool
// error/timeout SKIPS that ONE dimension (it is omitted from the Rubric) and the
// leg proceeds with whatever survives — NEVER an error, NEVER a blocked merge. An
// all-skipped run yields an empty Rubric => ok=false => no checker row.
type deterministicCheckerDispatcher struct {
	store    *db.Store
	diff     diffFileLister
	runner   subprocess.Runner
	checkout string
	// checkers is the resolved per-checker selector (already defaulted to the safe
	// set by the daemon constructor). Only the listed checkers run.
	checkers []string
}

var _ workflow.DeterministicCheckerDispatcher = (*deterministicCheckerDispatcher)(nil)

// Check runs the selected deterministic checkers for a just-merged implement job
// and returns their objective tool dimensions (#485). ok=false means NO dimension
// was producible at all (every checker skipped), so the engine writes no checker
// row. It NEVER mutates the merge: it reads the diff read-only and runs read-only
// tools, returning a value the engine harvests into the auto-trace run.
func (d *deterministicCheckerDispatcher) Check(ctx context.Context, implementJob db.Job, implementPayload workflow.JobPayload, mergedHead string) (workflow.Outcome, bool, error) {
	rubric := map[string]float64{}
	ran := make([]string, 0, len(d.checkers))

	for _, name := range d.checkers {
		score, ok := d.runChecker(ctx, strings.TrimSpace(name), implementPayload)
		if !ok {
			// DEGRADE-SKIP: the tool is absent, no checkout, or it errored/timed out.
			// Omit the dimension and proceed; never fail the leg.
			continue
		}
		rubric[name] = clampUnitChecker(score)
		ran = append(ran, name)
	}

	if len(rubric) == 0 {
		// Every checker skipped: nothing producible. ok=false => no checker row (the
		// harvester would skip an empty rubric anyway, but skipping here avoids the
		// store round-trip entirely).
		return workflow.Outcome{}, false, nil
	}

	sort.Strings(ran)
	findings := fmt.Sprintf("Deterministic checkers on PR #%d: %s.", implementPayload.PullRequest, strings.Join(ran, ", "))

	return workflow.Outcome{
		Kind:        workflow.OutcomeReviewed,
		Objective:   true,
		Repo:        implementPayload.Repo,
		PullRequest: implementPayload.PullRequest,
		HeadSHA:     mergedHead,
		Rubric:      rubric,
		Findings:    findings,
	}, true, nil
}

// runChecker dispatches one named checker and returns its [0,1] score. ok=false
// means SKIP (omit the dimension): an unknown name, a missing tool, no checkout, or
// a tool error/timeout — all degrade to skip, never an error.
func (d *deterministicCheckerDispatcher) runChecker(ctx context.Context, name string, payload workflow.JobPayload) (float64, bool) {
	switch name {
	case checkerDiffSize:
		// Pure-Go, always available: no tool, no checkout needed.
		return d.diffSizeScore(ctx, payload)
	case checkerDuplication:
		return d.duplicationScore(ctx)
	case checkerLint:
		return d.lintScore(ctx)
	case checkerComplexity:
		return d.complexityScore(ctx)
	default:
		// Unknown checker name (typo / unsupported): ignore, best-effort.
		return 0, false
	}
}

// diffSizeScore computes the pure-Go diff-size dimension from the GitHub PR file
// patches (#485): it counts changed (+/-) hunk lines across every file's unified
// diff and the number of changed files, then normalizes the total changed lines
// against checkerDiffSizeSoftCapLines so a tight diff scores ~1.0 and a
// larger-than-cap diff scores lower toward 0. It has NO external tool and NO
// checkout dependency, so it ALWAYS produces a dimension when the PR file read
// succeeds — the one always-available objective signal. A failed read or no PR
// degrades to SKIP (ok=false) so a malformed read is never scored.
func (d *deterministicCheckerDispatcher) diffSizeScore(ctx context.Context, payload workflow.JobPayload) (float64, bool) {
	if d.diff == nil || payload.PullRequest <= 0 {
		return 0, false
	}
	repo, ok := parseCheckerRepo(payload.Repo)
	if !ok {
		return 0, false
	}
	files, err := d.diff.ListPullRequestFiles(ctx, repo, int64(payload.PullRequest))
	if err != nil {
		return 0, false
	}
	changedLines := 0
	for _, file := range files {
		changedLines += countPatchChangedLines(file.Patch)
	}
	return diffSizeNormalize(changedLines), true
}

// countPatchChangedLines counts the added/removed lines in a unified-diff patch:
// lines beginning with a single '+' or '-' that are NOT the '+++'/'---' file
// headers and NOT an '@@' hunk header. It parses the patch TEXT because
// github.PullRequestFile carries no additions/deletions integers (only the Patch
// string) — so the metric is derived purely from the diff body.
func countPatchChangedLines(patch string) int {
	if strings.TrimSpace(patch) == "" {
		return 0
	}
	count := 0
	for _, line := range strings.Split(patch, "\n") {
		if line == "" {
			continue
		}
		// Skip the file headers (+++ / ---) and hunk headers (@@ ... @@).
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@") {
			continue
		}
		switch line[0] {
		case '+', '-':
			count++
		}
	}
	return count
}

// diffSizeNormalize maps a changed-line count to a [0,1] score: at or below the soft
// cap scores 1.0 (tight, contained diff), and beyond it decays linearly toward 0 as
// the diff grows to twice the cap, flooring at 0. The mapping is intentionally
// coarse + bucketed (a host-relative soft prior, not a hard gate) so the objective
// row stays reproducible enough to be useful while the optimizer weights it as a
// soft signal.
func diffSizeNormalize(changedLines int) float64 {
	if changedLines <= checkerDiffSizeSoftCapLines {
		return 1.0
	}
	over := changedLines - checkerDiffSizeSoftCapLines
	// Decay across one more cap's worth of lines down to 0.
	score := 1.0 - float64(over)/float64(checkerDiffSizeSoftCapLines)
	if score < 0 {
		return 0
	}
	return score
}

// duplicationScore runs the code-duplication tool (dupl, else jscpd) against the
// checkout and normalizes its reported duplicate count to [0,1] (#485). It
// DEGRADE-SKIPS (ok=false) when no checkout is available, neither tool binary is on
// PATH, or the tool truly failed (errored with no usable output) — so a tool-less
// host omits the dimension rather than failing.
//
// CRITICAL (#485 review): jscpd (and some dupl wrappers) exit NON-ZERO precisely
// when duplication EXCEEDS a threshold — i.e. exactly when there ARE clones to
// report, which is the signal the metric exists to capture. subprocess.Run returns a
// non-nil error for any non-zero exit, so blanket-skipping on err!=nil would drop
// the dimension on the WORST diffs and keep it (scoring 1.0) only on clean ones,
// biasing the objective upward. Instead we treat parseable stdout as a real signal
// regardless of exit code, and only SKIP when the run produced no usable output AND
// errored (binary crash / context timeout).
func (d *deterministicCheckerDispatcher) duplicationScore(ctx context.Context) (float64, bool) {
	if strings.TrimSpace(d.checkout) == "" || d.runner == nil {
		return 0, false
	}
	// Prefer dupl; fall back to jscpd. The leg only needs a clones COUNT, which both
	// can report; absence => SKIP.
	tool, ok := d.firstAvailable("dupl", "jscpd")
	if !ok {
		return 0, false
	}
	var args []string
	switch tool {
	case "dupl":
		args = []string{"-plumbing", "."}
	case "jscpd":
		args = []string{"--silent", "."}
	}
	res, err := d.runner.Run(ctx, d.checkout, tool, args...)
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		// No usable output: a real failure (err) SKIPS; a genuinely clean exit with no
		// clones scores 1.0.
		if err != nil {
			return 0, false
		}
		return 1.0, true
	}
	// Parseable clone listing (one clone/finding per line) is a real signal even when
	// the tool exits non-zero to flag "duplication found".
	return duplicationNormalize(countNonEmptyLines(out)), true
}

// duplicationNormalize maps a reported duplicate-clone count to [0,1]: zero clones
// is best (1.0), and the score decays as clones accumulate (bucketed, host-relative).
func duplicationNormalize(clones int) float64 {
	if clones <= 0 {
		return 1.0
	}
	// Each clone costs a fixed fraction; floor at 0 for heavily-duplicated diffs.
	score := 1.0 - float64(clones)*0.1
	if score < 0 {
		return 0
	}
	return score
}

// lintScore runs golangci-lint against the checkout and normalizes the issue count
// to [0,1] (#485). It DEGRADE-SKIPS (ok=false) when no checkout is available, the
// binary is absent, or the tool truly failed (errored with no usable output) — never
// failing the leg on a tool-less host.
//
// CRITICAL (#485 review): golangci-lint exits non-zero for TWO distinct reasons:
//  1. it FOUND lint issues (exit 1, with the issues on stdout), and
//  2. a genuine tool FAILURE (a compile/typecheck error in the tree, an invalid
//     .golangci.yml, an OOM/panic, an incompatible flag, or a context timeout —
//     exit >=2 with the diagnostic on stderr and STDOUT empty).
//
// We must NOT swallow case 2 and fabricate a perfect 1.0 "clean lint" signal from a
// linter that never actually linted (that violates DEGRADE-GRACEFULLY and corrupts
// the objective auto-trace dimension). So we honor the error: parseable stdout is a
// real issue-count signal regardless of exit code, an empty stdout with NO error is a
// genuinely clean lint (1.0), and an empty/unparseable stdout WITH an error SKIPS.
func (d *deterministicCheckerDispatcher) lintScore(ctx context.Context) (float64, bool) {
	if strings.TrimSpace(d.checkout) == "" || d.runner == nil {
		return 0, false
	}
	if _, err := d.runner.LookPath("golangci-lint"); err != nil {
		return 0, false
	}
	// `run` prints one issue per line; we count non-empty output lines as a coarse
	// issue count.
	res, err := d.runner.Run(ctx, d.checkout, "golangci-lint", "run")
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		// No usable output: a real failure (err) SKIPS rather than fabricating a 1.0;
		// a clean run (no err, no issues) scores 1.0.
		if err != nil {
			return 0, false
		}
		return 1.0, true
	}
	// Non-empty parseable output: a real issue count even if the exit was non-zero
	// (non-zero merely flags "issues found").
	return lintNormalize(countNonEmptyLines(out)), true
}

// lintNormalize maps a lint-issue count to [0,1]: zero issues is best (1.0), more
// issues decay the score (bucketed, host/version-relative — different linter
// versions may differ, which is why this is weighted as a soft objective prior).
func lintNormalize(issues int) float64 {
	if issues <= 0 {
		return 1.0
	}
	score := 1.0 - float64(issues)*0.05
	if score < 0 {
		return 0
	}
	return score
}

// complexityScore runs the cyclomatic-complexity tool (gocyclo) against the checkout
// and normalizes the over-threshold function count to [0,1] (#485). It
// DEGRADE-SKIPS (ok=false) when no checkout is available, the binary is absent, or
// the tool truly failed (errored with no usable output).
//
// CRITICAL (#485 review): `gocyclo -over 15` exits NON-ZERO precisely WHEN
// over-threshold functions exist — i.e. exactly the case the complexity dimension
// exists to penalize. subprocess.Run returns a non-nil error for that non-zero exit,
// so blanket-skipping on err!=nil would emit the dimension ONLY on a clean tree
// (scoring 1.0) and drop it whenever there is real complexity to flag — silently
// inverting the metric. Instead the function list on stdout is the signal of truth:
// parse it regardless of exit code, and only SKIP when the run produced no usable
// output AND errored (binary crash / context timeout).
func (d *deterministicCheckerDispatcher) complexityScore(ctx context.Context) (float64, bool) {
	if strings.TrimSpace(d.checkout) == "" || d.runner == nil {
		return 0, false
	}
	if _, err := d.runner.LookPath("gocyclo"); err != nil {
		return 0, false
	}
	// `gocyclo -over 15 .` lists every function whose complexity exceeds 15, one per
	// line, and exits non-zero when that list is non-empty; an empty list means no
	// over-threshold function.
	res, err := d.runner.Run(ctx, d.checkout, "gocyclo", "-over", "15", ".")
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		// No usable output: a real failure (err) SKIPS; a clean tree (no err, no
		// over-threshold functions) scores 1.0.
		if err != nil {
			return 0, false
		}
		return 1.0, true
	}
	// The over-threshold function listing is a real signal even though gocyclo exits
	// non-zero to flag that such functions exist.
	return complexityNormalize(countNonEmptyLines(out)), true
}

// complexityNormalize maps the over-threshold function count to [0,1]: none is best
// (1.0), more over-complex functions decay the score (bucketed, host-relative).
func complexityNormalize(overThreshold int) float64 {
	if overThreshold <= 0 {
		return 1.0
	}
	score := 1.0 - float64(overThreshold)*0.1
	if score < 0 {
		return 0
	}
	return score
}

// firstAvailable returns the first of the given tool names found on PATH via the
// runner's LookPath, so the dispatcher can prefer one tool and fall back to another.
// ok=false when none is available (DEGRADE-SKIP).
func (d *deterministicCheckerDispatcher) firstAvailable(names ...string) (string, bool) {
	if d.runner == nil {
		return "", false
	}
	for _, name := range names {
		if _, err := d.runner.LookPath(name); err == nil {
			return name, true
		}
	}
	return "", false
}

// countNonEmptyLines counts the non-blank lines in tool output, a coarse proxy for
// the issue/clone/function count tools emit one-per-line.
func countNonEmptyLines(out string) int {
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// clampUnitChecker clamps a normalized checker score to [0,1] so a metric that
// returns an out-of-range value cannot push the projected mean outside the contract
// range.
func clampUnitChecker(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// parseCheckerRepo splits an "owner/name" repo into a github.Repository for the
// diff-size file read. It reports ok=false for a malformed repo (so the metric
// degrades to SKIP rather than panicking), mirroring parseReviewRepo.
func parseCheckerRepo(value string) (github.Repository, bool) {
	owner, name, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return github.Repository{}, false
	}
	return github.Repository{Owner: owner, Name: name}, true
}
