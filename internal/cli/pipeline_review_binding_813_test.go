package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const pipelineSourceReviewSpec = `name: source-review
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: review
    agent: reviewer
    prompt: Review the implementation PR.
    action: review
    source: impl
    needs: [impl]
    success_decisions: [approved]
`

func settleBoundImplementStageJob(t *testing.T, store *db.Store, jobID, decision string, binding pipeline.PipelineStagePRBinding) {
	t.Helper()
	ctx := context.Background()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	payload.PullRequest = binding.PullRequest
	payload.HeadSHA = binding.HeadSHA
	payload.Branch = binding.Branch
	payload.TaskID = binding.TaskID
	payload.LeadAgent = binding.LeadAgent
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: "implementation settled"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	to := jobStateForDecision(decision)
	ok, err := store.TransitionJobStatePayloadWithEvent(ctx, job.ID, job.State, to, string(encoded), db.JobEvent{JobID: job.ID, Kind: to, Message: "settled by test"})
	if err != nil || !ok {
		t.Fatalf("settle implement job: ok=%v err=%v", ok, err)
	}
}

func TestPipelineSourceReviewBindingAndFanoutSuppression(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	rec, spec := newTestPipeline(t, store, "source-review", pipelineSourceReviewSpec)
	baseEnqueue := testStageEnqueuer(store)
	requests := make([]workflow.JobRequest, 0, 2)
	enqueue := func(ctx context.Context, request workflow.JobRequest) (db.Job, error) {
		requests = append(requests, request)
		return baseEnqueue(ctx, request)
	}
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)

	impl := stageRow(t, store, run.ID, "impl")
	implJob, err := store.GetJob(ctx, impl.JobID)
	if err != nil {
		t.Fatalf("GetJob(impl): %v", err)
	}
	implPayload, err := workflow.ParseJobPayload(implJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(impl): %v", err)
	}
	if !implPayload.SkipNativeReviewFanout {
		t.Fatal("implement stage with downstream source-bound review did not suppress native review fan-out")
	}

	want := pipeline.PipelineStagePRBinding{PullRequest: 813, HeadSHA: "head-813", Branch: "feat/813", TaskID: "task-813", LeadAgent: "coder"}
	settleBoundImplementStageJob(t, store, impl.JobID, "implemented", want)
	run = advance(t, store, rec, spec, enqueue, run, now.Add(time.Second))

	review := stageRow(t, store, run.ID, "review")
	if review.State != pipeline.StageQueued || review.JobID == "" {
		t.Fatalf("review stage = %+v, want queued with bound job", review)
	}
	reviewJob, err := store.GetJob(ctx, review.JobID)
	if err != nil {
		t.Fatalf("GetJob(review): %v", err)
	}
	payload, err := workflow.ParseJobPayload(reviewJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(review): %v", err)
	}
	if payload.PullRequest != want.PullRequest || payload.HeadSHA != want.HeadSHA || payload.Branch != want.Branch || payload.TaskID != want.TaskID || payload.LeadAgent != want.LeadAgent {
		t.Fatalf("review binding = %+v, want %+v", payload, want)
	}
	if payload.ReviewRound != "" {
		t.Fatalf("review round = %q, want empty for pipeline review", payload.ReviewRound)
	}

	// A second unchanged scan must neither enqueue again nor derive a different
	// binding: the source job payload and deterministic stage job id are immutable.
	run = advance(t, store, rec, spec, enqueue, run, now.Add(2*time.Second))
	if len(requests) != 2 {
		t.Fatalf("enqueue request count after rescan = %d, want 2 (impl + review)", len(requests))
	}
	if got := requests[1]; got.PullRequest != want.PullRequest || got.HeadSHA != want.HeadSHA || got.Branch != want.Branch || got.TaskID != want.TaskID {
		t.Fatalf("captured review request changed across scan: %+v", got)
	}
}

func TestPipelineImplementWithoutSourceReviewKeepsNativeFanout(t *testing.T) {
	const specYAML = `name: native-review
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix it.
    action: implement
    write: true
  - id: review
    agent: reviewer
    prompt: General review.
    action: review
    needs: [impl]
`
	store := pipelineAdvanceStore(t)
	rec, spec := newTestPipeline(t, store, "native-review", specYAML)
	run := startTestRun(t, store, rec, spec, testStageEnqueuer(store), time.Now().UTC())
	job, err := store.GetJob(context.Background(), stageRow(t, store, run.ID, "impl").JobID)
	if err != nil {
		t.Fatalf("GetJob(impl): %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(impl): %v", err)
	}
	if payload.SkipNativeReviewFanout {
		t.Fatal("unbound downstream review unexpectedly suppressed native review fan-out")
	}
}

