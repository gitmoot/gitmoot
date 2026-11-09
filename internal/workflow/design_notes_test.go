package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

const (
	designNotesRepo    = "owner/repo"
	designNotesLedgerA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	designNotesLedgerB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	designNotesMerged  = "cccccccccccccccccccccccccccccccccccccccc"
)

// Monday 2026-11-09 is in ISO week 2026-W46.
var designNotesNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type postedDesignNote struct {
	repo  string
	issue int64
	body  string
}

type designNotesFake struct {
	states       map[int]DesignNotesPullRequestState
	stateCalls   map[int]int
	found        *DesignNotesIssue
	searches     int
	created      []string
	createLabels [][]string
	createBody   string
	comments     []postedDesignNote
	commentErr   map[int]error
	// landed marks a failing PR's comment as posted anyway: the API reported
	// an error for a write that happened.
	landed    map[int]bool
	listCalls int
	onState   func(number int)
	onComment func(number int)
}

func newDesignNotesFake() *designNotesFake {
	return &designNotesFake{states: map[int]DesignNotesPullRequestState{}, stateCalls: map[int]int{},
		commentErr: map[int]error{}, landed: map[int]bool{}}
}

func (f *designNotesFake) wire(e *Engine) {
	e.Now = func() time.Time { return designNotesNow }
	e.DesignNotesPullRequest = func(_ context.Context, repo string, number int) (DesignNotesPullRequestState, error) {
		f.stateCalls[number]++
		if f.onState != nil {
			f.onState(number)
		}
		state, ok := f.states[number]
		if !ok {
			return DesignNotesPullRequestState{}, fmt.Errorf("no state for %s#%d", repo, number)
		}
		return state, nil
	}
	e.DesignNotesFindIssue = func(_ context.Context, _, _, label string) (DesignNotesIssue, bool, error) {
		f.searches++
		if label != designNotesLabel {
			return DesignNotesIssue{}, false, fmt.Errorf("searched label %q", label)
		}
		if f.found != nil {
			return *f.found, true, nil
		}
		return DesignNotesIssue{}, false, nil
	}
	e.DesignNotesCreateIssue = func(_ context.Context, _, title, body string, labels []string) (DesignNotesIssue, error) {
		f.created = append(f.created, title)
		f.createLabels = append(f.createLabels, labels)
		f.createBody = body
		return DesignNotesIssue{Number: 500 + int64(len(f.created)), URL: fmt.Sprintf("https://github.com/owner/repo/issues/%d", 500+len(f.created))}, nil
	}
	e.DesignNotesListComments = func(_ context.Context, repo string, issue int64) ([]string, error) {
		f.listCalls++
		var bodies []string
		for _, c := range f.comments {
			if c.repo == repo && c.issue == issue {
				bodies = append(bodies, c.body)
			}
		}
		return bodies, nil
	}
	e.DesignNotesComment = func(_ context.Context, repo string, issue int64, body string) error {
		for pr, err := range f.commentErr {
			if strings.Contains(body, fmt.Sprintf("from #%d,", pr)) {
				if f.landed[pr] {
					f.comments = append(f.comments, postedDesignNote{repo: repo, issue: issue, body: body})
				}
				return err
			}
		}
		f.comments = append(f.comments, postedDesignNote{repo: repo, issue: issue, body: body})
		if f.onComment != nil {
			for pr := range f.states {
				if strings.Contains(body, fmt.Sprintf("from #%d,", pr)) {
					f.onComment(pr)
				}
			}
		}
		return nil
	}
}

// commentsFor returns every comment posted for a PR.
func (f *designNotesFake) commentsFor(pr int) []string {
	var found []string
	for _, c := range f.comments {
		if strings.Contains(c.body, fmt.Sprintf("from #%d,", pr)) {
			found = append(found, c.body)
		}
	}
	return found
}

func (f *designNotesFake) commentFor(t *testing.T, pr int) string {
	t.Helper()
	var found []string
	for _, c := range f.comments {
		if strings.Contains(c.body, fmt.Sprintf("from #%d,", pr)) {
			found = append(found, c.body)
		}
	}
	if len(found) != 1 {
		t.Fatalf("PR #%d has %d comments, want exactly 1: %q", pr, len(found), found)
	}
	return found[0]
}

