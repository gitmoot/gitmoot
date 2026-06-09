package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestRunAgentSubscribeListRemove(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--repo", "jerryfane/other",
		"--capability", "review",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "audit") || !strings.Contains(output, "jerryfane/gitmoot,jerryfane/other") || !strings.Contains(output, "review,ask") {
		t.Fatalf("list output = %q", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resubscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list after resubscribe exit code = %d, stderr=%s", code, stderr.String())
	}
	output = stdout.String()
	if !strings.Contains(output, "jerryfane/gitmoot") || strings.Contains(output, "jerryfane/other") {
		t.Fatalf("list after resubscribe output = %q", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "remove", "audit", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remove exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second list exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "audit") {
		t.Fatalf("agent was not removed: %q", stdout.String())
	}
}

func TestRunAgentShow(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--capability", "review",
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "show", "audit", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"name: audit",
		"runtime: codex",
		"runtime_ref: 550e8400-e29b-41d4-a716-446655440001",
		"role: reviewer",
		"capabilities: review",
		"policy: workspace-write",
		"allowed_repos: jerryfane/gitmoot",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("agent show output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "show", "audit", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent show --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var decoded agentShowOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if decoded.Name != "audit" || decoded.Policy != runtime.AutonomyPolicyWorkspaceWrite || strings.Join(decoded.AllowedRepos, ",") != "jerryfane/gitmoot" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestRunAgentSubscribeValidatesAutonomyPolicy(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--capability", "review",
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "audit")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.AutonomyPolicy != runtime.AutonomyPolicyWorkspaceWrite {
		t.Fatalf("autonomy policy = %q", agent.AutonomyPolicy)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "badpolicy",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440002",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--policy", "manual",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("invalid policy exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "autonomy policy") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunAgentAccessCommands(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "allow", "audit", "--home", home, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("allow exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "allowed audit on jerryfane/gitmoot") {
		t.Fatalf("allow output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "repos", "audit", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repos exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "jerryfane/gitmoot" {
		t.Fatalf("repos output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "deny", "audit", "--home", home, "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deny exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "repos", "audit", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repos after deny exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Fatalf("repos after deny output = %q", stdout.String())
	}
}

func TestRunAgentStartCreatesCodexSessionAndStoresAgent(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440011"}` + "\n"}}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "start", "lead",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"started lead (codex) for owner/repo",
		"session: 550e8400-e29b-41d4-a716-446655440011",
		"invoke: /gitmoot lead review",
		"next: cd " + repoDir,
		"next: gitmoot daemon start --home " + home + " --repo owner/repo",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("start output missing %q:\n%s", want, stdout.String())
		}
	}
	runner.want(t, 0, repoDir, "codex", "exec", "--json", "--")

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "lead")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Runtime != "codex" || agent.RuntimeRef != "550e8400-e29b-41d4-a716-446655440011" || agent.Role != "agent" || strings.Join(agent.Capabilities, ",") != "ask,review,implement" {
		t.Fatalf("agent = %+v", agent)
	}
	if _, err := store.GetRepo(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("GetRepo returned error: %v", err)
	}
	allowed, err := store.AgentCanAccessRepo(context.Background(), "lead", "owner/repo")
	if err != nil {
		t.Fatalf("AgentCanAccessRepo returned error: %v", err)
	}
	if !allowed {
		t.Fatal("agent start did not allow lead on owner/repo")
	}
	if !strings.Contains(runner.calls[0].args[len(runner.calls[0].args)-1], "You are a Gitmoot-managed agent named lead.") {
		t.Fatalf("startup prompt = %q", runner.calls[0].args[len(runner.calls[0].args)-1])
	}
}

func TestRunAgentStartAppliesInstalledTemplateDefaults(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedThermoTemplate(t, home)
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440012"}` + "\n"}}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "start", "thermo-review",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--template", "thermo-nuclear-code-quality-review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "thermo-review")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Role != "reviewer" || agent.TemplateID != "thermo-nuclear-code-quality-review" || strings.Join(agent.Capabilities, ",") != "ask,review" {
		t.Fatalf("agent = %+v", agent)
	}
	prompt := runner.calls[0].args[len(runner.calls[0].args)-1]
	if !strings.Contains(prompt, "Template: thermo-nuclear-code-quality-review @ abc123") || !strings.Contains(prompt, "Review deeply.") {
		t.Fatalf("startup prompt missing template content:\n%s", prompt)
	}
}

func TestRunAgentStartUsesInstalledCustomTemplate(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "Review frontend behavior.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "template", "add", "frontend-reviewer", "--home", home, "--file", promptPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440022"}` + "\n"}}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"agent", "start", "frontend-reviewer",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--template", "frontend-reviewer",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "frontend-reviewer")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Role != "agent" || agent.TemplateID != "frontend-reviewer" || strings.Join(agent.Capabilities, ",") != "ask,review,implement" {
		t.Fatalf("agent = %+v", agent)
	}
	prompt := runner.calls[0].args[len(runner.calls[0].args)-1]
	if !strings.Contains(prompt, "Template: frontend-reviewer @ sha256:") || !strings.Contains(prompt, "Review frontend behavior.") {
		t.Fatalf("startup prompt missing custom template content:\n%s", prompt)
	}
}

func TestRunAgentStartAppliesPlannerTemplateDefaults(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedPlannerTemplate(t, home)
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440032"}` + "\n"}}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "start", "planner",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--template", "planner",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Role != "planner" || agent.TemplateID != "planner" || strings.Join(agent.Capabilities, ",") != "ask" {
		t.Fatalf("agent = %+v", agent)
	}
	prompt := runner.calls[0].args[len(runner.calls[0].args)-1]
	if !strings.Contains(prompt, "Template: planner @ def456") || !strings.Contains(prompt, "Plan and write goals.") {
		t.Fatalf("startup prompt missing planner template content:\n%s", prompt)
	}
}

