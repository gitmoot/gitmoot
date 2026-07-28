package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedPromptRecordFixture builds an isolated home with a tracked repo and an agent
// carrying a real installed template and a repo_scope, returning the home path and
// a persistent store for assertions.
func seedPromptRecordFixture(t *testing.T) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("db.Open returned error: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	if err := store.UpsertAgentTemplate(context.Background(), db.AgentTemplate{
		ID:             "planner",
		Name:           "Planner",
		SourceRepo:     "gitmoot/gitmoot",
		SourceRef:      "main",
		SourcePath:     "skills/gitmoot/agent-templates/planner.md",
		ResolvedCommit: "abc123",
		Content:        "Plan the work carefully.\n",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:         "lead",
		Role:         "coordinator",
		Runtime:      runtime.ShellRuntime,
		RepoScope:    "owner/repo",
		TemplateID:   "planner",
		Capabilities: []string{"ask", "implement"},
		HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	return home, store
}

func sessionJobsFor(t *testing.T, store *db.Store) []db.Job {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	var session []db.Job
	for _, j := range jobs {
		if j.ExternallyDriven {
			session = append(session, j)
		}
	}
	return session
}

// TestAgentPromptRecordOpensSessionJob proves `agent prompt <agent> --record`
// opens a running externally_driven session job for the agent's repo_scope and
// prints the prompt with the exact clock-out header naming the job id, and that
// --json carries the job id (#657).
func TestAgentPromptRecordOpensSessionJob(t *testing.T) {
	home, store := seedPromptRecordFixture(t)

	var stdout, stderr bytes.Buffer
	if code := runAgentPrompt([]string{"lead", "--record", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent prompt --record --json exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	var out agentPromptOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON %q: %v", stdout.String(), err)
	}
	if strings.TrimSpace(out.JobID) == "" {
		t.Fatalf("agent prompt --record --json carried no job_id: %q", stdout.String())
	}
	if !strings.Contains(out.Content, "Plan the work carefully.") {
		t.Fatalf("--record --json content = %q, want the resolved prompt", out.Content)
	}

	session := sessionJobsFor(t, store)
	if len(session) != 1 {
		t.Fatalf("session jobs = %d, want exactly 1 opened by --record", len(session))
	}
	job := session[0]
	if job.ID != out.JobID {
		t.Fatalf("session job id = %q, want the JSON job_id %q", job.ID, out.JobID)
	}
	if job.State != string(workflow.JobRunning) || !job.ExternallyDriven {
		t.Fatalf("recorded session job = %+v, want running externally_driven", job)
	}
	if job.Type != "implement" {
		t.Fatalf("recorded session job type = %q, want implement (default)", job.Type)
	}
	if payload, err := workflow.ParseJobPayload(job.Payload); err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	} else if payload.Repo != "owner/repo" {
		t.Fatalf("recorded session job repo = %q, want owner/repo (the agent repo_scope)", payload.Repo)
	}

	// Text mode prints the exact clock-out header naming the job id.
	stdout.Reset()
	stderr.Reset()
	if code := runAgentPrompt([]string{"lead", "--record", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent prompt --record (text) exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "gitmoot session job ") || !strings.Contains(text, "gitmoot job close ") {
		t.Fatalf("--record text output missing the clock-out header:\n%s", text)
	}
	if !strings.Contains(text, "Plan the work carefully.") {
		t.Fatalf("--record text output missing the prompt body:\n%s", text)
	}
}

// TestAgentPromptWithoutRecordIsByteIdenticalAndOpensNoJob is the invariant guard:
// a plain `agent prompt` (no --record) is a pure read — it opens NO job and its
// output is byte-identical to before the flag existed (no header, empty job_id).
func TestAgentPromptWithoutRecordIsByteIdenticalAndOpensNoJob(t *testing.T) {
	home, store := seedPromptRecordFixture(t)

	var stdout, stderr bytes.Buffer
	if code := runAgentPrompt([]string{"lead", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent prompt exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "gitmoot session job ") {
		t.Fatalf("plain agent prompt leaked a session-job header:\n%s", stdout.String())
	}
	if got := strings.TrimRight(stdout.String(), "\n"); got != "Plan the work carefully." {
		t.Fatalf("plain agent prompt output = %q, want the bare prompt", got)
	}

	// JSON form omits job_id entirely (byte-identical to the historical shape).
	stdout.Reset()
	stderr.Reset()
	if code := runAgentPrompt([]string{"lead", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent prompt --json exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "job_id") {
		t.Fatalf("plain agent prompt --json leaked a job_id field:\n%s", stdout.String())
	}

	if session := sessionJobsFor(t, store); len(session) != 0 {
		t.Fatalf("plain agent prompt opened %d session job(s), want 0", len(session))
	}
}

// TestAgentPromptRecordRepoFlagOverridesScope proves --repo overrides the agent's
// repo_scope for the registered-agent path (unchanged behavior).
func TestAgentPromptRecordRepoFlagOverridesScope(t *testing.T) {
	home, store := seedPromptRecordFixture(t)
	seedDaemonWorkerRepo(t, store, "owner/other", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := runAgentPrompt([]string{"lead", "--record", "--repo", "owner/other", "--type", "ask", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent prompt --record --repo exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	session := sessionJobsFor(t, store)
	if len(session) != 1 {
		t.Fatalf("session jobs = %d, want 1", len(session))
	}
	if session[0].Type != "ask" {
		t.Fatalf("session job type = %q, want ask (--type override)", session[0].Type)
	}
	payload, err := workflow.ParseJobPayload(session[0].Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.Repo != "owner/other" {
		t.Fatalf("session job repo = %q, want owner/other (--repo override)", payload.Repo)
	}
}

func TestAgentPromptRecordReviewRequiresAndPersistsExactTarget(t *testing.T) {
	home, store := seedPromptRecordFixture(t)

	for _, args := range [][]string{
		{"lead", "--record", "--type", "review", "--head-sha", "review-head", "--home", home},
		{"lead", "--record", "--type", "review", "--pr", "7", "--home", home},
	} {
		var stderr bytes.Buffer
		if code := runAgentPrompt(args, io.Discard, &stderr); code != 2 {
			t.Fatalf("agent prompt incomplete review target exit = %d, want 2; stderr=%q", code, stderr.String())
		}
	}
	if session := sessionJobsFor(t, store); len(session) != 0 {
		t.Fatalf("incomplete review target opened %d session jobs, want 0", len(session))
	}

	var stdout, stderr bytes.Buffer
	if code := runAgentPrompt([]string{
		"lead", "--record", "--type", "review", "--pr", "7",
		"--head-sha", "review-head", "--workflow", "review/prompt",
		"--home", home, "--json",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent prompt review --record exit = %d, stderr=%q", code, stderr.String())
	}
	session := sessionJobsFor(t, store)
	if len(session) != 1 {
		t.Fatalf("session jobs = %d, want 1", len(session))
	}
	payload, err := workflow.ParseJobPayload(session[0].Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.PullRequest != 7 || payload.HeadSHA != "review-head" || payload.WorkflowID != "review/prompt" {
		t.Fatalf("recorded review target = PR %d head %q workflow %q", payload.PullRequest, payload.HeadSHA, payload.WorkflowID)
	}
}

// TestAgentPromptRecordBareTemplateOpensSessionJob proves the #673 extension: an id
// that resolves to a bare TEMPLATE (no registered agent of that name) plus an
// explicit --repo opens a running externally_driven session job recorded against
// the TEMPLATE id as the agent identity + that repo.
func TestAgentPromptRecordBareTemplateOpensSessionJob(t *testing.T) {
	home, store := seedPromptRecordFixture(t)
	// The fixture registers an agent named "lead" carrying the "planner" template;
	// no agent is named "planner", so `planner` resolves to the bare template.

	var stdout, stderr bytes.Buffer
	if code := runAgentPrompt([]string{"planner", "--record", "--repo", "owner/repo", "--type", "implement", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent prompt --record on a bare template exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	var out agentPromptOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON %q: %v", stdout.String(), err)
	}
	if strings.TrimSpace(out.JobID) == "" {
		t.Fatalf("bare-template --record --json carried no job_id: %q", stdout.String())
	}
	if !strings.Contains(out.Content, "Plan the work carefully.") {
		t.Fatalf("bare-template --record content = %q, want the resolved template prompt", out.Content)
	}

	session := sessionJobsFor(t, store)
	if len(session) != 1 {
		t.Fatalf("session jobs = %d, want exactly 1 opened by --record", len(session))
	}
	job := session[0]
	if job.ID != out.JobID {
		t.Fatalf("session job id = %q, want the JSON job_id %q", job.ID, out.JobID)
	}
	if job.State != string(workflow.JobRunning) || !job.ExternallyDriven {
		t.Fatalf("recorded session job = %+v, want running externally_driven", job)
	}
	if job.Agent != "planner" {
		t.Fatalf("recorded session job agent = %q, want the template id planner (#673)", job.Agent)
	}
	if payload, err := workflow.ParseJobPayload(job.Payload); err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	} else if payload.Repo != "owner/repo" {
		t.Fatalf("recorded session job repo = %q, want owner/repo (the --repo value)", payload.Repo)
	}
}

// TestAgentPromptRecordBareTemplateRequiresRepo proves that recording a bare
// template WITHOUT --repo fails with a clear, actionable error and opens no job —
// a template has no repo scope to fall back on (#673).
func TestAgentPromptRecordBareTemplateRequiresRepo(t *testing.T) {
	home, store := seedPromptRecordFixture(t)

	var stderr bytes.Buffer
	if code := runAgentPrompt([]string{"planner", "--record", "--home", home}, io.Discard, &stderr); code == 0 {
		t.Fatalf("agent prompt --record on a bare template without --repo exit code = 0, want non-zero")
	}
	msg := stderr.String()
	if !strings.Contains(msg, "--repo") || !strings.Contains(msg, "template") {
		t.Fatalf("bare-template --record without --repo stderr = %q, want a clear --repo-required error", msg)
	}
	if session := sessionJobsFor(t, store); len(session) != 0 {
		t.Fatalf("bare-template --record without --repo opened %d session job(s), want 0", len(session))
	}
}
