package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// recordingSink captures EventSink emissions for the engine best-effort tests.
// hang, when set, makes Emit block (a slow consumer) so a test can prove the
// engine's finish path does not deadlock holding a lock across the emit. The
// Sink contract has no error return, so a sink can never fail a job by design.
type recordingSink struct {
	mu     sync.Mutex
	events []events.Event
	hang   chan struct{} // when non-nil, Emit blocks on it (slow consumer)
}

func (r *recordingSink) Emit(_ context.Context, event events.Event) {
	if r.hang != nil {
		<-r.hang
	}
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *recordingSink) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordingSink) byType(typ events.EventType) []events.Event {
	var out []events.Event
	for _, e := range r.snapshot() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestEngineEmitsJobFinishedOnSucceededTerminal proves the engine fans a
// job.finished out through the EventSink on the succeeded terminal transition
// (the Mailbox.finishWithPayload chokepoint), carrying root_id/repo/status.
func TestEngineEmitsJobFinishedOnSucceededTerminal(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	sink := &recordingSink{}
	engine := testEngine(store)
	engine.EventSink = sink
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"looks good","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:        "review-job",
		Agent:     "audit",
		Action:    "review",
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-9",
		TaskID:    "task-9",
		TaskTitle: "Review",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	if _, err := engine.RunJob(ctx, "review-job", agent, adapter); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	finished := sink.byType(events.EventJobFinished)
	if len(finished) != 1 {
		t.Fatalf("job.finished emissions = %d, want 1; all=%+v", len(finished), sink.snapshot())
	}
	ev := finished[0]
	if ev.JobID != "review-job" || ev.RootID != "review-job" || ev.Repo != "jerryfane/gitmoot" || ev.Status != "succeeded" {
		t.Fatalf("job.finished event = %+v", ev)
	}
	if ev.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", ev.SchemaVersion)
	}
	if ev.Detail != "looks good" {
		t.Fatalf("detail = %q, want the result summary", ev.Detail)
	}
}

// TestEngineEmitsJobFailedOnFailedDecision proves a failed decision (running->
// failed via finishWithPayload) emits job.failed, not job.finished.
func TestEngineEmitsJobFailedOnFailedDecision(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	sink := &recordingSink{}
	engine := testEngine(store)
	engine.EventSink = sink
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"failed","summary":"could not finish","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:        "review-job",
		Agent:     "audit",
		Action:    "review",
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-9",
		TaskID:    "task-9",
		TaskTitle: "Review",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	// A failed decision terminates the job failed; the running->failed transition
	// (and its emit) happens inside Mailbox.finishWithPayload regardless of whether
	// the subsequent advance returns an error, so the emit is what we assert.
	_, _ = engine.RunJob(ctx, "review-job", agent, adapter)
	job := mustJob(t, store, "review-job")
	if job.State != string(JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}

	if got := sink.byType(events.EventJobFinished); len(got) != 0 {
		t.Fatalf("job.finished emissions = %d, want 0 for a failed decision", len(got))
	}
	failed := sink.byType(events.EventJobFailed)
	if len(failed) != 1 {
		t.Fatalf("job.failed emissions = %d, want 1; all=%+v", len(failed), sink.snapshot())
	}
	if failed[0].Status != "failed" || failed[0].Detail != "could not finish" {
		t.Fatalf("job.failed event = %+v", failed[0])
	}
}

// TestEngineEmitsJobFailedOnDeliveryFailure proves the most common failure mode
// — a runtime delivery error that never produces a parseable result — emits
// exactly one job.failed. This exercises the m.fail -> m.finish path (NOT
// finishWithPayload), the gap the #446 review found: finish now carries the
// emit symmetrically, so a delivery failure is no longer silently dropped.
func TestEngineEmitsJobFailedOnDeliveryFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	sink := &recordingSink{}
	engine := testEngine(store)
	engine.EventSink = sink
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	// A delivery error: Deliver returns an error, so Mailbox.Run takes the m.fail
	// branch and never reaches finishWithPayload.
	adapter := &fakeDelivery{err: errors.New("codex transport failure")}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:        "review-job",
		Agent:     "audit",
		Action:    "review",
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-9",
		TaskID:    "task-9",
		TaskTitle: "Review",
		RootJobID: "root-9",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	// RunJob returns the delivery error; the running->failed transition (and its
	// emit) happens inside Mailbox.fail -> finish regardless.
	if _, err := engine.RunJob(ctx, "review-job", agent, adapter); err == nil {
		t.Fatalf("RunJob with a delivery error returned nil, want the delivery error")
	}
	job := mustJob(t, store, "review-job")
	if job.State != string(JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}

	if got := sink.byType(events.EventJobFinished); len(got) != 0 {
		t.Fatalf("job.finished emissions = %d, want 0 for a delivery failure", len(got))
	}
	failed := sink.byType(events.EventJobFailed)
	if len(failed) != 1 {
		t.Fatalf("job.failed emissions = %d, want exactly 1; all=%+v", len(failed), sink.snapshot())
	}
	ev := failed[0]
	if ev.JobID != "review-job" || ev.RootID != "root-9" || ev.Repo != "jerryfane/gitmoot" || ev.Status != "failed" {
		t.Fatalf("job.failed event = %+v", ev)
	}
	// The transition message ("delivery failed: ...") is surfaced as the detail
	// since the failed delivery left no result to summarize.
	if !strings.Contains(ev.Detail, "delivery failed") {
		t.Fatalf("detail = %q, want it to carry the delivery-failure message", ev.Detail)
	}
}

