package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
)

func seedGhostSessionJob(t *testing.T, store *Store, id, workflowID string, updatedAt time.Time) {
	t.Helper()
	payload := fmt.Sprintf(`{"repo":"acme/widget","workflow_id":%q}`, workflowID)
	if err := store.CreateExternallyDrivenJobWithEvent(context.Background(), Job{
		ID:      id,
		Agent:   "coordinator",
		Type:    "ask",
		State:   "running",
		Payload: payload,
	}, JobEvent{Kind: "running", Message: "job started"}); err != nil {
		t.Fatalf("CreateExternallyDrivenJobWithEvent(%s): %v", id, err)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE jobs SET updated_at = ? WHERE id = ?`,
		updatedAt.UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("backdate job %s: %v", id, err)
	}
}

func seedGhostSessionWorkflowStatus(t *testing.T, store *Store, workflowID string, status WorkflowStatus, updatedAt time.Time) {
	t.Helper()
	if _, err := store.InsertWorkflowNoteWithMeta(context.Background(),
		WorkflowNote{WorkflowID: workflowID, Author: "operator", Body: "lifecycle"},
		WorkflowMeta{Status: string(status), StatusSet: true}); err != nil {
		t.Fatalf("InsertWorkflowNoteWithMeta(%s): %v", workflowID, err)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE workflow_meta SET updated_at = ? WHERE workflow_id = ?`,
		updatedAt.UTC().Format(time.RFC3339Nano), workflowID); err != nil {
		t.Fatalf("backdate workflow %s: %v", workflowID, err)
	}
}

func assertGhostSessionJob(t *testing.T, store *Store, id, wantState, wantMessage string, wantEvents int) {
	t.Helper()
	job, err := store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", id, err)
	}
	if job.State != wantState {
		t.Fatalf("job %s state = %q, want %q", id, job.State, wantState)
	}
	events, err := store.ListJobEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", id, err)
	}
	if len(events) != wantEvents {
		t.Fatalf("job %s events = %+v, want %d", id, events, wantEvents)
	}
	if wantMessage != "" {
		last := events[len(events)-1]
		if last.Kind != "cancelled" || last.Message != wantMessage {
			t.Fatalf("job %s final event = %+v", id, last)
		}
	}
}

func TestReapGhostSessionJobsDistinguishesGhostsFromLive(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)

	seedGhostSessionJob(t, store, "settled", "release/settled", now.Add(-10*time.Minute))
	seedGhostSessionWorkflowStatus(t, store, "release/settled", WorkflowStatusSettled, now.Add(-10*time.Minute))
	seedGhostSessionJob(t, store, "stale-unlinked", "", now.Add(-25*time.Hour))
	seedGhostSessionJob(t, store, "recent-open", "release/open-recent", now.Add(-time.Minute))
	seedGhostSessionWorkflowStatus(t, store, "release/open-recent", WorkflowStatusActive, now.Add(-time.Minute))
	seedGhostSessionJob(t, store, "stale-open", "release/open-stale", now.Add(-48*time.Hour))
	seedGhostSessionWorkflowStatus(t, store, "release/open-stale", WorkflowStatusActive, now.Add(-48*time.Hour))
	seedGhostSessionJob(t, store, "recent-unlinked", "", now.Add(-time.Minute))
	if err := store.CreateJob(ctx, Job{ID: "engine-running", Agent: "worker", Type: "ask", State: "running"}); err != nil {
		t.Fatalf("CreateJob(engine-running): %v", err)
	}

	reaped, err := store.ReapGhostSessionJobs(ctx, now, 24*time.Hour)
	if err != nil {
		t.Fatalf("ReapGhostSessionJobs: %v", err)
	}
	if fmt.Sprint(reaped) != "[stale-open stale-unlinked settled]" {
		t.Fatalf("reaped = %v", reaped)
	}
	assertGhostSessionJob(t, store, "settled", "cancelled", "reaped: parent workflow release/settled settled", 2)
	assertGhostSessionJob(t, store, "stale-unlinked", "cancelled", "reaped: no activity for >24h0m0s (workflow status: none)", 2)
	assertGhostSessionJob(t, store, "recent-open", "running", "", 1)
	assertGhostSessionJob(t, store, "stale-open", "cancelled", "reaped: no activity for >24h0m0s (workflow status: active)", 2)
	assertGhostSessionJob(t, store, "recent-unlinked", "running", "", 1)
	assertGhostSessionJob(t, store, "engine-running", "running", "", 0)
}

