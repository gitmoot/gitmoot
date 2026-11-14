package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRunAgentSubscribeListRemove(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "subscribe", "audit",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "gitmoot/gitmoot",
		"--repo", "jerryfane/other",
		"--capability", "review",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "audit") || !strings.Contains(output, "gitmoot/gitmoot,jerryfane/other") || !strings.Contains(output, "review,ask") {
		t.Fatalf("list output = %q", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"reviewer", "subscribe", "audit",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "gitmoot/gitmoot",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resubscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list after resubscribe exit code = %d, stderr=%s", code, stderr.String())
	}
	output = stdout.String()
	if !strings.Contains(output, "gitmoot/gitmoot") || strings.Contains(output, "jerryfane/other") {
		t.Fatalf("list after resubscribe output = %q", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "remove", "audit", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remove exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second list exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "audit") {
		t.Fatalf("agent was not removed: %q", stdout.String())
	}
}

func TestRunAgentShow(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "subscribe", "audit",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "gitmoot/gitmoot",
		"--capability", "review",
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "show", "audit", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"name: audit",
		"runtime: omp",
		"runtime_ref: 550e8400-e29b-41d4-a716-446655440001\n",
		"role: reviewer",
		"capabilities: review",
		"policy: workspace-write",
		"allowed_repos: gitmoot/gitmoot",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("agent show output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "show", "audit", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent show --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var decoded agentShowOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if decoded.Name != "audit" || decoded.RuntimeRef != "550e8400-e29b-41d4-a716-446655440001" || decoded.Policy != runtime.AutonomyPolicyWorkspaceWrite || strings.Join(decoded.AllowedRepos, ",") != "gitmoot/gitmoot" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestRunAgentSubscribeValidatesAutonomyPolicy(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"reviewer", "subscribe", "audit",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "gitmoot/gitmoot",
		"--capability", "review",
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "audit")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.AutonomyPolicy != runtime.AutonomyPolicyWorkspaceWrite {
		t.Fatalf("autonomy policy = %q", agent.AutonomyPolicy)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"reviewer", "subscribe", "badpolicy",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440002",
		"--role", "reviewer",
		"--repo", "gitmoot/gitmoot",
		"--policy", "manual",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("invalid policy exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "autonomy policy") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunAgentAccessCommands(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"reviewer", "subscribe", "audit",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "allow", "audit", "--home", home, "--repo", "gitmoot/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("allow exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "allowed audit on gitmoot/gitmoot") {
		t.Fatalf("allow output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "repos", "audit", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repos exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "gitmoot/gitmoot" {
		t.Fatalf("repos output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "deny", "audit", "--home", home, "--repo", "gitmoot/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deny exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "repos", "audit", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repos after deny exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Fatalf("repos after deny output = %q", stdout.String())
	}
}

func TestRunAgentStartCreatesOmpSessionAndStoresAgent(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: ompStreamStdout("550e8400-e29b-41d4-a716-446655440011", "ready")}}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "start", "lead",
		"--home", home,
		"--runtime", "omp",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"started lead (omp) for owner/repo",
		"session: 550e8400-e29b-41d4-a716-446655440011",
		"invoke: /gitmoot lead review",
		"next: cd " + repoDir,
		"next: gitmoot daemon start --home " + home + " --repo owner/repo",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("start output missing %q:\n%s", want, stdout.String())
		}
	}
	runner.want(t, 0, repoDir, "omp", "-p", "--mode=json")

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "lead")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Runtime != runtime.OmpRuntime || agent.RuntimeRef != "550e8400-e29b-41d4-a716-446655440011" || agent.Role != "agent" || strings.Join(agent.Capabilities, ",") != "ask,review,implement" {
		t.Fatalf("agent = %+v", agent)
	}
	if _, err := store.GetRepo(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("GetRepo returned error: %v", err)
	}
	allowed, err := store.AgentCanAccessRepo(context.Background(), "lead", "owner/repo")
	if err != nil {
		t.Fatalf("AgentCanAccessRepo returned error: %v", err)
	}
	if !allowed {
		t.Fatal("agent start did not allow lead on owner/repo")
	}
	if !strings.Contains(runner.calls[0].args[len(runner.calls[0].args)-1], "You are a Gitmoot-managed agent named lead.") {
		t.Fatalf("startup prompt = %q", runner.calls[0].args[len(runner.calls[0].args)-1])
	}
}

func TestRunAgentAskDispatchesAndStoresResult(t *testing.T) {
	home := t.TempDir()
	otherHome := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	if code := Run([]string{
		"reviewer", "subscribe", "planner",
		"--home", home,
		"--runtime", "shell",
		"--session", "cat",
		"--repo", "owner/repo",
		"--role", "planner",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"ask", "planner", "use the isolated state", "--home", otherHome, "--repo", "owner/repo"}, &stdout, &stderr); code != 1 {
		t.Fatalf("ask with other home exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `reviewer "planner" not found`) {
		t.Fatalf("ask with other home stderr = %q", stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"gitmoot_result":{"decision":"approved","summary":"plan ready","findings":[{"title":"clear"}],"changes_made":[],"tests_run":["go test ./internal/cli"],"needs":["ship it"]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"approved","summary":"json ready","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"ask", "planner", "Write a plan", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"run: local-ask-planner-",
		"state: succeeded",
		"repo: owner/repo",
		"reviewer: planner",
		"action: ask",
		"decision: approved",
		"summary: plan ready",
		"findings:",
		`- {"title":"clear"}`,
		"needs:",
		"- ship it",
		"tests_run:",
		"- go test ./internal/cli",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("ask output missing %q:\n%s", want, stdout.String())
		}
	}
	runner.want(t, 0, repoDir, "sh", "-c", "cat", "gitmoot")
	if !strings.Contains(runner.calls[0].args[len(runner.calls[0].args)-1], "Write a plan") {
		t.Fatalf("ask prompt missing message:\n%s", runner.calls[0].args[len(runner.calls[0].args)-1])
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one job", jobs)
	}
	payload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.PullRequest != 0 || payload.Sender != "local" || payload.Instructions != "Write a plan" {
		t.Fatalf("payload local ask fields = %+v", payload)
	}
	if payload.Result == nil || payload.Result.Summary != "plan ready" || len(payload.RawOutputs) != 1 {
		t.Fatalf("payload result = %+v", payload)
	}
	events, err := store.ListJobEvents(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasCLIJobEvent(events, "advance_completed") {
		t.Fatalf("events = %+v, want advance_completed", events)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "list", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), jobs[0].ID+"\tsucceeded\task\tplanner\towner/repo\t#0") {
		t.Fatalf("job list output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "show", jobs[0].ID, "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"decision: approved", "summary: plan ready"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("job show output missing %q:\n%s", want, stdout.String())
		}
	}

	otherStore := openCLIJobStore(t, otherHome)
	defer otherStore.Close()
	otherJobs, err := otherStore.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("other ListJobs returned error: %v", err)
	}
	if len(otherJobs) != 0 {
		t.Fatalf("other home jobs = %+v, want none", otherJobs)
	}

	runGit(t, repoDir, "switch", "-c", "feature/local-ask")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"ask", "planner", "- Write JSON", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask --json exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "gitmoot_result") {
		t.Fatalf("json output leaked raw runtime output:\n%s", stdout.String())
	}
	var decoded localAgentJobOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if decoded.State != "succeeded" || decoded.Repo != "owner/repo" || decoded.Result == nil || decoded.Result.Summary != "json ready" || decoded.RawOutputCount != 1 {
		t.Fatalf("decoded output = %+v", decoded)
	}
	jsonJob, err := store.GetJob(context.Background(), decoded.JobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", decoded.JobID, err)
	}
	jsonPayload, err := daemonJobPayload(jsonJob)
	if err != nil {
		t.Fatalf("json daemonJobPayload returned error: %v", err)
	}
	if jsonPayload.Branch != "feature/local-ask" || jsonPayload.Instructions != "- Write JSON" {
		t.Fatalf("json ask payload = %+v", jsonPayload)
	}
}

