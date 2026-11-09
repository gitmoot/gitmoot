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
	// designNotesGrace bounds a local write that may outlive the sweep's
	// deadline because it records a GitHub action already taken: a created
	// issue or a posted comment. Diagnostics never outlive the deadline.
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

// designNotesSweep is the state of one sweep. markers caches, per
// "repo#issue", the finding uids an issue's comments already carry.
type designNotesSweep struct {
	e       Engine
	now     time.Time
	week    string
	result  *DesignNotesSweepResult
	roles   map[string]string
	issues  map[string]DesignNotesIssue
	markers map[string]map[string]bool
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
// finding-uid marker succeeds. Before commenting on an issue for a pull
// request, the sweep records that issue as a pending post target. Every later
// sweep reads each recorded target's comments back first, and a finding whose
// marker is already on one (a comment whose record was lost, or a post that
// reported failure but landed) has its record repaired against that issue
// instead of being posted again, even when the ISO week has since rolled over
// to a new issue.
//
// DEADLINE: pull requests are processed one at a time, and no pull request,
// comment or diagnostic write is started once ctx is done. The store bounds
// each write's wait for SQLite's write lock by the deadline of the context it
// is given (the driver alone ignores cancellation while busy). The only writes
// allowed past the deadline record a GitHub action that already happened (a
// created issue, a posted comment), each bounded by designNotesGrace, and a
// lost comment record is repaired by the read-back anyway. So the sweep
// returns within its deadline plus one in-flight GitHub call plus
// designNotesGrace.
//
// Failures are per pull request: each is recorded as a design_notes_error job
// event on the review job that last observed a finding (while the deadline
// allows), returned in Errors, and the sweep continues. Only a failure to read
// the ledger aborts it.
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
		roles: map[string]string{}, issues: map[string]DesignNotesIssue{}, markers: map[string]map[string]bool{}}
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

// file files a merged pull request's findings. Findings whose marker is
// already on an issue this pull request was posted to are only recorded; the
// rest are posted to the week issue, after recording it as a target.
func (s *designNotesSweep) file(ctx context.Context, pr designNotesPullRequest, head string) {
	issueRole, err := s.role(ctx, latestDesignNote(pr.findings).ObserverJob)
	if err != nil {
		s.fail(ctx, pr, "resolve requester: "+err.Error())
		return
	}
	requesters := make(map[string]string, len(pr.findings))
	for _, f := range pr.findings {
		role, err := s.role(ctx, f.ObserverJob)
		if err != nil {
			s.fail(ctx, pr, "resolve requester: "+err.Error())
			return
		}
		requesters[f.FindingUID] = role
	}

	remaining := pr.findings
	targets, err := s.e.Store.ListDesignNotePendingPosts(ctx, pr.key.Repo, pr.key.PullRequest)
	if err != nil {
		s.fail(ctx, pr, "pending posts: "+err.Error())
		return
	}
	for _, target := range targets {
		var ok bool
		if remaining, ok = s.reconcile(ctx, pr, DesignNotesIssue{Number: target.IssueNumber, URL: target.IssueURL}, remaining); !ok {
			return
		}
	}
	if len(remaining) == 0 {
		return
	}
	issue, err := s.weekIssue(ctx, pr.key.Repo, issueRole)
	if err != nil {
		s.fail(ctx, pr, "week issue: "+err.Error())
		return
	}
	var ok bool
	if remaining, ok = s.reconcile(ctx, pr, issue, remaining); !ok || len(remaining) == 0 {
		return
	}
	if err := ctx.Err(); err != nil {
		s.fail(ctx, pr, "deadline before commenting on "+issue.URL+": "+err.Error())
		return
	}
	if err := s.e.Store.RecordDesignNotePendingPost(ctx, db.DesignNotePendingPost{
		Repo: pr.key.Repo, PullRequest: pr.key.PullRequest, IssueNumber: issue.Number, IssueURL: issue.URL}, s.now); err != nil {
		s.fail(ctx, pr, "record target "+issue.URL+" before commenting: "+err.Error())
		return
	}
	posted := s.markers[designNotesIssueKey(pr.key.Repo, issue.Number)]
	for _, comment := range designNotesComments(pr.key.PullRequest, head, remaining, requesters) {
		if err := ctx.Err(); err != nil {
			s.fail(ctx, pr, "deadline before commenting on "+issue.URL+": "+err.Error())
			return
		}
		if err := s.e.DesignNotesComment(ctx, pr.key.Repo, issue.Number, comment.body); err != nil {
			// The post may have landed regardless; the issue is a recorded
			// target, so the next sweep reads it back before posting again.
			s.fail(ctx, pr, "comment on "+issue.URL+": "+err.Error())
			return
		}
		for _, f := range comment.findings {
			posted[f.FindingUID] = true
		}
		grace, cancel := designNotesGraceContext(ctx)
		err := s.record(grace, comment.findings, issue.URL, "")
		cancel()
		if err != nil {
			// Posted but unrecorded: the next sweep finds the markers on the
			// recorded target and repairs these records without posting again.
			s.fail(ctx, pr, "record filing after comment on "+issue.URL+": "+err.Error())
			return
		}
		s.result.Filed += len(comment.findings)
	}
}

