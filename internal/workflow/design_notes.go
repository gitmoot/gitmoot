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
	// designNotesGrace bounds a local write that must outlive the sweep's
	// deadline: the record of a comment already posted, or a diagnostic event.
	designNotesGrace = 5 * time.Second
	// GitHub rejects a comment over 65536 characters. Every comment is kept
	// under this many BYTES, which bounds characters too, with margin.
	designNotesCommentMax = 60000
	// designNotesHeaderReserve is the comment budget kept for the header line.
	designNotesHeaderReserve = 512
	designNotesDetailMax     = 4000
	designNotesTitleMax      = 120
	designNotesFieldMax      = 300
	// designNotesCommentPrefix starts every comment the sweep posts; only such
	// comments are read back for markers.
	designNotesCommentPrefix = "Open P3 design notes from #"
	designNotesUIDMarker     = "finding-uid: "
	designNotesIssueBody     = "Design notes are advisory P3 findings from the review design checklist that were still open when their pull request merged. Pick up any worth doing, and close this issue once they are triaged.\n"
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
// at the first pull request after it and wraps around, so neither the bound
// nor a deadline can starve the pull requests later in the list.
type DesignNotesSweepOptions struct {
	Limit int
	After DesignNotesPullRequestKey
}

// DesignNotesSweepResult reports one sweep. Filed counts findings filed,
// including Repaired ones whose comment was already on the issue; Skipped
// counts findings of pull requests closed unmerged; Pending counts pull
// requests still open. Next is the last pull request this sweep attempted, or
// the sweep's After when it attempted none.
type DesignNotesSweepResult struct {
	Next     DesignNotesPullRequestKey
	Checked  int
	Filed    int
	Repaired int
	Skipped  int
	Pending  int
	Created  []string
	Errors   []string
}

type designNotesPullRequest struct {
	key      DesignNotesPullRequestKey
	findings []db.DesignNoteCandidate
}

// designNotesWeekIssue is a week issue as this sweep knows it: Posted holds
// the finding uids its comments already carry.
type designNotesWeekIssue struct {
	DesignNotesIssue
	posted map[string]bool
}

// designNotesSweep is the state of one sweep.
type designNotesSweep struct {
	e      Engine
	now    time.Time
	week   string
	result *DesignNotesSweepResult
	roles  map[string]string
	issues map[string]*designNotesWeekIssue
}

// SweepDesignNotes files the open P3 findings of merged pull requests (#2558).
// The design checklist in every review produces P3 findings, which never block
// a merge and are not seen again once the pull request merges. Each merged pull
// request gets comments, listing its open P3 findings, on the repository's
// issue for the current ISO week; findings of a pull request closed without
// merging are skipped for good, and an open pull request waits for a later
// sweep.
//
// EXACTLY ONCE: a finding is marked filed only after a comment carrying its
// finding-uid marker succeeds. Before posting, the sweep reads the issue's
// comments back, and a finding whose marker is already there (a comment whose
// record was lost, or a post that reported failure but landed) has its record
// repaired instead of being posted again.
//
// Pull requests are processed one at a time, and none is started once ctx is
// done. Failures are per pull request: each is recorded as a design_notes_error
// job event on the review job that last observed a finding, returned in
// Errors, and the sweep continues. Only a failure to read the ledger aborts it.
func (e Engine) SweepDesignNotes(ctx context.Context, opts DesignNotesSweepOptions) (DesignNotesSweepResult, error) {
	result := DesignNotesSweepResult{Next: opts.After}
	if e.Store == nil {
		return result, errors.New("design notes sweep requires a store")
	}
	if e.DesignNotesPullRequest == nil || e.DesignNotesCreateIssue == nil || e.DesignNotesListComments == nil || e.DesignNotesComment == nil {
		return result, errors.New("design notes sweep hooks are not wired")
	}
	now := e.now().UTC()
	s := designNotesSweep{e: e, now: now, week: designNotesWeek(now), result: &result,
		roles: map[string]string{}, issues: map[string]*designNotesWeekIssue{}}
	s.sendPendingNotices(ctx)

	candidates, err := e.Store.ListDesignNoteCandidates(ctx, now.Add(-DesignNotesWindow))
	if err != nil {
		return result, err
	}
	prs := groupDesignNoteCandidates(candidates)
	start := sort.Search(len(prs), func(i int) bool { return prs[i].key.after(opts.After) })
	order := append(append([]designNotesPullRequest{}, prs[start:]...), prs[:start]...)
	limit := opts.Limit
	if limit <= 0 {
		limit = DesignNotesSweepLimit
	}
	if len(order) > limit {
		order = order[:limit]
	}
	for _, pr := range order {
		if err := ctx.Err(); err != nil {
			result.Errors = append(result.Errors, "sweep stopped at its deadline: "+err.Error())
			break
		}
		result.Checked++
		result.Next = pr.key
		s.sweepPullRequest(ctx, pr)
	}
	return result, nil
}

