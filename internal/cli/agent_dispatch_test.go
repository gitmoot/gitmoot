package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// buildLocalAgentJobOutput must render a terminally-succeeded implement job into
// the same populated output the success path returns, so the advance-error
// recovery branch can surface the persisted result instead of discarding it.
func TestBuildLocalAgentJobOutputRendersSucceededJob(t *testing.T) {
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 7,
		Result:      &workflow.AgentResult{Decision: "implemented", Summary: "opened PR"},
		RawOutputs:  []string{`{"gitmoot_result":{}}`},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	job := db.Job{
		ID:      "local-implement-lead-abc",
		Agent:   "lead",
		Type:    "implement",
		State:   string(workflow.JobSucceeded),
		Payload: string(payload),
	}
	out, err := buildLocalAgentJobOutput(job, localAgentDispatchRequest{
		SelectedAction:       "implement",
		SelectedActionReason: "explicit agent implement",
		ExecutionPath:        "agent_implement",
	})
	if err != nil {
		t.Fatalf("buildLocalAgentJobOutput returned error: %v", err)
	}
	if out.JobID != job.ID || out.State != string(workflow.JobSucceeded) || out.Repo != "owner/repo" {
		t.Fatalf("output = %+v", out)
	}
	if out.Result == nil || out.Result.Summary != "opened PR" || out.RawOutputCount != 1 {
		t.Fatalf("output result = %+v (raw=%d)", out.Result, out.RawOutputCount)
	}
	if out.AdvanceError != "" {
		t.Fatalf("AdvanceError = %q, want empty by default", out.AdvanceError)
	}
}

// The terminal-success output, when an advance error is attached, must serialize
// the result AND the advance_error in --json mode, and render an advance_error
// line in human mode. This is the #387 surface: exit 0 with the result on
// stdout and the advance warning carried alongside.
func TestLocalAgentJobOutputSurfacesAdvanceError(t *testing.T) {
	out := localAgentJobOutput{
		JobID:        "local-implement-lead-abc",
		State:        string(workflow.JobSucceeded),
		Repo:         "owner/repo",
		Agent:        "lead",
		Action:       "implement",
		Result:       &workflow.AgentResult{Decision: "implemented", Summary: "opened PR"},
		AdvanceError: "workflow advance failed: workflow blocked: ci is pending",
	}

	t.Run("json includes advance_error and result", func(t *testing.T) {
		var buf bytes.Buffer
		if err := writeJSON(&buf, out); err != nil {
			t.Fatalf("writeJSON returned error: %v", err)
		}
		var decoded localAgentJobOutput
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatalf("decode: %v\n%s", err, buf.String())
		}
		if decoded.AdvanceError != out.AdvanceError {
			t.Fatalf("decoded advance_error = %q", decoded.AdvanceError)
		}
		if decoded.Result == nil || decoded.Result.Summary != "opened PR" {
			t.Fatalf("decoded result = %+v", decoded.Result)
		}
		if !strings.Contains(buf.String(), `"advance_error"`) {
			t.Fatalf("json missing advance_error key:\n%s", buf.String())
		}
	})

	t.Run("human mode prints advance_error line", func(t *testing.T) {
		var buf bytes.Buffer
		printLocalAgentJobOutput(&buf, out)
		if !strings.Contains(buf.String(), "advance_error: workflow advance failed: workflow blocked: ci is pending") {
			t.Fatalf("human output missing advance_error line:\n%s", buf.String())
		}
		// The result still renders.
		if !strings.Contains(buf.String(), "summary: opened PR") {
			t.Fatalf("human output missing result summary:\n%s", buf.String())
		}
	})
}

