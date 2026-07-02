package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// recordingSink captures Emit calls for the daemon best-effort event tests.
type recordingSink struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recordingSink) Emit(_ context.Context, event events.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *recordingSink) byType(typ events.EventType) []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Event
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (r *recordingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// orderedForJob returns, in emission order, the event types recorded for a given
// job id — used to assert that a deferral never emits job.failed before
// job.deferred (#532 slice E's pre-terminal property).
func (r *recordingSink) orderedForJob(jobID string) []events.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.EventType
	for _, e := range r.events {
		if e.JobID == jobID {
			out = append(out, e.Type)
		}
	}
	return out
}

// TestFinishQueuedJobEmitsJobFailed proves the DAEMON-owned pre-flight terminal
// path (a queued->failed transition that never reaches the engine's Mailbox
// chokepoint) fans a job.failed out through the sink, carrying repo/root_id from
// the payload.
func TestFinishQueuedJobEmitsJobFailed(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.CreateJob(ctx, db.Job{ID: "queued-job", Agent: "coord", Type: "ask", State: string(workflow.JobQueued)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	payload := workflow.JobPayload{Repo: "owner/repo", RootJobID: "root-1"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, "queued-job", string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload returned error: %v", err)
	}

	sink := &recordingSink{}
	worker := defaultJobWorker(store, io.Discard)
	worker.EventSinkOverride = sink

	if err := worker.finishQueuedJob(ctx, "queued-job", workflow.JobFailed, errors.New("preflight check failed")); err != nil {
		t.Fatalf("finishQueuedJob returned error: %v", err)
	}

	failed := sink.byType(events.EventJobFailed)
	if len(failed) != 1 {
		t.Fatalf("job.failed emissions = %d, want 1", len(failed))
	}
	ev := failed[0]
	if ev.JobID != "queued-job" || ev.RootID != "root-1" || ev.Repo != "owner/repo" || ev.Status != "failed" {
		t.Fatalf("job.failed event = %+v", ev)
	}
	if ev.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", ev.SchemaVersion)
	}
	if ev.Detail != "preflight check failed" {
		t.Fatalf("detail = %q, want the failure cause", ev.Detail)
	}
}

// TestFinishQueuedJobEmitsJobBlocked proves a queued->blocked pre-flight
// transition emits job.blocked.
func TestFinishQueuedJobEmitsJobBlocked(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.CreateJob(ctx, db.Job{ID: "queued-job", Agent: "coord", Type: "ask", State: string(workflow.JobQueued)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	sink := &recordingSink{}
	worker := defaultJobWorker(store, io.Discard)
	worker.EventSinkOverride = sink

	if err := worker.finishQueuedJob(ctx, "queued-job", workflow.JobBlocked, errors.New("missing capability")); err != nil {
		t.Fatalf("finishQueuedJob returned error: %v", err)
	}
	if got := sink.byType(events.EventJobBlocked); len(got) != 1 {
		t.Fatalf("job.blocked emissions = %d, want 1", len(got))
	}
}

// TestFinishQueuedJobNilSinkIsByteIdentical proves the off-by-default path: with
// no sink the transition still succeeds and nothing is emitted (no panic).
func TestFinishQueuedJobNilSinkIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.CreateJob(ctx, db.Job{ID: "queued-job", Agent: "coord", Type: "ask", State: string(workflow.JobQueued)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard) // no EventSinkOverride; home unset -> nil sink

	if err := worker.finishQueuedJob(ctx, "queued-job", workflow.JobFailed, errors.New("boom")); err != nil {
		t.Fatalf("finishQueuedJob returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "queued-job")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
}

// TestFinishQueuedJobNoTransitionDoesNotEmit proves the gate on the GENUINE
// transition: re-finishing an already-terminal job must NOT re-emit (the
// transitioned==false branch), so a retry never double-emits.
func TestFinishQueuedJobNoTransitionDoesNotEmit(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	// Already terminal (not queued): the transition is a no-op.
	if err := store.CreateJob(ctx, db.Job{ID: "done-job", Agent: "coord", Type: "ask", State: string(workflow.JobFailed)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	sink := &recordingSink{}
	worker := defaultJobWorker(store, io.Discard)
	worker.EventSinkOverride = sink

	if err := worker.finishQueuedJob(ctx, "done-job", workflow.JobFailed, errors.New("boom")); err != nil {
		t.Fatalf("finishQueuedJob returned error: %v", err)
	}
	if sink.count() != 0 {
		t.Fatalf("emissions = %d, want 0 for a non-transition", sink.count())
	}
}
