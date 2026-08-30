package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestImplementationFinalizationTargetRejectsEveryMissingField(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		payload workflow.JobPayload
		task    *db.Task
		want    string
	}{
		{
			name:    "task id",
			payload: workflow.JobPayload{Repo: "owner/repo", Branch: "feature/fix", FixWorktree: true, WorktreePath: "/tmp/fix"},
			want:    "`gitmoot agent implement lead \"Implement the task.\" --repo owner/repo --task <task-id> --branch <branch>`",
		},
		{
			name:    "worktree path",
			payload: workflow.JobPayload{Repo: "owner/repo", Branch: "feature/fix", TaskID: "task-1", FixWorktree: true},
			task:    &db.Task{ID: "task-1", RepoFullName: "owner/repo", Branch: "feature/fix"},
			want:    "no worktree path",
		},
		{
			name:    "branch",
			payload: workflow.JobPayload{Repo: "owner/repo", PullRequest: 1514, TaskID: "task-1", FixWorktree: true, WorktreePath: "/tmp/fix"},
			task:    &db.Task{ID: "task-1", RepoFullName: "owner/repo", WorktreePath: "/tmp/stale-task"},
			want:    "carries no payload branch",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := daemonWorkerStore(t)
			if test.task != nil {
				if err := store.UpsertTask(ctx, *test.task); err != nil {
					t.Fatalf("UpsertTask returned error: %v", err)
				}
			}
			_, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "advance-fix", Agent: "lead", Type: "implement"}, test.payload, implementationFinalizationBeforeRun)
			var blocked workflow.BlockedError
			if !errors.As(err, &blocked) || !blocked.ResultDeliveryFailed {
				t.Fatalf("error = %v, want result-delivery BlockedError", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestAdvanceImplementationPreflightBlocksBeforeModelAndKeepsResultHonest(t *testing.T) {
	for _, test := range []struct {
		mode          string
		wantCondition string
	}{
		{mode: "stale", wantCondition: "stale or divergent"},
		{mode: "divergent", wantCondition: "stale or divergent"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			result := runAdvanceImplementationPreflightFixture(t, test.mode)
			for _, want := range []string{
				"task-1514", test.wantCondition, result.currentHead, result.expectedHead, result.fixWorktree,
				"fetch origin feature/semantic-census",
				"reset --hard " + result.expectedHead,
			} {
				if !strings.Contains(result.message, want) {
					t.Fatalf("blocked message missing %q: %s", want, result.message)
				}
			}
			if !result.remedyCleared {
				t.Fatal("documented reset remedy did not clear the preflight refusal")
			}
			if test.mode == "divergent" {
				git := gitutil.NewHostClient(result.fixWorktree)
				currentAncestorDispatch, err := git.IsAncestor(context.Background(), result.currentHead, result.expectedHead)
				if err != nil {
					t.Fatalf("compare divergent current head with dispatch head: %v", err)
				}
				dispatchAncestorCurrent, err := git.IsAncestor(context.Background(), result.expectedHead, result.currentHead)
				if err != nil {
					t.Fatalf("compare divergent dispatch head with current head: %v", err)
				}
				if currentAncestorDispatch || dispatchAncestorCurrent {
					t.Fatalf("divergent fixture ancestry current->dispatch=%t dispatch->current=%t, want neither", currentAncestorDispatch, dispatchAncestorCurrent)
				}
			}
		})
	}
}

func TestAdvanceImplementationPreflightCannotVerifyMissingDispatchObjectRemotely(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "unfetched-stale")
	for _, want := range []string{
		"task-1514", result.fixWorktree, "missing dispatch head object " + result.expectedHead + " locally",
		"cannot verify that object against origin", "will not guess a fetch/reset remedy",
		"dispatch a new fix job against pull request #1514's current head",
	} {
		if !strings.Contains(result.message, want) {
			t.Fatalf("blocked message missing %q: %s", want, result.message)
		}
	}
	for _, forbidden := range []string{"git -C", "gitmoot job retry"} {
		if strings.Contains(result.message, forbidden) {
			t.Fatalf("missing-object message contains guessed recovery %q: %s", forbidden, result.message)
		}
	}
	if !result.remedyCleared {
		t.Fatal("new dispatch with the current object did not clear the preflight refusal")
	}
}

