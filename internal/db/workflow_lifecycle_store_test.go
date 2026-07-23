package db

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestValidateWorkflowStatus(t *testing.T) {
	for _, status := range []string{"", "active", "blocked", "ready_to_merge", "done", "settled", "parked"} {
		t.Run("valid_"+status, func(t *testing.T) {
			if err := ValidateWorkflowStatus(status); err != nil {
				t.Fatalf("ValidateWorkflowStatus(%q): %v", status, err)
			}
		})
	}
	for _, status := range []string{"garbage", "PR #1 open", "recent"} {
		t.Run("invalid_"+status, func(t *testing.T) {
			err := ValidateWorkflowStatus(status)
			if err == nil || !strings.Contains(err.Error(), "active, blocked, ready_to_merge, done, settled, parked") {
				t.Fatalf("ValidateWorkflowStatus(%q) error = %v", status, err)
			}
		})
	}
	store := openWorkflowTestStore(t)
	if _, err := store.InsertWorkflowNoteWithMeta(context.Background(),
		WorkflowNote{WorkflowID: "release/invalid-status", Body: "invalid"},
		WorkflowMeta{Status: "garbage", StatusSet: true}); err == nil ||
		!strings.Contains(err.Error(), "workflow status must be empty or one of") {
		t.Fatalf("store write invalid status error = %v", err)
	}
}

