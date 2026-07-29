package cli

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/evidence"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestReviewStatusReusedLabelAndExactBoundary(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	note, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "review/reused",
		Body:       "older review activity",
	})
	if err != nil {
		t.Fatalf("InsertWorkflowNote returned error: %v", err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()

	now := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)
	setNoteAt := func(at time.Time) {
		t.Helper()
		if _, err := raw.Exec(`UPDATE workflow_notes SET created_at = ? WHERE id = ?`,
			at.Format("2006-01-02 15:04:05"), note.ID); err != nil {
			t.Fatalf("set note timestamp: %v", err)
		}
	}
	jobAt := func(at time.Time) db.Job {
		return db.Job{
			ID:               "session-review",
			Type:             "review",
			State:            string(workflow.JobRunning),
			ExternallyDriven: true,
			WorkflowID:       "review/reused",
			CreatedAt:        at.Format("2006-01-02 15:04:05"),
		}
	}

	// A note left by an earlier review under the reused label cannot make the
	// newer review look older than its own open time.
	setNoteAt(now.Add(-2 * sessionReviewStaleGap))
	fresh := deriveReviewStatus(context.Background(), store, jobAt(now.Add(-10*time.Minute)), now)
	if fresh.Status != reviewStatusInProgress {
		t.Fatalf("fresh review with older note = %+v, want in_progress", fresh)
	}

	// The boundary is strict: exactly 20 minutes is active; one second past is
	// stalled. Both remain visibly reported/non-authoritative.
	setNoteAt(now.Add(-sessionReviewStaleGap))
	exact := deriveReviewStatus(context.Background(), store, jobAt(now.Add(-time.Hour)), now)
	if exact.Status != reviewStatusInProgress {
		t.Fatalf("review at exact stale gap = %+v, want in_progress", exact)
	}
	setNoteAt(now.Add(-sessionReviewStaleGap - time.Second))
	past := deriveReviewStatus(context.Background(), store, jobAt(now.Add(-time.Hour)), now)
	if past.Status != reviewStatusStalled ||
		past.Grade != evidence.GradeReported ||
		past.Authority != reviewStatusAuthorityNonAuthoritative {
		t.Fatalf("review past stale gap = %+v, want stalled/reported/non_authoritative", past)
	}
}