func TestAdvanceImplementationPreflightBoundsPersistentErrorsAcrossTicks(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "persistent-preflight-error")
	if len(result.tickErrors) != 3 {
		t.Fatalf("tick count = %d, want 3", len(result.tickErrors))
	}
	for tick, err := range result.tickErrors {
		if err != nil {
			t.Fatalf("tick %d aborted batch: %v", tick+1, err)
		}
		if got := result.attempts[tick]; got != tick+1 {
			t.Fatalf("tick %d durable attempts = %d, want %d", tick+1, got, tick+1)
		}
		if tick < 2 {
			retryAt, err := time.Parse(time.RFC3339Nano, result.retryAts[tick])
			if err != nil {
				t.Fatalf("tick %d retry time %q: %v", tick+1, result.retryAts[tick], err)
			}
			if !retryAt.After(result.tickStarted[tick]) {
				t.Fatalf("tick %d retry time %s is not after scheduler start %s", tick+1, retryAt, result.tickStarted[tick])
			}
			if hold := retryAt.Sub(result.tickStarted[tick]); hold < time.Second {
				t.Fatalf("tick %d retry hold = %s, want at least %s", tick+1, hold, time.Second)
			}
		}
	}
	for _, want := range []string{
		"automatic implementation preflight retries exhausted after 3 attempts",
		"task task-1514", result.fixWorktree, "compare implementation worktree HEAD",
		"inspect and repair the worktree", "gitmoot job retry advance-fix-persistent-preflight-error",
		"dispatch a fresh fix job against pull request #1514's current head",
	} {
		if !strings.Contains(result.failureMessage, want) {
			t.Fatalf("exhaustion message missing %q: %s", want, result.failureMessage)
		}
	}
	if result.adapterCalls != 0 || result.jobState != string(workflow.JobFailed) {
		t.Fatalf("persistent preflight error calls=%d state=%s, want zero and failed", result.adapterCalls, result.jobState)
	}
	if result.healthyState != string(workflow.JobSucceeded) || result.healthyCalls != 1 {
		t.Fatalf("healthy sibling state=%s calls=%d, want succeeded once in the first batch", result.healthyState, result.healthyCalls)
	}
	if result.exhaustedEvents != 1 || result.deferredEvents != 2 {
		t.Fatalf("retry event kinds exhausted=%d deferred=%d, want 1/2", result.exhaustedEvents, result.deferredEvents)
	}
	if got, want := strings.Join(result.retryEventKinds, ","), strings.Join([]string{blockerDeferredEventKind, blockerDeferredEventKind, blockerExhaustedEventKind, string(workflow.JobFailed)}, ","); got != want {
		t.Fatalf("retry event order = %s, want %s", got, want)
	}
	if result.failedEmissions != 1 {
		t.Fatalf("job.failed emissions = %d, want 1", result.failedEmissions)
	}
	if result.blockerSuggestedActionPresent {
		t.Fatal("stale blocker_suggested_action survived in persisted payload")
	}
	if !result.remedyCleared {
		t.Fatal("repair plus job retry did not clear the exhausted preflight refusal")
	}
}

func TestImplementationPreflightExhaustionReselectionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload := workflow.JobPayload{
		Repo: "owner/repo", TaskID: "task-1514", WorktreePath: "/tmp/fix", PullRequest: 1514,
		BlockerClass: "implementation_preflight", BlockerAttempts: implementationPreflightAttemptLimit,
		BlockerPreDelivery: true,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "exhausted-reselection", Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: string(encoded)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "exhausted-reselection", Kind: blockerExhaustedEventKind, Message: "recorded before interruption"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	job, parsed, err := daemonWorkerJobPayload(ctx, store, "exhausted-reselection")
	if err != nil {
		t.Fatalf("load queued exhausted job: %v", err)
	}
	if err := worker.retryImplementationPreflight(ctx, job, parsed, errors.New("persistent git failure")); err != nil {
		t.Fatalf("retryImplementationPreflight returned error: %v", err)
	}
	after, parsed, err := daemonWorkerJobPayload(ctx, store, job.ID)
	if err != nil {
		t.Fatalf("load settled job: %v", err)
	}
	if after.State != string(workflow.JobFailed) || parsed.BlockerAttempts != implementationPreflightAttemptLimit {
		t.Fatalf("settled state=%s attempts=%d, want failed/%d", after.State, parsed.BlockerAttempts, implementationPreflightAttemptLimit)
	}
	jobEvents, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if got := countJobEventsByKind(jobEvents, blockerExhaustedEventKind); got != 1 {
		t.Fatalf("exhausted events = %d, want 1", got)
	}
}

