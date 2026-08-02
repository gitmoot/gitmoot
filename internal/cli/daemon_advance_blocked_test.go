package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestBlockedAdvanceSettlesQueriedJobState(t *testing.T) {
	job, _, events := runNonFastForwardBlockedAdvance(t)
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("queried job state = %q, want blocked", job.State)
	}
	if !daemonWorkerHasEvent(events, "advance_blocked") {
		t.Fatalf("events = %+v, want advance_blocked", events)
	}
}

func TestBlockedAdvancePostsBlockedDecision(t *testing.T) {
	_, body, _ := runNonFastForwardBlockedAdvance(t)
	if !strings.Contains(body, "**Decision:** `blocked`") {
		t.Fatalf("blocked advancement comment did not report blocked:\n%s", body)
	}
	if strings.Contains(body, "**Decision:** `implemented`") {
		t.Fatalf("blocked advancement comment claimed implementation:\n%s", body)
	}
	if !strings.Contains(body, "**Diagnostics:**") || !strings.Contains(body, "push implementation branch failed") {
		t.Fatalf("blocked advancement comment omitted push diagnostics:\n%s", body)
	}
}

func TestRetryBlockedAdvanceSettlesQueriedJobState(t *testing.T) {
	job, _, events := runRetryBlockedAdvance(t)
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("queried retry job state = %q, want blocked; events=%+v", job.State, events)
	}
	if !daemonWorkerHasEvent(events, "advance_blocked") {
		t.Fatalf("retry events = %+v, want advance_blocked", events)
	}
}

func TestRetryBlockedAdvancePostsBlockedDecision(t *testing.T) {
	_, body, _ := runRetryBlockedAdvance(t)
	if !strings.Contains(body, "**Decision:** `blocked`") {
		t.Fatalf("retry-blocked advancement comment did not report blocked:\n%s", body)
	}
	if strings.Contains(body, "**Decision:** `approved`") {
		t.Fatalf("retry-blocked advancement comment repeated the stored approval:\n%s", body)
	}
}

func TestBlockedAdvanceDoesNotAbortQueuedJobPass(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-blocked", RepoFullName: "owner/repo", Title: "Blocked", State: string(workflow.TaskImplementing), Branch: "task-blocked", WorktreePath: t.TempDir()}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "a-blocked", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-blocked", TaskID: "task-blocked"})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "b-healthy", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})

	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(agent runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		decision := "approved"
		if agent.Name == "lead" {
			decision = "implemented"
		}
		return &cliWorkerFakeAdapter{output: resultJSON(decision)}, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, ImplementationFinalizer: selectiveBlockedFinalizer{blockedJobID: "a-blocked"}}
	}

	runErr := runQueuedJobsForRepo(ctx, worker, 1, "", "")
	blocked, err := store.GetJob(ctx, "a-blocked")
	if err != nil {
		t.Fatalf("GetJob(a-blocked) returned error: %v", err)
	}
	healthy, err := store.GetJob(ctx, "b-healthy")
	if err != nil {
		t.Fatalf("GetJob(b-healthy) returned error: %v", err)
	}
	if healthy.State != string(workflow.JobSucceeded) {
		t.Fatalf("other job state = %q, want succeeded in the same pass (pass error: %v)", healthy.State, runErr)
	}
	if blocked.State != string(workflow.JobBlocked) {
		t.Fatalf("blocked job state = %q, want blocked", blocked.State)
	}
	if runErr != nil {
		t.Fatalf("runQueuedJobsForRepo returned error after processing other jobs: %v", runErr)
	}
}