func designNotesEngine(t *testing.T) (Engine, *db.Store, *designNotesFake) {
	t.Helper()
	store := openEngineStore(t)
	fake := newDesignNotesFake()
	engine := Engine{Store: store}
	fake.wire(&engine)
	return engine, store, fake
}

func seedDesignNotesReviewJob(t *testing.T, store *db.Store, id, payload string) {
	t.Helper()
	if err := store.CreateJob(context.Background(), db.Job{ID: id, Agent: "reviewer", Type: "review", State: "succeeded", Payload: payload}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

type designNoteObs struct {
	pr        int64
	job       string
	state     db.FindingState
	severity  string
	title     string
	continues string
	head      string
	ago       time.Duration
	detail    string
	file      string
}

func recordDesignNote(t *testing.T, store *db.Store, o designNoteObs) string {
	t.Helper()
	head := o.head
	if head == "" {
		head = designNotesLedgerA
	}
	detail := o.detail
	if detail == "" {
		detail = "detail of " + o.title
	}
	file := o.file
	if file == "" {
		file = "internal/x.go"
	}
	obs := db.ReviewFindingObservation{
		Repo: designNotesRepo, PullRequest: o.pr, HeadSHA: head, State: o.state, Severity: o.severity,
		ContinuesUID: o.continues, ObserverJob: o.job, Title: o.title, Detail: detail,
		File: file, Line: 42, EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test ./internal/x/"}, ExecutedCount: 1,
		ObservedAt: designNotesNow.Add(-o.ago).Format(time.RFC3339),
	}
	if o.state == db.FindingWithdrawn {
		obs.WithdrawReason = "not a defect"
	}
	uid, err := store.RecordReviewFindingObservation(context.Background(), obs)
	if err != nil {
		t.Fatalf("RecordReviewFindingObservation(%+v): %v", o, err)
	}
	return uid
}

func designNoteFiled(t *testing.T, store *db.Store) map[string]bool {
	t.Helper()
	candidates, err := store.ListDesignNoteCandidates(context.Background(), designNotesNow.Add(-DesignNotesWindow))
	if err != nil {
		t.Fatalf("ListDesignNoteCandidates: %v", err)
	}
	pending := map[string]bool{}
	for _, c := range candidates {
		pending[c.FindingUID] = true
	}
	return pending
}

func TestDesignNotesSweepFilesMergedOpenP3sOnceIntoTheWeekIssue(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-7", `{"repo":"owner/repo","review_requester":"lead"}`)
	seedDesignNotesReviewJob(t, store, "review-8", `{"repo":"owner/repo","acting_org_role":"lead"}`)

	p3a := recordDesignNote(t, store, designNoteObs{pr: 7, job: "review-7", state: db.FindingOpen, severity: "P3", title: "name the seam", ago: 3 * time.Hour})
	p3b := recordDesignNote(t, store, designNoteObs{pr: 7, job: "review-7", state: db.FindingOpen, severity: "P3", title: "split the file", ago: 3 * time.Hour})
	p3c := recordDesignNote(t, store, designNoteObs{pr: 8, job: "review-8", state: db.FindingOpen, severity: "P3", title: "drop the alias", ago: 2 * time.Hour})
	// Not design notes: blocking severities, and P3s whose latest state is not open.
	p1 := recordDesignNote(t, store, designNoteObs{pr: 7, job: "review-7", state: db.FindingOpen, severity: "P1", title: "data loss", ago: 3 * time.Hour})
	p2 := recordDesignNote(t, store, designNoteObs{pr: 7, job: "review-7", state: db.FindingOpen, severity: "P2", title: "slow path", ago: 3 * time.Hour})
	var excluded = []string{p1, p2}
	for _, closing := range []db.FindingState{db.FindingAnswered, db.FindingWithdrawn, db.FindingSuperseded} {
		uid := recordDesignNote(t, store, designNoteObs{pr: 7, job: "review-7", state: db.FindingOpen, severity: "P3", title: "closed as " + string(closing), ago: 3 * time.Hour})
		recordDesignNote(t, store, designNoteObs{pr: 7, job: "review-7", state: closing, severity: "P3", title: "closed as " + string(closing), continues: uid, head: designNotesLedgerB, ago: time.Hour})
		excluded = append(excluded, uid)
	}
	// Outside the rolling 14-day window: first observed 15 days ago.
	old := recordDesignNote(t, store, designNoteObs{pr: 7, job: "review-7", state: db.FindingOpen, severity: "P3", title: "too old", ago: 15 * 24 * time.Hour})
	excluded = append(excluded, old)

	fake.states[7] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.states[8] = DesignNotesPullRequestState{Merged: true}

	result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("SweepDesignNotes: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("sweep errors: %v", result.Errors)
	}
	if result.Filed != 3 {
		t.Fatalf("filed %d findings, want 3", result.Filed)
	}
	if len(fake.created) != 1 || fake.created[0] != "[design-notes] owner/repo 2026-W46" {
		t.Fatalf("created issues %q, want one titled for 2026-W46", fake.created)
	}
	if got := strings.Join(fake.createLabels[0], ","); got != "design-notes,agent:lead" {
		t.Fatalf("issue labels = %s", got)
	}
	if !strings.Contains(fake.createBody, "advisory P3 findings from the review design checklist") || !strings.Contains(fake.createBody, "close this issue once they are triaged") {
		t.Fatalf("issue body does not explain design notes: %q", fake.createBody)
	}
	if len(fake.comments) != 2 {
		t.Fatalf("posted %d comments, want one per merged PR", len(fake.comments))
	}
	for _, c := range fake.comments {
		if c.issue != 501 || c.repo != designNotesRepo {
			t.Fatalf("comment went to %s#%d, want the week issue #501", c.repo, c.issue)
		}
	}
	seven := fake.commentFor(t, 7)
	for _, want := range []string{"name the seam", "split the file", "`internal/x.go:42`", "detail of name the seam",
		"`review-7`", "#7, merged head `" + designNotesMerged + "`", "finding-uid: " + p3a, "finding-uid: " + p3b} {
		if !strings.Contains(seven, want) {
			t.Fatalf("PR #7 comment lacks %q:\n%s", want, seven)
		}
	}
	for _, uid := range append(excluded, p3c) {
		if strings.Contains(seven, "finding-uid: "+uid+"\n") {
			t.Fatalf("PR #7 comment files %s, which is not an open P3 of #7:\n%s", uid, seven)
		}
	}
	// With no merged head from GitHub, the ledger head stands in.
	eight := fake.commentFor(t, 8)
	if !strings.Contains(eight, "finding-uid: "+p3c) || !strings.Contains(eight, "merged head `"+designNotesLedgerA+"`") {
		t.Fatalf("PR #8 comment:\n%s", eight)
	}
	messages, err := store.ListMessages(ctx, "lead", 0, 100)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(messages) != 1 || messages[0].Sender != db.MessageSystemSender ||
		messages[0].Body != "Design notes for owner/repo this week: https://github.com/owner/repo/issues/501. Advisory; nothing blocks." {
		t.Fatalf("role messages = %+v, want the one creation notice", messages)
	}

	// A second sweep files nothing and checks no pull request again.
	again, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("second SweepDesignNotes: %v", err)
	}
	if again.Checked != 0 || again.Filed != 0 || len(fake.comments) != 2 || len(fake.created) != 1 {
		t.Fatalf("second sweep checked %d, filed %d; comments %d, issues %d", again.Checked, again.Filed, len(fake.comments), len(fake.created))
	}

	// A PR merged later the same week joins the recorded issue: no new issue,
	// no search, and no second message.
	seedDesignNotesReviewJob(t, store, "review-9", `{"repo":"owner/repo","dispatched_by":"ops"}`)
	p3d := recordDesignNote(t, store, designNoteObs{pr: 9, job: "review-9", state: db.FindingOpen, severity: "P3", title: "later note", ago: time.Hour})
	fake.states[9] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	searches := fake.searches
	third, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || third.Filed != 1 {
		t.Fatalf("third sweep filed %d: %v %v", third.Filed, err, third.Errors)
	}
	if len(fake.created) != 1 || fake.searches != searches {
		t.Fatalf("third sweep created %d issues and searched %d times; the recorded issue must be reused", len(fake.created), fake.searches-searches)
	}
	nine := fake.commentFor(t, 9)
	if !strings.Contains(nine, "finding-uid: "+p3d) || !strings.Contains(nine, "requested by ops") {
		t.Fatalf("PR #9 comment (dispatched_by fallback):\n%s", nine)
	}
	if messages, _ := store.ListMessages(ctx, "lead", 0, 100); len(messages) != 1 {
		t.Fatalf("lead has %d messages, want still 1", len(messages))
	}
	if messages, _ := store.ListMessages(ctx, "ops", 0, 100); len(messages) != 0 {
		t.Fatalf("ops got %d messages for a reused issue", len(messages))
	}
}

func TestDesignNotesSweepSkipsClosedUnmergedForGoodAndRetriesOpen(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	closed := recordDesignNote(t, store, designNoteObs{pr: 20, job: "review-1", state: db.FindingOpen, severity: "P3", title: "closed pr note", ago: time.Hour})
	open := recordDesignNote(t, store, designNoteObs{pr: 21, job: "review-1", state: db.FindingOpen, severity: "P3", title: "open pr note", ago: time.Hour})
	fake.states[20] = DesignNotesPullRequestState{Closed: true}
	fake.states[21] = DesignNotesPullRequestState{}

	first, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("SweepDesignNotes: %v", err)
	}
	if first.Skipped != 1 || first.Pending != 1 || first.Filed != 0 || len(fake.created) != 0 || len(fake.comments) != 0 {
		t.Fatalf("first sweep = %+v, created %d, comments %d", first, len(fake.created), len(fake.comments))
	}
	if pending := designNoteFiled(t, store); pending[closed] || !pending[open] {
		t.Fatalf("candidates after first sweep = %v, want only the open PR's finding", pending)
	}

	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("second SweepDesignNotes: %v", err)
	}
	if fake.stateCalls[20] != 1 || fake.stateCalls[21] != 2 {
		t.Fatalf("state calls = %v: a closed PR is never checked again, an open one is", fake.stateCalls)
	}

	fake.states[21] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	third, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || third.Filed != 1 {
		t.Fatalf("third sweep filed %d: %v %v", third.Filed, err, third.Errors)
	}
	if body := fake.commentFor(t, 21); !strings.Contains(body, "finding-uid: "+open) {
		t.Fatalf("PR #21 comment:\n%s", body)
	}
}

