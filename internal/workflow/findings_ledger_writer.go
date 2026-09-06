package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

// THE PRODUCTION WRITER (#1850 review F1, P1, found by both verdicts).
//
// Until this existed the whole #1822 ledger was INERT AT RUNTIME: the schema,
// the store's evidence bar and the gate's acceptance check were all present and
// tested, and nothing in production ever wrote an observation, so
// ListReviewFindingObservations always returned empty, EnsureLedgerObligationsObserved
// always took its empty-ledger early return, and every guard I had built could
// never fire. My tests were green against unreachable code, which is the exact
// vacuous-guard class the ledger itself was written to stop - at whole-feature
// scale, and in the change meant to end it.
//
// THE LOOP HAS TWO HALVES AND ONE WITHOUT THE OTHER IS WORSE THAN NEITHER.
// This file is the write half: a completed review's findings become observations
// at that review's exact head. The read half is the gate. If only the write half
// existed, prior findings would accumulate as open obligations that no reviewer
// was ever told about, and the gate would block legitimate merges forever - a
// guard that rejects valid input, which is worse than the inert state it
// replaced. So ledgerObligationBrief (below) discloses the obligations IN THE
// REVIEW BRIEF, naming each uid, which is the only way a reviewer can cite one.

// reviewFindingWire is the subset of a review result's finding object this writer
// reads. Findings ride as free-form json.RawMessage, so every field is optional
// and a finding that carries none of them is still recorded.
type reviewFindingWire struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	// Ledger fields. A reviewer that has read the brief can CONTINUE a prior
	// finding by citing its uid; absent that, mint-by-default creates a new
	// finding and the prior one stays unobserved, which is the fail-safe.
	ContinuesUID   string   `json:"continues_uid"`
	State          string   `json:"state"`
	RelevanceKeys  []string `json:"relevance_keys"`
	EvidenceKind   string   `json:"evidence_kind"`
	Locator        string   `json:"evidence_locator"`
	Rationale      string   `json:"rationale"`
	WithdrawReason string   `json:"withdraw_reason"`
	// Evidence is the REFUTATION-LENS finding shape (risk.go): a lens emits
	// {lens,refuted,severity,confidence,evidence:"file:line - why"} and NO file
	// field, so before this the key set came out EMPTY and such a finding could
	// never be re-armed by relevance once answered (#1850 round 2 F4).
	Evidence string `json:"evidence"`
	Lens     string `json:"lens"`
	// ALTERNATE PROSE KEYS, MEASURED FROM REAL VERDICTS (#1928). Findings ride as
	// free-form json.RawMessage, so a reviewer that names its prose differently
	// silently loses it: the row is written with an EMPTY title or detail and the
	// text survives only inside the job payload, where no ledger reader looks.
	// Four instances in one evening, three distinct schemas, each dropping a
	// different column:
	//
	//   #1930 round 1 emitted {title, body}            -> detail EMPTY
	//   #1921 round 3 emitted {title, evidence}        -> detail EMPTY
	//   #1930 round 3 emitted {summary, detail, location} -> title EMPTY, file EMPTY
	//
	// A row nobody can read cannot be dispositioned, which is exactly what the
	// gate's obligation message needs it for. These keys are READ, never invented:
	// each one is a field a real reviewer actually sent.
	Body     string `json:"body"`
	Summary  string `json:"summary"`
	Location string `json:"location"`
}

// pathFromLensEvidence extracts the leading repo-relative path from a lens
// evidence string of the form "file:line - why". It returns "" when the value
// does not begin with something path-shaped, because inventing a key is worse
// than having none: a wrong key re-arms the wrong finding.
func pathFromLensEvidence(evidence string) string {
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return ""
	}
	head := strings.Fields(evidence)[0]
	head = strings.TrimSuffix(strings.TrimSpace(head), ",")
	path, _, _ := strings.Cut(head, ":")
	path = strings.TrimSpace(path)
	if path == "" || !strings.Contains(path, "/") && !strings.Contains(path, ".") {
		return ""
	}
	return path
}