func TestWorkflowLegacyStatusRemainsReadable(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	seedWorkflowJob(t, store, "legacy-job", "release/legacy", "succeeded", "acme/widget", 0, 0)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO workflow_meta(workflow_id, status) VALUES (?, ?)`,
		"release/legacy", "PR #1 open"); err != nil {
		t.Fatalf("seed legacy metadata: %v", err)
	}
	summaries, err := store.ListWorkflowSummaries(ctx)
	if err != nil || len(summaries) != 1 || summaries[0].WorkflowID != "release/legacy" {
		t.Fatalf("ListWorkflowSummaries = %+v, err=%v", summaries, err)
	}
	meta, err := store.GetWorkflowMeta(ctx, "release/legacy")
	if err != nil || meta.Status != "PR #1 open" {
		t.Fatalf("GetWorkflowMeta = %+v, err=%v", meta, err)
	}
}

func TestCloseWorkflowRecordsDoneAndIsIdempotent(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	seedWorkflowJob(t, store, "done-job", "release/done", "succeeded", "acme/widget", 0, 0)

	result, err := store.CloseWorkflow(ctx, "release/done", "shipped successfully")
	if err != nil {
		t.Fatalf("CloseWorkflow: %v", err)
	}
	if result.WorkflowID != "release/done" || result.Status != WorkflowStatusDone || result.AlreadyTerminal ||
		result.Note == nil || result.Note.Body != "[workflow:close] shipped successfully" ||
		result.Note.Author != WorkflowAutoNoteAuthor {
		t.Fatalf("close result = %+v", result)
	}
	meta, err := store.GetWorkflowMeta(ctx, "release/done")
	if err != nil || meta.Status != string(WorkflowStatusDone) {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}

	replay, err := store.CloseWorkflow(ctx, "release/done", "duplicate")
	if err != nil || !replay.AlreadyTerminal || replay.Status != WorkflowStatusDone || replay.Note != nil {
		t.Fatalf("replayed close = %+v, err=%v", replay, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, "release/done", 0)
	if err != nil || len(notes) != 1 || notes[0].Body != "[workflow:close] shipped successfully" ||
		notes[0].Author != WorkflowAutoNoteAuthor {
		t.Fatalf("notes after replay = %+v, err=%v", notes, err)
	}
}

func TestCloseWorkflowPreservesAlreadySettledWithoutCloseNote(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const label = "release/settled-close"
	seedWorkflowJob(t, store, "settled-close-job", label, "succeeded", "acme/widget", 0, 0)
	if _, err := store.InsertWorkflowNoteWithMeta(ctx,
		WorkflowNote{WorkflowID: label, Author: WorkflowAutoNoteAuthor, Body: "auto-settled"},
		WorkflowMeta{Status: string(WorkflowStatusSettled), StatusSet: true}); err != nil {
		t.Fatalf("seed settled: %v", err)
	}
	result, err := store.CloseWorkflow(ctx, label, "late close")
	if err != nil || !result.AlreadyTerminal || result.Status != WorkflowStatusSettled || result.Note != nil {
		t.Fatalf("CloseWorkflow = %+v, err=%v", result, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, label, 0)
	if err != nil || len(notes) != 1 || notes[0].Body != "auto-settled" {
		t.Fatalf("notes = %+v, err=%v", notes, err)
	}
}

func TestCloseWorkflowRefusesLiveJobsWithoutChanges(t *testing.T) {
	for _, state := range []string{"queued", "running"} {
		t.Run(state, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			label := "release/" + state
			seedWorkflowJob(t, store, state+"-job", label, state, "acme/widget", 0, 0)
			if _, err := store.InsertWorkflowNoteWithMeta(ctx,
				WorkflowNote{WorkflowID: label, Author: "operator", Body: "in progress"},
				WorkflowMeta{Status: string(WorkflowStatusActive), StatusSet: true}); err != nil {
				t.Fatalf("seed metadata: %v", err)
			}

			if _, err := store.CloseWorkflow(ctx, label, "not yet"); err == nil ||
				!strings.Contains(err.Error(), "1 job(s) still queued/running") {
				t.Fatalf("CloseWorkflow error = %v", err)
			}
			meta, err := store.GetWorkflowMeta(ctx, label)
			if err != nil || meta.Status != string(WorkflowStatusActive) {
				t.Fatalf("meta changed = %+v, err=%v", meta, err)
			}
			notes, err := store.ListWorkflowNotes(ctx, label, 0)
			if err != nil || len(notes) != 1 || notes[0].Body != "in progress" {
				t.Fatalf("notes changed = %+v, err=%v", notes, err)
			}
		})
	}
}

func TestCloseWorkflowUnknownLabelReturnsTypedError(t *testing.T) {
	store := openWorkflowTestStore(t)
	_, err := store.CloseWorkflow(context.Background(), "release/missing", "")
	var unknown *UnknownWorkflowError
	if !errors.As(err, &unknown) || unknown.Label != "release/missing" {
		t.Fatalf("CloseWorkflow error = %T %v", err, err)
	}
}

func TestWorkflowPlainNoteReopensTerminalStatus(t *testing.T) {
	for _, terminal := range []WorkflowStatus{WorkflowStatusDone, WorkflowStatusSettled} {
		t.Run(string(terminal), func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			label := "release/" + string(terminal)
			if _, err := store.InsertWorkflowNoteWithMeta(ctx,
				WorkflowNote{WorkflowID: label, Author: "operator", Body: "terminal seed"},
				WorkflowMeta{Status: string(terminal), StatusSet: true}); err != nil {
				t.Fatalf("seed terminal metadata: %v", err)
			}
			const body = "verbatim human note\nwith a second line"
			human, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: label, Author: "human", Body: body})
			if err != nil {
				t.Fatalf("InsertWorkflowNote: %v", err)
			}
			notes, err := store.ListWorkflowNotes(ctx, label, 0)
			if err != nil || len(notes) != 3 {
				t.Fatalf("notes = %+v, err=%v", notes, err)
			}
			if notes[1].Author != WorkflowAutoNoteAuthor ||
				notes[1].Body != "[auto:workflow:reopened] reopened from "+string(terminal) ||
				notes[2].ID != human.ID || notes[2].Body != body {
				t.Fatalf("reopen note ordering/content = %+v", notes)
			}
			meta, err := store.GetWorkflowMeta(ctx, label)
			if err != nil || meta.Status != string(WorkflowStatusActive) {
				t.Fatalf("meta = %+v, err=%v", meta, err)
			}
			summary, err := store.WorkflowSummary(ctx, label)
			if err != nil || summary.LastNote != body || summary.LastHumanAuthor != "human" {
				t.Fatalf("summary = %+v, err=%v", summary, err)
			}
		})
	}
}

func TestWorkflowExplicitStatusAndMachineReceiptDoNotImplicitlyReopen(t *testing.T) {
	t.Run("explicit status", func(t *testing.T) {
		store := openWorkflowTestStore(t)
		ctx := context.Background()
		const label = "release/explicit"
		if _, err := store.InsertWorkflowNoteWithMeta(ctx,
			WorkflowNote{WorkflowID: label, Body: "settled seed"},
			WorkflowMeta{Status: string(WorkflowStatusSettled), StatusSet: true}); err != nil {
			t.Fatalf("seed settled: %v", err)
		}
		if _, err := store.InsertWorkflowNoteWithMeta(ctx,
			WorkflowNote{WorkflowID: label, Author: "operator", Body: "blocked again"},
			WorkflowMeta{Status: string(WorkflowStatusBlocked), StatusSet: true}); err != nil {
			t.Fatalf("explicit status note: %v", err)
		}
		meta, err := store.GetWorkflowMeta(ctx, label)
		if err != nil || meta.Status != string(WorkflowStatusBlocked) {
			t.Fatalf("meta = %+v, err=%v", meta, err)
		}
		assertNoWorkflowReopenReceipt(t, store, label)
	})

	t.Run("machine PR receipt", func(t *testing.T) {
		store := openWorkflowTestStore(t)
		ctx := context.Background()
		const label = "release/machine"
		if _, err := store.InsertWorkflowNoteWithMeta(ctx,
			WorkflowNote{WorkflowID: label, Body: "settled seed"},
			WorkflowMeta{Status: string(WorkflowStatusSettled), StatusSet: true}); err != nil {
			t.Fatalf("seed settled: %v", err)
		}
		if _, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
			WorkflowNote{WorkflowID: label, Author: WorkflowAutoNoteAuthor, Body: "[auto:pr:7:merged] PR #7 merged"},
			WorkflowMeta{Status: string(WorkflowStatusActive), StatusSet: true}); err != nil || !inserted {
			t.Fatalf("InsertWorkflowAutoNoteWithMeta = (inserted=%v, err=%v)", inserted, err)
		}
		meta, err := store.GetWorkflowMeta(ctx, label)
		if err != nil || meta.Status != string(WorkflowStatusSettled) {
			t.Fatalf("machine receipt changed terminal status: %+v, err=%v", meta, err)
		}
		assertNoWorkflowReopenReceipt(t, store, label)
	})
}

func TestWorkflowObservationNoteReopensAtomically(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const label = "release/observation"
	if _, err := store.InsertWorkflowNoteWithMeta(ctx,
		WorkflowNote{WorkflowID: label, Body: "done seed"},
		WorkflowMeta{Status: string(WorkflowStatusDone), StatusSet: true}); err != nil {
		t.Fatalf("seed done: %v", err)
	}
	note, obs, err := store.InsertWorkflowNoteWithObservationAndMeta(ctx,
		WorkflowNote{WorkflowID: label, Author: "human", Body: "durable fact", Repo: "acme/widget"},
		MemoryObservation{
			Owner: MemoryOwner{Kind: "shared", Ref: "shared"}, AuthorRef: "human",
			Repo: "acme/widget", Scope: "repo", Content: "durable fact", TrustMark: "low",
		},
		WorkflowMeta{Author: "human"})
	if err != nil || note.MemoryObservationID == 0 || obs.ID != note.MemoryObservationID {
		t.Fatalf("observation note = %+v, observation=%+v, err=%v", note, obs, err)
	}
	meta, err := store.GetWorkflowMeta(ctx, label)
	if err != nil || meta.Status != string(WorkflowStatusActive) {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, label, 0)
	if err != nil || len(notes) != 3 ||
		notes[1].Body != "[auto:workflow:reopened] reopened from done" ||
		notes[2].ID != note.ID {
		t.Fatalf("notes = %+v, err=%v", notes, err)
	}
}

func assertNoWorkflowReopenReceipt(t *testing.T, store *Store, label string) {
	t.Helper()
	notes, err := store.ListWorkflowNotes(context.Background(), label, 0)
	if err != nil {
		t.Fatalf("ListWorkflowNotes: %v", err)
	}
	for _, note := range notes {
		if strings.HasPrefix(note.Body, "[auto:workflow:reopened]") {
			t.Fatalf("unexpected reopen receipt: %+v", notes)
		}
	}
}

func TestWorkflowPullRequestRefsUnionsJobsAndAutoNotes(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const label = "release/refs"
	seedAutoSettleJob(t, store, "ref-job", label, "succeeded", "acme/widget", 5)
	if _, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
		WorkflowNote{
			WorkflowID: label,
			Author:     WorkflowAutoNoteAuthor,
			Body:       "[auto:pr:7:merged] PR #7 merged",
		},
		WorkflowMeta{Status: string(WorkflowStatusActive), StatusSet: true}); err != nil || !inserted {
		t.Fatalf("insert fallback-repo receipt = (inserted=%v, err=%v)", inserted, err)
	}
	if _, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
		WorkflowNote{
			WorkflowID: label,
			Author:     WorkflowAutoNoteAuthor,
			Body:       "[auto:pr:9:closed] PR #9 closed",
			Repo:       "acme/other",
		},
		WorkflowMeta{Status: string(WorkflowStatusActive), StatusSet: true}); err != nil || !inserted {
		t.Fatalf("insert explicit-repo receipt = (inserted=%v, err=%v)", inserted, err)
	}
	refs, err := store.WorkflowPullRequestRefs(ctx, label)
	want := []PullRequestRef{
		{Repo: "acme/other", Number: 9},
		{Repo: "acme/widget", Number: 5},
		{Repo: "acme/widget", Number: 7},
	}
	if err != nil || !reflect.DeepEqual(refs, want) {
		t.Fatalf("WorkflowPullRequestRefs = %+v, err=%v, want %+v", refs, err, want)
	}
}

func TestWorkflowPullRequestRefsKeepsAmbiguousRepoUnresolved(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const label = "release/ambiguous"
	seedAutoSettleJob(t, store, "ref-a", label, "succeeded", "acme/a", 0)
	seedAutoSettleJob(t, store, "ref-b", label, "succeeded", "acme/b", 0)
	if _, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
		WorkflowNote{
			WorkflowID: label,
			Author:     WorkflowAutoNoteAuthor,
			Body:       "[auto:pr:7:merged] PR #7 merged",
		},
		WorkflowMeta{Status: string(WorkflowStatusActive), StatusSet: true}); err != nil || !inserted {
		t.Fatalf("insert receipt = (inserted=%v, err=%v)", inserted, err)
	}
	refs, err := store.WorkflowPullRequestRefs(ctx, label)
	if err != nil || len(refs) != 1 || refs[0] != (PullRequestRef{Number: 7}) {
		t.Fatalf("WorkflowPullRequestRefs = %+v, err=%v", refs, err)
	}
	settled, err := store.SettleWorkflowIfEligible(ctx, label, time.Now().UTC().Add(48*time.Hour), 24*time.Hour)
	if err != nil || settled {
		t.Fatalf("ambiguous ref settled=%v, err=%v", settled, err)
	}
}

func TestSettleWorkflowIfEligibleConservativeGates(t *testing.T) {
	tests := []struct {
		name       string
		jobState   string
		prState    string
		addPRRow   bool
		addPRRef   bool
		recentNote bool
		recentJob  bool
		status     string
		want       bool
	}{
		{name: "all terminal", jobState: "succeeded", prState: "merged", addPRRow: true, addPRRef: true, want: true},
		{name: "closed terminal", jobState: "succeeded", prState: "closed", addPRRow: true, addPRRef: true, want: true},
		{name: "open PR", jobState: "succeeded", prState: "open", addPRRow: true, addPRRef: true},
		{name: "missing PR row", jobState: "succeeded", addPRRef: true},
		{name: "zero PR refs", jobState: "succeeded"},
		{name: "queued job", jobState: "queued", prState: "merged", addPRRow: true, addPRRef: true},
		{name: "running job", jobState: "running", prState: "merged", addPRRow: true, addPRRef: true},
		{name: "recent human note", jobState: "succeeded", prState: "merged", addPRRow: true, addPRRef: true, recentNote: true},
		{name: "recent job update", jobState: "succeeded", prState: "merged", addPRRow: true, addPRRef: true, recentJob: true},
		{name: "blocked status", jobState: "succeeded", prState: "merged", addPRRow: true, addPRRef: true, status: "blocked"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			label := "release/" + strings.ReplaceAll(test.name, " ", "-")
			pr := 0
			if test.addPRRef {
				pr = 11
			}
			seedAutoSettleJob(t, store, "job-"+label, label, test.jobState, "acme/widget", pr)
			note := WorkflowNote{WorkflowID: label, Author: "operator", Body: "human checkpoint"}
			if test.status != "" {
				if _, err := store.InsertWorkflowNoteWithMeta(ctx, note,
					WorkflowMeta{Status: test.status, StatusSet: true}); err != nil {
					t.Fatalf("insert human note: %v", err)
				}
			} else if _, err := store.InsertWorkflowNote(ctx, note); err != nil {
				t.Fatalf("insert human note: %v", err)
			}
			if test.addPRRow {
				seedAutoSettlePullRequest(t, store, "acme/widget", int64(pr), test.prState)
			}
			now := time.Now().UTC().Add(72 * time.Hour)
			if test.recentNote {
				setWorkflowNoteTimes(t, store, label, "operator", now.Add(-time.Hour))
			}
			if test.recentJob {
				setWorkflowJobTime(t, store, "job-"+label, now.Add(-time.Hour))
			}
			settled, err := store.SettleWorkflowIfEligible(ctx, label, now, 24*time.Hour)
			if err != nil || settled != test.want {
				t.Fatalf("SettleWorkflowIfEligible = %v, err=%v, want %v", settled, err, test.want)
			}
		})
	}
}

func TestSettleWorkflowIfEligibleRequiresEveryReferencedPRTerminal(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const label = "release/multi-pr"
	seedAutoSettleJob(t, store, "multi-pr-job", label, "succeeded", "acme/widget", 1)
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: label, Author: "operator", Body: "old"}); err != nil {
		t.Fatalf("insert human note: %v", err)
	}
	if _, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
		WorkflowNote{
			WorkflowID: label,
			Author:     WorkflowAutoNoteAuthor,
			Body:       "[auto:pr:2:opened] PR #2 opened",
			Repo:       "acme/widget",
		},
		WorkflowMeta{Status: string(WorkflowStatusActive), StatusSet: true}); err != nil || !inserted {
		t.Fatalf("insert second PR receipt = (inserted=%v, err=%v)", inserted, err)
	}
	seedAutoSettlePullRequest(t, store, "acme/widget", 1, "merged")
	seedAutoSettlePullRequest(t, store, "acme/widget", 2, "open")
	now := time.Now().UTC().Add(72 * time.Hour)
	if settled, err := store.SettleWorkflowIfEligible(ctx, label, now, 24*time.Hour); err != nil || settled {
		t.Fatalf("settled with one open ref = %v, err=%v", settled, err)
	}
	seedAutoSettlePullRequest(t, store, "acme/widget", 2, "closed")
	if settled, err := store.SettleWorkflowIfEligible(ctx, label, now, 24*time.Hour); err != nil || !settled {
		t.Fatalf("settled after all refs terminal = %v, err=%v", settled, err)
	}
}

func TestSettleWorkflowIgnoresRecentDaemonNoteAndIsIdempotent(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const label = "release/daemon-quiet"
	seedAutoSettleJob(t, store, "daemon-quiet-job", label, "succeeded", "acme/widget", 17)
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: label, Author: "operator", Body: "old human"}); err != nil {
		t.Fatalf("insert human note: %v", err)
	}
	if _, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
		WorkflowNote{
			WorkflowID: label,
			Author:     WorkflowAutoNoteAuthor,
			Body:       "[auto:pr:17:merged] PR #17 merged",
			Repo:       "acme/widget",
		},
		WorkflowMeta{Status: string(WorkflowStatusActive), StatusSet: true}); err != nil || !inserted {
		t.Fatalf("insert daemon receipt = (inserted=%v, err=%v)", inserted, err)
	}
	seedAutoSettlePullRequest(t, store, "acme/widget", 17, "merged")
	now := time.Now().UTC().Add(72 * time.Hour)
	setWorkflowNoteTimes(t, store, label, WorkflowAutoNoteAuthor, now.Add(-time.Minute))

	candidates, err := store.ListWorkflowAutoSettleCandidates(ctx)
	if err != nil || len(candidates) != 1 || candidates[0].WorkflowID != label ||
		now.Sub(candidates[0].QuietAnchor) < 24*time.Hour {
		t.Fatalf("candidates = %+v, err=%v", candidates, err)
	}
	settled, err := store.SettleWorkflowIfEligible(ctx, label, now, 24*time.Hour)
	if err != nil || !settled {
		t.Fatalf("first settle = %v, err=%v", settled, err)
	}
	settled, err = store.SettleWorkflowIfEligible(ctx, label, now.Add(time.Hour), 24*time.Hour)
	if err != nil || settled {
		t.Fatalf("repeat settle = %v, err=%v", settled, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, label, 0)
	if err != nil {
		t.Fatalf("ListWorkflowNotes: %v", err)
	}
	settleNotes := 0
	for _, note := range notes {
		if note.Body == "[auto:workflow:settled] merged/closed PRs, quiet ≥ 24h0m0s" {
			settleNotes++
		}
	}
	if settleNotes != 1 {
		t.Fatalf("settle note count = %d, notes=%+v", settleNotes, notes)
	}

	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: label, Author: "operator", Body: "new work"}); err != nil {
		t.Fatalf("reopen with human note: %v", err)
	}
	meta, err := store.GetWorkflowMeta(ctx, label)
	if err != nil || meta.Status != string(WorkflowStatusActive) {
		t.Fatalf("meta after reopen = %+v, err=%v", meta, err)
	}
}

func TestListWorkflowAutoSettleCandidatesExcludesProtectedStates(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	for _, status := range []WorkflowStatus{
		WorkflowStatusParked,
		WorkflowStatusDone,
		WorkflowStatusSettled,
		WorkflowStatusBlocked,
	} {
		label := "release/" + string(status)
		seedAutoSettleJob(t, store, label+"-job", label, "succeeded", "acme/widget", 1)
		if _, err := store.InsertWorkflowNoteWithMeta(ctx,
			WorkflowNote{WorkflowID: label, Author: "operator", Body: "protected"},
			WorkflowMeta{Status: string(status), StatusSet: true}); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
	}
	// A journal-only coordinator workflow with no PR reference can never settle,
	// so it must be excluded from candidates (keeps the per-tick sweep bounded).
	seedAutoSettleJob(t, store, "journal-job", "release/journal-only", "succeeded", "acme/widget", 0)
	seedAutoSettleJob(t, store, "active-job", "release/active-candidate", "succeeded", "acme/widget", 1)
	candidates, err := store.ListWorkflowAutoSettleCandidates(ctx)
	if err != nil || len(candidates) != 1 || candidates[0].WorkflowID != "release/active-candidate" {
		t.Fatalf("candidates = %+v, err=%v", candidates, err)
	}
}

func seedAutoSettleJob(t *testing.T, store *Store, id, workflowID, state, repo string, pullRequest int) {
	t.Helper()
	payload := fmt.Sprintf(`{"repo":%q,"workflow_id":%q,"pull_request":%d}`, repo, workflowID, pullRequest)
	if err := store.CreateJob(context.Background(), Job{
		ID: id, Agent: "worker", Type: "ask", State: state, Payload: payload,
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

func seedAutoSettlePullRequest(t *testing.T, store *Store, repo string, number int64, state string) {
	t.Helper()
	if err := store.UpsertPullRequest(context.Background(), PullRequest{
		RepoFullName: repo,
		Number:       number,
		URL:          fmt.Sprintf("https://example.invalid/%s/pull/%d", repo, number),
		HeadBranch:   fmt.Sprintf("feature/%d", number),
		BaseBranch:   "main",
		State:        state,
	}); err != nil {
		t.Fatalf("UpsertPullRequest(%s#%d): %v", repo, number, err)
	}
}

func setWorkflowNoteTimes(t *testing.T, store *Store, workflowID, author string, at time.Time) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `UPDATE workflow_notes
SET created_at = ? WHERE workflow_id = ? AND author = ?`,
		at.UTC().Format("2006-01-02 15:04:05"), workflowID, author); err != nil {
		t.Fatalf("set note times: %v", err)
	}
}

func setWorkflowJobTime(t *testing.T, store *Store, jobID string, at time.Time) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `UPDATE jobs SET updated_at = ? WHERE id = ?`,
		at.UTC().Format("2006-01-02 15:04:05"), jobID); err != nil {
		t.Fatalf("set job time: %v", err)
	}
}
