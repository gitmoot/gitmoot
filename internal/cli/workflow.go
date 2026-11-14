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
	"strconv"
	"strings"
	"unicode"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
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
		jobs, err = store.ListJobs(context.Background())
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
	fmt.Fprintf(stdout, "reviewers: %d\n", len(agents))
	for _, agent := range agents {
		fmt.Fprintf(stdout, "  %s: %s %s\n", agent.Name, agent.Runtime, strings.Join(agent.Capabilities, ","))
	}
	// prs counts review records (the tasks table) by review state; github_prs
	// counts every pull request the daemon has seen, reviewed or not (#2600).
	fmt.Fprintf(stdout, "prs: %d\n", len(tasks))
	counts := taskStateCounts(tasks)
	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, state)
	}
	sort.Strings(states)
	for _, state := range states {
		fmt.Fprintf(stdout, "  %s: %d\n", state, counts[state])
	}
	fmt.Fprintf(stdout, "github_prs: %d\n", len(prs))
	filteredJobCount := countJobs(jobs, repoFullName)
	jobCounts := jobStateCounts(jobs, repoFullName)
	fmt.Fprintf(stdout, "runs: %d\n", filteredJobCount)
	for _, state := range sortedCountKeys(jobCounts) {
		fmt.Fprintf(stdout, "  %s: %d\n", state, jobCounts[state])
	}
	return 0
}

// runTask implements `gitmoot pr` (#2600, formerly `gitmoot task`). A pr is
// one record presented from two tables that stay separate: the review record
// in tasks (id, review state, worktree, disposal) and the GitHub record in
// pull_requests (number, url, heads, GitHub state).
func runTask(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printTaskUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runTaskList(args[1:], stdout, stderr)
	case "show":
		return runPRShow(args[1:], stdout, stderr)
	case "events":
		return runTaskEvents(args[1:], stdout, stderr)
	case "successor":
		return runTaskSuccessor(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown pr command %q\n\n", args[0])
		printTaskUsage(stderr)
		return 2
	}
}

func printTaskUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot pr events <pr> [--repo owner/repo] [--json]")
	fmt.Fprintln(w, "  gitmoot pr list [--repo owner/repo] [--state state] [--json]")
	fmt.Fprintln(w, "  gitmoot pr show <pr> [--repo owner/repo] [--json]")
	fmt.Fprintln(w, "  gitmoot pr successor <id> [--reason text] [--json]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "<pr> is a pr id (review-pr-N-...), owner/repo#N, or N with --repo.")
	fmt.Fprintln(w, "pr list puts rows that have no pull request (old adhoc-/task- rows) last.")
}

// runTaskSuccessor creates the successor of a disposed (stranded, superseded,
// dismissed) pr record, so its work can continue without resurrecting the
// disposed row (#2277). Dispatch binds to the newest live record of the chain.
// Repeating the command returns the existing live successor.
func runTaskSuccessor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr successor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	reason := fs.String("reason", "", "why the work continues (recorded on both pr records)")
	jsonOutput := fs.Bool("json", false, "print the successor as JSON")
	if helpRequested(args) {
		printTaskUsage(stdout)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "pr successor requires exactly one id")
		return 2
	}
	taskID := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || taskID == "" {
		fmt.Fprintln(stderr, "pr successor requires exactly one id")
		return 2
	}
	if refuseFlagLikeText("pr successor", *reason, stderr) {
		return 2
	}
	disposed := []string{string(workflow.TaskDismissed), string(workflow.TaskSuperseded), string(workflow.TaskStranded)}
	var successor taskListOutput
	var created bool
	if err := withStore(*home, func(store *db.Store) error {
		task, isNew, err := store.CreateTaskSuccessor(context.Background(), taskID, disposed, string(workflow.TaskPlanned), *reason)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("pr %q not found", taskID)
		}
		if err != nil {
			return err
		}
		created = isNew
		successor, err = prRecordForTask(context.Background(), store, task)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pr successor: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, struct {
			PR      taskListOutput `json:"pr"`
			Created bool           `json:"created"`
		}{successor, created}); err != nil {
			fmt.Fprintf(stderr, "pr successor: %v\n", err)
			return 1
		}
		return 0
	}
	verb := "created"
	if !created {
		verb = "already exists"
	}
	fmt.Fprintf(stdout, "successor %s %s for %s; dispatch the review again and it binds to %s\n", successor.ID, verb, taskID, successor.ID)
	return 0
}

