package db

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"
)

func TestAddressedWorkflowNoteWritesOutboxThroughEveryInsertEntryPoint(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	observation := func(content string) MemoryObservation {
		return MemoryObservation{
			Owner: MemoryOwner{Kind: "shared", Ref: "shared"}, AuthorRef: "operator",
			Repo: "owner/repo", Scope: "repo", Content: content, TrustMark: "low",
		}
	}

	var noteIDs []int64
	first, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wake/simple", Author: "operator", Body: "simple",
		AddressedTarget: "OWNER",
	})
	if err != nil {
		t.Fatal(err)
	}
	noteIDs = append(noteIDs, first.ID)

	second, err := store.InsertWorkflowNoteWithMeta(ctx, WorkflowNote{
		WorkflowID: "wake/meta", Author: "operator", Body: "meta",
		AddressedTarget: "owner",
	}, WorkflowMeta{Summary: "summary", SummarySet: true})
	if err != nil {
		t.Fatal(err)
	}
	noteIDs = append(noteIDs, second.ID)

	third, _, err := store.InsertWorkflowNoteWithObservation(ctx, WorkflowNote{
		WorkflowID: "wake/observation", Author: "operator", Body: "observation",
		Repo: "owner/repo", AddressedTarget: "owner",
	}, observation("observation"))
	if err != nil {
		t.Fatal(err)
	}
	noteIDs = append(noteIDs, third.ID)

	fourth, _, err := store.InsertWorkflowNoteWithObservationAndMeta(ctx, WorkflowNote{
		WorkflowID: "wake/both", Author: "operator", Body: "both",
		Repo: "owner/repo", AddressedTarget: "owner",
	}, observation("both"), WorkflowMeta{Description: "description", DescriptionSet: true})
	if err != nil {
		t.Fatal(err)
	}
	noteIDs = append(noteIDs, fourth.ID)

	outbox, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 4 {
		t.Fatalf("outbox rows = %+v, want four", outbox)
	}
	for index, entry := range outbox {
		if entry.SourceKind != WakeOutboxSourceWorkflowNote ||
			entry.SourceID != strconv.FormatInt(noteIDs[index], 10) ||
			entry.TargetRole != "owner" ||
			entry.CoalesceKey != WakeOutboxReplyCoalescePrefix+"owner" ||
			entry.State != WakeOutboxStatePending ||
			entry.AttemptCount != 0 || entry.AttemptedAt != "" || entry.FinishedAt != "" {
			t.Fatalf("outbox[%d] = %+v", index, entry)
		}
	}
}

func TestAddressedWorkflowNoteAndOutboxAreAtomic(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER reject_wake_outbox
BEFORE INSERT ON wake_outbox
BEGIN
	SELECT RAISE(ABORT, 'forced wake outbox failure');
END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wake/atomic", Author: "operator", Body: "must roll back",
		AddressedTarget: "owner",
	}); err == nil {
		t.Fatal("InsertWorkflowNote unexpectedly succeeded")
	}
	for _, table := range []string{"workflow_notes", "wake_outbox"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d after failed transaction, want zero", table, count)
		}
	}
}

func TestWakeOutboxClaimAndFinishStatesAreQueryable(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	note, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wake/state", Author: "operator", Body: "state",
		AddressedTarget: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListWakeOutbox(ctx, WakeOutboxStatePending)
	if err != nil || len(pending) != 1 || pending[0].SourceID != fmt.Sprint(note.ID) {
		t.Fatalf("pending = %+v, err=%v", pending, err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimWakeOutbox(ctx, []int64{pending[0].ID}, now)
	if err != nil || !claimed {
		t.Fatalf("ClaimWakeOutbox = %v, %v", claimed, err)
	}
	attempted, err := store.ListWakeOutbox(ctx, WakeOutboxStateAttempted)
	if err != nil || len(attempted) != 1 || attempted[0].AttemptCount != 1 || attempted[0].AttemptedAt == "" {
		t.Fatalf("attempted = %+v, err=%v", attempted, err)
	}
	if err := store.FinishWakeOutbox(ctx, []int64{pending[0].ID}, WakeOutboxStateDelivered, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	delivered, err := store.ListWakeOutbox(ctx, WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 || delivered[0].FinishedAt == "" {
		t.Fatalf("delivered = %+v, err=%v", delivered, err)
	}
}