func TestRunAgentStartAllowsImplementForPlannerTemplate(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedPlannerTemplate(t, home)
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440033"}` + "\n"}}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "start", "planner",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--template", "planner",
		"--capability", "ask",
		"--capability", "implement",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if strings.Join(agent.Capabilities, ",") != "ask,implement" {
		t.Fatalf("capabilities = %+v", agent.Capabilities)
	}
}

func TestRunAgentAskDispatchesAndStoresResult(t *testing.T) {
	home := t.TempDir()
	otherHome := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)
	seedPlannerTemplate(t, home)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "subscribe", "planner",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440021",
		"--repo", "owner/repo",
		"--template", "planner",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "ask", "planner", "use the isolated state", "--home", otherHome, "--repo", "owner/repo"}, &stdout, &stderr); code != 1 {
		t.Fatalf("ask with other home exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `agent "planner" not found`) {
		t.Fatalf("ask with other home stderr = %q", stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"gitmoot_result":{"decision":"approved","summary":"plan ready","findings":[{"title":"clear"}],"changes_made":[],"tests_run":["go test ./internal/cli"],"needs":["ship it"],"next_agents":["thermo-review"]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"approved","summary":"json ready","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "ask", "planner", "Write a plan", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"job: local-ask-planner-",
		"state: succeeded",
		"repo: owner/repo",
		"agent: planner",
		"action: ask",
		"decision: approved",
		"summary: plan ready",
		"findings:",
		`- {"title":"clear"}`,
		"needs:",
		"- ship it",
		"tests_run:",
		"- go test ./internal/cli",
		"next_agents:",
		"- thermo-review",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("ask output missing %q:\n%s", want, stdout.String())
		}
	}
	runner.want(t, 0, repoDir, "codex", "exec", "resume", "550e8400-e29b-41d4-a716-446655440021", "--")
	if !strings.Contains(runner.calls[0].args[len(runner.calls[0].args)-1], "Write a plan") {
		t.Fatalf("ask prompt missing message:\n%s", runner.calls[0].args[len(runner.calls[0].args)-1])
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one job", jobs)
	}
	payload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.TemplateID != "planner" || payload.TemplateResolvedCommit != "def456" || !strings.Contains(payload.TemplateContent, "Plan and write goals.") {
		t.Fatalf("payload template snapshot = %+v", payload)
	}
	if payload.PullRequest != 0 || payload.Sender != "local" || payload.Instructions != "Write a plan" {
		t.Fatalf("payload local ask fields = %+v", payload)
	}
	if payload.Result == nil || payload.Result.Summary != "plan ready" || len(payload.RawOutputs) != 1 {
		t.Fatalf("payload result = %+v", payload)
	}
	events, err := store.ListJobEvents(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasCLIJobEvent(events, "advance_completed") {
		t.Fatalf("events = %+v, want advance_completed", events)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "list", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), jobs[0].ID+"\tsucceeded\task\tplanner\towner/repo\t#0") {
		t.Fatalf("job list output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "show", jobs[0].ID, "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{`"template_id": "planner"`, "decision: approved", "summary: plan ready"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("job show output missing %q:\n%s", want, stdout.String())
		}
	}

	otherStore := openCLIJobStore(t, otherHome)
	defer otherStore.Close()
	otherJobs, err := otherStore.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("other ListJobs returned error: %v", err)
	}
	if len(otherJobs) != 0 {
		t.Fatalf("other home jobs = %+v, want none", otherJobs)
	}

	runGit(t, repoDir, "switch", "-c", "feature/local-ask")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "- Write JSON", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ask --json exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "gitmoot_result") {
		t.Fatalf("json output leaked raw runtime output:\n%s", stdout.String())
	}
	var decoded localAgentJobOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if decoded.State != "succeeded" || decoded.Repo != "owner/repo" || decoded.Result == nil || decoded.Result.Summary != "json ready" || decoded.RawOutputCount != 1 {
		t.Fatalf("decoded output = %+v", decoded)
	}
	jsonJob, err := store.GetJob(context.Background(), decoded.JobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", decoded.JobID, err)
	}
	jsonPayload, err := daemonJobPayload(jsonJob)
	if err != nil {
		t.Fatalf("json daemonJobPayload returned error: %v", err)
	}
	if jsonPayload.Branch != "feature/local-ask" || jsonPayload.Instructions != "- Write JSON" {
		t.Fatalf("json ask payload = %+v", jsonPayload)
	}
}

func TestSelectAgentRunAction(t *testing.T) {
	tests := []struct {
		name    string
		options agentRunOptions
		action  string
	}{
		{name: "task selects implement", options: agentRunOptions{taskID: "task-1", message: "anything"}, action: "implement"},
		{name: "pr selects review", options: agentRunOptions{prNumber: 7, message: "anything"}, action: "review"},
		{name: "head sha selects review", options: agentRunOptions{headSHA: strings.Repeat("a", 40), message: "anything"}, action: "review"},
		{name: "review language selects review", options: agentRunOptions{message: "please review this PR"}, action: "review"},
		{name: "implementation language selects implement", options: agentRunOptions{message: "update docs and add tests"}, action: "implement"},
		{name: "write code selects implement", options: agentRunOptions{message: "write code for the new command"}, action: "implement"},
		{name: "code question selects ask", options: agentRunOptions{message: "what does this code do?"}, action: "ask"},
		{name: "plain question selects ask", options: agentRunOptions{message: "what is the risk here?"}, action: "ask"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, _ := selectAgentRunAction(tt.options)
			if action != tt.action {
				t.Fatalf("action = %q, want %q", action, tt.action)
			}
		})
	}
}

func TestRunAgentAskBlocksWorkflowOrchestration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "ask", "planner", "create branch, commit, push, and open PR"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "This looks like implementation workflow orchestration") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestWorkflowOrchestrationGuardAllowsCommitQuestions(t *testing.T) {
	if looksLikeWorkflowOrchestration("Which commit introduced this regression?") {
		t.Fatal("commit analysis question was incorrectly treated as workflow orchestration")
	}
	if !looksLikeWorkflowOrchestration("commit and push the branch, then open a PR") {
		t.Fatal("explicit commit/push/PR workflow was not treated as workflow orchestration")
	}
}

