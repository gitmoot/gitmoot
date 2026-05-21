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
	"unicode"

	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/workflow"
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
	var tasks []db.Task
	var prs []db.PullRequest
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		if agents, err = store.ListAgents(context.Background()); err != nil {
			return err
		}
		if goals, err = store.ListGoals(context.Background()); err != nil {
			return err
		}
		if strings.TrimSpace(*repo) == "" {
			if tasks, err = store.ListTasks(context.Background()); err != nil {
				return err
			}
		} else {
			if tasks, err = store.ListTasksByRepo(context.Background(), *repo); err != nil {
				return err
			}
		}
		prs, err = store.ListPullRequests(context.Background(), *repo)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "agents: %d\n", len(agents))
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
	return 0
}

func runGoal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printGoalUsage(stdout)
		return 0
	}
	if args[0] != "import" {
		fmt.Fprintf(stderr, "unknown goal command %q\n\n", args[0])
		printGoalUsage(stderr)
		return 2
	}
	return runGoalImport(args[1:], stdout, stderr)
}

func printGoalUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot goal import --file <path> [--repo owner/repo]")
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
	if args[0] != "run" {
		fmt.Fprintf(stderr, "unknown task command %q\n\n", args[0])
		printTaskUsage(stderr)
		return 2
	}
	return runTaskRun(args[1:], stdout, stderr)
}

func printTaskUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot task run <id> --repo owner/repo --owner <agent> [--branch <branch>] [--base <branch>]")
}

func runTaskRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo scope as owner/repo")
	owner := fs.String("owner", "", "agent that will hold the branch lock")
	branch := fs.String("branch", "", "task branch name")
	base := fs.String("base", "", "base branch for git switch -c")
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
	if err := withStore(*home, func(store *db.Store) error {
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
		checkout := repoRecord.CheckoutPath
		requestBranch := firstNonEmpty(*branch, task.Branch, task.ID)
		engine := workflow.Engine{Store: store}
		started, err = engine.StartTaskBranch(context.Background(), workflow.TaskBranchRequest{
			Repo:       requestRepo,
			GoalID:     task.GoalID,
			TaskID:     task.ID,
			TaskTitle:  task.Title,
			Branch:     requestBranch,
			BaseBranch: *base,
			Owner:      *owner,
		}, gitutil.Client{Dir: checkout})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "run task: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "started %s on %s\n", started.ID, started.Branch)
	return 0
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
