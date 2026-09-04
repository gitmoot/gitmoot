package workflow

import (
	"context"
	"fmt"
	"sort"
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

// latestObservation folds the append-only observation log into the most recent
// observation per finding. Folding lives here rather than in the store, because
// a stored fold is where a "still true" column creeps back in.
func latestObservation(observations []db.ReviewFindingObservation) map[string]db.ReviewFindingObservation {
	latest := map[string]db.ReviewFindingObservation{}
	for _, obs := range observations {
		latest[obs.FindingUID] = obs
	}
	return latest
}

// observedAtHead reports which findings already carry an observation at head.
func observedAtHead(observations []db.ReviewFindingObservation, head string) map[string]bool {
	head = strings.TrimSpace(head)
	seen := map[string]bool{}
	for _, obs := range observations {
		if strings.EqualFold(strings.TrimSpace(obs.HeadSHA), head) {
			seen[obs.FindingUID] = true
		}
	}
	return seen
}

// relevanceTouched reports whether any of a finding's relevance keys intersects
// the change set. The INTERSECTION is mechanical; the KEY SET is judgement.
func relevanceTouched(keys []string, changed []string) (string, bool) {
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		for _, path := range changed {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if path == key || strings.HasPrefix(path, strings.TrimSuffix(key, "/")+"/") || strings.Contains(path, key) {
				return key, true
			}
		}
	}
	return "", false
}

// LedgerObligationsAtHead computes what a round at head must observe.
//
// MANDATORY: findings whose latest state is open, plus answered or superseded
// findings any of whose relevance keys the change set touches. A withdrawn
// finding is never mandatory. A QUOTED observation creates no obligation,
// because it establishes nothing and requiring an observation of a non-finding
// would reject valid input.
func LedgerObligationsAtHead(observations []db.ReviewFindingObservation, head string, changedPaths []string) []LedgerObligation {
	latest := latestObservation(observations)
	already := observedAtHead(observations, head)
	var out []LedgerObligation
	for uid, obs := range latest {
		if already[uid] {
			continue
		}
		if obs.EvidenceKind == db.EvidenceQuoted {
			continue
		}
		var reason string
		switch obs.State {
		case db.FindingOpen:
			reason = "still open"
		case db.FindingAnswered, db.FindingSuperseded:
			key, touched := relevanceTouched(obs.RelevanceKeys, changedPaths)
			if !touched {
				continue
			}
			reason = fmt.Sprintf("answered at %s and the diff touches relevance key %q", shortHead(obs.HeadSHA), key)
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

// EnsureLedgerObligationsObserved is the production acceptance check. It returns
// an error naming every unobserved finding UID, never a boolean, so a refusal is
// actionable rather than a bare no.
//
// A nil store or an empty ledger accepts: a guard that refuses when it has
// nothing to say is a guard that rejects valid input.
func EnsureLedgerObligationsObserved(ctx context.Context, store *db.Store, repo string, pullRequest int64, headSHA string, changedPaths []string) error {
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
	pending := LedgerObligationsAtHead(observations, headSHA, changedPaths)
	if len(pending) == 0 {
		return nil
	}
	var named []string
	for _, obligation := range pending {
		label := obligation.RoundLabel
		if strings.TrimSpace(label) == "" {
			label = "(unlabelled)"
		}
		named = append(named, fmt.Sprintf("%s [%s %s: %s]", obligation.FindingUID, label, obligation.Severity, obligation.Reason))
	}
	return fmt.Errorf("findings ledger: %d prior finding(s) carry no observation at head %s: %s",
		len(pending), shortHead(headSHA), strings.Join(named, "; "))
}

func shortHead(head string) string {
	head = strings.TrimSpace(head)
	if len(head) > 12 {
		return head[:12]
	}
	return head
}
