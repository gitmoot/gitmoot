package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func serviceContainsString(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func pipelineAdvanceStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestPipeline(t *testing.T, store *db.Store, name, specYAML string) (db.Pipeline, pipeline.Spec) {
	t.Helper()
	spec, err := pipeline.Load([]byte(specYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := db.Pipeline{Name: name, Repo: "owner/repo", SpecYAML: specYAML, SpecHash: pipeline.Hash([]byte(specYAML))}
	if err := store.CreateOrUpdatePipeline(context.Background(), rec); err != nil {
		t.Fatalf("CreateOrUpdatePipeline: %v", err)
	}
	got, ok, err := store.GetPipeline(context.Background(), name)
	if err != nil || !ok {
		t.Fatalf("GetPipeline: ok=%v err=%v", ok, err)
	}
	return got, spec
}

func testStageEnqueuer(store *db.Store) pipeline.PipelineStageEnqueuer {
	mailbox := workflow.Mailbox{Store: store}
	return func(ctx context.Context, request workflow.JobRequest) (db.Job, error) {
		return mailbox.Enqueue(ctx, request)
	}
}

func settleStageJob(t *testing.T, store *db.Store, jobID, decision, summary string, needs []string) {
	t.Helper()
	ctx := context.Background()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: summary, Needs: needs}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	to := jobStateForDecision(decision)
	ok, err := store.TransitionJobStatePayloadWithEvent(ctx, jobID, job.State, to, string(encoded),
		db.JobEvent{JobID: jobID, Kind: to, Message: "settled by test"})
	if err != nil || !ok {
		t.Fatalf("settle %s -> %s: ok=%v err=%v", jobID, to, ok, err)
	}
}

func startStageJob(t *testing.T, store *db.Store, jobID string) {
	t.Helper()
	ok, err := store.TransitionJobStateWithEvent(context.Background(), jobID, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{
		JobID: jobID, Kind: string(workflow.JobRunning), Message: "job started by test",
	})
	if err != nil || !ok {
		t.Fatalf("start %s: ok=%v err=%v", jobID, ok, err)
	}
}

func setPipelineJobEventTime(t *testing.T, store *db.Store, jobID, kind string, at time.Time) {
	t.Helper()
	conn, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	result, err := conn.Exec(`UPDATE job_events SET created_at = ? WHERE job_id = ? AND kind = ?`, at.UTC().Format(time.RFC3339Nano), jobID, kind)
	if err != nil {
		t.Fatalf("UPDATE job event time: %v", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("updated job event rows = %d, err=%v, want 1", changed, err)
	}
}

func setPipelineJobEventTimeAtIndex(t *testing.T, store *db.Store, jobID, kind string, index int, at time.Time) {
	t.Helper()
	conn, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	result, err := conn.Exec(`UPDATE job_events SET created_at = ? WHERE id = (
		SELECT id FROM job_events WHERE job_id = ? AND kind = ? ORDER BY id ASC LIMIT 1 OFFSET ?
	)`, at.UTC().Format(time.RFC3339Nano), jobID, kind, index)
	if err != nil {
		t.Fatalf("UPDATE indexed job event time: %v", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("updated indexed job event rows = %d, err=%v, want 1", changed, err)
	}
}

func jobStateForDecision(decision string) string {
	switch decision {
	case "blocked":
		return string(workflow.JobBlocked)
	case "failed":
		return string(workflow.JobFailed)
	default:
		return string(workflow.JobSucceeded)
	}
}

func stageRow(t *testing.T, store *db.Store, runID, stageID string) db.PipelineRunStage {
	t.Helper()
	stage, ok, err := store.GetPipelineRunStage(context.Background(), runID, stageID)
	if err != nil || !ok {
		t.Fatalf("GetPipelineRunStage(%s/%s): ok=%v err=%v", runID, stageID, ok, err)
	}
	return stage
}

func startTestRun(t *testing.T, store *db.Store, rec db.Pipeline, spec pipeline.Spec, enqueue pipeline.PipelineStageEnqueuer, now time.Time) db.PipelineRun {
	t.Helper()
	run, err := pipeline.CreatePipelineRun(context.Background(), store, rec, spec, "manual", "{}", now)
	if err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}
	run, err = pipeline.AdvancePipelineRun(context.Background(), store, enqueue, rec, spec, run, now)
	if err != nil {
		t.Fatalf("initial advance: %v", err)
	}
	return run
}

func advance(t *testing.T, store *db.Store, rec db.Pipeline, spec pipeline.Spec, enqueue pipeline.PipelineStageEnqueuer, run db.PipelineRun, now time.Time) db.PipelineRun {
	t.Helper()
	updated, err := pipeline.AdvancePipelineRun(context.Background(), store, enqueue, rec, spec, run, now)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	return updated
}

func runPipelineScanOnce(ctx context.Context, store *db.Store, enqueue pipeline.PipelineStageEnqueuer, now time.Time) error {
	return pipeline.RunPipelineScanOnce(ctx, store, enqueue, now)
}

func createPipelineRun(ctx context.Context, store *db.Store, rec db.Pipeline, spec pipeline.Spec, trigger, payloadJSON string, now time.Time) (db.PipelineRun, error) {
	return pipeline.CreatePipelineRun(ctx, store, rec, spec, trigger, payloadJSON, now)
}

func markPipelinePRMerged(t *testing.T, store *db.Store, repo string, number int64, state string) {
	t.Helper()
	if err := store.UpsertPullRequest(context.Background(), db.PullRequest{
		RepoFullName: repo,
		Number:       number,
		HeadBranch:   "gitmoot/some-branch",
		BaseBranch:   "main",
		State:        state,
	}); err != nil {
		t.Fatalf("UpsertPullRequest(%s#%d, %s): %v", repo, number, state, err)
	}
}

func pipelineStageEqual(a, b db.PipelineRunStage) bool {
	return a.State == b.State && a.JobID == b.JobID && a.Attempt == b.Attempt &&
		a.NeedsJSON == b.NeedsJSON && a.Summary == b.Summary &&
		a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt)
}

func seedPipelineRunState(t *testing.T, store *db.Store, id, name, state string, started time.Time) db.PipelineRun {
	t.Helper()
	run := db.PipelineRun{ID: id, Pipeline: name, Trigger: "manual", State: state, StartedAt: started}
	if err := store.CreatePipelineRun(context.Background(), run); err != nil {
		t.Fatalf("CreatePipelineRun %s: %v", id, err)
	}
	return run
}
