package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
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

func TestRunAgentStartAppliesInstalledPresetDefaults(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedThermoPreset(t, home)
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
		"--preset", "thermo-nuclear-code-quality-review",
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
	if agent.Role != "reviewer" || agent.PresetID != "thermo-nuclear-code-quality-review" || strings.Join(agent.Capabilities, ",") != "ask,review" {
		t.Fatalf("agent = %+v", agent)
	}
	prompt := runner.calls[0].args[len(runner.calls[0].args)-1]
	if !strings.Contains(prompt, "Preset: thermo-nuclear-code-quality-review @ abc123") || !strings.Contains(prompt, "Review deeply.") {
		t.Fatalf("startup prompt missing preset content:\n%s", prompt)
	}
}

func TestRunAgentStartUsesInstalledCustomPreset(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte("Review frontend behavior.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"preset", "add", "frontend-reviewer", "--home", home, "--file", promptPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("preset add exit code = %d, stderr=%s", code, stderr.String())
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
		"--preset", "frontend-reviewer",
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
	if agent.Role != "agent" || agent.PresetID != "frontend-reviewer" || strings.Join(agent.Capabilities, ",") != "ask,review,implement" {
		t.Fatalf("agent = %+v", agent)
	}
	prompt := runner.calls[0].args[len(runner.calls[0].args)-1]
	if !strings.Contains(prompt, "Preset: frontend-reviewer @ sha256:") || !strings.Contains(prompt, "Review frontend behavior.") {
		t.Fatalf("startup prompt missing custom preset content:\n%s", prompt)
	}
}

func TestRunAgentStartAppliesPlannerPresetDefaults(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedPlannerPreset(t, home)
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
		"--preset", "gitmoot-plan-and-goal",
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
	if agent.Role != "planner" || agent.PresetID != "gitmoot-plan-and-goal" || strings.Join(agent.Capabilities, ",") != "ask" {
		t.Fatalf("agent = %+v", agent)
	}
	prompt := runner.calls[0].args[len(runner.calls[0].args)-1]
	if !strings.Contains(prompt, "Preset: gitmoot-plan-and-goal @ def456") || !strings.Contains(prompt, "Plan and write goals.") {
		t.Fatalf("startup prompt missing planner preset content:\n%s", prompt)
	}
}

func TestRunAgentStartAllowsImplementForPlannerPreset(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedPlannerPreset(t, home)
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
		"--preset", "gitmoot-plan-and-goal",
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

func TestRunAgentStartRejectsMissingPresetBeforeRuntime(t *testing.T) {
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
		"--preset", "thermo-nuclear-code-quality-review",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("start exit code = %d, want 1", code)
	}
	want := "preset thermo-nuclear-code-quality-review is not installed; run gitmoot preset update thermo-nuclear-code-quality-review"
	if strings.TrimSpace(stderr.String()) != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started before preset validation: %+v", runner.calls)
	}
}

func TestRunAgentStartRejectsMissingCustomPresetBeforeRuntime(t *testing.T) {
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
		"--preset", "frontend-reviewer",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("start exit code = %d, want 1", code)
	}
	want := "preset frontend-reviewer is not installed; run gitmoot preset add frontend-reviewer --file <path>"
	if strings.TrimSpace(stderr.String()) != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime was started before preset validation: %+v", runner.calls)
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

func TestRunAgentStartUpdatePresetInstallsBeforeStart(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	restoreFetcher := replacePresetFetcher(fakePresetFetcher{content: "Updated review instructions."})
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
		"--preset", "thermo-nuclear-code-quality-review",
		"--update-preset",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit code = %d, stderr=%s", code, stderr.String())
	}
	prompt := runner.calls[0].args[len(runner.calls[0].args)-1]
	if !strings.Contains(prompt, "Updated review instructions.") {
		t.Fatalf("startup prompt missing updated preset:\n%s", prompt)
	}
}

func TestRunAgentStartRejectsShellRuntimeBeforePresetUpdate(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	restoreFetcher := replacePresetFetcher(fakePresetFetcher{content: "should not fetch"})
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
		"--preset", "thermo-nuclear-code-quality-review",
		"--update-preset",
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
	if _, err := store.GetPreset(context.Background(), "thermo-nuclear-code-quality-review"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("preset lookup error = %v, want sql.ErrNoRows", err)
	}
}

func TestRunAgentSubscribeAppliesInstalledPresetDefaults(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertPreset(context.Background(), db.Preset{
		ID:             "thermo-nuclear-code-quality-review",
		Name:           "Thermo-Nuclear Code Quality Review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertPreset returned error: %v", err)
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
		"--preset", "thermo-nuclear-code-quality-review",
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
	if agent.Role != "reviewer" || agent.PresetID != "thermo-nuclear-code-quality-review" || strings.Join(agent.Capabilities, ",") != "ask,review" {
		t.Fatalf("agent = %+v", agent)
	}
}

func TestRunAgentSubscribeUsesInstalledCustomPreset(t *testing.T) {
	home := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "frontend.md")
	if err := os.WriteFile(promptPath, []byte("Review frontend behavior.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"preset", "add", "frontend-reviewer", "--home", home, "--file", promptPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("preset add exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"agent", "subscribe", "frontend-reviewer",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--repo", "jerryfane/gitmoot",
		"--preset", "frontend-reviewer",
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
		"--preset", "frontend-reviewer",
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
	if agent.Role != "reviewer" || agent.PresetID != "frontend-reviewer" || strings.Join(agent.Capabilities, ",") != "ask,review" {
		t.Fatalf("agent = %+v", agent)
	}
}

func TestRunAgentSubscribeRejectsMissingPresetAndImplementCapability(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", "frontend-reviewer",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--repo", "jerryfane/gitmoot",
		"--preset", "frontend-reviewer",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("missing custom preset exit code = %d, want 1", code)
	}
	want := "preset frontend-reviewer is not installed; run gitmoot preset add frontend-reviewer --file <path>"
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
		"--preset", "thermo-nuclear-code-quality-review",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("missing preset exit code = %d, want 1", code)
	}
	want = "preset thermo-nuclear-code-quality-review is not installed; run gitmoot preset update thermo-nuclear-code-quality-review"
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
	if err := store.UpsertPreset(context.Background(), db.Preset{
		ID:             "thermo-nuclear-code-quality-review",
		Name:           "Thermo-Nuclear Code Quality Review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertPreset returned error: %v", err)
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
		"--preset", "thermo-nuclear-code-quality-review",
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

func seedThermoPreset(t *testing.T, home string) {
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
	if err := store.UpsertPreset(context.Background(), db.Preset{
		ID:             "thermo-nuclear-code-quality-review",
		Name:           "Thermo-Nuclear Code Quality Review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertPreset returned error: %v", err)
	}
}

func seedPlannerPreset(t *testing.T, home string) {
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
	if err := store.UpsertPreset(context.Background(), db.Preset{
		ID:             "gitmoot-plan-and-goal",
		Name:           "Gitmoot Plan and Goal Writer",
		SourceRepo:     "jerryfane/gitmoot",
		SourceRef:      "main",
		SourcePath:     "skills/gitmoot/presets/gitmoot-plan-and-goal.md",
		ResolvedCommit: "def456",
		Content:        "Plan and write goals.",
	}); err != nil {
		t.Fatalf("UpsertPreset returned error: %v", err)
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
