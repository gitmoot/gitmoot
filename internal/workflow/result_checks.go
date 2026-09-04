package workflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
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
	// ResultChecksOff disables the audit. It is also the meaning of the empty
	// zero value. Engine observations are persisted independently of this policy.
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
	// "implement-tests-listed"), suitable for later aggregation.
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
// beyond the parsed result: the job action, whether the job is a coordinator
// finalize continuation (payload.DelegationFinalize), and the engine's persisted
// observation of an implement worktree when one was available. Keeping this a
// small value type makes the check set trivially unit-testable without a store or
// a job row.
type ResultCheckInput struct {
	Action      string
	IsFinalize  bool
	Result      AgentResult
	Observation *ResultObservation
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
// only its value input (the claim plus an engine-owned worktree observation), so
// it is pure, fast, and side-effect-free. Callers that only care about failures
// use FailedResultChecks.
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
			// Same content test the needs gate uses: two gates disagreeing
			// about the same input is the defect, not the leniency. Before
			// this, the {} that the needs gate rejects satisfied these.
			madePass := hasActionableEntries(r.ChangesMade)
			checks = append(checks, ResultCheck{
				ID:          "implement-changes-listed",
				Action:      "implement",
				Question:    "Did the implement job list the concrete changes it made?",
				Pass:        madePass,
				Explanation: explain(madePass, "the implement job reports decision \"implemented\" but changes_made[] is empty"),
			})
			if in.Observation != nil && in.Observation.Source == ResultObservationSourceWorktreeDiff && in.Observation.Error == "" {
				invalidBindings := invalidCapturedBindingClaims(in.Observation.Changes, in.Observation.TouchedFiles)
				consistent := len(in.Observation.ClaimedOnlyFiles) == 0 && len(in.Observation.UnclaimedFiles) == 0 && len(in.Observation.UnboundClaims) == 0 && len(invalidBindings) == 0
				checks = append(checks, ResultCheck{
					ID:       "implement-changes-observed",
					Action:   "implement",
					Question: "Do the reported changes_made files match the files observed in the worktree diff?",
					Pass:     consistent,
					Explanation: explain(consistent, fmt.Sprintf(
						"work may be missing: claimed changes_made files absent from the diff: %s; diff files absent from changes_made: %s; claims without a touched-file path binding: %s; invalid captured path bindings: %s",
						displayPaths(in.Observation.ClaimedOnlyFiles),
						displayPaths(in.Observation.UnclaimedFiles),
						displayPaths(in.Observation.UnboundClaims),
						displayPaths(invalidBindings),
					)),
				})
			}
			testsPass := hasActionableEntries(r.TestsRun)
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
		// #1685: a review result that declares delegations is a coordinator FAN-OUT.
		// It is NOT refused here, and an earlier version of this file that refused it
		// was wrong twice over: the shipped review-panel template prescribes exactly
		// that result, so the check failed the product's own documented recipe, and
		// under result_checks=block it failed the job before AdvanceJob ever reached
		// dispatchDelegations — the announced panel could never run. Emitting a
		// fan-out is legitimate; COUNTING one as a verdict is the defect, and that is
		// fixed in the consumers that read decision (merge gate, pipeline auto-merge,
		// required-reviewer counting, the proof projector, the verdict wake).
		//
		// The evidence nudge below is skipped for the same reason: a coordinator that
		// has just announced a panel legitimately has no findings or tests_run yet,
		// and recording a contract violation on every panel run would train readers
		// to ignore the check.
		if isTerminalReviewVerdict(r.Decision) && len(r.Delegations) == 0 {
			// The obligation is that a verdict ACCOUNTS FOR ITSELF: it cites what it
			// found, names something it actually ran, or states why there was nothing
			// to run. Field non-emptiness is not that obligation, and testing it
			// failed in both directions — a one-token `tests_run: ["."]` passed while
			// an honest docs-only approval that explained itself in prose failed.
			//
			// This cannot detect FABRICATED evidence, and no deterministic check can.
			// It detects a verdict that accounts for nothing, which is the shape that
			// reached a near-merge twice.
			pass := reviewVerdictAccountsForItself(r)
			checks = append(checks, ResultCheck{
				ID:       "review-verdict-has-evidence",
				Action:   "review",
				Question: "Does the review verdict cite findings, name evidence it produced, or explain why there was none?",
				Pass:     pass,
				Explanation: explain(pass, fmt.Sprintf(
					"the review returned terminal decision %q with no findings, no substantive tests_run or changes_made entry, and a summary too short to be a rationale, so it accounts for nothing the reviewer did",
					strings.TrimSpace(r.Decision))),
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

func displayPaths(paths []string) string {
	if len(paths) == 0 {
		return "none"
	}
	return strings.Join(paths, ", ")
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

// rawJSONCarriesContent applies the content test to one JSON value taken from
// inside a container.
//
// The unquoting step is load-bearing and my own test caught its absence: a
// container's values arrive as RAW JSON, so an empty string inside one is the
// two-byte text `""`, which reads as non-empty text unless it is decoded
// first. Without this, {"name":"","command":""} still counted as evidence -
// the exact false-accept this round is closing.
func rawJSONCarriesContent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return strings.TrimSpace(text) != ""
		}
	}
	return entryCarriesContent(trimmed)
}

// actionableEntries returns only the entries that carry information.
//
// hasActionableEntries answers "is there anything here"; this answers "which of
// these is worth persisting". The gate-RECORDING path needs the second: the
// #1809 review found that `needs: [{}]` was admitted on a raw len() and written
// to job_gates verbatim, so a durable row whose need column is the two-byte
// text {} surfaced to a human through `gitmoot job gates` and the dashboard's
// "Needs a human" view. Filtering here keeps the gate that RECORDS agreeing
// with the gates that JUDGE - the principle this branch already applied to
// implement-changes-listed and implement-tests-listed, applied to the consumer
// literally named gates.
func actionableEntries(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if entryCarriesContent(value) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

// hasActionableEntries reports whether a string slice contains at least one
// entry that carries information, so an all-empty list - including one whose
// only entries are content-free containers - counts as no entries.
func hasActionableEntries(values []string) bool {
	for _, v := range values {
		if entryCarriesContent(v) {
			return true
		}
	}
	return false
}

// entryCarriesContent reports whether one list entry says anything.
//
// Trimming whitespace is no longer sufficient (#1805): object elements now
// decode into entries, so `needs: [{}]` arrives as the two-character string
// "{}" - non-empty to TrimSpace, and therefore actionable to the old check.
// That let a BLOCKED result whose ONLY blocker is an empty object pass the
// evidence heuristic, which is a GATE behaviour change rather than a cosmetic
// one. A content-free JSON container carries exactly as much information as ""
// and is treated the same.
//
// An object WITH fields still counts: the test is emptiness, not shape.
func entryCarriesContent(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	switch trimmed[0] {
	case '{', '[':
		var container []json.RawMessage
		if trimmed[0] == '{' {
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(trimmed), &object); err == nil {
				// NOT len(object) > 0: an object whose VALUES are all empty
				// carries no more information than "". A review reporting
				// {"name":"","command":""} reads as populated evidence to a
				// coordinator while saying nothing, which is worse than a hard
				// failure - the false-accept this check exists to close was
				// only half closed by counting keys.
				for _, value := range object {
					if rawJSONCarriesContent(value) {
						return true
					}
				}
				return false
			}
			// Unparseable: it is still text a human can read, so keep it.
			return true
		}
		if err := json.Unmarshal([]byte(trimmed), &container); err == nil {
			for _, value := range container {
				if rawJSONCarriesContent(value) {
					return true
				}
			}
			return false
		}
		return true
	default:
		return true
	}
}

// minReviewRationaleChars is the floor at which a review summary can carry the
// reviewer's account of why a verdict cites no other evidence. It is longer than
// the ask-answer floor on purpose: "lgtm" is a verdict with no account, while a
// real one has to say what was examined and why nothing needed running.
const minReviewRationaleChars = 40

// minEvidenceTokenChars is the floor at which a SINGLE-token evidence entry can
// still name something real — a path or a target. It exists so `tests_run: ["."]`
// is not mistaken for evidence while `internal/workflow/result_checks.go` is.
const minEvidenceTokenChars = 8

// reviewVerdictAccountsForItself reports whether a terminal review verdict
// accounts for what the reviewer did, by any of the three routes a real one
// takes: it cites findings, it names something substantive it ran or changed, or
// its summary states the rationale.
//
// This deliberately does NOT ask whether the evidence fields are non-empty. That
// question has a false answer in both directions: one arbitrary token satisfies
// it, and an honest approval of a change with nothing to run fails it.
func reviewVerdictAccountsForItself(r AgentResult) bool {
	if len(r.Findings) > 0 {
		return true
	}
	for _, entry := range r.TestsRun {
		if isSubstantiveEvidenceEntry(entry) {
			return true
		}
	}
	for _, entry := range r.ChangesMade {
		if isSubstantiveEvidenceEntry(entry) {
			return true
		}
	}
	return len(strings.TrimSpace(r.Summary)) >= minReviewRationaleChars
}

// isSubstantiveEvidenceEntry reports whether one evidence entry names something a
// reader could go and check. A phrase qualifies — a command with its outcome is
// the common shape — and so does a single token long enough to be a real path or
// target. A bare "." or "ok" does not.
func isSubstantiveEvidenceEntry(entry string) bool {
	trimmed := strings.TrimSpace(entry)
	if trimmed == "" {
		return false
	}
	if len(strings.Fields(trimmed)) > 1 {
		return true
	}
	return len(trimmed) >= minEvidenceTokenChars && strings.ContainsAny(trimmed, "/.")
}

// isTerminalReviewVerdict reports whether a review decision is one the merge
// gate and the review lifecycle treat as a settled answer about the code.
// "blocked" and "failed" are excluded on purpose: they are self-describing
// non-answers that no consumer mistakes for an approval, and they legitimately
// carry no findings or tests.
func isTerminalReviewVerdict(decision string) bool {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approved", "changes_requested":
		return true
	default:
		return false
	}
}

// explain returns the failure explanation for a failed check and "" for a passed
// one, so the ResultCheck.Explanation field is empty exactly when Pass is true.
func explain(pass bool, why string) string {
	if pass {
		return ""
	}
	return why
}
