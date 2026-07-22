package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/skillopt"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func TestSkillOptTrainGeneratorSelectionHonorsExplicitGeneratorType(t *testing.T) {
	dispatch, err := skillOptTrainGeneratorSelection(
		context.Background(),
		nil,
		db.SkillOptTrainSession{TemplateVersionID: "planner@v1"},
		db.SkillOptTrainIteration{BaseTemplateVersionID: "planner@v1"},
		db.EvalRun{TemplateVersionID: "planner@v1"},
		skillOptTrainContinueRequest{GeneratorType: "skillopt-generator"},
	)
	if err != nil {
		t.Fatalf("skillOptTrainGeneratorSelection returned error: %v", err)
	}
	if dispatch.Mode != skillOptTrainGenerationModeSkillOptGenerator || dispatch.Agent != "skillopt-generator" || dispatch.Type != "skillopt-generator" {
		t.Fatalf("dispatch = %+v", dispatch)
	}

	dispatch, err = skillOptTrainGeneratorSelection(
		context.Background(),
		nil,
		db.SkillOptTrainSession{TemplateVersionID: "planner@v1"},
		db.SkillOptTrainIteration{BaseTemplateVersionID: "planner@v1"},
		db.EvalRun{TemplateVersionID: "planner@v1"},
		skillOptTrainContinueRequest{GeneratorAgent: "custom-generator"},
	)
	if err != nil {
		t.Fatalf("custom skillOptTrainGeneratorSelection returned error: %v", err)
	}
	if dispatch.Mode != skillOptTrainGenerationModeCustomAgent || dispatch.Agent != "custom-generator" || dispatch.Type != "" {
		t.Fatalf("custom dispatch = %+v", dispatch)
	}
}

