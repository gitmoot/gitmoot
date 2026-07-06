package workflow

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// TestMailboxRunRecordsGatesFromNeeds proves the recording hook (#682): a blocked
// decision carrying a `needs` list persists one open gate per need at the single
// result-bearing terminal chokepoint, and emits the gates_recorded event.
func TestMailboxRunRecordsGatesFromNeeds(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"blocked","summary":"needs credentials","findings":[],"changes_made":[],"tests_run":[],"needs":["API key","R2 token"],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	gates, err := store.ListJobGates(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobGates returned error: %v", err)
	}
	if len(gates) != 2 || gates[0].Need != "API key" || gates[1].Need != "R2 token" || gates[0].Satisfied || gates[1].Satisfied {
		t.Fatalf("gates = %+v, want two open gates from needs", gates)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasEvent(events, "gates_recorded") {
		t.Fatalf("events = %+v, want a gates_recorded event", events)
	}
}

// TestMailboxRunBlockedWithoutNeedsRecordsNoGates proves the byte-identical
// default: a blocked result with no needs records no gates (the non-gated path).
func TestMailboxRunBlockedWithoutNeedsRecordsNoGates(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"blocked","summary":"blocked","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	total, _, err := store.CountJobGates(ctx, "job-1")
	if err != nil {
		t.Fatalf("CountJobGates returned error: %v", err)
	}
	if total != 0 {
		t.Fatalf("gate total = %d, want 0 for a blocked result with no needs", total)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasEvent(events, "gates_recorded") {
		t.Fatalf("events = %+v, want NO gates_recorded event", events)
	}
}

func seedBlockedJobWithGates(t *testing.T, store *db.Store, id string, payload JobPayload, needs []string) {
	t.Helper()
	ctx := context.Background()
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: id, Agent: "audit", Type: "ask", State: string(JobBlocked), Payload: encoded}, db.JobEvent{
		Kind:    string(JobBlocked),
		Message: "blocked",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if _, err := store.RecordJobGates(ctx, id, needs); err != nil {
		t.Fatalf("RecordJobGates returned error: %v", err)
	}
}

// TestMaybeResumeOnGatesClearedRequeuesWhenAllCleared proves the resume-on-clear
// loop closes: once every gate is satisfied the blocked stage re-queues through the
// real RetryJob machinery.
func TestMaybeResumeOnGatesClearedRequeuesWhenAllCleared(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedBlockedJobWithGates(t, store, "job-1", JobPayload{Repo: "owner/repo"}, []string{"API key"})

	// One still open -> not resumed, still blocked.
	out, err := MaybeResumeOnGatesCleared(ctx, store, "job-1")
	if err != nil {
		t.Fatalf("MaybeResumeOnGatesCleared returned error: %v", err)
	}
	if out.Resumed {
		t.Fatalf("resumed with an open gate: %+v", out)
	}
	if job, _ := store.GetJob(ctx, "job-1"); job.State != string(JobBlocked) {
		t.Fatalf("state = %q, want blocked while a gate is open", job.State)
	}

	// Clear it -> resume.
	if ok, err := store.SatisfyJobGate(ctx, "job-1", "API key"); err != nil || !ok {
		t.Fatalf("SatisfyJobGate = (%v, %v)", ok, err)
	}
	out, err = MaybeResumeOnGatesCleared(ctx, store, "job-1")
	if err != nil {
		t.Fatalf("MaybeResumeOnGatesCleared returned error: %v", err)
	}
	if !out.Resumed {
		t.Fatalf("not resumed after clearing all gates: %+v", out)
	}
	job, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(JobQueued) {
		t.Fatalf("state = %q, want queued (RetryJob re-queued the blocked stage)", job.State)
	}
}

// TestMaybeResumeOnGatesClearedSkipsSessionJob proves a session job (#657) is never
// auto-resumed even with all gates cleared.
func TestMaybeResumeOnGatesClearedSkipsSessionJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	encoded, err := marshalPayload(JobPayload{Repo: "owner/repo"})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateExternallyDrivenJobWithEvent(ctx, db.Job{ID: "sess-1", Agent: "lead", Type: "implement", State: string(JobBlocked), Payload: encoded}, db.JobEvent{Kind: string(JobBlocked), Message: "blocked"}); err != nil {
		t.Fatalf("CreateExternallyDrivenJobWithEvent returned error: %v", err)
	}
	if _, err := store.RecordJobGates(ctx, "sess-1", []string{"API key"}); err != nil {
		t.Fatalf("RecordJobGates returned error: %v", err)
	}
	if _, err := store.SatisfyAllJobGates(ctx, "sess-1"); err != nil {
		t.Fatalf("SatisfyAllJobGates returned error: %v", err)
	}
	out, err := MaybeResumeOnGatesCleared(ctx, store, "sess-1")
	if err != nil {
		t.Fatalf("MaybeResumeOnGatesCleared returned error: %v", err)
	}
	if out.Resumed {
		t.Fatalf("resumed a session job: %+v", out)
	}
	if job, _ := store.GetJob(ctx, "sess-1"); job.State != string(JobBlocked) {
		t.Fatalf("session job state = %q, want blocked (never re-queued)", job.State)
	}
}

// TestMaybeResumeOnGatesClearedSkipsAwaitingHuman proves a cleared resource gate on
// a stage whose tree is paused awaiting a human (#340) does NOT bypass the human.
func TestMaybeResumeOnGatesClearedSkipsAwaitingHuman(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", Title: "t", State: string(TaskAwaitingHuman)}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seedBlockedJobWithGates(t, store, "job-1", JobPayload{Repo: "owner/repo", TaskID: "task-1"}, []string{"human approval"})
	if _, err := store.SatisfyAllJobGates(ctx, "job-1"); err != nil {
		t.Fatalf("SatisfyAllJobGates returned error: %v", err)
	}
	out, err := MaybeResumeOnGatesCleared(ctx, store, "job-1")
	if err != nil {
		t.Fatalf("MaybeResumeOnGatesCleared returned error: %v", err)
	}
	if out.Resumed {
		t.Fatalf("resumed an awaiting-human stage: %+v", out)
	}
	if job, _ := store.GetJob(ctx, "job-1"); job.State != string(JobBlocked) {
		t.Fatalf("state = %q, want blocked (human gate must not be bypassed)", job.State)
	}
}
