package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
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
	designNotesIssueBody     = "Design notes are advisory P3 findings from the review design checklist that were still open when their pull request merged. " +
		"The notes are the comments below, one per merged pull request. " +
		"Each note may already be fixed on the default branch, so check the current code before acting on it. " +
		"Pick up any worth doing, and close this issue once every note is triaged.\n"
	// designNotesLineSlack widens a finding's cited line when the sweep checks
	// whether the default branch rewrote it after the merge.
	designNotesLineSlack = 3
)

// DesignNotesPullRequestState is what GitHub reports for a candidate pull
// request. Closed means closed WITHOUT merging. HeadSHA is the merged head,
// MergeCommitSHA the commit the merge put on the base branch (for a squash
// merge, the squashed commit), and DefaultBranch the repository's default
// branch, each when known.
type DesignNotesPullRequestState struct {
	Merged         bool
	Closed         bool
	HeadSHA        string
	MergeCommitSHA string
	// Repo is the repository's current owner/name on GitHub, "" when unknown.
	Repo          string
	DefaultBranch string
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
// counts findings recorded without a comment (their pull request closed
// unmerged, or the default branch removed or rewrote what they cite since the
// merge); Pending counts pull requests still open. Next is the last pull
// request this sweep attempted, or the sweep's After when it attempted none.
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
	// github is the repository's current name on GitHub, which every GitHub
	// call uses (#2569); key.Repo stays the ledger's name for the database.
	github string
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
// finding-uid marker succeeds. Before commenting on an issue, the sweep records
// one attempt per finding about to be posted. An attempt concerns only its own
// finding: it is retired when that finding is filed (in the filing
// transaction), when a read-back of its issue shows the finding absent, or
// when the finding ages out of the window. A later sweep reads back the
// attempts of the pull request's current candidates, grouped by issue, and a
// finding whose marker is already there (a comment whose record was lost, or a
// post that reported failure but landed) has its record repaired against that
// issue instead of being posted again, even when the ISO week has since rolled
// over to a new issue. An attempted issue that cannot be read holds back only
// the findings attempted there until it can be.
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
// NOT WORTH POSTING: a note can be settled or outdated by the time it is
// filed. A finding whose reviewer declared it answered or withdrawn (the
// ledger still records it open when the evidence was weak, for the merge
// gate) is recorded as skipped. So is one the default branch outdated: before
// posting, the sweep compares the pull request's merge commit with the
// repository's default branch, once per pull request (DesignNotesCompare),
// and a finding whose file was removed or renamed away since, or whose cited
// lines (within designNotesLineSlack) a later change rewrote, is never
// posted; one whose file changed elsewhere is posted with a "Changed since
// merge" line. Notes are advisory, so the compare fails open: with no merge
// commit, no default branch, a failed compare or a truncated one, every
// finding is posted as if nothing changed.
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
	// Housekeeping last, and only within the deadline: an attempt is deleted
	// unread only once its finding can never be filed again (recorded, or
	// first seen before the window).
	if ctx.Err() == nil {
		if err := e.Store.RetireAgedDesignNotePendingPosts(ctx, now.Add(-DesignNotesWindow)); err != nil {
			result.Errors = append(result.Errors, "retire aged attempts: "+err.Error())
		}
	}
	return result, nil
}

