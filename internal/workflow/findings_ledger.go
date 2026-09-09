package workflow

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// The #1822 findings-ledger acceptance invariant.
//
// A VERDICT AT HEAD H IS ACCEPTABLE ONLY IF EVERY MANDATORY LEDGER FINDING FOR
// THAT PR CARRIES AN OBSERVATION AT H. The direction matters: a ledger hit ADDS
// an obligation rather than removing one, so a ledger with six prior findings
// makes the round more expensive, not cheaper. That is what stops the ledger
// becoming a reviewer's only input, which is the delta-sign-off failure this
// design exists to avoid.
//
// WHAT THE LEDGER SAVES is re-DERIVATION: a prior finding's existence, its
// mechanism, and how it was answered. WHAT IT NEVER SAVES is re-CHECKING.
//
// AND IT IS NOT A SAFETY NET. Relevance keys are reviewer-supplied, so an
// incomplete key set can leave an answered finding advisory when a later change
// re-breaks it. THE LEDGER'S FAILURE MODE IS "IT DID NOT REMIND YOU", NEVER "IT
// TOLD YOU THE DEFECT WAS FIXED": no state means "still true", every answer
// records the head it was observed at, and the full exact-head review of the
// diff remains the default and is unchanged. A reader who mistakes this ledger
// for a guarantee is the real hazard (#1822 review 116564 condition 1).

// LedgerObligation is one finding a round at head H must observe.
type LedgerObligation struct {
	FindingUID string
	RoundLabel string
	Severity   string
	Title      string
	// Reason is why this finding is mandatory: either it is still open, or it was
	// answered earlier and the current diff touches one of its relevance keys.
	Reason string
}

// LedgerResolvers is the SINGLE source of the two facts the acceptance check
// cannot derive from the ledger alone: what a range CHANGED, and whether a cited
// locator EXISTS at a head.
//
// IT IS ONE VALUE HELD BY BOTH CONSUMERS, IN CODE, BECAUSE A COMMENT SAYING SO
// FAILED TWICE. The merge gate refuses a verdict naming an unobserved finding's
// uid; the review brief is the ONLY way a reviewer can learn that uid. If the
// two compute different obligation sets the merge wedges permanently. Round 2
// wedged because the brief passed an empty scope. Round 3 wedged again through a
// narrower door: an engine field documented as "the SAME resolver the merge gate
// uses" that nothing ever assigned, while the gate's copy was wired. Three
// separate comments asserted the equality while two independent structs were
// built from different inputs.
//
// So the equality is now structural: the daemon builds ONE LedgerResolvers and
// hands the same value to both, and neither consumer can be configured alone.
type LedgerResolvers struct {
	// ChangedSince enumerates paths changed between two heads. In production this
	// is the daemon's own checkout enumeration, which proves the range complete
	// and fails CLOSED on a capped API page.
	ChangedSince func(ctx context.Context, repo string, pullRequest int, previousHead string, currentHead string) ([]string, error)
	// PathExistsAtHead reports whether a repo-relative path exists at a head, so a
	// STATIC discharge citing a deleted file stops counting as an answer.
	PathExistsAtHead func(ctx context.Context, head string, path string) (bool, error)
}

// ScopeFor binds a repo, PR and task to the resolvers. This is the ONLY way a
// LedgerScope is constructed for production use, which is what makes the brief
// and the gate incapable of disagreeing.
func (r LedgerResolvers) ScopeFor(repo string, pullRequest int, taskID string) LedgerScope {
	scope := LedgerScope{TaskID: taskID, PathExistsAtHead: r.PathExistsAtHead}
	if r.ChangedSince != nil {
		scope.ChangedSince = func(ctx context.Context, previousHead string, currentHead string) ([]string, error) {
			return r.ChangedSince(ctx, repo, pullRequest, previousHead, currentHead)
		}
	}
	return scope
}

