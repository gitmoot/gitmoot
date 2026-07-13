package db

import (
	"context"
	"testing"
)

func TestDashboardChangeCursor(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()

	events, notes, err := store.DashboardChangeCursor(ctx)
	if err != nil {
		t.Fatalf("DashboardChangeCursor(empty): %v", err)
	}
	if events != 0 || notes != 0 {
		t.Fatalf("empty cursor = %d.%d, want 0.0", events, notes)
	}

	if err := store.CreateJobWithEvent(ctx,
		Job{ID: "job-1", Agent: "worker", Type: "ask", State: "queued"},
		JobEvent{Kind: "queued", Message: "created"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	events, notes, err = store.DashboardChangeCursor(ctx)
	if err != nil || events != 1 || notes != 0 {
		t.Fatalf("event cursor = %d.%d, err=%v, want 1.0", events, notes, err)
	}

	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: "release/one", Body: "checkpoint"}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	events, notes, err = store.DashboardChangeCursor(ctx)
	if err != nil || events != 1 || notes != 1 {
		t.Fatalf("note cursor = %d.%d, err=%v, want 1.1", events, notes, err)
	}

	events2, notes2, err := store.DashboardChangeCursor(ctx)
	if err != nil || events2 != events || notes2 != notes {
		t.Fatalf("repeat cursor = %d.%d, err=%v, want %d.%d", events2, notes2, err, events, notes)
	}
}
