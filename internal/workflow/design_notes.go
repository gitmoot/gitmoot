package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

const (
	designNotesLabel      = "design-notes"
	designNotesErrorEvent = "design_notes_error"
	// DesignNotesWindow is how far back a finding's FIRST observation may be
	// for the sweep to consider it. Rolling, measured from the sweep time.
	DesignNotesWindow = 14 * 24 * time.Hour
	// DesignNotesSweepLimit bounds the pull requests one sweep checks, so a
	// backlog cannot stall the daemon; the rest wait for the next sweep.
	DesignNotesSweepLimit = 50
	// GitHub rejects a comment over 65536 characters; the details of one pull
	// request's notes share this budget.
	designNotesCommentDetailBudget = 50000
	designNotesDetailMax           = 4000
	designNotesIssueBody           = "Design notes are advisory P3 findings from the review design checklist that were still open when their pull request merged. Pick up any worth doing, and close this issue once they are triaged.\n"
)

// DesignNotesPullRequestState is what GitHub reports for a candidate pull
// request. Closed means closed WITHOUT merging. HeadSHA is the merged head when
// known.
type DesignNotesPullRequestState struct {
	Merged  bool
	Closed  bool
	HeadSHA string
}

// DesignNotesIssue identifies a GitHub issue the sweep files into.
type DesignNotesIssue struct {
	Number int64
	URL    string
}

// DesignNotesPullRequestKey names one pull request; the sweep uses it as a
// round-robin cursor.
type DesignNotesPullRequestKey struct {
	Repo        string
	PullRequest int64
}

func (k DesignNotesPullRequestKey) after(other DesignNotesPullRequestKey) bool {
	if k.Repo != other.Repo {
		return k.Repo > other.Repo
	}
	return k.PullRequest > other.PullRequest
}

// DesignNotesSweepOptions bounds one sweep. Limit <= 0 means
// DesignNotesSweepLimit. After is the previous sweep's Next: this sweep starts
// at the first pull request after it, so a bounded sweep cannot starve the
// pull requests at the end of the list.
type DesignNotesSweepOptions struct {
	Limit int
	After DesignNotesPullRequestKey
}

// DesignNotesSweepResult reports one sweep. Filed and Skipped count findings;
// Pending counts pull requests still open. Next is the cursor for the next
// sweep, zero when this sweep reached every candidate.
type DesignNotesSweepResult struct {
	Next    DesignNotesPullRequestKey
	Checked int
	Filed   int
	Skipped int
	Pending int
	Created []string
	Errors  []string
}

type designNotesPullRequest struct {
	key      DesignNotesPullRequestKey
	findings []db.DesignNoteCandidate
	head     string
	role     string
}

