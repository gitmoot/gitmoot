package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
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
	written, skipped := 0, 0
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
			wire = reviewFindingWire{Title: text}
		}
		obs, ok := e.ledgerObservationFor(job, payload, wire, head, repo)
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
		Message: fmt.Sprintf("recorded %d of %d reported finding(s) to the #1822 ledger at head %s (%d skipped)",
			written, len(payload.Result.Findings), head, skipped),
	})
	return nil
}

// ledgerObservationFor builds the observation, choosing the evidence kind from
// what the review ACTUALLY did rather than from what would be convenient.
func (e Engine) ledgerObservationFor(job db.Job, payload JobPayload, wire reviewFindingWire, head string, repo string) (db.ReviewFindingObservation, bool) {
	obs := db.ReviewFindingObservation{
		Repo:           repo,
		PullRequest:    int64(payload.PullRequest),
		HeadSHA:        head,
		ObserverJob:    job.ID,
		Severity:       strings.TrimSpace(wire.Severity),
		RoundLabel:     strings.TrimSpace(wire.ID),
		Title:          strings.TrimSpace(wire.Title),
		Detail:         strings.TrimSpace(wire.Detail),
		File:           firstNonEmptyLedgerText(strings.TrimSpace(wire.File), pathFromLensEvidence(wire.Evidence)),
		Line:           int64(wire.Line),
		RelevanceKeys:  wire.RelevanceKeys,
		ContinuesUID:   strings.TrimSpace(wire.ContinuesUID),
		WithdrawReason: strings.TrimSpace(wire.WithdrawReason),
		SourceJob:      job.ID,
	}
	switch db.FindingState(strings.ToLower(strings.TrimSpace(wire.State))) {
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
	switch {
	case strings.EqualFold(strings.TrimSpace(wire.EvidenceKind), string(db.EvidenceQuoted)):
		obs.EvidenceKind = db.EvidenceQuoted
		obs.State = db.FindingOpen
	case len(commands) > 0:
		// EXECUTED, and the count is the number of checks the verdict itself
		// listed. This is the one number that is not manufactured: it is the
		// reviewer's own tests_run, which the acceptance rule already requires.
		obs.EvidenceKind = db.EvidenceExecuted
		obs.ExecutedCommands = commands
		obs.ExecutedCount = int64(len(commands))
	case obs.File != "":
		obs.EvidenceKind = db.EvidenceStatic
		obs.EvidenceLocator = obs.File
		if wire.Line > 0 {
			obs.EvidenceLocator = fmt.Sprintf("%s:%d", obs.File, wire.Line)
		}
		if strings.TrimSpace(wire.Locator) != "" {
			obs.EvidenceLocator = strings.TrimSpace(wire.Locator)
		}
		obs.Rationale = firstNonEmptyLedgerText(wire.Rationale, obs.Title, obs.Detail, "reported by a static review with no executed checks")
	default:
		return db.ReviewFindingObservation{}, false
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
	return obs, true
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
	for _, obligation := range pending {
		label := obligation.RoundLabel
		if strings.TrimSpace(label) == "" {
			label = "(unlabelled)"
		}
		b.WriteString(fmt.Sprintf("  uid=%s  was=%s  severity=%s  reason=%s  title=%s\n",
			obligation.FindingUID, label, obligation.Severity, obligation.Reason, obligation.Title))
	}
	return b.String()
}

// ledgerScopeFor binds the engine's SHARED resolvers to this repo and PR. The
// gate binds the same value the same way, which is what makes the brief and the
// gate incapable of disagreeing (#1850 round 3 item 1).
func (e Engine) ledgerScopeFor(repo string, pullRequest int, taskID string) LedgerScope {
	return e.LedgerResolvers.ScopeFor(repo, pullRequest, taskID)
}
