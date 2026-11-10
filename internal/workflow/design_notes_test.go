package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
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
	listErr   map[int64]error
	onState   func(number int)
	onComment func(number int)
	// compares maps a base (merge commit) to its compare with the default
	// branch; compareErr fails one. compareCalls logs "repo base...head".
	compares     map[string]github.CompareResult
	compareErr   map[string]error
	compareCalls []string
}

func newDesignNotesFake() *designNotesFake {
	return &designNotesFake{states: map[int]DesignNotesPullRequestState{}, stateCalls: map[int]int{},
		commentErr: map[int]error{}, landed: map[int]bool{}, listErr: map[int64]error{},
		compares: map[string]github.CompareResult{}, compareErr: map[string]error{}}
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
	e.DesignNotesFindIssue = func(_ context.Context, _, _ string) (DesignNotesIssue, bool, error) {
		f.searches++
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
		if err := f.listErr[issue]; err != nil {
			return nil, err
		}
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
	e.DesignNotesCompare = func(_ context.Context, repo, base, head string) (github.CompareResult, error) {
		f.compareCalls = append(f.compareCalls, repo+" "+base+"..."+head)
		if err := f.compareErr[base]; err != nil {
			return github.CompareResult{}, err
		}
		return f.compares[base], nil
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
	// noFile records a finding that cites no file; line overrides the
	// default line 42, and lineless records line 0.
	noFile   bool
	line     int64
	lineless bool
	declared db.FindingState
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
	if file == "" && !o.noFile {
		file = "internal/x.go"
	}
	line := o.line
	if line == 0 && !o.lineless {
		line = 42
	}
	obs := db.ReviewFindingObservation{
		Repo: designNotesRepo, PullRequest: o.pr, HeadSHA: head, State: o.state, DeclaredState: o.declared, Severity: o.severity,
		ContinuesUID: o.continues, ObserverJob: o.job, Title: o.title, Detail: detail,
		File: file, Line: line, EvidenceKind: db.EvidenceExecuted,
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
	// The body says where the notes are, that each may already be fixed, and
	// when to close the issue (#2566).
	for _, want := range []string{"advisory P3 findings from the review design checklist",
		"The notes are the comments below, one per merged pull request.",
		"may already be fixed on the default branch, so check the current code before acting",
		"close this issue once every note is triaged"} {
		if !strings.Contains(fake.createBody, want) {
			t.Fatalf("issue body lacks %q: %q", want, fake.createBody)
		}
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

// #2559 review round 2, finding 1: a comment that landed with an ambiguous
// error in the last minute of an ISO week is not posted again to the next
// week's issue: the next sweep reads back the issue it had recorded as the
// target, not just the current week's.
func TestDesignNotesWeekRolloverDoesNotRepostALandedComment(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	clock := time.Date(2026, 11, 15, 23, 59, 0, 0, time.UTC) // last minute of 2026-W46
	engine.Now = func() time.Time { return clock }
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	uid := recordDesignNote(t, store, designNoteObs{pr: 160, job: "review-1", state: db.FindingOpen, severity: "P3", title: "note", ago: time.Hour})
	fake.states[160] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.commentErr[160] = errors.New("github 502 after the write")
	fake.landed[160] = true

	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("Sunday sweep: %v", err)
	}
	delete(fake.commentErr, 160)
	clock = clock.Add(2 * time.Minute) // Monday 00:01, 2026-W47
	second, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("Monday sweep: %v", err)
	}
	copies := 0
	for _, c := range fake.comments {
		copies += strings.Count(c.body, "\nfinding-uid: "+uid+"\n")
	}
	if copies != 1 {
		t.Fatalf("comments_for_same_finding = %d, want 1 (issues created: %q)", copies, fake.created)
	}
	if second.Repaired != 1 || len(fake.created) != 1 {
		t.Fatalf("Monday sweep = %+v, issues %q; want the finding repaired against the W46 issue and no W47 issue", second, fake.created)
	}
	if pending := designNoteFiled(t, store); pending[uid] {
		t.Fatal("the landed finding is still unfiled")
	}
}

// #2559 review round 2, finding 2: under REAL writer contention (a second
// connection holding BEGIN IMMEDIATE far longer than the sweep's budget) the
// sweep returns near its deadline, not when the lock frees: the store's 15s
// busy wait would otherwise ignore the context.
func TestDesignNotesSweepBoundsLockWaitsByItsDeadline(t *testing.T) {
	const hold = 7 * time.Second
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store := openEngineStoreAt(t, path)
	fake := newDesignNotesFake()
	engine := Engine{Store: store}
	fake.wire(&engine)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	uid := recordDesignNote(t, store, designNoteObs{pr: 170, job: "review-1", state: db.FindingOpen, severity: "P3", title: "note", ago: time.Hour})
	fake.states[170] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.found = &DesignNotesIssue{Number: 77, URL: "https://github.com/owner/repo/issues/77"}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("second connection: %v", err)
	}
	defer raw.Close()
	blocker, err := raw.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker connection: %v", err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	release, released := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(released)
		select {
		case <-release:
		case <-time.After(hold):
		}
		_, _ = blocker.ExecContext(context.Background(), `ROLLBACK`)
	}()

	sweepCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	result, err := engine.SweepDesignNotes(sweepCtx, DesignNotesSweepOptions{})
	elapsed := time.Since(start)
	close(release)
	<-released
	if err != nil {
		t.Fatalf("SweepDesignNotes: %v", err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("a 500ms sweep returned after %s against a %s lock hold; its SQLite waits ignore the deadline", elapsed, hold)
	}
	if len(fake.comments) != 0 {
		t.Fatalf("posted %d comments without first recording the target issue", len(fake.comments))
	}
	if pending := designNoteFiled(t, store); !pending[uid] {
		t.Fatalf("the finding was marked filed; result %+v", result)
	}
}

// countDesignNoteMarkers counts the comments carrying uid's marker line.
func countDesignNoteMarkers(fake *designNotesFake, uid string) int {
	n := 0
	for _, c := range fake.comments {
		n += strings.Count(c.body, "\nfinding-uid: "+uid+"\n")
	}
	return n
}

// #2559 review round 3, finding 2 (the probe): a completed attempt is retired,
// so a week later a new finding on the same pull request reaches the new
// week's issue although the old issue now returns 404, and nothing is
// reposted.
func TestDesignNotesCompletedAttemptDoesNotBlockLaterFindings(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	clock := designNotesNow
	engine.Now = func() time.Time { return clock }
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	first := recordDesignNote(t, store, designNoteObs{pr: 180, job: "review-1", state: db.FindingOpen, severity: "P3", title: "first", ago: time.Hour})
	fake.states[180] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	if r, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil || r.Filed != 1 {
		t.Fatalf("first sweep = %+v, %v", r, err)
	}
	if attempts, err := store.ListDesignNotePendingPosts(ctx, designNotesRepo, 180); err != nil || len(attempts) != 0 {
		t.Fatalf("attempts after a completed post = %+v, %v; want it retired", attempts, err)
	}

	clock = clock.Add(7 * 24 * time.Hour)
	later := recordDesignNote(t, store, designNoteObs{pr: 180, job: "review-1", state: db.FindingOpen, severity: "P3", title: "later", ago: -7*24*time.Hour + time.Hour})
	fake.listErr[501] = errors.New("HTTP 404: Not Found")
	second, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || second.Filed != 1 || len(second.Errors) != 0 {
		t.Fatalf("second sweep = %+v, %v; want the new finding filed with no error", second, err)
	}
	if len(fake.created) != 2 {
		t.Fatalf("issues created %q, want W46 and W47", fake.created)
	}
	if a, b := countDesignNoteMarkers(fake, first), countDesignNoteMarkers(fake, later); a != 1 || b != 1 {
		t.Fatalf("marker copies first=%d later=%d, want 1 and 1", a, b)
	}
	if pending := designNoteFiled(t, store); pending[later] {
		t.Fatal("the later finding is unfiled")
	}
}

// #2559 review round 3, finding 2: an unresolved attempt on an issue that
// cannot be read holds back only the findings it may carry; other findings of
// the pull request still reach this week's issue, and the held one is
// reconciled, not reposted, once the issue reads again.
func TestDesignNotesUnreadableAttemptHoldsBackOnlyItsFindings(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	clock := designNotesNow
	engine.Now = func() time.Time { return clock }
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	unknown := recordDesignNote(t, store, designNoteObs{pr: 190, job: "review-1", state: db.FindingOpen, severity: "P3", title: "unknown", ago: time.Hour})
	fake.states[190] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.commentErr[190] = errors.New("github 502 after the write")
	fake.landed[190] = true
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	delete(fake.commentErr, 190)

	clock = clock.Add(7 * 24 * time.Hour)
	fresh := recordDesignNote(t, store, designNoteObs{pr: 190, job: "review-1", state: db.FindingOpen, severity: "P3", title: "fresh", ago: -7*24*time.Hour + time.Hour})
	fake.listErr[501] = errors.New("github 500")
	second, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || second.Filed != 1 || len(second.Errors) != 1 || !strings.Contains(second.Errors[0], "holding back") {
		t.Fatalf("second sweep = %+v, %v", second, err)
	}
	if a, b := countDesignNoteMarkers(fake, unknown), countDesignNoteMarkers(fake, fresh); a != 1 || b != 1 {
		t.Fatalf("marker copies unknown=%d fresh=%d, want 1 and 1: the held finding must not be reposted", a, b)
	}
	if pending := designNoteFiled(t, store); !pending[unknown] || pending[fresh] {
		t.Fatalf("candidates = %v: the held finding waits, the fresh one is filed", pending)
	}

	delete(fake.listErr, 501)
	third, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || third.Repaired != 1 || countDesignNoteMarkers(fake, unknown) != 1 {
		t.Fatalf("third sweep = %+v, %v; copies %d", third, err, countDesignNoteMarkers(fake, unknown))
	}
	if attempts, err := store.ListDesignNotePendingPosts(ctx, designNotesRepo, 190); err != nil || len(attempts) != 0 {
		t.Fatalf("attempts after reconciliation = %+v, %v", attempts, err)
	}
}

// #2559 review round 4 (the reviewer's probe): F1 is posted with an ambiguous
// outcome, then answered; an unrelated finding on the same pull request is
// filed the next week. The unread attempt carrying F1 must survive that sweep,
// so when F1 reopens it is repaired from the attempted issue, not reposted.
func TestDesignNotesUnreadAttemptSurvivesUntilItsFindingReopens(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	clock := designNotesNow
	engine.Now = func() time.Time { return clock }
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	f1 := recordDesignNote(t, store, designNoteObs{pr: 200, job: "review-1", state: db.FindingOpen, severity: "P3", title: "f1", ago: time.Hour})
	fake.states[200] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.commentErr[200] = errors.New("github 502 after the write")
	fake.landed[200] = true
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	delete(fake.commentErr, 200)
	recordDesignNote(t, store, designNoteObs{pr: 200, job: "review-1", state: db.FindingAnswered, severity: "P3", title: "f1",
		continues: f1, head: designNotesLedgerB, ago: 30 * time.Minute})

	clock = clock.Add(7 * 24 * time.Hour)
	unrelated := recordDesignNote(t, store, designNoteObs{pr: 200, job: "review-1", state: db.FindingOpen, severity: "P3", title: "unrelated", ago: -7*24*time.Hour + time.Hour})
	if r, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil || r.Filed != 1 {
		t.Fatalf("next-week sweep = %+v, %v", r, err)
	}
	if pending := designNoteFiled(t, store); pending[unrelated] {
		t.Fatal("the unrelated finding was not filed")
	}
	attempts, err := store.ListDesignNotePendingPosts(ctx, designNotesRepo, 200)
	if err != nil || len(attempts) != 1 || attempts[0].IssueNumber != 501 {
		t.Fatalf("attempts = %+v, %v; the unread attempt carrying F1 on #501 must be kept", attempts, err)
	}

	recordDesignNote(t, store, designNoteObs{pr: 200, job: "review-1", state: db.FindingOpen, severity: "P3", title: "f1",
		continues: f1, head: designNotesMerged, ago: -7*24*time.Hour + 30*time.Minute})
	reopened, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("reopen sweep: %v", err)
	}
	if n := countDesignNoteMarkers(fake, f1); n != 1 || reopened.Repaired != 1 {
		t.Fatalf("F1 marker copies = %d, Repaired = %d; want 1 and 1 (repaired from #501, not reposted)", n, reopened.Repaired)
	}
	if attempts, err := store.ListDesignNotePendingPosts(ctx, designNotesRepo, 200); err != nil || len(attempts) != 0 {
		t.Fatalf("attempts after the read-back = %+v, %v; want it retired", attempts, err)
	}
}

// An unread attempt whose findings have all left the window can never matter
// again, so a later sweep retires it without reading it; rows cannot pile up.
func TestDesignNotesUnreadAttemptIsRetiredOnceItsFindingsLeaveTheWindow(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	clock := designNotesNow
	engine.Now = func() time.Time { return clock }
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	f1 := recordDesignNote(t, store, designNoteObs{pr: 210, job: "review-1", state: db.FindingOpen, severity: "P3", title: "f1", ago: time.Hour})
	fake.states[210] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.commentErr[210] = errors.New("github 502 after the write")
	fake.landed[210] = true
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	recordDesignNote(t, store, designNoteObs{pr: 210, job: "review-1", state: db.FindingAnswered, severity: "P3", title: "f1",
		continues: f1, head: designNotesLedgerB, ago: 30 * time.Minute})

	clock = clock.Add(7 * 24 * time.Hour)
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("in-window sweep: %v", err)
	}
	if attempts, _ := store.ListDesignNotePendingPosts(ctx, designNotesRepo, 210); len(attempts) != 1 {
		t.Fatalf("attempts inside the window = %+v; want the unread attempt kept", attempts)
	}
	clock = designNotesNow.Add(15 * 24 * time.Hour)
	listCalls := fake.listCalls
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("out-of-window sweep: %v", err)
	}
	if attempts, _ := store.ListDesignNotePendingPosts(ctx, designNotesRepo, 210); len(attempts) != 0 || fake.listCalls != listCalls {
		t.Fatalf("attempts after the window = %+v, reads %d; want it retired unread", attempts, fake.listCalls-listCalls)
	}
}

