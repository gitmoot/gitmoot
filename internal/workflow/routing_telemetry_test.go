package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// TestRoutingTelemetryRecordsRuntimeDefaultModel proves the recorded model matches
// the model delivery ACTUALLY ran on: a job with no --model and an agent with no
// Model, but a configured runtime registry default_model (#652), records that
// default rather than an empty bucket. Mirrors deliver()'s job.Model > agent.Model
// > RuntimeDefaultModel precedence.
func TestRoutingTelemetryRecordsRuntimeDefaultModel(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "jerryfane/gitmoot")
	agent := runtime.Agent{Name: "impl", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	m := Mailbox{Store: store, RuntimeDefaultModel: func(rt string) string {
		if rt == runtime.ShellRuntime {
			return "gpt-5.5"
		}
		return ""
	}}
	if _, err := m.Enqueue(ctx, JobRequest{ID: "impl-job", Agent: "impl", Action: "implement", Repo: "jerryfane/gitmoot", Branch: "task-1", TaskID: "task-1", TaskTitle: "Impl"}); err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	if _, err := m.Run(ctx, "impl-job", agent, adapter); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	rows, err := store.ListRoutingTelemetry(ctx, db.RoutingTelemetryFilter{})
	if err != nil {
		t.Fatalf("ListRoutingTelemetry error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Model != "gpt-5.5" {
		t.Fatalf("recorded model = %q, want the runtime default_model %q", rows[0].Model, "gpt-5.5")
	}
}

// TestRoutingTelemetryRecordsAdapterFailure proves a hard adapter-delivery error
// (never reaching a parsed decision) still records one advisory observation with
// JobState failed, so a runtime/model that repeatedly crashes at the adapter level
// lowers its recorded success rate instead of contributing zero rows.
func TestRoutingTelemetryRecordsAdapterFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "flaky", []string{"implement"}, "jerryfane/gitmoot")
	agent := runtime.Agent{Name: "flaky", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	adapter := &fakeDelivery{err: errors.New("adapter boom")}
	m := Mailbox{Store: store}
	if _, err := m.Enqueue(ctx, JobRequest{ID: "flaky-job", Agent: "flaky", Action: "implement", Repo: "jerryfane/gitmoot", Branch: "task-1", TaskID: "task-1", TaskTitle: "Flaky"}); err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	if _, err := m.Run(ctx, "flaky-job", agent, adapter); err == nil {
		t.Fatalf("Run expected a delivery error, got nil")
	}
	rows, err := store.ListRoutingTelemetry(ctx, db.RoutingTelemetryFilter{})
	if err != nil {
		t.Fatalf("ListRoutingTelemetry error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 failure observation", len(rows))
	}
	if rows[0].JobState != string(JobFailed) {
		t.Fatalf("recorded JobState = %q, want %q", rows[0].JobState, JobFailed)
	}
	if rows[0].Runtime != runtime.ShellRuntime || rows[0].Action != "implement" || rows[0].Approved {
		t.Fatalf("failure row mismatch: %+v", rows[0])
	}
}