func TestImplementationPreflightStaleGenerationHasNoTerminalSideEffects(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload := workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 1514, TaskID: "task-1514", WorktreePath: "/tmp/fix",
		BlockerAttempts: implementationPreflightAttemptLimit - 1,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	const jobID = "stale-preflight-generation"
	if err := store.CreateJob(ctx, db.Job{ID: jobID, Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: string(encoded)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	staleJob, stalePayload, err := daemonWorkerJobPayload(ctx, store, jobID)
	if err != nil {
		t.Fatalf("load stale lifecycle: %v", err)
	}
	transitioned, err := store.TransitionJobState(ctx, jobID, string(workflow.JobQueued), string(workflow.JobFailed))
	if err != nil || !transitioned {
		t.Fatalf("settle old lifecycle: transitioned=%t err=%v", transitioned, err)
	}
	current, err := workflow.RetryJob(ctx, store, jobID)
	if err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	comments := &cliPollFakeGitHub{}
	sink := &recordingSink{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CommenterFactory = func(string) github.Client { return comments }
	worker.EventSinkOverride = sink
	if err := worker.retryImplementationPreflight(ctx, staleJob, stalePayload, errors.New("stale lifecycle failure")); err != nil {
		t.Fatalf("stale retryImplementationPreflight returned error: %v", err)
	}
	after, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if after.State != string(workflow.JobQueued) || after.LifecycleGeneration != current.LifecycleGeneration {
		t.Fatalf("current lifecycle changed: state=%s generation=%d, want queued/%d", after.State, after.LifecycleGeneration, current.LifecycleGeneration)
	}
	if len(comments.posted) != 0 {
		t.Fatalf("stale lifecycle posted %d result comments", len(comments.posted))
	}
	if got := len(sink.byType(events.EventJobFailed)); got != 0 {
		t.Fatalf("stale lifecycle emitted %d job.failed events", got)
	}
	jobEvents, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if got := countJobEventsByKind(jobEvents, "comment_posted"); got != 0 {
		t.Fatalf("stale lifecycle reserved %d result comments", got)
	}
}

func TestImplementationPreflightExhaustionDeduplicatesWithinLifecycle(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload := workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 1514, TaskID: "task-1514", WorktreePath: "/tmp/fix",
		BlockerAttempts: implementationPreflightAttemptLimit - 1,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	const jobID = "two-preflight-lifecycles"
	if err := store.CreateJob(ctx, db.Job{ID: jobID, Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: string(encoded)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	job, parsed, err := daemonWorkerJobPayload(ctx, store, jobID)
	if err != nil {
		t.Fatalf("load first lifecycle: %v", err)
	}
	if err := worker.retryImplementationPreflight(ctx, job, parsed, errors.New("first lifecycle failure")); err != nil {
		t.Fatalf("exhaust first lifecycle: %v", err)
	}
	if _, err := workflow.RetryJob(ctx, store, jobID); err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	for attempt := 1; attempt <= implementationPreflightAttemptLimit; attempt++ {
		job, parsed, err = daemonWorkerJobPayload(ctx, store, jobID)
		if err != nil {
			t.Fatalf("load second lifecycle attempt %d: %v", attempt, err)
		}
		if err := worker.retryImplementationPreflight(ctx, job, parsed, fmt.Errorf("second lifecycle failure %d", attempt)); err != nil {
			t.Fatalf("second lifecycle attempt %d: %v", attempt, err)
		}
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if got := countJobEventsByKind(events, blockerExhaustedEventKind); got != 2 {
		t.Fatalf("exhausted events across two lifecycles = %d, want 2", got)
	}
}

func TestImplementationPreflightExhaustionDoesNotDeduplicateOtherBlockerClass(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload := workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 1514, TaskID: "task-1514", WorktreePath: "/tmp/fix",
		BlockerClass: string(blockerClassRuntimeQuota), BlockerAttempts: implementationPreflightAttemptLimit,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	const jobID = "cross-class-preflight-exhaustion"
	if err := store.CreateJob(ctx, db.Job{ID: jobID, Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: string(encoded)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	job, parsed, err := daemonWorkerJobPayload(ctx, store, jobID)
	if err != nil {
		t.Fatalf("load queued job: %v", err)
	}
	if err := worker.retryImplementationPreflight(ctx, job, parsed, errors.New("cross-class exhaustion")); err != nil {
		t.Fatalf("retryImplementationPreflight returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if got := countJobEventsByKind(events, blockerExhaustedEventKind); got != 1 {
		t.Fatalf("cross-class exhausted events = %d, want 1", got)
	}
}

func TestImplementationPreflightRetrySharesLifetimeBoundAcrossBlockerClasses(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	raw := `{"repo":"owner/repo","task_id":"task-1514","worktree_path":"/tmp/fix","pull_request":1514}`
	if err := store.CreateJob(ctx, db.Job{ID: "alternating-blockers", Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: raw}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	job, payload, err := daemonWorkerJobPayload(ctx, store, "alternating-blockers")
	if err != nil {
		t.Fatalf("load initial job: %v", err)
	}
	if err := worker.retryImplementationPreflight(ctx, job, payload, errors.New("preflight one")); err != nil {
		t.Fatalf("first preflight retry: %v", err)
	}
	job, payload, err = daemonWorkerJobPayload(ctx, store, job.ID)
	if err != nil {
		t.Fatalf("load after first preflight: %v", err)
	}
	if payload.BlockerAttempts != 1 {
		t.Fatalf("first preflight attempts = %d, want 1", payload.BlockerAttempts)
	}
	transitioned, err := store.TransitionJobState(ctx, job.ID, string(workflow.JobQueued), string(workflow.JobRunning))
	if err != nil || !transitioned {
		t.Fatalf("queue job for operational blocker: transitioned=%t err=%v", transitioned, err)
	}
	deferred, err := worker.deferOperationalBlockerPreTerminal(ctx, job.ID, workflow.DeliveryError{Err: errors.New("rate limit reached; try again in 30 seconds")})
	if err != nil || !deferred {
		t.Fatalf("operational blocker: deferred=%t err=%v", deferred, err)
	}
	job, payload, err = daemonWorkerJobPayload(ctx, store, job.ID)
	if err != nil {
		t.Fatalf("load after operational blocker: %v", err)
	}
	if payload.BlockerAttempts != 2 || payload.BlockerClass != string(blockerClassRuntimeQuota) {
		t.Fatalf("operational blocker = class %q attempts %d, want runtime_quota/2", payload.BlockerClass, payload.BlockerAttempts)
	}
	if err := worker.retryImplementationPreflight(ctx, job, payload, errors.New("preflight two")); err != nil {
		t.Fatalf("second preflight retry: %v", err)
	}
	job, payload, err = daemonWorkerJobPayload(ctx, store, job.ID)
	if err != nil {
		t.Fatalf("load after exhausted preflight: %v", err)
	}
	if payload.BlockerAttempts != 3 || job.State != string(workflow.JobFailed) {
		t.Fatalf("final preflight = attempts %d state %s, want 3/failed", payload.BlockerAttempts, job.State)
	}
}

func TestImplementationPreflightRetryPreservesUnknownAndLegacyPayloadFields(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	raw := `{"repo":"owner/repo","task_id":"task-1514","worktree_path":"/tmp/fix","pull_request":1514,"future_evidence":{"proof":true},"preset_id":"legacy-preset"}`
	if err := store.CreateJob(ctx, db.Job{ID: "preflight-envelope", Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Payload: raw}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	job, payload, err := daemonWorkerJobPayload(ctx, store, "preflight-envelope")
	if err != nil {
		t.Fatalf("load initial job: %v", err)
	}
	if err := worker.retryImplementationPreflight(ctx, job, payload, errors.New("temporary git failure")); err != nil {
		t.Fatalf("retryImplementationPreflight returned error: %v", err)
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(after.Payload), &envelope); err != nil {
		t.Fatalf("unmarshal stored payload: %v", err)
	}
	for key, want := range map[string]string{
		"future_evidence": `{"proof":true}`,
		"preset_id":       `"legacy-preset"`,
	} {
		if got := string(envelope[key]); got != want {
			t.Fatalf("stored %s = %s, want %s", key, got, want)
		}
	}
}

func TestAdvanceImplementationPreflightRetriesTransientErrorThenProceeds(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "transient-preflight-error")
	if len(result.tickErrors) != 2 || result.tickErrors[0] != nil || result.tickErrors[1] != nil {
		t.Fatalf("tick errors = %v, want two non-aborting ticks", result.tickErrors)
	}
	if len(result.attempts) != 2 || result.attempts[0] != 1 || result.attempts[1] != 1 {
		t.Fatalf("durable attempts = %v, want [1 1] after recovery (state=%s blocked=%q failed=%q)", result.attempts, result.jobState, result.message, result.failureMessage)
	}
	if result.adapterCalls != 1 {
		t.Fatalf("adapter calls = %d, want one after transient preflight error cleared (state=%s attempts=%v failure=%q ticks=%v)", result.adapterCalls, result.jobState, result.attempts, result.failureMessage, result.tickErrors)
	}
	if result.healthyState != string(workflow.JobSucceeded) || result.healthyCalls != 1 {
		t.Fatalf("healthy sibling state=%s calls=%d, want succeeded once while peer retried", result.healthyState, result.healthyCalls)
	}
	if len(result.deliveredPrompts) != 1 || strings.Contains(result.deliveredPrompts[0], "NOTE (operational retry") {
		t.Fatalf("pre-delivery retry prompt carried side-effect reconciliation: %q", result.deliveredPrompts)
	}
}

func TestAdvanceImplementationPreflightRejectsMissingHeadBeforeModel(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "missing-head")
	for _, want := range []string{
		"task-1514", result.fixWorktree, "no dispatch head SHA", "pull request #1514",
		"--task task-1514", "--pr 1514", "--branch feature/semantic-census", "--head-sha <sha>",
	} {
		if !strings.Contains(result.message, want) {
			t.Fatalf("blocked message missing %q: %s", want, result.message)
		}
	}
	if !result.remedyCleared {
		t.Fatal("fresh dispatch-head metadata did not clear the missing-head refusal")
	}
}

func TestAdvanceImplementationPreflightAllowsCheckoutAheadOfDispatchHead(t *testing.T) {
	result := runAdvanceImplementationPreflightFixture(t, "ahead")
	if result.currentHead == result.expectedHead {
		t.Fatalf("ahead fixture current head = dispatch head %s", result.currentHead)
	}
	runDaemonWorkerGit(t, result.fixWorktree, "merge-base", "--is-ancestor", result.expectedHead, result.currentHead)
	if result.adapterCalls != 1 {
		t.Fatalf("adapter calls = %d, want one for checkout ahead of frozen dispatch head", result.adapterCalls)
	}
	if result.jobState != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed result from invoked adapter", result.jobState)
	}
}

func TestAdvanceImplementationPreflightRejectsDetachedAndWrongBranchBeforeModel(t *testing.T) {
	for _, mode := range []string{"detached", "wrong-branch"} {
		t.Run(mode, func(t *testing.T) {
			result := runAdvanceImplementationPreflightFixture(t, mode)
			for _, want := range []string{"task-1514", result.fixWorktree, "feature/semantic-census"} {
				if !strings.Contains(result.message, want) {
					t.Fatalf("blocked message missing %q: %s", want, result.message)
				}
			}
			if mode == "detached" && !strings.Contains(result.message, "current git branch is empty") {
				t.Fatalf("detached message = %q", result.message)
			}
			if mode == "wrong-branch" && !strings.Contains(result.message, "wrong-branch") {
				t.Fatalf("wrong-branch message = %q", result.message)
			}
		})
	}
}

type advanceImplementationPreflightResult struct {
	message                       string
	currentHead                   string
	expectedHead                  string
	fixWorktree                   string
	adapterCalls                  int
	jobState                      string
	remedyCleared                 bool
	runErr                        error
	failureMessage                string
	healthyState                  string
	healthyCalls                  int
	tickErrors                    []error
	attempts                      []int
	retryAts                      []string
	tickStarted                   []time.Time
	deliveredPrompts              []string
	exhaustedEvents               int
	deferredEvents                int
	blockerSuggestedActionPresent bool
	failedEmissions               int
	retryEventKinds               []string
}

func runAdvanceImplementationPreflightFixture(t *testing.T, mode string) advanceImplementationPreflightResult {
	t.Helper()
	ctx := context.Background()
	const branch = "feature/semantic-census"
	store := daemonWorkerStore(t)
	registered := createDaemonWorkerGitCheckout(t, "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runDaemonWorkerGit(t, filepath.Dir(remote), "init", "--bare", remote)
	runDaemonWorkerGit(t, registered, "remote", "set-url", "origin", remote)
	runDaemonWorkerGit(t, registered, "push", "-u", "origin", "main")
	runDaemonWorkerGit(t, registered, "switch", "-c", branch)
	runDaemonWorkerGit(t, registered, "push", "-u", "origin", branch)
	oldHead := strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
	fixWorktree := filepath.Join(t.TempDir(), "fix-worktree")
	runDaemonWorkerGit(t, filepath.Dir(fixWorktree), "clone", "--branch", branch, remote, fixWorktree)
	configureTestGit(t, fixWorktree)
	expectedHead := oldHead
	currentHead := ""
	var restorePreflightFixture func()
	wantBlocked := true
	switch mode {
	case "stale":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance reviewed branch")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", branch)
	case "unfetched-stale":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance reviewed branch")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
		runDaemonWorkerGit(t, remote, "update-ref", "refs/pull/1514/head", expectedHead)
	case "divergent":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance reviewed branch")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = strings.TrimSpace(runGitOutput(t, registered, "rev-parse", "HEAD"))
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", branch)
		runDaemonWorkerGit(t, fixWorktree, "commit", "--allow-empty", "-m", "divergent local fix")
	case "persistent-preflight-error":
		currentHead = oldHead
		restorePreflightFixture = breakGitBranchTip(t, fixWorktree, branch)
	case "transient-preflight-error":
		if os.Geteuid() == 0 {
			currentHead = oldHead
			restorePreflightFixture = breakGitBranchTip(t, fixWorktree, branch)
		} else {
			currentHead, restorePreflightFixture = makePackedCurrentCommitInaccessible(t, fixWorktree, oldHead)
		}
	case "missing-head":
		runDaemonWorkerGit(t, registered, "commit", "--allow-empty", "-m", "advance branch without head evidence")
		runDaemonWorkerGit(t, registered, "push", "origin", branch)
		expectedHead = ""
	case "ahead":
		runDaemonWorkerGit(t, fixWorktree, "commit", "--allow-empty", "-m", "newer local fix")
		wantBlocked = false
	case "detached":
		runDaemonWorkerGit(t, fixWorktree, "checkout", "--detach", oldHead)
	case "wrong-branch":
		runDaemonWorkerGit(t, fixWorktree, "switch", "-c", "wrong-branch")
	default:
		t.Fatalf("unknown fixture mode %q", mode)
	}
	if currentHead == "" {
		currentHead = strings.TrimSpace(runGitOutput(t, fixWorktree, "rev-parse", "HEAD"))
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", registered)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-1514", RepoFullName: "owner/repo", State: string(workflow.TaskChangesRequested),
		Branch: branch, WorktreePath: registered,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: branch, Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	jobID := "advance-fix-" + mode
	if mode == "missing-head" {
		enqueueAdvanceCreatedHeadlessFixJob(t, store, jobID, branch, fixWorktree)
	} else {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: jobID, Agent: "lead", Action: "implement", Repo: "owner/repo",
			Branch: branch, PullRequest: 1514, HeadSHA: expectedHead, TaskID: "task-1514",
			WorktreePath: fixWorktree, FixWorktree: true,
		})
	}
	if mode == "persistent-preflight-error" || mode == "transient-preflight-error" {
		seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: "z-healthy-after-preflight-error", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main",
		})
	}
	if mode == "persistent-preflight-error" {
		seeded, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob before stale-action seed: %v", err)
		}
		seededPayload, err := daemonJobPayload(seeded)
		if err != nil {
			t.Fatalf("daemonJobPayload before stale-action seed: %v", err)
		}
		seededPayload.BlockerSuggestedAction = "stale checkout remedy"
		encoded, err := json.Marshal(seededPayload)
		if err != nil {
			t.Fatalf("marshal stale-action seed: %v", err)
		}
		if err := store.UpdateJobPayload(ctx, jobID, string(encoded)); err != nil {
			t.Fatalf("UpdateJobPayload stale-action seed: %v", err)
		}
	}
	beforeRemote := strings.TrimSpace(runGitOutput(t, registered, "ls-remote", "origin", "refs/heads/"+branch))
	adapter := &cliWorkerFakeAdapter{output: resultJSON("failed")}
	healthyAdapter := &cliWorkerFakeAdapter{output: resultJSON("approved")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return fixWorktree, nil
	}
	worker.AdapterFactory = func(agent runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		if agent.Name == "audit" {
			return healthyAdapter, nil
		}
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine { return workflow.Engine{Store: store} }
	sink := &recordingSink{}
	worker.EventSinkOverride = sink
	worker.UsePool = mode == "persistent-preflight-error"
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob before run: %v", err)
	}
	var runErr error
	var tickErrors []error
	var attempts []int
	var retryAts []string
	var tickStarted []time.Time
	if mode == "persistent-preflight-error" || mode == "transient-preflight-error" {
		ticks := 3
		if mode == "transient-preflight-error" {
			ticks = 2
		}
		for tick := 0; tick < ticks; tick++ {
			tickStarted = append(tickStarted, time.Now().UTC())
			tickErrors = append(tickErrors, runQueuedJobsForRepo(ctx, worker, 1, "", ""))
			observed, err := store.GetJob(ctx, jobID)
			if err != nil {
				t.Fatalf("GetJob after tick %d: %v", tick+1, err)
			}
			observedPayload, err := daemonJobPayload(observed)
			if err != nil {
				t.Fatalf("daemonJobPayload after tick %d: %v", tick+1, err)
			}
			attempts = append(attempts, observedPayload.BlockerAttempts)
			retryAts = append(retryAts, observedPayload.BlockerRetryAt)
			if tick == 0 && mode == "transient-preflight-error" {
				restorePreflightFixture()
			}
			if observed.State == string(workflow.JobQueued) {
				expireImplementationPreflightHold(t, store, observed)
			}
		}
	} else {
		runErr = worker.run(ctx, job)
		if runErr != nil {
			t.Fatalf("worker.run returned error: %v", runErr)
		}
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob after run: %v", err)
	}
	var persistedEnvelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(after.Payload), &persistedEnvelope); err != nil {
		t.Fatalf("unmarshal persisted payload: %v", err)
	}
	_, blockerSuggestedActionPresent := persistedEnvelope["blocker_suggested_action"]
	healthyState := ""
	if mode == "persistent-preflight-error" || mode == "transient-preflight-error" {
		healthy, err := store.GetJob(ctx, "z-healthy-after-preflight-error")
		if err != nil {
			t.Fatalf("GetJob(z-healthy-after-preflight-error): %v", err)
		}
		healthyState = healthy.State
	}
	if mode == "persistent-preflight-error" {
		if adapter.calls != 0 {
			t.Fatalf("adapter calls = %d, want zero for infrastructure refusal", adapter.calls)
		}
		if healthyAdapter.calls != 1 {
			t.Fatalf("healthy adapter calls = %d, want one in same scheduler pass", healthyAdapter.calls)
		}
	} else if wantBlocked && mode != "transient-preflight-error" {
		if adapter.calls != 0 {
			t.Fatalf("adapter calls = %d, want zero: checkout preflight must run before the model", adapter.calls)
		}
		if after.State != string(workflow.JobBlocked) {
			t.Fatalf("job state = %q, want blocked", after.State)
		}
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload returned error: %v", err)
		}
		if payload.Result != nil {
			t.Fatalf("preflight-blocked payload result = %+v, want nil", payload.Result)
		}
	} else if mode != "transient-preflight-error" && adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want one: checkout includes the dispatch head", adapter.calls)
	}
	if got := strings.TrimSpace(runGitOutput(t, registered, "ls-remote", "origin", "refs/heads/"+branch)); got != beforeRemote {
		t.Fatalf("remote head changed from %q to %q despite preflight refusal", beforeRemote, got)
	}
	jobEvents, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	message := ""
	failureMessage := ""
	exhaustedEvents := 0
	deferredEvents := 0
	var retryEventKinds []string
	for _, event := range jobEvents {
		if event.Kind == string(workflow.JobBlocked) {
			message = event.Message
		}
		if event.Kind == string(workflow.JobFailed) {
			failureMessage = event.Message
		}
		if event.Kind == blockerExhaustedEventKind {
			exhaustedEvents++
		}
		if event.Kind == blockerDeferredEventKind {
			deferredEvents++
		}
		if event.Kind == blockerDeferredEventKind || event.Kind == blockerExhaustedEventKind || event.Kind == string(workflow.JobFailed) {
			retryEventKinds = append(retryEventKinds, event.Kind)
		}
	}
	remedyCleared := false
	if mode == "stale" || mode == "divergent" {
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", branch)
		runDaemonWorkerGit(t, fixWorktree, "reset", "--hard", expectedHead)
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload after remedy: %v", err)
		}
		if _, err := implementationFinalizationTargetFor(ctx, store, after, payload, implementationFinalizationBeforeRun); err != nil {
			t.Fatalf("documented reset remedy did not clear preflight: %v", err)
		}
		remedyCleared = true
	} else if mode == "unfetched-stale" {
		runDaemonWorkerGit(t, fixWorktree, "fetch", "origin", branch)
		runDaemonWorkerGit(t, fixWorktree, "reset", "--hard", expectedHead)
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload after fresh dispatch checkout: %v", err)
		}
		if _, err := implementationFinalizationTargetFor(ctx, store, after, payload, implementationFinalizationBeforeRun); err != nil {
			t.Fatalf("new dispatch with current object did not clear preflight: %v", err)
		}
		remedyCleared = true
	} else if mode == "missing-head" {
		payload, err := daemonJobPayload(after)
		if err != nil {
			t.Fatalf("daemonJobPayload after missing-head refusal: %v", err)
		}
		payload.HeadSHA = currentHead
		if _, err := implementationFinalizationTargetFor(ctx, store, after, payload, implementationFinalizationBeforeRun); err != nil {
			t.Fatalf("fresh dispatch head did not clear missing-head preflight: %v", err)
		}
		remedyCleared = true
	} else if mode == "persistent-preflight-error" {
		restorePreflightFixture()
		retried, err := workflow.RetryJob(ctx, store, job.ID)
		if err != nil {
			t.Fatalf("retry repaired exhausted job: %v", err)
		}
		retriedPayload, err := daemonJobPayload(retried)
		if err != nil {
			t.Fatalf("daemonJobPayload after retry: %v", err)
		}
		if _, err := implementationFinalizationTargetFor(ctx, store, retried, retriedPayload, implementationFinalizationBeforeRun); err != nil {
			t.Fatalf("repair plus job retry did not clear preflight: %v", err)
		}
		remedyCleared = true
	}
	return advanceImplementationPreflightResult{
		message: message, currentHead: currentHead, expectedHead: expectedHead, fixWorktree: fixWorktree,
		adapterCalls: adapter.calls, jobState: after.State, remedyCleared: remedyCleared,
		runErr: runErr, failureMessage: failureMessage, healthyState: healthyState,
		healthyCalls: healthyAdapter.calls, tickErrors: tickErrors, attempts: attempts,
		retryAts: retryAts, tickStarted: tickStarted, deliveredPrompts: append([]string(nil), adapter.prompts...),
		exhaustedEvents: exhaustedEvents, deferredEvents: deferredEvents,
		blockerSuggestedActionPresent: blockerSuggestedActionPresent,
		failedEmissions:               len(sink.byType(events.EventJobFailed)),
		retryEventKinds:               retryEventKinds,
	}
}