// taskListOutput is one pr as `gitmoot pr list|show` print it: the review
// record (tasks) joined to its GitHub record (pull_requests). ID is empty for
// a pull request gitmoot never reviewed; Number is 0 for an old row that has
// no pull request.
type taskListOutput struct {
	ID                     string   `json:"id"`
	Repo                   string   `json:"repo"`
	Number                 int64    `json:"number,omitempty"`
	URL                    string   `json:"url,omitempty"`
	GoalID                 string   `json:"goal_id"`
	Title                  string   `json:"title"`
	State                  string   `json:"state"`
	GitHubState            string   `json:"github_state,omitempty"`
	Branch                 string   `json:"branch"`
	BaseBranch             string   `json:"base_branch,omitempty"`
	HeadSHA                string   `json:"head_sha,omitempty"`
	MergeCommitSHA         string   `json:"merge_commit_sha,omitempty"`
	WorktreePath           string   `json:"worktree_path"`
	DisposalTier           string   `json:"disposal_tier,omitempty"`
	DisposalReason         string   `json:"disposal_reason,omitempty"`
	DisposedAt             string   `json:"disposed_at,omitempty"`
	DisposalEscalationRole string   `json:"disposal_escalation_role,omitempty"`
	OtherIDs               []string `json:"other_ids,omitempty"`
}

// displayState is the review state, or the GitHub state for a pull request
// that has no review record.
func (record taskListOutput) displayState() string {
	return firstNonEmpty(record.State, record.GitHubState)
}

func (record taskListOutput) reference() string {
	if record.Number > 0 {
		return fmt.Sprintf("%s#%d", record.Repo, record.Number)
	}
	return record.Repo
}

func prRecordFromTask(task db.Task) taskListOutput {
	number, _ := workflow.ReviewRecordPRNumber(task.ID)
	return taskListOutput{
		ID:                     task.ID,
		Repo:                   task.RepoFullName,
		Number:                 number,
		GoalID:                 task.GoalID,
		Title:                  task.Title,
		State:                  task.State,
		Branch:                 task.Branch,
		WorktreePath:           task.WorktreePath,
		DisposalTier:           task.DisposalTier,
		DisposalReason:         task.DisposalReason,
		DisposedAt:             task.DisposedAt,
		DisposalEscalationRole: task.DisposalEscalationRole,
	}
}

func prRecordFromPullRequest(pr db.PullRequest) taskListOutput {
	record := taskListOutput{Repo: pr.RepoFullName}
	record.addPullRequest(pr)
	return record
}

func (record *taskListOutput) addPullRequest(pr db.PullRequest) {
	record.Number = pr.Number
	record.URL = pr.URL
	record.GitHubState = pr.State
	record.Branch = firstNonEmpty(record.Branch, pr.HeadBranch)
	record.BaseBranch = pr.BaseBranch
	record.HeadSHA = pr.HeadSHA
	record.MergeCommitSHA = pr.MergeCommitSHA
}

type prRecordKey struct {
	repo   string
	number int64
}

