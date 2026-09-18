package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestOrgRoleReviewCLIQueuesForDaemonOwnership(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`
[org]
enforce = "warn"
[org.roles."owner"]
scope = ["*"]
[org.roles."joltra"]
parent = "owner"
scope = ["owner/repo"]
`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "run-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	adapter := installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"looks good","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	args := []string{
		"reviewer", "Review this exact head.", "--repo", "owner/repo", "--pr", "12",
		"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
		"--org-role", "joltra", "--home", home,
	}
	var stdout, stderr bytes.Buffer
	code := runAgentReview(args, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("review exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != string(workflow.JobQueued) {
		t.Fatalf("jobs = %+v, want one daemon-owned queued review", jobs)
	}
	// #2196: `agent review` routes THROUGH `review request` now, so the surface
	// is the router's. The property defended is unchanged and asserted on the job
	// row above: a daemon-owned QUEUED review, not an in-process run. The old
	// strings pinned the previous command's wording rather than the behaviour.
	for _, want := range []string{"(queued)", "watch: gitmoot job watch"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	}
	if adapter.calls != 0 {
		t.Fatalf("background review invoked runtime %d times, want zero", adapter.calls)
	}

	runArgs := append([]string{}, args...)
	runArgs[0] = "run-reviewer"
	runArgs = append(runArgs, "--action", "review")
	stdout.Reset()
	stderr.Reset()
	code = runAgentRun(runArgs, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent run review exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	jobs, err = store.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var runJob *db.Job
	for index := range jobs {
		if jobs[index].Agent == "run-reviewer" {
			runJob = &jobs[index]
			break
		}
	}
	if runJob == nil || runJob.Type != "review" || runJob.State != string(workflow.JobQueued) {
		t.Fatalf("agent run review job = %+v, want daemon-owned queued review", runJob)
	}
	runPayload, err := daemonJobPayload(*runJob)
	if err != nil {
		t.Fatal(err)
	}
	if runPayload.ActingOrgRole != "joltra" {
		t.Fatalf("agent run review acting role = %q, want joltra", runPayload.ActingOrgRole)
	}
	for _, want := range []string{"state: queued", "next: gitmoot job watch"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("agent run stdout = %q, want %q", stdout.String(), want)
		}
	}
	if adapter.calls != 0 {
		t.Fatalf("background agent run review invoked runtime %d times, want zero", adapter.calls)
	}

	stdout.Reset()
	stderr.Reset()
	code = runAgentReview(append(args, "--foreground"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("foreground review exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if adapter.calls != 1 {
		t.Fatalf("explicit foreground review invoked runtime %d times, want one", adapter.calls)
	}

	stdout.Reset()
	stderr.Reset()
	code = runAgentRun(append(runArgs, "--foreground"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("foreground agent run review exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if adapter.calls != 2 {
		t.Fatalf("explicit foreground agent run review invoked runtime %d times, want two total", adapter.calls)
	}
}

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
	githubtest.NoopClient
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
		[]string{"review", "implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	return cliReviewLoopFixture{
		store: store, home: home, checkout: checkout,
		record: db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout, DefaultBranch: "main", PollInterval: "30s"},
		repo:   github.Repository{Owner: "owner", Name: "repo"},
	}
}

func seedCLIReviewLoopVerdict(t *testing.T, store *db.Store, id, head, decision string) {
	t.Helper()
	seedCLIReviewLoopVerdictForAgent(t, store, "reviewer", id, head, decision)
}

func seedCLIReviewLoopVerdictForAgent(t *testing.T, store *db.Store, agent, id, head, decision string) {
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
		ID: id, Agent: agent, Type: "review", State: string(workflow.JobSucceeded), Payload: string(payload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: decision}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", id, err)
	}
}

func assertCLIReviewLoopHardRefusal(t *testing.T, fixture cliReviewLoopFixture, request localAgentDispatchRequest, priorJobID string) {
	t.Helper()
	adapterCalls := 0
	previousFactory := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) {
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

// The agent-identity loop guard never admits an unresolved requester. Production
// dispatch resolves and authorizes the reviewer before DetectReviewLoop, so an
// unregistered reviewer must fail closed without creating loop evidence or work.
func TestCLIReviewLoopUnresolvableReviewerFailsClosedBeforeIdentityGuard(t *testing.T) {
	ctx := context.Background()
	fixture := newCLIReviewLoopFixture(t)
	seedCLIReviewLoopVerdictForAgent(t, fixture.store, "ghost-reviewer", "prior-review", "same-head", "approved")

	family, resolved, err := workflow.ResolveRuntimeFamily(ctx, fixture.store, "", "ghost-reviewer", "")
	if err != nil {
		t.Fatalf("ResolveRuntimeFamily: %v", err)
	}
	if resolved || family != "" {
		t.Fatalf("fixture weakened: ghost-reviewer family = %q, resolved=%v; want unresolved", family, resolved)
	}

	adapterCalls := 0
	previousFactory := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) {
		adapterCalls++
		return nil, errors.New("adapter must not be selected for an unresolved reviewer")
	}
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousFactory })

	_, err = dispatchLocalAgentJob(ctx, fixture.store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "ghost-reviewer", LeadAgent: "reviewer", Action: "review", PullRequest: 227,
		Branch: "main", HeadSHA: "same-head", Instructions: "Review unchanged head.", Home: fixture.home,
	})
	if err == nil || !strings.Contains(err.Error(), `agent "ghost-reviewer" not found`) {
		t.Fatalf("dispatch error = %v, want fail-closed unregistered reviewer refusal", err)
	}

	jobs, listErr := fixture.store.ListJobs(ctx)
	if listErr != nil {
		t.Fatalf("ListJobs: %v", listErr)
	}
	if len(jobs) != 1 || jobs[0].ID != "prior-review" {
		t.Fatalf("jobs = %+v, want only the prior succeeded review", jobs)
	}
	if got := countCLIJobEvents(t, fixture.store, "prior-review", workflow.ReviewLoopDetectedEventKind); got != 0 {
		t.Fatalf("review_loop_detected events = %d, want zero before identity guard", got)
	}
	tasks, listErr := fixture.store.ListTasks(ctx)
	if listErr != nil {
		t.Fatalf("ListTasks: %v", listErr)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks = %+v, want none before reviewer admission", tasks)
	}
	worktreeRoot := filepath.Join(config.PathsForHome(fixture.home).Home, "worktrees")
	if _, statErr := os.Stat(worktreeRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree root %q was created before refusal: %v", worktreeRoot, statErr)
	}
	if adapterCalls != 0 {
		t.Fatalf("runtime adapter selected %d times, want zero", adapterCalls)
	}
}