func TestDesignNotesSweepReusesAWeekIssueFoundBySearch(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	recordDesignNote(t, store, designNoteObs{pr: 30, job: "review-1", state: db.FindingOpen, severity: "P3", title: "note", ago: time.Hour})
	fake.states[30] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.found = &DesignNotesIssue{Number: 77, URL: "https://github.com/owner/repo/issues/77"}

	result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || result.Filed != 1 {
		t.Fatalf("sweep filed %d: %v %v", result.Filed, err, result.Errors)
	}
	if len(fake.created) != 0 || len(fake.comments) != 1 || fake.comments[0].issue != 77 {
		t.Fatalf("created %q, comments %+v: the searched issue #77 must be reused", fake.created, fake.comments)
	}
	if messages, _ := store.ListMessages(ctx, "lead", 0, 100); len(messages) != 0 {
		t.Fatalf("a reused issue sent %d messages, want none", len(messages))
	}
	recorded, ok, err := store.GetDesignNoteIssue(ctx, designNotesRepo, "2026-W46")
	if err != nil || !ok || recorded.Number != 77 || recorded.Created {
		t.Fatalf("recorded issue = %+v ok=%v err=%v", recorded, ok, err)
	}
}

func TestDesignNotesSweepLeavesFindingsUnfiledWhenTheCommentFails(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	failing := recordDesignNote(t, store, designNoteObs{pr: 40, job: "review-1", state: db.FindingOpen, severity: "P3", title: "first", ago: time.Hour})
	filed := recordDesignNote(t, store, designNoteObs{pr: 41, job: "review-1", state: db.FindingOpen, severity: "P3", title: "second", ago: time.Hour})
	fake.states[40] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.states[41] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.commentErr[40] = errors.New("github 502")

	result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("SweepDesignNotes: %v", err)
	}
	if result.Filed != 1 || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "github 502") {
		t.Fatalf("result = %+v", result)
	}
	if pending := designNoteFiled(t, store); !pending[failing] || pending[filed] {
		t.Fatalf("candidates = %v: the failed PR's finding must stay unfiled, the other filed", pending)
	}
	events, err := store.ListJobEvents(ctx, "review-1")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	recorded := false
	for _, event := range events {
		recorded = recorded || (event.Kind == designNotesErrorEvent && strings.Contains(event.Message, "github 502"))
	}
	if !recorded {
		t.Fatalf("no design_notes_error event on the review job: %+v", events)
	}

	delete(fake.commentErr, 40)
	retry, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || retry.Filed != 1 {
		t.Fatalf("retry filed %d: %v %v", retry.Filed, err, retry.Errors)
	}
	if body := fake.commentFor(t, 40); !strings.Contains(body, "finding-uid: "+failing) {
		t.Fatalf("retried comment:\n%s", body)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created %d issues, want the week issue reused on retry", len(fake.created))
	}
}