func TestPipelineSourceReviewUsesSucceededRetryPayload(t *testing.T) {
	const specYAML = `name: source-review-retry
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
    retry: 1
  - id: review
    agent: reviewer
    prompt: Review it.
    action: review
    source: impl
    needs: [impl]
`
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	rec, spec := newTestPipeline(t, store, "source-review-retry", specYAML)
	enqueue := testStageEnqueuer(store)
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)
	first := stageRow(t, store, run.ID, "impl")
	settleStageJob(t, store, first.JobID, "failed", "first attempt failed", nil)
	run = advance(t, store, rec, spec, enqueue, run, now.Add(time.Second))
	second := stageRow(t, store, run.ID, "impl")
	if second.Attempt != 1 || second.JobID == first.JobID {
		t.Fatalf("retry row = %+v, first=%+v", second, first)
	}
	want := pipeline.PipelineStagePRBinding{PullRequest: 814, HeadSHA: "retry-head", Branch: "feat/retry", TaskID: "task-retry", LeadAgent: "coder"}
	settleBoundImplementStageJob(t, store, second.JobID, "implemented", want)
	run = advance(t, store, rec, spec, enqueue, run, now.Add(2*time.Second))
	reviewJob, err := store.GetJob(ctx, stageRow(t, store, run.ID, "review").JobID)
	if err != nil {
		t.Fatalf("GetJob(review): %v", err)
	}
	payload, err := workflow.ParseJobPayload(reviewJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(review): %v", err)
	}
	if payload.PullRequest != want.PullRequest || payload.HeadSHA != want.HeadSHA {
		t.Fatalf("review payload = %+v, want succeeded retry binding %+v", payload, want)
	}
}

func TestPipelineSourceReviewNoPRParksBlocked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
		addEvent bool
	}{
		{name: "terminal no-op", decision: "implemented", addEvent: true},
		{name: "approved without PR", decision: "approved"},
		{name: "skipped no work", decision: "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := pipelineAdvanceStore(t)
			rec, spec := newTestPipeline(t, store, "source-review", pipelineSourceReviewSpec)
			enqueue := testStageEnqueuer(store)
			now := time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC)
			run := startTestRun(t, store, rec, spec, enqueue, now)
			impl := stageRow(t, store, run.ID, "impl")
			settleBoundImplementStageJob(t, store, impl.JobID, tc.decision, pipeline.PipelineStagePRBinding{})
			if tc.addEvent {
				if err := store.AddJobEvent(ctx, db.JobEvent{JobID: impl.JobID, Kind: "advance_skipped_no_pr", Message: "no PR"}); err != nil {
					t.Fatalf("AddJobEvent: %v", err)
				}
			}
			run = advance(t, store, rec, spec, enqueue, run, now.Add(time.Second))
			review := stageRow(t, store, run.ID, "review")
			if review.State != pipeline.StageBlocked || review.JobID != "" {
				t.Fatalf("review stage = %+v, want blocked without a job", review)
			}
			if !strings.Contains(review.Summary, "produced no PR") || !strings.Contains(review.NeedsJSON, "source stage produced no PR; nothing to review") {
				t.Fatalf("review block detail = summary %q needs %q", review.Summary, review.NeedsJSON)
			}
			if run.State != pipeline.RunBlocked || run.HaltStage != "review" {
				t.Fatalf("run = %+v, want blocked at review", run)
			}
		})
	}
}

func TestPipelineSourceReviewWorktreePinnedToBoundHead(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "coder", runtime.ShellRuntime, pipelineStageResultCmd("implemented", "fixed", nil), []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, pipelineStageResultCmd("approved", "good", nil), []string{"review"}, "owner/repo")

	// Create a PR-head commit that is resolvable in the shared object database, then
	// return the registered checkout to main. The review must detach at this SHA,
	// never at the registered checkout's current main tip.
	runDaemonWorkerGit(t, checkout, "checkout", "-b", "feat-813")
	if err := os.WriteFile(filepath.Join(checkout, "feature.txt"), []byte("review me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile feature: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "feature.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "feature head")
	head, err := (gitutil.NewHostClient(checkout)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA(feature): %v", err)
	}
	runDaemonWorkerGit(t, checkout, "checkout", "main")
	mainHead, err := (gitutil.NewHostClient(checkout)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA(main): %v", err)
	}
	if head == mainHead {
		t.Fatal("test setup did not create a distinct PR head")
	}

	rec, spec := newTestPipeline(t, store, "source-review", pipelineSourceReviewSpec)
	enqueue := newPipelineStageEnqueuer(store, home)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")
	settleBoundImplementStageJob(t, store, impl.JobID, "implemented", pipeline.PipelineStagePRBinding{PullRequest: 813, HeadSHA: head, Branch: "feat-813", TaskID: "task-813", LeadAgent: "coder"})
	run = advance(t, store, rec, spec, enqueue, run, now.Add(time.Second))
	reviewJob, err := store.GetJob(ctx, stageRow(t, store, run.ID, "review").JobID)
	if err != nil {
		t.Fatalf("GetJob(review): %v", err)
	}
	payload, err := workflow.ParseJobPayload(reviewJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(review): %v", err)
	}
	if !payload.ReadOnlyWorktree || strings.TrimSpace(payload.WorktreePath) == "" {
		t.Fatalf("review worktree payload = %+v, want detached read-only worktree", payload)
	}
	gotHead, err := (gitutil.NewHostClient(payload.WorktreePath)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA(review worktree): %v", err)
	}
	if gotHead != head {
		t.Fatalf("review checkout HEAD = %s, want bound PR head %s (main=%s)", gotHead, head, mainHead)
	}
	worker := jobWorker{Store: store}
	gotCheckout, err := worker.defaultCheckoutForRunner(ctx, reviewJob, payload, runtime.Agent{Name: "reviewer"}, subprocess.ExecRunner{})
	if err != nil {
		t.Fatalf("defaultCheckoutForRunner rejected pinned review worktree: %v", err)
	}
	if gotCheckout != payload.WorktreePath {
		t.Fatalf("defaultCheckoutForRunner = %q, want %q", gotCheckout, payload.WorktreePath)
	}
}

func jobEventKindPresent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}