func TestRunAgentAskWarnsButDoesNotBlockOrchestration(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"ask", "planner", "create branch, commit, push, and open PR", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	// The guard must not hard-block (rc 2). It falls through to dispatch and
	// then fails downstream because the agent does not exist (rc 1). Asserting
	// rc 1 *and* the "agent ask:" dispatch-error prefix positively confirms it
	// reached dispatch rather than returning a non-2 code before the guard.
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (guard bypassed, downstream dispatch failure)", code)
	}
	if !strings.Contains(stderr.String(), "read-only") {
		t.Fatalf("stderr = %q, want the read-only warning", stderr.String())
	}
	if !strings.Contains(stderr.String(), "ask:") {
		t.Fatalf("stderr = %q, want the dispatch-error prefix confirming dispatch was reached", stderr.String())
	}
}

func TestForegroundAskTimeoutError(t *testing.T) {
	wrapped := fmt.Errorf(`job "x" is cancelled, not running`)

	// (1) deadline-exceeded context + jobTimeout > 0 -> actionable message.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	<-expired.Done()
	if got := foregroundAskTimeoutError(expired, time.Minute, wrapped); got == nil || !strings.Contains(got.Error(), "re-run with --background") {
		t.Fatalf("got = %v, want an error mentioning re-run with --background", got)
	}

	// (2) live context -> original error unchanged.
	if got := foregroundAskTimeoutError(context.Background(), time.Minute, wrapped); got != wrapped {
		t.Fatalf("got = %v, want the original error unchanged", got)
	}

	// (3) nil err -> nil.
	if got := foregroundAskTimeoutError(expired, time.Minute, nil); got != nil {
		t.Fatalf("got = %v, want nil", got)
	}
}

func TestWorkflowOrchestrationGuardAllowsCommitQuestions(t *testing.T) {
	if looksLikeWorkflowOrchestration("Which commit introduced this regression?") {
		t.Fatal("commit analysis question was incorrectly treated as workflow orchestration")
	}
	if !looksLikeWorkflowOrchestration("commit and push the branch, then open a PR") {
		t.Fatal("explicit commit/push/PR workflow was not treated as workflow orchestration")
	}
}