func TestBlockedAdvanceIsNotRetried(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload, err := json.Marshal(workflow.JobPayload{Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	// Seed advance_started FIRST so the retry state is genuinely TRUE when advance_blocked
	// arrives. Starting from advance_blocked alone proved nothing: needsRetry already
	// defaults false, so the assertion passed whether or not advance_blocked was classified
	// at all -- removing it from the reset arm left this green. This ordering makes the
	// guard bind the RESET, which is what it claims.
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "blocked-no-retry", Agent: "lead", Type: "implement", State: string(workflow.JobBlocked), Payload: string(payload)}, db.JobEvent{
		JobID: "blocked-no-retry", Kind: "advance_started", Message: "advance began",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)

	// Control: with only advance_started the job MUST be classified for retry. Without it the
	// test cannot distinguish "the reset worked" from "nothing ever set it".
	needsRetry, err := worker.jobNeedsAdvanceRetry(ctx, "blocked-no-retry")
	if err != nil {
		t.Fatalf("jobNeedsAdvanceRetry returned error: %v", err)
	}
	if !needsRetry {
		t.Fatal("advance_started did not set the retry state, so this test cannot prove advance_blocked clears it")
	}

	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "blocked-no-retry", Kind: "advance_blocked", Message: "push rejected"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	needsRetry, err = worker.jobNeedsAdvanceRetry(ctx, "blocked-no-retry")
	if err != nil {
		t.Fatalf("jobNeedsAdvanceRetry returned error: %v", err)
	}
	if needsRetry {
		t.Fatal("advance_blocked did not clear the retry state set by advance_started")
	}
}

// TestBlockedSettlementStandsDownWhenItsOwnRunMovedOn covers the settlement's remaining arm:
// the generation is UNCHANGED -- so this is still the run the advancement observed -- but the
// state has moved to something that is neither blocked nor succeeded.
//
// It exists because a mutation pass found the arm unreachable from every other test here. The
// ABA cases all exit at the FIRST generation check, and the concurrency case does too, so
// emptying this fallthrough killed nothing: it was live production code with no guard on it,
// which is the inert-coverage shape pointed the other way -- not a test that cannot fail, but
// a branch nothing tests.
//
// The behaviour it pins: a verdict is only deliverable onto the state it was formed about.
// Once the run has settled some other way, this advancement has lost and must record that
// rather than overwrite it -- the same rule as the ABA case, for a different reason.
func TestBlockedSettlementStandsDownWhenItsOwnRunMovedOn(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload, err := json.Marshal(workflow.JobPayload{Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "moved-on", Agent: "lead", Type: "implement", State: string(workflow.JobRunning), Payload: string(payload)}, db.JobEvent{
		JobID: "moved-on", Kind: "advance_started", Message: "advance began",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	before, err := store.GetJob(ctx, "moved-on")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	observed := observedJobLifecycle(before)

	// The SAME run settles failed by another path -- no re-queue, so no new generation.
	if _, err := store.TransitionJobState(ctx, "moved-on", string(workflow.JobRunning), string(workflow.JobFailed)); err != nil {
		t.Fatalf("TransitionJobState returned error: %v", err)
	}

	// PREMISE: this arm is only exercised when the generation is UNCHANGED. If it moved,
	// the first check would absorb the case and this test would silently become a
	// duplicate of the ABA guard.
	armed, err := store.GetJob(ctx, "moved-on")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if armed.LifecycleGeneration != observed.generation {
		t.Fatalf("generation moved to %d; this fixture must keep the SAME run to reach the intended arm", armed.LifecycleGeneration)
	}
	if armed.State == observed.state {
		t.Fatalf("state did not move from %q; this fixture does not exercise the moved-on arm", observed.state)
	}

	worker := defaultJobWorker(store, io.Discard)
	deliveryFailed := workflow.BlockedError{Reason: "result delivery failed", ResultDeliveryFailed: true}
	if err := worker.recordBlockedAdvancement(ctx, "moved-on", observed, deliveryFailed, deliveryFailed); err != nil {
		t.Fatalf("settlement returned an error instead of standing down: %v", err)
	}

	after, err := store.GetJob(ctx, "moved-on")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if after.State != string(workflow.JobFailed) {
		t.Fatalf("state = %q, want failed: the settlement overwrote an outcome its own run had already reached", after.State)
	}
	jobEvents, err := store.ListJobEvents(ctx, "moved-on")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	superseded := 0
	for _, event := range jobEvents {
		if event.Kind == "advance_blocked" {
			t.Fatalf("settlement wrote advance_blocked over a run that had already settled (events=%+v)", jobEvents)
		}
		if event.Kind == "advance_blocked_superseded" {
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("advance_blocked_superseded events = %d, want exactly 1 (events=%+v)", superseded, jobEvents)
	}
}

// TestSupersededSettlementDoesNotClearPendingAdvanceRetry binds the OTHER half of
// advance_blocked_superseded: the kind exists precisely so a stale settlement stays queryable
// WITHOUT steering retry, and nothing pinned that.
//
// The mutant it kills is adding advance_blocked_superseded to jobNeedsAdvanceRetry's reset
// arm -- a one-line edit that reads like tidy symmetry with advance_blocked next to it. It
// would mean a settlement from a PREVIOUS run silently cancels the pending advancement of the
// CURRENT one: the live lifecycle keeps its result, is never advanced, and nothing reports it.
// That is the same false-green this PR exists to close, re-entering through the classifier
// instead of the CAS.
//
// The advance_started seeding is load-bearing. needsRetry defaults false, so a fixture that
// asserted only "superseded leaves it false" would pass against a classifier that never
// examined the kind at all -- and against the mutant. The retry state must be genuinely TRUE
// first, which is what makes the final assertion a statement about the RESET arm.
func TestSupersededSettlementDoesNotClearPendingAdvanceRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload, err := json.Marshal(workflow.JobPayload{Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "superseded-keeps-retry", Agent: "lead", Type: "implement", State: string(workflow.JobFailed), Payload: string(payload)}, db.JobEvent{
		JobID: "superseded-keeps-retry", Kind: "advance_started", Message: "advance began",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)

	needsRetry, err := worker.jobNeedsAdvanceRetry(ctx, "superseded-keeps-retry")
	if err != nil {
		t.Fatalf("jobNeedsAdvanceRetry returned error: %v", err)
	}
	if !needsRetry {
		t.Fatal("advance_started did not set the retry state, so this test cannot prove advance_blocked_superseded preserves it")
	}

	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "superseded-keeps-retry", Kind: "advance_blocked_superseded", Message: "settlement from a previous run"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	needsRetry, err = worker.jobNeedsAdvanceRetry(ctx, "superseded-keeps-retry")
	if err != nil {
		t.Fatalf("jobNeedsAdvanceRetry returned error: %v", err)
	}
	if !needsRetry {
		t.Fatal("advance_blocked_superseded cleared the pending advance retry: a settlement describing a PREVIOUS run must never cancel the current run's advancement")
	}
}

// TestGenericBlockedAdvanceLeavesTerminalStateAndDecisionUnchanged binds the FALSE side of
// ResultDeliveryFailed: settlement must fire ONLY for delivery failures, and every other
// BlockedError must keep the pre-existing behaviour.
//
// Without this, a mutant that settles unconditionally left every other guard in this file
// green, because they all exercise the delivery-failed side.
func TestGenericBlockedAdvanceLeavesTerminalStateAndDecisionUnchanged(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", Branch: "task-generic", PullRequest: 11,
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "generic-block", Agent: "lead", Type: "implement", State: string(workflow.JobSucceeded), Payload: string(payload)}, db.JobEvent{
		JobID: "generic-block", Kind: "advance_started", Message: "advance began",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CommenterFactory = func(string) github.Client { return comments }

	before, err := store.GetJob(ctx, "generic-block")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	generic := workflow.BlockedError{Reason: "downstream precondition not met"}
	if err := worker.recordBlockedAdvancement(ctx, "generic-block", observedJobLifecycle(before), generic, generic); err != nil {
		t.Fatalf("recordBlockedAdvancement returned error: %v", err)
	}

	// POSITIVE path-execution assertion. Every other assertion in this test is
	// absence-shaped -- state unchanged, decision unchanged -- and a recordBlockedAdvancement
	// that did NOTHING AT ALL satisfies all of them. Requiring the generic branch's own
	// event is what separates "the non-delivery-failed path ran and correctly left the row
	// alone" from "no path ran". Exactly one, because the generic branch uses AddJobEvent
	// rather than AddJobEventIfAbsent, so a duplicate would be a real regression.
	jobEvents, err := store.ListJobEvents(ctx, "generic-block")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	blockedEvents := 0
	for _, event := range jobEvents {
		if event.Kind == "advance_blocked" {
			blockedEvents++
		}
	}
	if blockedEvents != 1 {
		t.Fatalf("advance_blocked events = %d, want exactly 1; a generic block must RECORD itself, not silently no-op (events=%+v)", blockedEvents, jobEvents)
	}

	after, err := store.GetJob(ctx, "generic-block")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if after.State != string(workflow.JobSucceeded) {
		t.Fatalf("generic block changed terminal state to %q, want it left at succeeded", after.State)
	}
	var stored workflow.JobPayload
	if err := json.Unmarshal([]byte(after.Payload), &stored); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if stored.Result == nil || stored.Result.Decision != "implemented" {
		t.Fatalf("generic block changed the outward decision to %+v, want implemented", stored.Result)
	}

	// The FORGE-VISIBLE body, not merely the stored fields. The stored decision and the
	// posted comment are two different representations of one outcome, and it is the comment
	// a human reads. Asserting only the row leaves a mutant free to store `implemented` and
	// post `blocked`, which is precisely the false report this whole PR is about.
	if err := worker.postJobResultComment(ctx, "generic-block", runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime}, t.TempDir(), nil); err != nil {
		t.Fatalf("postJobResultComment returned error: %v", err)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want one", comments.posted)
	}
	body := comments.posted[0].body
	if !strings.Contains(body, "**Decision:** `implemented`") {
		t.Fatalf("generic block changed the FORGE-VISIBLE decision; want implemented:\n%s", body)
	}
	if strings.Contains(body, "**Decision:** `blocked`") {
		t.Fatalf("generic block reported blocked to the forge while the row still said implemented:\n%s", body)
	}
}

// TestBlockedSettlementDoesNotOverwriteConcurrentRetry is the concurrency guard. An operator
// retry can move a job failed -> queued underneath a slow advancement, and RetryJob clears
// Result -- so a settlement that CASes from the CURRENT row strands the fresh lifecycle with
// no result plus an advance_blocked event that suppresses further advancement.
//
// The settlement must CAS from the state it OBSERVED, and lose.
func TestBlockedSettlementDoesNotOverwriteConcurrentRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload, err := json.Marshal(workflow.JobPayload{Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "retry-race", Agent: "lead", Type: "implement", State: string(workflow.JobFailed), Payload: string(payload)}, db.JobEvent{
		JobID: "retry-race", Kind: "advance_started", Message: "advance began",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	// The advancement observed the job as failed, at THAT run's generation...
	before, err := store.GetJob(ctx, "retry-race")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	observed := observedJobLifecycle(before)

	// ...then an operator retry wins the race and re-queues it.
	if _, err := store.TransitionJobStateWithEvent(ctx, "retry-race", string(workflow.JobFailed), string(workflow.JobQueued), db.JobEvent{
		JobID: "retry-race", Kind: "retry_queued", Message: "operator retry",
	}); err != nil {
		t.Fatalf("retry transition returned error: %v", err)
	}

	worker := defaultJobWorker(store, io.Discard)
	deliveryFailed := workflow.BlockedError{Reason: "result delivery failed", ResultDeliveryFailed: true}
	if err := worker.recordBlockedAdvancement(ctx, "retry-race", observed, deliveryFailed, deliveryFailed); err != nil {
		t.Fatalf("recordBlockedAdvancement returned error: %v", err)
	}

	after, err := store.GetJob(ctx, "retry-race")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if after.State != string(workflow.JobQueued) {
		t.Fatalf("state after manual retry won = %q, want queued (a stale settlement overwrote a live lifecycle)", after.State)
	}

	jobEvents, err := store.ListJobEvents(ctx, "retry-race")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	superseded := 0
	for _, event := range jobEvents {
		if event.Kind == "advance_blocked" {
			t.Fatal("stale settlement wrote advance_blocked onto a re-queued job, suppressing its advancement")
		}
		if event.Kind == "advance_blocked_superseded" {
			superseded++
		}
	}
	// POSITIVE requirement. The two assertions above are both absence-shaped -- the state was
	// ALREADY queued before the settlement ran, and "no advance_blocked" is satisfied by a
	// settlement that never executed. Together they cannot distinguish "the stale settlement
	// ran and correctly stood down" from "recordBlockedAdvancement returned without doing
	// anything", which is the premise mutant this guard previously survived. The superseded
	// record is the only observable proof the path RAN and reached its intended arm.
	if superseded != 1 {
		t.Fatalf("advance_blocked_superseded events = %d, want exactly 1; without it this test cannot tell correct stand-down from no execution (events=%+v)", superseded, jobEvents)
	}
}

// TestBlockedSettlementRejectsABARetryLifecycle is the ABA guard.
//
// The concurrency guard above catches the case where the retry leaves the job in a DIFFERENT
// state (queued). It cannot catch the harder one: a full retry lifecycle that runs to
// completion and returns the job to the SAME STATE STRING the advancement observed. A CAS
// anchored on state alone then succeeds -- not through any oversight, but because the values
// really are equal. That is ABA, and it is why the anchor carries a monotonic generation.
//
// All three terminal states are exercised because the settlement has THREE arms keyed on the
// state it reads back, and a returning value fools each one differently:
//
//	failed    - the CAS itself succeeds and overwrites a live lifecycle's verdict
//	blocked   - misread as "a concurrent settlement reached the same outcome", attributing
//	            this run's advance_blocked to a different run
//	succeeded - misread as a contradiction and returned as an ERROR, turning a lost race
//	            into a daemon-visible failure
//
// One arm passing says nothing about the other two: they are separate branches, and a fix
// that only anchored the CAS would leave the blocked and succeeded arms wrong while this
// test's failed arm went green.
func TestBlockedSettlementRejectsABARetryLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name         string
		finalState   workflow.JobState
		wantSameKind string
	}{
		{name: "new lifecycle fails again", finalState: workflow.JobFailed},
		{name: "new lifecycle ends blocked", finalState: workflow.JobBlocked},
		{name: "new lifecycle succeeds", finalState: workflow.JobSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := daemonWorkerStore(t)
			payload, err := json.Marshal(workflow.JobPayload{Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}})
			if err != nil {
				t.Fatalf("Marshal returned error: %v", err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{ID: "aba", Agent: "lead", Type: "implement", State: string(workflow.JobFailed), Payload: string(payload)}, db.JobEvent{
				JobID: "aba", Kind: "advance_started", Message: "advance began",
			}); err != nil {
				t.Fatalf("CreateJobWithEvent returned error: %v", err)
			}

			// The slow advancement observes the FIRST run, at failed.
			before, err := store.GetJob(ctx, "aba")
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			observed := observedJobLifecycle(before)

			// A COMPLETE retry lifecycle now runs to its terminal state while that
			// advancement is still in flight: failed -> queued -> running -> tc.finalState.
			for _, step := range [][2]string{
				{string(workflow.JobFailed), string(workflow.JobQueued)},
				{string(workflow.JobQueued), string(workflow.JobRunning)},
				{string(workflow.JobRunning), string(tc.finalState)},
			} {
				transitioned, err := store.TransitionJobState(ctx, "aba", step[0], step[1])
				if err != nil {
					t.Fatalf("TransitionJobState(%s->%s) returned error: %v", step[0], step[1], err)
				}
				if !transitioned {
					t.Fatalf("TransitionJobState(%s->%s) did not transition; the ABA fixture did not arm", step[0], step[1])
				}
			}

			// PREMISE. The setup is only an ABA if the state genuinely returned to what
			// the advancement observed AND the generation genuinely moved. Assert both:
			// without the first, the plain state CAS would refuse this on its own and the
			// test would pass for the wrong reason; without the second, there is no new
			// lifecycle and nothing to be stale about.
			armed, err := store.GetJob(ctx, "aba")
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if armed.LifecycleGeneration == observed.generation {
				t.Fatalf("generation did not advance across a full retry (%d); this fixture is not testing ABA", armed.LifecycleGeneration)
			}
			if tc.finalState == workflow.JobFailed && armed.State != observed.state {
				t.Fatalf("state = %q, want it RETURNED to %q; without a returning value this is not the ABA case", armed.State, observed.state)
			}

			worker := defaultJobWorker(store, io.Discard)
			deliveryFailed := workflow.BlockedError{Reason: "result delivery failed", ResultDeliveryFailed: true}
			// A lost race is not an error. Reporting one would surface a routine
			// interleaving as a daemon failure -- the succeeded arm's specific defect.
			if err := worker.recordBlockedAdvancement(ctx, "aba", observed, deliveryFailed, deliveryFailed); err != nil {
				t.Fatalf("stale settlement returned an error instead of standing down: %v", err)
			}

			after, err := store.GetJob(ctx, "aba")
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if after.State != string(tc.finalState) {
				t.Fatalf("state = %q, want %q: a settlement from the PREVIOUS run overwrote the live lifecycle", after.State, tc.finalState)
			}

			jobEvents, err := store.ListJobEvents(ctx, "aba")
			if err != nil {
				t.Fatalf("ListJobEvents returned error: %v", err)
			}
			superseded := 0
			for _, event := range jobEvents {
				if event.Kind == "advance_blocked" {
					t.Fatalf("stale settlement attributed advance_blocked to a NEW lifecycle, suppressing its advancement (events=%+v)", jobEvents)
				}
				if event.Kind == "advance_blocked_superseded" {
					superseded++
				}
			}
			if superseded != 1 {
				t.Fatalf("advance_blocked_superseded events = %d, want exactly 1 (events=%+v)", superseded, jobEvents)
			}
		})
	}
}

type selectiveBlockedFinalizer struct {
	blockedJobID string
}

func runRetryBlockedAdvance(t *testing.T) (db.Job, string, []db.JobEvent) {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	configureTestGit(t, checkout)
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write retry fixture README: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "base")
	runGit(t, checkout, "branch", "-M", "task-implement")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-implement", RepoFullName: "owner/repo", Title: "Implement", State: string(workflow.TaskImplementing), Branch: "task-implement", WorktreePath: checkout}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", Branch: "task-implement", PullRequest: 9, TaskID: "task-implement", HeadSHA: strings.Repeat("a", 40),
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "implementation complete"},
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-retry-blocked", Agent: "lead", Type: "implement", State: string(workflow.JobSucceeded), Payload: string(payload)}, db.JobEvent{
		JobID: "job-retry-blocked", Kind: string(workflow.JobSucceeded), Message: "implementation result stored",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-retry-blocked", Kind: "advance_retry", Message: "retry advancement"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, ImplementationFinalizer: selectiveBlockedFinalizer{blockedJobID: "job-retry-blocked"}}
	}
	worker.CommenterFactory = func(string) github.Client { return comments }
	if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(store)); err != nil {
		t.Fatalf("retryPendingJobAdvancements returned error: %v", err)
	}
	if err := worker.postJobResultComment(ctx, "job-retry-blocked", runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime}, checkout, nil); err != nil {
		t.Fatalf("postJobResultComment returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-retry-blocked")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want one", comments.posted)
	}
	return job, comments.posted[0].body, events
}