// LedgerScope is the bound form the acceptance check consumes. Both resolvers
// are optional and both DEGRADE rather than reject, because the ledger's
// declared failure mode is "it did not remind you" and never "it told you the
// defect was fixed" - converting an instrument outage into a merge block would
// invert that.
type LedgerScope struct {
	// ChangedSince enumerates paths changed between two heads for THIS pr.
	ChangedSince func(ctx context.Context, previousHead string, currentHead string) ([]string, error)
	// PathExistsAtHead reports whether a repo-relative path exists at a head.
	PathExistsAtHead func(ctx context.Context, head string, path string) (bool, error)
	// FindingsAdvisory carries the repository's #1969 declaration. When true the
	// acceptance check RECORDS the obligations it is letting through and returns
	// nil instead of refusing. It lives on the scope, and the recording happens
	// here rather than in the gate, because the gate's store surface is a
	// deliberate firewall (TestMergeGateStoreAccessSurface): the gate passes
	// DATA, and the ledger, which already holds the store, does the writing.
	FindingsAdvisory bool
	// Repo names the repository for the advisory record, so the note says which
	// declaration permitted the pass rather than leaving a reader to infer it.
	Repo string
	// TaskID lets the acceptance check record a degradation as a task event
	// WITHOUT the merge gate itself gaining a store write. The gate's *db.Store
	// surface is a deliberate firewall pinned by TestMergeGateStoreAccessSurface,
	// so the gate passes DATA and the ledger, which already holds the store, does
	// the writing.
	TaskID string
	// Degraded overrides the default task-event recording, for tests.
	Degraded func(note string)
}

func (s LedgerScope) degrade(format string, args ...any) {
	if s.Degraded != nil {
		s.Degraded(fmt.Sprintf(format, args...))
	}
}

// latestObservation folds the append-only observation log into the most recent
// observation per finding. Folding lives here rather than in the store, because
// a stored fold is where a "still true" column creeps back in. Input order is
// the store's INSERTION order (rowid), which no caller can set.
//
// A QUOTED ROW NEVER BECOMES THE FOLDED STATE OF A FINDING THAT HAS A REAL ONE
// (#1850 review F3, P1, confirmed independently by both verdicts). QUOTED
// establishes nothing, so letting it win the fold let one evidence-free row
// overwrite a real finding's state and silence it at every later head - the
// exact inversion of the rule the store's ErrFindingQuotedDischarge enforces on
// the write path. A QUOTED row is still recorded and is still the fold for a
// finding that has no other observation, which keeps a quoted-only mention
// exactly as weak as it should be.
// LatestObservationsInOrder folds an append-only observation list to the latest
// row per finding UID, preserving first-appearance order.
//
// EXPORTED SO THERE IS ONE IMPLEMENTATION (#2086 review, P2). internal/cli had a
// copy for the findings listing, and two copies of a fold rule is the drift
// shape this campaign keeps finding: a listing that folded differently from the
// gate would disagree with it about which obligations are open. A test that they
// stay in sync would only detect that drift; sharing the function prevents it.
func LatestObservationsInOrder(observations []db.ReviewFindingObservation) []db.ReviewFindingObservation {
	latest := latestObservation(observations)
	seen := make(map[string]bool, len(latest))
	folded := make([]db.ReviewFindingObservation, 0, len(latest))
	for _, obs := range observations {
		if seen[obs.FindingUID] {
			continue
		}
		seen[obs.FindingUID] = true
		folded = append(folded, latest[obs.FindingUID])
	}
	return folded
}

func latestObservation(observations []db.ReviewFindingObservation) map[string]db.ReviewFindingObservation {
	latest := map[string]db.ReviewFindingObservation{}
	for _, obs := range observations {
		previous, seen := latest[obs.FindingUID]
		if seen && obs.EvidenceKind == db.EvidenceQuoted && previous.EvidenceKind != db.EvidenceQuoted {
			continue
		}
		latest[obs.FindingUID] = obs
	}
	return latest
}

// dischargedAtHead reports which findings carry a DISCHARGING observation at
// head. Mere existence of a row is not enough: an obligation is cleared only by
// EXECUTED or STATIC evidence, because QUOTED discharges nothing (#1850 F3).
// This is the read-path half of the store's evidence bar, and without it a round
// facing 27 obligations could clear them all with 27 quoted rows.
func dischargedAtHead(observations []db.ReviewFindingObservation, head string) map[string]bool {
	head = strings.TrimSpace(head)
	seen := map[string]bool{}
	for _, obs := range observations {
		if !strings.EqualFold(strings.TrimSpace(obs.HeadSHA), head) {
			continue
		}
		if obs.EvidenceKind == db.EvidenceQuoted {
			continue
		}
		// #1941 f4, P1. STATIC ROWS DO NOT SHORT-CIRCUIT HERE. This function ran
		// BEFORE answeredIsMandatory and skipped any same-head non-QUOTED row, so
		// a discharge recorded at the head being judged was accepted without its
		// locator ever being resolved: a reviewer probe continued a mandatory P1
		// citing does/not/exist.go, AdvanceJob persisted it STATIC/answered,
		// EnsureLedgerObligationsObserved accepted it, and PathExistsAtHead ran
		// ZERO times.
		//
		// STATIC is the ONLY kind whose locator existence is re-checked, so a
		// STATIC row must reach answeredIsMandatory and be judged there - at the
		// same head or a later one. EXECUTED rows keep the short-circuit: they
		// carry executed commands and have no locator to resolve.
		//
		// The fix is the ORDER, not another writer-side compensation: the writer
		// cannot know whether a path exists (it has no tree) and every attempt to
		// compensate for that in the writer produced a new invention.
		if obs.EvidenceKind == db.EvidenceStatic {
			continue
		}
		seen[obs.FindingUID] = true
	}
	return seen
}

