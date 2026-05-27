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
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
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
	if err := os.WriteFile(promptPath, []byte("Review frontend behavior.\n"), 0o600); err != nil {
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
	if !strings.Contains(stdout.String(), "max_background: 1") || !strings.Contains(stdout.String(), "capabilities: ask") {
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
	runner.want(t, 0, repoDir, "codex", "exec", "--json", "--")

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
	if len(instances) != 1 || instances[0].Type != "planner" || instances[0].RuntimeRef != "550e8400-e29b-41d4-a716-446655440111" {
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
	if err := os.WriteFile(promptPath, []byte("Review frontend behavior.\n"), 0o600); err != nil {
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
	results []subprocess.Result
	errs    []error
	calls   []agentStartCall
}

type agentStartCall struct {
	dir     string
	command string
	args    []string
}

func (r *agentStartRunner) Run(_ context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
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