// SweepDesignNotes files the open P3 findings of merged pull requests (#2558).
// The design checklist in every review produces P3 findings, which never block
// a merge and are not seen again once the pull request merges. Each merged pull
// request gets ONE comment, listing its open P3 findings, on the repository's
// issue for the current ISO week; findings of a pull request closed without
// merging are skipped for good, and an open pull request waits for a later
// sweep. A finding is marked filed only after its comment succeeds.
//
// Failures are per pull request: each is recorded as a design_notes_error job
// event on the review job that last observed a finding, returned in Errors,
// and the sweep continues. Only a failure to read the ledger aborts it.
func (e Engine) SweepDesignNotes(ctx context.Context, opts DesignNotesSweepOptions) (DesignNotesSweepResult, error) {
	var result DesignNotesSweepResult
	if e.Store == nil {
		return result, errors.New("design notes sweep requires a store")
	}
	if e.DesignNotesPullRequest == nil || e.DesignNotesCreateIssue == nil || e.DesignNotesComment == nil {
		return result, errors.New("design notes sweep hooks are not wired")
	}
	now := e.now().UTC()
	week := designNotesWeek(now)
	e.sendPendingDesignNotesNotices(ctx, week, now, &result)

	candidates, err := e.Store.ListDesignNoteCandidates(ctx, now.Add(-DesignNotesWindow))
	if err != nil {
		return result, err
	}
	prs := groupDesignNoteCandidates(candidates)
	limit := opts.Limit
	if limit <= 0 {
		limit = DesignNotesSweepLimit
	}
	batch := prs
	if len(prs) > limit {
		start := sort.Search(len(prs), func(i int) bool { return prs[i].key.after(opts.After) })
		rotated := append(append([]designNotesPullRequest{}, prs[start:]...), prs[:start]...)
		batch = rotated[:limit]
		result.Next = batch[len(batch)-1].key
	}

	roles := map[string]string{}
	var repos []string
	merged := map[string][]designNotesPullRequest{}
	for _, pr := range batch {
		if ctx.Err() != nil {
			result.Errors = append(result.Errors, "sweep stopped before checking every pull request: "+ctx.Err().Error())
			break
		}
		result.Checked++
		state, err := e.DesignNotesPullRequest(ctx, pr.key.Repo, int(pr.key.PullRequest))
		if err != nil {
			e.designNotesError(ctx, &result, pr, "pull request state: "+err.Error())
			continue
		}
		switch {
		case state.Merged:
			pr.head = firstNonEmptyLedgerText(state.HeadSHA, latestDesignNote(pr.findings).HeadSHA)
			for _, f := range pr.findings {
				e.designNotesRole(ctx, f.ObserverJob, roles)
			}
			pr.role = e.designNotesRole(ctx, latestDesignNote(pr.findings).ObserverJob, roles)
			if _, seen := merged[pr.key.Repo]; !seen {
				repos = append(repos, pr.key.Repo)
			}
			merged[pr.key.Repo] = append(merged[pr.key.Repo], pr)
		case state.Closed:
			filings := make([]db.DesignNoteFiling, 0, len(pr.findings))
			for _, f := range pr.findings {
				filings = append(filings, db.DesignNoteFiling{FindingUID: f.FindingUID, Repo: f.Repo, PullRequest: f.PullRequest,
					HeadSHA: f.HeadSHA, SkipReason: db.DesignNoteSkipClosedUnmerged})
			}
			if err := e.Store.RecordDesignNoteFilings(ctx, filings, now); err != nil {
				e.designNotesError(ctx, &result, pr, "record skip: "+err.Error())
				continue
			}
			result.Skipped += len(filings)
		default:
			result.Pending++
		}
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			result.Errors = append(result.Errors, "sweep stopped before filing every pull request: "+ctx.Err().Error())
			break
		}
		repoPRs := merged[repo]
		issue, err := e.designNotesWeekIssue(ctx, repo, week, repoPRs, now, &result)
		if err != nil {
			for _, pr := range repoPRs {
				e.designNotesError(ctx, &result, pr, "week issue: "+err.Error())
			}
			continue
		}
		for _, pr := range repoPRs {
			if err := e.DesignNotesComment(ctx, repo, issue.Number, designNotesComment(pr, roles)); err != nil {
				e.designNotesError(ctx, &result, pr, "comment on "+issue.URL+": "+err.Error())
				continue
			}
			filings := make([]db.DesignNoteFiling, 0, len(pr.findings))
			for _, f := range pr.findings {
				filings = append(filings, db.DesignNoteFiling{FindingUID: f.FindingUID, Repo: f.Repo, PullRequest: f.PullRequest,
					HeadSHA: f.HeadSHA, IssueURL: issue.URL})
			}
			// The comment is posted, so its record must not be lost to the sweep's
			// own deadline. If the write fails anyway the next sweep posts the
			// comment again: a duplicate is the recoverable failure, losing the
			// findings is not.
			if err := e.Store.RecordDesignNoteFilings(context.WithoutCancel(ctx), filings, now); err != nil {
				e.designNotesError(ctx, &result, pr, "record filing after comment on "+issue.URL+": "+err.Error())
				continue
			}
			result.Filed += len(filings)
		}
	}
	return result, nil
}

