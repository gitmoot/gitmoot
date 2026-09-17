package cli

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

var taskWorktreeHasLiveProcess = workflow.WorktreeHasLiveProcess
var taskWorktreeLiveness = workflow.WorktreeLiveness

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

func runTask(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printTaskUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runTaskList(args[1:], stdout, stderr)
	case "events":
		return runTaskEvents(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown task command %q\n\n", args[0])
		printTaskUsage(stderr)
		return 2
	}
}

func printTaskUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot task events <id> [--json]")
	fmt.Fprintln(w, "  gitmoot task list [--repo owner/repo] [--state state] [--json]")
}

type taskListOutput struct {
	ID                     string `json:"id"`
	Repo                   string `json:"repo"`
	GoalID                 string `json:"goal_id"`
	Title                  string `json:"title"`
	State                  string `json:"state"`
	Branch                 string `json:"branch"`
	WorktreePath           string `json:"worktree_path"`
	DisposalTier           string `json:"disposal_tier,omitempty"`
	DisposalReason         string `json:"disposal_reason,omitempty"`
	DisposedAt             string `json:"disposed_at,omitempty"`
	DisposalEscalationRole string `json:"disposal_escalation_role,omitempty"`
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
			ID:                     task.ID,
			Repo:                   task.RepoFullName,
			GoalID:                 task.GoalID,
			Title:                  task.Title,
			State:                  task.State,
			Branch:                 task.Branch,
			WorktreePath:           task.WorktreePath,
			DisposalTier:           task.DisposalTier,
			DisposalReason:         task.DisposalReason,
			DisposedAt:             task.DisposedAt,
			DisposalEscalationRole: task.DisposalEscalationRole,
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

func runTaskEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print task events as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "task events requires exactly one id")
			return 2
		}
		return 0
	}
	taskID := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || taskID == "" {
		fmt.Fprintln(stderr, "task events requires exactly one id")
		return 2
	}
	var events []db.TaskEvent
	if err := withStore(*home, func(store *db.Store) error {
		if _, err := store.GetTask(context.Background(), taskID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("task %q not found", taskID)
			}
			return err
		}
		var err error
		events, err = store.ListTaskEvents(context.Background(), taskID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "task events: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, events); err != nil {
			fmt.Fprintf(stderr, "task events: %v\n", err)
			return 1
		}
		return 0
	}
	for _, event := range events {
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%s\t%s\n", event.ID, event.CreatedAt, event.Kind, event.FromState, event.ToState, event.Reason)
	}
	return 0
}

func taskWorktreeDirtyWithRunner(ctx context.Context, task db.Task, runner subprocess.Runner) (bool, error) {
	if strings.TrimSpace(task.WorktreePath) == "" {
		return false, nil
	}
	status, err := jobGitClient(task.WorktreePath, runner).StatusPorcelain(ctx)
	if err != nil {
		return false, fmt.Errorf("inspect task worktree %s: %w", task.WorktreePath, err)
	}
	return strings.TrimSpace(status) != "", nil
}

func taskBranchReusableForImplement(state string) bool {
	switch workflow.TaskState(strings.TrimSpace(state)) {
	case "", workflow.TaskPlanned, workflow.TaskImplementing, workflow.TaskChangesRequested, workflow.TaskBlocked, workflow.TaskAwaitingHuman:
		return true
	default:
		return false
	}
}

func findActiveImplementJobForTask(ctx context.Context, store *db.Store, repo string, branch string, taskID string) (db.Job, bool, error) {
	return findActiveJobMatching(ctx, store, repo, branch, func(job db.Job, payload workflow.JobPayload) bool {
		return job.Type == "implement" && payload.TaskID == taskID
	})
}

// findActiveJobForBranch returns the first queued/running job whose structured
// payload targets repo+branch, regardless of job type or task attribution. The
// merge gate uses this broader branch ownership check so ask/review fix jobs are
// protected just like implement jobs.
func findActiveJobForBranch(ctx context.Context, store *db.Store, repo string, branch string) (db.Job, bool, error) {
	if strings.TrimSpace(branch) == "" {
		return db.Job{}, false, nil
	}
	return findActiveJobMatching(ctx, store, repo, branch, func(db.Job, workflow.JobPayload) bool { return true })
}

func findActiveJobMatching(ctx context.Context, store *db.Store, repo string, branch string, matches func(db.Job, workflow.JobPayload) bool) (db.Job, bool, error) {
	jobs, err := store.ListActiveJobs(ctx)
	if err != nil {
		return db.Job{}, false, err
	}
	for _, job := range jobs {
		payload, err := daemonJobPayload(job)
		if err != nil {
			continue
		}
		if payload.Repo == repo && payload.Branch == branch && matches(job, payload) {
			return job, true, nil
		}
	}
	return db.Job{}, false, nil
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
