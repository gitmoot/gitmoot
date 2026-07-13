package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

type shellDismissE2EGitHub struct{ stubTaskRecoverGitHub }

func (*shellDismissE2EGitHub) ListPullRequests(context.Context, github.Repository, string) ([]github.PullRequest, error) {
	return nil, nil
}

func (*shellDismissE2EGitHub) ListIssueComments(context.Context, github.Repository, int64) ([]github.IssueComment, error) {
	return nil, nil
}

type shellDismissE2ERemote struct{ calls int }

func (r *shellDismissE2ERemote) RemoteBranches(context.Context, string, []string) (map[string]struct{}, error) {
	r.calls++
	return map[string]struct{}{}, nil
}

func TestNoLLMShellTaskDismissAndRecoverE2E(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/throwaway.sock")
	oldHerdr, hadHerdr := os.LookupEnv("HERDR_ENV")
	if err := os.Unsetenv("HERDR_ENV"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadHerdr {
			_ = os.Setenv("HERDR_ENV", oldHerdr)
		} else {
			_ = os.Unsetenv("HERDR_ENV")
		}
	})

	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "origin.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	writeFile(t, filepath.Join(repoDir, "README.md"), "main\n")
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	// task run validates a GitHub-shaped registered remote; switch back to the
	// local bare origin immediately after allocation so every push stays local.
	runGit(t, repoDir, "remote", "set-url", "origin", "https://github.com/owner/repo.git")
	withWorkingDirectory(t, repoDir)

	goal := filepath.Join(home, "GOAL.md")
	writeFile(t, goal, "# E2E\n\n### Task 1: Bootstrap\n")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"goal", "import", "--home", home, "--file", goal, "--repo", "owner/repo"}, &stdout, &stderr); code != 0 {
		t.Fatalf("goal import code=%d stderr=%s", code, stderr.String())
	}
	subscribeShellImplementAgent(t, home, "lead", "owner/repo")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"task", "run", "task-001", "--home", home, "--repo", "owner/repo", "--owner", "lead"}, &stdout, &stderr); code != 0 {
		t.Fatalf("task run code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runGit(t, repoDir, "remote", "set-url", "origin", remoteDir)

	store := openCLIJobStore(t, home)
	defer store.Close()
	task, err := store.GetTask(ctx, "task-001")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(task.WorktreePath, "partial.txt"), "recover me\n")
	jobID := taskRunImplementJobID(task.ID, "lead")
	if ok, err := store.TransitionJobStateWithEvent(ctx, jobID, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning), Message: "shell stub started"}); err != nil || !ok {
		t.Fatalf("mark running ok=%v err=%v", ok, err)
	}
	if _, err := workflow.CancelJob(ctx, store, jobID); err != nil {
		t.Fatal(err)
	}
	if settled, err := workflow.SettleCancelledRunningJob(ctx, store, jobID, "shell stub settled"); err != nil || !settled {
		t.Fatalf("settle cancelled settled=%v err=%v", settled, err)
	}

	configPath := filepath.Join(home, ".gitmoot", "config.toml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, []byte("\n[workflow]\nstale_task_ttl = \"1ns\"\n")...)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	gh := &shellDismissE2EGitHub{}
	remote := &shellDismissE2ERemote{}
	d := daemon.Daemon{
		Repo:           github.Repository{Owner: "owner", Name: "repo"},
		Store:          store,
		GitHub:         gh,
		RemoteBranches: remote,
		Now:            func() time.Time { return time.Now().UTC().Add(time.Hour) },
	}
	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	task, _ = store.GetTask(ctx, task.ID)
	if task.State != string(workflow.TaskDismissed) || remote.calls != 1 {
		t.Fatalf("after reconcile task=%+v remote calls=%d", task, remote.calls)
	}
	events, _ := store.ListTaskEvents(ctx, task.ID)
	if len(events) != 1 || events[0].Kind != "task_dismissed_auto" {
		t.Fatalf("auto events=%+v", events)
	}

	if _, err := recoverTaskImplementation(ctx, store, task.ID, "owner/repo", "lead", false, gh); err != nil {
		t.Fatalf("task recover: %v", err)
	}
	task, _ = store.GetTask(ctx, task.ID)
	events, _ = store.ListTaskEvents(ctx, task.ID)
	if task.State != string(workflow.TaskPullRequestOpen) || len(events) != 3 || events[1].ToState != string(workflow.TaskImplementing) || events[2].ToState != string(workflow.TaskPullRequestOpen) {
		t.Fatalf("recovered task=%+v events=%+v", task, events)
	}
}
