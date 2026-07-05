package cli

import (
	"bufio"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
	"github.com/jerryfane/gitmoot/skills"
)

var taskHeadingPattern = regexp.MustCompile(`^### Task ([0-9]+):\s*(.+)$`)

type importedGoal struct {
	Goal  db.Goal
	Tasks []db.Task
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "filter pull requests by owner/repo")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "status does not accept positional arguments")
		return 2
	}

	var agents []db.Agent
	var goals []db.Goal
	var repos []db.Repo
	var tasks []db.Task
	var prs []db.PullRequest
	var jobs []db.Job
	var locks []db.BranchLock
	repoFullName := ""
	if strings.TrimSpace(*repo) != "" {
		var ok bool
		repoFullName, ok = normalizeOptionalRepoFlag(*repo, stderr)
		if !ok {
			return 2
		}
	}
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		if agents, err = store.ListAgents(context.Background()); err != nil {
			return err
		}
		if strings.TrimSpace(repoFullName) != "" {
			filtered := []db.Agent{}
			for _, agent := range agents {
				allowed, err := store.AgentCanAccessRepo(context.Background(), agent.Name, repoFullName)
				if err != nil {
					return err
				}
				if allowed {
					filtered = append(filtered, agent)
				}
			}
			agents = filtered
		}
		if goals, err = store.ListGoals(context.Background()); err != nil {
			return err
		}
		if repos, err = store.ListRepos(context.Background()); err != nil {
			return err
		}
		if strings.TrimSpace(repoFullName) == "" {
			if tasks, err = store.ListTasks(context.Background()); err != nil {
				return err
			}
		} else {
			if tasks, err = store.ListTasksByRepo(context.Background(), repoFullName); err != nil {
				return err
			}
		}
		if prs, err = store.ListPullRequests(context.Background(), repoFullName); err != nil {
			return err
		}
		if jobs, err = store.ListJobs(context.Background()); err != nil {
			return err
		}
		locks, err = store.ListBranchLocks(context.Background(), repoFullName)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}

	if strings.TrimSpace(repoFullName) == "" {
		fmt.Fprintln(stdout, "scope: global")
	} else {
		fmt.Fprintf(stdout, "scope: %s\n", repoFullName)
	}
	fmt.Fprintf(stdout, "repos: %d\n", countRepos(repos, repoFullName))
	fmt.Fprintf(stdout, "agents: %d\n", len(agents))
	for _, agent := range agents {
		fmt.Fprintf(stdout, "  %s: %s %s\n", agent.Name, agent.Runtime, strings.Join(agent.Capabilities, ","))
	}
	fmt.Fprintf(stdout, "goals: %d\n", len(goals))
	fmt.Fprintf(stdout, "tasks: %d\n", len(tasks))
	counts := taskStateCounts(tasks)
	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, state)
	}
	sort.Strings(states)
	for _, state := range states {
		fmt.Fprintf(stdout, "  %s: %d\n", state, counts[state])
	}
	fmt.Fprintf(stdout, "pull_requests: %d\n", len(prs))
	filteredJobCount := countJobs(jobs, repoFullName)
	jobCounts := jobStateCounts(jobs, repoFullName)
	fmt.Fprintf(stdout, "jobs: %d\n", filteredJobCount)
	for _, state := range sortedCountKeys(jobCounts) {
		fmt.Fprintf(stdout, "  %s: %d\n", state, jobCounts[state])
	}
	fmt.Fprintf(stdout, "locks: %d\n", len(locks))
	return 0
}

func runGoal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printGoalUsage(stdout)
		return 0
	}
	switch args[0] {
	case "import":
		return runGoalImport(args[1:], stdout, stderr)
	case "template":
		return runGoalTemplate(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown goal command %q\n\n", args[0])
		printGoalUsage(stderr)
		return 2
	}
}

func printGoalUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot goal import --file <path> [--repo owner/repo]")
	fmt.Fprintln(w, "  gitmoot goal template")
}

func runGoalTemplate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("goal template", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "goal template does not accept positional arguments")
		return 2
	}
	content, err := skills.FS.ReadFile("gitmoot/references/GOAL_TEMPLATE.md")
	if err != nil {
		fmt.Fprintf(stderr, "goal template: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(content); err != nil {
		fmt.Fprintf(stderr, "goal template: %v\n", err)
		return 1
	}
	return 0
}

func runGoalImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("goal import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	file := fs.String("file", "", "goal markdown file to import")
	repo := fs.String("repo", "", "repo scope as owner/repo")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "goal import does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*file) == "" {
		fmt.Fprintln(stderr, "goal import requires --file")
		return 2
	}
	repoScope := strings.TrimSpace(*repo)
	if repoScope != "" {
		parsedRepo, err := daemon.ParseRepository(repoScope)
		if err != nil {
			fmt.Fprintf(stderr, "invalid repo: %v\n", err)
			return 2
		}
		repoScope = parsedRepo.FullName()
	}
	imported, err := parseGoalFile(*file, repoScope)
	if err != nil {
		fmt.Fprintf(stderr, "parse goal: %v\n", err)
		return 1
	}
	if err := withStore(*home, func(store *db.Store) error {
		if err := validateImportConflicts(context.Background(), store, imported.Tasks); err != nil {
			return err
		}
		return store.UpsertGoalWithTasks(context.Background(), imported.Goal, imported.Tasks)
	}); err != nil {
		fmt.Fprintf(stderr, "import goal: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "imported goal %s with %d tasks\n", imported.Goal.ID, len(imported.Tasks))
	return 0
}

func runTask(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printTaskUsage(stdout)
		return 0
	}
	switch args[0] {
	case "run":
		return runTaskRun(args[1:], stdout, stderr)
	case "list":
		return runTaskList(args[1:], stdout, stderr)
	case "recover":
		return runTaskRecover(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown task command %q\n\n", args[0])
		printTaskUsage(stderr)
		return 2
	}
}

func printTaskUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot task run <id> --repo owner/repo --owner <agent> [--branch <branch>] [--base <branch>]")
	fmt.Fprintln(w, "  gitmoot task recover <id> --repo owner/repo [--owner <agent>] [--skip-native-review-fanout] [--json]")
	fmt.Fprintln(w, "  gitmoot task list [--repo owner/repo] [--state state] [--json]")
}

type taskListOutput struct {
	ID           string `json:"id"`
	Repo         string `json:"repo"`
	GoalID       string `json:"goal_id"`
	Title        string `json:"title"`
	State        string `json:"state"`
	Branch       string `json:"branch"`
	WorktreePath string `json:"worktree_path"`
}

func runTaskList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo scope as owner/repo")
	state := fs.String("state", "", "task state filter")
	jsonOutput := fs.Bool("json", false, "print tasks as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "task list does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*repo) != "" {
		if _, err := normalizeRepoFlag(*repo); err != nil {
			fmt.Fprintf(stderr, "invalid repo: %v\n", err)
			return 2
		}
	}

	var tasks []db.Task
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		if strings.TrimSpace(*repo) != "" {
			tasks, err = store.ListTasksByRepo(context.Background(), strings.TrimSpace(*repo))
		} else {
			tasks, err = store.ListTasks(context.Background())
		}
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "task list: %v\n", err)
		return 1
	}
	outputs := make([]taskListOutput, 0, len(tasks))
	stateFilter := strings.TrimSpace(*state)
	for _, task := range tasks {
		if stateFilter != "" && task.State != stateFilter {
			continue
		}
		outputs = append(outputs, taskListOutput{
			ID:           task.ID,
			Repo:         task.RepoFullName,
			GoalID:       task.GoalID,
			Title:        task.Title,
			State:        task.State,
			Branch:       task.Branch,
			WorktreePath: task.WorktreePath,
		})
	}
	if *jsonOutput {
		if err := writeJSON(stdout, outputs); err != nil {
			fmt.Fprintf(stderr, "task list: %v\n", err)
			return 1
		}
		return 0
	}
	for _, task := range outputs {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\n", task.ID, task.State, task.Repo, task.Branch, task.WorktreePath, task.Title)
	}
	return 0
}

func runTaskRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo scope as owner/repo")
	owner := fs.String("owner", "", "agent that will hold the branch lock")
	branch := fs.String("branch", "", "task branch name")
	base := fs.String("base", "", "base branch for git worktree add")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "task run requires exactly one id")
			return 2
		}
		return 0
	}
	taskID := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "task run requires exactly one id")
		return 2
	}
	if strings.TrimSpace(*owner) == "" {
		fmt.Fprintln(stderr, "task run requires --owner")
		return 2
	}

	var started db.Task
	var startedJob db.Job
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		task, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("task %q not found", taskID)
			}
			return err
		}
		requestRepo := firstNonEmpty(*repo, task.RepoFullName)
		if strings.TrimSpace(requestRepo) == "" {
			return errors.New("task run requires --repo when task has no repo")
		}
		if strings.TrimSpace(*repo) != "" && strings.TrimSpace(task.RepoFullName) != "" && *repo != task.RepoFullName {
			return fmt.Errorf("task %s belongs to repo %s, not %s", task.ID, task.RepoFullName, *repo)
		}
		repo, err := daemon.ParseRepository(requestRepo)
		if err != nil {
			return fmt.Errorf("invalid repo: %w", err)
		}
		repoRecord, err := repoRecordForCheckout(context.Background(), repo, gitutil.Client{Dir: "."})
		if err != nil {
			return err
		}
		if err := store.UpsertRepo(context.Background(), repoRecord); err != nil {
			return err
		}
		agent, err := store.GetAgent(context.Background(), strings.TrimSpace(*owner))
		if err != nil {
			return fmt.Errorf("load task owner agent %q: %w", strings.TrimSpace(*owner), err)
		}
		if err := ensureLocalAgentAccess(context.Background(), store, agent, requestRepo, "implement"); err != nil {
			return err
		}
		checkout := repoRecord.CheckoutPath
		requestBranch := firstNonEmpty(*branch, task.Branch, task.ID)
		engine := workflow.Engine{Store: store}
		started, err = engine.AllocateTaskWorktree(context.Background(), workflow.TaskWorktreeRequest{
			Home:       paths.Home,
			Repo:       requestRepo,
			GoalID:     task.GoalID,
			TaskID:     task.ID,
			TaskTitle:  task.Title,
			Branch:     requestBranch,
			BaseBranch: *base,
			Owner:      *owner,
			Checkout:   checkout,
		}, gitutil.Client{Dir: checkout})
		if err != nil {
			return err
		}
		headSHA, err := (gitutil.Client{Dir: started.WorktreePath}).HeadSHA(context.Background())
		if err != nil {
			return fmt.Errorf("resolve task worktree head: %w", err)
		}
		recoverCmd := taskRecoverCommand(task.ID, *home, requestRepo, strings.TrimSpace(*owner))
		dirty, err := taskWorktreeDirty(context.Background(), started)
		if err != nil {
			return err
		}
		request := taskRunImplementJobRequest(taskRunImplementJobID(started.ID, strings.TrimSpace(*owner)), started, strings.TrimSpace(*owner), headSHA)
		if active, ok, err := findActiveTaskRunJob(context.Background(), store, request); err != nil {
			return err
		} else if ok {
			startedJob = active
			return nil
		}
		if dirty {
			return fmt.Errorf("task %s worktree has uncommitted changes at %s; inspect it, then run %s to commit/push/open a PR, or clean/stash it before retrying task run", started.ID, started.WorktreePath, recoverCmd)
		}
		startedJob, err = enqueueTaskRunImplementJob(context.Background(), store, started, strings.TrimSpace(*owner), headSHA, paths.Home)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "run task: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "started %s on %s\n", started.ID, started.Branch)
	if strings.TrimSpace(started.WorktreePath) != "" {
		fmt.Fprintf(stdout, "worktree: %s\n", started.WorktreePath)
	}
	if strings.TrimSpace(startedJob.ID) != "" {
		fmt.Fprintf(stdout, "job: %s\n", startedJob.ID)
		if startedJob.State == string(workflow.JobQueued) {
			fmt.Fprintf(stdout, "next: %s\n", jobRunCommand(startedJob.ID, *home))
		} else {
			fmt.Fprintf(stdout, "next: %s\n", jobWatchCommand(startedJob.ID, *home))
		}
	}
	return 0
}