func (s *designNotesSweep) sweepPullRequest(ctx context.Context, pr designNotesPullRequest) {
	state, err := s.e.DesignNotesPullRequest(ctx, pr.key.Repo, int(pr.key.PullRequest))
	if err != nil {
		s.fail(ctx, pr, "pull request state: "+err.Error())
		return
	}
	pr.github = pr.key.Repo
	if current := strings.TrimSpace(state.Repo); current != "" {
		pr.github = current
	}
	switch {
	case state.Merged:
		s.file(ctx, pr, state)
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

// file files a merged pull request's findings. First it reads back the issues
// its current findings were attempted on: findings whose marker is there are
// recorded, attempts shown absent are retired. An attempted issue that cannot
// be read holds back only the findings attempted there. The rest are screened
// (screen), and those still worth posting go to the week issue, after
// recording one attempt per finding.
func (s *designNotesSweep) file(ctx context.Context, pr designNotesPullRequest, state DesignNotesPullRequestState) {
	head := firstNonEmptyLedgerText(state.HeadSHA, latestDesignNote(pr.findings).HeadSHA)
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
	attempts, err := s.e.Store.ListDesignNotePendingPosts(ctx, pr.key.Repo, pr.key.PullRequest)
	if err != nil {
		s.fail(ctx, pr, "pending posts: "+err.Error())
		return
	}
	held := map[string]bool{}
	for _, group := range groupDesignNoteAttempts(attempts, pr.findings) {
		var ok bool
		if remaining, ok = s.readBack(ctx, pr, group, remaining, held); !ok {
			return
		}
	}
	if len(held) > 0 {
		var free []db.DesignNoteCandidate
		for _, f := range remaining {
			if !held[f.FindingUID] {
				free = append(free, f)
			}
		}
		remaining = free
	}
	if len(remaining) == 0 {
		return
	}
	screened, ok := s.screen(ctx, pr, state, remaining)
	if !ok || len(screened.post) == 0 {
		return
	}
	remaining = screened.post
	issue, err := s.weekIssue(ctx, pr.github, pr.key.Repo, issueRole)
	if err != nil {
		s.fail(ctx, pr, "week issue: "+err.Error())
		return
	}
	if remaining, ok = s.reconcile(ctx, pr, issue, remaining); !ok || len(remaining) == 0 {
		return
	}
	if err := ctx.Err(); err != nil {
		s.fail(ctx, pr, "deadline before commenting on "+issue.URL+": "+err.Error())
		return
	}
	uids := make([]string, 0, len(remaining))
	for _, f := range remaining {
		uids = append(uids, f.FindingUID)
	}
	if err := s.e.Store.RecordDesignNotePendingPosts(ctx, pr.key.Repo, pr.key.PullRequest, issue.Number, issue.URL, uids, s.now); err != nil {
		s.fail(ctx, pr, "record attempts on "+issue.URL+" before commenting: "+err.Error())
		return
	}
	posted := s.markers[designNotesIssueKey(pr.github, issue.Number)]
	for _, comment := range designNotesComments(pr.key.PullRequest, head, remaining, requesters, screened.branch, screened.changed) {
		if err := ctx.Err(); err != nil {
			s.fail(ctx, pr, "deadline before commenting on "+issue.URL+": "+err.Error())
			return
		}
		if err := s.e.DesignNotesComment(ctx, pr.github, issue.Number, comment.body); err != nil {
			// The post may have landed regardless; the attempts are recorded,
			// so the next sweep reads the issue back before posting again.
			s.fail(ctx, pr, "comment on "+issue.URL+": "+err.Error())
			return
		}
		for _, f := range comment.findings {
			posted[f.FindingUID] = true
		}
		// Filing these findings retires exactly their attempts, in the same
		// transaction; other findings' attempts on this issue are untouched.
		grace, cancel := designNotesGraceContext(ctx)
		err := s.record(grace, comment.findings, issue.URL, "")
		cancel()
		if err != nil {
			// Posted but unrecorded: the attempts stay, so the next sweep finds
			// the markers and repairs these records without posting again.
			s.fail(ctx, pr, "record filing after comment on "+issue.URL+": "+err.Error())
			return
		}
		s.result.Filed += len(comment.findings)
	}
}

// designNotesScreen is screen's verdict on the findings about to be posted:
// those still to post, the default branch they were checked against, and the
// uids whose file that branch changed elsewhere since the merge.
type designNotesScreen struct {
	post    []db.DesignNoteCandidate
	branch  string
	changed map[string]bool
}

// screen records as skipped, for good, the findings not worth posting, in one
// transaction: those whose reviewer declared them resolved (designNotesResolved)
// and those the default branch outdated since the merge. For the second it
// compares the pull request's merge commit with the repository's default
// branch, in one call (compare). The compare starts at the merge commit, not
// the pull request's head: after a squash merge the head is no ancestor of the
// default branch, so a three-dot compare from it would list the pull request's
// own changes. In any doubt (no merge commit or default branch, a failed or
// truncated compare, a missing patch, an unreadable hunk header) a finding is
// posted as if nothing changed. ok is false only when the skips could not be
// recorded; the pull request then waits for a later sweep.
func (s *designNotesSweep) screen(ctx context.Context, pr designNotesPullRequest, state DesignNotesPullRequestState, findings []db.DesignNoteCandidate) (verdict designNotesScreen, ok bool) {
	var skips []db.DesignNoteFiling
	var live []db.DesignNoteCandidate
	for _, f := range findings {
		if designNotesResolved(f) {
			skips = append(skips, s.filings([]db.DesignNoteCandidate{f}, "", db.DesignNoteSkipDeclaredResolved)...)
		} else {
			live = append(live, f)
		}
	}
	verdict = designNotesScreen{post: live}
	if compare, branch, compared := s.compare(ctx, pr, state, live); compared {
		files := make(map[string]github.PullRequestFile, len(compare.Files))
		renamedAway := map[string]bool{}
		for _, file := range compare.Files {
			files[designNotesPath(file.Filename)] = file
			if file.Status == "renamed" {
				renamedAway[designNotesPath(file.PreviousFilename)] = true
			}
		}
		verdict = designNotesScreen{branch: branch, changed: map[string]bool{}}
		for _, f := range live {
			skip, changed := designNotesStaleFinding(f, files, renamedAway)
			if skip != "" {
				skips = append(skips, s.filings([]db.DesignNoteCandidate{f}, "", skip)...)
				continue
			}
			verdict.post = append(verdict.post, f)
			verdict.changed[f.FindingUID] = changed
		}
	}
	if err := s.e.Store.RecordDesignNoteFilings(ctx, skips, s.now); err != nil {
		s.fail(ctx, pr, "record skips of findings not worth posting: "+err.Error())
		return designNotesScreen{}, false
	}
	s.result.Skipped += len(skips)
	return verdict, true
}

// compare returns the compare of the pull request's merge commit with the
// default branch. A sweep files a pull request once, so this is one GitHub
// call per pull request per sweep. ok is false when there is nothing to check
// (no finding cites a file) or nothing to check against: no hook, no merge
// commit or default branch, a failed compare (reported in Errors) or a
// truncated one.
func (s *designNotesSweep) compare(ctx context.Context, pr designNotesPullRequest, state DesignNotesPullRequestState, findings []db.DesignNoteCandidate) (result github.CompareResult, branch string, ok bool) {
	mergeCommit := strings.TrimSpace(state.MergeCommitSHA)
	branch = strings.TrimSpace(state.DefaultBranch)
	if s.e.DesignNotesCompare == nil || mergeCommit == "" || branch == "" || !designNotesCiteFiles(findings) {
		return github.CompareResult{}, "", false
	}
	result, err := s.e.DesignNotesCompare(ctx, pr.github, mergeCommit, branch)
	if err != nil {
		s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s#%d: compare %s...%s: %v; filing without the staleness check",
			pr.key.Repo, pr.key.PullRequest, mergeCommit, branch, err))
		return github.CompareResult{}, "", false
	}
	if result.Truncated {
		return github.CompareResult{}, "", false
	}
	return result, branch, true
}