func countJobEventsByKind(events []db.JobEvent, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func breakGitBranchTip(t *testing.T, worktree, branch string) func() {
	t.Helper()
	refPath := filepath.Join(worktree, ".git", "refs", "heads", filepath.FromSlash(branch))
	original, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatalf("read branch tip: %v", err)
	}
	info, err := os.Stat(refPath)
	if err != nil {
		t.Fatalf("stat branch tip: %v", err)
	}
	if err := os.WriteFile(refPath, []byte(strings.Repeat("f", 40)+"\n"), info.Mode().Perm()); err != nil {
		t.Fatalf("install broken branch tip: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := os.WriteFile(refPath, original, info.Mode().Perm()); err != nil {
			t.Fatalf("restore branch tip: %v", err)
		}
	}
	t.Cleanup(restore)
	return restore
}

func makePackedCurrentCommitInaccessible(t *testing.T, worktree, dispatchHead string) (string, func()) {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "commit", dispatchHead)
	cmd.Dir = worktree
	commit, err := cmd.Output()
	if err != nil {
		t.Fatalf("read dispatch commit before packing: %v", err)
	}
	runDaemonWorkerGit(t, worktree, "commit", "--allow-empty", "-m", "packed current head")
	currentHead := strings.TrimSpace(runGitOutput(t, worktree, "rev-parse", "HEAD"))
	runDaemonWorkerGit(t, worktree, "gc", "--aggressive", "--prune=now")
	packDir := filepath.Join(worktree, ".git", "objects", "pack")
	savedPackDir := packDir + ".preflight-fixture"
	if err := os.Rename(packDir, savedPackDir); err != nil {
		t.Fatalf("hide packed objects while rehydrating dispatch commit: %v", err)
	}
	if err := os.Mkdir(packDir, 0o755); err != nil {
		t.Fatalf("create temporary pack directory: %v", err)
	}
	commitFile := filepath.Join(t.TempDir(), "dispatch.commit")
	if err := os.WriteFile(commitFile, commit, 0o644); err != nil {
		t.Fatalf("write dispatch commit fixture: %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, worktree, "hash-object", "-t", "commit", "-w", commitFile)); got != dispatchHead {
		t.Fatalf("rehydrated dispatch commit = %s, want %s", got, dispatchHead)
	}
	if err := os.Remove(packDir); err != nil {
		t.Fatalf("remove temporary pack directory: %v", err)
	}
	if err := os.Rename(savedPackDir, packDir); err != nil {
		t.Fatalf("restore packed objects: %v", err)
	}
	packs, err := filepath.Glob(filepath.Join(packDir, "*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("packed current-head fixture = %v, %v", packs, err)
	}
	modes := make([]os.FileMode, len(packs))
	for i, pack := range packs {
		info, err := os.Stat(pack)
		if err != nil {
			t.Fatalf("stat pack %s: %v", pack, err)
		}
		modes[i] = info.Mode().Perm()
		if err := os.Chmod(pack, 0); err != nil {
			t.Fatalf("make pack inaccessible: %v", err)
		}
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		for i, pack := range packs {
			if err := os.Chmod(pack, modes[i]); err != nil {
				t.Fatalf("restore pack access: %v", err)
			}
		}
	}
	t.Cleanup(restore)
	return currentHead, restore
}

func expireImplementationPreflightHold(t *testing.T, store *db.Store, job db.Job) {
	t.Helper()
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload before next simulated tick: %v", err)
	}
	payload.BlockerRetryAt = "2000-01-01T00:00:00Z"
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload before next simulated tick: %v", err)
	}
	if err := store.UpdateJobPayload(context.Background(), job.ID, string(encoded)); err != nil {
		t.Fatalf("expire preflight retry hold before next simulated tick: %v", err)
	}
}

