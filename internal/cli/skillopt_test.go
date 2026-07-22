package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func waitPendingInteractivePrompt(t *testing.T, home string, timeout time.Duration) db.InteractivePrompt {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		store, err := db.Open(config.PathsForHome(home).Database)
		if err == nil {
			prompts, listErr := store.ListInteractivePrompts(context.Background(), db.InteractivePromptStatePending)
			store.Close()
			if listErr == nil && len(prompts) == 1 {
				return prompts[0]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pending interactive prompt")
	return db.InteractivePrompt{}
}

func cliTrainInitChoiceByID(t *testing.T, choices []skillopt.TrainInitTemplateChoice, id string) skillopt.TrainInitTemplateChoice {
	t.Helper()
	for _, choice := range choices {
		if choice.ID == id {
			return choice
		}
	}
	t.Fatalf("choice %s not found in %+v", id, choices)
	return skillopt.TrainInitTemplateChoice{}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	result, err := subprocess.ExecRunner{}.Run(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, result.Stderr)
	}
	return result.Stdout
}

// skillOptTrainItemAwareRunner answers generation jobs based on the item id
// embedded in the delivered prompt rather than a fixed call sequence, so item
// success/failure is deterministic regardless of goroutine scheduling. Start
// calls return a thread, resume (delivery) calls return an implemented result
// for any item id in failItems unless it is listed there, in which case they
// return a non-implemented result that fails generation for that item.
type skillOptTrainItemAwareRunner struct {
	mu        sync.Mutex
	failItems map[string]bool
	threadSeq int
	prompts   []string
}

func (r *skillOptTrainItemAwareRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	isResume := false
	for _, arg := range args {
		if arg == "resume" {
			isResume = true
			break
		}
	}
	if !isResume {
		r.threadSeq++
		threadID := fmt.Sprintf("550e8400-e29b-41d4-a716-44665544%04d", 700+r.threadSeq)
		return subprocess.Result{Command: command, Args: args, Stdout: `{"type":"thread.started","thread_id":"` + threadID + `"}` + "\n"}, nil
	}
	prompt := ""
	if len(args) > 0 {
		prompt = args[len(args)-1]
	}
	r.prompts = append(r.prompts, prompt)
	for itemID := range r.failItems {
		if strings.Contains(prompt, "Item id: "+itemID) {
			return subprocess.Result{Command: command, Args: args, Stdout: `{"gitmoot_result":{"decision":"blocked","summary":"cannot generate","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"}, nil
		}
	}
	return subprocess.Result{Command: command, Args: args, Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option\n\nGenerated content.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}` + "\n"}, nil
}

func (r *skillOptTrainItemAwareRunner) LookPath(file string) (string, error) {
	if file == "" {
		return "", errors.New("empty file")
	}
	return "/usr/bin/" + file, nil
}

func startSkillOptTrainGenerationForPersistTest(t *testing.T, home string) {
	t.Helper()
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
}

type skillOptConcurrentGenerationRunner struct {
	mu               sync.Mutex
	calls            []agentStartCall
	startCalls       int
	activeResumes    int
	maxActiveResumes int
	startDelay       time.Duration
}

func (r *skillOptConcurrentGenerationRunner) Run(_ context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, agentStartCall{dir: dir, command: command, args: append([]string{}, args...)})
	isResume := false
	isStart := false
	for _, arg := range args {
		if arg == "resume" {
			isResume = true
		}
		if arg == "--json" {
			isStart = true
		}
	}
	if isStart && !isResume {
		r.startCalls++
		threadID := fmt.Sprintf("550e8400-e29b-41d4-a716-%012d", 446655440500+r.startCalls)
		startDelay := r.startDelay
		r.mu.Unlock()
		if startDelay > 0 {
			time.Sleep(startDelay)
		}
		return subprocess.Result{Command: command, Args: args, Stdout: fmt.Sprintf(`{"type":"thread.started","thread_id":"%s"}`+"\n", threadID)}, nil
	}
	if isResume {
		r.activeResumes++
		if r.activeResumes > r.maxActiveResumes {
			r.maxActiveResumes = r.activeResumes
		}
		r.mu.Unlock()
		time.Sleep(750 * time.Millisecond)
		r.mu.Lock()
		r.activeResumes--
		r.mu.Unlock()
		prompt := ""
		if len(args) > 0 {
			prompt = args[len(args)-1]
		}
		summary := "# Generated option\n\n" + strings.Split(prompt, "\n")[0]
		return subprocess.Result{Command: command, Args: args, Stdout: fmt.Sprintf(`{"gitmoot_result":{"decision":"implemented","summary":%q,"findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`+"\n", summary)}, nil
	}
	r.mu.Unlock()
	return subprocess.Result{Command: command, Args: args}, nil
}

func (r *skillOptConcurrentGenerationRunner) LookPath(file string) (string, error) {
	if file == "" {
		return "", errors.New("empty file")
	}
	return "/usr/bin/" + file, nil
}

func writeSkillOptTrainItemsFile(t *testing.T) string {
	t.Helper()
	itemsPath := filepath.Join(t.TempDir(), "items.yml")
	if err := os.WriteFile(itemsPath, []byte(`items:
  - item_id: hero-saas
    title: SaaS hero
    brief: Design a landing page hero for a workflow SaaS product.
    target_audience: founders
    output_type: vue landing page
    artifact_hints:
      - clickable preview
  - item_id: ecommerce-proof
    title: Ecommerce proof section
    brief: Design a social proof section for an ecommerce analytics product.
    target_audience: growth teams
    output_type: vue landing page
`), 0o644); err != nil {
		t.Fatalf("write items file: %v", err)
	}
	return itemsPath
}

func cliImplementedSummaryResult(t *testing.T, summary string) subprocess.Result {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"gitmoot_result": map[string]any{
			"decision":     "implemented",
			"summary":      summary,
			"findings":     []any{},
			"changes_made": []any{},
			"tests_run":    []any{},
			"needs":        []any{},
			"delegations":  []any{},
		},
	})
	if err != nil {
		t.Fatalf("Marshal implemented summary result returned error: %v", err)
	}
	return subprocess.Result{Stdout: string(encoded) + "\n"}
}