func TestDesignNotesSweepIsBoundedAndRotates(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	for pr := 50; pr < 53; pr++ {
		recordDesignNote(t, store, designNoteObs{pr: int64(pr), job: "review-1", state: db.FindingOpen, severity: "P3", title: "note", ago: time.Hour})
		fake.states[pr] = DesignNotesPullRequestState{}
	}
	var cursor DesignNotesPullRequestKey
	for sweep := range 3 {
		result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{Limit: 2, After: cursor})
		if err != nil || result.Checked != 2 {
			t.Fatalf("sweep %d checked %d: %v", sweep, result.Checked, err)
		}
		cursor = result.Next
	}
	for pr := 50; pr < 53; pr++ {
		if fake.stateCalls[pr] != 2 {
			t.Fatalf("state calls = %v: three bounded sweeps of two must check each of three PRs twice", fake.stateCalls)
		}
	}
}

func TestDesignNotesWeekIsISO(t *testing.T) {
	for at, want := range map[time.Time]string{
		designNotesNow: "2026-W46",
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC):   "2026-W53",
		time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC): "2026-W01",
	} {
		if got := designNotesWeek(at); got != want {
			t.Fatalf("designNotesWeek(%s) = %s, want %s", at, got, want)
		}
	}
}

// #2559 review 1: the latest state is the ledger's insertion order. An answer
// recorded AFTER the open observation but stamped EARLIER still closes it.
func TestDesignNotesLatestStateFollowsInsertionOrder(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	uid := recordDesignNote(t, store, designNoteObs{pr: 90, job: "review-1", state: db.FindingOpen, severity: "P3", title: "answered later", ago: time.Hour})
	recordDesignNote(t, store, designNoteObs{pr: 90, job: "review-1", state: db.FindingAnswered, severity: "P3", title: "answered later",
		continues: uid, head: designNotesLedgerB, ago: 2 * time.Hour})
	fake.states[90] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}

	if pending := designNoteFiled(t, store); pending[uid] {
		t.Fatalf("finding %s is a candidate although its latest inserted observation answered it", uid)
	}
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("SweepDesignNotes: %v", err)
	}
	if len(fake.comments) != 0 || len(fake.created) != 0 {
		t.Fatalf("an answered finding was filed: comments %d, issues %d", len(fake.comments), len(fake.created))
	}
}

