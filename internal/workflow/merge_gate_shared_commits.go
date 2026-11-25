package workflow

import (
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// SharedCommitReviewGate names the merge-gate check that holds a pull request
// carrying a commit another review rejected (#2755).
const SharedCommitReviewGate = "shared-commit review gate"

// sharedChangesRequested is one changes_requested verdict, still unanswered,
// whose reviewed head is a commit the merging pull request carries.
type sharedChangesRequested struct {
	pullRequest int
	jobID       string
	commit      string
}

// unresolvedSharedChangesRequested returns the changes_requested verdicts that
// hold a merge of repo#pullRequest at headSHA, whose commit list is commits
// (#2755). jobs are review rows at those commits (Store.ListReviewJobsAtHeads).
//
// A verdict holds when it is a succeeded, non-delegated, pre-merge review of
// repo whose effective decision is changes_requested and whose head is one of
// the carried commits, on ANY pull request: code a reviewer rejected must not
// reach the base through another pull request whose reviewer never saw that
// verdict. It is answered by a LATER succeeded pre-merge approval at that exact
// commit. Across pull requests only a whole-head approval answers it: a split
// review's part slug names a part of ONE pull request's review, so a same-part
// approval answers an objection only on the same pull request.
//
// Post-merge reviews (#2265 level 2) neither hold nor answer: their findings
// are follow-up work on an already merged head, never a merge verdict, and a
// stacked pull request can still list that head after a squash merge.
//
// The merging pull request's own verdicts take two narrower rules:
//   - at its current head they are skipped: the exact-head review gate owns
//     them, with per-reviewer supersession this check must not second-guess.
//   - at an earlier head of its own they are also answered by a later approval
//     of this pull request at its current head. That is the ordinary fix
//     round: the head approved after the objection contains the objected
//     commit plus its fixes, reviewed as one diff. An objection from another
//     pull request is never answered that way, because the approving reviewer
//     there never saw it (the #2755 incident: #841 was approved at its own
//     head while #840's verdict on a commit #841 carried stood unanswered).
func unresolvedSharedChangesRequested(jobs []db.Job, repo string, pullRequest int, headSHA string, commits []string, blockingSeverity string) ([]sharedChangesRequested, error) {
	carried := make(map[string]struct{}, len(commits))
	for _, commit := range commits {
		if commit = normalizedSHA(commit); commit != "" {
			carried[commit] = struct{}{}
		}
	}
	headSHA = normalizedSHA(headSHA)
	type verdict struct {
		job     db.Job
		payload JobPayload
		head    string
	}
	var objections, approvals []verdict
	for _, job := range jobs {
		if job.Type != "review" || JobState(job.State) != JobSucceeded || isDelegationChild(job) {
			continue
		}
		payload, err := DecodeJobPayload(job)
		if err != nil {
			return nil, fmt.Errorf("decode review job %s: %w", job.ID, err)
		}
		head := normalizedSHA(payload.HeadSHA)
		if payload.Result == nil || payload.PostMergeReview || !strings.EqualFold(strings.TrimSpace(payload.Repo), strings.TrimSpace(repo)) {
			continue
		}
		if _, ok := carried[head]; !ok {
			continue
		}
		switch effectiveReviewDecision(payload.Result, blockingSeverity) {
		case "changes_requested":
			objections = append(objections, verdict{job: job, payload: payload, head: head})
		case "approved":
			approvals = append(approvals, verdict{job: job, payload: payload, head: head})
		}
	}
	answers := func(approval, objection verdict) bool {
		if part := reviewPartOf(approval.payload); part != "" &&
			(part != reviewPartOf(objection.payload) || approval.payload.PullRequest != objection.payload.PullRequest) {
			return false
		}
		return reviewJobRecordedAfter(approval.job, objection.job)
	}
	var held []sharedChangesRequested
	for _, objection := range objections {
		own := objection.payload.PullRequest == pullRequest
		if own && objection.head == headSHA {
			continue
		}
		answered := false
		for _, approval := range approvals {
			if !answers(approval, objection) {
				continue
			}
			if approval.head == objection.head ||
				(own && approval.payload.PullRequest == pullRequest && approval.head == headSHA) {
				answered = true
				break
			}
		}
		if !answered {
			held = append(held, sharedChangesRequested{
				pullRequest: objection.payload.PullRequest,
				jobID:       objection.job.ID,
				commit:      objection.head,
			})
		}
	}
	return held, nil
}

// sharedChangesRequestedCause is the operator-facing cause naming every held
// verdict: the other pull request, its review job and the commit.
func sharedChangesRequestedCause(held []sharedChangesRequested, pullRequest int) string {
	parts := make([]string, 0, len(held))
	for _, verdict := range held {
		if verdict.pullRequest == pullRequest {
			parts = append(parts, fmt.Sprintf("commit %s is the head of this pull request's own earlier changes_requested review (job %s), with no later approval at that commit or at the current head",
				verdict.commit, verdict.jobID))
			continue
		}
		parts = append(parts, fmt.Sprintf("commit %s carried by this pull request is the head of PR #%d's changes_requested review (job %s), with no later approval at that commit; get PR #%d approved at %s, or drop that commit from this pull request",
			verdict.commit, verdict.pullRequest, verdict.jobID, verdict.pullRequest, verdict.commit))
	}
	return strings.Join(parts, "; ")
}

func normalizedSHA(sha string) string {
	return strings.ToLower(strings.TrimSpace(sha))
}