func (s *designNotesSweep) sweepPullRequest(ctx context.Context, pr designNotesPullRequest) {
	state, err := s.e.DesignNotesPullRequest(ctx, pr.key.Repo, int(pr.key.PullRequest))
	if err != nil {
		s.fail(ctx, pr, "pull request state: "+err.Error())
		return
	}
	switch {
	case state.Merged:
		s.file(ctx, pr, firstNonEmptyLedgerText(state.HeadSHA, latestDesignNote(pr.findings).HeadSHA))
	case state.Closed:
		if err := s.record(ctx, pr.findings, "", db.DesignNoteSkipClosedUnmerged); err != nil {
			s.fail(ctx, pr, "record skip: "+err.Error())
			return
		}
		s.result.Skipped += len(pr.findings)
	default:
		s.result.Pending++
	}
}

// file posts a merged pull request's findings to the week issue, minus those
// whose marker the issue already carries, which are only recorded.
func (s *designNotesSweep) file(ctx context.Context, pr designNotesPullRequest, head string) {
	for _, f := range pr.findings {
		if _, err := s.role(ctx, f.ObserverJob); err != nil {
			s.fail(ctx, pr, "resolve requester: "+err.Error())
			return
		}
	}
	issue, err := s.weekIssue(ctx, pr.key.Repo, s.roles[strings.TrimSpace(latestDesignNote(pr.findings).ObserverJob)])
	if err != nil {
		s.fail(ctx, pr, "week issue: "+err.Error())
		return
	}
	var posted, unposted []db.DesignNoteCandidate
	for _, f := range pr.findings {
		if issue.posted[f.FindingUID] {
			posted = append(posted, f)
		} else {
			unposted = append(unposted, f)
		}
	}
	if len(posted) > 0 {
		if err := s.record(ctx, posted, issue.URL, ""); err != nil {
			s.fail(ctx, pr, "repair filing already on "+issue.URL+": "+err.Error())
			return
		}
		s.result.Filed += len(posted)
		s.result.Repaired += len(posted)
	}
	for _, comment := range designNotesComments(pr.key.PullRequest, head, unposted, s.roles) {
		if err := ctx.Err(); err != nil {
			s.fail(ctx, pr, "deadline before commenting on "+issue.URL+": "+err.Error())
			return
		}
		if err := s.e.DesignNotesComment(ctx, pr.key.Repo, issue.Number, comment.body); err != nil {
			// The post may have landed regardless; the next sweep reads the
			// issue's comments back before posting again.
			s.fail(ctx, pr, "comment on "+issue.URL+": "+err.Error())
			return
		}
		for _, f := range comment.findings {
			issue.posted[f.FindingUID] = true
		}
		if err := s.record(ctx, comment.findings, issue.URL, ""); err != nil {
			// Posted but unrecorded: the next sweep finds the markers on the
			// issue and repairs these records without posting again.
			s.fail(ctx, pr, "record filing after comment on "+issue.URL+": "+err.Error())
			return
		}
		s.result.Filed += len(comment.findings)
	}
}

// weekIssue returns the repository's issue for this week: from the store,
// else from a GitHub search by exact title and label (a lost row must not
// open a duplicate), else a new issue, whose role is then told once. A known
// issue's comments are read back for the markers already posted.
func (s *designNotesSweep) weekIssue(ctx context.Context, repo, role string) (*designNotesWeekIssue, error) {
	if issue := s.issues[repo]; issue != nil {
		return issue, nil
	}
	recorded, ok, err := s.e.Store.GetDesignNoteIssue(ctx, repo, s.week)
	if err != nil {
		return nil, err
	}
	issue := &designNotesWeekIssue{posted: map[string]bool{}}
	if ok {
		issue.DesignNotesIssue = DesignNotesIssue{Number: recorded.Number, URL: recorded.URL}
	} else {
		created, err := s.findOrCreateIssue(ctx, repo, role, issue)
		if err != nil {
			return nil, err
		}
		if created {
			// A new issue has no comments to read back.
			s.issues[repo] = issue
			return issue, nil
		}
	}
	bodies, err := s.e.DesignNotesListComments(ctx, repo, issue.Number)
	if err != nil {
		return nil, fmt.Errorf("list comments of %s: %w", issue.URL, err)
	}
	for _, body := range bodies {
		for uid := range designNotesMarkers(body) {
			issue.posted[uid] = true
		}
	}
	s.issues[repo] = issue
	return issue, nil
}