// reconcile records, as filed into issue, the findings whose marker the
// issue's comments already carry, and returns the rest. ok is false when the
// issue could not be read or the repair could not be recorded; the pull
// request is then left for the next sweep.
func (s *designNotesSweep) reconcile(ctx context.Context, pr designNotesPullRequest, issue DesignNotesIssue, findings []db.DesignNoteCandidate) (remaining []db.DesignNoteCandidate, ok bool) {
	markers, err := s.issueMarkers(ctx, pr.key.Repo, issue)
	if err != nil {
		s.fail(ctx, pr, err.Error())
		return nil, false
	}
	var posted []db.DesignNoteCandidate
	for _, f := range findings {
		if markers[f.FindingUID] {
			posted = append(posted, f)
		} else {
			remaining = append(remaining, f)
		}
	}
	if len(posted) == 0 {
		return remaining, true
	}
	if err := s.record(ctx, posted, issue.URL, ""); err != nil {
		s.fail(ctx, pr, "repair filing already on "+issue.URL+": "+err.Error())
		return nil, false
	}
	s.result.Filed += len(posted)
	s.result.Repaired += len(posted)
	return remaining, true
}

// issueMarkers returns the finding uids an issue's comments carry, reading
// the comments once per sweep.
func (s *designNotesSweep) issueMarkers(ctx context.Context, repo string, issue DesignNotesIssue) (map[string]bool, error) {
	key := designNotesIssueKey(repo, issue.Number)
	if markers, ok := s.markers[key]; ok {
		return markers, nil
	}
	bodies, err := s.e.DesignNotesListComments(ctx, repo, issue.Number)
	if err != nil {
		return nil, fmt.Errorf("list comments of %s: %w", issue.URL, err)
	}
	markers := map[string]bool{}
	for _, body := range bodies {
		for uid := range designNotesMarkers(body) {
			markers[uid] = true
		}
	}
	s.markers[key] = markers
	return markers, nil
}

// weekIssue returns the repository's issue for this week: from the store,
// else from a GitHub search by exact title and label (a lost row must not
// open a duplicate), else a new issue, whose role is then told once.
func (s *designNotesSweep) weekIssue(ctx context.Context, repo, role string) (DesignNotesIssue, error) {
	if issue, ok := s.issues[repo]; ok {
		return issue, nil
	}
	recorded, ok, err := s.e.Store.GetDesignNoteIssue(ctx, repo, s.week)
	if err != nil {
		return DesignNotesIssue{}, err
	}
	issue := DesignNotesIssue{Number: recorded.Number, URL: recorded.URL}
	if !ok {
		if issue, err = s.findOrCreateIssue(ctx, repo, role); err != nil {
			return DesignNotesIssue{}, err
		}
	}
	s.issues[repo] = issue
	return issue, nil
}

