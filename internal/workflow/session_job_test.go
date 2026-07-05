package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
)

// TestOpenExternalJobCreatesRunningNoQueue proves `job open` (clock-in) creates a
// job directly in running state, flagged externally_driven, with a running-state
// event and NO queued row — so the daemon's queued selector never claims it and no
// runtime is ever dispatched.
func TestOpenExternalJobCreatesRunningNoQueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"ask"}, "jerryfane/gitmoot")

	job, err := (Mailbox{Store: store}).OpenExternalJob(ctx, JobRequest{
		ID:     "session-ask-lead-1",
		Agent:  "lead",
		Action: "ask",
		Repo:   "jerryfane/gitmoot",
	})
	if err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	if job.State != string(JobRunning) {
		t.Fatalf("job state = %q, want running", job.State)
	}

	stored := mustJob(t, store, "session-ask-lead-1")
	if stored.State != string(JobRunning) {
		t.Fatalf("stored state = %q, want running", stored.State)
	}
	if !stored.ExternallyDriven {
		t.Fatalf("stored ExternallyDriven = false, want true")
	}

	queued, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(queued) != 0 {
		t.Fatalf("queued jobs = %d, want 0 (session jobs never queue)", len(queued))
	}

	evs, err := store.ListJobEvents(ctx, "session-ask-lead-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != string(JobRunning) {
		t.Fatalf("events = %+v, want a single running event", evs)
	}
}

// TestCloseExternalJobAppliesDecision proves close applies the decision through the
// same state mapping an engine result uses, writes the terminal-state event, emits
// the outbound terminal event through the wired EventSink, and — critically — does
// NOT write an advance_started event (a session job has no engine advancement).
func TestCloseExternalJobAppliesDecision(t *testing.T) {
	cases := []struct {
		decision  string
		wantState JobState
		wantType  events.EventType
	}{
		{"approved", JobSucceeded, events.EventJobFinished},
		{"implemented", JobSucceeded, events.EventJobFinished},
		{"changes_requested", JobSucceeded, events.EventJobFinished},
		{"blocked", JobBlocked, events.EventJobBlocked},
		{"failed", JobFailed, events.EventJobFailed},
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "lead", []string{"ask"}, "jerryfane/gitmoot")
			sink := &recordingSink{}
			engine := testEngine(store)
			engine.EventSink = sink

			if _, err := engine.OpenExternalJob(ctx, JobRequest{
				ID:     "session-job",
				Agent:  "lead",
				Action: "ask",
				Repo:   "jerryfane/gitmoot",
			}); err != nil {
				t.Fatalf("OpenExternalJob returned error: %v", err)
			}

			closed, err := engine.CloseExternalJob(ctx, "session-job", AgentResult{
				Decision: tc.decision,
				Summary:  "session done",
			}, 0, "")
			if err != nil {
				t.Fatalf("CloseExternalJob returned error: %v", err)
			}
			if closed.State != string(tc.wantState) {
				t.Fatalf("closed state = %q, want %q", closed.State, tc.wantState)
			}

			// The stored result must be the session's result.
			payload, err := unmarshalPayload(closed.Payload)
			if err != nil {
				t.Fatalf("unmarshalPayload returned error: %v", err)
			}
			if payload.Result == nil || payload.Result.Decision != tc.decision || payload.Result.Summary != "session done" {
				t.Fatalf("payload result = %+v, want decision %q", payload.Result, tc.decision)
			}

			// Exactly one outbound terminal event of the right type.
			got := sink.byType(tc.wantType)
			if len(got) != 1 {
				t.Fatalf("%s emissions = %d, want 1; all=%+v", tc.wantType, len(got), sink.snapshot())
			}
			if got[0].Status != string(tc.wantState) || got[0].Detail != "session done" {
				t.Fatalf("terminal event = %+v", got[0])
			}

			// No advance_started event: a session job must not trigger engine
			// advancement (which would try to advance work the engine never owned).
			evs, err := store.ListJobEvents(ctx, "session-job")
			if err != nil {
				t.Fatalf("ListJobEvents returned error: %v", err)
			}
			for _, ev := range evs {
				if ev.Kind == "advance_started" {
					t.Fatalf("close wrote an advance_started event: %+v", evs)
				}
			}
			// The terminal-state event must be present.
			if !hasEventKind(evs, string(tc.wantState)) {
				t.Fatalf("events %+v missing terminal %q", evs, tc.wantState)
			}
		})
	}
}