func TestSkillOptTrainContinueGeneratesOptionsWithCurrentSkill(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	template := cliSkillOptTemplate("planner", "Plan the work.")
	if err := store.UpsertAgentTemplate(context.Background(), template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440201"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nHero with strong product narrative.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440202"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nDashboard-led layout with proof metrics.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440203"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nCheckout analytics proof block.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440204"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nLifecycle commerce story with motion notes.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	// Live generation progress goes to stderr: a start line and one line per
	// completed option, ending at (4/4).
	for _, want := range []string{"generating 4 options (2 items x 2)", "option ", " done (4/4) - "} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("generation progress stderr missing %q:\n%s", want, stderr.String())
		}
	}
	for _, want := range []string{
		"current_phase: options_generated",
		"continue_ready: true",
		"generated_options: 4",
		"jobs: 4",
		"generator_agent: skillopt-target-landing-train-review-001-",
		"generator_runtime: codex",
		"next: publish the human review packet",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 8 {
		t.Fatalf("runtime calls = %+v, want one start and delivery per option", runner.calls)
	}
	runner.want(t, 0, repoDir, "codex", "exec", "--json", "--")
	runner.want(t, 1, repoDir, "codex", "exec", "--json", "resume", "550e8400-e29b-41d4-a716-446655440201", "--")
	if !strings.Contains(runner.calls[1].args[len(runner.calls[1].args)-1], "Option label: A") || !strings.Contains(runner.calls[1].args[len(runner.calls[1].args)-1], "Generate one review option") {
		t.Fatalf("generation prompt = %q", runner.calls[1].args[len(runner.calls[1].args)-1])
	}
	if !strings.Contains(runner.calls[0].args[len(runner.calls[0].args)-1], "Template: planner@v1") || !strings.Contains(runner.calls[0].args[len(runner.calls[0].args)-1], "Plan the work.") {
		t.Fatalf("target-skill startup prompt = %q", runner.calls[0].args[len(runner.calls[0].args)-1])
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateOptionsGenerated || !strings.Contains(iteration.MetadataJSON, `"status":"succeeded"`) || !strings.Contains(iteration.MetadataJSON, `"prompts"`) {
		t.Fatalf("iteration after continue = %+v metadata=%s", iteration, iteration.MetadataJSON)
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 4 {
		t.Fatalf("jobs = %+v, want four generated jobs", jobs)
	}
	for _, job := range jobs {
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("daemonJobPayload returned error: %v", err)
		}
		if payload.Repo != "owner/workspace" {
			t.Fatalf("generated job repo = %q, want owner/workspace", payload.Repo)
		}
	}
	items, err := store.ListEvalReviewItems(context.Background(), "landing-train-review-001")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	for _, item := range items {
		options, err := store.ListEvalReviewOptions(context.Background(), "landing-train-review-001", item.ItemID)
		if err != nil {
			t.Fatalf("ListEvalReviewOptions %s returned error: %v", item.ItemID, err)
		}
		if len(options) != 2 || options[0].Label != "a" || options[1].Label != "b" {
			t.Fatalf("options for %s = %+v", item.ItemID, options)
		}
		artifactRecord, err := store.GetEvalArtifact(context.Background(), options[0].ArtifactID)
		if err != nil {
			t.Fatalf("GetEvalArtifact %s returned error: %v", options[0].ArtifactID, err)
		}
		if artifactRecord.MediaType != "text/markdown" || artifactRecord.Driver != "text" || !strings.Contains(options[0].MetadataJSON, `"job_id"`) || !strings.Contains(options[0].MetadataJSON, `"generation_mode":"target_skill"`) || !strings.Contains(options[0].MetadataJSON, `"template_version_id":"planner@v1"`) {
			t.Fatalf("artifact=%+v option=%+v", artifactRecord, options[0])
		}
	}

	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: review_published") ||
		!strings.Contains(stdout.String(), "continue_ready: true") ||
		!strings.Contains(stdout.String(), "review_repo: owner/product") ||
		!strings.Contains(stdout.String(), "preview_urls: 0") {
		t.Fatalf("second continue stdout = %q", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "owner/product" ||
		!strings.Contains(fakeGitHub.createdIssue.Body, "## Review Items") ||
		!strings.Contains(fakeGitHub.createdIssue.Body, "| Option | Reply |") ||
		strings.Contains(fakeGitHub.createdIssue.Body, "## Inline Options Without Public Links") {
		t.Fatalf("created review issue = %+v", fakeGitHub.createdIssue)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("third train continue without comments exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: review_published",
		"github_feedback_sync: failed",
		"github_feedback_error: no comments found",
		"next: sync human feedback from the review surface",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("third continue without comments stdout missing %q:\n%s", want, stdout.String())
		}
	}

	fakeGitHub.comments = map[int64][]github.IssueComment{
		8: {
			{
				ID: 11,
				Body: `LGTM, copying the review block.

` + "```yaml" + `
run_id: landing-train-review-001
items:
  - item_id: hero-saas
    ranking:
      - B > A
    quality: acceptable
    continue_mode: refine
    promote: no
    reasoning: Option B has the clearer hero.
  - item_id: ecommerce-proof
    ranking:
      - A > B
    quality: acceptable
    continue_mode: refine
    promote: no
    reasoning: Option A has stronger proof.
` + "```",
				URL:       "https://github.com/owner/product/issues/8#issuecomment-11",
				Author:    "jerry",
				CreatedAt: "2026-06-03T12:00:00Z",
			},
		},
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("fourth train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: feedback_synced",
		"continue_ready: true",
		"github_feedback_sync: imported",
		"github_feedback_events: 2",
		"feedback_events: 2",
		"next: export the training package before running the optimizer",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("fourth continue stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(fakeGitHub.listedComments) != 2 || fakeGitHub.listedComments[1].Repo.FullName() != "owner/product" || fakeGitHub.listedComments[1].IssueNumber != 8 {
		t.Fatalf("listed comments = %+v", fakeGitHub.listedComments)
	}
}

func TestSkillOptTrainGenerationStampsCorrelationIDs(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440501"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nHero with strong product narrative.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440502"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nDashboard-led layout with proof metrics.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440503"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nCheckout analytics proof block.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440504"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nLifecycle commerce story with motion notes.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()

	const runID = "landing-train-review-001"
	prefix := "skillopt-train-generation:" + runID
	if prefix != "skillopt-train-generation:"+runID {
		t.Fatalf("correlation prefix = %q", prefix)
	}

	allJobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	var children []db.Job
	for _, job := range allJobs {
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("daemonJobPayload returned error: %v", err)
		}
		if strings.HasPrefix(payload.TaskID, prefix+":") {
			children = append(children, job)
		}
	}
	if len(children) != 4 {
		t.Fatalf("generation jobs with TaskID prefix %q = %d, want 4: %+v", prefix, len(children), children)
	}

	// Each generation child carries the per-option TaskID encoding
	// (run, item, label, attempt). It must NOT carry ParentJobID/RootJobID:
	// those are delegation-engine fields, and a synthetic value would make
	// AdvanceJob fail for a requeued generation job.
	wantTaskIDs := map[string]bool{
		skillOptTrainGenerationTaskID(runID, "hero-saas", "a", 0):       false,
		skillOptTrainGenerationTaskID(runID, "hero-saas", "b", 0):       false,
		skillOptTrainGenerationTaskID(runID, "ecommerce-proof", "a", 0): false,
		skillOptTrainGenerationTaskID(runID, "ecommerce-proof", "b", 0): false,
	}
	for _, job := range children {
		if job.ParentJobID != "" {
			t.Fatalf("job %s parent_job_id = %q, want empty (generation jobs are not delegations)", job.ID, job.ParentJobID)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("daemonJobPayload returned error: %v", err)
		}
		if payload.ParentJobID != "" || payload.RootJobID != "" {
			t.Fatalf("job %s payload parent/root = %q/%q, want empty", job.ID, payload.ParentJobID, payload.RootJobID)
		}
		seen, ok := wantTaskIDs[payload.TaskID]
		if !ok {
			t.Fatalf("job %s has unexpected task_id %q", job.ID, payload.TaskID)
		}
		if seen {
			t.Fatalf("duplicate generation task_id %q", payload.TaskID)
		}
		wantTaskIDs[payload.TaskID] = true
	}
	for taskID, seen := range wantTaskIDs {
		if !seen {
			t.Fatalf("generation task_id %q was not stamped on any child job", taskID)
		}
	}
}

func TestSkillOptTrainContinueGeneratesRequiredVuePreviewBundles(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "preview-train",
		"--workspace-repo", "owner/workspace",
		"--preview-repo", "owner/previews",
		"--request", "Train landing page previews.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440501"}` + "\n"},
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option A hero")),
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440502"}` + "\n"},
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option B hero")),
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440503"}` + "\n"},
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option A proof")),
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440504"}` + "\n"},
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option B proof")),
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "preview-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: options_generated") || !strings.Contains(stdout.String(), "generated_options: 4") {
		t.Fatalf("train continue stdout = %s", stdout.String())
	}
	prompt := runner.calls[1].args[len(runner.calls[1].args)-1]
	for _, want := range []string{
		"Vue/Vite preview bundle",
		"summary as a string value",
		"Do not set gitmoot_result.summary to a nested object",
		"package.json, index.html, src/main.js, src/App.vue",
		"Do not include local absolute paths",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("preview prompt missing %q:\n%s", want, prompt)
		}
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	options, err := store.ListEvalReviewOptions(context.Background(), "preview-train-review-001", "hero-saas")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 2 {
		t.Fatalf("options = %+v", options)
	}
	artifactRecord, err := store.GetEvalArtifact(context.Background(), options[0].ArtifactID)
	if err != nil {
		t.Fatalf("GetEvalArtifact returned error: %v", err)
	}
	if artifactRecord.MediaType != "application/json" || artifactRecord.Driver != skillopt.TrainPreviewRendererVueVite {
		t.Fatalf("artifact = %+v", artifactRecord)
	}
	artifactContent, err := artifact.NewStore(paths.ArtifactBlobs).Read(artifactRecord.Hash)
	if err != nil {
		t.Fatalf("Read preview bundle artifact returned error: %v", err)
	}
	if _, err := skillopt.ParsePreviewBundle(artifactContent); err != nil {
		t.Fatalf("stored preview bundle did not parse: %v\n%s", err, string(artifactContent))
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(options[0].MetadataJSON), &metadata); err != nil {
		t.Fatalf("option metadata unmarshal returned error: %v", err)
	}
	bundleMetadata, ok := metadata["preview_bundle"].(map[string]any)
	if !ok {
		t.Fatalf("option metadata missing preview_bundle: %s", options[0].MetadataJSON)
	}
	if bundleMetadata["renderer"] != skillopt.TrainPreviewRendererVueVite || int(bundleMetadata["file_count"].(float64)) != 4 || bundleMetadata["build_command"] != "npm run build" || bundleMetadata["dist_dir"] != "dist" {
		t.Fatalf("preview bundle metadata = %+v", bundleMetadata)
	}
	if _, ok := bundleMetadata["content"]; ok {
		t.Fatalf("preview bundle metadata included file content: %+v", bundleMetadata)
	}

	previewDir := t.TempDir()
	runGit(t, previewDir, "init")
	runGit(t, previewDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, previewDir, "config", "user.name", "Gitmoot")
	runGit(t, previewDir, "branch", "-m", "main")
	runGit(t, previewDir, "remote", "add", "origin", "https://github.com/owner/previews.git")
	if err := os.WriteFile(filepath.Join(previewDir, "README.md"), []byte("previews\n"), 0o644); err != nil {
		t.Fatalf("write preview README: %v", err)
	}
	runGit(t, previewDir, "add", "README.md")
	runGit(t, previewDir, "commit", "-m", "init")
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "previews", CheckoutPath: previewDir, PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo preview returned error: %v", err)
	}
	previewRunner := &skillOptTrainFakePreviewRunner{}
	oldPreviewRunner := skillOptTrainPreviewRunner
	skillOptTrainPreviewRunner = previewRunner
	defer func() {
		skillOptTrainPreviewRunner = oldPreviewRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "preview-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: review_published") ||
		!strings.Contains(stdout.String(), "review_repo: owner/previews") ||
		!strings.Contains(stdout.String(), "preview_urls: 4") {
		t.Fatalf("second train continue stdout = %s", stdout.String())
	}
	wantPreviewURL := "https://owner.github.io/previews/runs/preview-train-review-001/hero-saas/a/"
	if fakeGitHub.createdIssue.Repo.FullName() != "owner/previews" ||
		!strings.Contains(fakeGitHub.createdIssue.Body, "| A | [open]("+wantPreviewURL+") |") ||
		strings.Contains(fakeGitHub.createdIssue.Body, "## Inline Options Without Public Links") ||
		strings.Contains(fakeGitHub.createdIssue.Body, `"renderer":"vue-vite"`) {
		t.Fatalf("created preview review issue = %+v\n%s", fakeGitHub.createdIssue, fakeGitHub.createdIssue.Body)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 8)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.RunID != "preview-train-review-001" ||
		watch.Status != db.SkillOptReviewWatchStatusWatching ||
		watch.StaleThresholdSeconds != int64(skillOptReviewWatchDefaultStaleThreshold.Seconds()) ||
		!strings.Contains(watch.ExpectedItemIDsJSON, "hero-saas") {
		t.Fatalf("review watch = %+v", watch)
	}
	if _, err := os.Stat(filepath.Join(previewDir, "runs", "preview-train-review-001", "hero-saas", "a", "index.html")); err != nil {
		t.Fatalf("preview index was not published: %v", err)
	}
	options, err = store.ListEvalReviewOptions(context.Background(), "preview-train-review-001", "hero-saas")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions after publish returned error: %v", err)
	}
	if !strings.Contains(options[0].MetadataJSON, `"preview_url":"`+wantPreviewURL+`"`) {
		t.Fatalf("option metadata missing preview_url: %s", options[0].MetadataJSON)
	}
}