// TestCLIReviewLoopAllowsNewHeadAndMixedDecisions kills repo/PR-only and
// decision-blind suppression mutants at the CLI preparation seam.
func TestCLIReviewLoopAllowsNewHeadAndMixedDecisions(t *testing.T) {
	t.Run("new head", func(t *testing.T) {
		fixture := newCLIReviewLoopFixture(t)
		seedCLIReviewLoopVerdict(t, fixture.store, "prior-review", "old-head", "changes_requested")
		head := strings.TrimSpace(runGitOutput(t, fixture.checkout, "rev-parse", "HEAD"))
		request, err := prepareLocalReviewDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			PullRequest: 227, Branch: "main", HeadSHA: head, Home: fixture.home,
		})
		if err != nil {
			t.Fatalf("new-head prepare: %v", err)
		}
		if strings.TrimSpace(request.TaskID) == "" {
			t.Fatal("new-head prepare did not bind a review task")
		}
	})

	t.Run("mixed decisions at same head", func(t *testing.T) {
		fixture := newCLIReviewLoopFixture(t)
		head := strings.TrimSpace(runGitOutput(t, fixture.checkout, "rev-parse", "HEAD"))
		seedCLIReviewLoopVerdict(t, fixture.store, "prior-approved", head, "approved")
		seedCLIReviewLoopVerdict(t, fixture.store, "prior-changes", head, "changes_requested")
		request, err := prepareLocalReviewDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			PullRequest: 227, Branch: "main", HeadSHA: head, Home: fixture.home,
		})
		if err != nil {
			t.Fatalf("mixed-decision prepare: %v", err)
		}
		if strings.TrimSpace(request.TaskID) == "" {
			t.Fatal("mixed-decision prepare did not bind a review task")
		}
	})
}

// TestCLIReviewLoopHerdres227Shape reproduces the 319-attempt incident shape:
// one succeeded changes_requested review at one head followed by 318 identical
// local admissions. It kills reliance on nanosecond local job IDs, missing job-
// count enforcement, and non-idempotent event emission.
func TestCLIReviewLoopHerdres227Shape(t *testing.T) {
	fixture := newCLIReviewLoopFixture(t)
	// A FULL sha, because #2054 now refuses a sha-shaped abbreviation at
	// dispatch and this test's subject is the review-loop refusal, not head
	// formatting. The token is arbitrary and shared with the seeded verdict; its
	// LENGTH was never load-bearing here.
	const herdres227Head = "2da08e1f4c7b6a5d3e2f1908a7b6c5d4e3f21098"
	seedCLIReviewLoopVerdict(t, fixture.store, "herdres-227-first", herdres227Head, "changes_requested")
	request := localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 227,
		Branch: "main", HeadSHA: herdres227Head, Instructions: "Review unchanged head.", Home: fixture.home,
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

// Kills parser mutants that omit either --lead form, accept a blank value, or
// parse the flag without threading it into localAgentDispatchRequest.
func TestParseAgentRunOptionsCapturesLeadForReviewAndRun(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		args    []string
	}{
		{name: "review spaced", command: "review", args: []string{"reviewer", "Review it.", "--pr", "7", "--lead", "implementer"}},
		{name: "review inline", command: "review", args: []string{"reviewer", "Review it.", "--pr=7", "--lead=implementer"}},
		{name: "run spaced", command: "run", args: []string{"reviewer", "Review it.", "--action", "review", "--pr", "7", "--lead", "implementer"}},
		{name: "run inline", command: "run", args: []string{"reviewer", "Review it.", "--action=review", "--pr=7", "--lead=implementer"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			options, ok := parseAgentRunOptions(test.command, test.args, &stderr)
			if !ok {
				t.Fatalf("parseAgentRunOptions failed: %s", stderr.String())
			}
			if options.lead != "implementer" {
				t.Fatalf("lead = %q, want implementer", options.lead)
			}
			request := localAgentDispatchRequestFromOptions(options, "review", "test", "test")
			if request.LeadAgent != "implementer" {
				t.Fatalf("request LeadAgent = %q, want implementer", request.LeadAgent)
			}
		})
	}

	for _, args := range [][]string{
		{"reviewer", "Review it.", "--pr", "7", "--lead", "   "},
		{"reviewer", "Review it.", "--pr=7", "--lead="},
	} {
		var stderr bytes.Buffer
		if _, ok := parseAgentRunOptions("review", args, &stderr); ok || !strings.Contains(stderr.String(), "--lead requires a non-blank value") {
			t.Fatalf("blank --lead args=%v ok=%v stderr=%q", args, ok, stderr.String())
		}
	}
}

// Kills the scope mutant that silently persists review routing metadata on
// ask, implement, or orchestrate dispatches.
func TestAgentLeadRejectedOutsideReviewDispatch(t *testing.T) {
	for _, command := range []string{"implement", "orchestrate"} {
		var stderr bytes.Buffer
		_, ok := parseAgentRunOptions(command, []string{"worker", "Do it.", "--lead", "implementer"}, &stderr)
		if ok || !strings.Contains(stderr.String(), "--lead is only supported for agent review and agent run") {
			t.Fatalf("command=%s ok=%v stderr=%q", command, ok, stderr.String())
		}
	}

	for _, action := range []string{"ask", "implement"} {
		options := agentRunOptions{home: t.TempDir(), agent: "worker", message: "Do it.", lead: "implementer"}
		var stdout, stderr bytes.Buffer
		_, exit := dispatchAgentCommand(options, action, "test", "agent_run", &stdout, &stderr)
		if exit != 2 || !strings.Contains(stderr.String(), "--lead is only supported when routing to review") {
			t.Fatalf("action=%s exit=%d stderr=%q", action, exit, stderr.String())
		}
	}
}

// PRE-FIX RED: the review ran and only the later fix advance blocked. This kills
// validation limited to explicit --lead and validation delayed until advance.
func TestDispatchReviewWithoutLeadRejectsReviewOnlyAgentBeforeEnqueue(t *testing.T) {
	fixture := reviewLeadRefusalStore(t)
	store := fixture.store
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	adapter := installReviewLeadTestAdapter(t, "")

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 7,
		HeadSHA: fixture.head, Branch: "feature/review", Home: fixture.home,
	})
	if err == nil || !strings.Contains(err.Error(), `review lead "reviewer" lacks implement capability`) {
		t.Fatalf("dispatch error = %v", err)
	}
	assertReviewLeadHardRefusal(t, store, fixture.checkout, adapter)
}

// TestDispatchReviewRejectsAbbreviatedHeadSHABeforeEnqueue is #2054 defect 1.
//
// An abbreviated --head-sha was ACCEPTED at dispatch and then cancelled by the
// daemon's staleness check, which compared the 8-character value against the
// PR's 40-character head and reported `superseded_stale_head: PR #N moved from
// head "7e4b39d0" to "7e4b39d0ef82…"`. Those are the same commit. The review
// never ran, and the event sent its operator to look for a push that never
// happened.
//
// Measured contrast on 2026-09-08: the review of #2035 that SUCCEEDED carries a
// 40-character head_sha; the #2047 job that died carries 8. Same dispatcher,
// same reviewer, same day.
//
// A refusal that arrives AFTER the job exists is the wrong shape regardless of
// its message, so this asserts the refusal happens where the other pre-enqueue
// refusals do: no job row, no task, no worktree.
func TestDispatchReviewRejectsAbbreviatedHeadSHABeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	checkout, _, _, head, _ := promptHeadBindingCheckout(t)
	store, home := blockerE2EHome(t)
	seedReviewDispatchFixture(t, store, checkout)
	if len(head) != 40 {
		t.Fatalf("fixture head is %d characters, want 40: the abbreviation under test must be a real prefix", len(head))
	}

	before, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tasksBefore, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := reviewDispatchRequest(home, head[:8])
	_, dispatchErr := dispatchLocalAgentJob(ctx, store, request)
	if dispatchErr == nil {
		t.Fatal("dispatch accepted an abbreviated --head-sha; the daemon would cancel it later as a stale head")
	}
	// The message must name the EXPECTED LENGTH, because the operator's next
	// action is to re-run with a full sha and "invalid head" does not say how.
	for _, want := range []string{"40", head[:8]} {
		if !strings.Contains(dispatchErr.Error(), want) {
			t.Fatalf("dispatch error = %v, want it to name %q", dispatchErr, want)
		}
	}
	// No job row: a refusal that arrives after the row exists has already spent
	// a worktree and a queue slot.
	after, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("job rows went from %d to %d; the refusal created a row", len(before), len(after))
	}
	// NO TASK EITHER, and this arm is review finding F3 on cycle two: the guard
	// used to run one call too late, after prepareLocalReviewDispatchRequest had
	// ended in prepareLocalReviewTask, whose UpsertTaskUnlessStates INSERTS OR
	// UPDATES the review Task. The comment above this test promised "no job, no
	// task, no worktree" while only the job was checked, so the durable half of
	// the promise was unasserted and the regression was invisible.
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != len(tasksBefore) {
		t.Fatalf("task rows went from %d to %d; the refusal left durable task state behind for a review that never ran", len(tasksBefore), len(tasks))
	}
}