func cliVuePreviewBundleSummary(t *testing.T, marker string) string {
	t.Helper()
	bundle := skillopt.PreviewBundle{
		Renderer:     skillopt.TrainPreviewRendererVueVite,
		BuildCommand: "npm run build",
		DistDir:      "dist",
		Files: []skillopt.PreviewBundleFile{
			{Path: "package.json", Content: `{"scripts":{"build":"vite build"}}`},
			{Path: "index.html", Content: `<div id="app"></div><script type="module" src="/src/main.js"></script>`},
			{Path: "src/main.js", Content: `import { createApp } from 'vue'; import App from './App.vue'; createApp(App).mount('#app');`},
			{Path: "src/App.vue", Content: `<template><main>` + marker + `</main></template>`},
		},
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("Marshal Vue preview bundle returned error: %v", err)
	}
	return string(encoded)
}

func floatPtr(value float64) *float64 {
	return &value
}

type skillOptFakeGitHub struct {
	github.NoopClient

	createdIssue       github.CreateIssueInput
	postedComments     []skillOptPostedGitHubComment
	upsertedFiles      []skillOptUpsertedGitHubFile
	closedIssues       []skillOptClosedGitHubIssue
	listedComments     []skillOptListedGitHubComments
	preflightRepos     []github.Repository
	comments           map[int64][]github.IssueComment
	createIssueErr     error
	postCommentErr     error
	upsertFileErr      error
	closeIssueErr      error
	preflightErr       error
	host               string
	commentKinds       map[int64]string
	commentURLOverride string
}

type skillOptPostedGitHubComment struct {
	Repo        github.Repository
	IssueNumber int64
	Body        string
}

type skillOptUpsertedGitHubFile struct {
	Repo    github.Repository
	Path    string
	Content string
	Message string
}