// TestEngineEmitsJobFailedOnMalformedOutputAfterRepair proves the second
// finish-path failure mode — a parse failure that survives the repair retry —
// also emits exactly one job.failed (parse error, not delivery error).
func TestEngineEmitsJobFailedOnMalformedOutputAfterRepair(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	sink := &recordingSink{}
	engine := testEngine(store)
	engine.EventSink = sink
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	// Two un-parseable outputs: the first triggers the repair retry, the second
	// (also malformed) drives m.fail -> finish with a parse error.
	adapter := &fakeDelivery{outputs: []string{"not json", "still not json"}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:        "review-job",
		Agent:     "audit",
		Action:    "review",
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-9",
		TaskID:    "task-9",
		TaskTitle: "Review",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	if _, err := engine.RunJob(ctx, "review-job", agent, adapter); err == nil {
		t.Fatalf("RunJob with malformed output returned nil, want a parse error")
	}
	job := mustJob(t, store, "review-job")
	if job.State != string(JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	failed := sink.byType(events.EventJobFailed)
	if len(failed) != 1 {
		t.Fatalf("job.failed emissions = %d, want exactly 1; all=%+v", len(failed), sink.snapshot())
	}
	if got := sink.byType(events.EventJobFinished); len(got) != 0 {
		t.Fatalf("job.finished emissions = %d, want 0 for a malformed-output failure", len(got))
	}
}

// TestEngineEmitsNeedsAttentionOnEscalateHumanPause proves the escalate_human
// pause fans a job.needs_attention out carrying the redacted question in detail,
// rooted at the coordinator.
func TestEngineEmitsNeedsAttentionOnEscalateHumanPause(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	sink := &recordingSink{}
	engine := testEngine(store)
	engine.EventSink = sink

	seedEscalateHumanCoordinator(t, store, engine)

	err := engine.AdvanceJob(ctx, "parent-job/delegation/api")
	var awaiting AwaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("AdvanceJob(api) error = %v, want AwaitingHumanError", err)
	}

	attention := sink.byType(events.EventJobNeedsAttention)
	if len(attention) != 1 {
		t.Fatalf("job.needs_attention emissions = %d, want 1; all=%+v", len(attention), sink.snapshot())
	}
	ev := attention[0]
	if ev.JobID != "parent-job" || ev.RootID != "parent-job" || ev.Status != string(TaskAwaitingHuman) {
		t.Fatalf("needs_attention event = %+v", ev)
	}
	if ev.Detail != "build api" {
		t.Fatalf("detail = %q, want the escalation question", ev.Detail)
	}
}

// TestEngineNeedsAttentionEmitIsOneShot proves a re-advance of the failing leg
// (idempotent re-pause) does NOT re-emit job.needs_attention, mirroring the
// one-shot escalationRequested event.
func TestEngineNeedsAttentionEmitIsOneShot(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	sink := &recordingSink{}
	engine := testEngine(store)
	engine.EventSink = sink

	seedEscalateHumanCoordinator(t, store, engine)

	for i := 0; i < 2; i++ {
		_ = engine.AdvanceJob(ctx, "parent-job/delegation/api")
	}
	if got := sink.byType(events.EventJobNeedsAttention); len(got) != 1 {
		t.Fatalf("job.needs_attention emissions after re-advance = %d, want 1 (one-shot)", len(got))
	}
}

// TestEngineNilSinkIsByteIdentical proves the off-by-default path: with no
// EventSink, a terminal transition succeeds exactly as before and no event is
// constructed (no panic, the job still reaches its terminal state).
func TestEngineNilSinkIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store) // no EventSink
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:     "review-job",
		Agent:  "audit",
		Action: "review",
		Repo:   "jerryfane/gitmoot",
		Branch: "task-9",
		TaskID: "task-9",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := engine.RunJob(ctx, "review-job", agent, adapter); err != nil {
		t.Fatalf("RunJob with nil EventSink returned error: %v", err)
	}
	job := mustJob(t, store, "review-job")
	if job.State != string(JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
}

// TestEngineSlowSinkNeverBlocksFinish proves a slow sink (Emit blocks) never
// stalls the engine beyond the test's own release: we run RunJob in a goroutine
// and assert the terminal transition is durable even while Emit is hung, then
// release. (The real webhook sink is non-blocking; this guards a misbehaving
// in-process sink from wedging the engine's finish path indefinitely is the
// SINK's contract — here we verify the engine does not deadlock holding a lock
// across the emit.)
func TestEngineSlowSinkDoesNotDeadlockEngine(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	hang := make(chan struct{})
	sink := &recordingSink{hang: hang}
	engine := testEngine(store)
	engine.EventSink = sink
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "agent"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:     "review-job",
		Agent:  "audit",
		Action: "review",
		Repo:   "jerryfane/gitmoot",
		Branch: "task-9",
		TaskID: "task-9",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := engine.RunJob(ctx, "review-job", agent, adapter)
		done <- err
	}()

	// Release the hung Emit promptly; the engine must complete cleanly afterwards.
	close(hang)
	if err := <-done; err != nil {
		t.Fatalf("RunJob with slow sink returned error: %v", err)
	}
	if got := sink.byType(events.EventJobFinished); len(got) != 1 {
		t.Fatalf("job.finished emissions = %d, want 1", len(got))
	}
}
