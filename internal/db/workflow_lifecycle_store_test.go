package db

import (
	"context"
	"errors"
	"strings"
	"testing"
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