func (f selectiveBlockedFinalizer) FinalizeImplementation(_ context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
	if job.ID == f.blockedJobID {
		return payload, blockedResultDelivery("push implementation branch failed: non-fast-forward")
	}
	return payload, nil
}

func runNonFastForwardBlockedAdvance(t *testing.T) (db.Job, string, []db.JobEvent) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	remote := filepath.Join(home, "remote.git")
	checkout := filepath.Join(home, "checkout")
	peer := filepath.Join(home, "peer")
	runGit(t, home, "init", "--bare", remote)
	runGit(t, home, "clone", remote, checkout)
	configureTestGit(t, checkout)
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base README: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "base")
	runGit(t, checkout, "branch", "-M", "main")
	runGit(t, checkout, "push", "-u", "origin", "main")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, checkout, "switch", "-c", "task-1")
	runGit(t, checkout, "push", "-u", "origin", "task-1")
	runGit(t, home, "clone", remote, peer)
	configureTestGit(t, peer)
	runGit(t, peer, "switch", "task-1")
	if err := os.WriteFile(filepath.Join(peer, "remote.txt"), []byte("remote advance\n"), 0o644); err != nil {
		t.Fatalf("write peer change: %v", err)
	}
	runGit(t, peer, "add", "remote.txt")
	runGit(t, peer, "commit", "-m", "advance remote")
	runGit(t, peer, "push", "origin", "task-1")
	remoteHead := strings.TrimSpace(runGitOutput(t, peer, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(checkout, "local.txt"), []byte("local implementation\n"), 0o644); err != nil {
		t.Fatalf("write local change: %v", err)
	}

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: checkout}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-non-fast-forward", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-1", PullRequest: 7, TaskID: "task-1", TaskTitle: "Task 1"})
	comments := &cliPollFakeGitHub{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{output: resultJSON("implemented")}, nil
	}
	worker.CommenterFactory = func(string) github.Client { return comments }
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-non-fast-forward")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %+v, want one", comments.posted)
	}
	remoteAfter := strings.TrimSpace(runGitOutput(t, checkout, "ls-remote", "origin", "refs/heads/task-1"))
	if !strings.HasPrefix(remoteAfter, remoteHead) {
		t.Fatalf("remote task branch changed after rejected push: got %q, want head %s", remoteAfter, remoteHead)
	}
	return job, comments.posted[0].body, events
}

func configureTestGit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot Test")
}

func resultJSON(decision string) string {
	return `{"gitmoot_result":{"decision":"` + decision + `","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
}