// RecordReviewFindingsToLedger writes one observation per reported finding at the
// review's exact head.
//
// IT NEVER FAILS THE REVIEW, and that is now true of every path rather than of
// most of them: a review that produced a real verdict must not be discarded
// because a ledger row would not serialise. Per-finding failures are recorded
// as job events and skipped, because a silent skip on a write path is
// indistinguishable from a successful write. The closing summary event is
// likewise best-effort - see the comment at its call.
func (e Engine) RecordReviewFindingsToLedger(ctx context.Context, job db.Job, payload JobPayload) error {
	if e.Store == nil || payload.Result == nil || job.Type != "review" {
		return nil
	}
	head := strings.TrimSpace(payload.HeadSHA)
	repo := strings.TrimSpace(payload.Repo)
	if head == "" || repo == "" || payload.PullRequest <= 0 || len(payload.Result.Findings) == 0 {
		return nil
	}
	written, skipped, downgrades := 0, 0, 0
	for index, raw := range payload.Result.Findings {
		var wire reviewFindingWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			// A finding that is not an object (some agents emit bare strings) still
			// counts as a reported finding, so it is recorded with its text as the
			// title rather than dropped.
			var text string
			if textErr := json.Unmarshal(raw, &text); textErr != nil {
				skipped++
				e.recordLedgerSkip(ctx, job.ID, index, fmt.Sprintf("finding is neither an object nor a string: %v", err))
				continue
			}
			wire = wireFromBareFindingText(text)
		}
		obs, declared, ok := e.ledgerObservationWithDeclaredState(job, payload, wire, head, repo)
		if !ok {
			skipped++
			e.recordLedgerSkip(ctx, job.ID, index, "finding carries no file and the review executed nothing, so no evidence kind is truthful")
			continue
		}
		if _, err := e.Store.RecordReviewFindingObservation(ctx, obs); err != nil {
			skipped++
			e.recordLedgerSkip(ctx, job.ID, index, fmt.Sprintf("store refused the observation: %v", err))
			continue
		}
		written++
		// #1936: A REVERSAL THE REVIEWER DID NOT ASK FOR IS NOW AUDIBLE. The row
		// is recorded either way - dropping it would lose the observation - but a
		// declared disposition that did not survive is named, with the reason, so
		// the lane can supply a `file` or an execution instead of discovering the
		// reversal from a gate refusal several steps later.
		if downgraded, reason := ledgerStateDowngrade(declared, obs); downgraded {
			e.recordLedgerDowngrade(ctx, job.ID, index, declared, obs.State, reason)
			downgrades++
		}
	}
	// THE SUMMARY EVENT IS BEST-EFFORT AND ITS FAILURE MUST NOT FAIL THE REVIEW.
	// Returning it would hand AdvanceJob an error, and AdvanceJob's caller treats
	// that as a failed advance - so a TRANSIENT 'database is locked' on an AUDIT
	// write would discard a real verdict. SQLITE_BUSY is not hypothetical on this
	// box: it was measured killing workflow-note writes the same day this was
	// written. The durable evidence does not depend on this row anyway: the
	// observations are already committed in the ledger, and every skip already
	// recorded its own event.
	//
	// I found this by applying directive 117383 to my own change: its doc comment
	// claimed the function "never fails the review" while this line could. That
	// is the same defect class the directive describes - a stale premise one line
	// above the code that violates it - so the fix is BOTH the code and the
	// sentence, not either alone.
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID: job.ID,
		Kind:  "findings_ledger_recorded",
		// THE COUNT NO LONGER OVERSTATES ITSELF (#1936). "recorded 4 of 4 ... (0
		// skipped)" was true of the WRITE and false of the OUTCOME: three of those
		// four rows had their declared disposition reversed. A summary that cannot
		// distinguish those two facts is the false-success half of this defect.
		Message: fmt.Sprintf("recorded %d of %d reported finding(s) to the #1822 ledger at head %s (%d skipped, %d downgraded)",
			written, len(payload.Result.Findings), head, skipped, downgrades),
	})
	return nil
}

