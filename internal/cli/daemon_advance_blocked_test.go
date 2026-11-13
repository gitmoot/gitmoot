package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestBlockedAdvancePreservesUnknownAndLegacyPayloadFields(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	raw := `{
		"repo":"owner/repo",
		"preset_id":"legacy-template",
		"preset_resolved_commit":"abc123",
		"preset_content":"legacy body",
		"future_evidence":{"kind":"future","values":[1,2]},
		"result":{"decision":"implemented","summary":"model summary","future_result":{"score":7}}
	}`
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "blocked-preserves-payload", Agent: "lead", Type: "implement",
		State: string(workflow.JobSucceeded), Payload: raw,
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "model completed"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "blocked-preserves-payload")
	if err != nil {
		t.Fatalf("GetJob before settlement: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	if err := worker.settleBlockedAdvancement(ctx, job.ID, observedJobLifecycle(job), workflow.BlockedError{Reason: "pull request creation failed", ResultDeliveryFailed: true}); err != nil {
		t.Fatalf("settleBlockedAdvancement returned error: %v", err)
	}
	job, err = store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob after settlement: %v", err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", job.State)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(job.Payload), &envelope); err != nil {
		t.Fatalf("decode settled payload: %v", err)
	}
	for key, want := range map[string]string{
		"preset_id":              `"legacy-template"`,
		"preset_resolved_commit": `"abc123"`,
		"preset_content":         `"legacy body"`,
		"future_evidence":        `{"kind":"future","values":[1,2]}`,
	} {
		assertEquivalentJSON(t, key, envelope[key], want)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(envelope["result"], &result); err != nil {
		t.Fatalf("decode settled result: %v", err)
	}
	for key, want := range map[string]string{
		"decision":      `"blocked"`,
		"summary":       `"model summary"`,
		"future_result": `{"score":7}`,
	} {
		assertEquivalentJSON(t, "result."+key, result[key], want)
	}
}

func assertEquivalentJSON(t *testing.T, field string, got json.RawMessage, want string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode %s: %v (raw %q)", field, err, got)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode expected %s: %v", field, err)
	}
	gotJSON, _ := json.Marshal(gotValue)
	wantJSON, _ := json.Marshal(wantValue)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s = %s, want %s", field, gotJSON, wantJSON)
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