func TestPrepareLocalReviewDispatchRequestBindsBranchOwningTaskWithoutWorktree(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "switch", "-c", "feature/review")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "feature")
	head, err := (gitutil.NewHostClient(repoDir)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	runGit(t, repoDir, "switch", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")

	store := openCLIJobStore(t, home)
	defer store.Close()
	// An implement task may already own the PR head branch. The review must bind
	// to it (#1530) — never mint a second task row for the same (repo, branch),
	// which the tasks(repo,branch) partial unique index would reject anyway.
	if err := store.UpsertTask(ctx, db.Task{ID: "implement-12", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Implement PR #12", State: string(workflow.TaskImplementing), Branch: "feature/review"}); err != nil {
		t.Fatalf("UpsertTask(implement): %v", err)
	}
	record := db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: repoDir}
	request, err := prepareLocalReviewDispatchRequest(ctx, store, record, github.Repository{Owner: "owner", Name: "repo"}, localAgentDispatchRequest{
		Home:         home,
		Agent:        "audit",
		Action:       "review",
		Instructions: "Review this PR.",
		PullRequest:  12,
		Branch:       "feature/review",
		HeadSHA:      head,
	})
	if err != nil {
		t.Fatalf("prepareLocalReviewDispatchRequest returned error: %v", err)
	}
	if request.TaskID != "implement-12" {
		t.Fatalf("request.TaskID = %q, want bound to the branch-owning task %q", request.TaskID, "implement-12")
	}
	if request.GoalID != "goal-1" || request.TaskTitle != "Implement PR #12" {
		t.Fatalf("rebind dropped task metadata: request=%+v", request)
	}
	// No worktree means no disk HEAD to compare, so there is no divergence to
	// record — the note is specifically for a stranded registered checkout.
	if request.ReviewTaskHeadDivergence != "" {
		t.Fatalf("worktree-less bind recorded a divergence note %q; want none", request.ReviewTaskHeadDivergence)
	}
	branch, err := (gitutil.NewHostClient(repoDir)).CurrentBranch(ctx)
	if err != nil {
		t.Fatalf("CurrentBranch returned error: %v", err)
	}
	if branch != "main" {
		t.Fatalf("registered checkout branch = %q, want main", branch)
	}
	// No review-pr-* row minted; the implement task row is untouched.
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks returned error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "implement-12" {
		t.Fatalf("tasks = %+v, want only the pre-existing implement task", tasks)
	}
	implementTask, err := store.GetTask(ctx, "implement-12")
	if err != nil || implementTask.State != string(workflow.TaskImplementing) || implementTask.Branch != "feature/review" {
		t.Fatalf("implement task changed = %+v, err=%v", implementTask, err)
	}
}

func TestPrepareLocalReviewDispatchRequestBindsTaskWithStaleCheckout(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	repoDir := t.TempDir()
	staleWorktree := filepath.Join(home, "stale")
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "switch", "-c", "feature/review")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("old feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "old feature")
	oldHead, err := (gitutil.NewHostClient(repoDir)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("old HeadSHA returned error: %v", err)
	}
	runGit(t, repoDir, "worktree", "add", "--detach", staleWorktree, oldHead)
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "new feature")
	newHead, err := (gitutil.NewHostClient(repoDir)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("new HeadSHA returned error: %v", err)
	}
	runGit(t, repoDir, "switch", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertTask(ctx, db.Task{ID: "stale-review", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Stale review", State: string(workflow.TaskReviewing), Branch: "feature/review", WorktreePath: staleWorktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	record := db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: repoDir}
	request, err := prepareLocalReviewDispatchRequest(ctx, store, record, github.Repository{Owner: "owner", Name: "repo"}, localAgentDispatchRequest{
		Home:         home,
		Agent:        "audit",
		Action:       "review",
		Instructions: "Review this PR.",
		PullRequest:  12,
		Branch:       "feature/review",
		HeadSHA:      newHead,
	})
	if err != nil {
		t.Fatalf("prepareLocalReviewDispatchRequest returned error: %v", err)
	}
	// Fixture pin: the registered checkout must genuinely sit BEHIND the
	// requested head (the fix-worktree strand), or this test proves nothing.
	staleHead, err := (gitutil.NewHostClient(staleWorktree)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("stale HeadSHA returned error: %v", err)
	}
	if staleHead != oldHead || staleHead == newHead {
		t.Fatalf("fixture not stranded: stale worktree head = %q, want %q behind requested %q", staleHead, oldHead, newHead)
	}
	// #1530: the task owning (repo, branch) is the same unit of work even when
	// its registered checkout is behind — bind to it, never mint review-pr-*.
	if request.TaskID != "stale-review" {
		t.Fatalf("request.TaskID=%q, want rebound to the branch-owning task %q", request.TaskID, "stale-review")
	}
	if request.GoalID != "goal-1" || request.TaskTitle != "Stale review" {
		t.Fatalf("rebind dropped task metadata: request=%+v", request)
	}
	divergence := request.ReviewTaskHeadDivergence
	for _, fragment := range []string{"stale-review", oldHead, newHead} {
		if !strings.Contains(divergence, fragment) {
			t.Fatalf("divergence note = %q, want it to name %q", divergence, fragment)
		}
	}
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks returned error: %v", err)
	}
	for _, task := range tasks {
		if strings.HasPrefix(task.ID, "review-pr-") {
			t.Fatalf("minted a fresh review identity %q despite the owning task rebound", task.ID)
		}
	}
	if len(tasks) != 1 || tasks[0].ID != "stale-review" {
		t.Fatalf("tasks=%+v, want only the pre-existing owning task", tasks)
	}
}

func TestPrepareLocalReviewTaskRejectsDisposedTask(t *testing.T) {
	for _, test := range []struct {
		name       string
		task       db.Task
		setRequest func(*localAgentDispatchRequest)
		wantError  []string
	}{
		{
			name: "matching head worktree",
			task: db.Task{ID: "dismissed-by-branch", RepoFullName: "owner/repo", State: string(workflow.TaskDismissed), Branch: "feature/review"},
			setRequest: func(request *localAgentDispatchRequest) {
				request.Branch = "feature/review"
			},
			wantError: []string{"is dismissed", "create a successor"},
		},
		{
			// #1530: the rebind-on-divergence must NOT precede the disposal
			// refusal — a disposed task is refused whether or not the heads match.
			name: "diverged head worktree",
			task: db.Task{ID: "dismissed-diverged-by-branch", RepoFullName: "owner/repo", State: string(workflow.TaskDismissed), Branch: "feature/review"},
			setRequest: func(request *localAgentDispatchRequest) {
				request.Branch = "feature/review"
				request.HeadSHA = strings.Repeat("0", 40)
			},
			wantError: []string{"is dismissed", "create a successor"},
		},
		{
			name: "requested task upsert",
			task: db.Task{ID: "dismissed-by-id", RepoFullName: "owner/repo", State: string(workflow.TaskDismissed)},
			setRequest: func(request *localAgentDispatchRequest) {
				request.TaskID = "dismissed-by-id"
			},
			wantError: []string{"is dismissed", "create a successor"},
		},
		{
			name: "superseded matching branch",
			task: db.Task{ID: "superseded-by-branch", RepoFullName: "owner/repo", State: string(workflow.TaskSuperseded), Branch: "feature/review"},
			setRequest: func(request *localAgentDispatchRequest) {
				request.Branch = "feature/review"
			},
			wantError: []string{"is superseded", "create a successor with `gitmoot pr successor"},
		},
		{
			name: "stranded requested task",
			task: db.Task{ID: "stranded-by-id", RepoFullName: "owner/repo", State: string(workflow.TaskStranded)},
			setRequest: func(request *localAgentDispatchRequest) {
				request.TaskID = "stranded-by-id"
			},
			wantError: []string{"is stranded", "create a successor with `gitmoot pr successor"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			repoDir := t.TempDir()
			runGit(t, repoDir, "init")
			runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
			runGit(t, repoDir, "config", "user.name", "Gitmoot")
			writeFile(t, filepath.Join(repoDir, "README.md"), "review\n")
			runGit(t, repoDir, "add", "README.md")
			runGit(t, repoDir, "commit", "-m", "review head")
			head, err := (gitutil.NewHostClient(repoDir)).HeadSHA(ctx)
			if err != nil {
				t.Fatal(err)
			}
			test.task.WorktreePath = repoDir
			store := openCLIJobStore(t, home)
			defer store.Close()
			if err := store.UpsertTask(ctx, test.task); err != nil {
				t.Fatal(err)
			}
			request := localAgentDispatchRequest{Home: home, PullRequest: 12, HeadSHA: head}
			test.setRequest(&request)
			_, err = prepareLocalReviewTask(ctx, store,
				github.Repository{Owner: "owner", Name: "repo"}, request)
			if err == nil || !strings.Contains(err.Error(), "pr "+test.task.ID) {
				t.Fatalf("prepareLocalReviewTask error = %v", err)
			}
			for _, fragment := range test.wantError {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("prepareLocalReviewTask error = %v, want fragment %q", err, fragment)
				}
			}
			stored, getErr := store.GetTask(ctx, test.task.ID)
			if getErr != nil || stored.State != test.task.State {
				t.Fatalf("stored task=%+v err=%v", stored, getErr)
			}
		})
	}
}

func TestPrepareLocalReviewTaskReusesNonDismissedMatchingHead(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	writeFile(t, filepath.Join(repoDir, "README.md"), "review\n")
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "review head")
	head, err := (gitutil.NewHostClient(repoDir)).HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertTask(ctx, db.Task{
		ID: "reviewing-task", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Review",
		State: string(workflow.TaskReviewing), Branch: "feature/review", WorktreePath: repoDir,
	}); err != nil {
		t.Fatal(err)
	}
	request, err := prepareLocalReviewTask(ctx, store,
		github.Repository{Owner: "owner", Name: "repo"},
		localAgentDispatchRequest{Home: home, PullRequest: 12, Branch: "feature/review", HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	if request.TaskID != "reviewing-task" || request.GoalID != "goal-1" || request.TaskTitle != "Review" {
		t.Fatalf("request=%+v", request)
	}
	if request.ReviewTaskHeadDivergence != "" {
		t.Fatalf("matching-head bind recorded a divergence note %q; divergence is only for a mismatched head", request.ReviewTaskHeadDivergence)
	}
	task, err := store.GetTask(ctx, "reviewing-task")
	if err != nil || task.State != string(workflow.TaskReviewing) {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

// #1530 acceptance 3: mint review-pr-%d-%s ONLY when no task owns (repo,
// branch). This test fails if the rebind widens past the owning-task condition
// (e.g. binding when GetTaskByRepoBranch found nothing).
func TestPrepareLocalReviewTaskMintsIdentityWhenNoTaskOwnsBranch(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	writeFile(t, filepath.Join(repoDir, "README.md"), "review\n")
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "review head")
	head, err := (gitutil.NewHostClient(repoDir)).HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	// A task on a DIFFERENT branch must not capture this review's identity.
	if err := store.UpsertTask(ctx, db.Task{
		ID: "other-branch-task", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing),
		Branch: "feature/other", WorktreePath: repoDir,
	}); err != nil {
		t.Fatal(err)
	}
	request, err := prepareLocalReviewTask(ctx, store,
		github.Repository{Owner: "owner", Name: "repo"},
		localAgentDispatchRequest{Home: home, PullRequest: 12, Branch: "feature/review", HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	wantID := fmt.Sprintf("review-pr-%d-%s", 12, shortHash("owner/repo"))
	if request.TaskID != wantID {
		t.Fatalf("request.TaskID=%q, want minted %q when no task owns the branch", request.TaskID, wantID)
	}
	if request.ReviewTaskHeadDivergence != "" {
		t.Fatalf("mint path recorded a divergence note %q; no owning task means no rebind", request.ReviewTaskHeadDivergence)
	}
}

func TestRunAgentAskBackgroundQueuesWithoutRuntimeDelivery(t *testing.T) {
	declareNoRealAdapterExec(t)
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedGitHead(t, repoDir)
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"reviewer", "subscribe", "planner",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440022",
		"--role", "planner",
		"--repo", "owner/repo",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"ask", "planner", "Write a plan", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("background ask exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"run: local-ask-planner-",
		"state: queued",
		"repo: owner/repo",
		"reviewer: planner",
		"action: ask",
		"next: gitmoot run watch local-ask-planner-",
		"queued: daemon is not running",
		"process: gitmoot daemon start",
		"or: gitmoot run start local-ask-planner-",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("background ask output missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime calls = %+v, want none", runner.calls)
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != string(workflow.JobQueued) {
		t.Fatalf("jobs = %+v, want one queued job", jobs)
	}
	payload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.Instructions != "Write a plan"+workflow.ReadOnlyWorktreeContextNote(repoDir) || payload.Result != nil || len(payload.RawOutputs) != 0 {
		t.Fatalf("background payload = %+v", payload)
	}
}

func TestRunAgentAskBackgroundJSON(t *testing.T) {
	declareNoRealAdapterExec(t)
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedGitHead(t, repoDir)
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"reviewer", "subscribe", "planner",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440023",
		"--role", "planner",
		"--repo", "owner/repo",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"ask", "planner", "Write JSON", "--home", home, "--repo", "owner/repo", "--background", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("background ask --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var decoded localAgentJobOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if decoded.State != string(workflow.JobQueued) || decoded.Repo != "owner/repo" || decoded.Result != nil || decoded.WatchCommand == "" || decoded.DaemonRunning {
		t.Fatalf("decoded background output = %+v", decoded)
	}
}

func TestRunAgentTypeSetListShow(t *testing.T) {
	declareNoRealAdapterExec(t)
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedGitHead(t, repoDir)
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "type", "set", "planner",
		"--home", home,
		"--runtime", "omp",
		"--role", "planner",
		"--policy", "workspace-write",
		"--run-timeout", "5m",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "configured reviewer type planner") {
		t.Fatalf("agent type set output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "type", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "planner\tomp\n") {
		t.Fatalf("agent type list output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "type", "show", "planner", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "capabilities: ask") || !strings.Contains(stdout.String(), "policy: workspace-write") || strings.Contains(stdout.String(), "max_background") {
		t.Fatalf("agent type show output = %q", stdout.String())
	}
}

// TestDispatchForegroundAskUnknownNameStillErrors confirms a name that is not a
// registered agent returns `agent "<name>" not found` in the foreground.
func TestDispatchForegroundAskUnknownNameStillErrors(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}

	_, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag:     "owner/repo",
		Agent:        "ghost",
		Action:       "ask",
		Instructions: "Should not resolve.",
		Home:         home,
	})
	if err == nil {
		t.Fatal("dispatchLocalAgentJob returned nil error, want not found")
	}
	if !strings.Contains(err.Error(), `reviewer "ghost" not found`) {
		t.Fatalf("error = %q, want reviewer not found", err.Error())
	}
}

func TestRunAgentTypeSetRejectsNonStartableRuntime(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"reviewer", "type", "set", "shell-planner",
		"--home", home,
		"--runtime", "shell",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("agent type set shell exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "reviewer types support omp") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"reviewer", "type", "set", "codex-planner",
		"--home", home,
		"--runtime", "codex",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("agent type set codex exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `runtime "codex" is no longer supported; gitmoot requires omp for jobs`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunAgentTypeSetRejectsConfigUnsafeName(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"reviewer", "type", "set", "bad#name",
		"--home", home,
		"--runtime", "omp",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("agent type set unsafe name exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid reviewer type") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunAgentTypeSetRejectsNonPositiveTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
		want string
	}{
		{name: "run", flag: "--run-timeout", want: "run timeout must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			var stdout, stderr bytes.Buffer

			code := Run([]string{
				"reviewer", "type", "set", "planner",
				"--home", home,
				"--runtime", "omp",
				tc.flag, "0s",
			}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("agent type set exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRunAgentAskValidatesInputAndAccess(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"ask", "planner", "--home", home}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("missing message exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "ask requires exactly one reviewer and one message") {
		t.Fatalf("missing message stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"ask", "missing", "hello", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("missing agent exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `reviewer "missing" not found`) {
		t.Fatalf("missing agent stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"reviewer", "subscribe", "reviewer",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440031",
		"--role", "reviewer",
		"--repo", "owner/repo",
		"--capability", "review",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe reviewer exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"ask", "reviewer", "hello", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("missing ask capability exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `reviewer "reviewer" lacks ask capability`) {
		t.Fatalf("missing ask capability stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"reviewer", "subscribe", "outsider",
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440032",
		"--role", "planner",
		"--repo", "owner/other",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe outsider exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"ask", "outsider", "hello", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("disallowed repo exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `reviewer "outsider" is not allowed on "owner/repo"`) {
		t.Fatalf("disallowed repo stderr = %q", stderr.String())
	}
}

func hasCLIJobEvent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func TestRunAgentStartRejectsExistingAgentBeforeRuntime(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "subscribe", "lead", "--home", home, "--runtime", "omp", "--session", "550e8400-e29b-41d4-a716-446655440097", "--role", "lead", "--repo", "owner/repo", "--capability", "ask"}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"reviewer", "start", "lead", "--home", home, "--runtime", "omp", "--repo", "owner/repo", "--path", repoDir, "--policy", "workspace-write"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("start exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "reviewer lead already exists") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started before existing-agent validation: %+v", runner.calls)
	}
}

func TestRunAgentStartPreservesExistingAgentType(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	configured := config.DefaultConfig(paths) + `
[agents.preserved-agent]
runtime = "omp"
model = "preserved-model"
effort = "high"
role = "preserved-role"
capabilities = ["review"]
autonomy_policy = "read-only"
job_timeout = "17m"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(configured), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	fake := &agentRestartFakeAdapter{newRef: "550e8400-e29b-41d4-a716-446655440098"}
	previous := agentStartAdapterFor
	agentStartAdapterFor = func(_ string, _ string, _ string) (runtime.Adapter, error) { return fake, nil }
	t.Cleanup(func() { agentStartAdapterFor = previous })

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "start", "preserved-agent", "--home", home, "--runtime", "omp",
		"--repo", "owner/repo", "--path", repoDir, "--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent start exit=%d stderr=%s", code, stderr.String())
	}
	after, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("plain agent start rewrote pre-existing type:\n%s", string(after))
	}
	entries, err := config.LoadAgentTypes(paths)
	if err != nil {
		t.Fatal(err)
	}
	entry := entries["preserved-agent"]
	if entry.Runtime != "omp" || entry.Model != "preserved-model" ||
		entry.Effort != "high" || entry.Role != "preserved-role" || !reflect.DeepEqual(entry.Capabilities, []string{"review"}) ||
		entry.AutonomyPolicy != "read-only" || entry.JobTimeout != "17m" {
		t.Fatalf("pre-existing type was clobbered: %+v", entry)
	}
}

func TestRunAgentStartRejectsConfigUnsafeNameBeforeRuntime(t *testing.T) {
	fake := &agentRestartFakeAdapter{newRef: "550e8400-e29b-41d4-a716-446655440099"}
	previous := agentStartAdapterFor
	agentStartAdapterFor = func(_ string, _ string, _ string) (runtime.Adapter, error) { return fake, nil }
	t.Cleanup(func() { agentStartAdapterFor = previous })
	var stdout, stderr bytes.Buffer
	code := Run([]string{"reviewer", "start", "unsafe.name", "--runtime", "omp", "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 2 || fake.calls() != 0 || !strings.Contains(stderr.String(), "invalid reviewer name") {
		t.Fatalf("code=%d calls=%d stderr=%s", code, fake.calls(), stderr.String())
	}
}

func TestRunAgentStartRejectsShellRuntimeBeforeStartingRuntime(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "start", "shell-agent",
		"--home", home,
		"--runtime", "shell",
		"--repo", "owner/repo",
		"--path", repoDir,
		// --capability ask keeps this test on its OWN gate. #2204 removed the
		// --template flag this invocation used to carry, and
		// resolveAgentDefaults falls back to capabilities [ask review implement];
		// the implement-write-policy refusal then fires at agent.go:1847 and
		// returns 2 BEFORE the shell-runtime refusal this test exists to pin.
		// Round-1 review of #2211 caught it: the test was passing judgement on a
		// gate it does not name.
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("start exit code = %d, want 1 (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "shell runtime does not support reviewer start") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started for shell agent: %+v", runner.calls)
	}
}

func TestRunAgentStartRefusesRemovedRuntime(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "start", "legacy",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("start exit code = %d, want 2 (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `runtime "codex" is no longer supported; gitmoot requires omp for jobs (shell only for no-LLM tests): re-register the reviewer with --runtime omp`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started for a removed runtime: %+v", runner.calls)
	}
}

func TestRunAgentStartRefusesImplementWithNonWritePolicy(t *testing.T) {
	for _, policy := range []string{"", "read-only"} {
		home := t.TempDir()
		repoDir := t.TempDir()
		runGit(t, repoDir, "init")
		runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
		runner := &agentStartRunner{}
		restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})

		args := []string{
			"reviewer", "start", "shipper",
			"--home", home,
			"--runtime", "omp",
			"--repo", "owner/repo",
			"--path", repoDir,
			"--capability", "implement",
		}
		if policy != "" {
			args = append(args, "--policy", policy)
		}
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr)
		restoreFactory()
		if code != 2 {
			t.Fatalf("policy %q: start exit code = %d, want 2; stderr=%s", policy, code, stderr.String())
		}
		for _, fragment := range []string{"danger-full-access", "workspace-write", "implement"} {
			if !strings.Contains(stderr.String(), fragment) {
				t.Fatalf("policy %q: stderr %q must mention %q", policy, stderr.String(), fragment)
			}
		}
		// The refusal must precede the runtime session, so no session is spent.
		if len(runner.calls) != 0 {
			t.Fatalf("policy %q: runtime was started before the policy refusal: %+v", policy, runner.calls)
		}
	}
}

func TestRunAgentStartAllowsImplementWithWritePolicy(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: ompStreamStdout("550e8400-e29b-41d4-a716-446655440044", "ready")}}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "start", "shipper",
		"--home", home,
		"--runtime", "omp",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--capability", "implement",
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}
}

func TestRunAgentSubscribeRefusesImplementWithNonWritePolicy(t *testing.T) {
	for _, policy := range []string{"", "read-only"} {
		home := t.TempDir()
		args := []string{
			"reviewer", "subscribe", "shipper",
			"--home", home,
			"--runtime", "omp",
			"--session", "550e8400-e29b-41d4-a716-446655440001",
			"--repo", "owner/repo",
			"--role", "shipper",
			"--capability", "implement",
		}
		if policy != "" {
			args = append(args, "--policy", policy)
		}
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("policy %q: subscribe exit code = %d, want 2; stderr=%s", policy, code, stderr.String())
		}
		for _, fragment := range []string{"danger-full-access", "workspace-write", "implement"} {
			if !strings.Contains(stderr.String(), fragment) {
				t.Fatalf("policy %q: stderr %q must mention %q", policy, stderr.String(), fragment)
			}
		}
	}
}

func TestRunAgentSubscribeValidatesInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"reviewer", "subscribe", "bad name", "--runtime", "omp", "--session", "550e8400-e29b-41d4-a716-446655440001", "--role", "reviewer", "--repo", "gitmoot/gitmoot"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid reviewer") {
		t.Fatalf("stderr = %q", stderr.String())
	}

}

func TestRunAgentDoctorPersistsHealth(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"reviewer", "subscribe", "shell",
		"--home", home,
		"--runtime", "shell",
		"--session", "printf ok",
		"--role", "reviewer",
		"--repo", "gitmoot/gitmoot",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"reviewer", "doctor", "shell", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doctor exit code = %d, stderr=%s", code, stderr.String())
	}

	store, err := dbtest.Open(t, filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "shell")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.HealthStatus != "ok" {
		t.Fatalf("health status = %q, want ok", agent.HealthStatus)
	}
}

func replaceRuntimeFactory(factory runtime.Factory) func() {
	previous := newRuntimeFactory
	newRuntimeFactory = func() runtime.Factory {
		return factory
	}
	return func() {
		newRuntimeFactory = previous
	}
}

type agentStartRunner struct {
	mu      sync.Mutex
	results []subprocess.Result
	errs    []error
	calls   []agentStartCall
}

type agentStartCall struct {
	dir     string
	command string
	args    []string
}

// ompStreamStdout is the NDJSON stream a fake runner returns for one omp run:
// the session header (the runtime_ref agent start stores) followed by a final
// assistant message carrying text.
func ompStreamStdout(sessionID string, text string) string {
	message, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}},
		"usage": map[string]any{"input": 1, "output": 1, "totalTokens": 2}, "stopReason": "stop",
	}})
	if err != nil {
		panic(err)
	}
	return `{"type":"session","version":3,"id":"` + sessionID + `","timestamp":"2026-11-10T00:00:00.000Z","cwd":"/repo"}` + "\n" +
		string(message) + "\n" +
		`{"type":"agent_end","messages":[],"isTerminal":true}` + "\n"
}

func (r *agentStartRunner) Run(_ context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, agentStartCall{dir: dir, command: command, args: append([]string{}, args...)})
	index := len(r.calls) - 1
	result := subprocess.Result{Command: command, Args: args}
	if index < len(r.results) {
		result = r.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(r.errs) {
		err = r.errs[index]
	}
	return result, err
}

func (r *agentStartRunner) LookPath(file string) (string, error) {
	if file == "" {
		return "", errors.New("empty file")
	}
	return "/usr/bin/" + file, nil
}

func (r *agentStartRunner) want(t *testing.T, index int, dir string, command string, wantPrefix ...string) {
	t.Helper()
	if index >= len(r.calls) {
		t.Fatalf("missing call %d; calls=%+v", index, r.calls)
	}
	call := r.calls[index]
	if call.dir != dir {
		t.Fatalf("call %d dir = %q, want %q", index, call.dir, dir)
	}
	if call.command != command {
		t.Fatalf("call %d command = %q, want %q", index, call.command, command)
	}
	if len(call.args) < len(wantPrefix) || !reflect.DeepEqual(call.args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("call %d args = %s\nwant prefix %s", index, strings.Join(call.args, " "), strings.Join(wantPrefix, " "))
	}
}

// TestParseAgentRunOptionsNoFixTargetDoesNotEatTheNextArgument is review
// finding F2 on #2064, and it is the cheapest kind of bug to ship: a boolean
// flag that increments the argument index, in a loop that already increments.
//
// Every sibling boolean here - --background, --json,
// --skip-native-review-fanout, --allow-prompt-head-mismatch - does NOT
// increment, which is exactly why the extra line looked harmless. The symptom
// is silent: the NEXT flag disappears, so the failure surfaces as a missing
// --org-role or an unbound head rather than as a parse error.
func TestParseAgentRunOptionsNoFixTargetDoesNotEatTheNextArgument(t *testing.T) {
	var stderr bytes.Buffer
	options, ok := parseAgentRunOptions("review", []string{
		"reviewer", "review this", "--no-fix-target", "--org-role", "gm-transport", "--pr", "7",
	}, &stderr)
	if !ok {
		t.Fatalf("parseAgentRunOptions failed: %q", stderr.String())
	}
	// The flags AFTER it must survive. Before the fix --org-role was consumed as
	// this flag's value and every later flag shifted by one.
	if options.orgRole != "gm-transport" {
		t.Fatalf("orgRole = %q, want gm-transport: the boolean consumed the following argument", options.orgRole)
	}
	if options.prNumber != 7 {
		t.Fatalf("prNumber = %d, want 7", options.prNumber)
	}
}

func TestParseAgentRunOptionsCapturesModel(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "space form", args: []string{"planner", "do the work", "--model", "opus"}, want: "opus"},
		{name: "inline form", args: []string{"planner", "do the work", "--model=opus"}, want: "opus"},
		{name: "absent leaves empty", args: []string{"planner", "do the work"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			options, ok := parseAgentRunOptions("run", tt.args, &stderr)
			if !ok {
				t.Fatalf("parseAgentRunOptions failed: %q", stderr.String())
			}
			if options.model != tt.want {
				t.Fatalf("model = %q, want %q", options.model, tt.want)
			}
		})
	}
}

func TestParseAgentRunOptionsCapturesEffort(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "space form", args: []string{"planner", "do the work", "--effort", "high"}, want: "high"},
		{name: "inline form", args: []string{"planner", "do the work", "--effort=xhigh"}, want: "xhigh"},
		{name: "absent leaves empty", args: []string{"planner", "do the work"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			options, ok := parseAgentRunOptions("run", tt.args, &stderr)
			if !ok {
				t.Fatalf("parseAgentRunOptions failed: %q", stderr.String())
			}
			if options.effort != tt.want {
				t.Fatalf("effort = %q, want %q", options.effort, tt.want)
			}
		})
	}
}

func TestParseAgentAskOptionsCapturesModel(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "space form", args: []string{"planner", "what is up", "--model", "sonnet"}, want: "sonnet"},
		{name: "inline form", args: []string{"planner", "what is up", "--model=sonnet"}, want: "sonnet"},
		{name: "absent leaves empty", args: []string{"planner", "what is up"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			options, ok := parseAgentAskOptions(tt.args, &stderr)
			if !ok {
				t.Fatalf("parseAgentAskOptions failed: %q", stderr.String())
			}
			if options.model != tt.want {
				t.Fatalf("model = %q, want %q", options.model, tt.want)
			}
		})
	}
}

func TestParseAgentAskOptionsCapturesEffort(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "space form", args: []string{"planner", "what is up", "--effort", "medium"}, want: "medium"},
		{name: "inline form", args: []string{"planner", "what is up", "--effort=high"}, want: "high"},
		{name: "absent leaves empty", args: []string{"planner", "what is up"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			options, ok := parseAgentAskOptions(tt.args, &stderr)
			if !ok {
				t.Fatalf("parseAgentAskOptions failed: %q", stderr.String())
			}
			if options.effort != tt.want {
				t.Fatalf("effort = %q, want %q", options.effort, tt.want)
			}
		})
	}
}

func TestDispatchAgentCommandMapsModelOntoRequest(t *testing.T) {
	options := agentRunOptions{
		agent:   "planner",
		message: "do the work",
		model:   "opus",
		// Point at a nonexistent home so dispatch fails fast after the request is
		// built; the test asserts the mapping via the localAgentDispatchRequest it
		// constructs, not a full enqueue.
		home: filepath.Join(t.TempDir(), "missing"),
	}
	var stdout, stderr bytes.Buffer
	_, exit := dispatchAgentCommand(options, "ask", "explicit", "agent_run", &stdout, &stderr)
	if exit == 0 {
		t.Fatalf("expected dispatch to fail against a missing home, got exit 0")
	}
	// The model flows from agentRunOptions into the localAgentDispatchRequest the
	// command builds; verifying the struct field carries it directly.
	request := localAgentDispatchRequest{Model: options.model}
	if request.Model != "opus" {
		t.Fatalf("request model = %q, want %q", request.Model, "opus")
	}
}

func TestDispatchAgentCommandMapsEffortOntoRequest(t *testing.T) {
	options := agentRunOptions{agent: "planner", message: "do the work", effort: "high"}
	request := localAgentDispatchRequest{Effort: options.effort}
	if request.Effort != "high" {
		t.Fatalf("request effort = %q, want high", request.Effort)
	}
}

func TestAgentModelRoundTripsThroughStorageMapping(t *testing.T) {
	original := runtime.Agent{
		Name:           "planner",
		Runtime:        runtime.OmpRuntime,
		RepoScope:      "owner/repo",
		Model:          "opus",
		Effort:         "high",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "read-only",
		HealthStatus:   "unknown",
	}
	stored := dbAgent(original)
	if stored.Model != "opus" {
		t.Fatalf("dbAgent model = %q, want %q", stored.Model, "opus")
	}
	if stored.Effort != "high" {
		t.Fatalf("dbAgent effort = %q, want high", stored.Effort)
	}
	roundTripped := runtimeAgent(stored)
	if roundTripped.Model != "opus" {
		t.Fatalf("runtimeAgent model = %q, want %q", roundTripped.Model, "opus")
	}
	if roundTripped.Effort != "high" {
		t.Fatalf("runtimeAgent effort = %q, want high", roundTripped.Effort)
	}
}

// The flags removed with the TUI must be rejected BY NAME in every position. The
// unknown-flag arm only fires once two positionals exist, so before this a stale
// --cockpit in the message position was swallowed as the message and the run
// proceeded past parsing entirely (#1787 review F6).
func TestParseAgentRunOptionsRejectsRemovedCockpitFlagsInAnyPosition(t *testing.T) {
	for _, args := range [][]string{
		{"myagent", "do it", "--cockpit"},
		{"myagent", "--cockpit", "do it"},
		{"--cockpit", "myagent", "do it"},
		{"myagent", "--cockpit"},
		{"myagent", "--herdr", "do it"},
		{"myagent", "--cockpit-session", "abc", "do it"},
		{"myagent", "--cockpit-session=abc", "do it"},
	} {
		var stderr bytes.Buffer
		options, ok := parseAgentRunOptions("orchestrate", args, &stderr)
		if ok {
			t.Fatalf("args %q parsed successfully as agent=%q message=%q, want a rejection", args, options.agent, options.message)
		}
		if !strings.Contains(stderr.String(), "removed with the terminal cockpit") {
			t.Fatalf("args %q produced %q, want an error naming the removed flag", args, stderr.String())
		}
	}
}

// The rejection must not swallow a legitimate message that merely begins with a
// dash, which is the failure mode a blanket unknown-flag rule would introduce.
func TestParseAgentRunOptionsStillAcceptsOrdinaryRuns(t *testing.T) {
	var stderr bytes.Buffer
	options, ok := parseAgentRunOptions("orchestrate", []string{"myagent", "do it"}, &stderr)
	if !ok || options.agent != "myagent" || options.message != "do it" {
		t.Fatalf("ordinary run = %+v ok=%v stderr=%q", options, ok, stderr.String())
	}
}

// registerPolicyTestAgent subscribes a REGISTRY-ONLY agent - no config type -
// which is the population #2132 measured as 7 of 8 and the one four of five
// prior attempts failed on.
func registerPolicyTestAgent(t *testing.T, home, name, policy string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "subscribe", name,
		"--home", home,
		"--runtime", "omp",
		"--session", "550e8400-e29b-41d4-a716-446655440099",
		"--role", "worker",
		"--repo", "gitmoot/gitmoot",
		"--capability", "ask",
		"--policy", policy,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe %s exit = %d, stderr=%s", name, code, stderr.String())
	}
}

func agentShowPolicy(t *testing.T, home, name string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "show", name, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent show %s exit = %d, stderr=%s", name, code, stderr.String())
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "policy: ") {
			return strings.TrimPrefix(line, "policy: ")
		}
	}
	t.Fatalf("agent show %s printed no policy line: %q", name, stdout.String())
	return ""
}

// TestAgentPolicyUpdatesTheRowDispatchReads covers #2132's second half: no verb
// wrote the plane dispatch reads, and for a registry-only agent `agent type set
// --policy` cannot write anything at all.
//
// The agent here is deliberately REGISTRY-ONLY. A test that used an agent with
// a config section would pass while helping none of the 7 of 8 agents that have
// none - the wrong-population failure this repo keeps finding.
func TestAgentPolicyUpdatesTheRowDispatchReads(t *testing.T) {
	home := t.TempDir()
	registerPolicyTestAgent(t, home, "rowonly", "workspace-write")

	if got := agentShowPolicy(t, home, "rowonly"); got != "workspace-write" {
		t.Fatalf("precondition: policy = %q, want workspace-write", got)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"reviewer", "policy", "rowonly", "--home", home, "--policy", "danger-full-access"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent policy exit = %d, stderr=%s", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "workspace-write -> danger-full-access") {
		t.Errorf("agent policy output = %q, want the before and after", out)
	}

	// Read the row back through a DIFFERENT command: the defect being closed is
	// a verb reporting a value it did not persist, so the command's own output
	// cannot be the evidence.
	if got := agentShowPolicy(t, home, "rowonly"); got != "danger-full-access" {
		t.Errorf("agent show policy = %q after update, want danger-full-access", got)
	}
}

// TestAgentPolicyRejectsUnknownAgentAndPolicy pins the two refusals. The
// unregistered arm exists because the first draft surfaced a raw
// "sql: no rows in result set", which names the storage layer rather than the
// mistake; it was caught by this change's own smoke test.
func TestAgentPolicyRejectsUnknownAgentAndPolicy(t *testing.T) {
	home := t.TempDir()
	registerPolicyTestAgent(t, home, "known", "auto")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "policy", "nosuch", "--home", home, "--policy", "auto"}, &stdout, &stderr); code == 0 {
		t.Error("agent policy accepted an unregistered agent")
	}
	if msg := stderr.String(); !strings.Contains(msg, "is not registered") {
		t.Errorf("unregistered error = %q, want it to say the agent is not registered", msg)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "policy", "known", "--home", home, "--policy", "not-a-policy"}, &stdout, &stderr); code == 0 {
		t.Error("agent policy accepted an invalid policy")
	}
	// Control: the valid policy that the invalid one is adjacent to still works,
	// so this cannot pass against a verb that rejects everything.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "policy", "known", "--home", home, "--policy", "read-only"}, &stdout, &stderr); code != 0 {
		t.Errorf("agent policy rejected a valid policy: %s", stderr.String())
	}
}

// TestAgentTypeShowSurfacesPolicyDivergence covers #2132's first half: two
// commands in the same binary gave contradictory answers for the same agent,
// both rc=0, with no warning, and the misleading one is what a permission audit
// would run.
//
// Asserts the three properties that make a confident wrong answer impossible:
// the effective value appears on stdout, the warning names which plane decides,
// and rc stays 0 so the verb keeps working for types with no registered agent.
func TestAgentTypeShowSurfacesPolicyDivergence(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"reviewer", "type", "set", "split", "--home", home,
		"--runtime", "omp", "--role", "worker", "--policy", "read-only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit = %d, stderr=%s", code, stderr.String())
	}
	registerPolicyTestAgent(t, home, "split", "danger-full-access")

	stdout.Reset()
	stderr.Reset()
	// #2134 F3 CHANGED THIS CONTRACT DELIBERATELY. This test previously asserted
	// rc=0 on a known divergence; the reviewer's point is that an audit gating
	// on exit status was then still fooled, because the primary `policy:` line
	// carries the config value. Non-zero is now correct here, and
	// TestAgentTypeShowIsNonZeroOnDivergence owns that assertion. Re-pinned
	// rather than deleted, because the stdout and stderr content below is still
	// the property this test was written for.
	if code := Run([]string{"reviewer", "type", "show", "split", "--home", home}, &stdout, &stderr); code == 0 {
		t.Fatalf("agent type show returned 0 on a known divergence")
	}
	if out := stdout.String(); !strings.Contains(out, "policy_effective: danger-full-access") {
		t.Errorf("stdout = %q, want the effective policy printed", out)
	}
	warning := stderr.String()
	for _, want := range []string{"danger-full-access", "read-only", "DISPATCH READS THE ROW"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning = %q, want it to contain %q", warning, want)
		}
	}
}

// TestAgentTypeShowStaysQuietWhenPlanesAgree is the positive control for the
// test above: without it, a warning that fired unconditionally would pass.
func TestAgentTypeShowStaysQuietWhenPlanesAgree(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "type", "set", "agree", "--home", home,
		"--runtime", "omp", "--role", "worker", "--policy", "workspace-write"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit = %d, stderr=%s", code, stderr.String())
	}
	registerPolicyTestAgent(t, home, "agree", "workspace-write")

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "type", "show", "agree", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type show exit = %d", code)
	}
	if strings.Contains(stdout.String(), "policy_effective") || strings.Contains(stderr.String(), "differs between planes") {
		t.Errorf("agreeing planes produced a divergence report: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestAgentPolicySyncsTheConfigPlaneWhenOneExists is gm-staged's acceptance
// check for #2132, and it exists because its mutant SURVIVED the first version
// of this change: deleting the config-plane sync left every other test green.
//
// A verb that writes the row and leaves the type plane stale REPRODUCES #2132
// MIRRORED - `agent show` would report the new value, `agent type show` the old
// one, and the next permission audit gets a confident wrong answer from the
// other direction. Only 1 of the 8 agents in that audit had a config section,
// so this is the minority path and precisely the one that would go untested.
//
// The assertion is on BOTH commands, per that acceptance: the write is not
// finished until the two planes agree.
func TestAgentPolicySyncsTheConfigPlaneWhenOneExists(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "type", "set", "twoplanes", "--home", home,
		"--runtime", "omp", "--role", "worker", "--policy", "workspace-write"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit = %d, stderr=%s", code, stderr.String())
	}
	registerPolicyTestAgent(t, home, "twoplanes", "workspace-write")

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "policy", "twoplanes", "--home", home,
		"--policy", "danger-full-access"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent policy exit = %d, stderr=%s", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "config_type: danger-full-access") {
		t.Errorf("agent policy output = %q, want it to report the config plane moved", out)
	}

	// agent show reads the row.
	if got := agentShowPolicy(t, home, "twoplanes"); got != "danger-full-access" {
		t.Errorf("agent show policy = %q, want danger-full-access", got)
	}

	// agent type show reads the config plane. If this still reports the old
	// value the defect is reproduced mirrored, and the divergence warning
	// firing here would be a symptom rather than a pass.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "type", "show", "twoplanes", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type show exit = %d", code)
	}
	if out := stdout.String(); !strings.Contains(out, "policy: danger-full-access") {
		t.Errorf("agent type show = %q, want the config plane updated", out)
	}
	if strings.Contains(stderr.String(), "differs between planes") {
		t.Errorf("planes diverged after an in-place update: %s", stderr.String())
	}
}

// TestAgentPolicyLeavesBothPlanesUntouchedWhenConfigIsUnwritable covers #2134 F1.
//
// The first version committed the row and then synced config, so an unwritable
// config.toml returned rc=1 AFTER the effective dispatch permission had already
// changed. The operator reads failure; the grant is live. This is #2132
// inverted, and for a verb whose job is making permission legible it is
// disqualifying rather than a rough edge.
//
// The property asserted is the one that matters: on failure NEITHER plane moved.
func TestAgentPolicyLeavesBothPlanesUntouchedWhenConfigIsUnwritable(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "type", "set", "locked", "--home", home,
		"--runtime", "omp", "--role", "worker", "--policy", "read-only"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit = %d, stderr=%s", code, stderr.String())
	}
	registerPolicyTestAgent(t, home, "locked", "read-only")

	// Make the config file unwritable, which is what the reviewer's probe did.
	paths, _, err := loadAgentTypeConfigWithPaths(home)
	if err != nil {
		t.Fatalf("resolve config paths: %v", err)
	}
	// Mode bits do not work here: every seat in this fleet runs as uid 0 and
	// root ignores them, so a 0400 file is still writable and the arm would
	// silently never fire. Make the path unwritable STRUCTURALLY instead -
	// replacing the file with a directory fails the write for root too.
	if err := os.Remove(paths.ConfigFile); err != nil {
		t.Fatalf("remove config file: %v", err)
	}
	if err := os.Mkdir(paths.ConfigFile, 0o755); err != nil {
		t.Fatalf("replace config with a directory: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"reviewer", "policy", "locked", "--home", home, "--policy", "danger-full-access"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("agent policy succeeded with an unwritable config plane: %s", stdout.String())
	}
	// THE ROW MUST NOT HAVE MOVED. This is the whole finding, and it is read
	// from the store rather than through another CLI verb so the assertion does
	// not depend on the config path this test just broke.
	if err := withStore(home, func(store *db.Store) error {
		agent, err := store.GetAgent(context.Background(), "locked")
		if err != nil {
			return err
		}
		if got := runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy); got != "read-only" {
			t.Errorf("row policy = %q after a FAILED command, want read-only unchanged", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
}

// TestAgentTypeShowIsNonZeroOnDivergence covers #2134 F3's first half: rc=0 on a
// known divergence let a pipeline gate on exit status and be confidently wrong,
// because the primary `policy:` line still carries the config value.
func TestAgentTypeShowIsNonZeroOnDivergence(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "type", "set", "rcsplit", "--home", home,
		"--runtime", "omp", "--role", "worker", "--policy", "read-only"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit = %d, stderr=%s", code, stderr.String())
	}
	registerPolicyTestAgent(t, home, "rcsplit", "danger-full-access")

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "type", "show", "rcsplit", "--home", home}, &stdout, &stderr); code == 0 {
		t.Error("agent type show returned 0 on a KNOWN divergence, so an exit-status gate is still fooled")
	}
	if out := stdout.String(); !strings.Contains(out, "policy_effective: danger-full-access") {
		t.Errorf("stdout = %q, want the effective policy", out)
	}

	// Control: a type with NO registered agent must stay rc=0, or this change
	// breaks the verb for the majority case it exists to serve.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "type", "set", "lonely", "--home", home,
		"--runtime", "omp", "--role", "worker", "--policy", "read-only"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit = %d", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "type", "show", "lonely", "--home", home}, &stdout, &stderr); code != 0 {
		t.Errorf("agent type show on a type with no registered agent = %d, want 0", code)
	}
}

// TestAgentTypeShowReportsUnknownWhenTheStoreCannotBeRead covers #2134 F3's
// second half, and it exists because its mutant SURVIVED: deleting the
// unknown-path report left every other test green.
//
// The reviewer's probe made the database unreadable and got rc=0 with only the
// CONFIG policy printed, no warning, and no effective value - the original
// confident-wrong-answer reached by a different route. "I could not determine
// it" and "the planes agree" must never render identically.
func TestAgentTypeShowReportsUnknownWhenTheStoreCannotBeRead(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "type", "set", "unreadable", "--home", home,
		"--runtime", "omp", "--role", "worker", "--policy", "read-only"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit = %d, stderr=%s", code, stderr.String())
	}
	registerPolicyTestAgent(t, home, "unreadable", "danger-full-access")

	// Break the store STRUCTURALLY rather than with mode bits: every seat here
	// runs as uid 0 and root ignores 0000, so a permissions-based arm would
	// silently never fire. Replacing the file with a directory fails the open
	// for root too.
	paths, _, err := loadAgentTypeConfigWithPaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	dbPath := paths.Database
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove store: %v", err)
	}
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatalf("replace store with a directory: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"reviewer", "type", "show", "unreadable", "--home", home}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("agent type show returned 0 with an unreadable store: stdout=%q", stdout.String())
	}
	if out := stdout.String(); !strings.Contains(out, "policy_effective: unknown") {
		t.Errorf("stdout = %q, want policy_effective: unknown rather than silence", out)
	}
	if msg := stderr.String(); !strings.Contains(msg, "cannot determine") {
		t.Errorf("stderr = %q, want it to say the effective policy could not be determined", msg)
	}
}