func enqueueAdvanceCreatedHeadlessFixJob(t *testing.T, store *db.Store, jobID, branch, fixWorktree string) {
	t.Helper()
	ctx := context.Background()
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "original-headless-implement", Agent: "lead", Action: "implement", Repo: "owner/repo",
		Branch: branch, PullRequest: 1514, TaskID: "task-1514",
	})
	if err := store.UpdateJobState(ctx, "original-headless-implement", string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("UpdateJobState(original-headless-implement): %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "headless-review", Agent: "audit", Action: "review", Repo: "owner/repo",
		Branch: branch, PullRequest: 1514, TaskID: "task-1514", LeadAgent: "lead",
	})
	review, err := store.GetJob(ctx, "headless-review")
	if err != nil {
		t.Fatalf("GetJob(headless-review): %v", err)
	}
	payload, err := daemonJobPayload(review)
	if err != nil {
		t.Fatalf("daemonJobPayload(headless-review): %v", err)
	}
	payload.Result = &workflow.AgentResult{Decision: "changes_requested", Summary: "fix the headless review"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal headless review payload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, review.ID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload(headless-review): %v", err)
	}
	if err := store.UpdateJobState(ctx, review.ID, string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("UpdateJobState(headless-review): %v", err)
	}
	engine := workflow.Engine{
		Store: store,
		JobID: func(workflow.JobRequest) string { return jobID },
		RequireWorkflowPolicy: func(string) workflow.RequireWorkflowPolicy {
			return workflow.RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
		},
		FixWorktreeAllocator: func(context.Context, workflow.FixWorktreeRequest) (workflow.FixWorktreeAllocation, error) {
			return workflow.FixWorktreeAllocation{Path: fixWorktree}, nil
		},
	}
	if err := engine.AdvanceJob(ctx, review.ID); err != nil {
		t.Fatalf("AdvanceJob(headless-review): %v", err)
	}
	fix, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	fixPayload, err := daemonJobPayload(fix)
	if err != nil {
		t.Fatalf("daemonJobPayload(%s): %v", jobID, err)
	}
	if !fixPayload.FixWorktree || fixPayload.WorktreePath != fixWorktree || strings.TrimSpace(fixPayload.HeadSHA) != "" {
		t.Fatalf("advance-created fix payload = %+v, want headless dedicated fix worktree", fixPayload)
	}
}