// TestRunErrorFromAPreviousRunDoesNotClobberARetriedJob is the regression guard for the ABA hole
// review found in handleRunJobError -- and it is a PRODUCTION defect, not a test gap.
//
// The reachable interleaving: RunJob produces an old run's ResultDeliveryFailed BlockedError; an
// operator retries the terminal job, clearing its result and moving it to queued at a NEWER
// generation; handleRunJobError then re-reads that fresh row, sees `queued`, and its queued fast
// path flips it to blocked. The retried job is left terminal with no result -- exactly the
// stranding this PR exists to stop, reached through a path the first fix did not anchor.
//
// Anchoring only settleBlockedAdvancement was anchoring one door of a room with several. The
// generation is now checked once, before every state fast path.
func TestRunErrorFromAPreviousRunDoesNotClobberARetriedJob(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload, err := json.Marshal(workflow.JobPayload{Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "clobber", Agent: "lead", Type: "implement", State: string(workflow.JobRunning), Payload: string(payload)}, db.JobEvent{
		JobID: "clobber", Kind: "running", Message: "job started",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	// A full retry lifecycle runs and returns the job to the SAME state the stale run
	// observed, at a NEWER generation.
	//
	// The first version of this test used running -> queued: two DIFFERENT states, so a
	// state-only comparison passed it too and it did not enforce the property it named.
	// Same-state / different-generation is the only fixture that discriminates a generation
	// anchor from a state one.
	observed := seedSameStateNewerGeneration(t, store, "clobber", workflow.JobRunning)

	worker := defaultJobWorker(store, io.Discard)
	deliveryFailed := workflow.BlockedError{Reason: "result delivery failed", ResultDeliveryFailed: true}
	if err := worker.handleRunJobError(ctx, "clobber", observed, deliveryFailed); err != nil {
		t.Fatalf("stale run error returned an error instead of standing down: %v", err)
	}

	after, err := store.GetJob(ctx, "clobber")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if after.State != string(workflow.JobRunning) {
		t.Fatalf("state = %q, want running: a PREVIOUS run's failure settled a job that had already been retried", after.State)
	}
}

// seedSameStateNewerGeneration returns a job to the SAME state it started in, at a NEWER
// generation, and returns the lifecycle a stale run observed.
//
// Same-state / different-generation is the ONLY discriminating fixture. Round 7's guard used
// running -> queued: two DIFFERENT states, so a state-only comparison passed it too and the test
// did not enforce the property it named. That is the discriminator rule this campaign has applied
// to other people's guards throughout, missed in my own one round after writing ABA guards that
// do it correctly.
func seedSameStateNewerGeneration(t *testing.T, store *db.Store, jobID string, state workflow.JobState) jobLifecycle {
	t.Helper()
	ctx := context.Background()
	before, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	observed := observedJobLifecycle(before)
	if state != workflow.JobQueued && state != workflow.JobRunning {
		t.Fatalf("seedSameStateNewerGeneration state = %q, want queued or running", state)
	}
	if _, err := workflow.CancelJob(ctx, store, jobID); err != nil {
		t.Fatalf("CancelJob returned error: %v", err)
	}
	if state == workflow.JobRunning {
		settled, err := workflow.SettleCancelledRunningJob(ctx, store, jobID, "stale generation fixture settled the cancelled run")
		if err != nil {
			t.Fatalf("SettleCancelledRunningJob returned error: %v", err)
		}
		if !settled {
			t.Fatal("SettleCancelledRunningJob did not settle the cancelled run")
		}
	}
	if _, err := workflow.RetryJob(ctx, store, jobID); err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	if state == workflow.JobRunning {
		claimed, err := store.ClaimRunningJob(ctx, jobID, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{
			JobID: jobID, Kind: string(workflow.JobRunning), Message: "retried job admitted by generation fixture",
		}, 0, "")
		if err != nil {
			t.Fatalf("ClaimRunningJob returned error: %v", err)
		}
		if !claimed {
			t.Fatal("ClaimRunningJob did not admit the retried job")
		}
	}
	armed, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if armed.State != observed.state {
		t.Fatalf("fixture state = %q, want it RETURNED to %q -- a different state does not discriminate a generation anchor from a state one", armed.State, observed.state)
	}
	if armed.LifecycleGeneration == observed.generation {
		t.Fatalf("generation did not advance (%d); this fixture is not testing an anchor", armed.LifecycleGeneration)
	}
	return observed
}

func mustWorkerJob(t *testing.T, store *db.Store, jobID string) db.Job {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	}
	return job
}

// TestPermissionBlockDoesNotBlockANewerRun anchors WRITER 1 (markJobPermissionBlocked).
//
// Review reproduced this one: the state-only CAS accepted a NEWER run's `running` and blocked it,
// giving new run state = "blocked", want running.
func TestPermissionBlockDoesNotBlockANewerRun(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "perm-aba", Agent: "lead", Type: "implement", State: string(workflow.JobRunning), Payload: "{}"}, db.JobEvent{
		JobID: "perm-aba", Kind: "running", Message: "job started",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	admitted := mustWorkerJob(t, store, "perm-aba")
	observed := seedSameStateNewerGeneration(t, store, "perm-aba", workflow.JobRunning)
	if admitted.LifecycleGeneration != observed.generation {
		t.Fatalf("admitted generation = %d, observed generation = %d", admitted.LifecycleGeneration, observed.generation)
	}

	if _, err := markJobPermissionBlocked(ctx, store, admitted); err != nil {
		t.Fatalf("markJobPermissionBlocked returned error: %v", err)
	}
	after, err := store.GetJob(ctx, "perm-aba")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if after.State != string(workflow.JobRunning) {
		t.Fatalf("state = %q, want running: a permission verdict from a PREVIOUS run blocked a newer one", after.State)
	}
}

// TestFinishQueuedJobDoesNotCloseANewerRun anchors WRITER 2 (finishQueuedJob).
func TestFinishQueuedJobDoesNotCloseANewerRun(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "queued-aba", Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: "{}"}, db.JobEvent{
		JobID: "queued-aba", Kind: "queued", Message: "job queued",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	admitted := mustWorkerJob(t, store, "queued-aba")
	observed := seedSameStateNewerGeneration(t, store, "queued-aba", workflow.JobQueued)
	if admitted.LifecycleGeneration != observed.generation {
		t.Fatalf("admitted generation = %d, observed generation = %d", admitted.LifecycleGeneration, observed.generation)
	}

	worker := defaultJobWorker(store, io.Discard)
	if err := worker.finishQueuedJob(ctx, admitted, workflow.JobFailed, errors.New("stale run failure")); err != nil {
		t.Fatalf("finishQueuedJob returned error: %v", err)
	}
	after, err := store.GetJob(ctx, "queued-aba")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if after.State != string(workflow.JobQueued) {
		t.Fatalf("state = %q, want queued: a failure from a PREVIOUS run closed a newer one", after.State)
	}
}