// #2559 review round 5 (the reviewer's probe): F1's comment lands ambiguously,
// F1 is answered, and a day later an unrelated finding of the same pull
// request is posted to the SAME week issue. Filing the unrelated finding must
// retire only its own attempt, so when F1 reopens it is repaired from the
// issue, not reposted.
func TestDesignNotesFilingAnotherFindingKeepsAnUnresolvedAttemptOnTheSameIssue(t *testing.T) {
	ctx := context.Background()
	engine, store, fake := designNotesEngine(t)
	clock := designNotesNow
	engine.Now = func() time.Time { return clock }
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	f1 := recordDesignNote(t, store, designNoteObs{pr: 220, job: "review-1", state: db.FindingOpen, severity: "P3", title: "f1", ago: time.Hour})
	fake.states[220] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	fake.commentErr[220] = errors.New("github 502 after the write")
	fake.landed[220] = true
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	delete(fake.commentErr, 220)
	recordDesignNote(t, store, designNoteObs{pr: 220, job: "review-1", state: db.FindingAnswered, severity: "P3", title: "f1",
		continues: f1, head: designNotesLedgerB, ago: 30 * time.Minute})

	clock = clock.Add(24 * time.Hour) // same ISO week, same issue #501
	unrelated := recordDesignNote(t, store, designNoteObs{pr: 220, job: "review-1", state: db.FindingOpen, severity: "P3", title: "unrelated", ago: -24*time.Hour + time.Hour})
	if r, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil || r.Filed != 1 {
		t.Fatalf("next-day sweep = %+v, %v", r, err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("issues created %q, want the one W46 issue", fake.created)
	}
	if pending := designNoteFiled(t, store); pending[unrelated] {
		t.Fatal("the unrelated finding was not filed")
	}
	attempts, err := store.ListDesignNotePendingPosts(ctx, designNotesRepo, 220)
	if err != nil || len(attempts) != 1 || attempts[0].IssueNumber != 501 {
		t.Fatalf("attempts = %+v, %v; want only F1's unresolved attempt on #501 kept", attempts, err)
	}

	recordDesignNote(t, store, designNoteObs{pr: 220, job: "review-1", state: db.FindingOpen, severity: "P3", title: "f1",
		continues: f1, head: designNotesMerged, ago: -24*time.Hour + 30*time.Minute})
	reopened, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("reopen sweep: %v", err)
	}
	if n := countDesignNoteMarkers(fake, f1); n != 1 || reopened.Repaired != 1 {
		t.Fatalf("marker_copies = %d, Repaired = %d; want 1 and 1", n, reopened.Repaired)
	}
	if attempts, err := store.ListDesignNotePendingPosts(ctx, designNotesRepo, 220); err != nil || len(attempts) != 0 {
		t.Fatalf("attempts after the repair = %+v, %v", attempts, err)
	}
}
