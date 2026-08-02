package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	taskDisposalTierOwnPRMerged     = "tier1_own_pr_merged"
	taskDisposalTierSuccessorMerged = "tier2_successor_merged"
	taskDisposalTierSubjectClosed   = "tier3_subject_closed"
	taskDisposalTierStranded        = "tier4_stranded"
	taskDisposalReasonUnavailable   = "subject unavailable"
	taskDisposalReasonNoEvidence    = "no disposal evidence"
)

var (
	qualifiedIssuePattern = regexp.MustCompile(`(?i)([a-z0-9_.-]+/[a-z0-9_.-]+)#([1-9][0-9]*)`)
	shortRepoIssuePattern = regexp.MustCompile(`(?i)\b([a-z0-9_.-]+)#([1-9][0-9]*)\b`)
	issueWordPattern      = regexp.MustCompile(`(?i)\bissue\s+#?([1-9][0-9]*)\b`)
	bareIssuePattern      = regexp.MustCompile(`(?:^|[\s(])#([1-9][0-9]*)\b`)
)

type taskDisposalEvidence struct {
	subjectRepo  string
	subjectIssue int64
	pullRequest  int64
}

type issueGetter interface {
	GetIssue(context.Context, github.Repository, int64) (github.Issue, error)
}

// reconcileTaskDisposals terminates old blocked obligations by forge evidence,
// strongest first, then by the bounded stranded fallback. It deliberately does
// not inspect git ancestry: superseding branches need not be ancestors of main.
func (d Daemon) reconcileTaskDisposals(ctx context.Context, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	candidates, err := d.Store.ListStaleTaskCandidates(ctx, d.Repo.FullName(), []string{
		string(workflow.TaskBlocked), string(workflow.TaskAwaitingHumanMerge),
	}, d.now().Add(-ttl), staleTaskReconcileScanLimit)
	if err != nil || len(candidates) == 0 {
		return err
	}
	if len(candidates) > staleTaskReconcileLimit {
		candidates = candidates[:staleTaskReconcileLimit]
	}

	tasks, taskErr := d.Store.ListTasksByRepo(ctx, d.Repo.FullName())
	jobs, jobErr := d.Store.ListJobsByRepo(ctx, d.Repo.FullName())
	evidence := buildTaskDisposalEvidence(d.Repo.FullName(), tasks, jobs)
	orgConfig, orgErr := config.LoadOrg(config.Paths{ConfigFile: taskDisposalConfigPath(d.Store)})
	if orgErr != nil {
		orgConfig = config.OrgConfig{}
	}

	var firstErr error
	for _, candidate := range candidates {
		if err := d.disposeTaskCandidate(ctx, candidate, tasks, evidence, taskErr, jobErr, orgConfig); err != nil {
			// One bad row never aborts the pass. The candidate helper attempts the
			// stranded transition before returning; only a failed terminal write lands
			// here, and later candidates still receive their bound.
			d.logf("task disposal %s failed: %v", candidate.ID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (d Daemon) disposeTaskCandidate(ctx context.Context, candidate db.StaleTaskCandidate, tasks []db.Task, evidence map[string]taskDisposalEvidence, taskErr, jobErr error, orgConfig config.OrgConfig) error {
	blockedAt, err := parseTaskStoreTime(candidate.UpdatedAt)
	if err != nil {
		return d.strandTask(ctx, candidate, taskDisposalReasonUnavailable+": invalid task timestamp", orgConfig)
	}
	if taskErr != nil || jobErr != nil {
		return d.strandTask(ctx, candidate, taskDisposalReasonUnavailable, orgConfig)
	}
	itemEvidence := evidence[candidate.ID]
	if itemEvidence.pullRequest == 0 && strings.TrimSpace(candidate.Branch) != "" {
		stored, lookupErr := d.Store.GetPullRequestByRepoBranch(ctx, candidate.RepoFullName, candidate.Branch)
		if lookupErr == nil {
			itemEvidence.pullRequest = stored.Number
		} else if !errors.Is(lookupErr, sql.ErrNoRows) {
			return d.strandTask(ctx, candidate, taskDisposalReasonUnavailable, orgConfig)
		}
	}

	var ownPR github.PullRequest
	var ownPRKnown bool
	if itemEvidence.pullRequest > 0 {
		ownPR, err = d.GitHub.GetPullRequest(ctx, d.Repo, itemEvidence.pullRequest)
		if err != nil {
			return d.strandTask(ctx, candidate, taskDisposalReasonUnavailable, orgConfig)
		}
		ownPRKnown = true
		if pullRequestListedAsMerged(ownPR) {
			return d.commitTaskDisposal(ctx, candidate, workflow.TaskMerged, taskDisposalTierOwnPRMerged,
				fmt.Sprintf("own PR #%d is merged in forge", ownPR.Number), "", "")
		}
		if workflow.TaskState(candidate.State) == workflow.TaskAwaitingHumanMerge && strings.EqualFold(strings.TrimSpace(ownPR.State), "open") {
			return d.strandTask(ctx, candidate, taskDisposalReasonNoEvidence+": own PR remains open and awaiting_human_merge is protected", orgConfig)
		}
	}

	if itemEvidence.subjectIssue > 0 {
		for _, successor := range tasks {
			if successor.ID == candidate.ID || !taskUpdatedAfter(successor.UpdatedAt, blockedAt) {
				continue
			}
			peerEvidence := evidence[successor.ID]
			if peerEvidence.subjectIssue != itemEvidence.subjectIssue || !strings.EqualFold(peerEvidence.subjectRepo, itemEvidence.subjectRepo) {
				continue
			}
			if workflow.TaskState(successor.State) == workflow.TaskMerged {
				return d.commitTaskDisposal(ctx, candidate, workflow.TaskSuperseded, taskDisposalTierSuccessorMerged,
					fmt.Sprintf("successor task %s on %s#%d merged after this task blocked", successor.ID, itemEvidence.subjectRepo, itemEvidence.subjectIssue), "", "")
			}
			if peerEvidence.pullRequest <= 0 {
				continue
			}
			pull, pullErr := d.GitHub.GetPullRequest(ctx, d.Repo, peerEvidence.pullRequest)
			if pullErr != nil {
				return d.strandTask(ctx, candidate, taskDisposalReasonUnavailable, orgConfig)
			}
			if pullRequestListedAsMerged(pull) && mergedAfter(pull.MergedAt, blockedAt) {
				return d.commitTaskDisposal(ctx, candidate, workflow.TaskSuperseded, taskDisposalTierSuccessorMerged,
					fmt.Sprintf("successor PR #%d on %s#%d merged after this task blocked", pull.Number, itemEvidence.subjectRepo, itemEvidence.subjectIssue), "", "")
			}
		}
		getter, ok := d.GitHub.(issueGetter)
		if ok {
			subjectRepo, parseErr := ParseRepository(itemEvidence.subjectRepo)
			if parseErr != nil {
				return d.strandTask(ctx, candidate, taskDisposalReasonUnavailable, orgConfig)
			}
			issue, issueErr := getter.GetIssue(ctx, subjectRepo, itemEvidence.subjectIssue)
			if issueErr != nil {
				return d.strandTask(ctx, candidate, taskDisposalReasonUnavailable, orgConfig)
			}
			if strings.EqualFold(strings.TrimSpace(issue.State), "closed") {
				return d.commitTaskDisposal(ctx, candidate, workflow.TaskSuperseded, taskDisposalTierSubjectClosed,
					fmt.Sprintf("referenced subject %s#%d is closed", itemEvidence.subjectRepo, itemEvidence.subjectIssue), "", "")
			}
		}
	}
	if itemEvidence.subjectIssue == 0 && ownPRKnown && strings.EqualFold(strings.TrimSpace(ownPR.State), "closed") {
		return d.commitTaskDisposal(ctx, candidate, workflow.TaskSuperseded, taskDisposalTierSubjectClosed,
			fmt.Sprintf("referenced PR #%d is closed without a merge", ownPR.Number), "", "")
	}
	return d.strandTask(ctx, candidate, taskDisposalReasonNoEvidence, orgConfig)
}

func (d Daemon) strandTask(ctx context.Context, candidate db.StaleTaskCandidate, reason string, orgConfig config.OrgConfig) error {
	role := d.taskDisposalEscalationRole(ctx, candidate, orgConfig)
	routeDetail := "escalation unroutable: no live ancestor"
	if role != "" {
		routeDetail = "escalation addressed to " + role
	}
	fullReason := strings.TrimSpace(reason) + "; " + routeDetail
	payload := ""
	if role != "" {
		ev := events.NewEvent(events.EventJobNeedsAttention, candidate.ID, candidate.ID, candidate.RepoFullName,
			string(workflow.TaskStranded), fmt.Sprintf("task %s stranded: %s", candidate.ID, fullReason), d.now(), workflow.RedactCommentText)
		ev.Cause = "task_disposal_stranded"
		ev.WakeKind = db.WakeOutboxKindEscalation
		ev.WakeTargetRole = role
		encoded, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		payload = string(encoded)
	}
	return d.commitTaskDisposal(ctx, candidate, workflow.TaskStranded, taskDisposalTierStranded, fullReason, role, payload)
}

func (d Daemon) commitTaskDisposal(ctx context.Context, candidate db.StaleTaskCandidate, state workflow.TaskState, tier, reason, role, event string) error {
	_, _, err := d.Store.DisposeTask(ctx, candidate.ID,
		[]string{string(workflow.TaskBlocked), string(workflow.TaskAwaitingHumanMerge)},
		string(state), tier, reason, role, event, d.now())
	return err
}

func (d Daemon) taskDisposalEscalationRole(ctx context.Context, candidate db.StaleTaskCandidate, orgConfig config.OrgConfig) string {
	actingRole := ""
	if lock, err := d.Store.GetBranchLock(ctx, candidate.RepoFullName, candidate.Branch); err == nil {
		actingRole = strings.ToLower(strings.TrimSpace(lock.ActingOrgRole))
	}
	if _, ok := orgConfig.Role(actingRole); ok {
		for _, ancestor := range orgConfig.Ancestors(actingRole) {
			if role, exists := orgConfig.Role(ancestor); exists && config.ScopeMatches(role.Scope, candidate.RepoFullName) {
				return ancestor
			}
		}
	}
	for _, root := range orgConfig.Roots() {
		if role, ok := orgConfig.Role(root); ok && config.ScopeMatches(role.Scope, candidate.RepoFullName) {
			return root
		}
	}
	return ""
}

func buildTaskDisposalEvidence(repo string, tasks []db.Task, jobs []db.Job) map[string]taskDisposalEvidence {
	byID := make(map[string]taskDisposalEvidence, len(tasks))
	byBranch := make(map[string]string, len(tasks))
	for _, task := range tasks {
		subjectRepo, subjectIssue := taskIssueSubject(repo, task.Title)
		byID[task.ID] = taskDisposalEvidence{subjectRepo: subjectRepo, subjectIssue: subjectIssue}
		if branch := strings.TrimSpace(task.Branch); branch != "" {
			byBranch[branch] = task.ID
		}
	}
	for _, job := range jobs {
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			continue
		}
		taskID := strings.TrimSpace(payload.TaskID)
		if taskID == "" && strings.TrimSpace(payload.Repo) == strings.TrimSpace(repo) {
			taskID = byBranch[strings.TrimSpace(payload.Branch)]
		}
		if _, ok := byID[taskID]; !ok {
			continue
		}
		item := byID[taskID]
		if payload.PullRequest > 0 {
			item.pullRequest = int64(payload.PullRequest)
		}
		if subjectRepo, issue := taskIssueSubject(repo, payload.Instructions); issue > 0 {
			item.subjectRepo, item.subjectIssue = subjectRepo, issue
		}
		byID[taskID] = item
	}
	return byID
}

func taskIssueSubject(defaultRepo, text string) (string, int64) {
	if match := qualifiedIssuePattern.FindStringSubmatch(text); len(match) == 3 {
		number, _ := strconv.ParseInt(match[2], 10, 64)
		return strings.ToLower(match[1]), number
	}
	if match := shortRepoIssuePattern.FindStringSubmatch(text); len(match) == 3 {
		owner, repoName, ok := strings.Cut(strings.ToLower(strings.TrimSpace(defaultRepo)), "/")
		if ok && strings.EqualFold(repoName, match[1]) {
			number, _ := strconv.ParseInt(match[2], 10, 64)
			return owner + "/" + repoName, number
		}
	}
	for _, pattern := range []*regexp.Regexp{issueWordPattern, bareIssuePattern} {
		if match := pattern.FindStringSubmatch(text); len(match) == 2 {
			number, _ := strconv.ParseInt(match[1], 10, 64)
			return strings.ToLower(strings.TrimSpace(defaultRepo)), number
		}
	}
	return "", 0
}

func parseTaskStoreTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid task timestamp %q", value)
}

func taskUpdatedAfter(value string, after time.Time) bool {
	parsed, err := parseTaskStoreTime(value)
	return err == nil && parsed.After(after)
}

func mergedAfter(value string, after time.Time) bool {
	parsed, err := parseTaskStoreTime(value)
	return err == nil && parsed.After(after)
}

func taskDisposalConfigPath(store *db.Store) string {
	return filepath.Join(filepath.Dir(store.DatabasePath()), config.ConfigName)
}