func TestOrdinaryImplementationSkipsAdvanceFinalizationPreflight(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "main", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "ordinary-implement", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "main",
	})
	adapter := &cliWorkerFakeAdapter{output: resultJSON("failed")}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine { return workflow.Engine{Store: store} }
	job, err := store.GetJob(ctx, "ordinary-implement")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want one: ordinary implement is not an advance finalization candidate", adapter.calls)
	}
}

func TestDaemonImplementationFinalizerKeepsMissingBranchBackstop(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-backstop", RepoFullName: "owner/repo", WorktreePath: t.TempDir()}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	payload := workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 12, TaskID: "task-backstop",
		FixWorktree: true, WorktreePath: t.TempDir(), Result: &workflow.AgentResult{Decision: "implemented"},
	}
	_, err := (newHostDaemonImplementationFinalizer(store, github.NoopClient{})).FinalizeImplementation(ctx, db.Job{ID: "late-backstop", Agent: "lead", Type: "implement"}, payload)
	var blocked workflow.BlockedError
	if !errors.As(err, &blocked) || !blocked.ResultDeliveryFailed || !strings.Contains(err.Error(), "carries no payload branch") {
		t.Fatalf("FinalizeImplementation error = %v, want delivery-blocked missing-branch backstop", err)
	}
}

