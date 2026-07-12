package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestSelectAgentRunActionExplicitOverrideMatrix(t *testing.T) {
	tests := []struct {
		name    string
		options agentRunOptions
		want    string
	}{
		{name: "no override task", options: agentRunOptions{taskID: "task-1"}, want: "implement"},
		{name: "no override pr", options: agentRunOptions{prNumber: 7}, want: "review"},
		{name: "implement override pr", options: agentRunOptions{action: "implement", prNumber: 7}, want: "implement"},
		{name: "review override pr", options: agentRunOptions{action: "review", prNumber: 7}, want: "review"},
		{name: "managed type does not override pr routing", options: agentRunOptions{typeName: "X", prNumber: 7}, want: "review"},
		{name: "action and managed type are independent", options: agentRunOptions{action: "implement", typeName: "X", prNumber: 7}, want: "implement"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := selectAgentRunAction(tt.options)
			if got != tt.want {
				t.Fatalf("selectAgentRunAction() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseAgentRunActionValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantOK  bool
		wantErr string
	}{
		{name: "space form", args: []string{"worker", "fix it", "--action", "implement", "--pr", "7"}, wantOK: true},
		{name: "inline form", args: []string{"worker", "review it", "--action=review", "--pr=7"}, wantOK: true},
		{name: "invalid enum", args: []string{"worker", "do it", "--action=ship"}, wantErr: "one of ask, review, or implement"},
		{name: "review requires pr", args: []string{"worker", "review it", "--action=review"}, wantErr: "--action review requires --pr number"},
		{name: "ask rejects pr", args: []string{"worker", "explain it", "--action=ask", "--pr=7"}, wantErr: "--action ask cannot be combined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			options, ok := parseAgentRunOptions("run", tt.args, &stderr)
			if ok != tt.wantOK {
				t.Fatalf("parse ok = %v, want %v; stderr=%q", ok, tt.wantOK, stderr.String())
			}
			if tt.wantOK && options.action == "" {
				t.Fatal("parsed action is empty")
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestForcedManagedTypeNotFoundExplainsActionFlag(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	_, _, err := resolveLocalDispatchAgent(context.Background(), store, localAgentDispatchRequest{
		Home:       home,
		Agent:      "worker",
		Type:       "implement",
		Action:     "review",
		Background: true,
	}, "owner/repo", db.Repo{Owner: "owner", Name: "repo"})
	if err == nil {
		t.Fatal("resolveLocalDispatchAgent accepted an unknown forced type")
	}
	for _, want := range []string{"--type selects a managed agent type", "--action chooses the job action"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestAgentRunForegroundReviewForcedMissingTypeNamesType(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/e2e/toy.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", "stub",
		"--home", home,
		"--runtime", "shell",
		"--session", "printf unused",
		"--role", "reviewer",
		"--repo", "e2e/toy",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent subscribe exit = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "run", "stub", "fix",
		"--home", home,
		"--repo", "e2e/toy",
		"--pr", "5",
		"--type", "implement",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("agent run exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		`agent review: managed agent type "implement" not found`,
		"--type selects a managed agent type",
		"--action chooses the job action",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), `agent "stub" not found`) {
		t.Fatalf("stderr falsely reports the registered agent missing: %q", stderr.String())
	}
}

type fixPassPullRequestClient struct {
	github.NoopClient
	pr  github.PullRequest
	err error
}

func (c fixPassPullRequestClient) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	return c.pr, c.err
}

func installFixPassPullRequestClient(t *testing.T, pr github.PullRequest) {
	t.Helper()
	previous := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client {
		return fixPassPullRequestClient{pr: pr}
	}
	t.Cleanup(func() { newAgentDispatchGitHubClient = previous })
}

type fixPassFixture struct {
	home       string
	checkout   string
	record     db.Repo
	repo       github.Repository
	store      *db.Store
	task       db.Task
	branch     string
	owner      string
	pullNumber int
	headSHA    string
}

func newFixPassFixture(t *testing.T, state workflow.TaskState) fixPassFixture {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	writeFile(t, filepath.Join(checkout, "README.md"), "main\n")
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")

	store := openCLIJobStore(t, home)
	t.Cleanup(func() { store.Close() })
	branch := "feature/fix-pass"
	owner := "implementer"
	record := db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: checkout}
	repo := github.Repository{Owner: "owner", Name: "repo"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-existing",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-existing",
		Title:        "Existing task",
		State:        string(workflow.TaskPlanned),
		Branch:       branch,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	task, _, err := prepareLocalImplementDispatchRequest(ctx, store, record, repo, localAgentDispatchRequest{
		Home:          home,
		Agent:         owner,
		Action:        "implement",
		Instructions:  "Initial implementation.",
		Branch:        branch,
		ImplementBase: "HEAD",
	})
	if err != nil {
		t.Fatalf("allocate initial worktree: %v", err)
	}
	task.State = string(state)
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("set fixture task state: %v", err)
	}
	return fixPassFixture{
		home: home, checkout: checkout, record: record, repo: repo, store: store,
		task: task, branch: branch, owner: owner, pullNumber: 17,
		headSHA: strings.TrimSpace(runGitOutput(t, task.WorktreePath, "rev-parse", "HEAD")),
	}
}

func (f fixPassFixture) pullRequest() github.PullRequest {
	return github.PullRequest{
		Number:           int64(f.pullNumber),
		State:            "open",
		HeadRef:          f.branch,
		HeadRepoFullName: f.repo.FullName(),
		HeadSHA:          f.headSHA,
	}
}

func TestPrepareLocalImplementFixPassReusesTaskAndCarriesPullRequest(t *testing.T) {
	fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
	installFixPassPullRequestClient(t, fixture.pullRequest())
	task, request, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
		Home:          fixture.home,
		Agent:         fixture.owner,
		Action:        "implement",
		Instructions:  "Fix review findings.",
		PullRequest:   fixture.pullNumber,
		ImplementBase: "HEAD",
	})
	if err != nil {
		t.Fatalf("prepare fix-pass: %v", err)
	}
	if task.ID != fixture.task.ID || task.WorktreePath != fixture.task.WorktreePath {
		t.Fatalf("task = %+v, want reused task/worktree %+v", task, fixture.task)
	}
	if request.TaskID != fixture.task.ID || request.Branch != fixture.branch || request.PullRequest != fixture.pullNumber {
		t.Fatalf("request binding = %+v", request)
	}
	tasks, err := fixture.store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("fix-pass minted tasks: %+v", tasks)
	}
}

func TestPrepareLocalImplementFixPassKeepsChangesRequestedReusable(t *testing.T) {
	fixture := newFixPassFixture(t, workflow.TaskChangesRequested)
	installFixPassPullRequestClient(t, fixture.pullRequest())
	task, request, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
		Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "address findings", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
	})
	if err != nil {
		t.Fatalf("changes_requested fix-pass: %v", err)
	}
	if task.ID != fixture.task.ID || request.PullRequest != fixture.pullNumber {
		t.Fatalf("changes_requested binding = task %+v request %+v", task, request)
	}
}

func TestPrepareLocalImplementFixPassRejections(t *testing.T) {
	t.Run("bare pr_open branch remains refused", func(t *testing.T) {
		fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
		_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", Branch: fixture.branch, ImplementBase: "HEAD",
		})
		if err == nil || !strings.Contains(err.Error(), "state pr_open") {
			t.Fatalf("error = %v, want unchanged pr_open refusal", err)
		}
	})

	t.Run("requested branch mismatch", func(t *testing.T) {
		fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
		installFixPassPullRequestClient(t, fixture.pullRequest())
		_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, Branch: "other", ImplementBase: "HEAD",
		})
		if err == nil || !strings.Contains(err.Error(), "does not match requested branch") {
			t.Fatalf("error = %v, want branch mismatch", err)
		}
	})

	t.Run("stale worktree head", func(t *testing.T) {
		fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
		pr := fixture.pullRequest()
		pr.HeadSHA = strings.Repeat("f", 40)
		installFixPassPullRequestClient(t, pr)
		_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
		})
		if err == nil || !strings.Contains(err.Error(), "refusing to run against stale code") {
			t.Fatalf("error = %v, want stale-head refusal", err)
		}
		for _, guidance := range []string{
			"fetch origin refs/pull/17/head",
			"reset --hard FETCH_HEAD",
			"inspect or stash local changes",
		} {
			if !strings.Contains(err.Error(), guidance) {
				t.Fatalf("error = %q, want synchronization guidance %q", err, guidance)
			}
		}
	})

	for _, tt := range []struct {
		name  string
		state string
		merge bool
		want  string
	}{
		{name: "closed", state: "closed", want: "requires an open pull request"},
		{name: "merged", state: "closed", merge: true, want: "is merged"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
			pr := fixture.pullRequest()
			pr.State = tt.state
			pr.Merged = tt.merge
			installFixPassPullRequestClient(t, pr)
			_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
				Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

	t.Run("fork head", func(t *testing.T) {
		fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
		pr := fixture.pullRequest()
		pr.HeadRepoFullName = "fork/repo"
		installFixPassPullRequestClient(t, pr)
		_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
		})
		if err == nil || !strings.Contains(err.Error(), "fork or unrelated heads") {
			t.Fatalf("error = %v, want fork refusal", err)
		}
	})

	for _, state := range []workflow.TaskState{workflow.TaskReviewing, workflow.TaskReadyToMerge} {
		t.Run(string(state), func(t *testing.T) {
			fixture := newFixPassFixture(t, state)
			installFixPassPullRequestClient(t, fixture.pullRequest())
			_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
				Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
			})
			if err == nil || !strings.Contains(err.Error(), "review or merge is in progress") {
				t.Fatalf("error = %v, want state refusal", err)
			}
		})
	}
}

