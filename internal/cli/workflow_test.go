package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestRunGoalImportAndStatus(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, `# Build Gitmoot

### Task 1: Bootstrap Runtime

Set up the runtime adapter.

### Task 10: Docs & Status

Document the workflow.
`)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("goal import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported goal goal with 2 tasks") {
		t.Fatalf("goal import output = %q", stdout.String())
	}

	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	goals, err := store.ListGoals(context.Background())
	if err != nil {
		t.Fatalf("ListGoals returned error: %v", err)
	}
	if len(goals) != 1 {
		t.Fatalf("goals len = %d, want 1", len(goals))
	}
	if goals[0].ID != "goal" || goals[0].Title != "Build Gitmoot" || goals[0].Status != "planned" {
		t.Fatalf("goal = %+v", goals[0])
	}

	task, err := store.GetTask(context.Background(), "task-001")
	if err != nil {
		t.Fatalf("GetTask task-001 returned error: %v", err)
	}
	if task.RepoFullName != "jerryfane/gitmoot" || task.GoalID != "goal" || task.Branch != "task-001-bootstrap-runtime" {
		t.Fatalf("task-001 = %+v", task)
	}
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "other-task",
		RepoFullName: "jerryfane/other",
		GoalID:       "other",
		Title:        "Other",
		State:        "blocked",
		Branch:       "other-task",
	}); err != nil {
		t.Fatalf("UpsertTask other repo returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--home", home, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"agents: 0", "goals: 1", "tasks: 2", "  planned: 2", "pull_requests: 0"} {
		if !strings.Contains(output, want) {
			t.Fatalf("status output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "blocked") {
		t.Fatalf("status output included another repo task:\n%s", output)
	}
}

func TestRunGoalTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "template"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("goal template exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"# <Goal Title>",
		"### Task 1: <Task Title>",
		"codex exec review is clean; ready for manual /review.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("goal template output missing %q:\n%s", want, output)
		}
	}
}

func TestRunGoalTemplateValidatesInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "template", "extra"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "goal template does not accept positional arguments") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunGoalImportValidatesInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", t.TempDir()}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "goal import requires --file") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunStatusIncludesUnscopedImportedTasks(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("goal import exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--home", home, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"tasks: 1", "  planned: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("status output missing %q:\n%s", want, output)
		}
	}
}

func TestRunGoalImportRejectsInvalidRepo(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "gitmoot"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid repo") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".gitmoot", "gitmoot.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database stat error = %v, want os.ErrNotExist", err)
	}
}

func TestRunGoalImportRejectsTaskIDConflict(t *testing.T) {
	home := t.TempDir()
	firstGoal := filepath.Join(t.TempDir(), "first.md")
	writeFile(t, firstGoal, "# First\n\n### Task 1: Bootstrap\n")
	secondGoal := filepath.Join(t.TempDir(), "second.md")
	writeFile(t, secondGoal, "# Second\n\n### Task 1: Other Bootstrap\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", firstGoal, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("first import exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"goal", "import", "--home", home, "--file", secondGoal, "--repo", "jerryfane/other"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("second import exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "task task-001 already exists") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunGoalImportPreservesExistingTaskProgress(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("first import exit code = %d, stderr=%s", code, stderr.String())
	}

	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "task-001",
		RepoFullName: "jerryfane/gitmoot",
		GoalID:       "goal",
		Title:        "Bootstrap",
		State:        "implementing",
		Branch:       "custom-branch",
	}); err != nil {
		t.Fatalf("UpsertTask progress returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap Updated\n")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"goal", "import", "--home", home, "--file", goalPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second import exit code = %d, stderr=%s", code, stderr.String())
	}

	store, err = db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	task, err := store.GetTask(context.Background(), "task-001")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.RepoFullName != "jerryfane/gitmoot" || task.Title != "Bootstrap Updated" || task.State != "implementing" || task.Branch != "custom-branch" {
		t.Fatalf("task after reimport = %+v", task)
	}
}