// designNotesResolvedTitle matches the titles reviewers give a confirmation
// rather than a concern: "Resolved: ...", "Addressed at <sha>: ...", "R3
// addressed: ...", "R8 remains addressed: ...". Measured on the ledger
// (#2566); "R5 remains unresolved: ..." is a concern and does not match.
var designNotesResolvedTitle = regexp.MustCompile(`(?i)^(?:resolved|addressed|r\d+\s+(?:remains\s+)?(?:resolved|addressed))\b`)

// designNotesResolved reports whether the reviewer declared a finding's latest
// observation resolved, though the ledger recorded it open: the writer records
// a declared answer or withdrawal with weak evidence as open, for the merge
// gate. A row from before declared_state existed that the migration could not
// backfill has no declared state, and its title stands in.
func designNotesResolved(f db.DesignNoteCandidate) bool {
	switch f.DeclaredState {
	case db.FindingAnswered, db.FindingWithdrawn:
		return true
	case "":
		return designNotesResolvedTitle.MatchString(strings.TrimSpace(f.Title))
	default:
		return false
	}
}

// designNotesStaleFinding classifies one finding against the compare of its
// pull request's merge commit with the default branch: a skip reason, or
// whether its file changed there (elsewhere than its cited lines).
func designNotesStaleFinding(f db.DesignNoteCandidate, files map[string]github.PullRequestFile, renamedAway map[string]bool) (skip string, changed bool) {
	path := designNotesPath(f.File)
	if path == "" {
		return "", false
	}
	file, listed := files[path]
	if (listed && file.Status == "removed") || (!listed && renamedAway[path]) {
		return db.DesignNoteSkipFileRemoved, false
	}
	if !listed || file.Patch == "" {
		// Unchanged, or changed with no patch to read: GitHub omits it for
		// binary and oversized diffs.
		return "", false
	}
	if f.Line > 0 {
		touched, ok := designNotesHunksTouch(file.Patch, f.Line)
		if !ok {
			return "", false
		}
		if touched {
			return db.DesignNoteSkipLinesChanged, false
		}
	}
	return "", true
}

