package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRunStatusIncludesUnscopedTasks(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	// A repo-less task row is the shape the status counters have to survive: the
	// review-cycle mint always names a repo, but historical and branchless rows
	// do not, and a repo-scoped status must still count them.
	if err := store.UpsertTask(context.Background(), db.Task{ID: "task-001", Title: "Bootstrap", State: "planned"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", "--home", home, "--repo", "gitmoot/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"prs: 1", "  planned: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("status output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "goals:") {
		t.Fatalf("status still reports goals:\n%s", output)
	}
}

func TestRunTaskList(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "review-pr-12-3f3a1026",
		RepoFullName: "gitmoot/gitmoot",
		GoalID:       "local-review",
		Title:        "Review PR #12",
		State:        "reviewing",
		Branch:       "fix/twelve",
		WorktreePath: "/tmp/gitmoot/worktrees/gitmoot--gitmoot/review-pr-12",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "review-pr-13-aa11bb22",
		RepoFullName: "gitmoot/gitmoot",
		GoalID:       "local-review",
		Title:        "Review PR #13",
		State:        "merged",
		Branch:       "fix/thirteen",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"pr", "list", "--home", home, "--repo", "gitmoot/gitmoot", "--state", "reviewing"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pr list exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "review-pr-12-3f3a1026\treviewing\tgitmoot/gitmoot#12\tfix/twelve\t/tmp/gitmoot/worktrees/gitmoot--gitmoot/review-pr-12\tReview PR #12") {
		t.Fatalf("pr list output = %q", output)
	}
	if strings.Contains(output, "review-pr-13") {
		t.Fatalf("pr list did not apply state filter:\n%s", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"pr", "list", "--home", home, "--repo", "gitmoot/gitmoot", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pr list --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var decoded []taskListOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if len(decoded) != 2 || decoded[0].ID != "review-pr-12-3f3a1026" || decoded[0].WorktreePath == "" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// TestPRListAndShowJoinTasksAndPullRequests pins #2600: a pr is one record read
// from both tables. The review record (tasks) and the GitHub record
// (pull_requests) join by the number in the id or by repo+branch; a pull
// request never reviewed is listed with no id, and an old row with no pull
// request is listed last under its own heading.
func TestPRListAndShowJoinTasksAndPullRequests(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	for _, task := range []db.Task{
		{ID: "review-pr-7-aa11bb22", RepoFullName: "o/r", GoalID: "local-review", Title: "Review PR #7", State: "dismissed", Branch: "feat/seven"},
		{ID: "review-pr-7-aa11bb22-s1", RepoFullName: "o/r", GoalID: "local-review", Title: "Review PR #7", State: "ready_to_merge"},
		{ID: "adhoc-old", RepoFullName: "o/r", Title: "old adhoc", State: "merged", Branch: "adhoc/old"},
		{ID: "task-branchbound", RepoFullName: "o/r", Title: "branch bound", State: "pr_open", Branch: "feat/nine"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatalf("UpsertTask(%s): %v", task.ID, err)
		}
	}
	for _, pr := range []db.PullRequest{
		{RepoFullName: "o/r", Number: 7, URL: "https://github.com/o/r/pull/7", HeadBranch: "feat/seven", BaseBranch: "main", HeadSHA: "sha7", State: "open"},
		{RepoFullName: "o/r", Number: 8, URL: "https://github.com/o/r/pull/8", HeadBranch: "feat/eight", BaseBranch: "main", HeadSHA: "sha8", State: "closed"},
		{RepoFullName: "o/r", Number: 9, URL: "https://github.com/o/r/pull/9", HeadBranch: "feat/nine", BaseBranch: "main", HeadSHA: "sha9", State: "open"},
	} {
		if err := store.UpsertPullRequest(ctx, pr); err != nil {
			t.Fatalf("UpsertPullRequest(%d): %v", pr.Number, err)
		}
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pr", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pr list exit = %d, stderr=%s", code, stderr.String())
	}
	var listed []taskListOutput
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	var got []string
	for _, record := range listed {
		got = append(got, fmt.Sprintf("%s|%d|%s|%s", record.ID, record.Number, record.State, record.GitHubState))
	}
	want := []string{
		"review-pr-7-aa11bb22|7|dismissed|open",
		"review-pr-7-aa11bb22-s1|7|ready_to_merge|open",
		"|8||closed",
		"task-branchbound|9|pr_open|open",
		"adhoc-old|0|merged|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pr list records =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	stdout.Reset()
	if code := Run([]string{"pr", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pr list exit = %d, stderr=%s", code, stderr.String())
	}
	text := stdout.String()
	heading := strings.Index(text, "# rows without a pull request")
	if heading < 0 || heading > strings.Index(text, "adhoc-old") || heading < strings.Index(text, "task-branchbound") {
		t.Fatalf("pr list text must list rows without a pull request last, under a heading:\n%s", text)
	}

	stdout.Reset()
	if code := Run([]string{"pr", "show", "o/r#7", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pr show exit = %d, stderr=%s", code, stderr.String())
	}
	var shown taskListOutput
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if shown.ID != "review-pr-7-aa11bb22-s1" || shown.State != "ready_to_merge" || shown.URL != "https://github.com/o/r/pull/7" ||
		shown.HeadSHA != "sha7" || shown.BaseBranch != "main" || len(shown.OtherIDs) != 1 || shown.OtherIDs[0] != "review-pr-7-aa11bb22" {
		t.Fatalf("pr show = %+v, want the live successor joined to PR #7 with the disposed record listed", shown)
	}

	stdout.Reset()
	if code := Run([]string{"pr", "show", "8", "--repo", "o/r", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pr show 8 exit = %d, stderr=%s", code, stderr.String())
	}
	for _, line := range []string{"pr: o/r#8\n", "id: -\n", "github_state: closed\n", "url: https://github.com/o/r/pull/8\n"} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("pr show 8 missing %q:\n%s", line, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pr", "events", "o/r#8", "--home", home}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "never reviewed") {
		t.Fatalf("pr events on an unreviewed pr: code=%d stderr=%s", code, stderr.String())
	}
}

// TestTaskCommandAliasStillWorks pins the one-release transition alias.
func TestTaskCommandAliasStillWorks(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if err := store.UpsertTask(context.Background(), db.Task{ID: "review-pr-3-ab", RepoFullName: "o/r", State: "reviewing"}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	store.Close()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"task", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("task list alias exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "review-pr-3-ab") || !strings.Contains(stderr.String(), `"task" is now "pr"`) {
		t.Fatalf("task alias: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestTaskBranchNameFallsBackToTaskID(t *testing.T) {
	if got := taskBranchName("task-001", "!!!"); got != "task-001" {
		t.Fatalf("taskBranchName returned %q, want task-001", got)
	}
}

func subscribeShellImplementAgent(t *testing.T, home string, name string, repo string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"reviewer", "subscribe", name,
		"--home", home,
		"--runtime", "shell",
		"--session", `printf '%s\n' '{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}'`,
		"--role", "lead",
		"--repo", repo,
		"--capability", "implement",
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent subscribe exit code = %d, stderr=%s", code, stderr.String())
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func seedGitHead(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir,
		"-c", "user.name=Gitmoot Test",
		"-c", "user.email=gitmoot@example.invalid",
		"commit", "--allow-empty", "-m", "initial",
	)
}

func withWorkingDirectory(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// runPRJSON runs a pr subcommand with --json and decodes one record.
func runPRJSON(t *testing.T, args ...string) (taskListOutput, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(append(args, "--json"), &stdout, &stderr)
	var record taskListOutput
	if code == 0 {
		if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
			t.Fatalf("%v: decode %v\n%s", args, err, stdout.String())
		}
	}
	return record, code, stderr.String()
}

// TestPRShowAndEventsJoinReusedBranchLikeList pins that `pr show` and
// `pr events` join a branch-bound record to the same pull request `pr list`
// does: when two pull requests share a head branch, the newest one owns it,
// so the older pull request has no review record of its own.
func TestPRShowAndEventsJoinReusedBranchLikeList(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-reused", RepoFullName: "o/r", Title: "reused", State: "pr_open", Branch: "feat/reused"}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{TaskID: "task-reused", Kind: "note", Reason: "event of the record on #12"}); err != nil {
		t.Fatalf("AddTaskEvent: %v", err)
	}
	for _, pr := range []db.PullRequest{
		{RepoFullName: "o/r", Number: 11, HeadBranch: "feat/reused", BaseBranch: "main", HeadSHA: "sha11", State: "closed"},
		{RepoFullName: "o/r", Number: 12, HeadBranch: "feat/reused", BaseBranch: "main", HeadSHA: "sha12", State: "open"},
	} {
		if err := store.UpsertPullRequest(ctx, pr); err != nil {
			t.Fatalf("UpsertPullRequest(%d): %v", pr.Number, err)
		}
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pr", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pr list exit = %d, stderr=%s", code, stderr.String())
	}
	var listed []taskListOutput
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	for _, record := range listed {
		if record.ID == "task-reused" && record.Number != 12 {
			t.Fatalf("pr list joined task-reused to #%d, want #12 (newest pull request on the branch)", record.Number)
		}
	}

	if shown, code, errText := runPRJSON(t, "pr", "show", "o/r#12", "--home", home); code != 0 || shown.ID != "task-reused" {
		t.Fatalf("pr show o/r#12 = %+v code=%d stderr=%s, want task-reused", shown, code, errText)
	}
	if shown, code, errText := runPRJSON(t, "pr", "show", "o/r#11", "--home", home); code != 0 || shown.ID != "" || shown.GitHubState != "closed" {
		t.Fatalf("pr show o/r#11 = %+v code=%d stderr=%s, want the unreviewed closed pull request (list gives task-reused to #12)", shown, code, errText)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pr", "events", "o/r#11", "--home", home}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "never reviewed") {
		t.Fatalf("pr events o/r#11: code=%d stdout=%q stderr=%q, want never reviewed (not #12's events)", code, stdout.String(), stderr.String())
	}
}

// TestPRShowRepoQualifiedIgnoresUnscopedRecords pins that a record with no
// repo never answers an owner/repo#N lookup: it is listed, but it neither
// invents a pull request nor outranks the repo's own record.
func TestPRShowRepoQualifiedIgnoresUnscopedRecords(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	for _, task := range []db.Task{
		{ID: "review-pr-20-unscoped", Title: "unscoped twenty", State: "reviewing"},
		{ID: "review-pr-21-unscoped", Title: "unscoped twenty-one", State: "reviewing"},
		{ID: "review-pr-21-own", RepoFullName: "o/r", Title: "own twenty-one", State: "dismissed", Branch: "feat/twenty-one"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatalf("UpsertTask(%s): %v", task.ID, err)
		}
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "o/r", Number: 21, HeadBranch: "feat/twenty-one", BaseBranch: "main", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pr", "list", "--home", home, "--repo", "o/r"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "review-pr-20-unscoped") {
		t.Fatalf("pr list --repo must still list unscoped rows: code=%d\n%s", code, stdout.String())
	}
	if shown, code, _ := runPRJSON(t, "pr", "show", "o/r#20", "--home", home); code != 1 {
		t.Fatalf("pr show o/r#20 = %+v code=%d, want not found (the only #20 record has no repo)", shown, code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pr", "events", "20", "--repo", "o/r", "--home", home}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("pr events o/r#20: code=%d stderr=%q, want not found", code, stderr.String())
	}
	shown, code, errText := runPRJSON(t, "pr", "show", "o/r#21", "--home", home)
	if code != 0 || shown.ID != "review-pr-21-own" || shown.Repo != "o/r" || len(shown.OtherIDs) != 0 {
		t.Fatalf("pr show o/r#21 = %+v code=%d stderr=%s, want the repo's own (disposed) record, no unscoped one", shown, code, errText)
	}
}

// TestPRShowPicksNewestSuccessorGeneration pins that when every record of a
// successor chain is disposed, `pr show` and `pr events` present the newest
// generation the successor command recorded, not the lexically last id
// (-successor-9 sorts after -successor-10).
func TestPRShowPicksNewestSuccessorGeneration(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	disposed := []string{string(workflow.TaskDismissed), string(workflow.TaskSuperseded), string(workflow.TaskStranded)}
	root := "review-pr-40-abc"
	if err := store.UpsertTask(ctx, db.Task{ID: root, RepoFullName: "o/r", GoalID: "local-review", Title: "Review PR #40", State: string(workflow.TaskReviewing), Branch: "feat/forty"}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "o/r", Number: 40, HeadBranch: "feat/forty", BaseBranch: "main", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	latest := root
	from := string(workflow.TaskReviewing)
	for {
		if ok, _, err := store.DisposeTask(ctx, latest, []string{from}, string(workflow.TaskDismissed), "test", "test disposal", "", "", time.Now().UTC()); err != nil || !ok {
			t.Fatalf("DisposeTask(%s): ok=%v err=%v", latest, ok, err)
		}
		if latest == root+"-successor-10" {
			break
		}
		successor, created, err := store.CreateTaskSuccessor(ctx, root, disposed, string(workflow.TaskPlanned), "test")
		if err != nil || !created {
			t.Fatalf("CreateTaskSuccessor after %s: created=%v err=%v", latest, created, err)
		}
		latest, from = successor.ID, string(workflow.TaskPlanned)
	}
	store.Close()

	newest := root + "-successor-10"
	shown, code, errText := runPRJSON(t, "pr", "show", "o/r#40", "--home", home)
	if code != 0 || shown.ID != newest || len(shown.OtherIDs) != 9 {
		t.Fatalf("pr show o/r#40 = id %q others %v code=%d stderr=%s, want %s with 9 others", shown.ID, shown.OtherIDs, code, errText, newest)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pr", "events", "o/r#40", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pr events exit = %d, stderr=%s", code, stderr.String())
	}
	var events []prEventOutput
	if err := json.Unmarshal(stdout.Bytes(), &events); err != nil || len(events) == 0 {
		t.Fatalf("decode events: %v\n%s", err, stdout.String())
	}
	for _, event := range events {
		if event.TaskID != newest {
			t.Fatalf("pr events read %s's events, want %s's", event.TaskID, newest)
		}
	}
}