type taskRecoverOutput struct {
	TaskID      string `json:"task_id"`
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	PullRequest int    `json:"pull_request"`
	HeadSHA     string `json:"head_sha"`
	Summary     string `json:"summary"`
}

func runTaskRecover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task recover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo scope as owner/repo")
	owner := fs.String("owner", "", "agent/operator attributed as recovery lead")
	skipFanout := fs.Bool("skip-native-review-fanout", false, "persist skip-native-review-fanout before opening the PR")
	jsonOutput := fs.Bool("json", false, "print recovery result as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "task recover requires exactly one id")
			return 2
		}
		return 0
	}
	taskID := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "task recover requires exactly one id")
		return 2
	}
	var output taskRecoverOutput
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		payload, err := recoverTaskImplementation(context.Background(), store, taskID, strings.TrimSpace(*repo), strings.TrimSpace(*owner), *skipFanout, nil)
		if err != nil {
			return err
		}
		output = taskRecoverOutput{
			TaskID:      payload.TaskID,
			Repo:        payload.Repo,
			Branch:      payload.Branch,
			PullRequest: payload.PullRequest,
			HeadSHA:     payload.HeadSHA,
			Summary:     "recovered implementation worktree and opened/adopted PR",
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "recover task: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "recover task: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "recovered %s on %s\n", output.TaskID, output.Branch)
	if output.PullRequest > 0 {
		fmt.Fprintf(stdout, "pull_request: #%d\n", output.PullRequest)
	}
	if output.HeadSHA != "" {
		fmt.Fprintf(stdout, "head_sha: %s\n", output.HeadSHA)
	}
	return 0
}

func recoverTaskImplementation(ctx context.Context, store *db.Store, taskID string, repoFlag string, owner string, skipFanout bool, gh github.Client) (workflow.JobPayload, error) {
	task, err := store.GetTask(ctx, strings.TrimSpace(taskID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workflow.JobPayload{}, fmt.Errorf("task %q not found", taskID)
		}
		return workflow.JobPayload{}, err
	}
	requestRepo := firstNonEmpty(repoFlag, task.RepoFullName)
	if strings.TrimSpace(requestRepo) == "" {
		return workflow.JobPayload{}, errors.New("task recover requires --repo when task has no repo")
	}
	if strings.TrimSpace(repoFlag) != "" && strings.TrimSpace(task.RepoFullName) != "" && repoFlag != task.RepoFullName {
		return workflow.JobPayload{}, fmt.Errorf("task %s belongs to repo %s, not %s", task.ID, task.RepoFullName, repoFlag)
	}
	if _, err := daemon.ParseRepository(requestRepo); err != nil {
		return workflow.JobPayload{}, fmt.Errorf("invalid repo: %w", err)
	}
	if _, err := store.GetRepo(ctx, requestRepo); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return workflow.JobPayload{}, err
		}
		return workflow.JobPayload{}, fmt.Errorf("repo %s is not registered; run gitmoot repo add from the checkout before task recover", requestRepo)
	}
	if strings.TrimSpace(task.WorktreePath) == "" {
		return workflow.JobPayload{}, errors.New("task has no worktree path; rerun through gitmoot task run or gitmoot agent implement")
	}
	headSHA, err := (gitutil.Client{Dir: task.WorktreePath}).HeadSHA(ctx)
	if err != nil {
		return workflow.JobPayload{}, fmt.Errorf("resolve task worktree head: %w", err)
	}
	if strings.TrimSpace(owner) == "" {
		owner = "task recover"
	}
	job := db.Job{
		ID:    taskRecoverJobID(task.ID, owner),
		Agent: owner,
		Type:  "implement",
	}
	payload := workflow.JobPayload{
		Repo:                   requestRepo,
		Branch:                 task.Branch,
		GoalID:                 task.GoalID,
		TaskID:                 task.ID,
		TaskTitle:              task.Title,
		LeadAgent:              owner,
		HeadSHA:                headSHA,
		SkipNativeReviewFanout: skipFanout,
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: gh}
	return finalizer.FinalizeImplementation(ctx, job, payload)
}