func TestPrepareLocalReviewDispatchRequestCreatesReviewWorktree(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "switch", "-c", "feature/review")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "feature")
	head, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	runGit(t, repoDir, "switch", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")

	store := openCLIJobStore(t, home)
	defer store.Close()
	record := db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: repoDir}
	request, checkout, err := prepareLocalReviewDispatchRequest(ctx, store, record, github.Repository{Owner: "owner", Name: "repo"}, localAgentDispatchRequest{
		Home:         home,
		Agent:        "audit",
		Action:       "review",
		Instructions: "Review this PR.",
		PullRequest:  12,
		Branch:       "feature/review",
		HeadSHA:      head,
	})
	if err != nil {
		t.Fatalf("prepareLocalReviewDispatchRequest returned error: %v", err)
	}
	if request.TaskID == "" || checkout == "" {
		t.Fatalf("request.TaskID=%q checkout=%q", request.TaskID, checkout)
	}
	reviewHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("review HeadSHA returned error: %v", err)
	}
	if reviewHead != head {
		t.Fatalf("review worktree head = %q, want %q", reviewHead, head)
	}
	branch, err := (gitutil.Client{Dir: repoDir}).CurrentBranch(ctx)
	if err != nil {
		t.Fatalf("CurrentBranch returned error: %v", err)
	}
	if branch != "main" {
		t.Fatalf("registered checkout branch = %q, want main", branch)
	}
	task, err := store.GetTask(ctx, request.TaskID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.WorktreePath != checkout || task.State != string(workflow.TaskReviewing) {
		t.Fatalf("task = %+v, checkout=%q", task, checkout)
	}
}

func TestPrepareLocalReviewDispatchRequestDoesNotReuseStaleReviewWorktree(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	repoDir := t.TempDir()
	staleWorktree := filepath.Join(home, "stale")
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "switch", "-c", "feature/review")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("old feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "old feature")
	oldHead, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("old HeadSHA returned error: %v", err)
	}
	runGit(t, repoDir, "worktree", "add", "--detach", staleWorktree, oldHead)
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "new feature")
	newHead, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("new HeadSHA returned error: %v", err)
	}
	runGit(t, repoDir, "switch", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertTask(ctx, db.Task{ID: "stale-review", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Stale review", State: string(workflow.TaskReviewing), Branch: "feature/review", WorktreePath: staleWorktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	record := db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: repoDir}
	request, checkout, err := prepareLocalReviewDispatchRequest(ctx, store, record, github.Repository{Owner: "owner", Name: "repo"}, localAgentDispatchRequest{
		Home:         home,
		Agent:        "audit",
		Action:       "review",
		Instructions: "Review this PR.",
		PullRequest:  12,
		Branch:       "feature/review",
		HeadSHA:      newHead,
	})
	if err != nil {
		t.Fatalf("prepareLocalReviewDispatchRequest returned error: %v", err)
	}
	if checkout == staleWorktree || request.TaskID == "stale-review" {
		t.Fatalf("reused stale checkout=%q taskID=%q", checkout, request.TaskID)
	}
	reviewHead, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("review HeadSHA returned error: %v", err)
	}
	if reviewHead != newHead {
		t.Fatalf("review worktree head = %q, want %q", reviewHead, newHead)
	}
	staleHead, err := (gitutil.Client{Dir: staleWorktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("stale HeadSHA returned error: %v", err)
	}
	if staleHead != oldHead {
		t.Fatalf("stale worktree head = %q, want %q", staleHead, oldHead)
	}
}

func TestRunAgentAskBackgroundQueuesWithoutRuntimeDelivery(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "subscribe", "planner",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440022",
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
	code := Run([]string{"agent", "ask", "planner", "Write a plan", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("background ask exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"job: local-ask-planner-",
		"state: queued",
		"repo: owner/repo",
		"agent: planner",
		"action: ask",
		"next: gitmoot job watch local-ask-planner-",
		"queued: daemon is not running",
		"process: gitmoot daemon start",
		"or: gitmoot job run local-ask-planner-",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("background ask output missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime calls = %+v, want none", runner.calls)
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
	payload, err := daemonJobPayload(jobs[0])
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}
	if payload.Instructions != "Write a plan" || payload.Result != nil || len(payload.RawOutputs) != 0 {
		t.Fatalf("background payload = %+v", payload)
	}
}

func TestRunAgentAskBackgroundJSON(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "subscribe", "planner",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440023",
		"--role", "planner",
		"--repo", "owner/repo",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "ask", "planner", "Write JSON", "--home", home, "--repo", "owner/repo", "--background", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("background ask --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var decoded localAgentJobOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if decoded.State != string(workflow.JobQueued) || decoded.Repo != "owner/repo" || decoded.Result != nil || decoded.WatchCommand == "" || decoded.DaemonRunning {
		t.Fatalf("decoded background output = %+v", decoded)
	}
}

func TestRunAgentTypeSetListShowAndManagedBackgroundAsk(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--role", "planner",
		"--policy", "workspace-write",
		"--max-background", "1",
		"--idle-timeout", "20m",
		"--job-timeout", "5m",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "configured agent type planner") {
		t.Fatalf("agent type set output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "type", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "planner\tcodex\t\t1") {
		t.Fatalf("agent type list output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "type", "show", "planner", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "max_background: 1") || !strings.Contains(stdout.String(), "capabilities: ask") || !strings.Contains(stdout.String(), "policy: workspace-write") {
		t.Fatalf("agent type show output = %q", stdout.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440111"}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "First plan", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("managed background ask exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: queued") {
		t.Fatalf("managed background ask output = %q", stdout.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runtime start calls = %+v, want one", runner.calls)
	}
	runner.want(t, 0, repoDir, "codex", "exec", "--sandbox", "workspace-write", "--json", "--")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "Second plan", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second managed background ask exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runtime start calls after reuse = %+v, want one", runner.calls)
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	instances, err := store.ListAgentInstances(context.Background())
	if err != nil {
		t.Fatalf("ListAgentInstances returned error: %v", err)
	}
	if len(instances) != 1 || instances[0].Type != "planner" || instances[0].RuntimeRef != "550e8400-e29b-41d4-a716-446655440111" || instances[0].AutonomyPolicy != runtime.AutonomyPolicyWorkspaceWrite {
		t.Fatalf("instances = %+v", instances)
	}
	config, err := (jobWorker{Store: store, ConfigHome: home}).managedJobConfig(context.Background(), instances[0].Name)
	if err != nil {
		t.Fatalf("managedJobConfig returned error: %v", err)
	}
	if !config.OK || config.JobTimeout != 5*time.Minute || config.IdleTimeout != 20*time.Minute {
		t.Fatalf("managedJobConfig = %+v; want 5m job and 20m idle", config)
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 2 || jobs[0].Agent != instances[0].Name || jobs[1].Agent != instances[0].Name {
		t.Fatalf("jobs = %+v, instance = %+v", jobs, instances[0])
	}
}

func TestDispatchManagedSyncUsesJobTimeoutForRuntimeLock(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--max-background", "1",
		"--idle-timeout", "20m",
		"--job-timeout", "45m",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	runner := &managedSyncLockRunner{
		t:          t,
		store:      store,
		runtimeKey: "runtime:codex:550e8400-e29b-41d4-a716-446655440222",
		minTTL:     44 * time.Minute,
	}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	output, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag:         "owner/repo",
		Agent:            "planner",
		Action:           "ask",
		Instructions:     "Check the managed sync lock timeout.",
		Type:             "planner",
		Home:             home,
		AllowManagedSync: true,
	})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob returned error: %v", err)
	}
	if output.Result == nil || output.Result.Decision != "implemented" {
		t.Fatalf("dispatch output = %+v", output)
	}
	if !runner.checked {
		t.Fatal("runner did not inspect runtime lock during resume")
	}
}

func TestDispatchManagedAgentStartsFreshInstanceWhenPolicyChanges(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--max-background", "1",
		"--capability", "ask",
		"--policy", "read-only",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set read-only exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440411"}` + "\n"},
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440412"}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "First", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("first managed ask exit code = %d, stderr=%s", code, stderr.String())
	}
	runner.want(t, 0, repoDir, "codex", "exec", "--sandbox", "read-only", "--json", "--")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--max-background", "1",
		"--capability", "ask",
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set workspace-write exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "Second", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second managed ask exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runtime start calls = %+v, want two", runner.calls)
	}
	runner.want(t, 1, repoDir, "codex", "exec", "--sandbox", "workspace-write", "--json", "--")

	store := openCLIJobStore(t, home)
	defer store.Close()
	instances, err := store.ListAgentInstances(context.Background())
	if err != nil {
		t.Fatalf("ListAgentInstances returned error: %v", err)
	}
	policies := map[string]int{}
	for _, instance := range instances {
		policies[instance.AutonomyPolicy]++
	}
	if policies[runtime.AutonomyPolicyReadOnly] != 1 || policies[runtime.AutonomyPolicyWorkspaceWrite] != 1 {
		t.Fatalf("instances = %+v, want one read-only and one workspace-write", instances)
	}
}

func TestDispatchLocalAgentJobBlocksReadOnlyImplement(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           "lead",
		Role:           "implementer",
		Runtime:        runtime.ShellRuntime,
		RuntimeRef:     "unused",
		RepoScope:      "owner/repo",
		Capabilities:   []string{"implement"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}

	output, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag:     "owner/repo",
		Agent:        "lead",
		Action:       "implement",
		Instructions: "Implement task 1.",
		Home:         home,
	})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob returned error: %v", err)
	}

	if output.State != string(workflow.JobBlocked) || output.Action != "implement" {
		t.Fatalf("dispatch output = %+v", output)
	}
	job, err := store.GetJob(context.Background(), output.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want blocked", job.State)
	}
	events, err := store.ListJobEvents(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, want permission_blocked", events)
	}
}

