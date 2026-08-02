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
	return fmt.Sprintf("review loop detected for %s pull request #%d at head %s: succeeded review job %s already recorded the stable %s decision; push a new commit or escalate to an operator before retrying",
		m.Repo, m.PullRequest, m.HeadSHA, m.JobID, m.Decision)
}

// DetectReviewLoop checks the safe, evidence-only half of review-loop
// prevention. A non-empty head is refused only when every succeeded verdict at
// that exact head agrees. Mixed decisions proceed because the earlier claim is
// unstable. An empty requested head proceeds only before any succeeded history
// exists; afterward it fails closed because it cannot prove a new commit.
//
// On a match, the event is claimed against the matched succeeded job. No new
// review job is created, and no prior result is returned to the caller.
func DetectReviewLoop(ctx context.Context, store *db.Store, repo string, pullRequest int, headSHA string) (ReviewLoopMatch, bool, error) {
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
