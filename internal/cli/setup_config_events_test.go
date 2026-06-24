package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestRunConfigPathAndShow(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"config", "path", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("config path exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), filepath.Join(home, ".gitmoot", "config.toml")) {
		t.Fatalf("config path output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"config", "show", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("config show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"[paths]", "database", "workspaces"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("config show missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunSetupRegistersRepoAndAgent(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"setup", "--home", home, "--repo", "owner/repo", "--path", repoDir, "--agent", "lead", "--runtime", "codex", "--session", "last"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"step: initialize local state", "step: register repo owner/repo", "step: subscribe agent lead", "next: gitmoot daemon start --repo owner/repo"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("setup output missing %q:\n%s", want, stdout.String())
		}
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if _, err := store.GetRepo(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("GetRepo returned error: %v", err)
	}
	agent, err := store.GetAgent(context.Background(), "lead")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Runtime != "codex" || agent.RuntimeRef != "last" {
		t.Fatalf("agent = %+v", agent)
	}
	allowed, err := store.AgentCanAccessRepo(context.Background(), "lead", "owner/repo")
	if err != nil {
		t.Fatalf("AgentCanAccessRepo returned error: %v", err)
	}
	if !allowed {
		t.Fatal("setup did not allow lead on owner/repo")
	}
}

func TestRunSetupPrintsTaggingReadinessByDefault(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"setup", "--home", home, "--repo", "owner/repo", "--path", repoDir, "--agent", "lead", "--runtime", "codex", "--session", "last"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"readiness:",
		"repo registered: owner/repo",
		"agent access: lead -> owner/repo",
		"gitmoot daemon start --repo owner/repo --watch-issues",
		"@lead ask <your question>",
		"only the `ask` action is acted on",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("tagging readiness output missing %q:\n%s", want, out)
		}
	}
}

func TestRunSetupWatchIssuesFalseSuppressesTagHelp(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"setup", "--home", home, "--repo", "owner/repo", "--path", repoDir, "--agent", "lead", "--runtime", "codex", "--session", "last", "--watch-issues=false"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("setup exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "issue-watching is OFF for this run") {
		t.Fatalf("expected off-note when --watch-issues=false:\n%s", out)
	}
	if strings.Contains(out, "@lead ask") {
		t.Fatalf("did not expect tag help when issue-watching is off:\n%s", out)
	}
}

func TestRunSetupPreservesExistingAgentRepoAccess(t *testing.T) {
	home := t.TempDir()
	firstRepoDir := t.TempDir()
	runGit(t, firstRepoDir, "init")
	runGit(t, firstRepoDir, "remote", "add", "origin", "https://github.com/owner/first.git")
	secondRepoDir := t.TempDir()
	runGit(t, secondRepoDir, "init")
	runGit(t, secondRepoDir, "remote", "add", "origin", "https://github.com/owner/second.git")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"setup", "--home", home, "--repo", "owner/first", "--path", firstRepoDir, "--agent", "lead", "--runtime", "codex", "--session", "last"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("first setup exit code = %d, stderr=%s", code, stderr.String())
	}
	store := openCLIJobStore(t, home)
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           "lead",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      "owner/first",
		Capabilities:   []string{"review"},
		AutonomyPolicy: "manual",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent existing metadata returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store returned error: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"setup", "--home", home, "--repo", "owner/second", "--path", secondRepoDir, "--agent", "lead", "--runtime", "codex", "--session", "last"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second setup exit code = %d, stderr=%s", code, stderr.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "lead")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Role != "reviewer" || agent.AutonomyPolicy != "manual" || agent.HealthStatus != "ok" || strings.Join(agent.Capabilities, ",") != "review" {
		t.Fatalf("setup overwrote existing agent metadata: %+v", agent)
	}
	for _, repo := range []string{"owner/first", "owner/second"} {
		allowed, err := store.AgentCanAccessRepo(context.Background(), "lead", repo)
		if err != nil {
			t.Fatalf("AgentCanAccessRepo(%s) returned error: %v", repo, err)
		}
		if !allowed {
			t.Fatalf("lead lost access to %s", repo)
		}
	}
}

func TestRunEventsListsRepoJobAndLockEvents(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "job-1",
		Agent:   "lead",
		Type:    "review",
		State:   string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "task-1", PullRequest: 1}),
	}, "queued")
	acquireCLILock(t, store, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"})
	if _, err := store.ReleaseLockWithEvent(context.Background(), db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}, db.BranchLockEvent{Kind: "released", Message: "done"}); err != nil {
		t.Fatalf("ReleaseLockWithEvent returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"events", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("events exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"job\tjob-1\tqueued\tqueued\tqueued", "lock\ttask-1\tlead\treleased\tdone"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("events output missing %q:\n%s", want, stdout.String())
		}
	}
}
