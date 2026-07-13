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

func TestListDashboardAutoWorkflowsExcludesPipelineStages(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	for _, job := range []Job{
		{ID: "pipeline-job", Agent: "pipeline-worker", Type: "produce", State: "succeeded", Payload: `{"repo":"acme/data"}`},
		{ID: "adhoc-job", Agent: "adhoc-worker", Type: "ask", State: "running", Payload: `{"repo":"acme/app"}`},
		{ID: "labeled-pipeline-job", Agent: "pipeline-worker", Type: "produce", State: "succeeded", Payload: `{"repo":"acme/manual","workflow_id":"pipeline/manual"}`},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", job.ID, err)
		}
	}
	if err := store.CreatePipelineRun(ctx, PipelineRun{ID: "run-1", Pipeline: "nightly", State: "succeeded"}); err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}
	for _, stage := range []PipelineRunStage{
		{RunID: "run-1", StageID: "unlabeled", State: "succeeded", JobID: "pipeline-job"},
		{RunID: "run-1", StageID: "labeled", State: "succeeded", JobID: "labeled-pipeline-job"},
	} {
		if err := store.CreatePipelineRunStage(ctx, stage); err != nil {
			t.Fatalf("CreatePipelineRunStage(%s): %v", stage.StageID, err)
		}
	}

	auto, err := store.ListDashboardAutoWorkflows(ctx)
	if err != nil {
		t.Fatalf("ListDashboardAutoWorkflows: %v", err)
	}
	if len(auto) != 1 || auto[0].Summary.WorkflowID != "adhoc/adhoc-worker" || auto[0].Summary.JobCount != 1 || auto[0].Summary.Running != 1 {
		t.Fatalf("auto workflows = %+v, want only adhoc/adhoc-worker", auto)
	}
	if len(auto[0].Repos) != 1 || auto[0].Repos[0] != "acme/app" {
		t.Fatalf("adhoc repos = %v, want [acme/app]", auto[0].Repos)
	}

	summaries, err := store.ListWorkflowSummaries(ctx)
	if err != nil {
		t.Fatalf("ListWorkflowSummaries: %v", err)
	}
	if len(summaries) != 1 || summaries[0].WorkflowID != "pipeline/manual" || summaries[0].JobCount != 1 || summaries[0].Succeeded != 1 {
		t.Fatalf("labeled workflows = %+v, want pipeline/manual", summaries)
	}
}