func TestPrepareLocalImplementFixPassPreservesWorktreeSafetyChecks(t *testing.T) {
	t.Run("active implement job", func(t *testing.T) {
		fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
		installFixPassPullRequestClient(t, fixture.pullRequest())
		if err := fixture.store.CreateJob(context.Background(), db.Job{
			ID: "active-implement", Agent: fixture.owner, Type: "implement", State: string(workflow.JobQueued),
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: fixture.repo.FullName(), Branch: fixture.branch, TaskID: fixture.task.ID}),
		}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
		})
		if err == nil || !strings.Contains(err.Error(), "active implement job") {
			t.Fatalf("error = %v, want active-job refusal", err)
		}
	})

	t.Run("dirty worktree", func(t *testing.T) {
		fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
		installFixPassPullRequestClient(t, fixture.pullRequest())
		writeFile(t, filepath.Join(fixture.task.WorktreePath, "dirty.txt"), "dirty\n")
		_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
		})
		if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
			t.Fatalf("error = %v, want dirty-worktree refusal", err)
		}
	})

	t.Run("live process", func(t *testing.T) {
		fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
		installFixPassPullRequestClient(t, fixture.pullRequest())
		previous := taskWorktreeHasLiveProcess
		taskWorktreeHasLiveProcess = func(path string) bool { return path == fixture.task.WorktreePath }
		t.Cleanup(func() { taskWorktreeHasLiveProcess = previous })
		_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
		})
		if err == nil || !strings.Contains(err.Error(), "live process") {
			t.Fatalf("error = %v, want live-process refusal", err)
		}
	})

	t.Run("foreign branch lock", func(t *testing.T) {
		fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
		installFixPassPullRequestClient(t, fixture.pullRequest())
		lock := db.BranchLock{RepoFullName: fixture.repo.FullName(), Branch: fixture.branch, Owner: fixture.owner}
		if released, err := fixture.store.ReleaseLock(context.Background(), lock); err != nil || !released {
			t.Fatalf("release fixture lock: released=%v err=%v", released, err)
		}
		if acquired, err := fixture.store.AcquireLock(context.Background(), db.BranchLock{RepoFullName: fixture.repo.FullName(), Branch: fixture.branch, Owner: "other"}); err != nil || !acquired {
			t.Fatalf("acquire foreign lock: acquired=%v err=%v", acquired, err)
		}
		_, _, err := prepareLocalImplementDispatchRequest(context.Background(), fixture.store, fixture.record, fixture.repo, localAgentDispatchRequest{
			Home: fixture.home, Agent: fixture.owner, Action: "implement", Instructions: "retry", PullRequest: fixture.pullNumber, ImplementBase: "HEAD",
		})
		if err == nil || !strings.Contains(err.Error(), "branch lock rejected action") {
			t.Fatalf("error = %v, want foreign-lock refusal", err)
		}
	})
}

