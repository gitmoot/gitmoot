package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func TestSkillOptFeedbackRejectsIncompleteCommands(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantErr      string
		wantStdout   string
		wantExitCode int
		wantNoStderr bool
		wantNoStdout bool
	}{
		{
			name:         "feedback help",
			args:         []string{"skillopt", "feedback", "--help"},
			wantStdout:   "gitmoot skillopt feedback github publish",
			wantExitCode: 0,
			wantNoStderr: true,
		},
		{
			name:         "unknown collector",
			args:         []string{"skillopt", "feedback", "json"},
			wantErr:      `unknown skillopt feedback collector "json"`,
			wantExitCode: 2,
			wantNoStdout: true,
		},
		{
			name:         "missing markdown subcommand",
			args:         []string{"skillopt", "feedback", "markdown"},
			wantErr:      "skillopt feedback markdown requires a subcommand",
			wantExitCode: 2,
			wantNoStdout: true,
		},
		{
			name:         "missing github subcommand",
			args:         []string{"skillopt", "feedback", "github"},
			wantErr:      "skillopt feedback github requires a subcommand",
			wantExitCode: 2,
			wantNoStdout: true,
		},
		{
			name:         "missing github sync target",
			args:         []string{"skillopt", "feedback", "github", "sync", "--run", "run-1"},
			wantErr:      "skillopt feedback github sync requires --issue or --pr",
			wantExitCode: 2,
			wantNoStdout: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tt.args, &stdout, &stderr)
			if code != tt.wantExitCode {
				t.Fatalf("exit code = %d, want %d; stdout=%s stderr=%s", code, tt.wantExitCode, stdout.String(), stderr.String())
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Fatalf("stdout = %q, want substring %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}
			if tt.wantNoStdout && stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if tt.wantNoStderr && stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestSkillOptFeedbackGitHubCommands(t *testing.T) {
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
	baselineBlob, err := blobStore.Put([]byte("baseline"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidateBlob, err := blobStore.Put([]byte("candidate"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{ID: "baseline", Hash: baselineBlob.Hash, MediaType: "text/markdown", SizeBytes: baselineBlob.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{ID: "candidate", Hash: candidateBlob.Hash, MediaType: "text/markdown", SizeBytes: candidateBlob.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{ID: "run-1", TargetRepo: "owner/repo", State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               "run-1",
		ItemID:              "item-001",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			8: {
				{ID: 100, Body: "run_id: run-1\nitem-001: b - More concrete.", URL: "https://github.com/owner/repo/issues/8#issuecomment-100", Author: "alice", CreatedAt: "2026-05-31T10:00:00Z"},
			},
		},
	}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fake }
	t.Cleanup(func() {
		newSkillOptGitHubClient = oldClient
	})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "feedback", "github", "publish", "--home", home, "--run", "run-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github publish exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "published github feedback issue for run-1 to owner/repo#8") {
		t.Fatalf("publish stdout = %q", stdout.String())
	}
	if fake.createdIssue.Repo.FullName() != "owner/repo" || !strings.Contains(fake.createdIssue.Body, "Copy-Paste YAML Reply") {
		t.Fatalf("created issue = %+v", fake.createdIssue)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "github", "sync", "--home", home, "--run", "run-1", "--issue", "8"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github sync exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 1 github feedback events") {
		t.Fatalf("sync stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after sync returned error: %v", err)
	}
	defer store.Close()
	events, err := store.ListFeedbackEvents(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Reviewer != "alice" || events[0].Source != "github" {
		t.Fatalf("events = %+v", events)
	}
}

func TestSkillOptFeedbackGitHubCommandsEnforceTrainReviewRepo(t *testing.T) {
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
	previewPolicy, err := skillopt.BuildTrainPreviewPolicy("owner/product", "owner/previews", "", "", "", "")
	if err != nil {
		t.Fatalf("BuildTrainPreviewPolicy returned error: %v", err)
	}
	metadata := skillOptTrainStartMetadata("Train landing page reviews.", db.EvalRunModeExplore, db.ExplorationLevelHigh, 4, "soft", nil, nil, previewPolicy, skillOptTrainStartConfigDefaults{}, nil)
	session := db.SkillOptTrainSession{
		ID:           "preview-train",
		TemplateID:   "planner",
		TargetRepo:   "owner/product",
		PreviewRepo:  "owner/previews",
		TaskKind:     "design",
		State:        skillopt.TrainStateItemsReady,
		MetadataJSON: metadata,
	}
	iteration := db.SkillOptTrainIteration{
		ID:               "preview-train-001",
		SessionID:        session.ID,
		EvalRunID:        "preview-train-review-001",
		Mode:             db.EvalRunModeExplore,
		ExplorationLevel: db.ExplorationLevelHigh,
		State:            skillopt.TrainStateItemsReady,
		MetadataJSON:     metadata,
	}
	run := db.EvalRun{
		ID:               iteration.EvalRunID,
		TemplateID:       "planner",
		TargetRepo:       "owner/product",
		State:            "review",
		Mode:             db.EvalRunModeExplore,
		ExplorationLevel: db.ExplorationLevelHigh,
		OptionsCount:     4,
		MetadataJSON:     metadata,
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
		RunID:  run.ID,
		ItemID: "item-001",
		Title:  "Landing page",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		content := []byte("option " + label)
		blob, err := blobStore.Put(content)
		if err != nil {
			t.Fatalf("Put option %s returned error: %v", label, err)
		}
		artifactID := "train-option-" + label
		if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{ID: artifactID, Hash: blob.Hash, MediaType: "text/markdown", SizeBytes: blob.Size, Driver: "text"}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", label, err)
		}
		if err := store.UpsertEvalReviewOption(context.Background(), db.EvalReviewOption{RunID: run.ID, ItemID: "item-001", Label: label, ArtifactID: artifactID, Role: "option"}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			8: {
				{ID: 100, Body: "run_id: preview-train-review-001\nitem-001 ranking: C > A > D > B\n", URL: "https://github.com/owner/previews/issues/8#issuecomment-100", Author: "alice", CreatedAt: "2026-05-31T10:00:00Z"},
			},
		},
	}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fake }
	t.Cleanup(func() {
		newSkillOptGitHubClient = oldClient
	})

	var stdout, stderr bytes.Buffer
	fake.preflightErr = errors.New("gh auth missing")
	code := Run([]string{"skillopt", "feedback", "github", "publish", "--home", home, "--run", "preview-train-review-001"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("github publish preflight failure exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "gh auth missing") {
		t.Fatalf("github publish preflight failure stderr = %q", stderr.String())
	}
	if fake.createdIssue.Repo.FullName() != "" {
		t.Fatalf("preflight failure created issue = %+v", fake.createdIssue)
	}
	if len(fake.preflightRepos) != 1 || fake.preflightRepos[0].FullName() != "owner/previews" {
		t.Fatalf("preflight repos = %+v, want owner/previews", fake.preflightRepos)
	}
	fake.preflightErr = nil
	fake.preflightRepos = nil
	stdout.Reset()
	stderr.Reset()

	code = Run([]string{"skillopt", "feedback", "github", "publish", "--home", home, "--run", "preview-train-review-001"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github publish default repo exit code = %d, stderr=%s", code, stderr.String())
	}
	if fake.createdIssue.Repo.FullName() != "owner/previews" {
		t.Fatalf("created issue repo = %s, want owner/previews", fake.createdIssue.Repo.FullName())
	}
	if len(fake.preflightRepos) != 1 || fake.preflightRepos[0].FullName() != "owner/previews" {
		t.Fatalf("preflight repos after publish = %+v, want owner/previews", fake.preflightRepos)
	}

	stdout.Reset()
	stderr.Reset()
	fake.preflightRepos = nil
	code = Run([]string{"skillopt", "feedback", "github", "publish", "--home", home, "--run", "preview-train-review-001", "--repo", "Owner/Previews", "--pr", "9"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github publish matching repo exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(fake.postedComments) != 1 || !strings.EqualFold(fake.postedComments[0].Repo.FullName(), "owner/previews") || fake.postedComments[0].IssueNumber != 9 {
		t.Fatalf("posted comments = %+v, want owner/previews#9", fake.postedComments)
	}
	if len(fake.preflightRepos) != 1 || !strings.EqualFold(fake.preflightRepos[0].FullName(), "owner/previews") {
		t.Fatalf("preflight repos after pr publish = %+v, want owner/previews", fake.preflightRepos)
	}

	stdout.Reset()
	stderr.Reset()
	fake.createdIssue = github.CreateIssueInput{}
	code = Run([]string{"skillopt", "feedback", "github", "publish", "--home", home, "--run", "preview-train-review-001", "--repo", "owner/product"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("github publish wrong repo exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "expects github feedback repo owner/previews; got owner/product") {
		t.Fatalf("github publish wrong repo stderr = %q", stderr.String())
	}
	if fake.createdIssue.Repo.FullName() != "" {
		t.Fatalf("wrong repo publish created issue = %+v", fake.createdIssue)
	}

	stdout.Reset()
	stderr.Reset()
	fake.preflightRepos = nil
	code = Run([]string{"skillopt", "feedback", "github", "sync", "--home", home, "--run", "preview-train-review-001", "--issue", "8"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github sync default repo exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(fake.listedComments) != 1 || fake.listedComments[0].Repo.FullName() != "owner/previews" {
		t.Fatalf("listed comments = %+v, want owner/previews", fake.listedComments)
	}
	if len(fake.preflightRepos) != 1 || fake.preflightRepos[0].FullName() != "owner/previews" {
		t.Fatalf("preflight repos after sync = %+v, want owner/previews", fake.preflightRepos)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "github", "sync", "--home", home, "--run", "preview-train-review-001", "--repo", "owner/product", "--issue", "8"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("github sync wrong repo exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "expects github feedback repo owner/previews; got owner/product") {
		t.Fatalf("github sync wrong repo stderr = %q", stderr.String())
	}
}

func TestSkillOptFeedbackRepoResolutionPreservesNonTrainExpectedRepoFallback(t *testing.T) {
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
	run := db.EvalRun{
		ID:           "standalone-run",
		TargetRepo:   "owner/product",
		MetadataJSON: `{"review":{"expected_repo":"owner/previews"}}`,
	}
	repo, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, "")
	if err != nil {
		t.Fatalf("resolveSkillOptFeedbackRepo returned error: %v", err)
	}
	if repo.FullName() != "owner/previews" {
		t.Fatalf("resolved repo = %s, want owner/previews", repo.FullName())
	}
	explicit, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, "owner/explicit")
	if err != nil {
		t.Fatalf("resolveSkillOptFeedbackRepo explicit returned error: %v", err)
	}
	if explicit.FullName() != "owner/explicit" {
		t.Fatalf("explicit repo = %s, want owner/explicit", explicit.FullName())
	}
}
