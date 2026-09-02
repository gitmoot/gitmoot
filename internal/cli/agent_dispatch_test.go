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
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/githubtest"
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

	family, resolved, err := workflow.ResolveRuntimeFamily(ctx, fixture.store, "ghost-reviewer", "")
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
func TestReviewDispatchRoutesChangesRequestedFixToTaskImplementer(t *testing.T) {
	for _, surface := range []string{"review", "run"} {
		t.Run(surface, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			defer store.Close()
			if err := store.SetPullRequestAutoFixPolicy(
				context.Background(),
				"owner/repo",
				7,
				false,
				"test",
				"explicit autonomous-chain opt-in",
			); err != nil {
				t.Fatalf("SetPullRequestAutoFixPolicy returned error: %v", err)
			}
			checkout := createDaemonWorkerGitCheckout(t, "main")
			makeReviewFixOriginFetchable(t, checkout, "feature/review")
			seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
			seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
			seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
			seedDaemonWorkerAgentWithPolicy(t, store, "payload-default", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
			if err := store.UpsertTask(context.Background(), db.Task{
				ID: "owned-task", RepoFullName: "owner/repo", Branch: "feature/review", State: string(workflow.TaskReviewing),
			}); err != nil {
				t.Fatalf("UpsertTask returned error: %v", err)
			}
			originalPayload, err := json.Marshal(workflow.JobPayload{
				Repo: "owner/repo", Branch: "feature/review", PullRequest: 7, TaskID: "owned-task",
				Result: &workflow.AgentResult{Decision: "implemented"},
			})
			if err != nil {
				t.Fatalf("marshal original implement payload: %v", err)
			}
			if err := store.CreateJob(context.Background(), db.Job{
				ID: "original-implement", Agent: "implementer", Type: "implement",
				State: string(workflow.JobSucceeded), Payload: string(originalPayload),
			}); err != nil {
				t.Fatalf("CreateJob(original-implement): %v", err)
			}
			head, err := (gitutil.NewHostClient(checkout)).HeadSHA(context.Background())
			if err != nil {
				t.Fatalf("HeadSHA returned error: %v", err)
			}
			adapter := installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"changes_requested","severity":"P1","summary":"fix the edge case","findings":[{"severity":"P1","description":"edge case"}],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`)
			previousGitHubFactory := newAgentDispatchGitHubClient
			newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
			t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

			args := []string{
				"reviewer", "Review this PR.", "--repo", "owner/repo", "--pr", "7",
				"--head-sha", head, "--branch", "feature/review", "--lead", "payload-default", "--home", home,
			}
			var stdout, stderr bytes.Buffer
			var exit int
			if surface == "review" {
				exit = runAgentReview(args, &stdout, &stderr)
			} else {
				args = append(args, "--action", "review")
				exit = runAgentRun(args, &stdout, &stderr)
			}
			if exit != 0 {
				t.Fatalf("%s exit=%d stderr=%s stdout=%s", surface, exit, stderr.String(), stdout.String())
			}
			if adapter.calls != 1 {
				t.Fatalf("adapter calls = %d, want one review delivery", adapter.calls)
			}
			jobs, err := store.ListJobs(context.Background())
			if err != nil {
				t.Fatalf("ListJobs returned error: %v", err)
			}
			if len(jobs) != 3 {
				t.Fatalf("jobs = %+v, want original implement, review, and fix jobs", jobs)
			}
			var reviewJob, fixJob *db.Job
			for index := range jobs {
				switch jobs[index].Type {
				case "review":
					reviewJob = &jobs[index]
				case "implement":
					if jobs[index].ID != "original-implement" {
						fixJob = &jobs[index]
					}
				}
			}
			if reviewJob == nil || reviewJob.Agent != "reviewer" {
				t.Fatalf("review job = %+v, want reviewer", reviewJob)
			}
			if fixJob == nil || fixJob.Agent != "implementer" {
				t.Fatalf("fix job = %+v, want implementer", fixJob)
			}
			payload, err := daemonJobPayload(*fixJob)
			if err != nil {
				t.Fatalf("daemonJobPayload returned error: %v", err)
			}
			if payload.LeadAgent != "implementer" {
				t.Fatalf("fix payload LeadAgent = %q, want implementer", payload.LeadAgent)
			}
		})
	}
}

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
