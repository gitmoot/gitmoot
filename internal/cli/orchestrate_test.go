package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestRunOrchestrateForwardsToBackgroundRun asserts that `gitmoot orchestrate`
// is sugar for `gitmoot agent run <agent> --background`: it forwards through the
// same dispatch with Background forced on (so the job is queued without runtime
// delivery) and reports the orchestrate execution path.
func TestRunOrchestrateForwardsToBackgroundRun(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "subscribe", "conductor",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440099",
		"--role", "planner",
		"--repo", "owner/repo",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	// No --background flag: orchestrate must force it on.
	code := Run([]string{"orchestrate", "conductor", "fan out delegations to the players", "--home", home, "--repo", "owner/repo", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("orchestrate exit code = %d, stderr=%s", code, stderr.String())
	}

	var decoded localAgentJobOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("orchestrate JSON did not decode: %v\n%s", err, stdout.String())
	}
	// Background==true is observable as a queued job with no synchronous result.
	if decoded.State != string(workflow.JobQueued) {
		t.Fatalf("orchestrate state = %q, want %q (Background must be forced on)", decoded.State, workflow.JobQueued)
	}
	if decoded.Result != nil {
		t.Fatalf("orchestrate produced a synchronous result %+v, want none (background)", decoded.Result)
	}
	if decoded.Repo != "owner/repo" || decoded.Agent != "conductor" {
		t.Fatalf("orchestrate forwarded wrong target: %+v", decoded)
	}
	if decoded.ExecutionPath != "orchestrate" {
		t.Fatalf("orchestrate execution path = %q, want %q", decoded.ExecutionPath, "orchestrate")
	}
	// The action resolves through the same selector as `agent run`.
	if decoded.SelectedAction != "ask" {
		t.Fatalf("orchestrate selected action = %q, want %q", decoded.SelectedAction, "ask")
	}

	// Background dispatch must not deliver to the runtime.
	if len(runner.calls) != 0 {
		t.Fatalf("orchestrate runtime calls = %+v, want none (queued, not delivered)", runner.calls)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != string(workflow.JobQueued) {
		t.Fatalf("jobs = %+v, want one queued job", jobs)
	}
}

func TestRunOrchestrateHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"orchestrate", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("orchestrate --help exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"gitmoot orchestrate <agent>",
		"gitmoot agent run <agent> --background",
		"delegations[] score",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("orchestrate --help output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunOrchestrateRequiresAgentAndMessage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"orchestrate"}, &stdout, &stderr); code != 2 {
		t.Fatalf("orchestrate with no args exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "orchestrate requires exactly one agent and one message") {
		t.Fatalf("orchestrate stderr = %q", stderr.String())
	}
}