func TestRunTaskList(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap\n\n### Task 2: Review\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("goal import exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "task-001",
		RepoFullName: "jerryfane/gitmoot",
		GoalID:       "goal",
		Title:        "Bootstrap",
		State:        "implementing",
		Branch:       "task-001-bootstrap",
		WorktreePath: "/tmp/gitmoot/worktrees/jerryfane--gitmoot/task-001",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"task", "list", "--home", home, "--repo", "jerryfane/gitmoot", "--state", "implementing"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task list exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "task-001\timplementing\tjerryfane/gitmoot\ttask-001-bootstrap\t/tmp/gitmoot/worktrees/jerryfane--gitmoot/task-001\tBootstrap") {
		t.Fatalf("task list output = %q", output)
	}
	if strings.Contains(output, "task-002") {
		t.Fatalf("task list did not apply state filter:\n%s", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"task", "list", "--home", home, "--repo", "jerryfane/gitmoot", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task list --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var decoded []taskListOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if len(decoded) != 2 || decoded[0].ID != "task-001" || decoded[0].WorktreePath == "" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestRunGoalImportRollsBackOnTaskFailure(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "task-existing",
		RepoFullName: "jerryfane/gitmoot",
		GoalID:       "existing",
		Title:        "Existing",
		State:        "planned",
		Branch:       "task-002-conflict",
	}); err != nil {
		t.Fatalf("UpsertTask existing returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: First\n\n### Task 2: Conflict\n")

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("import exit code = %d, want 1; stderr=%s", code, stderr.String())
	}

	store, err = db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	if _, err := store.GetTask(context.Background(), "task-001"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("task-001 error = %v, want sql.ErrNoRows", err)
	}
	goals, err := store.ListGoals(context.Background())
	if err != nil {
		t.Fatalf("ListGoals returned error: %v", err)
	}
	if len(goals) != 0 {
		t.Fatalf("goals after failed import = %+v, want none", goals)
	}
}

func TestRunTaskRunValidatesInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"task", "run", "task-001", "--home", t.TempDir()}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "task run requires --owner") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunTaskRunRejectsRepoMismatch(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("goal import exit code = %d, stderr=%s", code, stderr.String())
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/jerryfane/other.git")
	withWorkingDirectory(t, repoDir)

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"task", "run", "task-001", "--home", home, "--repo", "jerryfane/other", "--owner", "lead"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("task run exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "belongs to repo jerryfane/gitmoot") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunTaskRunRejectsWrongCheckout(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("goal import exit code = %d, stderr=%s", code, stderr.String())
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/jerryfane/other.git")
	withWorkingDirectory(t, repoDir)

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"task", "run", "task-001", "--home", home, "--repo", "jerryfane/gitmoot", "--owner", "lead"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("task run exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not jerryfane/gitmoot") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunTaskRunRegistersCurrentRepo(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("goal import exit code = %d, stderr=%s", code, stderr.String())
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/jerryfane/gitmoot.git")
	withWorkingDirectory(t, repoDir)

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"task", "run", "task-001", "--home", home, "--repo", "jerryfane/gitmoot", "--owner", "lead"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task run exit code = %d, stderr=%s", code, stderr.String())
	}

	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	repo, err := store.GetRepo(context.Background(), "jerryfane/gitmoot")
	if err != nil {
		t.Fatalf("GetRepo returned error: %v", err)
	}
	if repo.CheckoutPath != repoDir || repo.RemoteURL != "https://github.com/jerryfane/gitmoot.git" {
		t.Fatalf("repo = %+v", repo)
	}
}

func TestRunTaskRunBlocksWhenCheckoutMutationLocked(t *testing.T) {
	home := t.TempDir()
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	writeFile(t, goalPath, "# Build Gitmoot\n\n### Task 1: Bootstrap\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("goal import exit code = %d, stderr=%s", code, stderr.String())
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/jerryfane/gitmoot.git")
	withWorkingDirectory(t, repoDir)

	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	absoluteCheckout, err := filepath.Abs(repoDir)
	if err != nil {
		t.Fatalf("Abs returned error: %v", err)
	}
	if acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: "checkout-mutation:" + filepath.Clean(absoluteCheckout),
		OwnerJobID:  "task:other",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"task", "run", "task-001", "--home", home, "--repo", "jerryfane/gitmoot", "--owner", "lead"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("task run exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "This checkout is already being mutated by another Gitmoot task") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	branches := runGitOutput(t, repoDir, "branch", "--list", "task-001")
	if strings.TrimSpace(branches) != "" {
		t.Fatalf("task branch was created despite checkout lock: %q", branches)
	}
}

func TestTaskBranchNameFallsBackToTaskID(t *testing.T) {
	if got := taskBranchName("task-001", "!!!"); got != "task-001" {
		t.Fatalf("taskBranchName returned %q, want task-001", got)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
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