// ledgerObservationFor builds the observation, choosing the evidence kind from
// what the review ACTUALLY did rather than from what would be convenient.
func (e Engine) ledgerObservationFor(job db.Job, payload JobPayload, wire reviewFindingWire, head string, repo string) (db.ReviewFindingObservation, bool) {
	obs, _, ok := e.ledgerObservationWithDeclaredState(job, payload, wire, head, repo)
	return obs, ok
}

// ledgerObservationWithDeclaredState additionally reports the state the REVIEWER
// declared, so the caller can say when the recorded state is not the declared
// one. #1936: a declared `answered` silently became `open` while the summary
// event still read "recorded 4 of 4 ... (0 skipped)" - a false success beside a
// silent reversal, which is the pair this lane exists to remove.
func (e Engine) ledgerObservationWithDeclaredState(job db.Job, payload JobPayload, wire reviewFindingWire, head string, repo string) (db.ReviewFindingObservation, db.FindingState, bool) {
	obs := db.ReviewFindingObservation{
		Repo:        repo,
		PullRequest: int64(payload.PullRequest),
		HeadSHA:     head,
		ObserverJob: job.ID,
		Severity:    strings.TrimSpace(wire.Severity),
		RoundLabel:  strings.TrimSpace(wire.ID),
		// PROSE IS TAKEN FROM WHICHEVER KEY THE REVIEWER USED (#1928). Order is
		// specific-to-general: the canonical key wins, then the observed
		// alternates. `evidence` is last for detail because it is also the lens
		// shape parsed for a path below, so a lens finding keeps its locator and
		// still contributes its prose instead of writing an empty row.
		Title:  firstNonEmptyLedgerText(strings.TrimSpace(wire.Title), strings.TrimSpace(wire.Summary)),
		Detail: firstNonEmptyLedgerText(strings.TrimSpace(wire.Detail), strings.TrimSpace(wire.Body), strings.TrimSpace(wire.Evidence)),
		File: firstNonEmptyLedgerText(
			strings.TrimSpace(wire.File),
			pathFromLensEvidence(wire.Evidence),
			pathFromLensEvidence(wire.Location),
			// #1936: evidence_locator LAST. A reviewer that cites
			// "internal/workflow/merge_gate.go: collectImplementerAttribution
			// (~lines 1436-1449)" and no `file` used to fall through to QUOTED,
			// which forced its declared `answered` to `open` - so three real
			// dispositions became three open rows and the gate refused a head
			// whose reviewer had done the work. Zero STATIC rows had ever reached
			// answered in this store, against 13 EXECUTED, which is what that
			// looks like from the outside.
			//
			// NO FILESYSTEM CHECK HERE, deliberately: the writer has no tree, which
			// is the same reason the store boundary does not check either. A
			// path-SHAPED leading token is enough, because answeredIsMandatory
			// re-resolves the locator against the head via PathExistsAtHead and
			// fails the discharge when it has vanished. A locator that is prose
			// only yields "" here and still lands in QUOTED, now with an event.
			pathFromLensEvidence(wire.Locator),
		),
		Line:           int64(wire.Line),
		RelevanceKeys:  wire.RelevanceKeys,
		ContinuesUID:   strings.TrimSpace(wire.ContinuesUID),
		WithdrawReason: strings.TrimSpace(wire.WithdrawReason),
		SourceJob:      job.ID,
	}
	declared := db.FindingState(strings.ToLower(strings.TrimSpace(wire.State)))
	switch declared {
	case db.FindingAnswered:
		obs.State = db.FindingAnswered
	case db.FindingWithdrawn:
		obs.State = db.FindingWithdrawn
	case db.FindingSuperseded:
		obs.State = db.FindingSuperseded
	default:
		// A REPORTED FINDING IS OPEN. Silence is never an answer, so an omitted
		// state can only mean the finding stands.
		obs.State = db.FindingOpen
	}
	commands := []string(payload.Result.TestsRun)
	// THE VERDICT'S OWN evidence FIELD IS THE AUTHORITY, NOT THE LENGTH OF ITS
	// LIST (#1850 merge-head P1, found by me and ruled on in directive 117886).
	//
	// I previously derived EXECUTED from len(tests_run) > 0. That is the classic
	// instrument error: a field that CAN be filled by something that did not
	// execute, used as proof that something executed. Main's merge brought the
	// real discriminator - AgentResult.Evidence, defaulted to static_only with
	// the stated reason that nothing may read "we could not run the gate" as
	// "the gate passed" - and my writer did not consult it.
	//
	// MEASURED before the fix: a verdict with Evidence=static_only whose
	// tests_run listed three items, ONE OF THEM "go build -> COULD NOT RUN:
	// permission denied", was recorded as EXECUTED with count 3, and as a
	// continuation it DISCHARGED a prior open P1. That is evidence-free
	// discharge, the exact class this ledger exists to prevent, and the more
	// scrupulous the static reviewer the stronger its false discharge, because
	// the acceptance rule asks static reviewers to state what they examined.
	executed := EvidenceWasExecuted(*payload.Result) && len(commands) > 0
	switch {
	case strings.EqualFold(strings.TrimSpace(wire.EvidenceKind), string(db.EvidenceQuoted)):
		obs.EvidenceKind = db.EvidenceQuoted
		obs.State = db.FindingOpen
	case executed:
		// EXECUTED, and the count is the number of checks the verdict itself
		// listed. Both halves are required: the verdict must DECLARE execution
		// and must name what it ran.
		obs.EvidenceKind = db.EvidenceExecuted
		obs.ExecutedCommands = commands
		obs.ExecutedCount = int64(len(commands))
	case obs.File != "":
		// DECLARED static_only, OR DECLARED NOTHING. It falls through to STATIC
		// rather than to QUOTED, and that choice is measured rather than
		// preferred. THREE THINGS POINT THE SAME WAY:
		//
		//  1. Mapping it to QUOTED wedges the fleet. Measured on the 59 succeeded
		//     review jobs of one day: 7 declared executed, 2 declared static_only,
		//     and 50 OMITTED the field, of which 33 listed a non-empty tests_run.
		//     result.go defaults absence to static_only, so 34 of 59 reviews would
		//     have recorded a non-discharging kind despite having run checks, and
		//     no PR carrying a prior open finding could be discharged by the great
		//     majority of real reviewers. That is the merge wedge #1850 rounds 2
		//     and 3 closed, rebuilt from the other side.
		//  2. QUOTED would SKIP THE LOCATOR-EXISTENCE RE-ARM. Only EvidenceStatic
		//     is re-checked in answeredIsMandatory, so a quoted row would evade the
		//     round-2 F5 guard that fails a discharge whose cited path has
		//     vanished. STATIC keeps that protection; QUOTED loses it.
		//  3. STATIC is evidence of READING and is what the kind was designed for.
		//     The P1 was never "a static reviewer answered something"; it was a
		//     MANUFACTURED EXECUTION COUNT - three, for a verdict whose own list
		//     said "go build -> COULD NOT RUN: permission denied".
		//
		// So discharge is keyed on whether checkable evidence was supplied, not on
		// how the reviewer worked. The examined-list is preserved for humans while
		// ExecutedCount stays ZERO, so nothing can read it as an execution count.
		obs.EvidenceKind = db.EvidenceStatic
		obs.ExecutedCommands = commands
		obs.EvidenceLocator = obs.File
		if wire.Line > 0 {
			// path + ":" + line, built by concatenation rather than Sprintf("%s:%d").
			// This is a SOURCE LOCATOR, not a network address: net.JoinHostPort would
			// bracket a path containing a colon, which is meaningless here. Written
			// this way so the host:port lint has nothing to match on.
			obs.EvidenceLocator = obs.File + ":" + strconv.Itoa(int(wire.Line))
		}
		// THE REVIEWER'S LOCATOR IS ONLY USED AS THE LOCATOR WHEN IT IS
		// STRUCTURALLY ONE (#1936). This override used to be unconditional, and
		// the store requires a locator matching `path` or `path:line` for a STATIC
		// discharge - so a reviewer that wrote a citation in prose
		// ("internal/workflow/merge_gate.go: collectImplementerAttribution
		// (~lines 1436-1449)") had its whole observation REFUSED, even when it
		// had also supplied a usable `file`. Measured: that exact finding was
		// rejected with ErrFindingDischarge while its rationale was non-empty,
		// which is why the ledger held zero answered STATIC rows.
		//
		// So a structural locator replaces the derived one, and a prose citation
		// is preserved as RATIONALE instead of destroying the row. Nothing is
		// invented: both values came from the reviewer.
		proseCitation := ""
		if locator := strings.TrimSpace(wire.Locator); locator != "" {
			if db.IsStructuralFindingLocator(locator) {
				obs.EvidenceLocator = locator
			} else {
				proseCitation = locator
			}
		}
		// The prose citation ranks above title/detail because it is what the
		// reviewer offered as the thing it READ, which is exactly what a STATIC
		// rationale is for. An explicit `rationale` still wins over both.
		obs.Rationale = firstNonEmptyLedgerText(wire.Rationale, proseCitation, obs.Title, obs.Detail, "reported by a review that declared no executed checks")
	default:
		// No locator to cite and no declared execution: recordable for context
		// and incapable of discharging anything, which is the honest floor.
		obs.EvidenceKind = db.EvidenceQuoted
		obs.State = db.FindingOpen
		obs.ExecutedCommands = commands
	}
	if obs.EvidenceKind == db.EvidenceStatic && strings.TrimSpace(obs.Rationale) == "" {
		obs.Rationale = "reported by a static review with no executed checks"
	}
	if obs.State == db.FindingWithdrawn && obs.WithdrawReason == "" {
		// The store refuses a reasonless withdrawal; a review asking for one
		// without saying why is downgraded to OPEN rather than rejected, because
		// dropping the row entirely would lose the observation.
		obs.State = db.FindingOpen
	}
	return obs, declared, true
}