func TestTaskRecoverPredicateStillRejectsPullRequestOpen(t *testing.T) {
	if taskBranchReusableForImplement(string(workflow.TaskPullRequestOpen)) {
		t.Fatal("shared task recover/implement predicate was widened to pr_open")
	}
	fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
	_, err := recoverTaskImplementation(context.Background(), fixture.store, fixture.task.ID, fixture.repo.FullName(), fixture.owner, false, github.NoopClient{})
	if err == nil || !strings.Contains(err.Error(), "task recover only supports active implementation states") {
		t.Fatalf("task recover error = %v, want unchanged pr_open refusal", err)
	}
}

type finalizerFixPassGitHub struct {
	github.NoopClient
	prs         map[int64]github.PullRequest
	getCalls    []int64
	ensureCalls int
}

func (c *finalizerFixPassGitHub) GetPullRequest(_ context.Context, _ github.Repository, number int64) (github.PullRequest, error) {
	c.getCalls = append(c.getCalls, number)
	pr, ok := c.prs[number]
	if !ok {
		return github.PullRequest{}, fmt.Errorf("unexpected pull request #%d", number)
	}
	return pr, nil
}

func (c *finalizerFixPassGitHub) EnsurePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	c.ensureCalls++
	return github.PullRequest{}, errors.New("validated fix-pass must not derive a pull request by branch")
}

