package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/evidence"
	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

// TestOpenExternalJobCreatesRunningNoQueue proves `job open` (clock-in) creates a
// job directly in running state, flagged externally_driven, with a running-state
// event and NO queued row — so the daemon's queued selector never claims it and no
// runtime is ever dispatched.
func TestOpenExternalJobCreatesRunningNoQueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"ask"}, "gitmoot/gitmoot")

	job, err := (Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}).OpenExternalJob(ctx, JobRequest{
		ID:      "session-ask-lead-1",
		Agent:   "lead",
		Action:  "ask",
		Repo:    "gitmoot/gitmoot",
		HeadSHA: "must-not-enter-payload",
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
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.HeadSHA != "" {
		t.Fatalf("gate-visible payload head SHA = %q, want empty", payload.HeadSHA)
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
	if len(evs) != 2 || evs[0].Kind != string(JobRunning) || evs[1].Kind != SessionJobDisplayEventKind {
		t.Fatalf("events = %+v, want running plus display-only head metadata", evs)
	}
	display, ok := ParseSessionJobDisplayEvent(evs[1])
	if !ok || display.HeadSHA != "must-not-enter-payload" {
		t.Fatalf("display event = %+v ok=%v, want caller head outside payload", display, ok)
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
		{"skipped", JobSucceeded, events.EventJobFinished},
		{"blocked", JobBlocked, events.EventJobBlocked},
		{"failed", JobFailed, events.EventJobFailed},
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "lead", []string{"ask"}, "gitmoot/gitmoot")
			sink := &recordingSink{}
			engine := testEngine(store)
			engine.EventSink = sink

			if _, err := engine.OpenExternalJob(ctx, JobRequest{
				ID:     "session-job",
				Agent:  "lead",
				Action: "ask",
				Repo:   "gitmoot/gitmoot",
			}); err != nil {
				t.Fatalf("OpenExternalJob returned error: %v", err)
			}

			closed, err := engine.CloseExternalJob(ctx, "session-job", AgentResult{
				Decision: tc.decision,
				Summary:  "session done",
			}, 0, "", "")
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

func TestCloseExternalJobRequiresSeverityForChangesRequestedReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	if _, err := engine.OpenExternalJob(ctx, JobRequest{
		ID:     "session-review",
		Agent:  "lead",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}

	if _, err := engine.CloseExternalJob(ctx, "session-review", AgentResult{
		Decision: "changes_requested",
		Summary:  "fix the finding",
	}, 0, "", ""); err == nil || !strings.Contains(err.Error(), "severity is required") {
		t.Fatalf("CloseExternalJob without severity error = %v, want severity requirement", err)
	}
	if stored := mustJob(t, store, "session-review"); stored.State != string(JobRunning) {
		t.Fatalf("job state after rejected close = %q, want running", stored.State)
	}

	closed, err := engine.CloseExternalJob(ctx, "session-review", AgentResult{
		Decision: "changes_requested",
		Severity: "P1",
		Summary:  "fix the finding",
	}, 0, "", "")
	if err != nil {
		t.Fatalf("CloseExternalJob with severity returned error: %v", err)
	}
	payload, err := unmarshalPayload(closed.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Result == nil || payload.Result.Severity != "P1" {
		t.Fatalf("payload result = %+v, want severity P1", payload.Result)
	}
}

// A session review job closes here and never runs AdvanceJob, so the durable
// review_approved_with_notes event that path writes has to be written here too.
// The merge gate folds a sub-threshold verdict into an approval either way, while
// proof/project.go keys its review.approved claim on this event — so without it
// the two surfaces disagree about the same verdict and `gitmoot proof` renders
// "0 approved" for a PR the gate will merge.
func TestCloseExternalJobRecordsApprovedWithNotesForSubthresholdReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P1 }

	if _, err := engine.OpenExternalJob(ctx, JobRequest{
		ID: "session-review-notes", Agent: "lead", Action: "review",
		Repo: "gitmoot/gitmoot", PullRequest: 8,
	}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	if _, err := engine.CloseExternalJob(ctx, "session-review-notes", AgentResult{
		Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "non-blocking polish",
	}, 0, "", ""); err != nil {
		t.Fatalf("CloseExternalJob returned error: %v", err)
	}
	if got := countJobEvents(t, store, "session-review-notes", ReviewApprovedWithNotesEventKind); got != 1 {
		t.Fatalf("%s events = %d, want 1", ReviewApprovedWithNotesEventKind, got)
	}
	// The raw verdict is never rewritten; only the outcome is recorded alongside.
	stored := mustJob(t, store, "session-review-notes")
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Result == nil || payload.Result.Decision != "changes_requested" {
		t.Fatalf("stored decision = %+v, want the raw changes_requested preserved", payload.Result)
	}
}

// AT-THRESHOLD CONTROL: a genuinely blocking session review owes no outcome
// event. Without this the test above would pass on a mutant that writes the
// event unconditionally.
func TestCloseExternalJobRecordsNoOutcomeForBlockingSessionReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P2 }

	if _, err := engine.OpenExternalJob(ctx, JobRequest{
		ID: "session-review-blocking", Agent: "lead", Action: "review",
		Repo: "gitmoot/gitmoot", PullRequest: 8,
	}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	if _, err := engine.CloseExternalJob(ctx, "session-review-blocking", AgentResult{
		Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "blocking",
	}, 0, "", ""); err != nil {
		t.Fatalf("CloseExternalJob returned error: %v", err)
	}
	if got := countJobEvents(t, store, "session-review-blocking", ReviewApprovedWithNotesEventKind); got != 0 {
		t.Fatalf("%s events = %d, want 0 for an at-threshold blocking review", ReviewApprovedWithNotesEventKind, got)
	}
}

// TestCloseExternalJobRecordsPRAndBranch proves the optional --pr/--branch
// overrides land on the stored payload.
func TestCloseExternalJobRecordsPRAndBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "gitmoot/gitmoot")

	if _, err := (Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}).OpenExternalJob(ctx, JobRequest{
		ID:     "session-review",
		Agent:  "lead",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	closed, err := (Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}).CloseExternalJob(ctx, "session-review", AgentResult{
		Decision: "approved",
		Summary:  "reviewed",
	}, 42, "reviewed-head", "feat/x")
	if err != nil {
		t.Fatalf("CloseExternalJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(closed.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.PullRequest != 42 || payload.HeadSHA != "" || payload.Branch != "feat/x" {
		t.Fatalf("payload pr/head/branch = %d/%q/%q, want 42/empty/feat/x", payload.PullRequest, payload.HeadSHA, payload.Branch)
	}
	event, ok, err := store.GetLatestJobEventByKind(ctx, "session-review", SessionJobDisplayEventKind)
	if err != nil || !ok {
		t.Fatalf("GetLatestJobEventByKind(display) ok=%v err=%v", ok, err)
	}
	display, ok := ParseSessionJobDisplayEvent(event)
	if !ok || display.HeadSHA != "reviewed-head" {
		t.Fatalf("display event = %+v ok=%v, want reviewed-head", display, ok)
	}
}

func TestCloseExternalReviewPersistsReportedGrade(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "gitmoot/gitmoot")
	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}

	if _, err := mb.OpenExternalJob(ctx, JobRequest{
		ID:     "session-review-grade",
		Agent:  "lead",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	if _, err := mb.CloseExternalJob(ctx, "session-review-grade", AgentResult{
		Decision: "approved",
		Summary:  "reviewed",
	}, 0, "", ""); err != nil {
		t.Fatalf("CloseExternalJob returned error: %v", err)
	}

	stored := mustJob(t, store, "session-review-grade")
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.ReviewStatusGrade != evidence.GradeReported {
		t.Fatalf("stored review status grade = %q, want %q", payload.ReviewStatusGrade, evidence.GradeReported)
	}
}

// TestCloseExternalJobErrors proves the clean-error edges: double-close, closing an
// unknown id, closing an engine (non-session) job, and an invalid decision.
func TestCloseExternalJobErrors(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"ask"}, "gitmoot/gitmoot")
	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}

	// Unknown id.
	if _, err := mb.CloseExternalJob(ctx, "nope", AgentResult{Decision: "approved"}, 0, "", ""); err == nil {
		t.Fatalf("CloseExternalJob(unknown) returned nil error")
	}

	// Invalid decision.
	if _, err := mb.CloseExternalJob(ctx, "nope", AgentResult{Decision: "bogus"}, 0, "", ""); err == nil || !strings.Contains(err.Error(), "unsupported decision") {
		t.Fatalf("CloseExternalJob(bad decision) err = %v, want unsupported decision", err)
	}

	// Engine (non-session) job cannot be closed.
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "engine-job", Agent: "lead", Type: "ask", State: string(JobRunning), Payload: `{}`}, db.JobEvent{Kind: string(JobRunning), Message: "job started"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if _, err := mb.CloseExternalJobWithUsage(ctx, "engine-job", AgentResult{Decision: "approved"}, 0, "", "", ExternalJobUsage{Model: "must-not-land", InputTokens: 7, OutputTokens: 9}); err == nil || !strings.Contains(err.Error(), "not a session job") {
		t.Fatalf("CloseExternalJob(engine job) err = %v, want not-a-session-job", err)
	}
	engineJob := mustJob(t, store, "engine-job")
	if engineJob.Model != "" || engineJob.InputTokens != 0 || engineJob.OutputTokens != 0 || engineJob.State != string(JobRunning) {
		t.Fatalf("refused engine close changed job: %+v", engineJob)
	}

	// Open then close, then double-close.
	if _, err := mb.OpenExternalJob(ctx, JobRequest{ID: "sess", Agent: "lead", Action: "ask", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	if _, err := mb.CloseExternalJob(ctx, "sess", AgentResult{Decision: "approved"}, 0, "", ""); err != nil {
		t.Fatalf("first CloseExternalJob returned error: %v", err)
	}
	if _, err := mb.CloseExternalJob(ctx, "sess", AgentResult{Decision: "approved"}, 0, "", ""); err == nil || !strings.Contains(err.Error(), "already been closed") {
		t.Fatalf("double CloseExternalJob err = %v, want already-been-closed", err)
	}
}

func TestCloseExternalJobAfterGhostReaperReturnsCleanAlreadyClosedError(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"ask"}, "gitmoot/gitmoot")
	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mb.OpenExternalJob(ctx, JobRequest{
		ID:         "session-race",
		Agent:      "lead",
		Action:     "ask",
		Repo:       "gitmoot/gitmoot",
		WorkflowID: "release/settled",
	}); err != nil {
		t.Fatalf("OpenExternalJob: %v", err)
	}
	if _, err := store.InsertWorkflowNoteWithMeta(ctx,
		db.WorkflowNote{WorkflowID: "release/settled", Author: "operator", Body: "done"},
		db.WorkflowMeta{Status: string(db.WorkflowStatusDone), StatusSet: true}); err != nil {
		t.Fatalf("InsertWorkflowNoteWithMeta: %v", err)
	}
	reaped, err := store.ReapGhostSessionJobs(ctx, time.Now().UTC().Add(10*time.Minute), 24*time.Hour)
	if err != nil {
		t.Fatalf("ReapGhostSessionJobs: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "session-race" {
		t.Fatalf("reaped = %v", reaped)
	}
	if _, err := mb.CloseExternalJob(ctx, "session-race", AgentResult{Decision: "approved"}, 0, "", ""); err == nil ||
		!strings.Contains(err.Error(), "already been closed") {
		t.Fatalf("CloseExternalJob after reaper err = %v, want already-been-closed", err)
	}
	job, err := store.GetJob(ctx, "session-race")
	if err != nil || job.State != string(JobCancelled) {
		t.Fatalf("job = %+v, err=%v", job, err)
	}
	events, err := store.ListJobEvents(ctx, "session-race")
	if err != nil || len(events) != 2 || events[1].Kind != string(JobCancelled) {
		t.Fatalf("events = %+v, err=%v", events, err)
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
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")

	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mb.OpenExternalJob(ctx, JobRequest{ID: "sess-retry", Agent: "lead", Action: "implement", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	// Close it into a retry-eligible terminal state (failed).
	if _, err := mb.CloseExternalJob(ctx, "sess-retry", AgentResult{Decision: "failed"}, 0, "", ""); err != nil {
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