// A REVISION EXPRESSION is the same defect wearing different characters, and the
// first version of this guard let it through: review finding F3. The daemon
// compares this value to the pull request's head by equality, so `<sha>^` can no
// more bind than `<sha8>` can.
func TestDispatchReviewRejectsARevisionExpressionHead(t *testing.T) {
	ctx := context.Background()
	checkout, _, _, head, _ := promptHeadBindingCheckout(t)
	store, home := blockerE2EHome(t)
	seedReviewDispatchFixture(t, store, checkout)

	for _, expression := range []string{head + "^", head[:8] + "~1", "HEAD"} {
		t.Run(expression, func(t *testing.T) {
			request := reviewDispatchRequest(home, expression)
			if _, err := dispatchLocalAgentJob(ctx, store, request); err == nil {
				t.Fatalf("dispatch accepted %q as a head", expression)
			} else if !strings.Contains(err.Error(), "40") {
				t.Fatalf("refusal for %q = %v, want it to name the expected length", expression, err)
			}
		})
	}
}

// A FULL head sha is the same dispatch and must not be refused: without this the
// length check could reject everything and both tests above would still pass.
func TestDispatchReviewAcceptsFullHeadSHA(t *testing.T) {
	fixture := reviewLeadRefusalStore(t)
	store := fixture.store
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "true", []string{"implement", "review"}, "owner/repo", runtime.AutonomyPolicyDangerFullAccess)
	installReviewLeadTestAdapter(t, "")

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 7, LeadAgent: "lead",
		HeadSHA: fixture.head, Branch: "feature/review", Home: fixture.home, Background: true,
	})
	if err != nil && strings.Contains(err.Error(), "head") {
		t.Fatalf("a full 40-character head sha was refused by the head guard: %v", err)
	}
}

// TestDispatchReviewOnlyNeedsNoImplementCapableLead is #2054 defect 2.
//
// `agent review <reviewer>` refused when the reviewer could not implement,
// because a changes_requested verdict needs somewhere to go. The documented
// fallback, `agent ask`, has no --head-sha and cannot bind a verdict to a head
// at all - so on 2026-09-08 that pair sent every seat needing an exact-head
// review onto ask, and one dispatch bound to a DIFFERENT pull request's merge
// commit.
//
// The requirement is kept and made stateable: --no-fix-target declares the
// operator owns the follow-up. An UNSTATED absence must still refuse, which
// TestDispatchReviewWithoutLeadRejectsReviewOnlyAgentBeforeEnqueue pins.
func TestDispatchReviewOnlyNeedsNoImplementCapableLead(t *testing.T) {
	ctx := context.Background()
	checkout, _, _, head, _ := promptHeadBindingCheckout(t)
	store, home := blockerE2EHome(t)
	seedReviewDispatchFixture(t, store, checkout)

	request := reviewDispatchRequest(home, head)
	// The reviewer cannot implement, and no lead is named: exactly the dispatch
	// that refused all day and pushed every seat onto `agent ask`.
	request.LeadAgent = ""
	request.NoFixTarget = true

	out, err := dispatchLocalAgentJob(ctx, store, request)
	if err != nil {
		t.Fatalf("a review-only dispatch was refused: %v", err)
	}
	// THE DECISION MUST REACH THE PAYLOAD, not only a job event. Review found
	// that clearing the lead was not enough: enqueue restored the reviewer
	// through firstNonEmpty, so a changes_requested verdict would have routed
	// its fix to the agent that produced the verdict, and advancement had no
	// field to read the declaration from.
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !payload.NoFixTarget {
		t.Fatalf("payload.NoFixTarget = false; advancement cannot honour a declaration it cannot read: %s", job.Payload)
	}
	if strings.TrimSpace(payload.LeadAgent) != "" {
		t.Fatalf("payload.LeadAgent = %q, want empty: a review-only dispatch must not fall back to the reviewer", payload.LeadAgent)
	}
	events, err := store.ListJobEvents(ctx, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "review_no_fix_target" {
			return
		}
	}
	t.Fatalf("no review_no_fix_target event: %+v", events)
}

// --no-fix-target and --lead answer the same question in opposite directions, so
// accepting both would leave which one wins undefined.
func TestDispatchReviewOnlyRejectsAnExplicitLead(t *testing.T) {
	fixture := reviewLeadRefusalStore(t)
	store := fixture.store
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "true", []string{"implement", "review"}, "owner/repo", runtime.AutonomyPolicyDangerFullAccess)
	adapter := installReviewLeadTestAdapter(t, "")

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 7, LeadAgent: "lead",
		HeadSHA: fixture.head, Branch: "feature/review", Home: fixture.home, NoFixTarget: true,
	})
	// This refusal must precede the capacity and worktree work, like the other
	// lead refusals, which is why it stays on the refusal fixture.
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("dispatch error = %v, want a mutual-exclusion refusal", err)
	}
	assertReviewLeadHardRefusal(t, store, fixture.checkout, adapter)
}

// Kills a missing-existence-check mutant and a mutant that silently falls back
// to the reviewer when an explicit lead does not exist.
func TestDispatchReviewRejectsUnknownLeadBeforeEnqueue(t *testing.T) {
	fixture := reviewLeadRefusalStore(t)
	store := fixture.store
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	adapter := installReviewLeadTestAdapter(t, "")

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 7, LeadAgent: "missing",
		HeadSHA: fixture.head, Branch: "feature/review", Home: fixture.home,
	})
	if err == nil || !strings.Contains(err.Error(), `review lead "missing" is not subscribed`) {
		t.Fatalf("dispatch error = %v", err)
	}
	assertReviewLeadHardRefusal(t, store, fixture.checkout, adapter)
}

// Kills existence-only validation that never checks the lead's DB capability.
func TestDispatchReviewRejectsLeadWithoutImplementBeforeEnqueue(t *testing.T) {
	fixture := reviewLeadRefusalStore(t)
	store := fixture.store
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "true", []string{"ask", "review"}, "owner/repo", runtime.AutonomyPolicyDangerFullAccess)
	adapter := installReviewLeadTestAdapter(t, "")

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 7, LeadAgent: "lead",
		HeadSHA: fixture.head, Branch: "feature/review", Home: fixture.home,
	})
	if err == nil || !strings.Contains(err.Error(), `review lead "lead" lacks implement capability`) {
		t.Fatalf("dispatch error = %v", err)
	}
	assertReviewLeadHardRefusal(t, store, fixture.checkout, adapter)
}