func TestImplementationFinalizationTargetAcceptsCompleteTarget(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := createDaemonWorkerGitCheckout(t, "feature/ok")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-ok", RepoFullName: "owner/repo", Branch: "feature/ok", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	head := strings.TrimSpace(runGitOutput(t, worktree, "rev-parse", "HEAD"))
	target, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "advance-ok", Agent: "lead", Type: "implement"}, workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/ok", HeadSHA: head, TaskID: "task-ok", FixWorktree: true, WorktreePath: worktree,
	}, implementationFinalizationBeforeRun)
	if err != nil {
		t.Fatalf("implementationFinalizationTargetFor returned error: %v", err)
	}
	if target.Task.ID != "task-ok" || target.WorktreePath != worktree {
		t.Fatalf("target = %+v, want task-ok and fix worktree", target)
	}
	if got := strings.TrimSpace(runGitOutput(t, worktree, "rev-parse", "HEAD")); got != head {
		t.Fatalf("complete-target fixture HEAD = %s, want exact dispatch head %s", got, head)
	}
}

func TestImplementationFinalizationTargetClassifiesBranchLookupByPhase(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-branch-error", RepoFullName: "owner/repo", Branch: "feature/fix", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "feature/fix", TaskID: "task-branch-error", FixWorktree: true, WorktreePath: worktree}
	job := db.Job{ID: "advance-branch-error", Agent: "lead", Type: "implement"}

	for _, test := range []struct {
		name        string
		phase       implementationFinalizationPhase
		wantBlocked bool
	}{
		{name: "before run blocks", phase: implementationFinalizationBeforeRun, wantBlocked: true},
		{name: "after run retries", phase: implementationFinalizationAfterRun, wantBlocked: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := implementationFinalizationTargetFor(ctx, store, job, payload, test.phase)
			if err == nil {
				t.Fatal("implementationFinalizationTargetFor returned nil error")
			}
			var blocked workflow.BlockedError
			if got := errors.As(err, &blocked); got != test.wantBlocked {
				t.Fatalf("BlockedError = %t, want %t: %v", got, test.wantBlocked, err)
			}
			if !test.wantBlocked && !strings.Contains(err.Error(), "resolve implementation branch") {
				t.Fatalf("after-run error = %q, want retryable branch-resolution context", err)
			}
		})
	}
}

func TestImplementationFinalizationTargetRejectsUnsetPhase(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	_, err := implementationFinalizationTargetFor(ctx, store, db.Job{ID: "advance-unset"}, workflow.JobPayload{}, implementationFinalizationPhaseUnset)
	if err == nil || !strings.Contains(err.Error(), "finalization phase 0 is invalid") {
		t.Fatalf("implementationFinalizationTargetFor error = %v, want invalid zero phase", err)
	}
}