func TestDispatchLocalAgentJobBlocksReadOnlyManagedImplementBeforeStart(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "type", "set", "builder",
		"--home", home,
		"--runtime", "codex",
		"--policy", "read-only",
		"--max-background", "1",
		"--idle-timeout", "20m",
		"--job-timeout", "45m",
		"--capability", "implement",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	output, err := dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
		RepoFlag:         "owner/repo",
		Agent:            "builder",
		Action:           "implement",
		Instructions:     "Implement task 1.",
		Type:             "builder",
		Home:             home,
		AllowManagedSync: true,
	})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob returned error: %v", err)
	}

	if output.State != string(workflow.JobBlocked) || output.Action != "implement" || output.Agent != "builder" {
		t.Fatalf("dispatch output = %+v", output)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started before read-only block: %+v", runner.calls)
	}
	events, err := store.ListJobEvents(context.Background(), output.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "permission_blocked") {
		t.Fatalf("events = %+v, want permission_blocked", events)
	}
}

func TestEnsureManagedAgentInstanceKeepsNewInstanceReservedUntilRelease(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--max-background", "2",
		"--idle-timeout", "20m",
		"--job-timeout", "45m",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	record := db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}
	if err := store.UpsertRepo(context.Background(), record); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440333"}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	agent, release, err := ensureManagedAgentInstance(context.Background(), store, home, "planner", "owner/repo", record)
	if err != nil {
		t.Fatalf("ensureManagedAgentInstance returned error: %v", err)
	}
	instance, err := store.GetAgentInstance(context.Background(), agent.Name)
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.State != "starting" || instance.RuntimeRef != "550e8400-e29b-41d4-a716-446655440333" {
		t.Fatalf("instance before release = %+v", instance)
	}
	reusable, ok, err := store.FindReusableAgentInstance(context.Background(), "planner", "owner/repo", "auto", time.Now().UTC())
	if err != nil {
		t.Fatalf("FindReusableAgentInstance returned error: %v", err)
	}
	if ok {
		t.Fatalf("new managed instance is reusable before release: %+v", reusable)
	}
	if err := release(context.Background()); err != nil {
		t.Fatalf("release returned error: %v", err)
	}
	instance, err = store.GetAgentInstance(context.Background(), agent.Name)
	if err != nil {
		t.Fatalf("GetAgentInstance after release returned error: %v", err)
	}
	if instance.State != "idle" {
		t.Fatalf("instance state after release = %s, want idle", instance.State)
	}
}