// The lead exists only in the DB fixture. This kills capability-only validation,
// checking the reviewer's policy, and config.toml-based policy lookup.
func TestDispatchReviewRejectsReadOnlyLeadBeforeEnqueue(t *testing.T) {
	fixture := reviewLeadRefusalStore(t)
	store := fixture.store
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	adapter := installReviewLeadTestAdapter(t, "")

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 7, LeadAgent: "lead",
		HeadSHA: fixture.head, Branch: "feature/review", Home: fixture.home,
	})
	if err == nil || !strings.Contains(err.Error(), `review lead "lead": autonomy policy "read-only" grants no write permission`) {
		t.Fatalf("dispatch error = %v", err)
	}
	assertReviewLeadHardRefusal(t, store, fixture.checkout, adapter)
}

// Kills validation that accepts a lead which the later fix dispatch cannot use
// on the review's repository.
func TestDispatchReviewRejectsLeadWithoutRepoAccessBeforeEnqueue(t *testing.T) {
	fixture := reviewLeadRefusalStore(t)
	store := fixture.store
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.ShellRuntime, "true", []string{"implement"}, "other/repo", runtime.AutonomyPolicyWorkspaceWrite)
	adapter := installReviewLeadTestAdapter(t, "")

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Action: "review", PullRequest: 7, LeadAgent: "lead",
		HeadSHA: fixture.head, Branch: "feature/review", Home: fixture.home,
	})
	if err == nil || !strings.Contains(err.Error(), `review lead "lead" is not allowed on "owner/repo"`) {
		t.Fatalf("dispatch error = %v", err)
	}
	assertReviewLeadHardRefusal(t, store, fixture.checkout, adapter)
}

// Kills the managed-type mutant that provisions a runtime before a concrete,
// DB-backed fix target can be validated. The configured type pins error
// precedence: only an existing managed type reaches the requires-lead refusal.
func TestDispatchReviewManagedTypeRequiresExplicitLeadBeforeProvisioning(t *testing.T) {
	fixture := reviewLeadRefusalStore(t)
	store := fixture.store
	home := fixture.home
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := config.SaveAgentType(paths, config.AgentType{
		Name: "reviewer-type", Runtime: runtime.CodexRuntime,
		Capabilities: []string{"review"}, AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	adapter := installReviewLeadTestAdapter(t, "")

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", Type: "reviewer-type", Action: "review", PullRequest: 7,
		HeadSHA: fixture.head, Branch: "feature/review", Home: home,
	})
	if err == nil || !strings.Contains(err.Error(), `managed type "reviewer-type" requires --lead`) {
		t.Fatalf("dispatch error = %v", err)
	}
	assertReviewLeadHardRefusal(t, store, fixture.checkout, adapter)
	instances, listErr := store.ListAgentInstances(context.Background())
	if listErr != nil || len(instances) != 0 {
		t.Fatalf("managed instances = %+v, err=%v; want none", instances, listErr)
	}
}

// A review's --lead routes the review workflow, but it is not PR ownership.
// Auto-fix must use the implementing agent recorded on the shared task.
func makeReviewFixOriginFetchable(t *testing.T, checkout, branch string) {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "origin.git")
	runDaemonWorkerGit(t, checkout, "init", "--bare", remote)
	runDaemonWorkerGit(t, checkout, "push", remote, "HEAD:refs/heads/"+branch)
	runDaemonWorkerGit(t, checkout, "remote", "set-url", "origin", "git@github.com:owner/repo.git")
	ssh := filepath.Join(t.TempDir(), "git-ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nexec git-upload-pack \"$GITMOOT_TEST_FIX_REMOTE\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile git-ssh: %v", err)
	}
	// Preserve a GitHub-shaped origin for checkout validation while routing the
	// fixture's SSH upload-pack transport to a local bare repository.
	t.Setenv("GITMOOT_TEST_FIX_REMOTE", remote)
	t.Setenv("GIT_SSH_COMMAND", ssh)
}

type reviewLeadRefusalFixture struct {
	store    *db.Store
	checkout string
	head     string
	home     string
}

func reviewLeadRefusalStore(t *testing.T) reviewLeadRefusalFixture {
	t.Helper()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	head, err := (gitutil.NewHostClient(checkout)).HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	return reviewLeadRefusalFixture{store: store, checkout: checkout, head: head, home: t.TempDir()}
}

func installReviewLeadTestAdapter(t *testing.T, output string) *cliWorkerFakeAdapter {
	t.Helper()
	adapter := &cliWorkerFakeAdapter{output: output}
	previous := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) {
		return adapter, nil
	}
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previous })
	return adapter
}

func assertReviewLeadHardRefusal(t *testing.T, store *db.Store, checkout string, adapter *cliWorkerFakeAdapter) {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want zero after hard refusal", jobs)
	}
	tasks, err := store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks returned error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks = %+v, want zero after hard refusal", tasks)
	}
	entries, err := os.ReadDir(filepath.Join(checkout, ".git", "worktrees"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read git worktree registry: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("git worktree registry entries = %+v, want none after hard refusal", entries)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want zero after hard refusal", adapter.calls)
	}
}

