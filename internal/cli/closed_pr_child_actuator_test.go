package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestClosedPullRequestChildConvergesThroughTheRetryActuator is the round-2 P1 of
// PR #1763, and it enters through the REAL pending-advancement actuator
// (retryPendingJobAdvancements) rather than any helper.
//
// THE INTERRUPTION IT REPRODUCES: a delegation child superseded because its pull
// request closed is terminalized and marked `advance_retry`, and the process stops
// before the parent is advanced. The actuator picks the child up by that marker and
// calls advanceJob, which needs the child's synthetic RESULT.
//
// Round 1 committed the marker with the transition but wrote the result separately, so
// an interruption in between left a failed child with a marker and NO result: the
// actuator rejected the nil result, re-stamped the marker, and repeated forever, and no
// sweep could repair it because the sweeps list QUEUED jobs. The result is now written
// in the same commit, so this state cannot exist and the actuator converges.
//
// SEMANTIC REVERSION THIS KILLS: write the payload as a second statement (or drop it
// from the transition) and the actuator loops instead of converging.
func TestClosedPullRequestChildConvergesThroughTheRetryActuator(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	checkout := filepath.Join(root, "checkout")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "clone", remote, checkout)
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot Test")
	writeFile(t, filepath.Join(checkout, "README.md"), "initial\n")
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "push", "-u", "origin", "main")
	runGit(t, checkout, "remote", "set-url", "origin", "https://github.com/gitmoot/gitmoot.git")
	if err := store.UpsertRepo(ctx, db.Repo{
		Owner: "gitmoot", Name: "gitmoot", CheckoutPath: checkout,
		DefaultBranch: "main", PollInterval: "30s",
	}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1",
		Title: "Parent", State: "implementing", Branch: "task-7", WorktreePath: checkout,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	// THE HEAD DELIBERATELY DOES NOT MATCH THE CHECKOUT. That is the production shape
	// for a closed PR - the shared checkout sits on a long-lived branch, never on a dead
	// PR's head - and an earlier version of this test pinned them equal, which masked the
	// defect: the actuator's delivery preflight failed, re-stamped the marker, and left
	// the parent stranded forever. Terminal parent advancement no longer depends on that
	// preflight, so convergence must hold with a mismatched head.

	// The parent fanned out one child and is waiting on it.
	parentPayload, err := json.Marshal(workflow.JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		Sender:      "coord",
		HeadSHA:     "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Result: &workflow.AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []workflow.Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal parent payload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "parent-job", Agent: "coord", Type: "ask",
		State: string(workflow.JobSucceeded), Payload: string(parentPayload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "done"}); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// PRODUCE THE INTERRUPTED STATE THROUGH PRODUCTION, not by hand. The first version
	// of this test seeded a failed child that already carried its result, so it could not
	// see the store dropping the payload - the same criticism the round-1 verdict made.
	for _, agent := range []db.Agent{
		{Name: "coord", Role: "lead", Runtime: "codex", RuntimeRef: "last", RepoScope: "gitmoot/gitmoot",
			Capabilities: []string{"ask"}, AutonomyPolicy: "auto", HealthStatus: "ok"},
		{Name: "api", Role: "reviewer", Runtime: "codex", RuntimeRef: "last", RepoScope: "gitmoot/gitmoot",
			Capabilities: []string{"review"}, AutonomyPolicy: "auto", HealthStatus: "ok"},
	} {
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent %s: %v", agent.Name, err)
		}
	}
	// A PR-bound job in production has its repo row and its task row; without them the
	// actuator's preflight fails on sql.ErrNoRows and this test would measure a fixture
	// gap instead of the convergence property.
	engine := workflow.Engine{Store: store}
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent): %v", err)
	}
	child := "parent-job/delegation/api"
	// The child must actually exist and be queued, or everything below measures nothing.
	spawned, err := store.GetJob(ctx, child)
	if err != nil {
		t.Fatalf("GetJob(%s): %v - the fan-out never produced the child", child, err)
	}
	if spawned.State != string(workflow.JobQueued) {
		t.Fatalf("child state = %q, want queued", spawned.State)
	}

	// STOP THE SEQUENCE in the exact window the P1 occupies: after the atomic terminal
	// commit, before the parent is advanced.
	workflow.SetClosedPRChildPreAdvanceHookForTest(func(context.Context, string) error {
		return errors.New("process stopped before advancing the parent")
	})
	_, finalizeErr := engine.FinalizeClosedPullRequestDelegationChild(ctx, child,
		"queued review job superseded: gitmoot/gitmoot pull request #7 is no longer open")
	workflow.SetClosedPRChildPreAdvanceHookForTest(nil)
	if finalizeErr == nil || !strings.Contains(finalizeErr.Error(), "process stopped before advancing the parent") {
		t.Fatalf("finalize error = %v, want the injected interruption: any other error means this test never reached the boundary", finalizeErr)
	}
	interrupted, err := store.GetJob(ctx, child)
	if err != nil {
		t.Fatalf("GetJob after interruption: %v", err)
	}
	if interrupted.State != string(workflow.JobFailed) {
		t.Fatalf("child state = %q after interruption, want failed", interrupted.State)
	}

	// The child must be a candidate at all - otherwise this test proves nothing.
	candidates, err := store.JobIDsWithPendingAdvanceRetry(ctx)
	if err != nil {
		t.Fatalf("JobIDsWithPendingAdvanceRetry: %v", err)
	}
	found := false
	for _, id := range candidates {
		if id == child {
			found = true
		}
	}
	if !found {
		t.Fatalf("child %s is not a pending-advancement candidate; candidates = %v", child, candidates)
	}

	// DRIVE THE REAL ACTUATOR, twice: convergence means the second pass finds nothing
	// owed, not that one pass happened to work.
	worker := defaultJobWorker(store, io.Discard)
	for pass := range 2 {
		if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(store)); err != nil {
			t.Fatalf("retryPendingJobAdvancements pass %d: %v", pass+1, err)
		}
	}

	events, err := store.ListJobEvents(ctx, child)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	retries, resolutions := 0, 0
	for _, event := range events {
		switch event.Kind {
		case "advance_retry":
			retries++
		case "advance_completed", "advance_retried", "advance_blocked", "advance_retry_skipped":
			resolutions++
		}
	}
	if resolutions == 0 {
		t.Fatalf("the actuator never resolved the advancement: it is looping. events = %+v", events)
	}
	// THE STRUCTURAL GUARANTEE: a parent-DAG-only advancement cannot dispatch the
	// child's OWN delegations. If this job appears, the actuator ran the full advance
	// under an unvalidated checkout.
	if _, err := store.GetJob(ctx, child+"/delegation/grandchild"); err == nil {
		t.Fatal("the child's own delegation was dispatched: parent-only advancement ran the full advance")
	}
	if remaining, err := store.JobIDsWithPendingAdvanceRetry(ctx); err != nil {
		t.Fatalf("JobIDsWithPendingAdvanceRetry after: %v", err)
	} else {
		for _, id := range remaining {
			if id == child {
				t.Fatalf("child %s is STILL owed an advancement after two actuator passes: retries=%d resolutions=%d",
					child, retries, resolutions)
			}
		}
	}
}