type skillOptClosedGitHubIssue struct {
	Repo        github.Repository
	IssueNumber int64
}

type skillOptListedGitHubComments struct {
	Repo        github.Repository
	IssueNumber int64
}

func (f *skillOptFakeGitHub) Preflight(_ context.Context, repo github.Repository) error {
	f.preflightRepos = append(f.preflightRepos, repo)
	if f.preflightErr != nil {
		return f.preflightErr
	}
	return nil
}

func seedSkillOptReviewWatcherRun(t *testing.T) (string, *db.Store, artifact.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	template := cliSkillOptTemplate("planner", "Plan landing page improvements.")
	if err := store.UpsertAgentTemplate(context.Background(), template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainSession(context.Background(), db.SkillOptTrainSession{
		ID:                "watcher-train",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/product",
		State:             skillopt.TrainStateReviewPublished,
	}); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(context.Background(), db.SkillOptTrainIteration{
		ID:                    "watcher-train-001",
		SessionID:             "watcher-train",
		EvalRunID:             "watcher-review-001",
		Mode:                  db.EvalRunModeExplore,
		ExplorationLevel:      db.ExplorationLevelHigh,
		State:                 skillopt.TrainStateReviewPublished,
		IssueRepo:             "owner/previews",
		IssueNumber:           67,
		BaseTemplateVersionID: installed.VersionID,
	}); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "watcher-review-001",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/product",
		State:             "review",
		Mode:              db.EvalRunModeExplore,
		ExplorationLevel:  db.ExplorationLevelHigh,
		OptionsCount:      4,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	for _, itemID := range []string{"item-001", "item-002"} {
		if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
			RunID:  "watcher-review-001",
			ItemID: itemID,
			Title:  itemID,
		}); err != nil {
			t.Fatalf("UpsertEvalReviewItem %s returned error: %v", itemID, err)
		}
		for _, label := range []string{"a", "b", "c", "d"} {
			content := []byte(itemID + " option " + label)
			blob, err := blobStore.Put(content)
			if err != nil {
				t.Fatalf("Put %s %s returned error: %v", itemID, label, err)
			}
			artifactID := itemID + "-option-" + label
			if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{ID: artifactID, Hash: blob.Hash, MediaType: "text/markdown", SizeBytes: blob.Size, Driver: "text"}); err != nil {
				t.Fatalf("UpsertEvalArtifact %s returned error: %v", artifactID, err)
			}
			if err := store.UpsertEvalReviewOption(context.Background(), db.EvalReviewOption{RunID: "watcher-review-001", ItemID: itemID, Label: label, ArtifactID: artifactID, Role: "option"}); err != nil {
				t.Fatalf("UpsertEvalReviewOption %s %s returned error: %v", itemID, label, err)
			}
		}
	}
	itemIDsJSON, err := json.Marshal([]string{"item-001", "item-002"})
	if err != nil {
		t.Fatalf("Marshal item ids returned error: %v", err)
	}
	if err := store.UpsertSkillOptReviewWatch(context.Background(), db.SkillOptReviewWatch{
		Repo:                "owner/previews",
		IssueNumber:         67,
		RunID:               "watcher-review-001",
		ExpectedItemIDsJSON: string(itemIDsJSON),
		Status:              db.SkillOptReviewWatchStatusWatching,
	}); err != nil {
		t.Fatalf("UpsertSkillOptReviewWatch returned error: %v", err)
	}
	return home, store, blobStore
}

func setSkillOptReviewWatchStaleAfter(t *testing.T, store *db.Store, staleAfter time.Time) {
	t.Helper()
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	watch.StaleAfter = staleAfter.UTC().Format(time.RFC3339Nano)
	watch.StaleThresholdSeconds = int64((24 * time.Hour).Seconds())
	if err := store.UpsertSkillOptReviewWatch(context.Background(), watch); err != nil {
		t.Fatalf("UpsertSkillOptReviewWatch returned error: %v", err)
	}
}