// By default (no advance error) the JSON output must NOT carry an advance_error
// key — the field is omitempty, so the normal success path stays byte-identical.
func TestLocalAgentJobOutputOmitsAdvanceErrorByDefault(t *testing.T) {
	out := localAgentJobOutput{
		JobID:  "local-implement-lead-abc",
		State:  string(workflow.JobSucceeded),
		Repo:   "owner/repo",
		Agent:  "lead",
		Action: "implement",
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "opened PR"},
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, out); err != nil {
		t.Fatalf("writeJSON returned error: %v", err)
	}
	if strings.Contains(buf.String(), "advance_error") {
		t.Fatalf("default json should omit advance_error:\n%s", buf.String())
	}
	var human bytes.Buffer
	printLocalAgentJobOutput(&human, out)
	if strings.Contains(human.String(), "advance_error") {
		t.Fatalf("default human output should omit advance_error:\n%s", human.String())
	}
}

// recoverAdvanceErrorOutput is the post-success advance-recovery glue extracted
// from dispatchLocalAgentJob: it recovers the persisted result ONLY when the run
// error is a workflow.AdvanceError AND the re-fetched job is terminally
// succeeded. It seeds a real db.Store job (mirroring the success-path payload)
// and asserts the three branches.
func TestRecoverAdvanceErrorOutput(t *testing.T) {
	ctx := context.Background()
	request := localAgentDispatchRequest{
		SelectedAction:       "implement",
		SelectedActionReason: "explicit agent implement",
		ExecutionPath:        "agent_implement",
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 7,
		Result:      &workflow.AgentResult{Decision: "implemented", Summary: "opened PR"},
		RawOutputs:  []string{`{"gitmoot_result":{}}`},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	seedJob := func(t *testing.T, store *db.Store, id string, state string) {
		t.Helper()
		if err := store.CreateJob(ctx, db.Job{
			ID:      id,
			Agent:   "lead",
			Type:    "implement",
			State:   state,
			Payload: string(payload),
		}); err != nil {
			t.Fatalf("CreateJob returned error: %v", err)
		}
	}

	t.Run("succeeded job + AdvanceError recovers with result and warning", func(t *testing.T) {
		store := daemonWorkerStore(t)
		seedJob(t, store, "local-implement-lead-ok", string(workflow.JobSucceeded))
		runErr := workflow.AdvanceError{Err: errors.New("workflow blocked: ci is pending")}

		out, recovered, err := recoverAdvanceErrorOutput(ctx, store, "local-implement-lead-ok", request, runErr)
		if err != nil {
			t.Fatalf("recoverAdvanceErrorOutput returned error: %v", err)
		}
		if !recovered {
			t.Fatal("recovered = false, want true for a succeeded job with an AdvanceError")
		}
		if out.Result == nil || out.Result.Summary != "opened PR" {
			t.Fatalf("output result = %+v", out.Result)
		}
		if out.AdvanceError == "" {
			t.Fatalf("AdvanceError = %q, want the advance warning attached", out.AdvanceError)
		}
		if out.AdvanceError != runErr.Error() {
			t.Fatalf("AdvanceError = %q, want %q", out.AdvanceError, runErr.Error())
		}
	})

	t.Run("plain (non-AdvanceError) error does not recover", func(t *testing.T) {
		store := daemonWorkerStore(t)
		seedJob(t, store, "local-implement-lead-plain", string(workflow.JobSucceeded))

		out, recovered, err := recoverAdvanceErrorOutput(ctx, store, "local-implement-lead-plain", request, errors.New("delivery failed"))
		if err != nil {
			t.Fatalf("recoverAdvanceErrorOutput returned error: %v", err)
		}
		if recovered {
			t.Fatalf("recovered = true, want false for a non-AdvanceError; out=%+v", out)
		}
	})

	t.Run("AdvanceError but job not succeeded does not recover", func(t *testing.T) {
		store := daemonWorkerStore(t)
		seedJob(t, store, "local-implement-lead-blocked", string(workflow.JobBlocked))
		runErr := workflow.AdvanceError{Err: errors.New("workflow blocked: ci is pending")}

		out, recovered, err := recoverAdvanceErrorOutput(ctx, store, "local-implement-lead-blocked", request, runErr)
		if err != nil {
			t.Fatalf("recoverAdvanceErrorOutput returned error: %v", err)
		}
		if recovered {
			t.Fatalf("recovered = true, want false when the re-fetched job is not succeeded; out=%+v", out)
		}
	})
}

type reviewLoopPullRequestClient struct {
	github.NoopClient
	pr github.PullRequest
}

func (c reviewLoopPullRequestClient) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	return c.pr, nil
}