// #2194: `agent review` must subscribe the acting role to the verdict, the way
// `review request` always has. Measured 2026-09-16: 687 of 688 reviews on this
// box were dispatched by this command, so in practice no review subscribed
// anyone and every verdict was relayed by hand - and twice in one day a
// published verdict left its requester still waiting for it.
func TestAgentReviewSubscribesTheActingRoleToTheVerdict(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`
[org]
enforce = "warn"
[org.roles."owner"]
scope = ["*"]
[org.roles."joltra"]
parent = "owner"
scope = ["owner/repo"]
`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "run-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	args := []string{
		"reviewer", "Review this exact head.", "--repo", "owner/repo", "--pr", "12",
		"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
		"--org-role", "joltra", "--home", home,
	}
	var stdout, stderr bytes.Buffer
	if code := runAgentReview(args, &stdout, &stderr); code != 0 {
		t.Fatalf("review exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	// The wait must exist in the STORE, keyed to this exact head and purpose -
	// not merely be printed.
	waiting, err := store.ListAwaitedFacts(context.Background(), "joltra", db.AwaitedFactStateWaiting)
	if err != nil {
		t.Fatal(err)
	}
	wantKey, err := db.ReviewRequestSubjectKey("owner/repo", 12, head, db.DefaultReviewPurpose)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, fact := range waiting {
		if fact.SubjectKind == db.AwaitedFactSubjectReviewVerdict && fact.SubjectKey == wantKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("joltra holds no wait on %s: waiting=%+v", wantKey, waiting)
	}
	if !strings.Contains(stdout.String(), "notify: awaited fact wake to joltra") {
		t.Fatalf("stdout = %q, want the attached wait reported", stdout.String())
	}

	// Dispatching twice at the same head must not spend a second wait.
	//
	// THREE DEFENDERS, NAMED SEPARATELY, because two earlier versions of this
	// comment each credited the wrong one and each would have told a reader that
	// a load-bearing line was safe to delete:
	//   1. DEDUP (no second row) - the store: a unique index on
	//      (waiter_role, subject_kind, subject_key) WHERE state='waiting' plus
	//      ON CONFLICT in SubscribeAwaitedFact. Breaking ON CONFLICT kills the
	//      row-count assertion; deleting the pre-check loop does not.
	//   2. DEADLINE PRESERVATION - the pre-check loop in
	//      subscribeRoleToReviewVerdict. ON CONFLICT EXTENDS a deadline when the
	//      joiner's is later and every re-dispatch computes now+ttl, so without
	//      the loop a role re-dispatching its own review pushes its expiry out
	//      each time, and a wait that never expires never escalates.
	//   3. HOLD SUPPRESSION - also the loop: it returns before the insert, so a
	//      repeat dispatch does not re-emit the same head-blind holds.
	// A comment naming a guard's defender is a claim about CAUSATION, and
	// causation is what reading cannot establish. Each line above was checked by
	// running the mutant that breaks only that defender.
	var stdout2, stderr2 bytes.Buffer
	if code := runAgentReview(args, &stdout2, &stderr2); code != 0 {
		t.Fatalf("second review exit=%d stderr=%q", code, stderr2.String())
	}
	after, err := store.ListAwaitedFacts(context.Background(), "joltra", db.AwaitedFactStateWaiting)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(waiting) {
		t.Fatalf("waits grew from %d to %d on a repeat dispatch at the same head", len(waiting), len(after))
	}

	// AND THE DEADLINE MUST NOT MOVE. This is what the pre-check loop actually
	// defends (#2194 review asked whether it had a non-dedup purpose - it does):
	// SubscribeAwaitedFact's ON CONFLICT arm EXTENDS the deadline whenever the
	// joiner's is later, and every re-dispatch computes now+ttl, which always
	// is. Without the short-circuit a role re-dispatching its own review pushes
	// its expiry out each time, and a wait that never expires never escalates.
	var deadlineBefore, deadlineAfter string
	for _, fact := range waiting {
		if fact.SubjectKey == wantKey {
			deadlineBefore = fact.Deadline
		}
	}
	for _, fact := range after {
		if fact.SubjectKey == wantKey {
			deadlineAfter = fact.Deadline
		}
	}
	if deadlineBefore == "" || deadlineAfter != deadlineBefore {
		t.Fatalf("deadline moved on a repeat dispatch: %q -> %q", deadlineBefore, deadlineAfter)
	}
}

// The invariant the #2194 review named: no attached fact MUST mean a stated
// hold. A path that attaches nothing and says nothing is this PR's own defect
// re-created inside the fix.
func TestAgentReviewNeverFailsToAttachSilently(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options agentRunOptions
	}{
		{"no acting role", agentRunOptions{repo: "owner/repo", prNumber: 12, headSHA: "0bd967c5ba8e506607bd3a9999a94a4db5b881b4"}},
		{"no head", agentRunOptions{repo: "owner/repo", prNumber: 12, orgRole: "joltra"}},
		{"no pull request", agentRunOptions{repo: "owner/repo", orgRole: "joltra", headSHA: "0bd967c5ba8e506607bd3a9999a94a4db5b881b4"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			options := testCase.options
			options.home = t.TempDir()
			if err := config.Initialize(config.PathsForHome(options.home)); err != nil {
				t.Fatal(err)
			}
			var output localAgentJobOutput
			var stderr bytes.Buffer
			attachReviewVerdictWait(&output, options, &stderr)
			if output.AwaitedFactID == 0 && len(output.SubscriptionHolds) == 0 {
				t.Fatal("no fact and no hold: the requester would wait its full TTL with nothing attached and nothing said")
			}
		})
	}
}

// A dispatch that cannot be woken must SAY so: an absent subscription is
// indistinguishable from a working one until the wait expires (#2194).
func TestAgentReviewReportsWhenNoVerdictWaitCanBeAttached(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "run-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	var stdout, stderr bytes.Buffer
	code := runAgentReview([]string{
		"reviewer", "Review this exact head.", "--repo", "owner/repo", "--pr", "12",
		"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
		"--foreground", "--home", home,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("review exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not awaiting verdict: no --org-role") {
		t.Fatalf("stdout = %q, want the missing-role hold stated", stdout.String())
	}
}

// #2194 review: the attach site used to HARDCODE db.DefaultReviewPurpose. That
// is correct only while `agent review` cannot express another purpose - and the
// person who adds --purpose would inherit a wait keyed to "code" while the
// review runs as something else, reading a comment that said the line was fine.
// Correct-by-unreachability is the signature defect of this very subsystem
// (#2180's fallback), so the purpose is now READ FROM THE DISPATCHED JOB.
//
// Driven through the real helper against a real job row, because the CLI has no
// flag to express a non-default purpose yet: that is exactly why a test is
// needed now rather than when the flag lands.
func TestAgentReviewWaitUsesTheJobsOwnReviewPurpose(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	ctx := context.Background()
	const head = "0bd967c5ba8e506607bd3a9999a94a4db5b881b4"
	payload, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", PullRequest: 12, ReviewPurpose: "security"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "job-purpose-derived", Agent: "reviewer", Type: "review",
		State: string(workflow.JobQueued), Repo: "owner/repo", PullRequest: 12, Payload: string(payload),
	}); err != nil {
		t.Fatal(err)
	}
	output := localAgentJobOutput{JobID: "job-purpose-derived"}
	var stderr bytes.Buffer
	attachReviewVerdictWait(&output, agentRunOptions{
		home: home, repo: "owner/repo", prNumber: 12, headSHA: head, orgRole: "joltra",
	}, &stderr)
	if output.AwaitedFactID == 0 {
		t.Fatalf("no wait attached: holds=%v stderr=%q", output.SubscriptionHolds, stderr.String())
	}
	wantSecurity, err := db.ReviewRequestSubjectKey("owner/repo", 12, head, "security")
	if err != nil {
		t.Fatal(err)
	}
	wantCode, err := db.ReviewRequestSubjectKey("owner/repo", 12, head, db.DefaultReviewPurpose)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := store.ListAwaitedFacts(ctx, "joltra", db.AwaitedFactStateWaiting)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, fact := range waiting {
		keys = append(keys, fact.SubjectKey)
	}
	var foundSecurity, foundCode bool
	for _, key := range keys {
		if key == wantSecurity {
			foundSecurity = true
		}
		if key == wantCode {
			foundCode = true
		}
	}
	if !foundSecurity {
		t.Fatalf("wait not keyed to the job's own purpose: keys=%v want %s", keys, wantSecurity)
	}
	if foundCode {
		t.Fatalf("wait keyed to the hardcoded default as well: keys=%v", keys)
	}

	// AND A JOB RECORDING NO PURPOSE MUST STILL ATTACH. ReviewRequestSubjectKey
	// REJECTS an empty purpose ("review purpose is required"), so taking the
	// recorded value unconditionally would turn a blank field into a failed
	// subscription - the requester left waiting, which is this PR's own defect.
	// No dispatch produces a blank purpose today; that is exactly why the guard
	// is pinned here rather than trusted.
	blank, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", PullRequest: 13})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "job-purpose-blank", Agent: "reviewer", Type: "review",
		State: string(workflow.JobQueued), Repo: "owner/repo", PullRequest: 13, Payload: string(blank),
	}); err != nil {
		t.Fatal(err)
	}
	blankOutput := localAgentJobOutput{JobID: "job-purpose-blank"}
	var blankStderr bytes.Buffer
	attachReviewVerdictWait(&blankOutput, agentRunOptions{
		home: home, repo: "owner/repo", prNumber: 13, headSHA: head, orgRole: "joltra",
	}, &blankStderr)
	if blankOutput.AwaitedFactID == 0 {
		t.Fatalf("a job with no recorded purpose attached no wait: holds=%v stderr=%q",
			blankOutput.SubscriptionHolds, blankStderr.String())
	}
}