func TestReapGhostSessionJobsReapsStaleJobUnderActiveWorkflow(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)

	seedGhostSessionJob(t, store, "stale-active", "g2/130-org-canvas-rebuild", now.Add(-48*time.Hour))
	seedGhostSessionWorkflowStatus(t, store, "g2/130-org-canvas-rebuild", WorkflowStatusActive, now.Add(-time.Minute))

	reaped, err := store.ReapGhostSessionJobs(ctx, now, 24*time.Hour)
	if err != nil {
		t.Fatalf("ReapGhostSessionJobs: %v", err)
	}
	if fmt.Sprint(reaped) != "[stale-active]" {
		t.Fatalf("reaped = %v", reaped)
	}
	assertGhostSessionJob(t, store, "stale-active", "cancelled",
		"reaped: no activity for >24h0m0s (workflow status: active)", 2)
}

func TestReapGhostSessionJobsZeroDisablesOnlyAgeFallback(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	seedGhostSessionJob(t, store, "settled", "release/done", now.Add(-10*time.Minute))
	seedGhostSessionWorkflowStatus(t, store, "release/done", WorkflowStatusDone, now.Add(-10*time.Minute))
	seedGhostSessionJob(t, store, "stale-unlinked", "", now.Add(-72*time.Hour))

	reaped, err := store.ReapGhostSessionJobs(ctx, now, 0)
	if err != nil {
		t.Fatalf("ReapGhostSessionJobs: %v", err)
	}
	if fmt.Sprint(reaped) != "[settled]" {
		t.Fatalf("reaped = %v", reaped)
	}
	assertGhostSessionJob(t, store, "settled", "cancelled", "reaped: parent workflow release/done settled", 2)
	assertGhostSessionJob(t, store, "stale-unlinked", "running", "", 1)
}

func TestBackfillGhostSessionJobsReusesReaperAndIsIdempotent(t *testing.T) {
	store, err := openRealTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Now().UTC()
	seedGhostSessionJob(t, store, "settled", "release/settled", old)
	seedGhostSessionWorkflowStatus(t, store, "release/settled", WorkflowStatusSettled, old)
	seedGhostSessionJob(t, store, "stale-unlinked", "", old)
	seedGhostSessionJob(t, store, "recent-unlinked", "", recent)
	seedGhostSessionJob(t, store, "stale-open", "release/open", old)
	seedGhostSessionWorkflowStatus(t, store, "release/open", WorkflowStatusActive, old)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertGhostSessionJob(t, store, "settled", "cancelled", "reaped: parent workflow release/settled settled", 2)
	assertGhostSessionJob(t, store, "stale-unlinked", "cancelled", "reaped: no activity for >24h0m0s (workflow status: none)", 2)
	assertGhostSessionJob(t, store, "recent-unlinked", "running", "", 1)
	assertGhostSessionJob(t, store, "stale-open", "cancelled", "reaped: no activity for >24h0m0s (workflow status: active)", 2)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	assertGhostSessionJob(t, store, "settled", "cancelled", "reaped: parent workflow release/settled settled", 2)
	assertGhostSessionJob(t, store, "stale-unlinked", "cancelled", "reaped: no activity for >24h0m0s (workflow status: none)", 2)
	assertGhostSessionJob(t, store, "stale-open", "cancelled", "reaped: no activity for >24h0m0s (workflow status: active)", 2)
}

func TestBackfillGhostSessionJobsHonorsDisabledAgePolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), config.DirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(`
[workflow]
auto_settle_after = "0"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := openRealTestStore(t, filepath.Join(root, config.DBName))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	seedGhostSessionJob(t, store, "stale-unlinked", "", old)
	seedGhostSessionJob(t, store, "settled", "release/done", old)
	seedGhostSessionWorkflowStatus(t, store, "release/done", WorkflowStatusDone, old)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertGhostSessionJob(t, store, "stale-unlinked", "running", "", 1)
	assertGhostSessionJob(t, store, "settled", "cancelled", "reaped: parent workflow release/done settled", 2)
}