func (f *skillOptFakeGitHub) CreateIssue(_ context.Context, input github.CreateIssueInput) (github.Issue, error) {
	if f.createIssueErr != nil {
		return github.Issue{}, f.createIssueErr
	}
	f.createdIssue = input
	return github.Issue{Number: 8, URL: f.baseURL() + "/" + input.Repo.FullName() + "/issues/8"}, nil
}

func (f *skillOptFakeGitHub) CloseIssue(_ context.Context, repo github.Repository, issueNumber int64) (github.Issue, error) {
	if f.closeIssueErr != nil {
		return github.Issue{}, f.closeIssueErr
	}
	f.closedIssues = append(f.closedIssues, skillOptClosedGitHubIssue{Repo: repo, IssueNumber: issueNumber})
	return github.Issue{Number: issueNumber, State: "closed", URL: f.baseURL() + "/" + repo.FullName() + "/issues/" + fmt.Sprint(issueNumber)}, nil
}

func (f *skillOptFakeGitHub) PostIssueComment(_ context.Context, repo github.Repository, issueNumber int64, body string) (github.IssueComment, error) {
	if f.postCommentErr != nil {
		return github.IssueComment{}, f.postCommentErr
	}
	f.postedComments = append(f.postedComments, skillOptPostedGitHubComment{Repo: repo, IssueNumber: issueNumber, Body: body})
	kind := "issues"
	if f.commentKinds != nil && f.commentKinds[issueNumber] != "" {
		kind = f.commentKinds[issueNumber]
	}
	url := f.baseURL() + "/" + repo.FullName() + "/" + kind + "/" + fmt.Sprint(issueNumber) + "#issuecomment-" + fmt.Sprint(len(f.postedComments))
	if strings.TrimSpace(f.commentURLOverride) != "" {
		url = strings.TrimSpace(f.commentURLOverride)
	}
	return github.IssueComment{ID: int64(len(f.postedComments)), Body: body, URL: url}, nil
}

func (f *skillOptFakeGitHub) UpsertFile(_ context.Context, input github.UpsertFileInput) (github.RepositoryFile, error) {
	if f.upsertFileErr != nil {
		return github.RepositoryFile{}, f.upsertFileErr
	}
	f.upsertedFiles = append(f.upsertedFiles, skillOptUpsertedGitHubFile{
		Repo:    input.Repo,
		Path:    strings.Trim(strings.TrimSpace(input.Path), "/"),
		Content: string(input.Content),
		Message: input.Message,
	})
	path := strings.Trim(strings.TrimSpace(input.Path), "/")
	return github.RepositoryFile{
		Path: path,
		URL:  f.baseURL() + "/" + input.Repo.FullName() + "/blob/main/" + path,
		SHA:  "fake-sha",
	}, nil
}

func (f *skillOptFakeGitHub) ListIssueComments(_ context.Context, repo github.Repository, issueNumber int64) ([]github.IssueComment, error) {
	f.listedComments = append(f.listedComments, skillOptListedGitHubComments{Repo: repo, IssueNumber: issueNumber})
	return append([]github.IssueComment(nil), f.comments[issueNumber]...), nil
}

