package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// ReviewLoopDetectedEventKind marks a refused review dispatch whose verdict is
// already stable at the requested PR head.
const ReviewLoopDetectedEventKind = "review_loop_detected"

// ReviewLoopMatch identifies the succeeded review used only as escalation
// evidence. It is intentionally not an AgentResult and cannot be served as a
// cached verdict.
type ReviewLoopMatch struct {
	JobID       string
	Agent       string
	Repo        string
	PullRequest int
	HeadSHA     string
	Decision    string
	EmptyHead   bool
	// Family is the runtime family, already represented at this head, that the
	// requesting reviewer belongs to (#1528). Empty when the refusal is the
	// fail-closed kind: a family involved at this head could not be determined.
	Family string
}

// Reason is the actionable refusal shown by both dispatch paths.
func (m ReviewLoopMatch) Reason() string {
	if m.EmptyHead {
		return fmt.Sprintf("review loop detected for %s pull request #%d: requested head SHA is empty after succeeded review job %s (%s); supply the current head SHA before retrying",
			m.Repo, m.PullRequest, m.JobID, m.Decision)
	}
	if m.Family != "" {
		return fmt.Sprintf("review loop detected for %s pull request #%d at head %s: runtime family %q already holds the stable %s decision at this head (succeeded review job %s by %s); dispatch the review from an unrepresented runtime family or escalate to an operator before retrying",
			m.Repo, m.PullRequest, m.HeadSHA, m.Family, m.Decision, m.JobID, m.Agent)
	}
	return fmt.Sprintf("review loop detected for %s pull request #%d at head %s: every succeeded verdict at this head recorded the stable %s decision (succeeded review job %s) and a runtime family involved at this head could not be determined; refusing fail-closed — dispatch a reviewer with a registered or recorded runtime, or escalate to an operator before retrying",
		m.Repo, m.PullRequest, m.HeadSHA, m.Decision, m.JobID)
}

// DetectReviewLoop checks the safe, evidence-only half of review-loop
// prevention. A non-empty head is refused only when every succeeded verdict at
// that exact head agrees AND no requesting agent can PROVE it would bring a
// new runtime family to the head (#1528): a review from a family with no
// verdict at this head is new information by construction, and the guard
// exists to stop repetition, not information. Mixed decisions proceed because
// the earlier claim is unstable. An empty requested head proceeds only before
// any succeeded history exists; afterward it fails closed because it cannot
// prove a new commit — the family narrowing never applies to it.
//
// Unknown families FAIL CLOSED: if a requesting agent's family or a matched
// verdict's family cannot be determined (agent absent from the registry with
// no recorded runtime), the refusal stands. Unknown must never read as "not
// yet represented", or this guard becomes a way to add unlimited reviews.
//
// On a match, the event is claimed against the matched succeeded job. No new
// review job is created, and no prior result is returned to the caller.
func DetectReviewLoop(ctx context.Context, store *db.Store, repo string, pullRequest int, headSHA string, requestingAgents ...string) (ReviewLoopMatch, bool, error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	verdicts, err := store.SucceededReviewVerdicts(ctx, repo, pullRequest)
	if err != nil {
		return ReviewLoopMatch{}, false, err
	}
	if len(verdicts) == 0 {
		return ReviewLoopMatch{}, false, nil
	}

	var matched []db.SucceededReviewVerdict
	emptyHead := headSHA == ""
	family := ""
	if emptyHead {
		matched = verdicts[:1]
	} else {
		for _, verdict := range verdicts {
			if verdict.HeadSHA == headSHA {
				matched = append(matched, verdict)
			}
		}
		if len(matched) == 0 {
			return ReviewLoopMatch{}, false, nil
		}
		decision := matched[0].Decision
		for _, verdict := range matched[1:] {
			if verdict.Decision != decision {
				return ReviewLoopMatch{}, false, nil
			}
		}
		represented, representedFamily, err := requestingFamilyRepresented(ctx, store, matched, requestingAgents)
		if err != nil {
			return ReviewLoopMatch{}, false, err
		}
		if !represented {
			return ReviewLoopMatch{}, false, nil
		}
		family = representedFamily
	}

	evidence := matched[0]
	match := ReviewLoopMatch{
		JobID:       evidence.JobID,
		Agent:       evidence.Agent,
		Repo:        repo,
		PullRequest: pullRequest,
		HeadSHA:     headSHA,
		Decision:    evidence.Decision,
		EmptyHead:   emptyHead,
		Family:      family,
	}
	eventHead := headSHA
	if eventHead == "" {
		eventHead = "<empty>"
	}
	message := fmt.Sprintf("repo=%s pull_request=%d head_sha=%s decision=%s matched_job=%s: %s",
		repo, pullRequest, eventHead, evidence.Decision, evidence.JobID, match.Reason())
	if _, err := store.ClaimJobEvent(ctx, db.JobEvent{
		JobID:   evidence.JobID,
		Kind:    ReviewLoopDetectedEventKind,
		Message: message,
	}); err != nil {
		return ReviewLoopMatch{}, false, err
	}
	return match, true, nil
}

// requestingFamilyRepresented reports whether the unanimous matched verdicts
// already cover every requesting agent's runtime family. A requester PROVABLY
// bringing a new family — its own family resolved, every matched verdict's
// family resolved, and no overlap — returns represented=false (allow). Every
// unknown family keeps represented=true (fail closed). The returned family
// names a represented requesting family for the refusal message when one could
// be resolved; it stays empty for the fail-closed unknown case.
func requestingFamilyRepresented(ctx context.Context, store *db.Store, matched []db.SucceededReviewVerdict, requestingAgents []string) (bool, string, error) {
	families := make([]string, len(matched))
	known := make([]bool, len(matched))
	for i, verdict := range matched {
		family, ok, err := ResolveRuntimeFamily(ctx, store, verdict.Agent, verdict.EffectiveRuntime)
		if err != nil {
			return false, "", err
		}
		families[i], known[i] = family, ok
	}
	family := ""
	for _, requester := range requestingAgents {
		requesterFamily, ok, err := ResolveRuntimeFamily(ctx, store, requester, "")
		if err != nil {
			return false, "", err
		}
		if !ok {
			continue
		}
		provablyNew := true
		for i := range matched {
			if !known[i] {
				provablyNew = false
				continue
			}
			if families[i] == requesterFamily {
				provablyNew = false
				if family == "" {
					family = requesterFamily
				}
			}
		}
		if provablyNew {
			return false, "", nil
		}
	}
	return true, family, nil
}