// TestClosedPullRequestChildWithoutResultDoesNotLoopForever is the boundary the round-1
// test missed: a child that reached `failed` with a marker but NO result - the state the
// old non-atomic sequence could leave behind, and the state any pre-fix row still in a
// live database is in. The actuator must not spin on it forever.
func TestClosedPullRequestChildWithoutResultDoesNotLoopForever(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	childPayload, err := json.Marshal(workflow.JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		ParentJobID: "missing-parent",
		Sender:      "coord",
		// NO Result: the unrepairable shape.
	})
	if err != nil {
		t.Fatalf("marshal child payload: %v", err)
	}
	child := "missing-parent/delegation/api"
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: child, Agent: "api", Type: "review", State: string(workflow.JobFailed),
		Payload: string(childPayload), ParentJobID: "missing-parent", DelegationID: "api",
	}, db.JobEvent{Kind: "superseded_pr_closed", Message: "pull request #7 is no longer open"}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID: child, Kind: "advance_retry", Message: "parent advancement owed",
	}); err != nil {
		t.Fatalf("add advance_retry: %v", err)
	}

	worker := defaultJobWorker(store, io.Discard)
	for pass := range 3 {
		if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(store)); err != nil {
			t.Fatalf("retryPendingJobAdvancements pass %d: %v", pass+1, err)
		}
	}

	// The bound that matters: the marker must not GROW without limit. A single
	// last-one-wins marker is the design (recordAdvanceRetryOnce); an append per tick is
	// the unbounded audit trail this campaign has already produced once.
	events, err := store.ListJobEvents(ctx, child)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	retries := 0
	for _, event := range events {
		if event.Kind == "advance_retry" {
			retries++
		}
	}
	if retries > 2 {
		t.Fatalf("advance_retry events = %d after three actuator passes: the marker grows per tick", retries)
	}
}