func taskWorktreeDirty(ctx context.Context, task db.Task) (bool, error) {
	if strings.TrimSpace(task.WorktreePath) == "" {
		return false, nil
	}
	status, err := (gitutil.Client{Dir: task.WorktreePath}).StatusPorcelain(ctx)
	if err != nil {
		return false, fmt.Errorf("inspect task worktree %s: %w", task.WorktreePath, err)
	}
	return strings.TrimSpace(status) != "", nil
}

func taskRecoverCommand(taskID string, home string, repo string, owner string) string {
	args := []string{"gitmoot", "task", "recover", taskID}
	if strings.TrimSpace(home) != "" {
		args = append(args, "--home", home)
	}
	if strings.TrimSpace(repo) != "" {
		args = append(args, "--repo", repo)
	}
	if strings.TrimSpace(owner) != "" {
		args = append(args, "--owner", owner)
	}
	return shellArgs(args)
}

func taskRecoverJobID(taskID string, owner string) string {
	return slug("task-" + taskID + "-recover-" + owner)
}

func enqueueTaskRunImplementJob(ctx context.Context, store *db.Store, task db.Task, owner string, headSHA string, home string) (db.Job, error) {
	baseJobID := taskRunImplementJobID(task.ID, owner)
	jobID := baseJobID
	request := taskRunImplementJobRequest(jobID, task, owner, headSHA)
	if existing, err := store.GetJob(ctx, jobID); err == nil {
		if (existing.State == string(workflow.JobQueued) || existing.State == string(workflow.JobRunning)) && taskRunJobMatchesRequest(existing, request) {
			return existing, nil
		}
		if active, ok, err := findActiveTaskRunJob(ctx, store, request); err != nil {
			return db.Job{}, err
		} else if ok {
			return active, nil
		}
		jobID = baseJobID + "-" + shortHash(existing.State+"\x00"+headSHA+"\x00"+time.Now().UTC().Format(time.RFC3339Nano))
		request = taskRunImplementJobRequest(jobID, task, owner, headSHA)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return db.Job{}, err
	}
	return (workflow.Mailbox{Store: store, CanaryEnabled: canaryRoutingEnabled(home)}).Enqueue(ctx, request)
}

func findActiveTaskRunJob(ctx context.Context, store *db.Store, request workflow.JobRequest) (db.Job, bool, error) {
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return db.Job{}, false, err
	}
	for _, job := range jobs {
		if job.State != string(workflow.JobQueued) && job.State != string(workflow.JobRunning) {
			continue
		}
		if taskRunJobMatchesRequest(job, request) {
			return job, true, nil
		}
	}
	return db.Job{}, false, nil
}

func taskRunImplementJobRequest(jobID string, task db.Task, owner string, headSHA string) workflow.JobRequest {
	title := strings.TrimSpace(task.Title)
	label := task.ID
	if title != "" {
		label += ": " + title
	}
	return workflow.JobRequest{
		ID:           jobID,
		Agent:        owner,
		Action:       "implement",
		Repo:         task.RepoFullName,
		Branch:       task.Branch,
		HeadSHA:      headSHA,
		GoalID:       task.GoalID,
		TaskID:       task.ID,
		TaskTitle:    task.Title,
		LeadAgent:    owner,
		Sender:       "task run",
		Instructions: "Implement task " + label + ".",
	}
}

func taskRunJobMatchesRequest(job db.Job, request workflow.JobRequest) bool {
	if job.Type != request.Action {
		return false
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return false
	}
	if job.Agent != request.Agent &&
		!(payload.DelegationReason == "runtime_session_busy" &&
			payload.DelegatedAgent == job.Agent &&
			payload.OriginalAgent == request.Agent) {
		return false
	}
	return payload.Repo == request.Repo &&
		payload.Branch == request.Branch &&
		payload.PullRequest == request.PullRequest &&
		payload.HeadSHA == request.HeadSHA &&
		payload.GoalID == request.GoalID &&
		payload.TaskID == request.TaskID &&
		payload.TaskTitle == request.TaskTitle &&
		payload.LeadAgent == request.LeadAgent &&
		payload.Sender == request.Sender &&
		payload.Instructions == request.Instructions
}

func taskRunImplementJobID(taskID string, owner string) string {
	value := slug("task-" + taskID + "-implement-" + owner)
	if value == "" {
		return "task-implement-" + shortHash(taskID+"\x00"+owner)
	}
	return value
}