// designNotesHunksTouch reports whether a hunk of a unified-diff patch covers,
// on its old side, any line within designNotesLineSlack of line. A header
// reads "@@ -start,count +start,count @@", a count omitted meaning 1; its old
// range includes the hunk's context lines. A count of 0 (a pure insertion,
// with no context) sits between old lines start and start+1. ok is false when
// a header cannot be read.
func designNotesHunksTouch(patch string, line int64) (touched, ok bool) {
	for _, row := range strings.Split(patch, "\n") {
		header, isHeader := strings.CutPrefix(row, "@@ -")
		if !isHeader {
			continue
		}
		old, _, found := strings.Cut(header, " ")
		if !found {
			return false, false
		}
		startText, countText, hasCount := strings.Cut(old, ",")
		start, err := strconv.ParseInt(startText, 10, 64)
		if err != nil {
			return false, false
		}
		count := int64(1)
		if hasCount {
			if count, err = strconv.ParseInt(countText, 10, 64); err != nil {
				return false, false
			}
		}
		last := start + count - 1
		if count == 0 {
			last = start + 1
		}
		if start <= line+designNotesLineSlack && last >= line-designNotesLineSlack {
			return true, true
		}
	}
	return false, true
}

func designNotesCiteFiles(findings []db.DesignNoteCandidate) bool {
	for _, f := range findings {
		if designNotesPath(f.File) != "" {
			return true
		}
	}
	return false
}

// designNotesPath normalizes a repository-relative path as a finding cites it
// and as GitHub lists it.
func designNotesPath(path string) string {
	return strings.TrimPrefix(strings.TrimSpace(path), "./")
}

// designNotesAttemptGroup is one attempted issue and the current candidates
// attempted on it.
type designNotesAttemptGroup struct {
	issue DesignNotesIssue
	uids  map[string]bool
}

// groupDesignNoteAttempts groups the attempts of the pull request's current
// candidates by issue, in the order the issues were first attempted. Attempts
// of findings that are not candidates now (answered, for example) are left
// alone: they are neither read nor retired here.
func groupDesignNoteAttempts(attempts []db.DesignNotePendingPost, candidates []db.DesignNoteCandidate) []designNotesAttemptGroup {
	isCandidate := make(map[string]bool, len(candidates))
	for _, f := range candidates {
		isCandidate[f.FindingUID] = true
	}
	var groups []designNotesAttemptGroup
	index := map[int64]int{}
	for _, a := range attempts {
		if !isCandidate[a.FindingUID] {
			continue
		}
		i, ok := index[a.IssueNumber]
		if !ok {
			i = len(groups)
			index[a.IssueNumber] = i
			groups = append(groups, designNotesAttemptGroup{issue: DesignNotesIssue{Number: a.IssueNumber, URL: a.IssueURL}, uids: map[string]bool{}})
		}
		groups[i].uids[a.FindingUID] = true
	}
	return groups
}

