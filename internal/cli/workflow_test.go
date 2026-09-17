package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
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
	for _, want := range []string{"tasks: 1", "  planned: 1"} {
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
	code := Run([]string{"task", "list", "--home", home, "--repo", "gitmoot/gitmoot", "--state", "reviewing"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task list exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "review-pr-12-3f3a1026\treviewing\tgitmoot/gitmoot\tfix/twelve\t/tmp/gitmoot/worktrees/gitmoot--gitmoot/review-pr-12\tReview PR #12") {
		t.Fatalf("task list output = %q", output)
	}
	if strings.Contains(output, "review-pr-13") {
		t.Fatalf("task list did not apply state filter:\n%s", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"task", "list", "--home", home, "--repo", "gitmoot/gitmoot", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("task list --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var decoded []taskListOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if len(decoded) != 2 || decoded[0].ID != "review-pr-12-3f3a1026" || decoded[0].WorktreePath == "" {
		t.Fatalf("decoded = %+v", decoded)
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
		"agent", "subscribe", name,
		"--home", home,
		"--runtime", "shell",
		"--session", `printf '%s\n' '{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`,
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