// #2559 review 2: a comment that landed but was not recorded (the API reported
// an error, or the record write failed) is repaired from the issue's comments
// on the next sweep, never posted twice; a partly posted pull request posts
// only what is missing.
func TestDesignNotesSweepRepairsPostedCommentsInsteadOfReposting(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	a := recordDesignNote(t, store, designNoteObs{pr: 100, job: "review-1", state: db.FindingOpen, severity: "P3", title: "a", ago: time.Hour})
	b := recordDesignNote(t, store, designNoteObs{pr: 100, job: "review-1", state: db.FindingOpen, severity: "P3", title: "b", ago: time.Hour})
	fake.states[100] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.commentErr[100] = errors.New("github 502 after the write")
	fake.landed[100] = true

	first, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || first.Filed != 0 || len(first.Errors) == 0 {
		t.Fatalf("first sweep = %+v, %v", first, err)
	}
	if pending := designNoteFiled(t, store); !pending[a] || !pending[b] {
		t.Fatalf("an ambiguous post marked findings filed: %v", pending)
	}
	delete(fake.commentErr, 100)
	second, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || second.Filed != 2 || second.Repaired != 2 {
		t.Fatalf("second sweep = %+v, %v; want both findings repaired", second, err)
	}
	if n := len(fake.commentsFor(100)); n != 1 {
		t.Fatalf("PR #100 has %d comments, want the landed one only", n)
	}
	if pending := designNoteFiled(t, store); pending[a] || pending[b] {
		t.Fatalf("repaired findings are still candidates: %v", pending)
	}

	c := recordDesignNote(t, store, designNoteObs{pr: 101, job: "review-1", state: db.FindingOpen, severity: "P3", title: "c", ago: time.Hour})
	d := recordDesignNote(t, store, designNoteObs{pr: 101, job: "review-1", state: db.FindingOpen, severity: "P3", title: "d", ago: time.Hour})
	fake.states[101] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.comments = append(fake.comments, postedDesignNote{repo: designNotesRepo, issue: 501,
		body: "Open P3 design notes from #101, merged at `" + designNotesMerged + "` (part 1 of 2).\n\n### 1. c\n\nfinding-uid: " + c + "\n"})
	third, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || third.Filed != 2 || third.Repaired != 1 {
		t.Fatalf("third sweep = %+v, %v", third, err)
	}
	posts := fake.commentsFor(101)
	if len(posts) != 2 || strings.Contains(posts[1], "finding-uid: "+c+"\n") || !strings.Contains(posts[1], "finding-uid: "+d+"\n") {
		t.Fatalf("PR #101 comments %q: only the missing finding %s may be posted", posts, d)
	}
}