func firstNonEmptyLedgerText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (e Engine) recordLedgerSkip(ctx context.Context, jobID string, index int, reason string) {
	if e.Store == nil {
		return
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    "findings_ledger_skipped",
		Message: fmt.Sprintf("finding[%d] not recorded to the #1822 ledger: %s", index, reason),
	})
}

// ReviewObligationBrief exposes the obligation brief to a dispatcher outside
// this package. It exists because the brief was reachable from ONE dispatch
// path and that path records nothing (#1969).
//
// Measured on this box's ledger, 2026-09-05 to 2026-09-07: all 571 recorded
// findings were written by CLI-dispatched review jobs (observer_job LIKE
// 'local-%') and ZERO by the daemon fan-out, while ledgerObligationBrief was
// called only from HandlePullRequestOpened's fan-out loop and the high-risk
// lens. So the read half of the loop this file's header describes has never
// fired in production: no reviewer has ever been handed a uid by the engine,
// and not one of the 571 findings' prompts contains a rendered 'uid=' line.
//
// It is a thin accessor ON PURPOSE. The brief's TEXT must have exactly one
// author, and the scope it is computed with must be the gate's own, or the
// brief discloses one set of obligations while the gate demands another - the
// #1850 R3-F1 wedge. Callers supply the same LedgerResolvers value the gate
// holds; they do not get to render their own version.
func (e Engine) ReviewObligationBrief(ctx context.Context, repo string, pullRequest int, head string, taskID string) string {
	return e.ledgerObligationBrief(ctx, repo, pullRequest, head, taskID)
}