type cliReviewLoopFixture struct {
	store    *db.Store
	home     string
	checkout string
	record   db.Repo
	repo     github.Repository
}

func newCLIReviewLoopFixture(t *testing.T) cliReviewLoopFixture {
	t.Helper()
	store, home := blockerE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "printf ok",
		[]string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	return cliReviewLoopFixture{
		store: store, home: home, checkout: checkout,
		record: db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout, DefaultBranch: "main", PollInterval: "30s"},
		repo:   github.Repository{Owner: "owner", Name: "repo"},
	}
}

func seedCLIReviewLoopVerdict(t *testing.T, store *db.Store, id, head, decision string) {
	t.Helper()
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", Branch: "main", PullRequest: 227, HeadSHA: head,
		TaskID: "review-pr-227", ReviewRound: "review-1",
		Result: &workflow.AgentResult{Decision: decision, Summary: "historical evidence"},
	})
	if err != nil {
		t.Fatalf("Marshal verdict: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: id, Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded), Payload: string(payload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: decision}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", id, err)
	}
}

func assertCLIReviewLoopHardRefusal(t *testing.T, fixture cliReviewLoopFixture, request localAgentDispatchRequest, priorJobID string) {
	t.Helper()
	adapterCalls := 0
	previousFactory := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, string, string) (runtime.Adapter, error) {
		adapterCalls++
		return nil, errors.New("adapter must not be selected for a refused review loop")
	}
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousFactory })

	_, err := dispatchLocalAgentJob(context.Background(), fixture.store, request)
	if err == nil || !strings.Contains(err.Error(), "review loop detected") || !strings.Contains(err.Error(), priorJobID) {
		t.Fatalf("dispatch error = %v, want actionable review-loop refusal naming %s", err, priorJobID)
	}
	jobs, listErr := fixture.store.ListJobs(context.Background())
	if listErr != nil {
		t.Fatalf("ListJobs: %v", listErr)
	}
	if len(jobs) != 1 || jobs[0].ID != priorJobID {
		t.Fatalf("jobs = %+v, want only the prior succeeded review", jobs)
	}
	tasks, listErr := fixture.store.ListTasks(context.Background())
	if listErr != nil {
		t.Fatalf("ListTasks: %v", listErr)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks = %+v, want none before review-loop refusal", tasks)
	}
	worktreeRoot := filepath.Join(config.PathsForHome(fixture.home).Home, "worktrees")
	if _, statErr := os.Stat(worktreeRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree root %q was created before refusal: %v", worktreeRoot, statErr)
	}
	if adapterCalls != 0 {
		t.Fatalf("runtime adapter selected %d times, want zero", adapterCalls)
	}
}

// TestCLIReviewLoopRefusesBothHeadResolutionBranches kills a detector wired
// only into one prepareLocalReviewDispatchRequest branch, a post-worktree guard,
// and engine-only coverage. Both production CLI shapes hard-error with no new
// job, task, worktree, or runtime call.
func TestCLIReviewLoopRefusesBothHeadResolutionBranches(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolved bool
	}{
		{name: "caller supplied branch and head"},
		{name: "GitHub resolved branch and head", resolved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCLIReviewLoopFixture(t)
			seedCLIReviewLoopVerdict(t, fixture.store, "prior-review", "same-head", "changes_requested")
			request := localAgentDispatchRequest{
				RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 227,
				Instructions: "Review unchanged head.", Home: fixture.home,
			}
			if tc.resolved {
				previous := newAgentDispatchGitHubClient
				newAgentDispatchGitHubClient = func(string) github.Client {
					return reviewLoopPullRequestClient{pr: github.PullRequest{HeadRef: "main", HeadSHA: "same-head"}}
				}
				t.Cleanup(func() { newAgentDispatchGitHubClient = previous })
			} else {
				request.Branch = "main"
				request.HeadSHA = "same-head"
			}
			assertCLIReviewLoopHardRefusal(t, fixture, request, "prior-review")
			if got := countCLIJobEvents(t, fixture.store, "prior-review", workflow.ReviewLoopDetectedEventKind); got != 1 {
				t.Fatalf("review_loop_detected events = %d, want one", got)
			}
		})
	}
}

