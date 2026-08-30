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
}

// Reason is the actionable refusal shown by both dispatch paths.
func (m ReviewLoopMatch) Reason() string {
	if m.EmptyHead {
		return fmt.Sprintf("review loop detected for %s pull request #%d: requested head SHA is empty after succeeded review job %s (%s); supply the current head SHA before retrying",
			m.Repo, m.PullRequest, m.JobID, m.Decision)
	}
	return fmt.Sprintf("review loop detected for %s pull request #%d at head %s: agent %s already holds the stable %s decision at this head (succeeded review job %s); dispatch a different agent or push a new head before retrying",
		m.Repo, m.PullRequest, m.HeadSHA, m.Agent, m.Decision, m.JobID)
}

// DetectReviewLoop refuses a dispatch when any requested agent already produced
// a succeeded verdict at the exact requested head. Agent identity is the
// boundary: a different agent from the same runtime family is independent for
// merge-gate purposes and remains eligible. An empty requested head proceeds
// only before any succeeded history exists; afterward it fails closed because it
// cannot prove a new commit.
//
// On a match, the event is claimed against the matched succeeded job. No new
// review job is created, and no prior result is returned to the caller.
func DetectReviewLoop(ctx context.Context, store *db.Store, repo string, pullRequest int, headSHA string, requestingAgents []string) (ReviewLoopMatch, bool, error) {
	matches, err := FindRepeatedReviewers(ctx, store, repo, pullRequest, headSHA, requestingAgents)
	if err != nil {
		return ReviewLoopMatch{}, false, err
	}
	if len(matches) == 0 {
		return ReviewLoopMatch{}, false, nil
	}
	return matches[0], true, nil
}

// FindRepeatedReviewers returns exact-head verdict evidence for each requesting
// agent that has already reviewed the head. The result preserves requester
// order and performs one verdict query for the whole native roster.
func FindRepeatedReviewers(ctx context.Context, store *db.Store, repo string, pullRequest int, headSHA string, requestingAgents []string) ([]ReviewLoopMatch, error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	verdicts, err := store.SucceededReviewVerdicts(ctx, repo, pullRequest)
	if err != nil {
		return nil, err
	}
	if len(verdicts) == 0 || len(requestingAgents) == 0 {
		return nil, nil
	}

	requesters := compactStrings(requestingAgents)
	evidenceByAgent := make(map[string]db.SucceededReviewVerdict, len(verdicts))
	if headSHA != "" {
		for _, verdict := range verdicts {
			if verdict.HeadSHA != headSHA {
				continue
			}
			agent := strings.ToLower(strings.TrimSpace(verdict.Agent))
			if agent == "" {
				continue
			}
			if _, exists := evidenceByAgent[agent]; !exists {
				evidenceByAgent[agent] = verdict
			}
		}
		if len(evidenceByAgent) == 0 {
			return nil, nil
		}
	}

	matches := make([]ReviewLoopMatch, 0, len(requesters))
	seenRequesters := make(map[string]struct{}, len(requesters))
	for _, requester := range requesters {
		key := strings.ToLower(strings.TrimSpace(requester))
		if key == "" {
			continue
		}
		if _, seen := seenRequesters[key]; seen {
			continue
		}
		seenRequesters[key] = struct{}{}

		evidence := verdicts[0]
		if headSHA != "" {
			var ok bool
			evidence, ok = evidenceByAgent[key]
			if !ok {
				continue
			}
		}
		match := ReviewLoopMatch{
			JobID:       evidence.JobID,
			Agent:       evidence.Agent,
			Repo:        repo,
			PullRequest: pullRequest,
			HeadSHA:     headSHA,
			Decision:    evidence.Decision,
			EmptyHead:   headSHA == "",
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
			return nil, err
		}
		matches = append(matches, match)
	}
	return matches, nil
}