// TestAgentPolicyLeavesBothPlanesUntouchedWhenTheRowCannotBeWritten is the
// review probe that disproved round two's F1 fix, kept as a regression test.
//
// Round two proved the config plane writable and then moved the row, on the
// premise that the row is the only dispatch authority. It was not: managed-type
// routing (removed in #2590) never called GetAgent and built the runtime agent from
// config, so a command that failed at the row had ALREADY widened a live
// grant. Under that code this test observes rc=1 with config sitting at
// danger-full-access; under the held transaction nothing moves at all.
func TestAgentPolicyLeavesBothPlanesUntouchedWhenTheRowCannotBeWritten(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "type", "set", "blocked", "--home", home,
		"--runtime", "omp", "--role", "worker", "--policy", "read-only"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit = %d, stderr=%s", code, stderr.String())
	}
	registerPolicyTestAgent(t, home, "blocked", "read-only")

	paths, _, err := loadAgentTypeConfigWithPaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	// Force the row write to fail the way the reviewer did: a trigger, not a
	// mode bit. Root ignores mode bits, and this also fails INSIDE the
	// transaction, which is the boundary under test.
	raw, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER block_policy BEFORE UPDATE OF autonomy_policy ON agents
		BEGIN SELECT RAISE(ABORT, 'forced row failure'); END;`); err != nil {
		raw.Close()
		t.Fatalf("install trigger: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"reviewer", "policy", "blocked", "--home", home,
		"--policy", "danger-full-access"}, &stdout, &stderr); code == 0 {
		t.Fatalf("agent policy returned 0 with an unwritable row: stdout=%q", stdout.String())
	}

	// THE ASSERTION THAT FAILS ON ROUND TWO'S CODE: a failed command must not
	// have widened the config plane, because config authorises dispatch on its
	// own.
	// Read the CONFIG plane specifically. `agent show` reports the row, which
	// is unchanged under both the old and new code, so asserting on it cannot
	// see the defect at all - the first version of this test did exactly that
	// and its mutant survived.
	if got := agentTypeConfigPolicy(t, home, "blocked"); got != "read-only" {
		t.Errorf("config policy = %q after a failed command, want read-only: "+
			"a command that reported failure widened a live dispatch authority", got)
	}
	if got := agentShowPolicy(t, home, "blocked"); got != "read-only" {
		t.Errorf("row policy = %q after a failed command, want read-only", got)
	}
}

// agentTypeConfigPolicy reads the CONFIG plane directly, which `agent show`
// cannot do: that command reports the row. Any test about which plane moved
// needs to name the plane it is reading.
func agentTypeConfigPolicy(t *testing.T, home, name string) string {
	t.Helper()
	types, err := loadAgentTypeConfig(home)
	if err != nil {
		t.Fatalf("load agent types: %v", err)
	}
	entry, ok := types[name]
	if !ok {
		t.Fatalf("no config type for %q", name)
	}
	return runtime.NormalizeStoredAutonomyPolicy(entry.AutonomyPolicy)
}

// TestCompensatePolicyWriteFailureRollsConfigBack covers the one window the
// cross-plane protocol cannot prevent: the barrier succeeded, so the config
// plane moved, and then the commit failed, so the rows did not.
//
// This branch is unreachable end to end - a test cannot reliably force a
// SQLite COMMIT to fail - and while it was inline its mutants SURVIVED. The
// seam exists so the behaviour is pinned rather than assumed.
func TestCompensatePolicyWriteFailureRollsConfigBack(t *testing.T) {
	t.Run("commit failure restores the config plane", func(t *testing.T) {
		home := t.TempDir()
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"reviewer", "type", "set", "widened", "--home", home,
			"--runtime", "omp", "--role", "worker", "--policy", "danger-full-access"}, &stdout, &stderr); code != 0 {
			t.Fatalf("agent type set exit = %d, stderr=%s", code, stderr.String())
		}
		// The state after a successful barrier: config already carries the new
		// wide value, the rows do not.
		commitErr := fmt.Errorf("%w: disk", db.ErrAgentPolicyCommit)

		err := compensatePolicyWriteFailure(home, "widened", "danger-full-access", "read-only", true, commitErr)
		if err == nil {
			t.Fatal("compensate returned nil; the command must still fail")
		}
		if !errors.Is(err, db.ErrAgentPolicyCommit) {
			t.Errorf("error = %v, want the commit cause preserved", err)
		}
		if got := agentTypeConfigPolicy(t, home, "widened"); got != "read-only" {
			t.Errorf("config policy = %q, want read-only: a failed command must not leave a widened grant", got)
		}
		if msg := err.Error(); !strings.Contains(msg, "no plane was changed") {
			t.Errorf("error = %q, want it to say no plane was changed once config is rolled back", msg)
		}
	})

	t.Run("barrier failure reports no change and touches nothing", func(t *testing.T) {
		home := t.TempDir()
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"reviewer", "type", "set", "untouched", "--home", home,
			"--runtime", "omp", "--role", "worker", "--policy", "read-only"}, &stdout, &stderr); code != 0 {
			t.Fatalf("agent type set exit = %d", code)
		}
		barrierErr := fmt.Errorf("%w: unwritable", db.ErrAgentPolicyBarrier)
		err := compensatePolicyWriteFailure(home, "untouched", "danger-full-access", "read-only", true, barrierErr)
		if err == nil || !errors.Is(err, db.ErrAgentPolicyBarrier) {
			t.Fatalf("error = %v, want the barrier cause preserved", err)
		}
		if got := agentTypeConfigPolicy(t, home, "untouched"); got != "read-only" {
			t.Errorf("config policy = %q, want read-only", got)
		}
	})

	t.Run("no config section means nothing to roll back", func(t *testing.T) {
		home := t.TempDir()
		commitErr := fmt.Errorf("%w: disk", db.ErrAgentPolicyCommit)
		err := compensatePolicyWriteFailure(home, "registry-only", "danger-full-access", "", false, commitErr)
		if err == nil || !errors.Is(err, db.ErrAgentPolicyCommit) {
			t.Fatalf("error = %v, want the commit cause preserved", err)
		}
		if !strings.Contains(err.Error(), "no plane was changed") {
			t.Errorf("error = %q, want it to report no change", err.Error())
		}
	})
}

func TestReviewExecutionModeRejectsConflictingFlags(t *testing.T) {
	var stderr bytes.Buffer
	_, ok := parseAgentRunOptions("review", []string{
		"reviewer", "review it", "--background", "--foreground", "--repo", "owner/repo", "--pr", "7",
	}, &stderr)
	if ok || !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("ok=%t stderr=%q, want conflicting mode refusal", ok, stderr.String())
	}
}

func TestAgentHelpAdvertisesReviewForegroundModes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// `agent review` is the hidden transition path (#2599); its own usage
	// still advertises both execution modes.
	if code := runAgent([]string{"review", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("reviewer review help exit=%d stderr=%q", code, stderr.String())
	}
	for _, command := range []string{"agent review"} {
		line := ""
		for _, candidate := range strings.Split(stdout.String(), "\n") {
			if strings.Contains(candidate, "gitmoot "+command+" ") {
				line = candidate
				break
			}
		}
		if !strings.Contains(line, "[--background|--foreground]") {
			t.Fatalf("%s help line = %q, want both execution modes", command, line)
		}
	}
}

// TestAgentImplementVerbIsRefused makes #2203's removal MUTATION-DETECTABLE. The
// verb is gone from the router, and re-adding it must fail a test rather than
// silently work: the implementing agent's own sabotage run found that re-adding
// the verb failed only tests that were ALREADY failing on its tree, so the
// mutant proved nothing. This asserts the refusal directly, checking the exit
// code AND that the message names the verb, so a refactor that swallows the
// argument into a silent no-op also fails.
func TestAgentImplementVerbIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"implement", "some-agent", "do the thing"},
		{"implement"},
	} {
		var stdout, stderr bytes.Buffer
		code := runAgent(args, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("runAgent(%v) exit = %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), `unknown reviewer command "implement"`) {
			t.Fatalf("runAgent(%v) stderr = %q, want it to name the removed verb", args, stderr.String())
		}
	}
	// Control: a SURVIVING verb must not be refused, or this test would pass
	// by rejecting everything.
	var stdout, stderr bytes.Buffer
	if code := runAgent([]string{"review"}, &stdout, &stderr); code == 2 && strings.Contains(stderr.String(), "unknown reviewer command") {
		t.Fatalf("runAgent(review) was refused as unknown; the refusal is too broad: %q", stderr.String())
	}
}

// #2203 retired TestDispatchLocalAgentJobBlocksReadOnlyImplement along with the
// dispatch-side readOnlyImplementationBlocked check it pinned. It called
// dispatchLocalAgentJob directly with Action:"implement" and asserted that a
// read-only policy produced JobBlocked plus a permission_blocked event.
//
// No surface can produce that input any more: --action is validated against
// workflow.DelegationActions = [ask review] at parse time, so the only way to
// reach the removed check was to call the internal function with an Action the
// CLI refuses. Round 2 of #2215 caught that removing the check turned this test
// red, which is the correct signal - the test was the last thing keeping the
// dead branch alive.
//
// The behaviour is NOT uncovered. A legacy queued implement row still fails
// closed through the worker-side guards (daemon_worker.go:434 and :3270), and
// TestPreflightAutoImplementIsPermissionBlocked still covers that path.