func (s *designNotesSweep) findOrCreateIssue(ctx context.Context, repo, role string, issue *designNotesWeekIssue) (created bool, err error) {
	title := designNotesIssueTitle(repo, s.week)
	if s.e.DesignNotesFindIssue != nil {
		found, exists, err := s.e.DesignNotesFindIssue(ctx, repo, title, designNotesLabel)
		if err != nil {
			return false, fmt.Errorf("search %q: %w", title, err)
		}
		if exists {
			issue.DesignNotesIssue = found
			grace, cancel := designNotesGraceContext(ctx)
			defer cancel()
			if _, err := s.e.Store.RecordDesignNoteIssue(grace, db.DesignNoteIssue{Repo: repo, ISOWeek: s.week, Number: found.Number, URL: found.URL}, s.now); err != nil {
				s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s: record found issue %s: %v", repo, found.URL, err))
			}
			return false, nil
		}
	}
	agentLabel := postMergeUnassignedAgentLabel
	if role != "" {
		agentLabel = "agent:" + role
	}
	opened, err := s.e.DesignNotesCreateIssue(ctx, repo, title, designNotesIssueBody, []string{designNotesLabel, agentLabel})
	if err != nil {
		return false, fmt.Errorf("create %q: %w", title, err)
	}
	issue.DesignNotesIssue = opened
	s.result.Created = append(s.result.Created, opened.URL)
	grace, cancel := designNotesGraceContext(ctx)
	defer cancel()
	if _, err := s.e.Store.RecordDesignNoteIssue(grace, db.DesignNoteIssue{Repo: repo, ISOWeek: s.week, Number: opened.Number, URL: opened.URL, Role: role, Created: true}, s.now); err != nil {
		// The issue exists, so file into it; without the row the next sweep
		// finds it by search instead, and sends no notice rather than risk two.
		s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s: record created issue %s: %v", repo, opened.URL, err))
		return true, nil
	}
	s.notify(grace, repo, opened.URL)
	return true, nil
}

// sendPendingNotices retries the notices of this week's created issues whose
// earlier send failed.
func (s *designNotesSweep) sendPendingNotices(ctx context.Context) {
	pending, err := s.e.Store.ListDesignNoteIssuesAwaitingNotice(ctx, s.week)
	if err != nil {
		s.result.Errors = append(s.result.Errors, "pending notices: "+err.Error())
		return
	}
	for _, issue := range pending {
		s.notify(ctx, issue.Repo, issue.URL)
	}
}

// notify tells the role of the week's created issue, once: the message and
// the notified mark are one store transaction.
func (s *designNotesSweep) notify(ctx context.Context, repo, url string) {
	body := fmt.Sprintf("Design notes for %s this week: %s. Advisory; nothing blocks.", repo, url)
	if _, err := s.e.Store.SendDesignNoteIssueNotice(ctx, repo, s.week, body, s.now); err != nil {
		s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s: notice: %v", repo, err))
	}
}

// role resolves who requested the review job: ReviewRequester, else
// ActingOrgRole (the post-merge P2 rule), else the job's dispatched_by. ""
// means the job resolved with no attribution; a job that cannot be read is an
// error, not an unassigned role, and is not cached.
func (s *designNotesSweep) role(ctx context.Context, jobID string) (string, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return "", nil
	}
	if role, ok := s.roles[jobID]; ok {
		return role, nil
	}
	job, err := s.e.Store.GetJob(ctx, jobID)
	if err != nil {
		return "", fmt.Errorf("review job %s: %w", jobID, err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		return "", fmt.Errorf("review job %s payload: %w", jobID, err)
	}
	role := firstNonEmptyLedgerText(payload.ReviewRequester, payload.ActingOrgRole, job.DispatchedBy)
	s.roles[jobID] = role
	return role, nil
}

// record writes filing rows under a bounded grace context: a comment already
// posted must be recorded even when the sweep's deadline has just passed.
func (s *designNotesSweep) record(ctx context.Context, findings []db.DesignNoteCandidate, issueURL, skipReason string) error {
	filings := make([]db.DesignNoteFiling, 0, len(findings))
	for _, f := range findings {
		filings = append(filings, db.DesignNoteFiling{FindingUID: f.FindingUID, Repo: f.Repo, PullRequest: f.PullRequest,
			HeadSHA: f.HeadSHA, IssueURL: issueURL, SkipReason: skipReason})
	}
	grace, cancel := designNotesGraceContext(ctx)
	defer cancel()
	return s.e.Store.RecordDesignNoteFilings(grace, filings, s.now)
}

func (s *designNotesSweep) fail(ctx context.Context, pr designNotesPullRequest, message string) {
	s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s#%d: %s", pr.key.Repo, pr.key.PullRequest, message))
	if job := strings.TrimSpace(latestDesignNote(pr.findings).ObserverJob); job != "" {
		grace, cancel := designNotesGraceContext(ctx)
		defer cancel()
		_ = s.e.Store.AddJobEvent(grace, db.JobEvent{JobID: job, Kind: designNotesErrorEvent, Message: message})
	}
}

func designNotesGraceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), designNotesGrace)
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
// request, in ledger insertion order; its review job stands for the pull
// request.
func latestDesignNote(findings []db.DesignNoteCandidate) db.DesignNoteCandidate {
	var latest db.DesignNoteCandidate
	for i, f := range findings {
		if i == 0 || f.Seq > latest.Seq {
			latest = f
		}
	}
	return latest
}

func designNotesWeek(now time.Time) string {
	year, week := now.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func designNotesIssueTitle(repo, week string) string {
	return fmt.Sprintf("[design-notes] %s %s", repo, week)
}

// designNotesMarkers returns the finding uids a comment the sweep posted
// carries. Detail text is rendered as a quote, so no reviewer text can forge
// a marker line.
func designNotesMarkers(body string) map[string]bool {
	uids := map[string]bool{}
	if !strings.HasPrefix(strings.TrimSpace(body), designNotesCommentPrefix) {
		return uids
	}
	for _, line := range strings.Split(body, "\n") {
		if uid, ok := strings.CutPrefix(strings.TrimSpace(line), designNotesUIDMarker); ok && strings.TrimSpace(uid) != "" {
			uids[strings.TrimSpace(uid)] = true
		}
	}
	return uids
}

type designNotesCommentPart struct {
	body     string
	findings []db.DesignNoteCandidate
}

// designNotesComments renders a pull request's findings as comments, each
// under designNotesCommentMax bytes in total. Every field of a finding block is
// bounded, so findings that do not fit one comment continue in the next; none
// is dropped.
func designNotesComments(pr int64, head string, findings []db.DesignNoteCandidate, roles map[string]string) []designNotesCommentPart {
	head = designNotesClip(designNotesLine(head), designNotesFieldMax)
	var groups [][]int
	var blocks []string
	size := 0
	for i, f := range findings {
		block := designNotesBlock(i+1, f, pr, head, roles)
		blocks = append(blocks, block)
		if len(groups) == 0 || size+len(block) > designNotesCommentMax-designNotesHeaderReserve {
			groups = append(groups, nil)
			size = 0
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], i)
		size += len(block)
	}
	parts := make([]designNotesCommentPart, 0, len(groups))
	for n, group := range groups {
		var b strings.Builder
		fmt.Fprintf(&b, "%s%d, merged at `%s`", designNotesCommentPrefix, pr, head)
		if len(groups) > 1 {
			fmt.Fprintf(&b, " (part %d of %d)", n+1, len(groups))
		}
		b.WriteString(".\n")
		part := designNotesCommentPart{}
		for _, i := range group {
			b.WriteString(blocks[i])
			part.findings = append(part.findings, findings[i])
		}
		part.body = b.String()
		parts = append(parts, part)
	}
	return parts
}

func designNotesBlock(index int, f db.DesignNoteCandidate, pr int64, head string, roles map[string]string) string {
	detail := strings.TrimSpace(f.Detail)
	title := designNotesClip(designNotesLine(firstNonEmptyLedgerText(f.Title, detail, "(untitled)")), designNotesTitleMax)
	location := strings.TrimSpace(f.File)
	if location != "" && f.Line > 0 {
		location += ":" + strconv.FormatInt(f.Line, 10)
	}
	location = designNotesClip(designNotesLine(firstNonEmptyLedgerText(location, "not stated")), designNotesFieldMax)
	job := designNotesClip(designNotesLine(firstNonEmptyLedgerText(f.ObserverJob, "unknown")), designNotesFieldMax)
	requester := designNotesClip(designNotesLine(firstNonEmptyLedgerText(roles[strings.TrimSpace(f.ObserverJob)], "unassigned")), designNotesFieldMax)
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %d. %s\n\n", index, title)
	fmt.Fprintf(&b, "- Location: `%s`\n", location)
	fmt.Fprintf(&b, "- Review job: `%s` (requested by %s)\n", job, requester)
	fmt.Fprintf(&b, "- Pull request: #%d, merged head `%s`\n", pr, head)
	if detail != "" {
		if len(detail) > designNotesDetailMax {
			detail = fmt.Sprintf("%s\n\n(detail truncated: first %d of %d bytes shown)",
				strings.ToValidUTF8(detail[:designNotesDetailMax], ""), designNotesDetailMax, len(detail))
		}
		b.WriteString("\n> ")
		b.WriteString(strings.ReplaceAll(detail, "\n", "\n> "))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n%s%s\n", designNotesUIDMarker, f.FindingUID)
	return b.String()
}

func designNotesLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func designNotesClip(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return strings.ToValidUTF8(value[:max], "") + "…"
}