// TestCLIReviewLoopAllowsNewHeadAndMixedDecisions kills repo/PR-only and
// decision-blind suppression mutants at the CLI preparation seam.
func TestCLIReviewLoopAllowsNewHeadAndMixedDecisions(t *testing.T) {
	t.Run("new head", func(t *testing.T) {
		fixture := newCLIReviewLoopFixture(t)
		seedCLIReviewLoopVerdict(t, fixture.store, "prior-review", "old-head", "changes_requested")
		head := strings.TrimSpace(runGitOutput(t, fixture.checkout, "rev-parse", "HEAD"))
		_, checkout, err := prepareLocalReviewDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			PullRequest: 227, Branch: "main", HeadSHA: head, Home: fixture.home,
		})
		if err != nil {
			t.Fatalf("new-head prepare: %v", err)
		}
		if strings.TrimSpace(checkout) == "" {
			t.Fatal("new-head prepare did not create/reuse a review worktree")
		}
	})

	t.Run("mixed decisions at same head", func(t *testing.T) {
		fixture := newCLIReviewLoopFixture(t)
		head := strings.TrimSpace(runGitOutput(t, fixture.checkout, "rev-parse", "HEAD"))
		seedCLIReviewLoopVerdict(t, fixture.store, "prior-approved", head, "approved")
		seedCLIReviewLoopVerdict(t, fixture.store, "prior-changes", head, "changes_requested")
		_, checkout, err := prepareLocalReviewDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			PullRequest: 227, Branch: "main", HeadSHA: head, Home: fixture.home,
		})
		if err != nil {
			t.Fatalf("mixed-decision prepare: %v", err)
		}
		if strings.TrimSpace(checkout) == "" {
			t.Fatal("mixed-decision prepare did not create/reuse a review worktree")
		}
	})
}

// TestCLIReviewLoopHerdres227Shape reproduces the 319-attempt incident shape:
// one succeeded changes_requested review at one head followed by 318 identical
// local admissions. It kills reliance on nanosecond local job IDs, missing job-
// count enforcement, and non-idempotent event emission.
func TestCLIReviewLoopHerdres227Shape(t *testing.T) {
	fixture := newCLIReviewLoopFixture(t)
	seedCLIReviewLoopVerdict(t, fixture.store, "herdres-227-first", "2da08", "changes_requested")
	request := localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 227,
		Branch: "main", HeadSHA: "2da08", Instructions: "Review unchanged head.", Home: fixture.home,
	}
	for attempt := 2; attempt <= 319; attempt++ {
		if _, err := dispatchLocalAgentJob(context.Background(), fixture.store, request); err == nil || !strings.Contains(err.Error(), "review loop detected") {
			t.Fatalf("attempt %d error = %v, want review-loop refusal", attempt, err)
		}
	}
	jobs, err := fixture.store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "herdres-227-first" {
		t.Fatalf("jobs after 319 attempted admissions = %+v, want only the first review", jobs)
	}
	if got := countCLIJobEvents(t, fixture.store, "herdres-227-first", workflow.ReviewLoopDetectedEventKind); got != 1 {
		t.Fatalf("review_loop_detected events = %d, want one after 318 refusals", got)
	}
}