func TestRunAgentTypeSetRejectsNonStartableRuntime(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"agent", "type", "set", "shell-planner",
		"--home", home,
		"--runtime", "shell",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("agent type set shell exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "managed agent types support codex or claude") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunAgentTypeSetMaxBackgroundCreatesSecondBusyInstance(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--max-background", "2",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440121"}` + "\n"},
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440122"}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	for _, prompt := range []string{"First", "Second"} {
		stdout.Reset()
		stderr.Reset()
		code = Run([]string{"agent", "ask", "planner", prompt, "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("managed background ask %q exit code = %d, stderr=%s", prompt, code, stderr.String())
		}
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runtime start calls = %+v, want two", runner.calls)
	}
}

func TestRunAgentTypeSetMaxBackgroundCapsAcrossRepos(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	otherRepoDir := t.TempDir()
	runGit(t, otherRepoDir, "init")
	runGit(t, otherRepoDir, "branch", "-m", "main")
	runGit(t, otherRepoDir, "remote", "add", "origin", "https://github.com/owner/other.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"repo", "add", "owner/other", "--home", home, "--path", otherRepoDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repo add exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440123"}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "First", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("first managed background ask exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "Second", "--home", home, "--repo", "owner/other", "--background"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("second repo managed background ask exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "reached max_background") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runtime start calls = %+v, want one", runner.calls)
	}
}

