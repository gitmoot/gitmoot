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

// PRE-FIX RED at the production CLI parser. This kills parsed-but-unthreaded
// --lead, reversed firstNonEmpty precedence, and fix routing back to the reviewer.
func TestReviewDispatchLeadRoutesChangesRequestedFixToImplementer(t *testing.T) {
	for _, surface := range []string{"review", "run"} {
		t.Run(surface, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			defer store.Close()
			checkout := createDaemonWorkerGitCheckout(t, "main")
			seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
			seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
			seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
			head, err := (gitutil.Client{Dir: checkout}).HeadSHA(context.Background())
			if err != nil {
				t.Fatalf("HeadSHA returned error: %v", err)
			}
			adapter := installReviewLeadTestAdapter(t, `{"gitmoot_result":{"decision":"changes_requested","summary":"fix the edge case","findings":[{"severity":"high","description":"edge case"}],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`)
			previousGitHubFactory := newAgentDispatchGitHubClient
			newAgentDispatchGitHubClient = func(string) github.Client { return github.NoopClient{} }
			t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

			args := []string{
				"reviewer", "Review this PR.", "--repo", "owner/repo", "--pr", "7",
				"--head-sha", head, "--branch", "feature/review", "--lead", "implementer", "--home", home,
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
			if len(jobs) != 2 {
				t.Fatalf("jobs = %+v, want review plus fix job", jobs)
			}
			var reviewJob, fixJob *db.Job
			for index := range jobs {
				switch jobs[index].Type {
				case "review":
					reviewJob = &jobs[index]
				case "implement":
					fixJob = &jobs[index]
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
	head, err := (gitutil.Client{Dir: checkout}).HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	return reviewLeadRefusalFixture{store: store, checkout: checkout, head: head, home: t.TempDir()}
}

func installReviewLeadTestAdapter(t *testing.T, output string) *cliWorkerFakeAdapter {
	t.Helper()
	adapter := &cliWorkerFakeAdapter{output: output}
	previous := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, string, string) (runtime.Adapter, error) {
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
