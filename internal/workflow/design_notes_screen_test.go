package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

const (
	designNotesMergeA = "dddddddddddddddddddddddddddddddddddddddd"
	designNotesMergeB = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// designNotesScreenEngine is designNotesEngine on a store whose file the test
// can also read directly, for the skip reasons the sweep records.
func designNotesScreenEngine(t *testing.T) (Engine, *db.Store, *designNotesFake, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store := openEngineStoreAt(t, path)
	fake := newDesignNotesFake()
	engine := Engine{Store: store}
	fake.wire(&engine)
	seedDesignNotesReviewJob(t, store, "review-1", `{"review_requester":"lead"}`)
	return engine, store, fake, path
}

// designNoteSkipReasons reads design_note_filings: finding uid to skip reason,
// "" for a finding filed into an issue.
func designNoteSkipReasons(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT finding_uid, skip_reason FROM design_note_filings`)
	if err != nil {
		t.Fatalf("read filings: %v", err)
	}
	defer rows.Close()
	reasons := map[string]string{}
	for rows.Next() {
		var uid, reason string
		if err := rows.Scan(&uid, &reason); err != nil {
			t.Fatalf("scan filing: %v", err)
		}
		reasons[uid] = reason
	}
	return reasons
}

const designNotesChangedLine = "- Changed since merge: `far.go` was modified on `main` after this PR merged; check the current code first.\n"

// #2566: before posting, the sweep compares each merged pull request's MERGE
// COMMIT with the default branch, once. A finding whose file was removed or
// renamed away, or whose cited lines a later hunk rewrote, is skipped for
// good; one whose file changed elsewhere is posted with a "Changed since
// merge" line; the rest are posted as before.
func TestDesignNotesSweepScreensFindingsAgainstTheDefaultBranchSinceMerge(t *testing.T) {
	ctx := context.Background()
	engine, store, fake, path := designNotesScreenEngine(t)
	note := func(pr int64, title string, o designNoteObs) string {
		o.pr, o.job, o.state, o.severity, o.title, o.ago = pr, "review-1", db.FindingOpen, "P3", title, time.Hour
		return recordDesignNote(t, store, o)
	}
	removed := note(30, "removed file", designNoteObs{file: "gone.go"})
	renamed := note(30, "renamed file", designNoteObs{file: "old.go"})
	rewritten := note(30, "rewritten lines", designNoteObs{file: "hunk.go", line: 42})
	elsewhere := note(30, "changed elsewhere", designNoteObs{file: "far.go", line: 42})
	lineless := note(30, "changed, no line", designNoteObs{file: "far.go", lineless: true})
	untouched := note(30, "untouched file", designNoteObs{file: "same.go"})
	fileless := note(30, "no file", designNoteObs{noFile: true})
	other := note(31, "other pr", designNoteObs{})

	fake.states[30] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged, MergeCommitSHA: designNotesMergeA, DefaultBranch: "main"}
	fake.states[31] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged, MergeCommitSHA: designNotesMergeB, DefaultBranch: "main"}
	fake.compares[designNotesMergeA] = github.CompareResult{Files: []github.PullRequestFile{
		{Filename: "gone.go", Status: "removed"},
		{Filename: "new.go", Status: "renamed", PreviousFilename: "old.go"},
		// The second hunk's old side, 44-45, reaches within 3 lines of 42.
		{Filename: "hunk.go", Status: "modified", Patch: "@@ -10,3 +10,4 @@ func a()\n x\n+y\n x\n x\n@@ -44,2 +45,2 @@ func b()\n-a\n+b\n x"},
		// Far from line 42.
		{Filename: "far.go", Status: "modified", Patch: "@@ -100,3 +100,4 @@\n x\n+y\n x\n x"},
	}}

	result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("SweepDesignNotes: %v %v", err, result.Errors)
	}
	if result.Skipped != 3 || result.Filed != 5 {
		t.Fatalf("skipped %d, filed %d; want 3 skipped, 5 filed", result.Skipped, result.Filed)
	}
	// One compare per pull request, from its merge commit, never its head.
	if want := []string{designNotesRepo + " " + designNotesMergeA + "...main", designNotesRepo + " " + designNotesMergeB + "...main"}; strings.Join(fake.compareCalls, ",") != strings.Join(want, ",") {
		t.Fatalf("compare calls = %q, want %q", fake.compareCalls, want)
	}

	body := fake.commentFor(t, 30)
	for _, uid := range []string{removed, renamed, rewritten} {
		if strings.Contains(body, "finding-uid: "+uid+"\n") {
			t.Fatalf("a finding the default branch outdated was posted:\n%s", body)
		}
	}
	for _, uid := range []string{elsewhere, lineless, untouched, fileless} {
		if !strings.Contains(body, "finding-uid: "+uid+"\n") {
			t.Fatalf("PR #30 comment lacks %s:\n%s", uid, body)
		}
	}
	if got := strings.Count(body, designNotesChangedLine); got != 2 {
		t.Fatalf("%d 'Changed since merge' lines, want 2 (the far.go findings):\n%s", got, body)
	}
	if strings.Count(body, "Changed since merge") != 2 {
		t.Fatalf("an unchanged or fileless finding is marked changed:\n%s", body)
	}
	if strings.Contains(fake.commentFor(t, 31), "Changed since merge") {
		t.Fatal("PR #31's untouched finding is marked changed")
	}

	reasons := designNoteSkipReasons(t, path)
	for uid, want := range map[string]string{
		removed: db.DesignNoteSkipFileRemoved, renamed: db.DesignNoteSkipFileRemoved, rewritten: db.DesignNoteSkipLinesChanged,
		elsewhere: "", lineless: "", untouched: "", fileless: "", other: "",
	} {
		if got, ok := reasons[uid]; !ok || got != want {
			t.Fatalf("filing of %s = %q (recorded %v), want %q", uid, got, ok, want)
		}
	}

	// Skips are final: a later sweep checks nothing again.
	again, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || again.Checked != 0 || len(fake.compareCalls) != 2 || len(designNoteFiled(t, store)) != 0 {
		t.Fatalf("second sweep checked %d, compares %d: %v", again.Checked, len(fake.compareCalls), err)
	}
}

// The staleness check fails open (#2566): notes are advisory, so a compare
// that cannot be trusted, or cannot be made, leaves every note posted as
// before and none marked changed.
func TestDesignNotesSweepPostsPlainWhenTheCompareCannotBeTrusted(t *testing.T) {
	ctx := context.Background()
	engine, store, fake, _ := designNotesScreenEngine(t)
	// Every finding cites internal/x.go:42, which each compare below says was
	// removed: only a trusted compare may act on that.
	gone := github.CompareResult{Files: []github.PullRequestFile{{Filename: "internal/x.go", Status: "removed"}}}
	merges := map[int]string{40: strings.Repeat("1", 40), 41: strings.Repeat("2", 40), 42: "", 43: strings.Repeat("4", 40)}
	for pr, merge := range merges {
		recordDesignNote(t, store, designNoteObs{pr: int64(pr), job: "review-1", state: db.FindingOpen, severity: "P3", title: "note", ago: time.Hour})
		branch := "main"
		if pr == 43 {
			branch = ""
		}
		fake.states[pr] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged, MergeCommitSHA: merge, DefaultBranch: branch}
		fake.compares[merge] = gone
	}
	fake.compareErr[merges[40]] = errors.New("HTTP 502")
	truncated := gone
	truncated.Truncated = true
	fake.compares[merges[41]] = truncated

	result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil {
		t.Fatalf("SweepDesignNotes: %v", err)
	}
	if result.Filed != 4 || result.Skipped != 0 {
		t.Fatalf("filed %d, skipped %d; want all 4 posted", result.Filed, result.Skipped)
	}
	// No merge commit (#42) or no default branch (#43): nothing to compare.
	if want := designNotesRepo + " " + merges[40] + "...main," + designNotesRepo + " " + merges[41] + "...main"; strings.Join(fake.compareCalls, ",") != want {
		t.Fatalf("compare calls = %q", fake.compareCalls)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "HTTP 502") {
		t.Fatalf("errors = %q, want the failed compare reported once", result.Errors)
	}
	for pr := range merges {
		if body := fake.commentFor(t, pr); strings.Contains(body, "Changed since merge") {
			t.Fatalf("PR #%d is marked changed on an untrusted compare:\n%s", pr, body)
		}
	}
}

// #2566 (from #2560's triage): the ledger records a declared answer with weak
// evidence as OPEN, for the merge gate, so 14 of #2560's 45 notes were the
// reviewer's own "Resolved: ..." confirmations. The sweep reads the declared
// state instead; a row from before it was recorded falls back to the title.
func TestDesignNotesSweepSkipsNotesTheReviewerDeclaredResolved(t *testing.T) {
	ctx := context.Background()
	engine, store, fake, path := designNotesScreenEngine(t)
	note := func(title string, declared db.FindingState) string {
		return recordDesignNote(t, store, designNoteObs{pr: 50, job: "review-1", state: db.FindingOpen, severity: "P3", title: title, ago: time.Hour, declared: declared})
	}
	skipped := []string{
		note("Distinguish attribution failures from unassigned reviews", db.FindingAnswered),
		note("Not a defect after all", db.FindingWithdrawn),
		note("Resolved: the figure no longer overflows", ""),
		note("ADDRESSED at 6a3e708: delete-all resets the photo", ""),
		note("R3 addressed: intervening holds invalidate classification", ""),
		note("R8 remains addressed: recent candidates participate", ""),
	}
	posted := []string{
		note("R5 remains unresolved: reject superseded readiness", ""),
		note("Resolvedness of the cache key is unclear", ""),
		note("Split the parser from the renderer", ""),
		// The reviewer said open: its word beats the title.
		note("Resolved: mostly, but the doc still lags", db.FindingOpen),
	}
	fake.states[50] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}

	result, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("SweepDesignNotes: %v %v", err, result.Errors)
	}
	if result.Skipped != len(skipped) || result.Filed != len(posted) {
		t.Fatalf("skipped %d, filed %d; want %d, %d", result.Skipped, result.Filed, len(skipped), len(posted))
	}
	body := fake.commentFor(t, 50)
	reasons := designNoteSkipReasons(t, path)
	for _, uid := range skipped {
		if strings.Contains(body, "finding-uid: "+uid+"\n") || reasons[uid] != db.DesignNoteSkipDeclaredResolved {
			t.Fatalf("declared-resolved %s: posted=%v reason=%q", uid, strings.Contains(body, uid), reasons[uid])
		}
	}
	for _, uid := range posted {
		if !strings.Contains(body, "finding-uid: "+uid+"\n") {
			t.Fatalf("PR #50 comment lacks %s:\n%s", uid, body)
		}
	}

	// A pull request whose every note was declared resolved opens nothing.
	recordDesignNote(t, store, designNoteObs{pr: 51, job: "review-1", state: db.FindingOpen, severity: "P3", title: "done", ago: time.Hour, declared: db.FindingAnswered})
	fake.states[51] = DesignNotesPullRequestState{Merged: true, HeadSHA: designNotesMerged}
	created := len(fake.created)
	if _, err := engine.SweepDesignNotes(ctx, DesignNotesSweepOptions{}); err != nil {
		t.Fatalf("second SweepDesignNotes: %v", err)
	}
	if len(fake.commentsFor(51)) != 0 || len(fake.created) != created {
		t.Fatalf("PR #51 got %d comments and %d new issues", len(fake.commentsFor(51)), len(fake.created)-created)
	}
}

func TestDesignNotesHunksTouchReadsOldSideRanges(t *testing.T) {
	for _, tc := range []struct {
		patch   string
		touched bool
		ok      bool
	}{
		{"@@ -45,1 +45,1 @@\n-a\n+b", true, true},                  // 45 = 42+3
		{"@@ -46,4 +46,4 @@\n-a\n+b", false, true},                 // starts past 45
		{"@@ -30,9 +30,9 @@\n x", false, true},                     // ends at 38, before 39
		{"@@ -30,10 +30,11 @@\n x", true, true},                    // ends at 39 = 42-3
		{"@@ -42 +42 @@\n-a\n+b", true, true},                      // count omitted means 1
		{"@@ -1,2 +1,2 @@\n x\n@@ -41,0 +42,2 @@\n+a", true, true}, // insertion after 41
		{"@@ -38,0 +39,2 @@\n+a", true, true},                      // inserted between 38 and 39: in range
		{"@@ -50,0 +51,2 @@\n+a", false, true},
		{"@@ -x,1 +1 @@\n x", false, false},
		{"@@ -4,2\n x", false, false},
	} {
		touched, ok := designNotesHunksTouch(tc.patch, 42)
		if touched != tc.touched || ok != tc.ok {
			t.Fatalf("designNotesHunksTouch(%q, 42) = %v, %v; want %v, %v", tc.patch, touched, ok, tc.touched, tc.ok)
		}
	}
}

// #2566 (from #2560's triage): #2473's whole note sat in a title the heading
// cut at 120 characters, and #2376's detail was a bare locator, so each note
// rendered as a fragment. A cut title with no real detail becomes the quote.
func TestDesignNotesBlockQuotesACutTitleWhenNothingElseSaysIt(t *testing.T) {
	long := "internal/cli/daemon_dispatch_tracked.go:1018 vs :1027: repoSlots is recomputed unlocked between the clamp and the selection, so the slots value is decorative."
	quote := "\n> " + long + "\n"
	for _, tc := range []struct {
		name, title, detail, file string
		wantQuote                 string
		wantAbsent                string
	}{
		{name: "no detail", title: long, file: "internal/cli/daemon_dispatch_tracked.go", wantQuote: quote},
		{name: "locator detail", title: long, detail: "internal/cli/daemon_dispatch_tracked.go:1018-1027 (dispatchTracked)",
			file: "internal/cli/daemon_dispatch_tracked.go", wantQuote: "\n> " + long + "\n> \n> internal/cli/daemon_dispatch_tracked.go:1018-1027 (dispatchTracked)\n"},
		{name: "structural locator", title: long, detail: "internal/x.go:12", file: "internal/y.go", wantQuote: "\n> " + long + "\n> \n> internal/x.go:12\n"},
		{name: "real detail", title: long, detail: "The clamp and the selection read the tracker twice.", file: "internal/cli/daemon_dispatch_tracked.go",
			wantQuote: "\n> The clamp and the selection read the tracker twice.\n", wantAbsent: quote},
		{name: "short title", title: "Name the seam", detail: "internal/x.go:12", file: "internal/x.go", wantQuote: "\n> internal/x.go:12\n", wantAbsent: "> Name the seam"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := designNotesBlock(1, db.DesignNoteCandidate{FindingUID: "o/r#1-f1", Title: tc.title, Detail: tc.detail, File: tc.file}, 1, designNotesMerged, "lead", "")
			if !strings.Contains(block, tc.wantQuote) {
				t.Fatalf("block lacks %q:\n%s", tc.wantQuote, block)
			}
			if tc.wantAbsent != "" && strings.Contains(block, tc.wantAbsent) {
				t.Fatalf("block carries %q:\n%s", tc.wantAbsent, block)
			}
		})
	}
}

// #2566: the writer keeps the state the reviewer declared beside the one it
// records, and #2376's {severity, locator, finding, impact} shape keeps its
// prose as the detail instead of the bare prose locator.
func TestAdvanceJobRecordsDeclaredStateAndKeepsFindingProseOverALocator(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("c", 40)
	const findingProse = "LoadCleanupBudget is not among the parsers validateConfigFile runs, so a bad budget passes validation."

	insertCompletedJob(t, store, db.Job{ID: "review-2566", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2566", PullRequest: 2566, HeadSHA: head,
		TaskID: "task-2566", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "static review", Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{
				json.RawMessage(`{"severity":"P3","state":"answered","title":"Resolved: the guard is back","detail":"fixed at this head"}`),
				json.RawMessage(`{"severity":"P3","locator":"internal/config/edit.go:107-137 (validateConfigFile)","finding":"` + findingProse + `","impact":"low"}`),
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-2566"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2566)
	if err != nil || len(observations) != 2 {
		t.Fatalf("observations = %+v, %v; want 2", observations, err)
	}
	answered, sparse := observations[0], observations[1]
	// The gate still sees an open finding; the reviewer's word is kept beside it.
	if answered.State != db.FindingOpen || answered.DeclaredState != db.FindingAnswered {
		t.Fatalf("declared answer recorded as state %q, declared %q; want open, answered", answered.State, answered.DeclaredState)
	}
	if sparse.State != db.FindingOpen || sparse.DeclaredState != db.FindingOpen {
		t.Fatalf("reported finding recorded as state %q, declared %q; want open, open", sparse.State, sparse.DeclaredState)
	}
	if sparse.Detail != findingProse || sparse.File != "internal/config/edit.go" {
		t.Fatalf("#2376 shape recorded detail %q, file %q; want the finding prose and the locator's path", sparse.Detail, sparse.File)
	}
}