// readBack reads one attempted issue back for the findings attempted there.
// Those whose marker it carries are filed against it and the attempts of those
// it lacks are retired, in one transaction. An unreadable issue (deleted, 404,
// a GitHub error) is reported, its attempts are kept, and its findings are
// added to held, so they wait until it is readable while the others go on. ok
// is false only when the outcome could not be recorded; the pull request then
// waits for a later sweep.
func (s *designNotesSweep) readBack(ctx context.Context, pr designNotesPullRequest, group designNotesAttemptGroup, findings []db.DesignNoteCandidate, held map[string]bool) (remaining []db.DesignNoteCandidate, ok bool) {
	var relevant int
	for _, f := range findings {
		if group.uids[f.FindingUID] {
			relevant++
		}
	}
	if relevant == 0 {
		// Every finding attempted here was already resolved by an earlier group.
		return findings, true
	}
	markers, err := s.issueMarkers(ctx, pr.github, group.issue)
	if err != nil {
		s.fail(ctx, pr, fmt.Sprintf("%v; holding back the %d finding(s) attempted there until it can be read", err, relevant))
		for uid := range group.uids {
			held[uid] = true
		}
		return findings, true
	}
	var found []db.DesignNoteCandidate
	var absent []string
	for _, f := range findings {
		switch {
		case !group.uids[f.FindingUID]:
			remaining = append(remaining, f)
		case markers[f.FindingUID]:
			found = append(found, f)
		default:
			absent = append(absent, f.FindingUID)
			remaining = append(remaining, f)
		}
	}
	if err := s.e.Store.RecordDesignNoteReadBack(ctx, group.issue.Number, s.filings(found, group.issue.URL, ""), absent, s.now); err != nil {
		s.fail(ctx, pr, "record read-back of "+group.issue.URL+": "+err.Error())
		return nil, false
	}
	s.result.Filed += len(found)
	s.result.Repaired += len(found)
	return remaining, true
}