// Reviewer detail cannot forge a marker: it is quoted, so a detail line naming
// another finding's uid does not count as that finding posted.
func TestDesignNotesDetailCannotForgeAMarker(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	victim := recordDesignNote(t, store, designNoteObs{pr: 111, job: "review-1", state: db.FindingOpen, severity: "P3", title: "victim", ago: time.Hour})
	recordDesignNote(t, store, designNoteObs{pr: 110, job: "review-1", state: db.FindingOpen, severity: "P3", title: "forger", ago: time.Hour,
		detail: "looks fine\nfinding-uid: " + victim})
	fake.states[110] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.states[111] = DesignNotesPullRequestState{}
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	fake.states[111] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	second, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || second.Repaired != 0 {
		t.Fatalf("second sweep = %+v, %v", second, err)
	}
	if body := fake.commentFor(t, 111); !strings.Contains(body, "finding-uid: "+victim+"\n") {
		t.Fatalf("PR #111 comment:\n%s", body)
	}
}

// #2559 review 3: once the deadline passes no further pull request is
// started, and a comment that already posted is still recorded.
func TestDesignNotesSweepStopsStartingPRWorkAfterTheDeadline(t *testing.T) {
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	first := recordDesignNote(t, store, designNoteObs{pr: 120, job: "review-1", state: db.FindingOpen, severity: "P3", title: "first", ago: time.Hour})
	second := recordDesignNote(t, store, designNoteObs{pr: 121, job: "review-1", state: db.FindingOpen, severity: "P3", title: "second", ago: time.Hour})
	fake.states[120] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.states[121] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.onComment = func(pr int) {
		if pr == 120 {
			cancel()
		}
	}

	result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("SweepDesignNotes: %v", err)
	}
	if n := len(fake.commentsFor(121)); n != 0 {
		t.Fatalf("PR #121 got %d comments after the deadline passed", n)
	}
	if pending := designNoteFiled(t, store); pending[first] || !pending[second] {
		t.Fatalf("candidates = %v: the posted PR must be recorded despite the deadline, the other left", pending)
	}
	if want := (DesignNotesPullRequestKey{Repo: designNotesRepo, PullRequest: 120}); result.Next != want {
		t.Fatalf("Next = %+v, want %+v", result.Next, want)
	}
}