// designNotesWeekIssue returns the repository's issue for week: from the
// store, else from a GitHub search by exact title and label (a lost row must
// not open a duplicate), else a new issue, whose role is then told once.
func (e Engine) designNotesWeekIssue(ctx context.Context, repo, week string, prs []designNotesPullRequest, now time.Time, result *DesignNotesSweepResult) (DesignNotesIssue, error) {
	recorded, ok, err := e.Store.GetDesignNoteIssue(ctx, repo, week)
	if err != nil {
		return DesignNotesIssue{}, err
	}
	if ok {
		return DesignNotesIssue{Number: recorded.Number, URL: recorded.URL}, nil
	}
	title := designNotesIssueTitle(repo, week)
	if e.DesignNotesFindIssue != nil {
		found, exists, err := e.DesignNotesFindIssue(ctx, repo, title, designNotesLabel)
		if err != nil {
			return DesignNotesIssue{}, fmt.Errorf("search %q: %w", title, err)
		}
		if exists {
			if _, err := e.Store.RecordDesignNoteIssue(ctx, db.DesignNoteIssue{Repo: repo, ISOWeek: week, Number: found.Number, URL: found.URL}, now); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: record found issue %s: %v", repo, found.URL, err))
			}
			return found, nil
		}
	}
	role := designNotesIssueRole(prs)
	agentLabel := postMergeUnassignedAgentLabel
	if role != "" {
		agentLabel = "agent:" + role
	}
	created, err := e.DesignNotesCreateIssue(ctx, repo, title, designNotesIssueBody, []string{designNotesLabel, agentLabel})
	if err != nil {
		return DesignNotesIssue{}, fmt.Errorf("create %q: %w", title, err)
	}
	result.Created = append(result.Created, created.URL)
	stored, err := e.Store.RecordDesignNoteIssue(context.WithoutCancel(ctx), db.DesignNoteIssue{Repo: repo, ISOWeek: week, Number: created.Number, URL: created.URL, Role: role, Created: true}, now)
	if err != nil {
		// The issue exists, so file into it; without the row the next sweep
		// finds it by search instead. The notice is skipped rather than risk
		// sending it twice.
		result.Errors = append(result.Errors, fmt.Sprintf("%s: record created issue %s: %v", repo, created.URL, err))
		return created, nil
	}
	e.sendDesignNotesNotice(ctx, stored, now, result)
	return DesignNotesIssue{Number: stored.Number, URL: stored.URL}, nil
}

// sendPendingDesignNotesNotices retries notices for this week's created issues
// whose earlier send failed.
func (e Engine) sendPendingDesignNotesNotices(ctx context.Context, week string, now time.Time, result *DesignNotesSweepResult) {
	pending, err := e.Store.ListDesignNoteIssuesAwaitingNotice(ctx, week)
	if err != nil {
		result.Errors = append(result.Errors, "pending notices: "+err.Error())
		return
	}
	for _, issue := range pending {
		e.sendDesignNotesNotice(ctx, issue, now, result)
	}
}

// sendDesignNotesNotice tells the issue's role about a week's new issue. The
// claim is taken before the message is written, so a role hears at most once
// per repository per week; a failed write releases the claim for a retry.
func (e Engine) sendDesignNotesNotice(ctx context.Context, issue db.DesignNoteIssue, now time.Time, result *DesignNotesSweepResult) {
	if !issue.Created || strings.TrimSpace(issue.Role) == "" {
		return
	}
	// Local writes only; the sweep's GitHub deadline must not strand a claim.
	ctx = context.WithoutCancel(ctx)
	claimed, err := e.Store.ClaimDesignNoteIssueNotice(ctx, issue.Repo, issue.ISOWeek, now)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s: claim notice: %v", issue.Repo, err))
		return
	}
	if !claimed {
		return
	}
	body := fmt.Sprintf("Design notes for %s this week: %s. Advisory; nothing blocks.", issue.Repo, issue.URL)
	if _, err := e.Store.CreateMessage(ctx, db.Message{Sender: db.MessageSystemSender, Recipient: issue.Role, Body: body}); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s: notice to %s: %v", issue.Repo, issue.Role, err))
		if releaseErr := e.Store.ReleaseDesignNoteIssueNotice(ctx, issue.Repo, issue.ISOWeek); releaseErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: release notice claim: %v", issue.Repo, releaseErr))
		}
	}
}

// designNotesRole resolves who requested the review job: ReviewRequester, else
// ActingOrgRole (the post-merge P2 rule), else the job's dispatched_by. ""
// means unassigned.
func (e Engine) designNotesRole(ctx context.Context, jobID string, cache map[string]string) string {
	jobID = strings.TrimSpace(jobID)
	if role, ok := cache[jobID]; ok {
		return role
	}
	role := ""
	if jobID != "" {
		if job, err := e.Store.GetJob(ctx, jobID); err == nil {
			if payload, err := ParseJobPayload(job.Payload); err == nil {
				role = firstNonEmptyLedgerText(payload.ReviewRequester, payload.ActingOrgRole)
			}
			role = firstNonEmptyLedgerText(role, job.DispatchedBy)
		}
	}
	cache[jobID] = role
	return role
}