func (f *skillOptFakeGitHub) ListRepoIssueComments(_ context.Context, _ github.Repository, _ time.Time) ([]github.IssueComment, error) {
	var out []github.IssueComment
	for number, list := range f.comments {
		for _, c := range list {
			c.IssueNumber = number
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *skillOptFakeGitHub) baseURL() string {
	if strings.TrimSpace(f.host) == "" {
		return "https://github.com"
	}
	return strings.TrimRight(strings.TrimSpace(f.host), "/")
}

func skillOptFakeGitHubUpsertedPath(files []skillOptUpsertedGitHubFile, path string) bool {
	for _, file := range files {
		if file.Path == path {
			return true
		}
	}
	return false
}

func seedSkillOptTrainFeedbackSynced(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := cliSkillOptTemplate("planner", "Plan the work.")
	if err := store.UpsertAgentTemplate(context.Background(), template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	baselineBlob, err := blobStore.Put([]byte("# Baseline\n\nPlan the edit.\n"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidateBlob, err := blobStore.Put([]byte("# Candidate\n\nPlan the edit and verification.\n"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	for _, record := range []db.EvalArtifact{
		{ID: "baseline-artifact", Hash: baselineBlob.Hash, MediaType: "text/markdown", SizeBytes: baselineBlob.Size, Driver: "text"},
		{ID: "candidate-artifact", Hash: candidateBlob.Hash, MediaType: "text/markdown", SizeBytes: candidateBlob.Size, Driver: "text"},
	} {
		if err := store.UpsertEvalArtifact(context.Background(), record); err != nil {
			t.Fatalf("UpsertEvalArtifact returned error: %v", err)
		}
	}
	previewPolicy, err := skillopt.BuildTrainPreviewPolicy("owner/product", "", "", "", "", "")
	if err != nil {
		t.Fatalf("BuildTrainPreviewPolicy returned error: %v", err)
	}
	metadata := skillOptTrainStartMetadata("Train planner outputs from human feedback.", db.EvalRunModeValidate, db.ExplorationLevelLow, 2, "hard_then_soft", nil, nil, previewPolicy, skillOptTrainStartConfigDefaults{}, nil)
	session := db.SkillOptTrainSession{
		ID:                "optimizer-train",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/product",
		RequestSummary:    "Train planner outputs from human feedback.",
		TaskKind:          "custom",
		State:             skillopt.TrainStateFeedbackSynced,
		MetadataJSON:      metadata,
	}
	iteration := db.SkillOptTrainIteration{
		ID:                    "optimizer-train-001",
		SessionID:             session.ID,
		EvalRunID:             "optimizer-train-review-001",
		BaseTemplateVersionID: installed.VersionID,
		Mode:                  db.EvalRunModeValidate,
		ExplorationLevel:      db.ExplorationLevelLow,
		State:                 skillopt.TrainStateFeedbackSynced,
		MetadataJSON:          metadata,
	}
	run := db.EvalRun{
		ID:                iteration.EvalRunID,
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/product",
		State:             "review",
		Mode:              db.EvalRunModeValidate,
		ExplorationLevel:  db.ExplorationLevelLow,
		OptionsCount:      2,
		MetadataJSON:      metadata,
	}
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), run); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               run.ID,
		ItemID:              "item-001",
		Title:               "README plan",
		BaselineArtifactID:  "baseline-artifact",
		CandidateArtifactID: "candidate-artifact",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     run.ID,
		ItemID:    "item-001",
		Choice:    "b",
		Reasoning: "Candidate is more complete.",
		Reviewer:  "github:jerry",
		Source:    "github",
		SourceURL: "https://github.com/owner/product/issues/1#issuecomment-1",
		CreatedAt: "2026-06-02T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	return home, installed.VersionID
}

type skillOptTrainFakeOptimizerRunner struct {
	candidate                skillopt.CandidatePackage
	fail                     bool
	failAfterCandidate       bool
	failAfterCandidateStderr string
	lookPathValue            string
	lookPathErr              error
	versionErr               error
	versionStderr            string
	helpErr                  error
	helpStderr               string
	beforeRun                func(dir string, args []string) error
	preflightCalls           []skillOptTrainFakeOptimizerCall
	calls                    []skillOptTrainFakeOptimizerCall
}

type skillOptTrainFakeOptimizerCall struct {
	dir     string
	command string
	args    []string
}

func (r *skillOptTrainFakeOptimizerRunner) Run(_ context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	result := subprocess.Result{Command: command, Args: args}
	if len(args) == 1 && args[0] == "--version" {
		r.preflightCalls = append(r.preflightCalls, skillOptTrainFakeOptimizerCall{dir: dir, command: command, args: append([]string{}, args...)})
		if r.versionErr != nil {
			result.Stderr = strings.TrimSpace(r.versionStderr)
			if result.Stderr == "" {
				result.Stderr = r.versionErr.Error()
			}
			return result, r.versionErr
		}
		result.Stdout = "gitmoot-skillopt 0.2.0b1\n"
		return result, nil
	}
	if len(args) == 2 && args[0] == "optimize" && args[1] == "--help" {
		r.preflightCalls = append(r.preflightCalls, skillOptTrainFakeOptimizerCall{dir: dir, command: command, args: append([]string{}, args...)})
		if r.helpErr != nil {
			result.Stderr = strings.TrimSpace(r.helpStderr)
			if result.Stderr == "" {
				result.Stderr = r.helpErr.Error()
			}
			return result, r.helpErr
		}
		result.Stdout = "usage: gitmoot-skillopt optimize\n"
		return result, nil
	}
	r.calls = append(r.calls, skillOptTrainFakeOptimizerCall{dir: dir, command: command, args: append([]string{}, args...)})
	if r.beforeRun != nil {
		if err := r.beforeRun(dir, args); err != nil {
			result.Stderr = err.Error()
			return result, err
		}
	}
	if r.fail && !r.failAfterCandidate {
		result.Stderr = "optimizer stderr"
		return result, errors.New("exit status 2")
	}
	candidateOutput := argValue(args, "--candidate-output")
	artifactDir := argValue(args, "--artifact-dir")
	if candidateOutput == "" || artifactDir == "" {
		result.Stderr = "missing output paths"
		return result, errors.New("missing output paths")
	}
	diffContent := []byte("candidate diff\n")
	diffSize := int64(len(diffContent))
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		result.Stderr = err.Error()
		return result, err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), diffContent, 0o644); err != nil {
		result.Stderr = err.Error()
		return result, err
	}
	candidate := r.candidate
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []skillopt.CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      artifact.ContentHash(diffContent),
		MediaType: "text/markdown",
		Driver:    "text",
		SizeBytes: &diffSize,
	}}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		result.Stderr = err.Error()
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(candidateOutput), 0o755); err != nil {
		result.Stderr = err.Error()
		return result, err
	}
	if err := os.WriteFile(candidateOutput, encoded, 0o644); err != nil {
		result.Stderr = err.Error()
		return result, err
	}
	result.Stdout = "wrote candidate package: " + candidateOutput + "\n"
	if r.failAfterCandidate {
		result.Stderr = strings.TrimSpace(r.failAfterCandidateStderr)
		if result.Stderr == "" {
			result.Stderr = "optimizer stderr after candidate"
		}
		return result, errors.New("exit status 2")
	}
	return result, nil
}