type finalizerFixPassFixture struct {
	home     string
	checkout string
	store    *db.Store
	task     db.Task
	headSHA  string
}

func newFinalizerFixPassFixture(t *testing.T) finalizerFixPassFixture {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	remote := filepath.Join(home, "remote.git")
	checkout := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remote)
	runGit(t, home, "clone", remote, checkout)
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	writeFile(t, filepath.Join(checkout, "README.md"), "main\n")
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "push", "-u", "origin", "main")
	runGit(t, checkout, "switch", "-c", "task-1")

	store := openCLIJobStore(t, home)
	t.Cleanup(func() { store.Close() })
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	task := db.Task{
		ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1",
		State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: checkout,
	}
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	return finalizerFixPassFixture{
		home: home, checkout: checkout, store: store, task: task,
		headSHA: strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD")),
	}
}

func TestDaemonImplementationFinalizerPrefersValidatedPullRequest(t *testing.T) {
	ctx := context.Background()
	fixture := newFinalizerFixPassFixture(t)
	for _, pr := range []db.PullRequest{
		{RepoFullName: "owner/repo", Number: 17, URL: "https://github.com/owner/repo/pull/17", HeadBranch: fixture.task.Branch, BaseBranch: "release/v2", HeadSHA: fixture.headSHA, State: "open"},
		// The legacy branch lookup chooses the highest PR number. Keeping #99 open
		// on the same head proves the validated #17 bypasses that ambiguous lookup.
		{RepoFullName: "owner/repo", Number: 99, URL: "https://github.com/owner/repo/pull/99", HeadBranch: fixture.task.Branch, BaseBranch: "main", HeadSHA: fixture.headSHA, State: "open"},
	} {
		if err := fixture.store.UpsertPullRequest(ctx, pr); err != nil {
			t.Fatalf("UpsertPullRequest(%d): %v", pr.Number, err)
		}
	}
	gh := &finalizerFixPassGitHub{prs: map[int64]github.PullRequest{
		17: {
			Number: 17, State: "open", URL: "https://github.com/owner/repo/pull/17",
			HeadRef: fixture.task.Branch, HeadRepoFullName: "owner/repo", HeadSHA: fixture.headSHA,
			BaseRef: "release/v2",
		},
	}}
	payload := workflow.JobPayload{
		Repo: "owner/repo", Branch: fixture.task.Branch, PullRequest: 17,
		ValidatedPullRequest: true, HeadSHA: fixture.headSHA, TaskID: fixture.task.ID,
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "fix pass complete"},
	}
	finalized, err := (daemonImplementationFinalizer{Store: fixture.store, GitHub: gh}).FinalizeImplementation(ctx, db.Job{ID: "fix-pass", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation: %v", err)
	}
	if finalized.PullRequest != 17 || finalized.HeadSHA != fixture.headSHA {
		t.Fatalf("finalized payload = %+v, want exact PR #17 at %s", finalized, fixture.headSHA)
	}
	if len(gh.getCalls) != 1 || gh.getCalls[0] != 17 || gh.ensureCalls != 0 {
		t.Fatalf("GitHub calls: get=%v ensure=%d, want exact GetPullRequest(17) only", gh.getCalls, gh.ensureCalls)
	}
	stored, err := fixture.store.GetPullRequest(ctx, "owner/repo", 17)
	if err != nil {
		t.Fatalf("GetPullRequest(17): %v", err)
	}
	if stored.BaseBranch != "release/v2" || stored.HeadBranch != fixture.task.Branch {
		t.Fatalf("stored validated PR = %+v, want non-default base release/v2", stored)
	}
}