// #2196: `agent review` must route THROUGH `review request`, not beside it.
// Measured 2026-09-16: 687 of 688 reviews came through this command, so the
// router's machinery was reachable in principle and unused in practice.
// This pins the four things delegation must deliver, and the two the caller
// supplies that delegation must not eat.
func TestAgentReviewRoutesThroughTheReviewRouter(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[org]\nenforce = \"warn\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n[org.roles.\"joltra\"]\nparent = \"owner\"\nscope = [\"owner/repo\"]\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	store := openCLIJobStore(t, home)
	defer store.Close()
	ctx := context.Background()
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "run-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	// A reviewer THE ROUTER WOULD NEVER PICK: selectReviewRouterAgent excludes
	// implement-capable agents from its candidate pool, while an explicitly
	// named reviewer only needs the review capability. So naming this one makes
	// the assertion below discriminating - dropping --reviewer cannot pick it by
	// coincidence, which is exactly how the first version of this test passed
	// while asserting nothing.
	seedDaemonWorkerAgentWithPolicy(t, store, "dual-reviewer", runtime.ShellRuntime, "true", []string{"review", "implement"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	const message = "Attack point one: the precedence table. Attack point two: the holder predicate."
	var stdout, stderr bytes.Buffer
	if code := runAgentReview([]string{
		"dual-reviewer", message, "--repo", "owner/repo", "--pr", "12",
		"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
		"--org-role", "joltra", "--model", "openai-codex/gpt-5.6-sol", "--workflow", "release/queue",
		"--effort", "high", "--home", home,
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("review exit stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want exactly one", jobs)
	}
	job := jobs[0]
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}

	// 1. THE ROUTER PATH, not the direct one. Asserted on the route_selected
	// event because that is the SAME field the adoption measurement read: 687
	// "via agent_review" against 1 "via review_request". A test keyed to the
	// metric cannot pass while the metric stays broken.
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var route string
	for _, event := range events {
		if event.Kind == "route_selected" {
			route = event.Message
		}
	}
	if !strings.Contains(route, reviewRequestExecutionPath) {
		t.Fatalf("route_selected = %q, want the router path %q: the dispatch did not go through review request",
			route, reviewRequestExecutionPath)
	}
	// 2. THE CALLER'S NAMED REVIEWER SURVIVES - the reason this surface exists.
	if job.Agent != "dual-reviewer" {
		t.Fatalf("reviewer = %q, want the NAMED reviewer to survive delegation: the router's own pool excludes it, so this can only be the explicit path", job.Agent)
	}
	// 3. THE CALLER'S MESSAGE SURVIVES, verbatim.
	if !strings.Contains(payload.Instructions, message) {
		t.Fatalf("instructions dropped the caller's message: %q", payload.Instructions)
	}
	// 4. THE FIX TARGET SURVIVES: a named lead means a changes-requested verdict
	// has somewhere to route, which the router's own requests never have.
	if payload.LeadAgent != "implementer" {
		t.Fatalf("lead = %q, want implementer carried through", payload.LeadAgent)
	}
	// 5. THE MODEL POOL is attached by the router.
	if len(payload.ReviewModelPool) == 0 {
		t.Fatal("no review model pool attached: the router's fallback chain was not applied")
	}
	// 6. THE EXACT-HEAD CLAIM exists, which is what makes a duplicate request
	// attach instead of spending a second reviewer.
	subjectKey, err := db.ReviewRequestSubjectKey("owner/repo", 12, head, db.DefaultReviewPurpose)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.GetReviewRequest(ctx, subjectKey)
	if err != nil {
		t.Fatalf("no review claim recorded for %s: %v", subjectKey, err)
	}
	if claim.JobID != job.ID {
		t.Fatalf("claim job = %q, want the dispatched job %q", claim.JobID, job.ID)
	}
	// 7. THE VERDICT WAIT is attached to the acting role.
	waiting, err := store.ListAwaitedFacts(ctx, "joltra", db.AwaitedFactStateWaiting)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) == 0 {
		t.Fatal("no verdict wait attached for joltra")
	}

	// 8. --model SURVIVES, and this is the one that would fail invisibly. Every
	// dispatch on this campaign passes --model devin/swe-2; a review that loses
	// it still runs and still returns a verdict - a STATIC one, because the
	// model is what makes the seat able to execute. Nothing fails, so nothing
	// reports it.
	// DELIBERATELY NOT THE POOL HEAD: the pool head here is devin/swe-2, so an
	// assertion naming it would pass against a mutant that prints pool[0]. The
	// explicit model must differ from the default for the check to discriminate.
	if payload.Model != "openai-codex/gpt-5.6-sol" {
		t.Fatalf("model = %q, want the operator's explicit openai-codex/gpt-5.6-sol carried through delegation", payload.Model)
	}
	// 9. --workflow SURVIVES: a review filed under the wrong workflow is
	// invisible to every query keyed on it.
	if payload.WorkflowID != "release/queue" {
		t.Fatalf("workflow = %q, want release/queue", payload.WorkflowID)
	}
	// 10 and 11. --effort AND --session SURVIVE. Added because a mutant dropping
	// BOTH passed the earlier version: the fix carried four values and guarded
	// two, which is a guard that agrees on half its cases (#2196 review).
	if payload.Effort != "high" {
		t.Fatalf("effort = %q, want high carried through delegation", payload.Effort)
	}
	// --session IS NOT ASSERTED HERE ON PURPOSE. It lands as the runtime override
	// ref and REQUIRES --runtime, and forcing --runtime omp made this test pass
	// only on a host with the omp binary installed: CI refused the dispatch with
	// "executable file not found in $PATH" while it passed locally. A test that
	// depends on the host's installed runtimes is not testing delegation. Session
	// forwarding is asserted on the argument builder instead, which is host-free.
	if !slices.Contains(reviewRequestArgsFromAgentReview(agentRunOptions{
		repo: "owner/repo", prNumber: 12, headSHA: head, orgRole: "joltra", session: "fresh:probe",
	}), "--session") {
		t.Fatal("--session is not forwarded to review request")
	}

	// 13. THE RUNTIME REPORTED MUST BE THE ONE RESOLVED, NOT A FALLBACK. "omp"
	// was both the fallback and a real runtime, so the line read identically
	// whether the runtime was known or invented - a default indistinguishable
	// from a choice, which is this week's unifying defect (#2196 review, P2).
	if strings.Contains(stdout.String(), "on an unreported runtime") {
		t.Fatalf("stdout = %q, want the resolved runtime reported", stdout.String())
	}
	if !strings.Contains(stdout.String(), "reviewer: dual-reviewer on shell model") {
		t.Fatalf("stdout = %q, want the reviewer's own registered runtime (shell) reported rather than a fabricated omp", stdout.String())
	}
	// 12. THE OUTPUT MUST NOT MISREPORT WHAT IT DISPATCHED. This line hardcoded
	// "on omp model" and printed pool[0], so an operator passing --model saw the
	// pool head and concluded the override was dropped - positive false evidence,
	// worse than the silent drop it replaced.
	if !strings.Contains(stdout.String(), "model openai-codex/gpt-5.6-sol") {
		t.Fatalf("stdout = %q, want the DISPATCHED model reported, not the pool head devin/swe-2", stdout.String())
	}

	// AND A SECOND DISPATCH AT THE SAME HEAD MUST NOT SPEND A SECOND REVIEWER -
	// the dedup that was unreachable while this command bypassed the router.
	var stdout2, stderr2 bytes.Buffer
	if code := runAgentReview([]string{
		"dual-reviewer", message, "--repo", "owner/repo", "--pr", "12",
		"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
		"--org-role", "joltra", "--home", home,
	}, &stdout2, &stderr2); code != 0 {
		t.Fatalf("second review exit stderr=%q", stderr2.String())
	}
	after, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("second dispatch at the same head created %d jobs, want the claim to attach to the first", len(after))
	}

	// AND AN ATTACHING CALLER WITH DIFFERENT INPUTS MUST BE TOLD THEY WERE
	// DISCARDED, through the real command output rather than through the helper:
	// removing the call site has to fail something.
	var stdout3, stderr3 bytes.Buffer
	if code := runAgentReview([]string{
		"reviewer", "a DIFFERENT instruction", "--repo", "owner/repo", "--pr", "12",
		"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
		"--org-role", "joltra", "--home", home,
	}, &stdout3, &stderr3); code != 0 {
		t.Fatalf("third review exit stderr=%q", stderr3.String())
	}
	for _, want := range []string{"was NOT used", "instructions were NOT sent"} {
		if !strings.Contains(stdout3.String(), want) {
			t.Fatalf("attach output = %q, want %q: a caller told it is awaiting a verdict must be told its own inputs were discarded",
				stdout3.String(), want)
		}
	}
	// AND THE ATTACH PATH MUST REPORT THE RUNNING REVIEW'S RUNTIME. It set
	// output.Runtime nowhere, so every attach printed the fabricated "omp"
	// regardless of what the job runs on (#2196 review, P2).
	if !strings.Contains(stdout3.String(), "on shell model") {
		t.Fatalf("attach output = %q, want the running review's own runtime reported, not a fabricated omp", stdout3.String())
	}
}