func parseGoalFile(path string, repo string) (importedGoal, error) {
	file, err := os.Open(path)
	if err != nil {
		return importedGoal{}, err
	}
	defer file.Close()
	title := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	goalID := slug(title)
	if goalID == "" {
		goalID = "goal-" + shortHash(path)
	}
	scanner := bufio.NewScanner(file)
	tasks := []db.Task{}
	seenTaskIDs := map[string]bool{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "# ") && title == strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) {
			if heading := strings.TrimSpace(strings.TrimPrefix(line, "# ")); heading != "" {
				title = heading
			}
		}
		matches := taskHeadingPattern.FindStringSubmatch(line)
		if len(matches) != 3 {
			continue
		}
		number, err := strconv.Atoi(matches[1])
		if err != nil {
			return importedGoal{}, err
		}
		taskTitle := strings.TrimSpace(matches[2])
		taskID := fmt.Sprintf("task-%03d", number)
		if seenTaskIDs[taskID] {
			return importedGoal{}, fmt.Errorf("duplicate task heading %s", taskID)
		}
		seenTaskIDs[taskID] = true
		tasks = append(tasks, db.Task{
			ID:           taskID,
			RepoFullName: strings.TrimSpace(repo),
			GoalID:       goalID,
			Title:        taskTitle,
			State:        string(workflow.TaskPlanned),
			Branch:       taskBranchName(taskID, taskTitle),
		})
	}
	if err := scanner.Err(); err != nil {
		return importedGoal{}, err
	}
	if len(tasks) == 0 {
		return importedGoal{}, errors.New("goal file contains no task headings")
	}
	return importedGoal{
		Goal: db.Goal{
			ID:     goalID,
			Title:  title,
			Source: path,
			Status: "planned",
		},
		Tasks: tasks,
	}, nil
}

func validateImportConflicts(ctx context.Context, store *db.Store, tasks []db.Task) error {
	for _, task := range tasks {
		existing, err := store.GetTask(ctx, task.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if taskImportConflict(existing, task) {
			return fmt.Errorf("task %s already exists for goal %q repo %q; choose a different task number or remove the existing task before importing", task.ID, existing.GoalID, existing.RepoFullName)
		}
	}
	return nil
}

func taskImportConflict(existing db.Task, imported db.Task) bool {
	if existing.GoalID != imported.GoalID {
		return true
	}
	existingRepo := strings.TrimSpace(existing.RepoFullName)
	importedRepo := strings.TrimSpace(imported.RepoFullName)
	return existingRepo != "" && importedRepo != "" && existingRepo != importedRepo
}

func taskStateCounts(tasks []db.Task) map[string]int {
	counts := map[string]int{}
	for _, task := range tasks {
		state := strings.TrimSpace(task.State)
		if state == "" {
			state = "unknown"
		}
		counts[state]++
	}
	return counts
}

func countRepos(repos []db.Repo, repoFullName string) int {
	if strings.TrimSpace(repoFullName) == "" {
		return len(repos)
	}
	for _, repo := range repos {
		if repo.FullName() == repoFullName {
			return 1
		}
	}
	return 0
}

func countJobs(jobs []db.Job, repoFullName string) int {
	return len(filterJobsByRepo(jobs, repoFullName))
}

func jobStateCounts(jobs []db.Job, repoFullName string) map[string]int {
	counts := map[string]int{}
	for _, job := range filterJobsByRepo(jobs, repoFullName) {
		state := strings.TrimSpace(job.State)
		if state == "" {
			state = "unknown"
		}
		counts[state]++
	}
	return counts
}

func filterJobsByRepo(jobs []db.Job, repoFullName string) []db.Job {
	if strings.TrimSpace(repoFullName) == "" {
		return jobs
	}
	filtered := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		payload, err := daemonJobPayload(job)
		if err == nil && payload.Repo == repoFullName {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func sortedCountKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash && builder.Len() > 0 {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func taskBranchName(taskID string, title string) string {
	titleSlug := slug(title)
	if titleSlug == "" {
		return taskID
	}
	return taskID + "-" + titleSlug
}

func shortHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}