func (r *skillOptTrainFakeOptimizerRunner) LookPath(file string) (string, error) {
	if strings.TrimSpace(file) == "" {
		return "", errors.New("empty file")
	}
	if r.lookPathErr != nil {
		return "", r.lookPathErr
	}
	if strings.TrimSpace(r.lookPathValue) != "" {
		return r.lookPathValue, nil
	}
	return "/fake/bin/" + file, nil
}

type skillOptTrainFakePreviewRunner struct {
	failGitCommit bool
	pagesStatus   string
	pagesError    string
	pagesStatuses []string
	pagesCommits  []string
	calls         []skillOptTrainFakePreviewCall
}

type skillOptTrainFakePreviewCall struct {
	dir     string
	command string
	args    []string
}

func (r *skillOptTrainFakePreviewRunner) Run(ctx context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	r.calls = append(r.calls, skillOptTrainFakePreviewCall{dir: dir, command: command, args: append([]string{}, args...)})
	result := subprocess.Result{Command: command, Args: args}
	if command == "npm" {
		if len(args) == 2 && args[0] == "install" && args[1] == "--ignore-scripts" {
			result.Stdout = "installed\n"
			return result, nil
		}
		if len(args) == 2 && args[0] == "run" && args[1] == "build" {
			distDir := filepath.Join(dir, "dist")
			if err := os.MkdirAll(distDir, 0o755); err != nil {
				return result, err
			}
			if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte("<div id=\"app\">preview</div>\n"), 0o644); err != nil {
				return result, err
			}
			result.Stdout = "built\n"
			return result, nil
		}
		return result, fmt.Errorf("unexpected npm args: %v", args)
	}
	if command == "git" && len(args) == 1 && args[0] == "push" {
		result.Stdout = "pushed\n"
		return result, nil
	}
	if command == "git" && len(args) > 0 && args[0] == "commit" && r.failGitCommit {
		result.Stderr = "commit failed\n"
		return result, errors.New("exit status 1")
	}
	if command == "gh" && len(args) == 2 && args[0] == "api" && strings.HasSuffix(args[1], "/pages/builds/latest") {
		status := strings.TrimSpace(r.pagesStatus)
		if len(r.pagesStatuses) > 0 {
			status = strings.TrimSpace(r.pagesStatuses[0])
			if len(r.pagesStatuses) > 1 {
				r.pagesStatuses = r.pagesStatuses[1:]
			}
		}
		if status == "" {
			status = "built"
		}
		commitSHA := ""
		if len(r.pagesCommits) > 0 {
			commitSHA = strings.TrimSpace(r.pagesCommits[0])
			if len(r.pagesCommits) > 1 {
				r.pagesCommits = r.pagesCommits[1:]
			}
		}
		if commitSHA == "" {
			commitSHA = strings.TrimSpace(runGitOutputFromRunner(ctx, dir, "rev-parse", "HEAD"))
		}
		payload := map[string]any{
			"status":     status,
			"commit_sha": commitSHA,
		}
		if strings.TrimSpace(r.pagesError) != "" {
			payload["error"] = map[string]any{"message": strings.TrimSpace(r.pagesError)}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return result, err
		}
		result.Stdout = string(encoded)
		return result, nil
	}
	return subprocess.ExecRunner{}.Run(ctx, dir, command, args...)
}