// #2559 review 4: the cursor rotates whatever the candidate count and names
// the last pull request actually attempted, so a slow first pull request that
// eats the deadline cannot starve the rest.
func TestDesignNotesCursorResumesAfterTheLastAttemptedPR(t *testing.T) {
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	for _, pr := range []int{130, 131, 132} {
		recordDesignNote(t, store, designNoteObs{pr: int64(pr), job: "review-1", state: db.FindingOpen, severity: "P3", title: "note", ago: time.Hour})
		fake.states[pr] = DesignNotesPullRequestState{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var order []int
	fake.onState = func(pr int) {
		order = append(order, pr)
		if pr == 130 {
			cancel() // the slow first pull request uses up the deadline
		}
	}
	slow, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("slow sweep: %v", err)
	}
	if want := (DesignNotesPullRequestKey{Repo: designNotesRepo, PullRequest: 130}); slow.Checked != 1 || slow.Next != want {
		t.Fatalf("slow sweep checked %d, Next %+v; want 1 and %+v", slow.Checked, slow.Next, want)
	}
	order = nil
	fake.onState = func(pr int) { order = append(order, pr) }
	next, err := engine.SweepDesignNotes(context.Background(), DesignNotesSweepOptions{After: slow.Next})
	if err != nil {
		t.Fatalf("next sweep: %v", err)
	}
	if fmt.Sprint(order) != "[131 132 130]" {
		t.Fatalf("next sweep checked %v, want it to resume after #130 and wrap", order)
	}
	if want := (DesignNotesPullRequestKey{Repo: designNotesRepo, PullRequest: 130}); next.Next != want {
		t.Fatalf("Next = %+v, want the last attempted %+v", next.Next, want)
	}
}

// #2559 review 5: the whole rendered comment stays under GitHub's limit; a
// pull request whose notes do not fit is split across comments and every
// finding is filed, an over-long detail truncated with a note.
func TestDesignNotesCommentsSplitUnderGitHubLimitWithoutDroppingFindings(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	file := "internal/" + strings.Repeat("deep/", 50) + "x.go"
	uids := map[string]bool{}
	for i := range 120 {
		title := fmt.Sprintf("%03d %s", i, strings.Repeat("t", 140))
		uids[recordDesignNote(t, store, designNoteObs{pr: 140, job: "review-1", state: db.FindingOpen, severity: "P3", title: title,
			ago: time.Hour, file: file, detail: strings.Repeat("d", 500)})] = true
	}
	uids[recordDesignNote(t, store, designNoteObs{pr: 140, job: "review-1", state: db.FindingOpen, severity: "P3", title: "huge",
		ago: time.Hour, detail: strings.Repeat("h", 9000)})] = true
	fake.states[140] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}

	result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || result.Filed != len(uids) || len(result.Errors) != 0 {
		t.Fatalf("sweep = filed %d of %d, errors %v, %v", result.Filed, len(uids), result.Errors, err)
	}
	posts := fake.commentsFor(140)
	seen := map[string]int{}
	truncated := false
	for _, body := range posts {
		// Bytes bound characters: GitHub's limit is 65536 characters.
		if len(body) >= 65536 {
			t.Fatalf("a comment of %d bytes breaks GitHub's 65536-character limit", len(body))
		}
		for uid := range uids {
			seen[uid] += strings.Count(body, "\nfinding-uid: "+uid+"\n")
		}
		truncated = truncated || strings.Contains(body, "(detail truncated: first 4000 of 9000 bytes shown)")
	}
	if len(posts) < 2 {
		t.Fatalf("%d findings fit one comment; the test must force a split", len(uids))
	}
	for uid := range uids {
		if seen[uid] != 1 {
			t.Fatalf("finding %s appears in %d comments, want exactly 1", uid, seen[uid])
		}
	}
	if !truncated {
		t.Fatal("the over-long detail carries no truncation note")
	}
	if pending := designNoteFiled(t, store); len(pending) != 0 {
		t.Fatalf("%d findings left unfiled", len(pending))
	}
}

// #2559 review 7: a review job that cannot be read is an error for its pull
// request, retried later, not an unassigned filing.
func TestDesignNotesSweepRetriesAPRWhoseRequesterCannotBeResolved(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	uid := recordDesignNote(t, store, designNoteObs{pr: 150, job: "review-late", state: db.FindingOpen, severity: "P3", title: "note", ago: time.Hour})
	fake.states[150] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}

	first, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(fake.comments) != 0 || len(fake.created) != 0 || len(first.Errors) != 1 || !strings.Contains(first.Errors[0], "review-late") {
		t.Fatalf("first sweep: comments %d, issues %d, errors %v; an unreadable review job must not file as unassigned",
			len(fake.comments), len(fake.created), first.Errors)
	}
	if pending := designNoteFiled(t, store); !pending[uid] {
		t.Fatal("the finding was marked filed")
	}
	seedDesignNotesReviewJob(t, store, "review-late", `{"review_requester":"lead"}`)
	second, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || second.Filed != 1 {
		t.Fatalf("second sweep = %+v, %v", second, err)
	}
	if len(fake.createLabels) != 1 || strings.Join(fake.createLabels[0], ",") != "design-notes,agent:lead" {
		t.Fatalf("issue labels = %v", fake.createLabels)
	}
}
