package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1824 criterion 1. A review re-runs the repository's own test suite at a SHA
// where CI already proved it green, which buys a fresh local observation of an
// established fact and spends the review's execution budget on the one kind of
// work CI already does.
//
// MEASURED, and the arithmetic is why this is worth a prompt block rather than
// a comment. From the engine's own routing_telemetry over 14 days, reviews that
// executed something (tests_run > 0) run a median of 9.0 minutes (n=842, p90
// 26.8m). The #1783 verdict's own tests_run put its baseline suite at about 4.7
// minutes of that. So on a median review the re-established baseline is a
// majority of the job.
//
// WHAT THIS DOES NOT DO. It does not tell a reviewer to skip verification, and
// it must not be read that way: #1824 is explicit that mutation testing is the
// rigor catching inert fixes across this fleet and "that work should not be
// cut". A mutation run also needs a local control, so the baseline cannot
// simply be deleted - the block says establish the control you need, not run
// nothing. The distinction is the whole point: CI can prove a tree green, and
// only the reviewer can prove a test would have failed without the fix.
//
// The predicates come from internal/github, the same single definition the
// merge gate uses, so a prompt cannot describe a head as green that the gate
// would refuse.

// reviewCIEvidenceLister is the read this block needs. It matches the merge
// gate's own dependency so a test can inject one fake for both.
type reviewCIEvidenceLister interface {
	ListCheckRunsForRef(ctx context.Context, repo github.Repository, ref string) ([]github.PullRequestCheck, error)
}

// dispatchReviewCIEvidence is the seam tests replace.
var dispatchReviewCIEvidence = reviewCIEvidenceBlock

// reviewCIEvidenceBlock renders the dispatch-time CI evidence for a review's
// exact head, or "" when there is nothing trustworthy to say.
//
// FAILS OPEN, deliberately and in three separate ways: a nil lister, a read
// error, and a head with zero checks all produce an EMPTY string and therefore
// a byte-identical prompt. A review must never fail to dispatch because GitHub
// was unreachable, and a reviewer must never be told "CI is green" on the
// strength of a read that did not happen.
func reviewCIEvidenceBlock(ctx context.Context, lister reviewCIEvidenceLister, repo github.Repository, headSHA string) string {
	headSHA = strings.TrimSpace(headSHA)
	if lister == nil || headSHA == "" {
		return ""
	}
	checks, err := lister.ListCheckRunsForRef(ctx, repo, headSHA)
	if err != nil {
		return ""
	}
	var passed, pending, failed []string
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		// The gate's own context is not external CI and must not be counted as
		// evidence about the tree: it reports whether the gate cleared the head,
		// which is a statement about the gate.
		if name == workflow.GitmootMergeGateContext {
			continue
		}
		if name == "" {
			name = "unnamed check"
		}
		switch {
		case github.CheckPending(check):
			pending = append(pending, name)
		case github.CheckPassed(check):
			passed = append(passed, name)
		default:
			failed = append(failed, name)
		}
	}
	sort.Strings(passed)
	sort.Strings(pending)
	sort.Strings(failed)
	total := len(passed) + len(pending) + len(failed)
	if total == 0 {
		// An empty rollup is "not started", never "not required" (#1783's
		// lesson, recorded in AGENTS.md). Saying nothing is correct here.
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n## CI at this exact head (dispatch-time evidence, #1824)\n\n")
	fmt.Fprintf(&b, "Head `%s` carries %d check runs: %d successful, %d pending, %d not successful.\n",
		headSHA, total, len(passed), len(pending), len(failed))

	switch {
	case len(failed) > 0:
		fmt.Fprintf(&b, "\nNOT green. Not successful: %s.\n", strings.Join(failed, ", "))
		b.WriteString("Do not report this tree as green, and do not treat a local pass as overriding a failing gate: " +
			"CI builds the branch merged into its base, which a local run does not.\n")
	case len(pending) > 0:
		fmt.Fprintf(&b, "\nNot yet established: %d check(s) still running (%s).\n", len(pending), strings.Join(pending, ", "))
		b.WriteString("There is no CI evidence for this head yet, so establish your own baseline as usual.\n")
	default:
		b.WriteString("\nAll of them succeeded, so the repository's own gate has ALREADY established that this tree " +
			"builds, vets and passes its suites at this exact commit.\n\n" +
			"Do not re-run the full suite to re-establish that. Establish only the baseline your own verification " +
			"needs as a control - a mutation run is worthless without one - and spend the rest of your budget on " +
			"what CI cannot do: mutating the production path and proving each mutant is caught, checking that a test " +
			"would have failed before the fix, and reading the diff against the obligations at this head.\n\n" +
			"State in `tests_run` that the baseline came from CI at this head, with the check count, so your verdict " +
			"records what established it rather than implying you ran nothing.\n")
	}
	return b.String()
}