func (e Engine) designNotesError(ctx context.Context, result *DesignNotesSweepResult, pr designNotesPullRequest, message string) {
	result.Errors = append(result.Errors, fmt.Sprintf("%s#%d: %s", pr.key.Repo, pr.key.PullRequest, message))
	if job := strings.TrimSpace(latestDesignNote(pr.findings).ObserverJob); job != "" {
		_ = e.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: job, Kind: designNotesErrorEvent, Message: message})
	}
}

// groupDesignNoteCandidates groups candidates, already ordered by repository
// and pull request, into one entry per pull request.
func groupDesignNoteCandidates(candidates []db.DesignNoteCandidate) []designNotesPullRequest {
	var prs []designNotesPullRequest
	for _, c := range candidates {
		key := DesignNotesPullRequestKey{Repo: c.Repo, PullRequest: c.PullRequest}
		if n := len(prs); n > 0 && prs[n-1].key == key {
			prs[n-1].findings = append(prs[n-1].findings, c)
			continue
		}
		prs = append(prs, designNotesPullRequest{key: key, findings: []db.DesignNoteCandidate{c}})
	}
	return prs
}

// latestDesignNote returns the most recently observed finding of a pull
// request; its review job stands for the pull request.
func latestDesignNote(findings []db.DesignNoteCandidate) db.DesignNoteCandidate {
	var latest db.DesignNoteCandidate
	var latestAt time.Time
	for i, f := range findings {
		at, err := time.Parse(time.RFC3339Nano, f.ObservedAt)
		if i == 0 || (err == nil && !at.Before(latestAt)) {
			latest, latestAt = f, at
		}
	}
	return latest
}

// designNotesIssueRole picks the role a new week issue is labelled for and
// tells: the role behind the most merged pull requests in this sweep, ties by
// name; "" when none has a role.
func designNotesIssueRole(prs []designNotesPullRequest) string {
	counts := map[string]int{}
	best := ""
	for _, pr := range prs {
		if pr.role == "" {
			continue
		}
		counts[pr.role]++
		if c := counts[pr.role]; best == "" || c > counts[best] || (c == counts[best] && pr.role < best) {
			best = pr.role
		}
	}
	return best
}

func designNotesWeek(now time.Time) string {
	year, week := now.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func designNotesIssueTitle(repo, week string) string {
	return fmt.Sprintf("[design-notes] %s %s", repo, week)
}

func designNotesComment(pr designNotesPullRequest, roles map[string]string) string {
	detailMax := designNotesDetailMax
	if perFinding := designNotesCommentDetailBudget / len(pr.findings); perFinding < detailMax {
		detailMax = perFinding
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Open P3 design notes from #%d, merged at `%s`.\n", pr.key.PullRequest, pr.head)
	for i, f := range pr.findings {
		detail := strings.TrimSpace(f.Detail)
		if len(detail) > detailMax {
			detail = strings.ToValidUTF8(detail[:detailMax], "") + " …"
		}
		title := firstNonEmptyLedgerText(f.Title, detail, "(untitled)")
		if len(title) > 120 {
			title = strings.ToValidUTF8(title[:120], "")
		}
		location := strings.TrimSpace(f.File)
		if location != "" && f.Line > 0 {
			location += ":" + strconv.FormatInt(f.Line, 10)
		}
		if location == "" {
			location = "not stated"
		}
		requester := firstNonEmptyLedgerText(roles[strings.TrimSpace(f.ObserverJob)], "unassigned")
		fmt.Fprintf(&b, "\n### %d. %s\n\n", i+1, title)
		fmt.Fprintf(&b, "- Location: `%s`\n", location)
		fmt.Fprintf(&b, "- Review job: `%s` (requested by %s)\n", firstNonEmptyLedgerText(f.ObserverJob, "unknown"), requester)
		fmt.Fprintf(&b, "- Pull request: #%d, merged head `%s`\n", pr.key.PullRequest, pr.head)
		if detail != "" {
			fmt.Fprintf(&b, "\n%s\n", detail)
		}
		fmt.Fprintf(&b, "\nfinding-uid: %s\n", f.FindingUID)
	}
	return b.String()
}