// TestCloseExternalJobRecordsPRAndBranch proves the optional --pr/--branch
// overrides land on the stored payload.
func TestCloseExternalJobRecordsPRAndBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")

	if _, err := (Mailbox{Store: store}).OpenExternalJob(ctx, JobRequest{
		ID:     "session-impl",
		Agent:  "lead",
		Action: "implement",
		Repo:   "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	closed, err := (Mailbox{Store: store}).CloseExternalJob(ctx, "session-impl", AgentResult{
		Decision: "implemented",
		Summary:  "shipped",
	}, 42, "feat/x")
	if err != nil {
		t.Fatalf("CloseExternalJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(closed.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.PullRequest != 42 || payload.Branch != "feat/x" {
		t.Fatalf("payload pr/branch = %d/%q, want 42/feat/x", payload.PullRequest, payload.Branch)
	}
}

// TestCloseExternalJobErrors proves the clean-error edges: double-close, closing an
// unknown id, closing an engine (non-session) job, and an invalid decision.
func TestCloseExternalJobErrors(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"ask"}, "jerryfane/gitmoot")
	mb := Mailbox{Store: store}

	// Unknown id.
	if _, err := mb.CloseExternalJob(ctx, "nope", AgentResult{Decision: "approved"}, 0, ""); err == nil {
		t.Fatalf("CloseExternalJob(unknown) returned nil error")
	}

	// Invalid decision.
	if _, err := mb.CloseExternalJob(ctx, "nope", AgentResult{Decision: "bogus"}, 0, ""); err == nil || !strings.Contains(err.Error(), "unsupported decision") {
		t.Fatalf("CloseExternalJob(bad decision) err = %v, want unsupported decision", err)
	}

	// Engine (non-session) job cannot be closed.
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "engine-job", Agent: "lead", Type: "ask", State: string(JobRunning)}, db.JobEvent{Kind: string(JobRunning), Message: "job started"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if _, err := mb.CloseExternalJob(ctx, "engine-job", AgentResult{Decision: "approved"}, 0, ""); err == nil || !strings.Contains(err.Error(), "not a session job") {
		t.Fatalf("CloseExternalJob(engine job) err = %v, want not-a-session-job", err)
	}

	// Open then close, then double-close.
	if _, err := mb.OpenExternalJob(ctx, JobRequest{ID: "sess", Agent: "lead", Action: "ask", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	if _, err := mb.CloseExternalJob(ctx, "sess", AgentResult{Decision: "approved"}, 0, ""); err != nil {
		t.Fatalf("first CloseExternalJob returned error: %v", err)
	}
	if _, err := mb.CloseExternalJob(ctx, "sess", AgentResult{Decision: "approved"}, 0, ""); err == nil || !strings.Contains(err.Error(), "already been closed") {
		t.Fatalf("double CloseExternalJob err = %v, want already-been-closed", err)
	}
}

// TestRetryJobRefusesSessionJob proves the retry invariant hardening (#657): a
// session job that has reached a retry-eligible terminal state (failed here) must
// NOT be re-queued by RetryJob — re-queuing it would let the daemon claim it and
// Deliver an empty session payload to a real runtime (a session implement job could
// push a spurious branch/PR). Retry must refuse before any state transition, and
// the job must stay in its terminal state (never queued).
func TestRetryJobRefusesSessionJob(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")

	mb := Mailbox{Store: store}
	if _, err := mb.OpenExternalJob(ctx, JobRequest{ID: "sess-retry", Agent: "lead", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	// Close it into a retry-eligible terminal state (failed).
	if _, err := mb.CloseExternalJob(ctx, "sess-retry", AgentResult{Decision: "failed"}, 0, ""); err != nil {
		t.Fatalf("CloseExternalJob returned error: %v", err)
	}

	_, err := RetryJob(ctx, store, "sess-retry")
	if err == nil || !strings.Contains(err.Error(), "session job") {
		t.Fatalf("RetryJob(session job) err = %v, want a session-job refusal", err)
	}

	after := mustJob(t, store, "sess-retry")
	if after.State != string(JobFailed) {
		t.Fatalf("session job state after refused retry = %q, want failed (never re-queued)", after.State)
	}
	queued, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(queued) != 0 {
		t.Fatalf("queued jobs = %d, want 0 (refused retry must not queue the session job)", len(queued))
	}
}

func hasEventKind(events []db.JobEvent, kind string) bool {
	for _, ev := range events {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}