// TestRunJobRecordsRoutingTelemetry proves the #530 capture hook: a job that runs
// to a terminal decision leaves exactly one routing_telemetry row carrying the
// action, effective runtime, phase, template, terminal state, decision, and
// approval flag.
func TestRunJobRecordsRoutingTelemetry(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"looks good","findings":[],"changes_made":[],"tests_run":["go test"],"needs":[],"delegations":[]}}`,
	}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:        "review-job",
		Agent:     "audit",
		Action:    "review",
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-9",
		TaskID:    "task-9",
		TaskTitle: "Review",
		Phase:     "verify",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	if _, err := engine.RunJob(ctx, "review-job", agent, adapter); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	rows, err := store.ListRoutingTelemetry(ctx, db.RoutingTelemetryFilter{})
	if err != nil {
		t.Fatalf("ListRoutingTelemetry error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("routing_telemetry rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.JobID != "review-job" || got.Repo != "jerryfane/gitmoot" || got.Action != "review" ||
		got.Phase != "verify" || got.Runtime != runtime.ShellRuntime || got.Agent != "audit" ||
		got.JobState != string(JobSucceeded) || got.Decision != "approved" || !got.Approved || got.TestsRun != 1 {
		t.Fatalf("telemetry row mismatch: %+v", got)
	}
	if got.DurationMS < 0 {
		t.Fatalf("duration_ms = %d, want >= 0", got.DurationMS)
	}
}

// TestRoutingTelemetryFailureIsSwallowed proves the capture is fail-safe: the
// recorder swallows EVERY store error (here: a closed store makes both the
// GetJob usage read and the Insert fail) and returns normally without panicking.
// Because it is called AFTER the terminal transition, a swallowed error can never
// turn a finished job into a failure.
func TestRoutingTelemetryFailureIsSwallowed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	m := Mailbox{Store: store}
	// Close the underlying DB so every telemetry query errors.
	if err := store.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime}
	// Must not panic and must return (errors swallowed internally).
	m.recordRoutingTelemetry(ctx,
		db.Job{ID: "j1", Agent: "audit", Type: "review"},
		agent,
		JobPayload{Repo: "jerryfane/gitmoot"},
		AgentResult{Decision: "approved"},
		JobSucceeded,
		0)
}

// TestRouterContextOffByDefaultIdenticalPrompt proves the coordinator context
// injection is off by default: with routerContextEnabled unset the delivered
// prompt is byte-identical to the base rendered prompt even when telemetry rows
// exist, and turning it on appends a bounded (<=12 line) observed-performance
// table that carries the mandatory disclaimer.
func TestRouterContextOffByDefaultIdenticalPrompt(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "jerryfane/gitmoot")

	// Seed observed performance for this repo so an enabled build would inject.
	for i := 0; i < 3; i++ {
		if err := store.InsertRoutingTelemetry(ctx, db.RoutingTelemetry{
			Repo: "jerryfane/gitmoot", Action: "implement", Runtime: "codex", Model: "gpt-5.5",
			JobState: "succeeded", Approved: true, DurationMS: 100,
		}); err != nil {
			t.Fatalf("seed telemetry error: %v", err)
		}
	}

	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	req := JobRequest{ID: "coord", Agent: "lead", Action: "review", Repo: "jerryfane/gitmoot", Branch: "task-1", TaskID: "task-1", TaskTitle: "Coordinate"}

	// OFF (default construction): prompt has no router block.
	offAdapter := &fakeDelivery{outputs: []string{`{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, req); err != nil {
		t.Fatalf("Enqueue off error: %v", err)
	}
	if _, err := (Mailbox{Store: store}).Run(ctx, "coord", agent, offAdapter); err != nil {
		t.Fatalf("Run off error: %v", err)
	}
	if len(offAdapter.prompts) != 1 {
		t.Fatalf("off deliveries = %d, want 1", len(offAdapter.prompts))
	}
	offPrompt := offAdapter.prompts[0]
	if strings.Contains(offPrompt, "Observed routing performance") {
		t.Fatalf("off-by-default prompt leaked router context:\n%s", offPrompt)
	}

	// ON: same job, mailbox with routerContextEnabled=true, appends the block.
	onReq := req
	onReq.ID = "coord2"
	onAdapter := &fakeDelivery{outputs: []string{`{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, onReq); err != nil {
		t.Fatalf("Enqueue on error: %v", err)
	}
	if _, err := (Mailbox{Store: store, routerContextEnabled: true}).Run(ctx, "coord2", agent, onAdapter); err != nil {
		t.Fatalf("Run on error: %v", err)
	}
	if len(onAdapter.prompts) != 1 {
		t.Fatalf("on deliveries = %d, want 1", len(onAdapter.prompts))
	}
	onPrompt := onAdapter.prompts[0]
	if !strings.Contains(onPrompt, "Observed routing performance") {
		t.Fatalf("enabled prompt missing router context:\n%s", onPrompt)
	}
	if !strings.Contains(onPrompt, "not a benchmark") {
		t.Fatalf("router context missing the mandatory disclaimer:\n%s", onPrompt)
	}
	// The ON prompt is the OFF prompt plus exactly the appended block.
	if !strings.HasPrefix(onPrompt, offPrompt) {
		t.Fatalf("enabled prompt is not off-prompt + appended block:\noff=%q\non=%q", offPrompt, onPrompt)
	}
	block := strings.TrimPrefix(onPrompt, offPrompt)
	if lines := strings.Count(strings.TrimSpace(block), "\n") + 1; lines > 12 {
		t.Fatalf("router context block = %d lines, want <=12:\n%s", lines, block)
	}
}

// TestRouterContextSkippedForDelegationChild proves the context block targets
// coordinators only: a job WITH a parent (a delegation child) gets no block even
// when the feature is enabled and telemetry exists.
func TestRouterContextSkippedForDelegationChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "worker", []string{"review"}, "jerryfane/gitmoot")
	if err := store.InsertRoutingTelemetry(ctx, db.RoutingTelemetry{
		Repo: "jerryfane/gitmoot", Action: "implement", Runtime: "codex", JobState: "succeeded", Approved: true,
	}); err != nil {
		t.Fatalf("seed telemetry error: %v", err)
	}
	agent := runtime.Agent{Name: "worker", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID: "child", Agent: "worker", Action: "review", Repo: "jerryfane/gitmoot", Branch: "task-1", TaskID: "task-1", TaskTitle: "Child",
		ParentJobID: "some-parent", DelegationID: "d1",
	}); err != nil {
		t.Fatalf("Enqueue child error: %v", err)
	}
	adapter := &fakeDelivery{outputs: []string{`{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}}
	if _, err := (Mailbox{Store: store, routerContextEnabled: true}).Run(ctx, "child", agent, adapter); err != nil {
		t.Fatalf("Run child error: %v", err)
	}
	if strings.Contains(adapter.prompts[0], "Observed routing performance") {
		t.Fatalf("delegation child prompt should not carry router context:\n%s", adapter.prompts[0])
	}
}