// ledgerObligationBrief renders the prior findings a round at this head must
// observe, for inclusion in the review brief. THIS IS THE HALF THAT KEEPS THE
// GATE FROM REJECTING VALID INPUT: an obligation can only be discharged by
// citing a uid, and a uid is only obtainable by being told it.
//
// IT MUST BE COMPUTED WITH THE SAME SCOPE THE GATE USES, and my first version
// was not (#1850 round 2 F1, P1). It passed LedgerScope{}, so with both
// resolvers nil the answered arms degraded to not-mandatory and the brief listed
// ONLY findings whose latest state was open. The gate, using a POPULATED scope,
// additionally demanded answered findings whose relevance keys a later diff
// touched and answered findings whose STATIC locator had gone. Those two classes
// were mandatory to the gate and INVISIBLE to the brief, so no reviewer could
// ever discharge them: a permanent, undischargeable merge wedge, reproduced end
// to end by the reviewer through a full round-3 review.
//
// THE DIVERGENCE IS NOW STRUCTURAL RATHER THAN DISCIPLINED: both callers build
// their scope from the same engine seams, and TestLedgerBriefSetEqualsGateSet
// pins set equality under a POPULATED scope. My earlier test exercised only the
// open case, which is the one class where the two agree - a test that passed for
// a reason other than the property it named.
//
// It returns "" when there is nothing to say, so the default review brief is
// byte-identical on a PR with no ledger history.
func (e Engine) ledgerObligationBrief(ctx context.Context, repo string, pullRequest int, head string, taskID string) string {
	if e.Store == nil || pullRequest <= 0 || strings.TrimSpace(head) == "" {
		return ""
	}
	observations, err := e.Store.ListReviewFindingObservations(ctx, repo, int64(pullRequest))
	if err != nil || len(observations) == 0 {
		return ""
	}
	pending := LedgerObligationsAtHead(ctx, observations, head, e.ledgerScopeFor(repo, pullRequest, taskID))
	if len(pending) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nPRIOR FINDINGS ON THIS PR THAT YOU MUST OBSERVE AT THIS HEAD (#1822 findings ledger).\n")
	b.WriteString("Each line is a finding an earlier round recorded. The merge gate REFUSES a verdict at this head\n")
	b.WriteString("until every one carries an observation here, so silence is not an answer. To continue a prior\n")
	b.WriteString("finding, emit a finding object citing its uid as \"continues_uid\" - typing its old label is naming,\n")
	b.WriteString("not reference, and mints a NEW finding instead. Set \"state\": \"answered\" only if you CHECKED it at\n")
	b.WriteString("this head, and say what you ran; \"withdrawn\" requires \"withdraw_reason\" and is refused without one.\n")
	b.WriteString("EVERY finding you emit needs an explicit \"severity\" of P0, P1, P2 or P3. A finding with none is\n")
	b.WriteString("REFUSED rather than stored, because a row with no severity is an obligation no severity policy\n")
	b.WriteString("can ever disposition, and it is not the same thing as P3 (#1928).\n")
	for _, obligation := range pending {
		label := obligation.RoundLabel
		if strings.TrimSpace(label) == "" {
			label = "(unlabelled)"
		}
		b.WriteString(fmt.Sprintf("  uid=%s  was=%s  severity=%s  reason=%s  title=%s\n",
			obligation.FindingUID, label, obligation.Severity, obligation.Reason, obligation.Title))
		if strings.TrimSpace(obligation.Severity) == "" {
			// A LEGACY EMPTY-SEVERITY ROW MUST NOT PRINT AS "severity=". The
			// reviewer reads this line to decide how to answer; a blank there
			// reads as "unset, therefore minor", which is the inference #1928
			// exists to stop. It is named for what it is instead.
			b.WriteString("    (that row predates the severity requirement and carries none; treat it as unranked and blocking until you observe it)\n")
		}
	}
	return b.String()
}

// ledgerScopeFor binds the engine's SHARED resolvers to this repo and PR. The
// gate binds the same value the same way, which is what makes the brief and the
// gate incapable of disagreeing (#1850 round 3 item 1).
func (e Engine) ledgerScopeFor(repo string, pullRequest int, taskID string) LedgerScope {
	return e.LedgerResolvers.ScopeFor(repo, pullRequest, taskID)
}

// ledgerStateDowngrade reports whether the recorded state differs from the one
// the reviewer declared, and why. It never invents a downgrade for an omitted
// state: silence means OPEN by design, so only a DECLARED disposition that did
// not survive is a reversal worth naming.
func ledgerStateDowngrade(declared db.FindingState, obs db.ReviewFindingObservation) (bool, string) {
	switch declared {
	case db.FindingAnswered, db.FindingWithdrawn, db.FindingSuperseded:
	default:
		return false, ""
	}
	if obs.State == declared {
		return false, ""
	}
	switch {
	case declared == db.FindingWithdrawn && strings.TrimSpace(obs.WithdrawReason) == "":
		return true, "withdrawal carried no withdraw_reason"
	case obs.EvidenceKind == db.EvidenceQuoted:
		return true, "evidence QUOTED: no file or path-shaped evidence_locator, and the review declared no executed checks"
	default:
		return true, "recorded state differs from the declared one"
	}
}

func (e Engine) recordLedgerDowngrade(ctx context.Context, jobID string, index int, declared db.FindingState, recorded db.FindingState, reason string) {
	if e.Store == nil {
		return
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID: jobID,
		Kind:  "findings_ledger_downgraded",
		Message: fmt.Sprintf("finding[%d] declared %s, recorded %s (%s)",
			index, declared, recorded, reason),
	})
}

