package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

const delegatingResult = `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"d","agent":"a","action":"review","prompt":"go"}]}}`

// TestMailboxRunStripsPipelineStageDelegations proves a #681 pipeline stage is a
// LEAF: a stage-sender job whose shell command emits delegations[] has them
// stripped before the result is stored, so the engine's dispatchDelegations can
// never spawn phantom top-level children from a stage (whose ParentJobID is empty).
func TestMailboxRunStripsPipelineStageDelegations(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "pipeline-x-runner", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "pipeline-runner"}
	adapter := &fakeDelivery{outputs: []string{delegatingResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "stage-1", Agent: "pipeline-x-runner", Action: "ask", Repo: "jerryfane/gitmoot", Sender: PipelineJobSender}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "stage-1", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}

	job, err := store.GetJob(ctx, "stage-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.Result == nil {
		t.Fatalf("stage job has no result")
	}
	if len(payload.Result.Delegations) != 0 {
		t.Fatalf("pipeline stage delegations = %+v, want stripped (leaf)", payload.Result.Delegations)
	}
}

// TestMailboxRunKeepsNonPipelineDelegations is the control: a normal (non-pipeline)
// sender's delegations survive, so the strip is scoped strictly to pipeline stages.
func TestMailboxRunKeepsNonPipelineDelegations(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "coord", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "coordinator"}
	adapter := &fakeDelivery{outputs: []string{delegatingResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "coord", Action: "ask", Repo: "jerryfane/gitmoot", Sender: "user"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}

	job, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.Result == nil || len(payload.Result.Delegations) != 1 {
		t.Fatalf("non-pipeline delegations = %+v, want preserved (1)", payload.Result)
	}
}

const blockedNeedsResult = `{"gitmoot_result":{"decision":"blocked","summary":"missing token","findings":[],"changes_made":[],"tests_run":[],"needs":["R2 token"],"delegations":[]}}`

// TestMailboxRunSkipsJobGatesForPipelineStages proves a blocked #681 pipeline
// stage records NO job_gates rows (#693): stage needs live on the pipeline
// run/stage rows and resume happens at the RUN level (ResumePipelineRun mints
// attempt+1 with a new job id). A job-gate here would let `job gates clear`
// RetryJob the OLD stage id — an orphaned re-execution the advancer never folds.
func TestMailboxRunSkipsJobGatesForPipelineStages(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "pipeline-x-runner", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "pipeline-runner"}
	adapter := &fakeDelivery{outputs: []string{blockedNeedsResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "stage-b", Agent: "pipeline-x-runner", Action: "ask", Repo: "jerryfane/gitmoot", Sender: PipelineJobSender}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "stage-b", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}

	job, err := store.GetJob(ctx, "stage-b")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(JobBlocked) {
		t.Fatalf("job state = %s, want blocked", job.State)
	}
	total, _, err := store.CountJobGates(ctx, "stage-b")
	if err != nil {
		t.Fatalf("CountJobGates: %v", err)
	}
	if total != 0 {
		t.Fatalf("job_gates rows = %d, want 0 for a pipeline stage", total)
	}
}

// TestMaybeResumeOnGatesClearedRefusesPipelineStages is the belt-and-suspenders
// guard: even if gate rows exist for a pipeline stage (recorded before the
// mailbox-side exclusion, or written by hand), clearing them must NOT RetryJob
// the stage — run-level resume is the only sanctioned path.
func TestMaybeResumeOnGatesClearedRefusesPipelineStages(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "pipeline-x-runner", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "pipeline-runner"}
	adapter := &fakeDelivery{outputs: []string{blockedNeedsResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "stage-g", Agent: "pipeline-x-runner", Action: "ask", Repo: "jerryfane/gitmoot", Sender: PipelineJobSender}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "stage-g", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Simulate legacy/hand-written gate rows on the stage job, then clear them.
	if _, err := store.RecordJobGates(ctx, "stage-g", []string{"R2 token"}); err != nil {
		t.Fatalf("RecordJobGates: %v", err)
	}
	if _, err := store.SatisfyAllJobGates(ctx, "stage-g"); err != nil {
		t.Fatalf("SatisfyAllJobGates: %v", err)
	}

	outcome, err := MaybeResumeOnGatesCleared(ctx, store, "stage-g")
	if err != nil {
		t.Fatalf("MaybeResumeOnGatesCleared: %v", err)
	}
	if outcome.Resumed {
		t.Fatalf("outcome = %+v, want refused for a pipeline stage", outcome)
	}
	if !strings.Contains(outcome.Reason, "pipeline") {
		t.Fatalf("reason = %q, want the pipeline-stage refusal", outcome.Reason)
	}
	job, err := store.GetJob(ctx, "stage-g")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(JobBlocked) {
		t.Fatalf("job state = %s, want still blocked (no RetryJob)", job.State)
	}
}
