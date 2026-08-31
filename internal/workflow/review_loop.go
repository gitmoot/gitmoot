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

// ReviewScopeUnavailableMarker tags the task event recorded when a follow-up
// review range cannot be scoped from the exact head a reviewer last saw (a
// force-push makes that head DIVERGED, or the compare is truncated). It marks an
// audit record, never a block: blocking wedged the loop permanently, because the
// prior head a scope is derived from only advances when a new review job runs.
const ReviewScopeUnavailableMarker = "cannot safely scope follow-up review"

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

// ReviewScopeUnavailableError marks a follow-up RANGE that cannot be scoped from
// the exact head a reviewer last saw (a force-push left it diverged, the compare
// is truncated). HandlePullRequestOpened records it and re-reviews the full PR at
// that head, which re-anchors the prior head for the next round; blocking instead
// wedged the loop for good. A MISSING resolver is a wiring fault rather than an
// unscopable range, so it returns a plain error: the misconfiguration keeps
// surfacing instead of silently degrading every round to a full review.
type ReviewScopeUnavailableError struct {
	Reason string
}

func (e ReviewScopeUnavailableError) Error() string {
	return strings.TrimSpace(e.Reason)
}

// reviewScopeKey identifies one reviewer's follow-up scope. A high-risk round runs a
// single reviewer across several lenses (risk.go maps reviewers round-robin), so the
// lens id is part of the identity, and a ROUTINE round reads a separate aggregate.
// It is a struct, not a concatenated string: nothing validates delegation ids against
// control bytes, so a lens id carrying the reserved separator could otherwise collide
// with another key and overwrite that child's scope.
type reviewScopeKey struct {
	reviewer string
	lens     string
	// routine marks the aggregate a non-lens round reads. It is a distinct field
	// rather than a reserved lens value so no delegation id can name it.
	routine bool
}

func lensScopeKey(reviewer, delegationID string) reviewScopeKey {
	return reviewScopeKey{
		reviewer: strings.ToLower(strings.TrimSpace(reviewer)),
		lens:     strings.ToLower(strings.TrimSpace(delegationID)),
	}
}

// routineScopeKey names the union scope a ROUTINE (non-lens) round reads.
func routineScopeKey(reviewer string) reviewScopeKey {
	return reviewScopeKey{reviewer: strings.ToLower(strings.TrimSpace(reviewer)), routine: true}
}

func reviewScopeFor(scopes map[reviewScopeKey]*ReviewScope, reviewer, delegationID string) *ReviewScope {
	if scope := scopes[lensScopeKey(reviewer, delegationID)]; scope != nil {
		return scope
	}
	// A lens with no prior verdict of its own falls back to this reviewer's prior
	// ROUTINE verdict, never to the routine union: a lens must not inherit findings
	// raised by a sibling lens.
	return scopes[lensScopeKey(reviewer, "")]
}

// reviewScopeUnavailableRecorded reports whether this task already carries a
// review_scope_unavailable record for this PR at this exact head. It matches on that
// IDENTITY, not on the whole reason: the reason embeds the resolver's error text, so a
// second observation of the same unscopable head with a differently-worded transport
// error would otherwise add a duplicate audit row for one head.
//
// This is a read-then-write, so two lifecycle calls racing on the same head can both
// insert. There is no unique key on task_events to claim against, and the cost of the
// race is bounded to a duplicate AUDIT ROW: the dispatch itself is idempotent, and
// duplicate review legs are prevented separately by the exact-head/round guard in
// HandlePullRequestOpened.
func (e Engine) reviewScopeUnavailableRecorded(ctx context.Context, taskID string, pullRequest int, headSHA string) (bool, error) {
	events, err := e.Store.ListTaskEvents(ctx, taskID)
	if err != nil {
		return false, err
	}
	identity := fmt.Sprintf("pull_request=%d head_sha=%s:", pullRequest, strings.TrimSpace(headSHA))
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "review_scope_unavailable" && strings.Contains(events[i].Reason, identity) {
			return true, nil
		}
	}
	return false, nil
}

// reviewLegsAtHead returns the reviewers that already have a review job for this task
// at this exact head and round, keyed lowercase, mapped to that job's state.
//
// Two distinct cases, one answer. A QUEUED or RUNNING leg is work in flight, so a
// second leg is a duplicate. A TERMINAL leg cannot be revived by re-enqueue either:
// Enqueue fails on the existing id and the collision check compares derived content
// (payloadMatchesRequest compares Instructions and WorktreePath), so a re-derivation
// whose scope resolved differently — scoped on one poll, degraded to unscoped on the
// next — surfaced a raw `UNIQUE constraint failed: jobs.id` out of the lifecycle and
// re-fired it every poll. Identity is reviewer + head + round, and the deterministic
// id encodes exactly that, so skipping on identity is the idempotent answer to both.
// Re-attempting a HELD leg is the worker's path — the row stays queued and is
// re-dispatched — and nothing here silently retries a terminal verdict, which is the
// pre-existing contract.
func reviewLegsAtHead(jobs []db.Job, event PullRequestEvent, round string) map[string]string {
	current := JobPayload{Repo: event.Repo, PullRequest: event.PullRequest, TaskID: event.TaskID}
	head := strings.TrimSpace(event.HeadSHA)
	round = strings.TrimSpace(round)
	legs := map[string]string{}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			// An unparseable payload cannot prove a leg exists; the enqueue path stays
			// authoritative for it.
			continue
		}
		if !sameTask(current, payload) {
			continue
		}
		if strings.TrimSpace(payload.HeadSHA) != head || strings.TrimSpace(payload.ReviewRound) != round {
			continue
		}
		if agent := strings.ToLower(strings.TrimSpace(reviewDecisionAgent(job, payload))); agent != "" {
			legs[agent] = job.State
		}
	}
	return legs
}