func TestSkillOptTrainContinueRetriesInvalidRequiredVuePreviewOption(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "preview-retry-train",
		"--workspace-repo", "owner/workspace",
		"--preview-repo", "owner/previews",
		"--request", "Train landing page previews.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440504"}` + "\n"},
		cliImplementedSummaryResult(t, "# Option A\n\nMarkdown is not a preview bundle."),
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option A retry hero")),
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option B hero")),
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option A proof")),
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option B proof")),
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "preview-retry-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: options_generated") ||
		!strings.Contains(stdout.String(), "generated_options: 4") ||
		!strings.Contains(stdout.String(), "jobs: 5") {
		t.Fatalf("train continue stdout = %s", stdout.String())
	}
	if len(runner.calls) != 6 {
		t.Fatalf("runtime calls = %+v, want start plus five deliveries", runner.calls)
	}
	retryPrompt := runner.calls[2].args[len(runner.calls[2].args)-1]
	for _, want := range []string{
		"Retry this same review option only",
		"previous generated artifact failed validation",
		"decode preview bundle JSON",
		"Option label: A",
	} {
		if !strings.Contains(retryPrompt, want) {
			t.Fatalf("retry prompt missing %q:\n%s", want, retryPrompt)
		}
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	options, err := store.ListEvalReviewOptions(context.Background(), "preview-retry-train-review-001", "")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 4 {
		t.Fatalf("options = %+v", options)
	}
	retriedOptions := 0
	for _, option := range options {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(option.MetadataJSON), &metadata); err != nil {
			t.Fatalf("option metadata unmarshal returned error: %v", err)
		}
		retryAttempts, ok := metadata["retry_attempts"].(float64)
		if !ok {
			continue
		}
		retriedOptions++
		if int(retryAttempts) != 1 {
			t.Fatalf("retried option metadata = %+v", metadata)
		}
		if _, ok := metadata["validation_errors"].([]any); !ok {
			t.Fatalf("retried option metadata missing validation_errors: %+v", metadata)
		}
		if !strings.Contains(option.MetadataJSON, "decode preview bundle JSON") {
			t.Fatalf("retried option metadata missing validation error text: %s", option.MetadataJSON)
		}
	}
	if retriedOptions != 1 {
		t.Fatalf("retried options = %d, want 1; options=%+v", retriedOptions, options)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "preview-retry-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if !strings.Contains(iteration.MetadataJSON, `"attempt":1`) || !strings.Contains(iteration.MetadataJSON, `"validation_error"`) {
		t.Fatalf("iteration metadata missing retry attempt/error: %s", iteration.MetadataJSON)
	}
}

func TestSkillOptTrainContinueAllowsOptionalVuePreviewFallback(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "optional-preview-train",
		"--workspace-repo", "owner/workspace",
		"--preview-repo", "owner/previews",
		"--preview-mode", "optional",
		"--request", "Train landing page previews.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440503"}` + "\n"},
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option A optional preview")),
		cliImplementedSummaryResult(t, "# Option B\n\nMarkdown fallback."),
		cliImplementedSummaryResult(t, "# Option A\n\nMarkdown fallback."),
		cliImplementedSummaryResult(t, "# Option B\n\nMarkdown fallback."),
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optional-preview-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	prompt := runner.calls[1].args[len(runner.calls[1].args)-1]
	for _, want := range []string{
		"optional Vue/Vite previews",
		"Prefer a Vue/Vite preview bundle",
		"plain text or markdown is accepted only as inline fallback",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("optional preview prompt missing %q:\n%s", want, prompt)
		}
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	options, err := store.ListEvalReviewOptions(context.Background(), "optional-preview-train-review-001", "")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 4 {
		t.Fatalf("options = %+v", options)
	}
	var bundleOptions int
	var fallbackOptions int
	for _, option := range options {
		artifactRecord, err := store.GetEvalArtifact(context.Background(), option.ArtifactID)
		if err != nil {
			t.Fatalf("GetEvalArtifact returned error: %v", err)
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(option.MetadataJSON), &metadata); err != nil {
			t.Fatalf("option metadata unmarshal returned error: %v", err)
		}
		_, hasBundleMetadata := metadata["preview_bundle"]
		switch {
		case artifactRecord.MediaType == "application/json" && artifactRecord.Driver == skillopt.TrainPreviewRendererVueVite:
			bundleOptions++
			if !hasBundleMetadata {
				t.Fatalf("bundle option metadata missing preview_bundle: %s", option.MetadataJSON)
			}
		case artifactRecord.MediaType == "text/markdown" && artifactRecord.Driver == "text":
			fallbackOptions++
			if hasBundleMetadata {
				t.Fatalf("fallback option metadata unexpectedly included preview_bundle: %s", option.MetadataJSON)
			}
		default:
			t.Fatalf("unexpected optional preview artifact = %+v", artifactRecord)
		}
	}
	if bundleOptions == 0 || fallbackOptions == 0 {
		t.Fatalf("optional preview generated bundleOptions=%d fallbackOptions=%d", bundleOptions, fallbackOptions)
	}

	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optional-preview-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: review_published") ||
		!strings.Contains(stdout.String(), "review_repo: owner/previews") ||
		!strings.Contains(stdout.String(), "preview_urls: 0") {
		t.Fatalf("second train continue stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "owner/previews" ||
		!strings.Contains(fakeGitHub.createdIssue.Body, "Vue/Vite preview source") ||
		strings.Contains(fakeGitHub.createdIssue.Body, `"renderer":"vue-vite"`) {
		t.Fatalf("optional preview fallback issue = %+v\n%s", fakeGitHub.createdIssue, fakeGitHub.createdIssue.Body)
	}
}

func TestSkillOptTrainContinueFailsRequiredVuePreviewForProseOutput(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "preview-train",
		"--workspace-repo", "owner/workspace",
		"--preview-repo", "owner/previews",
		"--request", "Train landing page previews.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440502"}` + "\n"},
		cliImplementedSummaryResult(t, "# Option A\n\nMarkdown is not a preview bundle."),
		cliImplementedSummaryResult(t, "# Option A\n\nMarkdown is not a preview bundle."),
		cliImplementedSummaryResult(t, "# Option A\n\nMarkdown is not a preview bundle."),
		cliImplementedSummaryResult(t, "# Option A\n\nMarkdown is not a preview bundle."),
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "preview-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "validation_class=preview_bundle") ||
		!strings.Contains(stderr.String(), "retry_count=1") ||
		!strings.Contains(stderr.String(), "decode preview bundle JSON") {
		t.Fatalf("preview bundle failure stderr = %q", stderr.String())
	}
	if len(runner.calls) > 5 {
		t.Fatalf("runtime calls = %+v, want bounded per-option retries only", runner.calls)
	}
	retryPrompts := 0
	for _, call := range runner.calls {
		if len(call.args) > 0 && strings.Contains(call.args[len(call.args)-1], "Retry this same review option only") {
			retryPrompts++
		}
	}
	if retryPrompts == 0 || retryPrompts > 2 {
		t.Fatalf("retry prompt count = %d calls=%+v", retryPrompts, runner.calls)
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "preview-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady || !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) || !strings.Contains(iteration.MetadataJSON, "preview bundle") {
		t.Fatalf("iteration after preview failure = %+v metadata=%s", iteration, iteration.MetadataJSON)
	}
	options, err := store.ListEvalReviewOptions(context.Background(), "preview-train-review-001", "hero-saas")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("failure persisted preview options: %+v", options)
	}
}

func TestSkillOptTrainContinueRejectsNonImplementedGenerationResult(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440401"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"changes_requested","summary":"This is a review finding, not generated content.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"changes_requested","summary":"This is still a review finding, not generated content.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "returned changes_requested, want implemented") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	for _, call := range runner.calls {
		if len(call.args) > 0 && strings.Contains(call.args[len(call.args)-1], "Retry this same review option only") {
			t.Fatalf("non-retryable generation failure retried: %+v", runner.calls)
		}
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady || !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) {
		t.Fatalf("iteration after failure = %+v metadata=%s", iteration, iteration.MetadataJSON)
	}
	options, err := store.ListEvalReviewOptions(context.Background(), "landing-train-review-001", "hero-saas")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("non-generation decision persisted options: %+v", options)
	}
}

func TestSkillOptTrainContinueRecoversCompleteGeneratedOptions(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	store = openCLIJobStore(t, home)
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	items, err := store.ListEvalReviewItems(context.Background(), "landing-train-review-001")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	writes := make([]db.EvalReviewGenerationWrite, 0, len(items))
	for _, item := range items {
		var artifacts []db.EvalArtifact
		var options []db.EvalReviewOption
		for _, label := range []string{"a", "b"} {
			artifactRecord, err := prepareReviewItemContentArtifact(blobStore, "landing-train-review-001", item.ItemID, "option-"+label, []byte("existing option "+label), "text/markdown", "text")
			if err != nil {
				t.Fatalf("prepareReviewItemContentArtifact returned error: %v", err)
			}
			artifacts = append(artifacts, artifactRecord)
			options = append(options, db.EvalReviewOption{
				RunID:      "landing-train-review-001",
				ItemID:     item.ItemID,
				Label:      label,
				ArtifactID: artifactRecord.ID,
				Role:       "option",
			})
		}
		writes = append(writes, db.EvalReviewGenerationWrite{
			ItemID:    item.ItemID,
			Artifacts: artifacts,
			Options:   options,
		})
	}
	if err := store.ReplaceGeneratedEvalReviewArtifacts(context.Background(), "landing-train-review-001", writes); err != nil {
		t.Fatalf("ReplaceGeneratedEvalReviewArtifacts returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after generated option seed returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue recovery exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: options_generated") || !strings.Contains(stdout.String(), "generated_options: 4") {
		t.Fatalf("train continue recovery stdout = %s", stdout.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateOptionsGenerated || !strings.Contains(iteration.MetadataJSON, `"status":"recovered"`) {
		t.Fatalf("iteration after recovery = %+v metadata=%s", iteration, iteration.MetadataJSON)
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("recovery should not create generation jobs: %+v", jobs)
	}
}

// TestSkillOptTrainContinuePersistsCompletedItemBeforeLaterFailure proves the
// core data-loss fix: a completed item is committed the moment it finishes, so a
// later item's failure cannot lose it, and a resume completes without
// regenerating the already-durable item.
func TestSkillOptTrainContinuePersistsCompletedItemBeforeLaterFailure(t *testing.T) {
	home := t.TempDir()
	startSkillOptTrainGenerationForPersistTest(t, home)
	const runID = "landing-train-review-001"

	// hero-saas fails; ecommerce-proof succeeds and must survive.
	failRunner := &skillOptTrainItemAwareRunner{failItems: map[string]bool{"hero-saas": true}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: failRunner})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	restoreFactory()
	if code != 1 {
		t.Fatalf("first continue exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady || !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) {
		t.Fatalf("iteration after failure = %+v metadata=%s", iteration, iteration.MetadataJSON)
	}
	completedOptions, err := store.ListEvalReviewOptions(context.Background(), runID, "ecommerce-proof")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions ecommerce-proof returned error: %v", err)
	}
	if len(completedOptions) != 2 {
		t.Fatalf("completed item not durable: ecommerce-proof options = %+v", completedOptions)
	}
	failedOptions, err := store.ListEvalReviewOptions(context.Background(), runID, "hero-saas")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions hero-saas returned error: %v", err)
	}
	if len(failedOptions) != 0 {
		t.Fatalf("failed item persisted options: hero-saas options = %+v", failedOptions)
	}
	completedArtifactIDs := []string{completedOptions[0].ArtifactID, completedOptions[1].ArtifactID}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after first continue returned error: %v", err)
	}

	// Resume with everything succeeding: only hero-saas should be regenerated.
	successRunner := &skillOptTrainItemAwareRunner{}
	restoreFactory = replaceRuntimeFactory(runtime.Factory{Runner: successRunner})
	defer restoreFactory()
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resume continue exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: options_generated") || !strings.Contains(stdout.String(), "generated_options: 4") {
		t.Fatalf("resume continue stdout = %s", stdout.String())
	}
	// Only the incomplete item's two options were regenerated (1 start + 1
	// delivery per option).
	for _, prompt := range successRunner.prompts {
		if strings.Contains(prompt, "Item id: ecommerce-proof") {
			t.Fatalf("resume regenerated the already-complete item: %q", prompt)
		}
		if !strings.Contains(prompt, "Item id: hero-saas") {
			t.Fatalf("resume delivery for unexpected item: %q", prompt)
		}
	}
	if len(successRunner.prompts) != 2 {
		t.Fatalf("resume option deliveries = %d, want 2 (only hero-saas)", len(successRunner.prompts))
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	for _, itemID := range []string{"ecommerce-proof", "hero-saas"} {
		options, err := store.ListEvalReviewOptions(context.Background(), runID, itemID)
		if err != nil {
			t.Fatalf("ListEvalReviewOptions %s returned error: %v", itemID, err)
		}
		if len(options) != 2 || options[0].Label != "a" || options[1].Label != "b" {
			t.Fatalf("options for %s after resume = %+v", itemID, options)
		}
	}
	// The durable item's artifacts were not rewritten by the resume.
	resumeOptions, err := store.ListEvalReviewOptions(context.Background(), runID, "ecommerce-proof")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions ecommerce-proof after resume returned error: %v", err)
	}
	if resumeOptions[0].ArtifactID != completedArtifactIDs[0] || resumeOptions[1].ArtifactID != completedArtifactIDs[1] {
		t.Fatalf("resume rewrote durable item artifacts: before=%v after=%v", completedArtifactIDs, []string{resumeOptions[0].ArtifactID, resumeOptions[1].ArtifactID})
	}
}

// TestSkillOptTrainContinueResumeRegeneratesOnlyIncompleteItems seeds one
// complete item, then verifies resume regenerates only the missing item with
// correct totals and no duplicate options.
func TestSkillOptTrainContinueResumeRegeneratesOnlyIncompleteItems(t *testing.T) {
	home := t.TempDir()
	startSkillOptTrainGenerationForPersistTest(t, home)
	const runID = "landing-train-review-001"
	paths := config.PathsForHome(home)

	// Seed ecommerce-proof as already complete (durable from a prior partial run).
	store := openCLIJobStore(t, home)
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	var seededArtifactIDs []string
	var artifacts []db.EvalArtifact
	var options []db.EvalReviewOption
	for _, label := range []string{"a", "b"} {
		artifactRecord, err := prepareReviewItemContentArtifact(blobStore, runID, "ecommerce-proof", "option-"+label, []byte("seeded "+label), "text/markdown", "text")
		if err != nil {
			t.Fatalf("prepareReviewItemContentArtifact returned error: %v", err)
		}
		artifacts = append(artifacts, artifactRecord)
		seededArtifactIDs = append(seededArtifactIDs, artifactRecord.ID)
		options = append(options, db.EvalReviewOption{
			RunID:      runID,
			ItemID:     "ecommerce-proof",
			Label:      label,
			ArtifactID: artifactRecord.ID,
			Role:       "option",
		})
	}
	if err := store.ReplaceGeneratedEvalReviewArtifactsForItem(context.Background(), runID, db.EvalReviewGenerationWrite{
		ItemID:    "ecommerce-proof",
		Artifacts: artifacts,
		Options:   options,
	}); err != nil {
		t.Fatalf("ReplaceGeneratedEvalReviewArtifactsForItem returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after seed returned error: %v", err)
	}

	runner := &skillOptTrainItemAwareRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resume continue exit code = %d, stderr=%s", code, stderr.String())
	}
	// Progress total reflects only the incomplete item.
	if !strings.Contains(stderr.String(), "generating 2 options (1 items x 2)") {
		t.Fatalf("resume progress stderr = %s", stderr.String())
	}
	// existingGenerated (2) + generated (2) with no double-count.
	if !strings.Contains(stdout.String(), "current_phase: options_generated") || !strings.Contains(stdout.String(), "generated_options: 4") {
		t.Fatalf("resume continue stdout = %s", stdout.String())
	}
	for _, prompt := range runner.prompts {
		if !strings.Contains(prompt, "Item id: hero-saas") {
			t.Fatalf("resume delivered a prompt for an already-complete item: %q", prompt)
		}
	}
	if len(runner.prompts) != 2 {
		t.Fatalf("resume option deliveries = %d, want 2 (only hero-saas)", len(runner.prompts))
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	// hero-saas now generated, no duplicates.
	heroOptions, err := store.ListEvalReviewOptions(context.Background(), runID, "hero-saas")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions hero-saas returned error: %v", err)
	}
	if len(heroOptions) != 2 || heroOptions[0].Label != "a" || heroOptions[1].Label != "b" {
		t.Fatalf("hero-saas options after resume = %+v", heroOptions)
	}
	// Seeded item untouched: same artifact ids, still exactly two options.
	ecommerceOptions, err := store.ListEvalReviewOptions(context.Background(), runID, "ecommerce-proof")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions ecommerce-proof returned error: %v", err)
	}
	if len(ecommerceOptions) != 2 || ecommerceOptions[0].ArtifactID != seededArtifactIDs[0] || ecommerceOptions[1].ArtifactID != seededArtifactIDs[1] {
		t.Fatalf("seeded item changed after resume: %+v want artifacts %v", ecommerceOptions, seededArtifactIDs)
	}
}

func TestSkillOptTrainContinueUsesManagedGeneratorConcurrency(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "2",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &skillOptConcurrentGenerationRunner{startDelay: 300 * time.Millisecond}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: options_generated") || !strings.Contains(stdout.String(), "generated_options: 4") {
		t.Fatalf("train continue stdout = %s", stdout.String())
	}
	if runner.maxActiveResumes < 2 {
		t.Fatalf("max active resume calls = %d, want at least 2; calls=%+v", runner.maxActiveResumes, runner.calls)
	}
	if runner.startCalls < 2 {
		t.Fatalf("start calls = %d, want at least two managed instances", runner.startCalls)
	}
}

func TestSkillOptTrainContinueUsesRegisteredWorkspaceRepoCheckout(t *testing.T) {
	home := t.TempDir()
	targetDir := t.TempDir()
	runGit(t, targetDir, "init")
	runGit(t, targetDir, "branch", "-m", "main")
	runGit(t, targetDir, "remote", "add", "origin", "https://github.com/owner/product.git")
	workspaceDir := t.TempDir()
	runGit(t, workspaceDir, "init")
	runGit(t, workspaceDir, "branch", "-m", "main")
	runGit(t, workspaceDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(targetDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue without workspace checkout exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "gitmoot repo add owner/workspace --path /path/to/checkout") {
		t.Fatalf("stderr did not explain workspace registration:\n%s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"repo", "add", "owner/workspace", "--home", home, "--path", workspaceDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("repo add exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440401"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nWorkspace hero A.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nWorkspace hero B.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nWorkspace proof A.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nWorkspace proof B.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue with workspace checkout exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: options_generated") {
		t.Fatalf("train continue stdout = %s", stdout.String())
	}
	runner.want(t, 0, workspaceDir, "codex", "exec", "--json", "--")
}

func TestBuildSkillOptTrainGenerationPromptHonorsExplicitLowExploration(t *testing.T) {
	prompt := buildSkillOptTrainGenerationPrompt(
		db.SkillOptTrainSession{
			ID:             "landing-train",
			RequestSummary: "Train landing page outputs.",
		},
		db.SkillOptTrainIteration{ID: "landing-train-001"},
		db.EvalRun{
			ID:               "landing-train-review-001",
			Mode:             db.EvalRunModeExplore,
			ExplorationLevel: db.ExplorationLevelLow,
			OptionsCount:     4,
		},
		db.EvalReviewItem{
			ItemID: "hero-saas",
			Title:  "SaaS hero",
		},
		"a",
		true,
	)
	if strings.Contains(prompt, "Use high exploration") {
		t.Fatalf("low exploration prompt included high exploration rule:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Use low exploration") {
		t.Fatalf("low exploration prompt missing low exploration rule:\n%s", prompt)
	}
}

func TestSkillOptTrainContinueGeneratesValidateArtifacts(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "1",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "validate-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train validation comparisons.",
		"--mode", "validate",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	runner := &agentStartRunner{results: []subprocess.Result{
		{Stdout: `{"type":"thread.started","thread_id":"550e8400-e29b-41d4-a716-446655440301"}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Baseline\n\nConventional hero.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Candidate\n\nImproved hero.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Baseline\n\nConventional proof.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Candidate\n\nImproved proof.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "validate-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: options_generated",
		"generated_options: 4",
		"generator_runtime: codex",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 5 {
		t.Fatalf("runtime calls = %+v, want start plus four deliveries", runner.calls)
	}
	if !strings.Contains(runner.calls[1].args[len(runner.calls[1].args)-1], "A/B artifact role: baseline") {
		t.Fatalf("baseline prompt = %q", runner.calls[1].args[len(runner.calls[1].args)-1])
	}
	if !strings.Contains(runner.calls[2].args[len(runner.calls[2].args)-1], "A/B artifact role: candidate") {
		t.Fatalf("candidate prompt = %q", runner.calls[2].args[len(runner.calls[2].args)-1])
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	items, err := store.ListEvalReviewItems(context.Background(), "validate-train-review-001")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	for _, item := range items {
		if item.BaselineArtifactID == "" || item.CandidateArtifactID == "" {
			t.Fatalf("validate item missing A/B artifacts: %+v", item)
		}
		options, err := store.ListEvalReviewOptions(context.Background(), "validate-train-review-001", item.ItemID)
		if err != nil {
			t.Fatalf("ListEvalReviewOptions %s returned error: %v", item.ItemID, err)
		}
		if len(options) != 0 {
			t.Fatalf("validate item should not have ranked options: %+v", options)
		}
		if _, err := store.GetEvalArtifact(context.Background(), item.BaselineArtifactID); err != nil {
			t.Fatalf("GetEvalArtifact baseline returned error: %v", err)
		}
		if _, err := store.GetEvalArtifact(context.Background(), item.CandidateArtifactID); err != nil {
			t.Fatalf("GetEvalArtifact candidate returned error: %v", err)
		}
	}
}

func TestSkillOptTrainContinueRecordsGenerationFailureWithoutOptions(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}
	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue without generator exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `agent "skillopt-generator" not found`) {
		t.Fatalf("continue failure stderr = %q", stderr.String())
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady || !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) {
		t.Fatalf("iteration after failure = %+v metadata=%s", iteration, iteration.MetadataJSON)
	}
	items, err := store.ListEvalReviewItems(context.Background(), "landing-train-review-001")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	for _, item := range items {
		options, err := store.ListEvalReviewOptions(context.Background(), "landing-train-review-001", item.ItemID)
		if err != nil {
			t.Fatalf("ListEvalReviewOptions %s returned error: %v", item.ItemID, err)
		}
		if len(options) != 0 {
			t.Fatalf("failure persisted options for %s: %+v", item.ItemID, options)
		}
	}
}