// prRecords joins review records to pull requests: by the number in the
// record id, else by repo and head branch (the newest pull request wins, as in
// GetPullRequestByRepoBranch), and adds the pull requests no review record
// matched. Records with a pull request sort by repo and number; rows without
// one come last. `pr list`, `pr show` and `pr events` all join through here so
// they agree on which pull request a record belongs to.
func prRecords(tasks []db.Task, prs []db.PullRequest) []taskListOutput {
	byNumber := make(map[prRecordKey]db.PullRequest, len(prs))
	byBranch := make(map[[2]string]db.PullRequest, len(prs))
	for _, pr := range prs {
		byNumber[prRecordKey{pr.RepoFullName, pr.Number}] = pr
		if branch := strings.TrimSpace(pr.HeadBranch); branch != "" {
			if existing, ok := byBranch[[2]string{pr.RepoFullName, branch}]; !ok || pr.Number > existing.Number {
				byBranch[[2]string{pr.RepoFullName, branch}] = pr
			}
		}
	}
	records := make([]taskListOutput, 0, len(tasks))
	reviewed := make(map[prRecordKey]bool, len(tasks))
	for _, task := range tasks {
		record := prRecordFromTask(task)
		var pr db.PullRequest
		var ok bool
		if record.Number > 0 {
			pr, ok = byNumber[prRecordKey{task.RepoFullName, record.Number}]
		} else if branch := strings.TrimSpace(task.Branch); branch != "" {
			pr, ok = byBranch[[2]string{task.RepoFullName, branch}]
		}
		if ok {
			record.addPullRequest(pr)
		}
		if record.Number > 0 {
			reviewed[prRecordKey{record.Repo, record.Number}] = true
		}
		records = append(records, record)
	}
	for _, pr := range prs {
		if !reviewed[prRecordKey{pr.RepoFullName, pr.Number}] {
			records = append(records, prRecordFromPullRequest(pr))
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if (a.Number > 0) != (b.Number > 0) {
			return a.Number > 0
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Number != b.Number {
			return a.Number < b.Number
		}
		return a.ID < b.ID
	})
	return records
}

// prRecordForTask joins one review record to its pull request.
func prRecordForTask(ctx context.Context, store *db.Store, task db.Task) (taskListOutput, error) {
	record := prRecordFromTask(task)
	var pr db.PullRequest
	var err error
	switch {
	case record.Number > 0:
		pr, err = store.GetPullRequest(ctx, task.RepoFullName, record.Number)
	case strings.TrimSpace(task.Branch) != "" && strings.TrimSpace(task.RepoFullName) != "":
		pr, err = store.GetPullRequestByRepoBranch(ctx, task.RepoFullName, strings.TrimSpace(task.Branch))
	default:
		return record, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return record, nil
	}
	if err != nil {
		return record, err
	}
	record.addPullRequest(pr)
	return record, nil
}

// resolvePRRecord resolves a <pr> argument: a pr id (review-pr-N-...),
// owner/repo#N, or N with repo. A repo-qualified <pr> joins the repo's records
// and pull requests exactly as `pr list` does, then keeps the records of that
// repo and number; records with no repo never match one. When several review
// records belong to the pull request (a successor chain), the record is the
// newest of its chain, live preferred, and the others are listed in OtherIDs.
func resolvePRRecord(ctx context.Context, store *db.Store, ref string, repo string) (taskListOutput, error) {
	ref = strings.TrimSpace(ref)
	repoPart, numberPart, hasHash := strings.Cut(ref, "#")
	if !hasHash {
		if !isAllDigits(ref) {
			task, err := store.GetTask(ctx, ref)
			if errors.Is(err, sql.ErrNoRows) {
				return taskListOutput{}, fmt.Errorf("pr %q not found", ref)
			}
			if err != nil {
				return taskListOutput{}, err
			}
			return prRecordForTask(ctx, store, task)
		}
		if strings.TrimSpace(repo) == "" {
			return taskListOutput{}, fmt.Errorf("pr %q is a bare number; pass --repo owner/repo or use owner/repo#%s", ref, ref)
		}
		repoPart, numberPart = repo, ref
	}
	fullName, err := normalizeRepoFlag(repoPart)
	if err != nil {
		return taskListOutput{}, fmt.Errorf("invalid pr %q: %w", ref, err)
	}
	number, err := strconv.ParseInt(strings.TrimSpace(numberPart), 10, 64)
	if err != nil || number <= 0 {
		return taskListOutput{}, fmt.Errorf("invalid pr %q: want owner/repo#N", ref)
	}
	tasks, err := store.ListTasksByRepo(ctx, fullName)
	if err != nil {
		return taskListOutput{}, err
	}
	prs, err := store.ListPullRequests(ctx, fullName)
	if err != nil {
		return taskListOutput{}, err
	}
	var matches []taskListOutput
	for _, record := range prRecords(tasks, prs) {
		if record.Repo == fullName && record.Number == number {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return taskListOutput{}, fmt.Errorf("pr %s#%d not found", fullName, number)
	}
	primary, err := primaryPRRecord(ctx, store, matches)
	if err != nil {
		return taskListOutput{}, err
	}
	record := matches[primary]
	for i, other := range matches {
		if i != primary && other.ID != "" {
			record.OtherIDs = append(record.OtherIDs, other.ID)
		}
	}
	return record, nil
}

// primaryPRRecord picks which of one pull request's records `pr show` and
// `pr events` present. Only the newest record of a successor chain (by the
// chain the successor command records, not by id order, since -successor-10
// sorts before -successor-9) is a candidate; a live candidate wins over a
// disposed one.
func primaryPRRecord(ctx context.Context, store *db.Store, matches []taskListOutput) (int, error) {
	candidates := make([]int, 0, len(matches))
	for i, record := range matches {
		if record.ID == "" {
			continue
		}
		latest, err := store.LatestTaskInChain(ctx, record.ID)
		if err != nil {
			return 0, err
		}
		if latest.ID == record.ID {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return len(matches) - 1, nil
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		if !workflow.IsDisposedTaskState(matches[candidates[i]].State) {
			return candidates[i], nil
		}
	}
	return candidates[len(candidates)-1], nil
}

func runTaskList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo scope as owner/repo")
	state := fs.String("state", "", "pr state filter (review state; GitHub state for a pr with no review)")
	jsonOutput := fs.Bool("json", false, "print prs as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pr list does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*repo) != "" {
		if _, err := normalizeRepoFlag(*repo); err != nil {
			fmt.Fprintf(stderr, "invalid repo: %v\n", err)
			return 2
		}
	}

	var tasks []db.Task
	var prs []db.PullRequest
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		if strings.TrimSpace(*repo) != "" {
			tasks, err = store.ListTasksByRepo(context.Background(), strings.TrimSpace(*repo))
		} else {
			tasks, err = store.ListTasks(context.Background())
		}
		if err != nil {
			return err
		}
		prs, err = store.ListPullRequests(context.Background(), strings.TrimSpace(*repo))
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pr list: %v\n", err)
		return 1
	}
	outputs := []taskListOutput{}
	stateFilter := strings.TrimSpace(*state)
	for _, record := range prRecords(tasks, prs) {
		if stateFilter != "" && record.displayState() != stateFilter {
			continue
		}
		outputs = append(outputs, record)
	}
	if *jsonOutput {
		if err := writeJSON(stdout, outputs); err != nil {
			fmt.Fprintf(stderr, "pr list: %v\n", err)
			return 1
		}
		return 0
	}
	withoutPR := false
	for _, record := range outputs {
		if record.Number == 0 && !withoutPR {
			withoutPR = true
			fmt.Fprintln(stdout, "# rows without a pull request (old adhoc-/task- records):")
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\n", firstNonEmpty(record.ID, "-"), record.displayState(), record.reference(), record.Branch, record.WorktreePath, record.Title)
	}
	return 0
}

func runPRShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo for a bare pull request number")
	jsonOutput := fs.Bool("json", false, "print the pr as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pr show requires exactly one pr")
			return 2
		}
		return 0
	}
	ref := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || ref == "" {
		fmt.Fprintln(stderr, "pr show requires exactly one pr")
		return 2
	}
	var record taskListOutput
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		record, err = resolvePRRecord(context.Background(), store, ref, *repo)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pr show: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, record); err != nil {
			fmt.Fprintf(stderr, "pr show: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "pr: %s\n", record.reference())
	fmt.Fprintf(stdout, "id: %s\n", firstNonEmpty(record.ID, "-"))
	fmt.Fprintf(stdout, "state: %s\n", firstNonEmpty(record.State, "-"))
	for _, field := range [][2]string{
		{"github_state", record.GitHubState},
		{"url", record.URL},
		{"title", record.Title},
		{"branch", record.Branch},
		{"base_branch", record.BaseBranch},
		{"head_sha", record.HeadSHA},
		{"merge_commit_sha", record.MergeCommitSHA},
		{"worktree", record.WorktreePath},
		{"disposal_tier", record.DisposalTier},
		{"disposal_reason", record.DisposalReason},
		{"disposed_at", record.DisposedAt},
		{"disposal_escalation_role", record.DisposalEscalationRole},
		{"other_ids", strings.Join(record.OtherIDs, ", ")},
	} {
		if strings.TrimSpace(field[1]) != "" {
			fmt.Fprintf(stdout, "%s: %s\n", field[0], field[1])
		}
	}
	return 0
}

// prEventOutput is one review-state transition of a pr (task_events). It
// mirrors db.TaskEvent field for field so the two convert; only the JSON key
// of the record id differs (pr_id; the column stays task_id).
type prEventOutput struct {
	ID        int64  `json:"id"`
	TaskID    string `json:"pr_id"`
	Kind      string `json:"kind"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
}

func runTaskEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo for a bare pull request number")
	jsonOutput := fs.Bool("json", false, "print pr events as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pr events requires exactly one pr")
			return 2
		}
		return 0
	}
	ref := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || ref == "" {
		fmt.Fprintln(stderr, "pr events requires exactly one pr")
		return 2
	}
	var events []db.TaskEvent
	if err := withStore(*home, func(store *db.Store) error {
		record, err := resolvePRRecord(context.Background(), store, ref, *repo)
		if err != nil {
			return err
		}
		if record.ID == "" {
			return fmt.Errorf("pr %s was never reviewed, so it has no events", record.reference())
		}
		events, err = store.ListTaskEvents(context.Background(), record.ID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pr events: %v\n", err)
		return 1
	}
	if *jsonOutput {
		outputs := make([]prEventOutput, 0, len(events))
		for _, event := range events {
			outputs = append(outputs, prEventOutput(event))
		}
		if err := writeJSON(stdout, outputs); err != nil {
			fmt.Fprintf(stderr, "pr events: %v\n", err)
			return 1
		}
		return 0
	}
	for _, event := range events {
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%s\t%s\n", event.ID, event.CreatedAt, event.Kind, event.FromState, event.ToState, event.Reason)
	}
	return 0
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

// normalizeOptionalRepoFlag returns "" for an empty --repo flag and the
// canonical owner/repo otherwise, reporting an invalid value on stderr.
func normalizeOptionalRepoFlag(value string, stderr io.Writer) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	repo, err := github.ParseRepository(value)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return "", false
	}
	return repo.FullName(), true
}