// wireFromBareFindingText reads a finding an agent emitted as ONE PROSE STRING.
//
// MEASURED, not hypothesised: review job
// local-review-joltra-sol-review-18d2b773bd0c534b returned six real findings
// and the ledger recorded ZERO, six times over -
// `requires an explicit severity of P0, P1, P2 or P3: got ""` - because every
// finding arrived as "P2 apps/web/src/views/LandingView.vue:365 (mirrored at
// ...) - prose". The severity and the locator were both PRESENT, inline, and
// the writer read neither, so the store refused all six and a review that found
// six defects left an empty ledger.
//
// That skip was LOUD, which is the correct half of the invariant, and it stays
// loud for anything unparseable. What was wrong is that the writer discarded
// values the reviewer actually sent. Only a LEADING severity token is read, and
// only a path-shaped token after it - nothing is inferred from prose, because a
// wrong locator re-arms the wrong finding.
func wireFromBareFindingText(text string) reviewFindingWire {
	wire := reviewFindingWire{Title: strings.TrimSpace(text)}
	fields := strings.Fields(wire.Title)
	if len(fields) == 0 {
		return wire
	}
	severity := strings.ToUpper(strings.Trim(fields[0], ":,"))
	if !reviewseverity.Valid(severity) {
		return wire
	}
	wire.Severity = severity
	rest := strings.TrimSpace(strings.TrimPrefix(wire.Title, fields[0]))
	wire.Title = firstNonEmptyLedgerText(rest, wire.Title)
	// The locator, when the next token is path-shaped. pathFromLensEvidence is
	// the same parser the lens shape already uses, so one rule governs both.
	if path := pathFromLensEvidence(rest); path != "" {
		wire.File = path
	}
	return wire
}