func (s *designNotesSweep) findOrCreateIssue(ctx context.Context, repo, role string) (DesignNotesIssue, error) {
	title := designNotesIssueTitle(repo, s.week)
	if s.e.DesignNotesFindIssue != nil {
		found, exists, err := s.e.DesignNotesFindIssue(ctx, repo, title, designNotesLabel)
		if err != nil {
			return DesignNotesIssue{}, fmt.Errorf("search %q: %w", title, err)
		}
		if exists {
			// Not recording it only costs the next sweep another search.
			if _, err := s.e.Store.RecordDesignNoteIssue(ctx, db.DesignNoteIssue{Repo: repo, ISOWeek: s.week, Number: found.Number, URL: found.URL}, s.now); err != nil {
				s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s: record found issue %s: %v", repo, found.URL, err))
			}
			return found, nil
		}
	}
	agentLabel := postMergeUnassignedAgentLabel
	if role != "" {
		agentLabel = "agent:" + role
	}
	opened, err := s.e.DesignNotesCreateIssue(ctx, repo, title, designNotesIssueBody, []string{designNotesLabel, agentLabel})
	if err != nil {
		return DesignNotesIssue{}, fmt.Errorf("create %q: %w", title, err)
	}
	s.result.Created = append(s.result.Created, opened.URL)
	// A new issue has no comments to read back.
	s.markers[designNotesIssueKey(repo, opened.Number)] = map[string]bool{}
	grace, cancel := designNotesGraceContext(ctx)
	_, err = s.e.Store.RecordDesignNoteIssue(grace, db.DesignNoteIssue{Repo: repo, ISOWeek: s.week, Number: opened.Number, URL: opened.URL, Role: role, Created: true}, s.now)
	cancel()
	if err != nil {
		// The issue exists, so file into it; without the row the next sweep
		// finds it by search instead, and sends no notice rather than risk two.
		s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s: record created issue %s: %v", repo, opened.URL, err))
		return opened, nil
	}
	// Bounded by the sweep's deadline: an unsent notice stays pending and is
	// retried by the next sweep.
	s.notify(ctx, repo, opened.URL)
	return opened, nil
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

// record writes filing rows; the store bounds the wait for the write lock by
// ctx's deadline.
func (s *designNotesSweep) record(ctx context.Context, findings []db.DesignNoteCandidate, issueURL, skipReason string) error {
	filings := make([]db.DesignNoteFiling, 0, len(findings))
	for _, f := range findings {
		filings = append(filings, db.DesignNoteFiling{FindingUID: f.FindingUID, Repo: f.Repo, PullRequest: f.PullRequest,
			HeadSHA: f.HeadSHA, IssueURL: issueURL, SkipReason: skipReason})
	}
	return s.e.Store.RecordDesignNoteFilings(ctx, filings, s.now)
}

// fail reports a per-pull-request failure. The job event is a diagnostic, so
// it is written only while the sweep's deadline allows, and bounded by it.
func (s *designNotesSweep) fail(ctx context.Context, pr designNotesPullRequest, message string) {
	s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s#%d: %s", pr.key.Repo, pr.key.PullRequest, message))
	if ctx.Err() != nil {
		return
	}
	if job := strings.TrimSpace(latestDesignNote(pr.findings).ObserverJob); job != "" {
		_ = s.e.Store.AddDesignNoteJobEvent(ctx, db.JobEvent{JobID: job, Kind: designNotesErrorEvent, Message: message})
	}
}

// designNotesGraceContext bounds a write that records a GitHub action already
// taken. The store enforces the bound on its wait for the write lock.
func designNotesGraceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), designNotesGrace)
}

func designNotesIssueKey(repo string, issue int64) string {
	return repo + "#" + strconv.FormatInt(issue, 10)
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
func designNotesComments(pr int64, head string, findings []db.DesignNoteCandidate, requesters map[string]string) []designNotesCommentPart {
	head = designNotesClip(designNotesLine(head), designNotesFieldMax)
	var groups [][]int
	var blocks []string
	size := 0
	for i, f := range findings {
		block := designNotesBlock(i+1, f, pr, head, requesters[f.FindingUID])
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

func designNotesBlock(index int, f db.DesignNoteCandidate, pr int64, head string, requester string) string {
	detail := strings.TrimSpace(f.Detail)
	title := designNotesClip(designNotesLine(firstNonEmptyLedgerText(f.Title, detail, "(untitled)")), designNotesTitleMax)
	location := strings.TrimSpace(f.File)
	if location != "" && f.Line > 0 {
		location += ":" + strconv.FormatInt(f.Line, 10)
	}
	location = designNotesClip(designNotesLine(firstNonEmptyLedgerText(location, "not stated")), designNotesFieldMax)
	job := designNotesClip(designNotesLine(firstNonEmptyLedgerText(f.ObserverJob, "unknown")), designNotesFieldMax)
	requester = designNotesClip(designNotesLine(firstNonEmptyLedgerText(requester, "unassigned")), designNotesFieldMax)
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