// reconcile records, as filed into issue, the findings whose marker the
// issue's comments already carry, and returns the rest. ok is false when the
// issue could not be read or the repair could not be recorded; the pull
// request is then left for the next sweep.
func (s *designNotesSweep) reconcile(ctx context.Context, pr designNotesPullRequest, issue DesignNotesIssue, findings []db.DesignNoteCandidate) (remaining []db.DesignNoteCandidate, ok bool) {
	markers, err := s.issueMarkers(ctx, pr.github, issue)
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
//
// repo is the repository's current name on GitHub; ledger is the name its
// findings carry. They differ after a transfer or rename (#2569), and an issue
// opened earlier this week under the old name is still this week's issue: it
// is looked up by that name too, in the store and by its old title, before a
// new one is opened.
func (s *designNotesSweep) weekIssue(ctx context.Context, repo, ledger, role string) (DesignNotesIssue, error) {
	if issue, ok := s.issues[repo]; ok {
		return issue, nil
	}
	names := []string{repo}
	if ledger != "" && ledger != repo {
		names = append(names, ledger)
	}
	for i, name := range names {
		recorded, ok, err := s.e.Store.GetDesignNoteIssue(ctx, name, s.week)
		if err != nil {
			return DesignNotesIssue{}, err
		}
		if !ok {
			continue
		}
		issue := DesignNotesIssue{Number: recorded.Number, URL: recorded.URL}
		if i > 0 {
			s.recordFoundIssue(ctx, repo, issue)
		}
		s.issues[repo] = issue
		return issue, nil
	}
	issue, err := s.findOrCreateIssue(ctx, repo, names, role)
	if err != nil {
		return DesignNotesIssue{}, err
	}
	s.issues[repo] = issue
	return issue, nil
}

// recordFoundIssue records an existing week issue under repo. It is not a
// created issue, so no notice is sent for it.
func (s *designNotesSweep) recordFoundIssue(ctx context.Context, repo string, issue DesignNotesIssue) {
	// Not recording it only costs the next sweep another lookup.
	if _, err := s.e.Store.RecordDesignNoteIssue(ctx, db.DesignNoteIssue{Repo: repo, ISOWeek: s.week, Number: issue.Number, URL: issue.URL}, s.now); err != nil {
		s.result.Errors = append(s.result.Errors, fmt.Sprintf("%s: record found issue %s: %v", repo, issue.URL, err))
	}
}

// findOrCreateIssue searches repo (the current name, the only one GitHub
// search accepts) for this week's issue under each of names' titles, and
// opens one titled for repo when none exists.
func (s *designNotesSweep) findOrCreateIssue(ctx context.Context, repo string, names []string, role string) (DesignNotesIssue, error) {
	title := designNotesIssueTitle(repo, s.week)
	if s.e.DesignNotesFindIssue != nil {
		for _, name := range names {
			searched := designNotesIssueTitle(name, s.week)
			found, exists, err := s.e.DesignNotesFindIssue(ctx, repo, searched)
			if err != nil {
				return DesignNotesIssue{}, fmt.Errorf("search %q: %w", searched, err)
			}
			if exists {
				s.recordFoundIssue(ctx, repo, found)
				return found, nil
			}
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
	return s.e.Store.RecordDesignNoteFilings(ctx, s.filings(findings, issueURL, skipReason), s.now)
}

func (s *designNotesSweep) filings(findings []db.DesignNoteCandidate, issueURL, skipReason string) []db.DesignNoteFiling {
	filings := make([]db.DesignNoteFiling, 0, len(findings))
	for _, f := range findings {
		filings = append(filings, db.DesignNoteFiling{FindingUID: f.FindingUID, Repo: f.Repo, PullRequest: f.PullRequest,
			HeadSHA: f.HeadSHA, IssueURL: issueURL, SkipReason: skipReason})
	}
	return filings
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
// is dropped. A finding in changed gets a line saying its file was modified on
// branch after the merge.
func designNotesComments(pr int64, head string, findings []db.DesignNoteCandidate, requesters map[string]string, branch string, changed map[string]bool) []designNotesCommentPart {
	head = designNotesClip(designNotesLine(head), designNotesFieldMax)
	branch = designNotesClip(designNotesLine(branch), designNotesFieldMax)
	var groups [][]int
	var blocks []string
	size := 0
	for i, f := range findings {
		changedOn := ""
		if changed[f.FindingUID] {
			changedOn = branch
		}
		block := designNotesBlock(i+1, f, pr, head, requesters[f.FindingUID], changedOn)
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

func designNotesBlock(index int, f db.DesignNoteCandidate, pr int64, head, requester, changedOn string) string {
	detail := strings.TrimSpace(f.Detail)
	heading := designNotesLine(firstNonEmptyLedgerText(f.Title, detail, "(untitled)"))
	title := designNotesClip(heading, designNotesTitleMax)
	if fullTitle := strings.TrimSpace(f.Title); fullTitle != "" && title != heading && designNotesLocatorOnly(detail, f.File) {
		// The heading cut the title, and no detail says the rest: the
		// reviewer's whole text is in the title, so it becomes the quote,
		// with the bare locator, if any, kept under it.
		detail = strings.TrimSpace(fullTitle + "\n\n" + detail)
	}
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
	if changedOn != "" {
		path := designNotesClip(designNotesLine(f.File), designNotesFieldMax)
		fmt.Fprintf(&b, "- Changed since merge: `%s` was modified on `%s` after this PR merged; check the current code first.\n", path, changedOn)
	}
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

// designNotesLocatorOnly reports whether a finding's detail says nothing
// beyond where to look: empty, or one short line that is a locator, or that
// starts with the cited file ("internal/x.go:107-137 (validateConfigFile)").
func designNotesLocatorOnly(detail, file string) bool {
	if detail == "" {
		return true
	}
	if strings.Contains(detail, "\n") || len(detail) > designNotesFieldMax {
		return false
	}
	if db.IsStructuralFindingLocator(detail) {
		return true
	}
	file = designNotesPath(file)
	return file != "" && strings.HasPrefix(designNotesPath(detail), file)
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
