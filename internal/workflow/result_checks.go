package workflow

import (
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

// ResultChecksError is the typed error returned from Mailbox.Run when the audit
// is in block mode and one or more checks failed (#526). The job has already been
// transitioned to failed (via the shared contract-violation path) by the time it
// is returned; it carries the failed checks so a caller can inspect or log them.
type ResultChecksError struct {
	Failed []ResultCheck
}

func (e *ResultChecksError) Error() string {
	return SummarizeResultChecks(e.Failed)
}

// toDBResultCheckFailures maps the workflow-level failed checks onto the db
// feed-forward rows (#526). Only the per-check fields travel; the job/root/action
// context is passed alongside at the call site.
func toDBResultCheckFailures(failed []ResultCheck) []db.ResultCheckFailure {
	out := make([]db.ResultCheckFailure, 0, len(failed))
	for _, c := range failed {
		out = append(out, db.ResultCheckFailure{
			CheckID:     c.ID,
			Question:    c.Question,
			Explanation: c.Explanation,
		})
	}
	return out
}

// ResultCheckMode is the resolved result-check policy carried on the Engine and
// its Mailbox (#526). It is a plain string so the workflow package stays
// decoupled from internal/config (which owns parsing + the warn-by-default
// resolution). The zero value ("") is treated as OFF: every path that does not
// explicitly resolve the [workflow] result_checks config — every test, the ask/
// foreground path, and any Engine built with a bare struct literal — runs the
// audit disabled, so behavior is byte-identical. The daemon resolves the real
// mode (default warn) from config and wires it in.
type ResultCheckMode string

const (
	// ResultChecksOff disables the audit (byte-identical pre-#526 behavior). It is
	// also the meaning of the empty zero value.
	ResultChecksOff ResultCheckMode = "off"
	// ResultChecksWarn records failures as a job event + job-detail field + feed-
	// forward row without failing the job.
	ResultChecksWarn ResultCheckMode = "warn"
	// ResultChecksBlock additionally maps a failure onto the terminal contract-
	// violation path (the job fails), for strict workflows.
	ResultChecksBlock ResultCheckMode = "block"
)

// ResultChecksFailedEventKind is the job-event kind recorded when one or more
// deterministic result checks fail. It is visible in `gitmoot job events` and
// `gitmoot job show`.
const ResultChecksFailedEventKind = "result_checks_failed"

// normalizeResultCheckMode maps the zero value and any unrecognized string onto a
// safe mode. Empty ("") and "off" both disable the audit; only the exact "warn"
// and "block" values enable it, so a malformed injected value fails closed
// (disabled) rather than surprising an operator with a hard block.
func normalizeResultCheckMode(mode ResultCheckMode) ResultCheckMode {
	switch mode {
	case ResultChecksWarn:
		return ResultChecksWarn
	case ResultChecksBlock:
		return ResultChecksBlock
	default:
		return ResultChecksOff
	}
}

// ResultCheck is one deterministic yes/no audit of a parsed AgentResult (#526),
// modeled on BINEVAL's binary-verdict-plus-explanation shape. It is fully
// additive and serialized (omitempty on the payload slice) so a result that
// passes every applicable check is byte-identical on the wire.
type ResultCheck struct {
	// ID is a stable, machine-readable handle for the check (e.g.
	// "implement-tests-listed"), suitable for later SkillOpt aggregation.
	ID string `json:"id"`
	// Action is the job action the check applies to ("implement", "review",
	// "ask", "coordinator"), or "any" for a decision-scoped check that applies
	// regardless of action.
	Action string `json:"action"`
	// Question is the human-readable binary question the check answers.
	Question string `json:"question"`
	// Pass is the binary verdict.
	Pass bool `json:"pass"`
	// Explanation states, in one sentence, why the check failed (empty when it
	// passed) so the failure is self-describing in job output and the dashboard.
	Explanation string `json:"explanation"`
}

// ResultCheckInput carries the minimal job context the deterministic checks need
// beyond the parsed result: the job action and whether the job is a coordinator
// finalize continuation (payload.DelegationFinalize). Keeping this a small value
// type makes the check set trivially unit-testable without a store or a job row.
type ResultCheckInput struct {
	Action     string
	IsFinalize bool
	Result     AgentResult
}

// minActionableAnswerChars is the floor below which an ask/finalize answer is
// treated as non-actionable. It is intentionally tiny: a valid gitmoot_result
// already requires a non-empty summary, so this only catches degenerate
// single-token answers ("s", ".") rather than second-guessing terse-but-real
// answers like "ok" or "done".
const minActionableAnswerChars = 3

// RunResultChecks evaluates every deterministic check that applies to the given
// action/result and returns them all (passing and failing), each with its binary
// verdict and — when failing — an explanation. It performs NO LLM call and reads
// only fields that exist on AgentResult (see internal/workflow/result.go), so it
// is pure, fast, and side-effect-free. Callers that only care about failures use
// FailedResultChecks.
func RunResultChecks(in ResultCheckInput) []ResultCheck {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	r := in.Result
	var checks []ResultCheck

	// Decision-scoped (action-agnostic): a blocked result must list actionable
	// blockers. The engine already routes a blocked decision's `needs` into
	// resumable gates (#682); an empty or blank needs list is a blocked result with
	// nothing to act on.
	if r.Decision == "blocked" {
		pass := hasActionableEntries(r.Needs)
		checks = append(checks, ResultCheck{
			ID:          "blocked-blockers-actionable",
			Action:      "any",
			Question:    "Does the blocked result list actionable blockers (needs)?",
			Pass:        pass,
			Explanation: explain(pass, "the result is blocked but lists no actionable blockers in needs[]"),
		})
	}

	switch action {
	case "implement":
		// A job that claims it implemented changes must enumerate them, and must
		// list the tests it ran, so a human/continuation can see what actually
		// happened rather than trusting the summary prose.
		if r.Decision == "implemented" {
			madePass := len(r.ChangesMade) > 0
			checks = append(checks, ResultCheck{
				ID:          "implement-changes-listed",
				Action:      "implement",
				Question:    "Did the implement job list the concrete changes it made?",
				Pass:        madePass,
				Explanation: explain(madePass, "the implement job reports decision \"implemented\" but changes_made[] is empty"),
			})
			testsPass := len(r.TestsRun) > 0
			checks = append(checks, ResultCheck{
				ID:          "implement-tests-listed",
				Action:      "implement",
				Question:    "Did the implement job list the tests it ran?",
				Pass:        testsPass,
				Explanation: explain(testsPass, "the implement job reports decision \"implemented\" but tests_run[] is empty"),
			})
		}
	case "review":
		// A changes-requested review must carry findings — the concrete, evidence-
		// bearing objections the author is expected to address. A bare
		// changes_requested with no findings is an un-actionable verdict.
		if r.Decision == "changes_requested" {
			pass := len(r.Findings) > 0
			checks = append(checks, ResultCheck{
				ID:          "review-evidence-present",
				Action:      "review",
				Question:    "Does a changes-requested review include findings/evidence?",
				Pass:        pass,
				Explanation: explain(pass, "the review requests changes but findings[] is empty, so there is no evidence to act on"),
			})
		}
	case "ask":
		// The coordinator finalize continuation is dispatched as an "ask" carrying
		// DelegationFinalize (#305): it is a reconciliation, not a plain answer, so
		// it gets the coordinator check below instead of the ask-answer check.
		if !in.IsFinalize {
			pass := isActionableAnswer(r)
			checks = append(checks, ResultCheck{
				ID:          "ask-answer-actionable",
				Action:      "ask",
				Question:    "Did the ask job return a non-empty, actionable answer?",
				Pass:        pass,
				Explanation: explain(pass, "the ask job's answer (summary/artifact_body) is empty or too short to be actionable"),
			})
		}
	}

	// Coordinator finalize continuation (#305): a coordinator re-invoked to
	// reconcile its children's results must produce a substantive synthesis rather
	// than a terse rubber-stamp. This grounds "reconcile the children" on the
	// fields available at result-parse time (a substantive answer body); it does
	// NOT cross-reference each child job — deeper per-child reconciliation is left
	// as future work once child results are threaded to this seam.
	if in.IsFinalize {
		pass := isActionableAnswer(r)
		checks = append(checks, ResultCheck{
			ID:          "coordinator-outcome-reconciled",
			Action:      "coordinator",
			Question:    "Did the coordinator reconcile and summarize its children's outcomes?",
			Pass:        pass,
			Explanation: explain(pass, "the coordinator finalize produced no substantive reconciliation summary"),
		})
	}

	return checks
}

// FailedResultChecks runs the audit and returns only the checks that failed, in
// check order. An empty slice means the result passed every applicable check.
func FailedResultChecks(in ResultCheckInput) []ResultCheck {
	var failed []ResultCheck
	for _, c := range RunResultChecks(in) {
		if !c.Pass {
			failed = append(failed, c)
		}
	}
	return failed
}

// SummarizeResultChecks renders a one-line, human-readable summary of the failed
// checks for the job event message, e.g. "2 result check(s) failed:
// implement-tests-listed (…); blocked-blockers-actionable (…)".
func SummarizeResultChecks(failed []ResultCheck) string {
	if len(failed) == 0 {
		return "all result checks passed"
	}
	parts := make([]string, 0, len(failed))
	for _, c := range failed {
		parts = append(parts, fmt.Sprintf("%s (%s)", c.ID, c.Explanation))
	}
	return fmt.Sprintf("%d result check(s) failed: %s", len(failed), strings.Join(parts, "; "))
}

// isActionableAnswer reports whether a result carries a non-trivial answer body:
// a summary at least minActionableAnswerChars long, or a non-empty artifact_body,
// or any findings. It is the shared deterministic proxy for "actionable" used by
// the ask and coordinator checks.
func isActionableAnswer(r AgentResult) bool {
	if len(strings.TrimSpace(r.Summary)) >= minActionableAnswerChars {
		return true
	}
	if strings.TrimSpace(r.ArtifactBody) != "" {
		return true
	}
	return len(r.Findings) > 0
}

// hasActionableEntries reports whether a string slice contains at least one
// non-blank entry, so an all-empty or single-blank list counts as no entries.
func hasActionableEntries(values []string) bool {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// explain returns the failure explanation for a failed check and "" for a passed
// one, so the ResultCheck.Explanation field is empty exactly when Pass is true.
func explain(pass bool, why string) string {
	if pass {
		return ""
	}
	return why
}