// reviewScopeForRoutine resolves the scope for a ROUTINE (non-lens) round. It reads
// one key: the union routineScopeAggregates builds from EVERY candidate that reviewer
// has, lens or not. Reading the bare-reviewer key instead lost a preceding high-risk
// round's lens findings entirely (routine -> lens -> routine dropped the lens round),
// and choosing between bare and lens by recency would have dropped whichever side lost.
func reviewScopeForRoutine(scopes map[reviewScopeKey]*ReviewScope, reviewer string) *ReviewScope {
	return scopes[routineScopeKey(reviewer)]
}

// routineScopeAggregates unions ALL of a reviewer's scopes — its bare routine verdict
// and every lens verdict, whatever round each came from — into the one scope a routine
// round reads. Findings and changed files are merged, so no round's live findings are
// dropped, and the OLDEST baseline head is named because its changed-file range is the
// superset that covers every merged baseline. A reviewer with a single bare candidate
// aggregates to a copy of it, so the no-lens path is unchanged.
//
// Candidates are visited in a SORTED order rather than in map order. The merged finding
// list keeps first-seen order, so map-order iteration made the union's finding order —
// and therefore the reviewer's prompt, and therefore Engine.jobID, which hashes
// Instructions — differ between runs on identical inputs.
func routineScopeAggregates(candidates map[reviewScopeKey]reviewScopeCandidate, scopes map[reviewScopeKey]*ReviewScope) map[reviewScopeKey]*ReviewScope {
	merged := make(map[reviewScopeKey]*ReviewScope, len(candidates))
	rounds := make(map[reviewScopeKey]int, len(candidates))
	for _, key := range sortedReviewScopeKeys(candidates) {
		candidate := candidates[key]
		scope := scopes[key]
		if scope == nil {
			continue
		}
		routineKey := routineScopeKey(key.reviewer)
		prior, ok := merged[routineKey]
		if !ok {
			merged[routineKey] = &ReviewScope{
				PreviousHeadSHA: scope.PreviousHeadSHA,
				Findings:        append([]string(nil), scope.Findings...),
				ChangedFiles:    append([]string(nil), scope.ChangedFiles...),
			}
			rounds[routineKey] = candidate.round
			continue
		}
		if olderRoutineBaseline(candidate.round, scope.PreviousHeadSHA, rounds[routineKey], prior.PreviousHeadSHA) {
			prior.PreviousHeadSHA = scope.PreviousHeadSHA
			rounds[routineKey] = candidate.round
		}
		prior.Findings = append(prior.Findings, scope.Findings...)
		prior.ChangedFiles = append(prior.ChangedFiles, scope.ChangedFiles...)
	}
	for _, scope := range merged {
		scope.Findings = stableUniqueStrings(scope.Findings)
		scope.ChangedFiles = sortedUniqueStrings(scope.ChangedFiles)
	}
	return merged
}

// sortedReviewScopeKeys orders a candidate set so every derived union is byte-identical
// across runs. The order groups by reviewer, then that reviewer's own bare routine
// verdict (its lens id is empty, which sorts first), then each lens id: sorting the
// CANDIDATES rather than the merged finding text keeps the grouping the reviewer reads
// in its prompt — its own routine findings, then one stable block per lens.
func sortedReviewScopeKeys(candidates map[reviewScopeKey]reviewScopeCandidate) []reviewScopeKey {
	keys := make([]reviewScopeKey, 0, len(candidates))
	for key := range candidates {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].reviewer != keys[j].reviewer {
			return keys[i].reviewer < keys[j].reviewer
		}
		if keys[i].routine != keys[j].routine {
			return !keys[i].routine
		}
		return keys[i].lens < keys[j].lens
	})
	return keys
}

// olderRoutineBaseline reports whether a candidate scope is the older baseline: the
// earlier round wins, then the lexicographically smaller head so map iteration order
// cannot change which head the aggregate names.
func olderRoutineBaseline(round int, head string, priorRound int, priorHead string) bool {
	if round != priorRound {
		return round < priorRound
	}
	return head < priorHead
}

func (e Engine) followUpReviewScopes(ctx context.Context, event PullRequestEvent, reviewers []string, jobs []db.Job) (map[reviewScopeKey]*ReviewScope, error) {
	wanted := make(map[string]struct{}, len(reviewers))
	for _, reviewer := range reviewers {
		wanted[strings.ToLower(strings.TrimSpace(reviewer))] = struct{}{}
	}
	current := JobPayload{Repo: event.Repo, PullRequest: event.PullRequest, TaskID: event.TaskID}
	candidates := make(map[reviewScopeKey]reviewScopeCandidate, len(wanted))
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
		scopeKey := lensScopeKey(reviewerKey, payload.DelegationID)
		if prior, ok := candidates[scopeKey]; !ok || laterReviewScopeCandidate(candidate, prior) {
			candidates[scopeKey] = candidate
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if e.ReviewChangedFiles == nil {
		return nil, fmt.Errorf("scoped follow-up review requires a changed-files resolver")
	}
	filesByHead := make(map[string][]string, len(candidates))
	scopes := make(map[reviewScopeKey]*ReviewScope, len(candidates))
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
	for key, aggregate := range routineScopeAggregates(candidates, scopes) {
		scopes[key] = aggregate
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