// #2196 review: AN INPUT THE ROUTER CANNOT CARRY MUST NOT BE SILENTLY DROPPED.
// Delegation would discard it, so the dispatch keeps the direct path and SAYS
// which flag forced that. A caller who is not told cannot know its review
// differs from the one it asked for - the invisible-degradation shape this work
// exists to remove.
func TestAgentReviewDisclosesInputsTheRouterCannotCarry(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[org]\nenforce = \"warn\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n[org.roles.\"joltra\"]\nparent = \"owner\"\nscope = [\"owner/repo\"]\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "run-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	var stdout, stderr bytes.Buffer
	if code := runAgentReview([]string{
		"reviewer", "Review this.", "--repo", "owner/repo", "--pr", "12",
		"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
		"--org-role", "joltra", "--skip-native-review-fanout", "--home", home,
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("review exit stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--skip-native-review-fanout") {
		t.Fatalf("stderr = %q, want the flag that forced the direct path named", stderr.String())
	}
	if !strings.Contains(stderr.String(), "WITHOUT the review router") {
		t.Fatalf("stderr = %q, want the bypass stated", stderr.String())
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want the review still dispatched", jobs)
	}
}

// #2196 review (P1): THE FIX FOR A MISSED FLAG IS NOT A LONGER HAND LIST.
// Inspection found the message and --lead and missed at least eight others;
// adding those by hand leaves the flag someone adds NEXT MONTH in exactly the
// same position - silently dropped by a wrapper nobody re-audits.
//
// So this test enumerates `agent review`'s ACCEPTED FLAGS FROM ITS OWN PARSER
// SOURCE and asserts every one is either FORWARDED to `review request` or
// EXPLICITLY REJECTED into the direct path. A new flag that is neither fails
// here, which turns a silent degradation into a build break. Same move as
// deriving an expected method name from the method expression rather than
// typing it (#2188): the list stops being a second copy of the truth.
func TestAgentReviewWithoutLeadKeepsTheRefusalPath(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[org]\nenforce = \"warn\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n[org.roles.\"joltra\"]\nparent = \"owner\"\nscope = [\"owner/repo\"]\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	var stdout, stderr bytes.Buffer
	runAgentReview([]string{
		"reviewer", "Review this.", "--repo", "owner/repo", "--pr", "12",
		"--head-sha", head, "--branch", "feature/review",
		"--org-role", "joltra", "--home", home,
	}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "absent --lead with no --no-fix-target") {
		t.Fatalf("stderr = %q, want the absent-lead reason named: delegation must not silently convert #2054's refusal into a review-only dispatch", stderr.String())
	}
}

// #2196 review (P2): AN ATTACHING REQUEST MUST BE TOLD WHAT IT DID NOT GET.
// A second dispatch at a claimed head attaches to the running review - correct,
// and the dedup the router exists for - but the caller's own reviewer, message,
// lead and model are discarded. Without saying so, the caller is told it is
// awaiting a verdict and waits on a review it believes it commissioned. That is
// this campaign's central defect at the dispatch surface.
func TestAttachingRequestIsToldWhatWasDiscarded(t *testing.T) {
	for _, testCase := range []struct {
		name string
		opts reviewRequestOptions
		want string
	}{
		{"a different reviewer", reviewRequestOptions{reviewer: "my-reviewer"}, "--reviewer my-reviewer was NOT used"},
		{"instructions", reviewRequestOptions{message: "check the precedence table"}, "instructions were NOT sent"},
		{"a lead", reviewRequestOptions{lead: "my-implementer"}, "--lead my-implementer was NOT applied"},
		{"a model", reviewRequestOptions{model: "anthropic/claude-opus-5"}, "--model anthropic/claude-opus-5 was NOT used"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			holds := attachDiscardedInputs(reviewRequestOutput{Reviewer: "someone-else", Model: "devin/swe-2"}, testCase.opts)
			var joined string
			for _, hold := range holds {
				joined += hold + "\n"
			}
			if !strings.Contains(joined, testCase.want) {
				t.Fatalf("holds = %q, want %q stated", joined, testCase.want)
			}
		})
	}
	// AND NOTHING IS CLAIMED WHEN NOTHING WAS DISCARDED: a caller that named the
	// same reviewer and model the running review carries lost nothing, and a
	// false "your input was dropped" is its own misreport.
	if holds := attachDiscardedInputs(
		reviewRequestOutput{Reviewer: "reviewer", Model: "devin/swe-2"},
		reviewRequestOptions{reviewer: "Reviewer", model: "devin/swe-2"},
	); len(holds) != 0 {
		t.Fatalf("holds = %v, want none when the running review already matches the request", holds)
	}
}

// #2196 review (P3): the round-5 rewrite replaced the only END-TO-END session
// assertion with an argument-builder probe, which proves FORWARDING and not
// HONORING - a mutant deleting the one-line `RuntimeSession` mapping this PR
// added passed the whole suite. For a PR whose defect class is silently-dropped
// inputs, the newest carried input had lost its only real guard.
//
// Uses --runtime shell --session <command>, which is valid and needs NO host
// binary: that is what made the previous attempt CI-red.
func TestReviewRequestHonorsTheForwardedSession(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[org]\nenforce = \"warn\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n[org.roles.\"joltra\"]\nparent = \"owner\"\nscope = [\"owner/repo\"]\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	store := openCLIJobStore(t, home)
	defer store.Close()
	ctx := context.Background()
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "shell-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	var stdout, stderr bytes.Buffer
	if code := runReviewRequest([]string{
		"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review",
		"--role", "joltra", "--reviewer", "shell-reviewer",
		"--runtime", runtime.ShellRuntime, "--session", "true", "--home", home,
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("review request exit stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	jobs, listErr := store.ListJobs(ctx)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one", jobs)
	}
	payload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatal(err)
	}
	if payload.RuntimeOverrideRef != "true" {
		t.Fatalf("runtime override ref = %q, want the forwarded session HONORED in the payload", payload.RuntimeOverrideRef)
	}
	if payload.RuntimeOverride != runtime.ShellRuntime {
		t.Fatalf("runtime override = %q, want shell", payload.RuntimeOverride)
	}
	// AND THE OUTPUT REPORTS THAT RUNTIME rather than a fabricated omp.
	if !strings.Contains(stdout.String(), "on shell model") {
		t.Fatalf("stdout = %q, want the dispatched runtime reported", stdout.String())
	}
}

// #2196 review: A MESSAGE STARTING WITH '-' MUST STILL DISPATCH. The delegated
// path re-enters a flag parser, which the message never did before, so
// `agent review reviewer "-check the diff"` began exiting 2 with "flag provided
// but not defined". Rare input, loud failure, and a regression introduced by the
// delegation rather than a pre-existing limit.
func TestDelegatedMessageMayStartWithADash(t *testing.T) {
	args := reviewRequestArgsFromAgentReview(agentRunOptions{
		repo: "owner/repo", prNumber: 12, headSHA: "0bd967c5ba8e506607bd3a9999a94a4db5b881b4",
		orgRole: "joltra", agent: "reviewer", lead: "implementer",
		message: "-check the diff",
	})
	// The terminator must precede the message, and nothing may follow it.
	terminator := slices.Index(args, "--")
	if terminator < 0 {
		t.Fatalf("args = %v, want a -- terminator before the message", args)
	}
	if terminator != len(args)-2 || args[len(args)-1] != "-check the diff" {
		t.Fatalf("args = %v, want the message last, immediately after --", args)
	}
	// And the real parser must accept that shape, yielding the message as the
	// single positional rather than refusing it as an unknown flag.
	fs := flag.NewFlagSet("review request", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var pr int
	fs.IntVar(&pr, "pr", 0, "")
	for _, name := range []string{"repo", "head", "branch", "role", "reviewer", "lead", "home", "model", "effort", "workflow", "session", "runtime", "purpose"} {
		fs.String(name, "", "")
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("flag.Parse(%v) = %v, want the dash-leading message accepted", args, err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "-check the diff" {
		t.Fatalf("positionals = %v, want exactly the message", fs.Args())
	}
}

// #2199: A BYPASS NOBODY CAN QUERY IS INDISTINGUISHABLE FROM A ROUTER THAT
// SILENTLY DID NOT RUN. Measured 2026-09-17: three dispatches by one seat
// skipped the router on the new build and NOTHING IN THE STORE COULD SAY WHY -
// the reason was printed to stderr and nowhere else, so causes could be
// eliminated but none named. The reason is now a job event.
func TestRouterBypassReasonIsRecordedOnTheJob(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"no acting role", nil, "no --org-role"},
		{"an inexpressible flag", []string{"--org-role", "joltra", "--skip-native-review-fanout"}, "--skip-native-review-fanout"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("\n[org]\nenforce = \"warn\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n[org.roles.\"joltra\"]\nparent = \"owner\"\nscope = [\"owner/repo\"]\n"); err != nil {
				file.Close()
				t.Fatal(err)
			}
			file.Close()
			store := openCLIJobStore(t, home)
			defer store.Close()
			checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
			seedReviewDispatchFixture(t, store, checkout)
			seedDaemonWorkerAgentWithPolicy(t, store, "run-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
			replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
				return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
			})
			installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
			previousGitHubFactory := newAgentDispatchGitHubClient
			newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
			t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

			args := append([]string{
				"reviewer", "Review this.", "--repo", "owner/repo", "--pr", "12",
				"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
				"--home", home,
			}, testCase.extra...)
			var stdout, stderr bytes.Buffer
			if code := runAgentReview(args, &stdout, &stderr); code != 0 {
				t.Fatalf("review exit stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			jobs, err := store.ListJobs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 {
				t.Fatalf("jobs = %+v, want one", jobs)
			}
			events, err := store.ListJobEvents(context.Background(), jobs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			var recorded string
			for _, event := range events {
				if event.Kind == "router_bypassed" {
					recorded = event.Message
				}
			}
			if recorded == "" {
				t.Fatalf("no router_bypassed event on the job: the reason exists only on stderr, which is what this fixes")
			}
			if !strings.Contains(recorded, testCase.want) {
				t.Fatalf("router_bypassed = %q, want it to name %q", recorded, testCase.want)
			}
		})
	}
}

// #2199 review: the false-positive direction I asked for and then did not
// assert. A dispatch that DID route must record no bypass reason - recording one
// would be the same false-evidence shape #2196 round 5 shipped and had to fix.
// Also covers --foreground, the third reason, which the first test table omitted.
func TestRoutedDispatchRecordsNoBypassAndForegroundRecordsOne(t *testing.T) {
	setup := func(t *testing.T) (string, *db.Store, string) {
		t.Helper()
		home := t.TempDir()
		paths := config.PathsForHome(home)
		if err := config.Initialize(paths); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("\n[org]\nenforce = \"warn\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n[org.roles.\"joltra\"]\nparent = \"owner\"\nscope = [\"owner/repo\"]\n"); err != nil {
			file.Close()
			t.Fatal(err)
		}
		file.Close()
		store := openCLIJobStore(t, home)
		checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
		seedReviewDispatchFixture(t, store, checkout)
		seedDaemonWorkerAgentWithPolicy(t, store, "run-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
		replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
			return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
		})
		installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[],"delegations":[]}}`)
		previousGitHubFactory := newAgentDispatchGitHubClient
		newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
		t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })
		return home, store, head
	}
	bypassEvents := func(t *testing.T, store *db.Store) []string {
		t.Helper()
		jobs, err := store.ListJobs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, job := range jobs {
			events, err := store.ListJobEvents(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == "router_bypassed" {
					found = append(found, event.Message)
				}
			}
		}
		return found
	}

	t.Run("a routed dispatch records none", func(t *testing.T) {
		home, store, head := setup(t)
		defer store.Close()
		var stdout, stderr bytes.Buffer
		if code := runAgentReview([]string{
			"reviewer", "Review this.", "--repo", "owner/repo", "--pr", "12",
			"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
			"--org-role", "joltra", "--home", home,
		}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		if got := bypassEvents(t, store); len(got) != 0 {
			t.Fatalf("routed dispatch recorded a bypass reason %v: that is false evidence, the shape this instrument exists to avoid producing", got)
		}
	})

	t.Run("--foreground records its reason", func(t *testing.T) {
		home, store, head := setup(t)
		defer store.Close()
		var stdout, stderr bytes.Buffer
		if code := runAgentReview([]string{
			"reviewer", "Review this.", "--repo", "owner/repo", "--pr", "12",
			"--head-sha", head, "--branch", "feature/review", "--lead", "implementer",
			"--org-role", "joltra", "--foreground", "--home", home,
		}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		got := bypassEvents(t, store)
		if len(got) == 0 {
			t.Fatal("a --foreground dispatch recorded no reason: it prints nothing to stderr either, so the bypass would be invisible")
		}
		if !strings.Contains(got[0], "--foreground") {
			t.Fatalf("recorded %q, want the --foreground reason named", got[0])
		}
	})
}
