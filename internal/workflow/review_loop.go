package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// ReviewLoopDetectedEventKind marks a refused review dispatch whose verdict is
// already stable at the requested PR head.
const ReviewLoopDetectedEventKind = "review_loop_detected"

// ReviewScopeUnavailableBlockMarker distinguishes a durable, exact-head scope
// block from other workflow blocks that the daemon must continue retrying.
const ReviewScopeUnavailableBlockMarker = "cannot safely scope follow-up review"

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

type reviewScopeCandidate struct {
	job      db.Job
	payload  JobPayload
	round    int
	findings []string
}

// ReviewScopeUnavailableError marks a follow-up range that cannot be scoped
// safely. Callers block the task visibly instead of falling back to a full review
// or retrying the same permanently unscopable head on every daemon poll.
type ReviewScopeUnavailableError struct {
	Reason string
}

func (e ReviewScopeUnavailableError) Error() string {
	return strings.TrimSpace(e.Reason)
}

func reviewScopeKey(reviewer, delegationID string) string {
	reviewer = strings.ToLower(strings.TrimSpace(reviewer))
	delegationID = strings.ToLower(strings.TrimSpace(delegationID))
	if delegationID == "" {
		return reviewer
	}
	return reviewer + "\x00" + delegationID
}

func reviewScopeFor(scopes map[string]*ReviewScope, reviewer, delegationID string) *ReviewScope {
	if scope := scopes[reviewScopeKey(reviewer, delegationID)]; scope != nil {
		return scope
	}
	return scopes[reviewScopeKey(reviewer, "")]
}

func (e Engine) followUpReviewScopes(ctx context.Context, event PullRequestEvent, reviewers []string, jobs []db.Job) (map[string]*ReviewScope, error) {
	wanted := make(map[string]struct{}, len(reviewers))
	for _, reviewer := range reviewers {
		wanted[strings.ToLower(strings.TrimSpace(reviewer))] = struct{}{}
	}
	current := JobPayload{Repo: event.Repo, PullRequest: event.PullRequest, TaskID: event.TaskID}
	candidates := make(map[string]reviewScopeCandidate, len(wanted))
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return nil, err
		}
		if !sameTask(current, payload) || payload.Result == nil {
			continue
		}
		previousHead := strings.TrimSpace(payload.HeadSHA)
		if previousHead == "" || previousHead == strings.TrimSpace(event.HeadSHA) {
			continue
		}
		findings := namedReviewFindings(*payload.Result)
		switch strings.TrimSpace(payload.Result.Decision) {
		case "approved", "changes_requested":
			if job.State != string(JobSucceeded) {
				continue
			}
		case "blocked":
			if job.State != string(JobBlocked) || len(findings) == 0 {
				continue
			}
		default:
			continue
		}
		reviewerKey := strings.ToLower(strings.TrimSpace(reviewDecisionAgent(job, payload)))
		if _, ok := wanted[reviewerKey]; !ok {
			continue
		}
		candidate := reviewScopeCandidate{
			job:      job,
			payload:  payload,
			round:    reviewRoundCount(payload.ReviewRound),
			findings: findings,
		}
		scopeKey := reviewScopeKey(reviewerKey, payload.DelegationID)
		if prior, ok := candidates[scopeKey]; !ok || laterReviewScopeCandidate(candidate, prior) {
			candidates[scopeKey] = candidate
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if e.ReviewChangedFiles == nil {
		return nil, ReviewScopeUnavailableError{Reason: "scoped follow-up review requires a changed-files resolver"}
	}
	filesByHead := make(map[string][]string, len(candidates))
	scopes := make(map[string]*ReviewScope, len(candidates))
	for scopeKey, candidate := range candidates {
		previousHead := strings.TrimSpace(candidate.payload.HeadSHA)
		files, ok := filesByHead[previousHead]
		if !ok {
			var err error
			files, err = e.ReviewChangedFiles(ctx, event.Repo, event.PullRequest, previousHead, event.HeadSHA)
			if err != nil {
				return nil, fmt.Errorf("resolve review scope from %s to %s: %w", previousHead, event.HeadSHA, err)
			}
			files = sortedUniqueStrings(files)
			filesByHead[previousHead] = files
		}
		scopes[scopeKey] = &ReviewScope{
			PreviousHeadSHA: previousHead,
			Findings:        append([]string(nil), candidate.findings...),
			ChangedFiles:    append([]string(nil), files...),
		}
	}
	return scopes, nil
}

func laterReviewScopeCandidate(candidate, prior reviewScopeCandidate) bool {
	if candidate.round != prior.round {
		return candidate.round > prior.round
	}
	if candidate.job.UpdatedAt != prior.job.UpdatedAt {
		return candidate.job.UpdatedAt > prior.job.UpdatedAt
	}
	if candidate.job.CreatedAt != prior.job.CreatedAt {
		return candidate.job.CreatedAt > prior.job.CreatedAt
	}
	return candidate.job.ID > prior.job.ID
}

func namedReviewFindings(result AgentResult) []string {
	findings := make([]string, 0, len(result.Findings))
	for _, raw := range result.Findings {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			findings = append(findings, text)
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err == nil {
			findings = append(findings, compact.String())
		}
	}
	findings = stableUniqueStrings(findings)
	if len(findings) == 0 && strings.TrimSpace(result.Decision) == "changes_requested" && strings.TrimSpace(result.Summary) != "" {
		findings = []string{strings.TrimSpace(result.Summary)}
	}
	return findings
}

func stableUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func sortedUniqueStrings(values []string) []string {
	out := stableUniqueStrings(values)
	sort.Strings(out)
	return out
}

func scopedReviewInstructions(event PullRequestEvent, scope *ReviewScope) string {
	var instructions strings.Builder
	fmt.Fprintf(&instructions, "Review pull request #%d as a scoped follow-up for task %s.\n", event.PullRequest, taskLabel(event.TaskID, event.TaskTitle))
	fmt.Fprintf(&instructions, "The reviewer last saw exact head %s; the current head is %s. Diff from that prior head, never from the PR base.\n", scope.PreviousHeadSHA, event.HeadSHA)
	instructions.WriteString("Named findings still in scope:\n")
	if len(scope.Findings) == 0 {
		instructions.WriteString("- none\n")
	} else {
		for _, finding := range scope.Findings {
			fmt.Fprintf(&instructions, "- %s\n", finding)
		}
	}
	instructions.WriteString("Files changed since the reviewer last saw the branch:\n")
	if len(scope.ChangedFiles) == 0 {
		instructions.WriteString("- none\n")
	} else {
		for _, path := range scope.ChangedFiles {
			fmt.Fprintf(&instructions, "- %s\n", path)
		}
	}
	instructions.WriteString("Do not re-review the full PR-to-base diff. Validate the named findings and the listed changed files, inspecting dependencies only as needed to detect defects introduced by those changes. Report only unresolved named findings or new defects introduced since the prior head.")
	if len(scope.Findings) == 0 && len(scope.ChangedFiles) == 0 {
		instructions.WriteString(" The scope is empty: approve without rereading the full diff.")
	}
	return instructions.String()
}

func reviewFixInstructions(reviewer string, result AgentResult) string {
	base := fmt.Sprintf("Address requested changes from %s: %s", reviewer, result.Summary)
	findings := namedReviewFindings(result)
	if len(findings) == 0 {
		return base
	}
	var instructions strings.Builder
	instructions.WriteString(base)
	instructions.WriteString("\nNamed findings to close:\n")
	for _, finding := range findings {
		fmt.Fprintf(&instructions, "- %s\n", finding)
	}
	return strings.TrimSpace(instructions.String())
}