func runGitOutputFromRunner(ctx context.Context, dir string, args ...string) string {
	result, err := subprocess.ExecRunner{}.Run(ctx, dir, "git", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

func (r *skillOptTrainFakePreviewRunner) LookPath(file string) (string, error) {
	if strings.TrimSpace(file) == "" {
		return "", errors.New("empty file")
	}
	return "/fake/bin/" + file, nil
}

func argValue(args []string, name string) string {
	for index := 0; index < len(args)-1; index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cliSkillOptTemplate(id string, body string) db.AgentTemplate {
	content := cliSkillOptTemplateContent(id, body)
	parsed, err := agenttemplate.ParseTemplateContent(content)
	if err != nil {
		panic(err)
	}
	metadataJSON, err := agenttemplate.MarshalMetadata(parsed.Metadata)
	if err != nil {
		panic(err)
	}
	return db.AgentTemplate{
		ID:             id,
		Name:           parsed.Metadata.Name,
		Description:    parsed.Metadata.Description,
		SourceRepo:     agenttemplate.LocalSourceRepo,
		SourceRef:      agenttemplate.LocalSourceRef,
		SourcePath:     id + ".md",
		ResolvedCommit: agenttemplate.HashContent(content),
		Content:        content,
		MetadataJSON:   metadataJSON,
	}
}

func cliSkillOptCandidatePackage(t *testing.T, templateID string, baseVersionID string, body string) skillopt.CandidatePackage {
	t.Helper()
	candidateContent := cliSkillOptTemplateContent(templateID, body)
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	return skillopt.CandidatePackage{
		Kind:            skillopt.CandidatePackageKind,
		ContractVersion: skillopt.ContractVersion,
		TemplateID:      templateID,
		BaseVersionID:   baseVersionID,
		Candidate: skillopt.CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
		EvalReport: json.RawMessage(`{"score":0.82}`),
		Summary: skillopt.CandidateSummary{
			PreferenceSummary: "Candidate is more actionable.",
		},
	}
}

func cliSkillOptTemplateContent(id string, body string) string {
	return agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
		ID:                   id,
		Name:                 "Planner",
		Description:          "Plans implementation work.",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask"},
		RuntimeCompatibility: []string{"codex"},
		Tags:                 []string{"planning"},
		Inputs:               []string{"task"},
		Outputs:              []string{"plan"},
	}, "# Planner\n\n"+body+"\n")
}