// seedTerminalChild writes a settled delegation child with the given state, decision and
// event kind, so a test can state exactly which shape it is presenting to the actuator.
func seedTerminalChild(t *testing.T, store *db.Store, id string, state workflow.JobState, decision string, eventKind string) {
	t.Helper()
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		ParentJobID: "parent-job",
		HeadSHA:     "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Sender:      "coord",
		Result:      &workflow.AgentResult{Decision: decision, Summary: "terminal"},
	})
	if err != nil {
		t.Fatalf("marshal child payload: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: id, Agent: "api", Type: "review", State: string(state),
		Payload: string(payload), ParentJobID: "parent-job", DelegationID: "api",
	}, db.JobEvent{Kind: eventKind, Message: "terminal"}); err != nil {
		t.Fatalf("create child %s: %v", id, err)
	}
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID: id, Kind: "advance_retry", Message: "parent advancement owed",
	}); err != nil {
		t.Fatalf("add advance_retry to %s: %v", id, err)
	}
}

// TestCheckoutBypassIsLimitedToTheClosedPullRequestShape is the round-4 P1 control. The
// bypass exists for ONE shape - a child terminalized by the closed-PR sweep - and
// Engine.AdvanceJob does much more than parent advancement for other shapes: it
// dispatches their own delegations and continues into the review merge gate. So a
// SUCCEEDED review child and an IMPLEMENTED child must keep the full preflight even
// though both are settled and both carry a result.
//
// SEMANTIC REVERSION THIS KILLS: widen the predicate back to "any settled child with any
// result" and these two shapes advance under an unvalidated checkout.
func TestCheckoutBypassIsLimitedToTheClosedPullRequestShape(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	// No repo row at all, so checkoutForJob CANNOT succeed for any of these jobs. That
	// makes the preflight the only thing standing between them and AdvanceJob.
	seedTerminalChild(t, store, "child-superseded", workflow.JobFailed, "failed", "superseded_pr_closed")
	seedTerminalChild(t, store, "child-succeeded", workflow.JobSucceeded, "approved", "succeeded")
	seedTerminalChild(t, store, "child-implemented", workflow.JobFailed, "implemented", "failed")
	// AN ORDINARY FAILED REVIEW CHILD: same state and same decision as the superseded
	// shape, differing ONLY in that no closed-PR supersession event was written. It
	// isolates the event discriminator - without it, this shape is indistinguishable from
	// the sweep's own output and would advance under an unvalidated checkout.
	seedTerminalChild(t, store, "child-plain-failure", workflow.JobFailed, "failed", "failed")

	for _, agent := range []db.Agent{{
		Name: "api", Role: "reviewer", Runtime: "codex", RuntimeRef: "last",
		RepoScope: "gitmoot/gitmoot", Capabilities: []string{"review"},
		AutonomyPolicy: "auto", HealthStatus: "ok",
	}} {
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent: %v", err)
		}
	}

	worker := defaultJobWorker(store, io.Discard)
	if err := retryPendingJobAdvancements(ctx, worker, "", "", nil, newTickCandidates(store)); err != nil {
		t.Fatalf("retryPendingJobAdvancements: %v", err)
	}

	for _, tc := range []struct {
		id         string
		wantBypass bool
		why        string
	}{
		{"child-superseded", true, "the closed-PR shape must advance without a checkout"},
		{"child-succeeded", false, "a succeeded review child must keep the preflight: AdvanceJob would reach the merge gate"},
		{"child-implemented", false, "an implemented child must keep the preflight: AdvanceJob continues implementation advancement"},
		{"child-plain-failure", false, "an ordinary failed child must keep the preflight: only the closed-PR sweep's own output is exempt"},
	} {
		events, err := store.ListJobEvents(ctx, tc.id)
		if err != nil {
			t.Fatalf("ListJobEvents(%s): %v", tc.id, err)
		}
		preflightRefused := false
		for _, event := range events {
			if event.Kind == "advance_retry" && strings.Contains(event.Message, "preflight failed") {
				preflightRefused = true
			}
		}
		if tc.wantBypass && preflightRefused {
			t.Fatalf("%s: preflight refused it, but %s", tc.id, tc.why)
		}
		if !tc.wantBypass && !preflightRefused {
			t.Fatalf("%s: the checkout bypass swallowed it, but %s", tc.id, tc.why)
		}
	}
}