// relevanceTouched reports whether any of a finding's relevance keys intersects
// the change set. The INTERSECTION is mechanical; the KEY SET is judgement.
//
// MATCHING IS ANCHORED: exact path, or directory prefix. The unanchored
// strings.Contains arm is gone (#1850 review F7, both verdicts): it made the
// documented prefix semantics dead code and over-matched, so key "db" fired on
// "docs/adb/unrelated.md". Keys are constrained to path-like values at the store
// boundary, so a key that cannot match is now refused on write instead of
// stored and silently inert.
func relevanceTouched(keys []string, changed []string) (string, bool) {
	for _, key := range keys {
		key = strings.TrimSuffix(strings.TrimSpace(key), "/")
		if key == "" {
			continue
		}
		for _, path := range changed {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if path == key || strings.HasPrefix(path, key+"/") {
				return key, true
			}
		}
	}
	return "", false
}

// LedgerObligationsAtHead computes what a round at head must observe.
//
// MANDATORY: findings whose latest state is open, plus answered or superseded
// findings any of whose relevance keys the diff SINCE THEIR OWN ANSWERED HEAD
// touches, plus answered findings whose STATIC evidence cites a locator that no
// longer exists at this head. A withdrawn finding is never mandatory, because a
// reviewer has said it was never a defect and a later diff cannot re-break
// something that was never broken - the store now requires a recorded reason for
// that, which is the guard that stops withdrawal being the cheap exit.
//
// RELEVANCE IS EVALUATED PER FINDING, over the range from the head where the
// finding was answered to this head, rather than against one whole-PR file list
// (#1850 review F4). What matters is what changed SINCE the answer, and the
// engine already has a seam that computes exactly that and proves it complete.
func LedgerObligationsAtHead(ctx context.Context, observations []db.ReviewFindingObservation, head string, scope LedgerScope) []LedgerObligation {
	latest := latestObservation(observations)
	already := dischargedAtHead(observations, head)
	var out []LedgerObligation
	for uid, obs := range latest {
		if already[uid] {
			continue
		}
		var reason string
		switch obs.State {
		case db.FindingOpen:
			reason = "still open"
		case db.FindingAnswered, db.FindingSuperseded:
			key, touched, why := scope.answeredIsMandatory(ctx, obs, head)
			if !touched {
				continue
			}
			if key != "" {
				reason = fmt.Sprintf("answered at %s and the diff since then touches relevance key %q", shortHead(obs.HeadSHA), key)
			} else {
				reason = why
			}
		case db.FindingWithdrawn:
			continue
		default:
			continue
		}
		out = append(out, LedgerObligation{
			FindingUID: uid,
			RoundLabel: obs.RoundLabel,
			Severity:   obs.Severity,
			Title:      obs.Title,
			Reason:     reason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FindingUID < out[j].FindingUID })
	return out
}

// answeredIsMandatory decides whether an answered finding is mandatory again.
// It returns the matching relevance key when relevance is what re-armed it, and
// otherwise a reason string, so the caller can name WHY in its refusal.
func (s LedgerScope) answeredIsMandatory(ctx context.Context, obs db.ReviewFindingObservation, head string) (string, bool, string) {
	// LOCATOR EXISTENCE, the half that was missing entirely (#1850 review F5).
	// The structural check lives at the store boundary, which has no tree; the
	// EXISTENCE check needs the head, so it lives here. A STATIC discharge citing
	// a path that no longer exists at this head is not an answer any more.
	if obs.EvidenceKind == db.EvidenceStatic {
		locator := strings.TrimSpace(obs.EvidenceLocator)
		if path, _, ok := splitLocator(locator); ok {
			switch {
			case s.PathExistsAtHead == nil:
				// #1850 round 3 item 3. This arm used to skip SILENTLY while the
				// ChangedSince-nil arm degraded, and the engine field's own comment
				// claimed nil "records a degradation". An inert half with no
				// diagnostic is invisible in production, which is exactly how this
				// class survived two review rounds.
				s.degrade("findings ledger: no locator resolver, so the STATIC answer citing %q is unverified at %s", locator, shortHead(head))
			default:
				exists, err := s.PathExistsAtHead(ctx, head, path)
				switch {
				case err != nil:
					s.degrade("findings ledger: locator existence for %q at %s could not be resolved: %v", locator, shortHead(head), err)
				case !exists:
					return "", true, fmt.Sprintf("answered at %s by STATIC evidence citing %q, which does not exist at this head", shortHead(obs.HeadSHA), locator)
				}
			}
		}
	}
	if s.ChangedSince == nil {
		// DEGRADED, AND SAID SO. Without a diff the relevance half cannot be
		// evaluated, so answered findings stay advisory. This is the honest
		// version of the comment the first verdict correctly called a false
		// promise: the limitation is reported, not deferred to a caller that
		// does not exist.
		s.degrade("findings ledger: no changed-file resolver, so answered findings are advisory at %s", shortHead(head))
		return "", false, ""
	}
	changed, err := s.ChangedSince(ctx, obs.HeadSHA, head)
	if err != nil {
		s.degrade("findings ledger: changed files between %s and %s unavailable: %v", shortHead(obs.HeadSHA), shortHead(head), err)
		return "", false, ""
	}
	key, touched := relevanceTouched(obs.RelevanceKeys, changed)
	return key, touched, ""
}

// splitLocator separates "path" or "path:line" into its parts.
func splitLocator(locator string) (string, int, bool) {
	locator = strings.TrimSpace(locator)
	if locator == "" {
		return "", 0, false
	}
	path, lineText, found := strings.Cut(locator, ":")
	if !found {
		return locator, 0, true
	}
	line, err := strconv.Atoi(lineText)
	if err != nil {
		return locator, 0, true
	}
	return path, line, true
}

// EnsureLedgerObligationsObserved is the production acceptance check. It returns
// an error naming every unobserved finding UID, never a boolean, so a refusal is
// actionable rather than a bare no.
//
// A nil store or an empty ledger accepts: a guard that refuses when it has
// nothing to say is a guard that rejects valid input.
func EnsureLedgerObligationsObserved(ctx context.Context, store *db.Store, repo string, pullRequest int64, headSHA string, scope LedgerScope) error {
	if store == nil {
		return nil
	}
	observations, err := store.ListReviewFindingObservations(ctx, repo, pullRequest)
	if err != nil {
		return fmt.Errorf("list findings ledger for %s#%d: %w", repo, pullRequest, err)
	}
	if len(observations) == 0 {
		return nil
	}
	// DEFAULT DEGRADATION SINK. A degradation that is not recorded is
	// indistinguishable from a clean evaluation, which is the failure this whole
	// review round was about.
	if scope.Degraded == nil && strings.TrimSpace(scope.TaskID) != "" {
		taskID := strings.TrimSpace(scope.TaskID)
		scope.Degraded = func(note string) {
			_ = store.AddTaskEvent(ctx, db.TaskEvent{
				TaskID: taskID,
				Kind:   "findings_ledger_scope_degraded",
				Reason: note,
			})
		}
	}
	pending := LedgerObligationsAtHead(ctx, observations, headSHA, scope)
	if len(pending) == 0 {
		return nil
	}
	// SEVERITY DECIDES WHETHER AN OBLIGATION HOLDS THE MERGE, NOT WHETHER IT
	// EXISTS (owner decision, workflow note 129657). P1 and P2 hold. P3 is
	// REPORTED and does not hold.
	//
	// The split is here, at the blocking boundary, and NOT in
	// LedgerObligationsAtHead - so `gitmoot findings` and every other reader
	// still sees the whole obligation set. Filtering the predicate would have
	// deleted P3 from the ledger's answer to "what is outstanding", which is a
	// different and worse change than the one that was ruled.
	var blocking, reported []LedgerObligation
	for _, obligation := range pending {
		if obligationSeverityBlocks(obligation.Severity) {
			blocking = append(blocking, obligation)
			continue
		}
		reported = append(reported, obligation)
	}

	// STATED, NOT SILENT - the same rule the advisory path below already keeps.
	// A P3 that stops blocking must not also stop being visible, or the next
	// reviewer learns that filing one costs a round and buys nothing, and stops
	// filing. Two real P3s tonight - an orphaned comment and an unpinned map
	// ordering - were found that way.
	if len(reported) > 0 {
		if taskID := strings.TrimSpace(scope.TaskID); taskID != "" {
			_ = store.AddTaskEvent(ctx, db.TaskEvent{
				TaskID: taskID,
				Kind:   "findings_ledger_reported_not_blocking",
				Reason: fmt.Sprintf("%d finding(s) at head %s do not hold the merge under note 129657: %s",
					len(reported), shortHead(headSHA), namedObligations(reported)),
			})
		}
		scope.degrade("findings ledger: %d non-blocking finding(s) outstanding at %s: %s",
			len(reported), shortHead(headSHA), namedObligations(reported))
	}

	if len(blocking) == 0 {
		return nil
	}
	refusal := fmt.Errorf("findings ledger: %d prior finding(s) carry no observation at head %s: %s",
		len(blocking), shortHead(headSHA), namedObligations(blocking))
	if !scope.FindingsAdvisory {
		return refusal
	}
	// ADVISORY MEANS STATED, NOT SILENT (#1969). The declaration permits the
	// merge; it does not permit losing the record. The issue this answers is 211
	// findings recorded across five repositories that never answered one, with
	// nothing surfacing it, so a declaration that quietly dropped the same
	// obligations would reproduce that defect with an operator's signature on it.
	//
	// Best-effort: an audit write must never turn a permitted merge into a
	// failure. The obligations stay in the ledger regardless, and
	// `gitmoot findings` reports them whatever the declaration says.
	if taskID := strings.TrimSpace(scope.TaskID); taskID != "" {
		_ = store.AddTaskEvent(ctx, db.TaskEvent{
			TaskID: taskID,
			Kind:   "findings_ledger_advisory_merge",
			Reason: fmt.Sprintf("%s declares findings_consumption = %q, so this head proceeded past obligations that would otherwise have held it: %v",
				strings.TrimSpace(scope.Repo), "advisory", refusal),
		})
	}
	return nil
}

func shortHead(head string) string {
	head = strings.TrimSpace(head)
	if len(head) > 12 {
		return head[:12]
	}
	return head
}

// obligationSeverityBlocks decides whether an outstanding obligation HOLDS the
// merge. Owner decision, workflow note 129657: P1 and P2 hold, P3 does not.
//
// DEFAULT-DENY, AND THE STORE IS WHY RATHER THAN THE PRINCIPLE. 44 of the 1,015
// recorded observations carry an EMPTY severity. If "not P1 and not P2" meant
// "does not block", every one of those would become a free pass, and so would
// any future typo or new label - a merge bypass created by a spelling. Only a
// severity that is RECOGNISED and RANKED BELOW P2 stops holding the merge;
// everything else, including the unrecognised and the blank, still blocks.
//
// ONLY "P3" IS EXEMPT, AND P4/P5 ARE DELIBERATELY NOT. An earlier draft exempted
// them too; neither exists in the store (the vocabulary is P1 318, P2 414,
// P3 239, empty 44) and the ruling named P3. Exempting a label nobody uses is a
// bypass waiting for someone to start using it.
//
// AND THE BLANKS ARE LEFT BLOCKING ON PURPOSE. Reading "P1 and P2 only" as
// "nothing else blocks" would silently RELAX 44 existing rows, which is a wider
// change than the one ruled and in the direction that loses obligations. The
// ruling's subject was P3, so P3 is what changed.
func obligationSeverityBlocks(severity string) bool {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "P3":
		return false
	default:
		return true
	}
}

// namedObligations renders obligations for an operator-facing line.
func namedObligations(obligations []LedgerObligation) string {
	var named []string
	for _, obligation := range obligations {
		label := obligation.RoundLabel
		if strings.TrimSpace(label) == "" {
			label = "(unlabelled)"
		}
		named = append(named, fmt.Sprintf("%s [%s %s: %s]", obligation.FindingUID, label, obligation.Severity, obligation.Reason))
	}
	return strings.Join(named, "; ")
}