func TestRunAgentTypeSetMaxBackgroundUsesOnlyActiveFallback(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}

	now := time.Now().UTC()
	store := openCLIJobStore(t, home)
	if err := store.UpsertAgentInstance(context.Background(), db.AgentInstance{
		Name:         "planner-bg-expired",
		Type:         "planner",
		Runtime:      "codex",
		RuntimeRef:   "550e8400-e29b-41d4-a716-446655440131",
		RepoFullName: "owner/repo",
		Role:         "planner",
		Capabilities: []string{"ask"},
		State:        "idle",
		CreatedAt:    now.Add(-time.Hour).Format(time.RFC3339Nano),
		LastUsedAt:   now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt:    now.Add(-time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance expired returned error: %v", err)
	}
	if err := store.UpsertAgentInstance(context.Background(), db.AgentInstance{
		Name:         "planner-bg-active",
		Type:         "planner",
		Runtime:      "codex",
		RuntimeRef:   "550e8400-e29b-41d4-a716-446655440132",
		RepoFullName: "owner/repo",
		Role:         "planner",
		Capabilities: []string{"ask"},
		State:        "idle",
		CreatedAt:    now.Format(time.RFC3339Nano),
		LastUsedAt:   now.Format(time.RFC3339Nano),
		ExpiresAt:    now.Add(time.Hour).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance active returned error: %v", err)
	}
	if err := store.CreateJob(context.Background(), db.Job{ID: "queued-for-active", Agent: "planner-bg-active", Type: "ask", State: "queued", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	store.Close()

	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "Third", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("managed background ask exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime start calls = %+v, want none", runner.calls)
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	activeJobs := 0
	expiredJobs := 0
	for _, job := range jobs {
		if job.Agent == "planner-bg-active" {
			activeJobs++
		}
		if job.Agent == "planner-bg-expired" {
			expiredJobs++
		}
	}
	if len(jobs) != 2 || activeJobs != 2 || expiredJobs != 0 {
		t.Fatalf("jobs = %+v, want all jobs assigned to active instance", jobs)
	}
}

func TestRunAgentTypeSetMaxBackgroundCountsQueuedExpiredInstance(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}

	now := time.Now().UTC()
	store := openCLIJobStore(t, home)
	if err := store.UpsertAgentInstance(context.Background(), db.AgentInstance{
		Name:         "planner-bg-expired",
		Type:         "planner",
		Runtime:      "codex",
		RuntimeRef:   "550e8400-e29b-41d4-a716-446655440141",
		RepoFullName: "owner/repo",
		Role:         "planner",
		Capabilities: []string{"ask"},
		State:        "idle",
		CreatedAt:    now.Add(-time.Hour).Format(time.RFC3339Nano),
		LastUsedAt:   now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt:    now.Add(-time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}
	if err := store.CreateJob(context.Background(), db.Job{ID: "queued-for-expired", Agent: "planner-bg-expired", Type: "ask", State: "queued", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	store.Close()

	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "planner", "Second", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("managed background ask exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime start calls = %+v, want none", runner.calls)
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	expiredJobs := 0
	for _, job := range jobs {
		if job.Agent == "planner-bg-expired" {
			expiredJobs++
		}
	}
	if len(jobs) != 2 || expiredJobs != 2 {
		t.Fatalf("jobs = %+v, want queued expired instance to retain max slot", jobs)
	}
}

func TestRunAgentTypeSetValidatesTemplate(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"agent", "type", "set", "planner",
		"--home", home,
		"--runtime", "codex",
		"--template", "missing-template",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("agent type set missing template exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "agent template missing-template is not installed") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestManagedAgentAskRejectsRetiredCachedTemplate(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	retiredID := "planner-" + "here"
	configPath := filepath.Join(home, ".gitmoot", "config.toml")
	configContent, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config returned error: %v", err)
	}
	configContent = append(configContent, []byte(`

[agents.legacy-planner]
runtime = "codex"
template = "`+retiredID+`"
role = "planner"
capabilities = ["ask"]
max_background = 1
idle_timeout = "20m"
job_timeout = "10m"
`)...)
	if err := os.WriteFile(configPath, configContent, 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	store := openCLIJobStore(t, home)
	if err := store.UpsertAgentTemplate(context.Background(), db.AgentTemplate{
		ID:             retiredID,
		Name:           "Retired Planner",
		SourceRepo:     "jerryfane/gitmoot",
		SourceRef:      "main",
		SourcePath:     "skills/gitmoot/agent-templates/" + retiredID + ".md",
		ResolvedCommit: "old",
		Content:        "Old planner prompt.\n",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	store.Close()

	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "ask", "legacy-planner", "Plan", "--home", home, "--repo", "owner/repo", "--background"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("agent ask exit code = 0, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent template "+retiredID+" is retired; use planner") {
		t.Fatalf("stderr missing retired guidance:\n%s", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime start calls = %+v, want none", runner.calls)
	}
}

func TestRunAgentTypeSetRejectsConfigUnsafeName(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"agent", "type", "set", "bad#name",
		"--home", home,
		"--runtime", "codex",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("agent type set unsafe name exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid agent type") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunAgentTypeSetRejectsNonPositiveTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
		want string
	}{
		{name: "idle", flag: "--idle-timeout", want: "idle timeout must be positive"},
		{name: "job", flag: "--job-timeout", want: "job timeout must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			var stdout, stderr bytes.Buffer

			code := Run([]string{
				"agent", "type", "set", "planner",
				"--home", home,
				"--runtime", "codex",
				tc.flag, "0s",
			}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("agent type set exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRunAgentGCRemovesExpiredManagedInstances(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	now := time.Now().UTC().Add(-time.Hour)
	if err := store.UpsertAgentInstance(context.Background(), db.AgentInstance{
		Name:         "planner-bg-expired",
		Type:         "planner",
		Runtime:      "codex",
		RuntimeRef:   "550e8400-e29b-41d4-a716-446655440112",
		RepoFullName: "owner/repo",
		Role:         "planner",
		Capabilities: []string{"ask"},
		State:        "idle",
		CreatedAt:    now.Format(time.RFC3339Nano),
		LastUsedAt:   now.Format(time.RFC3339Nano),
		ExpiresAt:    now.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "gc", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent gc exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed 1 expired agent instances") {
		t.Fatalf("agent gc output = %q", stdout.String())
	}
}

func TestRunAgentAskValidatesInputAndAccess(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "ask", "planner", "--home", home}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("missing message exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "agent ask requires exactly one agent and one message") {
		t.Fatalf("missing message stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "missing", "hello", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("missing agent exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `agent "missing" not found`) {
		t.Fatalf("missing agent stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"agent", "subscribe", "reviewer",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440031",
		"--role", "reviewer",
		"--repo", "owner/repo",
		"--capability", "review",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe reviewer exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "reviewer", "hello", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("missing ask capability exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `agent "reviewer" lacks ask capability`) {
		t.Fatalf("missing ask capability stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"agent", "subscribe", "outsider",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440032",
		"--role", "planner",
		"--repo", "owner/other",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe outsider exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "ask", "outsider", "hello", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("disallowed repo exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `agent "outsider" is not allowed on "owner/repo"`) {
		t.Fatalf("disallowed repo stderr = %q", stderr.String())
	}
}

func TestRunAgentAskCancelsQueuedJobWhenRuntimeSessionBusy(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	t.Chdir(repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "subscribe", "planner",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440041",
		"--role", "planner",
		"--repo", "owner/repo",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe planner exit code = %d, stderr=%s", code, stderr.String())
	}
	store := openCLIJobStore(t, home)
	if acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: "runtime:codex:550e8400-e29b-41d4-a716-446655440041",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	store.Close()

	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "ask", "planner", "Write a plan", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("busy ask exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "runtime session runtime:codex:550e8400-e29b-41d4-a716-446655440041 is busy") {
		t.Fatalf("busy ask stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime calls = %+v, want none", runner.calls)
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one cancelled job", jobs)
	}
	if jobs[0].State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", jobs[0].State)
	}
	events, err := store.ListJobEvents(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasCLIJobEvent(events, string(workflow.JobCancelled)) || !hasCLIJobEvent(events, "runtime_lock_wait") {
		t.Fatalf("events = %+v, want cancellation and runtime_lock_wait", events)
	}
}

func hasCLIJobEvent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func TestRunAgentStartRejectsMissingTemplateBeforeRuntime(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "start", "thermo-review",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--template", "thermo-nuclear-code-quality-review",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("start exit code = %d, want 1", code)
	}
	want := "agent template thermo-nuclear-code-quality-review is not installed; run gitmoot agent template update thermo-nuclear-code-quality-review"
	if strings.TrimSpace(stderr.String()) != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started before template validation: %+v", runner.calls)
	}
}

func TestRunAgentStartRejectsMissingCustomTemplateBeforeRuntime(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "start", "frontend-reviewer",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--template", "frontend-reviewer",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("start exit code = %d, want 1", code)
	}
	want := "agent template frontend-reviewer is not installed; run gitmoot agent template add frontend-reviewer --file <path>"
	if strings.TrimSpace(stderr.String()) != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started before template validation: %+v", runner.calls)
	}
}

func TestRunAgentStartRejectsExistingAgentBeforeRuntime(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "subscribe", "lead", "--home", home, "--runtime", "codex", "--session", "last", "--role", "lead", "--repo", "owner/repo", "--capability", "ask"}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"agent", "start", "lead", "--home", home, "--runtime", "codex", "--repo", "owner/repo", "--path", repoDir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("start exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "agent lead already exists") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started before existing-agent validation: %+v", runner.calls)
	}
}

func TestRunAgentStartUpdateTemplateInstallsBeforeStart(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	restoreFetcher := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{content: "Updated review instructions."})
	defer restoreFetcher()
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440013"}` + "\n"}}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "start", "thermo-review",
		"--home", home,
		"--runtime", "codex",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--template", "thermo-nuclear-code-quality-review",
		"--update-template",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}
	prompt := runner.calls[0].args[len(runner.calls[0].args)-1]
	if !strings.Contains(prompt, "Updated review instructions.") {
		t.Fatalf("startup prompt missing updated template:\n%s", prompt)
	}
}

func TestRunAgentStartRejectsShellRuntimeBeforeTemplateUpdate(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	restoreFetcher := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{content: "should not fetch"})
	defer restoreFetcher()
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "start", "shell-agent",
		"--home", home,
		"--runtime", "shell",
		"--repo", "owner/repo",
		"--path", repoDir,
		"--template", "thermo-nuclear-code-quality-review",
		"--update-template",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("start exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "shell runtime does not support agent start") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started for shell agent: %+v", runner.calls)
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	if _, err := store.GetAgentTemplate(context.Background(), "thermo-nuclear-code-quality-review"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("template lookup error = %v, want sql.ErrNoRows", err)
	}
}

func TestRunAgentSubscribeAppliesInstalledTemplateDefaults(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), db.AgentTemplate{
		ID:             "thermo-nuclear-code-quality-review",
		Name:           "Thermo-Nuclear Code Quality Review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"agent", "subscribe", "thermo-review",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--repo", "jerryfane/gitmoot",
		"--template", "thermo-nuclear-code-quality-review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	store, err = db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "thermo-review")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Role != "reviewer" || agent.TemplateID != "thermo-nuclear-code-quality-review" || strings.Join(agent.Capabilities, ",") != "ask,review" {
		t.Fatalf("agent = %+v", agent)
	}
}

func TestRunAgentSubscribeUsesInstalledCustomTemplate(t *testing.T) {
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent("frontend-reviewer", "Review frontend behavior.\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "template", "add", "frontend-reviewer", "--home", home, "--file", promptPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"agent", "subscribe", "frontend-reviewer",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--repo", "jerryfane/gitmoot",
		"--template", "frontend-reviewer",
		"--role", "reviewer",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("subscribe without capabilities exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "does not define default capabilities") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "frontend-reviewer",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--repo", "jerryfane/gitmoot",
		"--template", "frontend-reviewer",
		"--role", "reviewer",
		"--capability", "ask",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "frontend-reviewer")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Role != "reviewer" || agent.TemplateID != "frontend-reviewer" || strings.Join(agent.Capabilities, ",") != "ask,review" {
		t.Fatalf("agent = %+v", agent)
	}
}

func TestRunAgentSubscribeRejectsMissingTemplateAndImplementCapability(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", "frontend-reviewer",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--repo", "jerryfane/gitmoot",
		"--template", "frontend-reviewer",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("missing custom template exit code = %d, want 1", code)
	}
	want := "agent template frontend-reviewer is not installed; run gitmoot agent template add frontend-reviewer --file <path>"
	if strings.TrimSpace(stderr.String()) != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "thermo-review",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--repo", "jerryfane/gitmoot",
		"--template", "thermo-nuclear-code-quality-review",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("missing template exit code = %d, want 1", code)
	}
	want = "agent template thermo-nuclear-code-quality-review is not installed; run gitmoot agent template update thermo-nuclear-code-quality-review"
	if strings.TrimSpace(stderr.String()) != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), db.AgentTemplate{
		ID:             "thermo-nuclear-code-quality-review",
		Name:           "Thermo-Nuclear Code Quality Review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "thermo-review",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--repo", "jerryfane/gitmoot",
		"--template", "thermo-nuclear-code-quality-review",
		"--capability", "implement",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("implement capability exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "does not allow implement capability") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunAgentSubscribeValidatesInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "subscribe", "bad name", "--runtime", "codex", "--session", "s", "--role", "reviewer", "--repo", "jerryfane/gitmoot"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid agent") {
		t.Fatalf("stderr = %q", stderr.String())
	}

}

func TestRunAgentDoctorPersistsHealth(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{
		"agent", "subscribe", "shell",
		"--home", home,
		"--runtime", "shell",
		"--session", "printf ok",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "doctor", "shell", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doctor exit code = %d, stderr=%s", code, stderr.String())
	}

	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "shell")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.HealthStatus != "ok" {
		t.Fatalf("health status = %q, want ok", agent.HealthStatus)
	}
}

func TestRunAgentDoctorClaudeReportsMaskedAuthEnv(t *testing.T) {
	withClaudeAuthEnv(t, map[string]string{runtime.ClaudeOAuthTokenEnv: "secret-token"})
	home := t.TempDir()
	subscribeClaudeAgent(t, home, "claude-ok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "doctor", "claude-ok", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doctor exit code = %d, stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "claude-auth-env ok") {
		t.Fatalf("stdout missing claude auth status:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), runtime.ClaudeOAuthTokenEnv+"=set") || strings.Contains(stdout.String(), "secret-token") {
		t.Fatalf("stdout leaked or missed masked auth detail:\n%s", stdout.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "claude-ok")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.HealthStatus != "ok" {
		t.Fatalf("health status = %q, want ok", agent.HealthStatus)
	}
}

func TestRunAgentDoctorClaudeFailsWhenAuthEnvMissing(t *testing.T) {
	withClaudeAuthEnv(t, nil)
	home := t.TempDir()
	subscribeClaudeAgent(t, home, "claude-missing")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "doctor", "claude-missing", "--home", home}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "claude-auth-env warn") || !strings.Contains(stderr.String(), "claude setup-token") {
		t.Fatalf("doctor output missing setup guidance:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "claude-missing")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.HealthStatus != "failed" {
		t.Fatalf("health status = %q, want failed", agent.HealthStatus)
	}
}

func TestRunAgentDoctorClaudeLiveRunsSmoke(t *testing.T) {
	withClaudeAuthEnv(t, nil)
	home := t.TempDir()
	subscribeClaudeAgent(t, home, "claude-live")
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: `{"result":"OK"}`}}}
	restoreRunner := replaceAgentDoctorRunner(runner)
	defer restoreRunner()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "doctor", "claude-live", "--home", home, "--live"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doctor exit code = %d, stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "claude-auth-env warn") || !strings.Contains(stdout.String(), "claude-live ok") {
		t.Fatalf("stdout missing live smoke result:\n%s", stdout.String())
	}
	runner.want(t, 0, "", "claude", "-p", "--output-format", "json", "--", runtime.ClaudeLiveCheckPrompt)
	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "claude-live")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.HealthStatus != "ok" {
		t.Fatalf("health status = %q, want ok", agent.HealthStatus)
	}
}

func TestRunAgentDoctorClaudeLiveClassifiesAuthFailure(t *testing.T) {
	withClaudeAuthEnv(t, map[string]string{runtime.ClaudeOAuthTokenEnv: "secret-token"})
	home := t.TempDir()
	subscribeClaudeAgent(t, home, "claude-live-fail")
	runner := &agentStartRunner{
		results: []subprocess.Result{{Stderr: "401 Invalid authentication credentials"}},
		errs:    []error{errors.New("exit 1")},
	}
	restoreRunner := replaceAgentDoctorRunner(runner)
	defer restoreRunner()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "doctor", "claude-live-fail", "--home", home, "--live"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "claude-live fail") || !strings.Contains(stderr.String(), "claude setup-token") {
		t.Fatalf("doctor output missing classified live failure:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	agent, err := store.GetAgent(context.Background(), "claude-live-fail")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.HealthStatus != "failed" {
		t.Fatalf("health status = %q, want failed", agent.HealthStatus)
	}
}

func subscribeClaudeAgent(t *testing.T, home string, name string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", name,
		"--home", home,
		"--runtime", "claude",
		"--session", "550e8400-e29b-41d4-a716-446655440099",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit code = %d, stderr=%s", code, stderr.String())
	}
}

func seedThermoTemplate(t *testing.T, home string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(context.Background(), db.AgentTemplate{
		ID:             "thermo-nuclear-code-quality-review",
		Name:           "Thermo-Nuclear Code Quality Review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
}

func seedPlannerTemplate(t *testing.T, home string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(context.Background(), db.AgentTemplate{
		ID:             "planner",
		Name:           "Gitmoot Plan and Goal Writer",
		SourceRepo:     "jerryfane/gitmoot",
		SourceRef:      "main",
		SourcePath:     "skills/gitmoot/agent-templates/planner.md",
		ResolvedCommit: "def456",
		Content:        "Plan and write goals.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
}

func replaceRuntimeFactory(factory runtime.Factory) func() {
	previous := newRuntimeFactory
	newRuntimeFactory = func() runtime.Factory {
		return factory
	}
	return func() {
		newRuntimeFactory = previous
	}
}

type agentStartRunner struct {
	mu      sync.Mutex
	results []subprocess.Result
	errs    []error
	calls   []agentStartCall
}

type agentStartCall struct {
	dir     string
	command string
	args    []string
}

type managedSyncLockRunner struct {
	t          *testing.T
	store      *db.Store
	runtimeKey string
	minTTL     time.Duration
	checked    bool
}

func (r *managedSyncLockRunner) Run(ctx context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	isResume := false
	for _, arg := range args {
		if arg == "resume" {
			isResume = true
			break
		}
	}
	if !isResume {
		return subprocess.Result{Command: command, Args: args, Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440222"}` + "\n"}, nil
	}
	lock, err := r.store.GetResourceLock(ctx, r.runtimeKey)
	if err != nil {
		r.t.Fatalf("GetResourceLock during resume returned error: %v", err)
	}
	acquiredAt, err := time.Parse(time.RFC3339Nano, lock.AcquiredAt)
	if err != nil {
		r.t.Fatalf("parse acquired_at %q: %v", lock.AcquiredAt, err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, lock.ExpiresAt)
	if err != nil {
		r.t.Fatalf("parse expires_at %q: %v", lock.ExpiresAt, err)
	}
	if ttl := expiresAt.Sub(acquiredAt); ttl < r.minTTL {
		r.t.Fatalf("runtime lock ttl = %s, want at least %s", ttl, r.minTTL)
	}
	r.checked = true
	return subprocess.Result{Command: command, Args: args, Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"}, nil
}

func (r *managedSyncLockRunner) LookPath(file string) (string, error) {
	if file == "" {
		return "", errors.New("empty file")
	}
	return "/usr/bin/" + file, nil
}

func (r *agentStartRunner) Run(_ context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, agentStartCall{dir: dir, command: command, args: append([]string{}, args...)})
	index := len(r.calls) - 1
	result := subprocess.Result{Command: command, Args: args}
	if index < len(r.results) {
		result = r.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(r.errs) {
		err = r.errs[index]
	}
	return result, err
}

func (r *agentStartRunner) LookPath(file string) (string, error) {
	if file == "" {
		return "", errors.New("empty file")
	}
	return "/usr/bin/" + file, nil
}

func (r *agentStartRunner) want(t *testing.T, index int, dir string, command string, wantPrefix ...string) {
	t.Helper()
	if index >= len(r.calls) {
		t.Fatalf("missing call %d; calls=%+v", index, r.calls)
	}
	call := r.calls[index]
	if call.dir != dir {
		t.Fatalf("call %d dir = %q, want %q", index, call.dir, dir)
	}
	if call.command != command {
		t.Fatalf("call %d command = %q, want %q", index, call.command, command)
	}
	if len(call.args) < len(wantPrefix) || !reflect.DeepEqual(call.args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("call %d args = %s\nwant prefix %s", index, strings.Join(call.args, " "), strings.Join(wantPrefix, " "))
	}
}

func withClaudeAuthEnv(t *testing.T, values map[string]string) {
	t.Helper()
	names := []string{
		runtime.ClaudeOAuthTokenEnv,
		runtime.AnthropicAPIKeyEnv,
		runtime.AnthropicAuthTokenEnv,
	}
	previous := make(map[string]string, len(names))
	present := make(map[string]bool, len(names))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			previous[name] = value
			present[name] = true
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	for name, value := range values {
		if err := os.Setenv(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range names {
			if present[name] {
				_ = os.Setenv(name, previous[name])
			} else {
				_ = os.Unsetenv(name)
			}
		}
	})
}

func replaceAgentDoctorRunner(runner subprocess.Runner) func() {
	original := agentDoctorRunner
	agentDoctorRunner = runner
	return func() {
		agentDoctorRunner = original
	}
}