func TestDaemonImplementationFinalizerPayloadlessUsesBranchFallback(t *testing.T) {
	ctx := context.Background()
	fixture := newFinalizerFixPassFixture(t)
	if err := fixture.store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo", Number: 99, URL: "https://github.com/owner/repo/pull/99",
		HeadBranch: fixture.task.Branch, BaseBranch: "main", HeadSHA: fixture.headSHA, State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	writeFile(t, filepath.Join(fixture.checkout, "fix.txt"), "payload-less fallback\n")
	gh := &finalizerFixPassGitHub{prs: map[int64]github.PullRequest{}}
	payload := workflow.JobPayload{
		Repo: "owner/repo", Branch: fixture.task.Branch, TaskID: fixture.task.ID,
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "ordinary implementation"},
	}
	finalized, err := (daemonImplementationFinalizer{Store: fixture.store, GitHub: gh}).FinalizeImplementation(ctx, db.Job{ID: "ordinary", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation: %v", err)
	}
	if finalized.PullRequest != 99 {
		t.Fatalf("payload-less finalized PR = %d, want branch-cache fallback #99", finalized.PullRequest)
	}
	if len(gh.getCalls) != 0 || gh.ensureCalls != 0 {
		t.Fatalf("payload-less GitHub calls: get=%v ensure=%d, want unchanged local branch fallback", gh.getCalls, gh.ensureCalls)
	}
}

func TestDaemonImplementationFinalizerRevalidatesBoundPullRequest(t *testing.T) {
	for _, tt := range []struct {
		name string
		pr   github.PullRequest
		want string
	}{
		{
			name: "closed",
			pr:   github.PullRequest{Number: 17, State: "closed", HeadRef: "task-1", BaseRef: "release/v2"},
			want: "is no longer open",
		},
		{
			name: "head branch changed",
			pr:   github.PullRequest{Number: 17, State: "open", HeadRef: "other", BaseRef: "release/v2"},
			want: "not task branch task-1",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFinalizerFixPassFixture(t)
			gh := &finalizerFixPassGitHub{prs: map[int64]github.PullRequest{17: tt.pr}}
			payload := workflow.JobPayload{
				Repo: "owner/repo", Branch: fixture.task.Branch, PullRequest: 17,
				ValidatedPullRequest: true, HeadSHA: fixture.headSHA, TaskID: fixture.task.ID,
				Result: &workflow.AgentResult{Decision: "implemented", Summary: "fix pass complete"},
			}
			_, err := (daemonImplementationFinalizer{Store: fixture.store, GitHub: gh}).FinalizeImplementation(context.Background(), db.Job{ID: "fix-pass", Agent: "lead", Type: "implement"}, payload)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("FinalizeImplementation error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAgentRunActionImplementPRFixPassShellE2E(t *testing.T) {
	fixture := newFixPassFixture(t, workflow.TaskPullRequestOpen)
	installFixPassPullRequestClient(t, fixture.pullRequest())
	// The existing task worktree is the execution target. A stale/non-default
	// checkout branch must not trigger the new-worktree implicit-base guard.
	runGit(t, fixture.checkout, "switch", "-c", "feature/stale-checkout")
	if err := fixture.store.UpsertRepo(context.Background(), fixture.record); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	shellResult := `printf '%s' '{"gitmoot_result":{"decision":"implemented","summary":"fix pass complete","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`
	if err := fixture.store.UpsertAgent(context.Background(), db.Agent{
		Name: fixture.owner, Role: "worker", Runtime: runtime.ShellRuntime, RuntimeRef: shellResult,
		RepoScope: fixture.repo.FullName(), Capabilities: []string{"implement"},
		AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite, HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "run", fixture.owner, "Fix the requested PR findings.",
		"--home", fixture.home,
		"--repo", fixture.repo.FullName(),
		"--action", "implement",
		"--pr", fmt.Sprint(fixture.pullNumber),
		"--skip-native-review-fanout",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent run exit = %d, stderr=%s", code, stderr.String())
	}
	var output localAgentJobOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output %q: %v", stdout.String(), err)
	}
	if output.Action != "implement" || output.SelectedAction != "implement" || output.ExecutionPath != "agent_run" {
		t.Fatalf("route output = %+v", output)
	}
	job, err := fixture.store.GetJob(context.Background(), output.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.TaskID != fixture.task.ID || payload.Branch != fixture.branch || payload.PullRequest != fixture.pullNumber || !payload.ValidatedPullRequest {
		t.Fatalf("payload binding = %+v", payload)
	}
	events, err := fixture.store.ListJobEvents(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	foundRoute := false
	for _, event := range events {
		if event.Kind == "route_selected" && strings.Contains(event.Message, "selected implement") {
			foundRoute = true
		}
	}
	if !foundRoute {
		t.Fatalf("route_selected event missing from %+v", events)
	}
	task, err := fixture.store.GetTask(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(workflow.TaskPullRequestOpen) {
		t.Fatalf("task state = %q, want pr_open after fix-pass lifecycle", task.State)
	}
	tasks, err := fixture.store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("fix-pass minted a second task: %+v", tasks)
	}
}
