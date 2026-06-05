package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestSkillOptExportAndImportCommands(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
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
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "baseline",
		Hash:      baselineBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: baselineBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "candidate",
		Hash:      candidateBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: candidateBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "run-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "ready",
		MetadataJSON:      `{"driver":"planner"}`,
	}); err != nil {
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

	exportPath := filepath.Join(t.TempDir(), "training.json")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "export", "--home", home, "--run", "run-1", "--output", exportPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt export exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "exported run-1") {
		t.Fatalf("export stdout = %q", stdout.String())
	}
	exportedContent, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var training skillopt.TrainingPackage
	if err := json.Unmarshal(exportedContent, &training); err != nil {
		t.Fatalf("decode training package: %v\n%s", err, string(exportedContent))
	}
	if training.Template.VersionID != installed.VersionID || len(training.Items) != 1 || len(training.Artifacts) != 2 {
		t.Fatalf("training package = %+v", training)
	}
	packetDir := filepath.Join(t.TempDir(), "packet")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "export", "--home", home, "--run", "run-1", "--output", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown export exit code = %d, stderr=%s", code, stderr.String())
	}
	feedbackYAML := `run_id: run-1
reviewer: jerry
items:
  - item_id: item-001
    choice: a
    reasoning: Clearer.
`
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackYAML), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "import", "--home", home, "--packet", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 1 feedback events") {
		t.Fatalf("feedback import stdout = %q", stdout.String())
	}

	candidateContent := cliSkillOptTemplateContent("planner", "Plan the work and include risks.")
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	candidate := skillopt.CandidatePackage{
		Kind:            skillopt.CandidatePackageKind,
		ContractVersion: skillopt.ContractVersion,
		TemplateID:      "planner",
		BaseVersionID:   installed.VersionID,
		Candidate: skillopt.CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
		EvalReport: json.RawMessage(`{"score":0.91}`),
		Summary: skillopt.CandidateSummary{
			Score:             floatPtr(0.91),
			PreferenceSummary: "Candidate is more specific.",
		},
	}
	candidatePath := filepath.Join(t.TempDir(), "candidate.json")
	encodedCandidate, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	if err := os.WriteFile(candidatePath, encodedCandidate, 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "import", "--home", home, "--file", candidatePath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported pending candidate planner@v2") {
		t.Fatalf("import stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "list", "--home", home, "--template", "planner"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "planner@v2") || !strings.Contains(stdout.String(), "Candidate is more specific.") {
		t.Fatalf("candidate list stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "show", "--home", home, "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: pending") || !strings.Contains(stdout.String(), "eval_report:") || !strings.Contains(stdout.String(), "content_diff:") {
		t.Fatalf("candidate show stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "promote", "--home", home, "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate promote exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "promoted candidate planner@v2") {
		t.Fatalf("candidate promote stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after import returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate current returned error: %v", err)
	}
	if current.VersionID != "planner@v2" {
		t.Fatalf("current version = %q, want planner@v2", current.VersionID)
	}
	latest, err := store.GetAgentTemplateReference(context.Background(), "planner@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest returned error: %v", err)
	}
	if latest.VersionID != "planner@v2" || latest.Content != candidateContent {
		t.Fatalf("latest = %+v", latest)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after promote returned error: %v", err)
	}
	rejectedContent := cliSkillOptTemplateContent("planner", "Plan the work and include every possible detail.")
	rejectedParsed, err := agenttemplate.ParseTemplateContent(rejectedContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent rejected returned error: %v", err)
	}
	candidate.Candidate.Content = rejectedContent
	candidate.Candidate.Metadata = rejectedParsed.Metadata
	encodedCandidate, err = json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal rejected candidate: %v", err)
	}
	if err := os.WriteFile(candidatePath, encodedCandidate, 0o644); err != nil {
		t.Fatalf("write rejected candidate: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "import", "--home", home, "--file", candidatePath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt import rejected exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported pending candidate planner@v3") {
		t.Fatalf("second import stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "reject", "--home", home, "planner@v3", "--reason", "too verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate reject exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "rejected candidate planner@v3") {
		t.Fatalf("candidate reject stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after reject returned error: %v", err)
	}
	defer store.Close()
	rejected, err := store.GetAgentTemplateVersionByID(context.Background(), "planner@v3")
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID rejected returned error: %v", err)
	}
	if rejected.State != "rejected" {
		t.Fatalf("rejected = %+v", rejected)
	}
	rejectedReview, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v3")
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview rejected returned error: %v", err)
	}
	if rejectedReview.DecisionReason != "too verbose" {
		t.Fatalf("rejected review = %+v", rejectedReview)
	}
	latest, err = store.GetAgentTemplateReference(context.Background(), "planner@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest after reject returned error: %v", err)
	}
	if latest.VersionID != "planner@v2" {
		t.Fatalf("latest after reject = %+v", latest)
	}
	events, err := store.ListFeedbackEvents(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Choice != "a" {
		t.Fatalf("feedback events = %+v", events)
	}
}

func TestSkillOptExportIncludesRankedFields(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		content := []byte("option " + label)
		if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
			ID:        "option-" + label,
			Hash:      artifact.ContentHash(content),
			MediaType: "text/markdown",
			SizeBytes: int64(len(content)),
			Driver:    "text",
		}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", label, err)
		}
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "ranked-export-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		Mode:              db.EvalRunModeExplore,
		ExplorationLevel:  db.ExplorationLevelHigh,
		OptionsCount:      4,
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:  "ranked-export-1",
		ItemID: "item-001",
		Title:  "Landing page",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		if err := store.UpsertEvalReviewOption(context.Background(), db.EvalReviewOption{
			RunID:      "ranked-export-1",
			ItemID:     "item-001",
			Label:      label,
			ArtifactID: "option-" + label,
			Role:       "option",
		}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	ranking, err := json.Marshal([]string{"c", "a", "d", "b"})
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(context.Background(), db.RankedFeedbackEvent{
		RunID:       "ranked-export-1",
		ItemID:      "item-001",
		RankingJSON: string(ranking),
		Winner:      "c",
		Reviewer:    "jerry",
		Source:      "github",
		CreatedAt:   "2026-06-02T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	outputPath := filepath.Join(t.TempDir(), "training.json")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "export", "--home", home, "--run", "ranked-export-1", "--output", outputPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt export exit code = %d, stderr=%s", code, stderr.String())
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read training package: %v", err)
	}
	var training skillopt.TrainingPackage
	if err := json.Unmarshal(content, &training); err != nil {
		t.Fatalf("decode training package: %v\n%s", err, string(content))
	}
	if training.EvalRun.Mode != db.EvalRunModeExplore || len(training.Items) != 1 || len(training.Items[0].Options) != 4 {
		t.Fatalf("ranked training package run/items = %+v %+v", training.EvalRun, training.Items)
	}
	if len(training.RankedFeedbackEvents) != 1 || len(training.PairwisePreferences) != 6 {
		t.Fatalf("ranked training feedback = %+v pairwise=%+v", training.RankedFeedbackEvents, training.PairwisePreferences)
	}
	if training.RankedFeedbackEvents[0].ID == "" || training.PairwisePreferences[0].RankedEventID != training.RankedFeedbackEvents[0].ID {
		t.Fatalf("ranked feedback provenance = %+v pairwise=%+v", training.RankedFeedbackEvents, training.PairwisePreferences)
	}
}

func TestSkillOptTrainStartStatusAndStop(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	requestPath := filepath.Join(t.TempDir(), "request.txt")
	if err := os.WriteFile(requestPath, []byte("Train landing page plans with diverse review items."), 0o644); err != nil {
		t.Fatalf("write request file: %v", err)
	}
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
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--preview-repo", "owner/previews",
		"--request-file", requestPath,
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "4",
		"--items-file", itemsPath,
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train start dry-run exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry_run: true") || !strings.Contains(stdout.String(), "template_version: "+installed.VersionID) {
		t.Fatalf("dry-run stdout = %q", stdout.String())
	}
	for _, want := range []string{
		"preview_mode: required",
		"preview_renderer: vue-vite",
		"preview_publisher: github-pages",
		"preview_route_template: runs/{run_id}/{item_id}/{option_label}/",
		"expected_review_repo: owner/previews",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("dry-run stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after dry-run returned error: %v", err)
	}
	if _, err := store.GetSkillOptTrainSession(context.Background(), "landing-train"); err == nil {
		t.Fatalf("dry-run created train session")
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSkillOptTrainSession dry-run returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after dry-run returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--request", "Train landing page plans.",
		"--options", "1",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("train start with one option exit code = %d, want 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--options must be zero or at least 2") {
		t.Fatalf("one-option stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--request", "Train landing page plans.",
		"--items-file", itemsPath,
		"--preview-mode", "required",
		"--preview-renderer", "none",
		"--yes",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("train start with invalid preview policy exit code = %d, want 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "preview renderer is required") {
		t.Fatalf("invalid preview policy stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--request", "Train landing page plans.",
		"--items-file", itemsPath,
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("train start without yes exit code = %d, want 2; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "confirmation_required: true") || !strings.Contains(stdout.String(), "--yes") {
		t.Fatalf("confirmation stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--preview-repo", "owner/previews",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "4",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created train session landing-train") || !strings.Contains(stdout.String(), "preferred_gate: soft") || !strings.Contains(stdout.String(), "items: 2") || !strings.Contains(stdout.String(), "warning: preview repo must be public or GitHub Pages-enabled") {
		t.Fatalf("train start stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "expected_review_repo: owner/previews") {
		t.Fatalf("train start stdout missing expected review repo: %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after start returned error: %v", err)
	}
	session, err := store.GetSkillOptTrainSession(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	if session.TemplateVersionID != installed.VersionID || session.WorkspaceRepo != "owner/workspace" || session.PreviewRepo != "owner/previews" || session.TaskKind != "design" {
		t.Fatalf("session = %+v", session)
	}
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	if policy.Mode != skillopt.TrainPreviewModeRequired || policy.Renderer != skillopt.TrainPreviewRendererVueVite || policy.Publisher != skillopt.TrainPreviewPublisherGitHubPages || policy.ExpectedReviewRepo != "owner/previews" {
		t.Fatalf("preview policy = %+v metadata=%s", policy, session.MetadataJSON)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.BaseTemplateVersionID != installed.VersionID || iteration.Mode != db.EvalRunModeExplore || iteration.ExplorationLevel != db.ExplorationLevelHigh || iteration.EvalRunID != "landing-train-review-001" || iteration.State != skillopt.TrainStateItemsReady {
		t.Fatalf("iteration = %+v", iteration)
	}
	run, err := store.GetEvalRun(context.Background(), "landing-train-review-001")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.TemplateVersionID != installed.VersionID || run.TargetRepo != "owner/product" || run.OptionsCount != 4 || skillOptMetadataString(run.MetadataJSON, "evaluation", "preferred_gate") != "soft" {
		t.Fatalf("eval run = %+v metadata=%s", run, run.MetadataJSON)
	}
	if !strings.Contains(run.MetadataJSON, "preview repo must be public or GitHub Pages-enabled") {
		t.Fatalf("eval run metadata did not include preview warning: %s", run.MetadataJSON)
	}
	if skillOptMetadataString(run.MetadataJSON, "review", "expected_repo") != "owner/previews" {
		t.Fatalf("eval run metadata missing expected review repo: %s", run.MetadataJSON)
	}
	feedbackRepo, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, "")
	if err != nil {
		t.Fatalf("resolveSkillOptFeedbackRepo returned error: %v", err)
	}
	if feedbackRepo.FullName() != "owner/previews" {
		t.Fatalf("feedback repo = %s, want owner/previews", feedbackRepo.FullName())
	}
	items, err := store.ListEvalReviewItems(context.Background(), "landing-train-review-001")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 2 || items[0].ItemID != "ecommerce-proof" || !strings.Contains(items[0].MetadataJSON, "growth teams") {
		t.Fatalf("items = %+v", items)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after start returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--request", "Overwrite existing train session.",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("duplicate train start exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("duplicate train start stderr = %q", stderr.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after duplicate start returned error: %v", err)
	}
	session, err = store.GetSkillOptTrainSession(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession after duplicate returned error: %v", err)
	}
	if session.RequestSummary != "Train landing page plans." {
		t.Fatalf("duplicate start changed session = %+v", session)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after duplicate start returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "status",
		"--home", home,
		"--session", "landing-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"preview_mode: required",
		"preview_renderer: vue-vite",
		"preview_publisher: github-pages",
		"preview_repo: owner/previews",
		"expected_review_repo: owner/previews",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train status stdout missing %q:\n%s", want, stdout.String())
		}
	}

	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize before eval-run collision returned error: %v", err)
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open before eval-run collision returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "eval-collision-review-001",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/other",
		State:             "review",
	}); err != nil {
		t.Fatalf("UpsertEvalRun collision returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close before eval-run collision returned error: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "eval-collision",
		"--request", "Train landing page plans.",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("eval-run collision train start exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "eval run eval-collision-review-001 already exists") {
		t.Fatalf("eval-run collision stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status_phase: items_ready") || !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "blocked_step: options_generated") || !strings.Contains(stdout.String(), "review_items: 2") {
		t.Fatalf("status stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "landing-train", "--json", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status json exit code = %d, stderr=%s", code, stderr.String())
	}
	var statusJSON skillOptTrainStatusSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &statusJSON); err != nil {
		t.Fatalf("train status json did not decode: %v\n%s", err, stdout.String())
	}
	if statusJSON.SessionID != "landing-train" ||
		statusJSON.CurrentPhase != skillopt.TrainStateItemsReady ||
		statusJSON.StatusPhase != skillopt.TrainStateItemsReady ||
		statusJSON.CurrentStep != skillopt.TrainStateOptionsGenerated ||
		statusJSON.PreviewPolicy.ExpectedReviewRepo != "owner/previews" ||
		statusJSON.Counts.ReviewItems != 2 ||
		statusJSON.Progress.ETA != "unknown" ||
		statusJSON.Verbose == nil ||
		statusJSON.Verbose.EvalRunID != "landing-train-review-001" ||
		len(statusJSON.Verbose.Items) != 2 {
		t.Fatalf("train status json = %+v", statusJSON)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "landing-train", "--watch", "--verbose", "--poll", "1ms"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status watch exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"status_phase: items_ready",
		"current_phase: items_ready",
		"current_step: options_generated",
		"elapsed:",
		"eta: unknown",
		"watch_state: waiting",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train status watch stdout missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "landing-train", "--watch", "--json", "--poll", "1ms"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("train status watch json exit code = %d, want 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not support --watch with --json") {
		t.Fatalf("train status watch json stderr = %q", stderr.String())
	}

	store = openCLIJobStore(t, home)
	lockExpiresAt := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	if acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: skillOptTrainGenerationLockKey("landing-train", "landing-train-001"),
		OwnerJobID:  "test-generation",
		OwnerToken:  "token-1",
		ExpiresAt:   lockExpiresAt,
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	lockedStatus, err := loadSkillOptTrainStatusSnapshot(context.Background(), store, "landing-train", true)
	if err != nil {
		t.Fatalf("loadSkillOptTrainStatusSnapshot returned error: %v", err)
	}
	if lockedStatus.Verbose == nil || len(lockedStatus.Verbose.ActiveLocks) != 1 || lockedStatus.Verbose.ActiveLocks[0].Name != "generation" {
		t.Fatalf("locked status active locks = %+v", lockedStatus.Verbose)
	}
	if skillOptTrainWatchDone(lockedStatus) {
		t.Fatalf("watch marked locked status done: %+v", lockedStatus.Verbose.ActiveLocks)
	}
	if released, err := store.ReleaseResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001"), "test-generation", "token-1"); err != nil || !released {
		t.Fatalf("ReleaseResourceLock returned released=%v err=%v", released, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after active lock test returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "stop", "--home", home, "--session", "landing-train", "--reason", "trial complete"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train stop exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped train session landing-train") || !strings.Contains(stdout.String(), "reason: trial complete") {
		t.Fatalf("stop stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue after stop exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "abandoned") {
		t.Fatalf("continue stderr = %q", stderr.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open for terminal stop returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainSession(context.Background(), db.SkillOptTrainSession{
		ID:         "promoted-train",
		TemplateID: "planner",
		State:      skillopt.TrainStateCandidatePromoted,
	}); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession terminal returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(context.Background(), db.SkillOptTrainIteration{
		ID:        "promoted-train-001",
		SessionID: "promoted-train",
		State:     skillopt.TrainStateCandidatePromoted,
	}); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration terminal returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close for terminal stop returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "stop", "--home", home, "--session", "promoted-train", "--reason", "do not overwrite"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train stop after terminal exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "terminal") {
		t.Fatalf("terminal stop stderr = %q", stderr.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after terminal stop returned error: %v", err)
	}
	terminalIteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "promoted-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration terminal returned error: %v", err)
	}
	if terminalIteration.State != skillopt.TrainStateCandidatePromoted {
		t.Fatalf("terminal stop changed iteration = %+v", terminalIteration)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after terminal stop returned error: %v", err)
	}
}

func TestSkillOptTrainStatusGeneratedProgressUsesCurrentIteration(t *testing.T) {
	session := db.SkillOptTrainSession{
		ID:           "landing-train",
		State:        skillopt.TrainStateItemsReady,
		MetadataJSON: `{"generation":{"status":"succeeded","generated_options":8}}`,
	}
	iteration := db.SkillOptTrainIteration{
		ID:        "landing-train-002",
		State:     skillopt.TrainStateItemsReady,
		EvalRunID: "landing-train-review-002",
	}
	counts := skillopt.TrainStatusCounts{ReviewItems: 2}
	summary := skillopt.BuildTrainStatusSummary(session, &iteration, counts)
	snapshot := buildSkillOptTrainStatusSnapshot(session, &iteration, summary, counts)
	if snapshot.Progress.GeneratedOptions != 0 {
		t.Fatalf("generated options = %d, want 0 for current iteration without generation metadata", snapshot.Progress.GeneratedOptions)
	}

	iteration.MetadataJSON = `{"generation":{"status":"succeeded","generated_options":0}}`
	summary = skillopt.BuildTrainStatusSummary(session, &iteration, counts)
	snapshot = buildSkillOptTrainStatusSnapshot(session, &iteration, summary, counts)
	if snapshot.Progress.GeneratedOptions != 0 {
		t.Fatalf("generated options = %d, want explicit current iteration zero", snapshot.Progress.GeneratedOptions)
	}

	summary = skillopt.BuildTrainStatusSummary(session, nil, skillopt.TrainStatusCounts{})
	snapshot = buildSkillOptTrainStatusSnapshot(session, nil, summary, skillopt.TrainStatusCounts{})
	if snapshot.Progress.GeneratedOptions != 8 {
		t.Fatalf("generated options = %d, want session fallback when no iteration exists", snapshot.Progress.GeneratedOptions)
	}
}

func TestSkillOptTrainStatusVerboseUsesCurrentIterationMetadata(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	session := db.SkillOptTrainSession{
		ID:           "landing-train",
		State:        skillopt.TrainStateItemsReady,
		MetadataJSON: `{"generation":{"status":"running"}}`,
	}
	iteration := db.SkillOptTrainIteration{
		ID:    "landing-train-002",
		State: skillopt.TrainStateItemsReady,
	}
	details, err := buildSkillOptTrainStatusVerbose(context.Background(), store, session, &iteration)
	if err != nil {
		t.Fatalf("buildSkillOptTrainStatusVerbose returned error: %v", err)
	}
	if len(details.MetadataStatus) != 0 {
		t.Fatalf("metadata status = %+v, want current iteration metadata without stale session generation", details.MetadataStatus)
	}

	iteration.MetadataJSON = `{"review":{"status":"publishing"}}`
	details, err = buildSkillOptTrainStatusVerbose(context.Background(), store, session, &iteration)
	if err != nil {
		t.Fatalf("buildSkillOptTrainStatusVerbose with iteration metadata returned error: %v", err)
	}
	if details.MetadataStatus["review"] != "publishing" || details.MetadataStatus["generation"] != "" {
		t.Fatalf("metadata status = %+v, want current iteration review only", details.MetadataStatus)
	}

	details, err = buildSkillOptTrainStatusVerbose(context.Background(), store, session, nil)
	if err != nil {
		t.Fatalf("buildSkillOptTrainStatusVerbose without iteration returned error: %v", err)
	}
	if details.MetadataStatus["generation"] != "running" {
		t.Fatalf("metadata status = %+v, want session fallback without iteration", details.MetadataStatus)
	}
}

func TestSkillOptTrainStatusItemsReportsABLabels(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:           "landing-train-review-001",
		Mode:         db.EvalRunModeValidate,
		OptionsCount: 2,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:               "landing-train-review-001",
		ItemID:              "item-001",
		Title:               "Landing page",
		BaselineArtifactID:  "artifact-baseline",
		CandidateArtifactID: "artifact-candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	items, err := skillOptTrainStatusItems(ctx, store, "landing-train-review-001")
	if err != nil {
		t.Fatalf("skillOptTrainStatusItems returned error: %v", err)
	}
	if len(items) != 1 || strings.Join(items[0].OptionLabels, ",") != "BASELINE,CANDIDATE" {
		t.Fatalf("items = %+v, want A/B option labels", items)
	}
}

func TestSkillOptTrainWatchDoneIgnoresStaleMetadataWithoutActiveLock(t *testing.T) {
	snapshot := skillOptTrainStatusSnapshot{
		CurrentPhase: skillopt.TrainStateOptionsGenerated,
		Verbose: &skillOptTrainStatusVerbose{
			MetadataStatus: map[string]string{"review": "publishing"},
		},
	}
	if !skillOptTrainWatchDone(snapshot) {
		t.Fatalf("watch treated stale metadata as active: %+v", snapshot.Verbose.MetadataStatus)
	}

	snapshot.Verbose.ActiveLocks = []skillOptTrainStatusLock{{Name: "review", Key: "skillopt-train-review:landing-train:landing-train-001"}}
	if skillOptTrainWatchDone(snapshot) {
		t.Fatalf("watch ignored active lock: %+v", snapshot.Verbose.ActiveLocks)
	}
}

func TestGeneratedSkillOptTrainSessionIDIncludesSubsecondEntropy(t *testing.T) {
	id := generatedSkillOptTrainSessionID("planner")
	if strings.HasSuffix(id, "-000000000") {
		t.Fatalf("generated session id used literal nanosecond suffix: %q", id)
	}
	second := generatedSkillOptTrainSessionID("planner")
	if id == second {
		t.Fatalf("generated session ids collided: %q", id)
	}
}

func TestSkillOptGitHubPagesURLHandlesProjectAndUserPages(t *testing.T) {
	project := githubPagesURL(db.Repo{Owner: "owner", Name: "previews"}, "runs/run-1/item/a/")
	if project != "https://owner.github.io/previews/runs/run-1/item/a/" {
		t.Fatalf("project pages URL = %q", project)
	}
	user := githubPagesURL(db.Repo{Owner: "owner", Name: "owner.github.io"}, "runs/run-1/item/a/")
	if user != "https://owner.github.io/runs/run-1/item/a/" {
		t.Fatalf("user pages URL = %q", user)
	}
}

func TestSkillOptPreviewRouteSlugsUnsafeSegments(t *testing.T) {
	route, err := skillOptPreviewRoute("", "run 1", "hero#main", "A/B?")
	if err != nil {
		t.Fatalf("skillOptPreviewRoute returned error: %v", err)
	}
	want := "runs/run-1-" + shortHash("run 1") + "/hero-main-" + shortHash("hero#main") + "/a-b-" + shortHash("A/B?") + "/"
	if route != want {
		t.Fatalf("route = %q, want %q", route, want)
	}
}

func TestTrustedVueViteScaffoldUsesRelativeBase(t *testing.T) {
	workDir := t.TempDir()
	if err := writeTrustedVueViteScaffold(workDir); err != nil {
		t.Fatalf("writeTrustedVueViteScaffold returned error: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(workDir, "vite.config.js"))
	if err != nil {
		t.Fatalf("read vite config: %v", err)
	}
	if !strings.Contains(string(config), "base: './'") {
		t.Fatalf("vite config missing relative base:\n%s", string(config))
	}
}

func TestPublishGitHubPagesPreviewRestoresCheckoutOnCommitFailure(t *testing.T) {
	previewDir := t.TempDir()
	runGit(t, previewDir, "init")
	runGit(t, previewDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, previewDir, "config", "user.name", "Gitmoot")
	runGit(t, previewDir, "branch", "-m", "main")
	if err := os.WriteFile(filepath.Join(previewDir, "README.md"), []byte("previews\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, previewDir, "add", "README.md")
	runGit(t, previewDir, "commit", "-m", "init")
	distDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte("<main>preview</main>\n"), 0o644); err != nil {
		t.Fatalf("write dist index: %v", err)
	}
	previewRunner := &skillOptTrainFakePreviewRunner{failGitCommit: true}
	oldPreviewRunner := skillOptTrainPreviewRunner
	skillOptTrainPreviewRunner = previewRunner
	defer func() {
		skillOptTrainPreviewRunner = oldPreviewRunner
	}()

	_, err := publishGitHubPagesPreview(context.Background(), db.Repo{Owner: "owner", Name: "previews", CheckoutPath: previewDir}, "runs/run-1/item/a/", distDir)
	if err == nil || !strings.Contains(err.Error(), "git commit") {
		t.Fatalf("publishGitHubPagesPreview error = %v, want git commit", err)
	}
	status := runGitOutput(t, previewDir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Fatalf("preview repo left dirty after commit failure:\n%s", status)
	}
	if _, err := os.Stat(filepath.Join(previewDir, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview route was not cleaned, stat err=%v", err)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	result, err := subprocess.ExecRunner{}.Run(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, result.Stderr)
	}
	return result.Stdout
}

func TestSkillOptTrainContinueGeneratesOptionsWithManagedAgent(t *testing.T) {
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
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nHero with strong product narrative.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nDashboard-led layout with proof metrics.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nCheckout analytics proof block.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nLifecycle commerce story with motion notes.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: options_generated",
		"continue_ready: true",
		"generated_options: 4",
		"jobs: 4",
		"generator_agent: skillopt-generator-bg-",
		"generator_runtime: codex",
		"next: publish the human review packet",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 5 {
		t.Fatalf("runtime calls = %+v, want start plus four deliveries", runner.calls)
	}
	runner.want(t, 0, repoDir, "codex", "exec", "--json", "--")
	runner.want(t, 1, repoDir, "codex", "exec", "resume", "550e8400-e29b-41d4-a716-446655440201", "--")
	if !strings.Contains(runner.calls[1].args[len(runner.calls[1].args)-1], "Option label: A") || !strings.Contains(runner.calls[1].args[len(runner.calls[1].args)-1], "Generate one review option") {
		t.Fatalf("generation prompt = %q", runner.calls[1].args[len(runner.calls[1].args)-1])
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
		if artifactRecord.MediaType != "text/markdown" || artifactRecord.Driver != "text" || !strings.Contains(options[0].MetadataJSON, `"job_id"`) {
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
	if fakeGitHub.createdIssue.Repo.FullName() != "owner/product" || !strings.Contains(fakeGitHub.createdIssue.Body, "## Inline Options Without Public Links") {
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
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option B hero")),
		cliImplementedSummaryResult(t, cliVuePreviewBundleSummary(t, "Option A proof")),
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
		!strings.Contains(fakeGitHub.createdIssue.Body, "Option A: [open]("+wantPreviewURL+")") ||
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
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optional-preview-train"}, &stdout, &stderr)
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
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optional-preview-train"}, &stdout, &stderr)
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
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "preview-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "preview bundle") || !strings.Contains(stderr.String(), "decode preview bundle JSON") {
		t.Fatalf("preview bundle failure stderr = %q", stderr.String())
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
		{Stdout: `{"gitmoot_result":{"decision":"changes_requested","summary":"This is a review finding, not generated content.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"changes_requested","summary":"This is still a review finding, not generated content.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "returned changes_requested, want implemented") {
		t.Fatalf("stderr = %q", stderr.String())
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

func TestSkillOptTrainContinueRefusesConcurrentGeneration(t *testing.T) {
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
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: skillOptTrainGenerationLockKey("landing-train", "landing-train-001"),
		OwnerJobID:  "test-concurrent-continue",
		OwnerToken:  "test-token",
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not acquire generation lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close before continue returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "skillopt train generation is already running") {
		t.Fatalf("train continue stderr = %q", stderr.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady {
		t.Fatalf("iteration state = %s, want %s", iteration.State, skillopt.TrainStateItemsReady)
	}
	if strings.Contains(iteration.MetadataJSON, `"status":"failed"`) {
		t.Fatalf("busy generation lock recorded failed metadata: %s", iteration.MetadataJSON)
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("busy generation lock dispatched jobs: %+v", jobs)
	}
}

func TestSkillOptTrainGenerationLockTTLScalesWithWorkload(t *testing.T) {
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
	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "2",
		"--job-timeout", "45m",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:               "landing-train-review-001",
		Mode:             db.EvalRunModeExplore,
		ExplorationLevel: db.ExplorationLevelHigh,
		OptionsCount:     4,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	for index := 0; index < 7; index++ {
		itemID := fmt.Sprintf("item-%03d", index+1)
		if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
			RunID:  "landing-train-review-001",
			ItemID: itemID,
			Title:  itemID,
		}); err != nil {
			t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
		}
	}
	ttl, err := estimateSkillOptTrainGenerationLockTTL(context.Background(), store, skillOptTrainContinueRequest{Home: home, GeneratorType: "skillopt-generator"}, db.SkillOptTrainIteration{
		EvalRunID: "landing-train-review-001",
	})
	if err != nil {
		t.Fatalf("estimateSkillOptTrainGenerationLockTTL returned error: %v", err)
	}
	want := 16*45*time.Minute + skillOptTrainGenerationLockBuffer
	if ttl != want {
		t.Fatalf("ttl = %s, want %s", ttl, want)
	}
	if ttl <= skillOptTrainGenerationLockTTL {
		t.Fatalf("ttl = %s, want greater than fixed minimum %s", ttl, skillOptTrainGenerationLockTTL)
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
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
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
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nWorkspace hero A.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nWorkspace hero B.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option A\n\nWorkspace proof A.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Option B\n\nWorkspace proof B.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
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
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Baseline\n\nConventional hero.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Candidate\n\nImproved hero.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Baseline\n\nConventional proof.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
		{Stdout: `{"gitmoot_result":{"decision":"implemented","summary":"# Candidate\n\nImproved proof.","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}` + "\n"},
	}}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "validate-train"}, &stdout, &stderr)
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
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train"}, &stdout, &stderr)
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

func TestSkillOptTrainContinueRunsOptimizerAndImportsCandidate(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with stronger candidate guidance.")
	candidate.BaseVersionID = ""
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: candidate,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--skillopt-bin", "gitmoot-skillopt",
		"--model", "gpt-5.5",
		"--optimizer-backend", "openai_chat",
		"--target-backend", "codex_exec",
		"--evaluator-id", "landing_page_v1",
		"--evaluator-model", "gpt-evaluator",
		"--evaluator-backend", "azure_openai",
		"--skill-update-mode", "full_rewrite_minibatch",
		"--num-epochs", "2",
		"--batch-size", "3",
		"--gate", "mixed",
		"--out-root", outRoot,
		"--timeout", "5m",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: candidate_created",
		"candidate: planner@v2",
		"continue_ready: true",
		"training_package: " + filepath.Join(outRoot, "training.json"),
		"candidate_package: " + filepath.Join(outRoot, "candidate.json"),
		"optimizer_dry_run: true",
		"imported_candidate: planner@v2",
		"next: publish candidate diff and preview review",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("optimizer calls = %+v, want one", runner.calls)
	}
	call := runner.calls[0]
	for _, want := range []string{
		"optimize",
		"--training-package", filepath.Join(outRoot, "training.json"),
		"--artifact-root", config.PathsForHome(home).ArtifactBlobs,
		"--out-root", outRoot,
		"--candidate-output", filepath.Join(outRoot, "candidate.json"),
		"--artifact-dir", filepath.Join(outRoot, "artifacts"),
		"--gate-metric", "mixed",
		"--optimizer-model", "gpt-5.5",
		"--target-model", "gpt-5.5",
		"--optimizer-backend", "openai_chat",
		"--target-backend", "codex_exec",
		"--evaluator-id", "landing_page_v1",
		"--evaluator-model", "gpt-evaluator",
		"--evaluator-backend", "azure_openai",
		"--skill-update-mode", "full_rewrite_minibatch",
		"--num-epochs", "2",
		"--batch-size", "3",
		"--dry-run",
	} {
		if !containsString(call.args, want) {
			t.Fatalf("optimizer args missing %q: %+v", want, call.args)
		}
	}
	trainingPackage, err := os.ReadFile(filepath.Join(outRoot, "training.json"))
	if err != nil {
		t.Fatalf("read training package: %v", err)
	}
	if !strings.Contains(string(trainingPackage), `"kind": "gitmoot-skillopt-training-package"`) {
		t.Fatalf("training package = %s", string(trainingPackage))
	}

	store := openCLIJobStore(t, home)
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateCandidateCreated || iteration.CandidateVersionID != "planner@v2" {
		t.Fatalf("iteration = %+v", iteration)
	}
	if !strings.Contains(iteration.MetadataJSON, `"candidate_version":"planner@v2"`) || !strings.Contains(iteration.MetadataJSON, `--gate-metric`) {
		t.Fatalf("iteration metadata = %s", iteration.MetadataJSON)
	}
	version, err := store.GetAgentTemplateVersionByID(context.Background(), "planner@v2")
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if version.State != "pending" {
		t.Fatalf("candidate version = %+v", version)
	}
}

func TestSkillOptTrainContinueBackendCodexResolvesPresetAndReportsPreflight(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with codex backend preset guidance.")
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: candidate,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--backend", "codex",
		"--target-backend", "codex",
		"--evaluator-id", "landing_page_v1",
		"--out-root", outRoot,
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue codex backend exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"current_phase: candidate_created",
		"backend: codex",
		"optimizer_backend: codex",
		"target_backend: codex",
		"internal_target_adapter: codex_exec",
		"evaluator_backend: codex",
		"backend_config_status: codex_no_azure_or_openai_required",
		"optimizer_lock: acquired",
		"recovery_available: true",
		"optimizer_dry_run: true",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue codex backend stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("optimizer calls = %+v, want one", runner.calls)
	}
	call := runner.calls[0]
	for _, want := range []string{
		"--optimizer-backend", "codex",
		"--target-backend", "codex_exec",
		"--evaluator-backend", "codex",
		"--evaluator-id", "landing_page_v1",
		"--dry-run",
	} {
		if !containsString(call.args, want) {
			t.Fatalf("optimizer args missing %q: %+v", want, call.args)
		}
	}
}

func TestSkillOptTrainContinueBackendCodexRejectsConflictingAdvancedBackend(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--backend", "codex",
		"--optimizer-backend", "openai_chat",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue conflict exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--backend codex conflicts with --optimizer-backend") {
		t.Fatalf("conflict stderr = %q", stderr.String())
	}
	for _, unwanted := range []string{
		"training_package:",
		"optimizer_backend:",
		"optimizer_lock:",
	} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Fatalf("conflict stdout included optimizer report field %q:\n%s", unwanted, stdout.String())
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("optimizer started despite backend conflict: %+v", runner.calls)
	}
}

func TestResolveSkillOptTrainBackendRequestReportsDefaultOpenAIBackends(t *testing.T) {
	_, resolution, err := resolveSkillOptTrainBackendRequest(skillOptTrainOptimizerRequest{
		TargetBackend: "codex_exec",
	})
	if err != nil {
		t.Fatalf("resolveSkillOptTrainBackendRequest returned error: %v", err)
	}
	if resolution.Backend != "custom" ||
		resolution.OptimizerBackend != "openai_chat" ||
		resolution.TargetBackend != "codex" ||
		resolution.InternalTargetAdapter != "codex_exec" ||
		resolution.EvaluatorBackend != "openai_chat" ||
		resolution.ConfigStatus != "external_credentials_may_be_required" {
		t.Fatalf("resolution = %+v", resolution)
	}
}

func TestSkillOptTrainContinueRecordsNoCandidateResult(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan the work.")
	candidate.EvalReport = json.RawMessage(`{
		"promotable": false,
		"no_candidate_reason": "gate_rejected_best_origin_initial_skill"
	}`)
	candidate.Summary.Metadata = json.RawMessage(`{
		"promotable": false,
		"no_candidate_reason": "gate_rejected_best_origin_initial_skill",
		"next_action": "Do not import or publish a candidate review; collect more feedback, rerun with gate-reject retry if budget remains, or inspect the candidate package."
	}`)
	baselineGateScore := 0.89
	candidateGateScore := 0.84
	candidate.Summary.GateRejection = &skillopt.GateRejectionPacket{
		RejectionType:  "candidate_score_regression",
		PrimaryReason:  "candidate_quality_regressed",
		AttemptedPatch: "artifact delivery only",
		RetryAttempts:  "1/1",
		Baseline: skillopt.GateRejectionScores{
			GateScore: &baselineGateScore,
		},
		Candidate: skillopt.GateRejectionScores{
			GateScore: &candidateGateScore,
		},
	}
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: candidate,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--out-root", outRoot,
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue no-candidate exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"current_phase: optimizer_completed_no_candidate",
		"candidate: -",
		"continue_ready: true",
		"candidate_package: " + filepath.Join(outRoot, "candidate.json"),
		"optimizer_dry_run: true",
		"no_candidate_reason: gate_rejected_best_origin_initial_skill",
		"next: Do not import or publish a candidate review; collect more feedback",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue no-candidate stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store := openCLIJobStore(t, home)
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateOptimizerCompletedNoCandidate || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after no-candidate = %+v", iteration)
	}
	if !strings.Contains(iteration.MetadataJSON, `"status":"no_candidate"`) ||
		!strings.Contains(iteration.MetadataJSON, `"no_candidate_reason":"gate_rejected_best_origin_initial_skill"`) ||
		!strings.Contains(iteration.MetadataJSON, `"attempted_patch":"artifact delivery only"`) {
		t.Fatalf("iteration metadata after no-candidate = %s", iteration.MetadataJSON)
	}
	if _, err := store.GetAgentTemplateVersionByID(context.Background(), "planner@v2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no-candidate imported planner@v2 err=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close no-candidate store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--json", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status no-candidate exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var statusJSON skillOptTrainStatusSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &statusJSON); err != nil {
		t.Fatalf("train status no-candidate json did not decode: %v\n%s", err, stdout.String())
	}
	if statusJSON.StatusPhase != "optimizer_completed_no_candidate" ||
		statusJSON.NoCandidateReason != "gate_rejected_best_origin_initial_skill" ||
		statusJSON.NoCandidateDetails["attempted_patch"] != "artifact delivery only" {
		t.Fatalf("train status no-candidate json = %+v", statusJSON)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status no-candidate text exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"no_candidate_reason: gate_rejected_best_origin_initial_skill",
		"attempted_patch: artifact delivery only",
		"retry_attempts: 1/1",
		"rejection: baseline_gate=0.89 candidate_gate=0.84",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train status no-candidate text missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue start-next after no-candidate exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") ||
		!strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") ||
		!strings.Contains(stdout.String(), "base_version: "+baseVersionID) {
		t.Fatalf("start-next after no-candidate stdout = %s", stdout.String())
	}
}

func TestSkillOptTrainContinueRerunsOptimizerAfterNoCandidate(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan the work."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--out-root", outRoot,
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue no-candidate exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("optimizer calls after no-candidate = %+v, want one", runner.calls)
	}

	runner.candidate = cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with rerun candidate guidance.")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--rerun-optimizer",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue rerun after no-candidate exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 2 {
		t.Fatalf("optimizer calls after rerun = %+v, want two", runner.calls)
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_created") ||
		!strings.Contains(stdout.String(), "imported_candidate: planner@v2") {
		t.Fatalf("rerun after no-candidate stdout = %s", stdout.String())
	}
}

func TestSkillOptTrainContinueMarksReviewPublishedFeedbackSynced(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: feedback_synced",
		"blocked_step: training_package_created",
		"feedback: 1",
		"continue_ready: true",
		"feedback_events: 1",
		"pairwise_preferences: 0",
		"next: export the training package before running the optimizer",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue stdout missing %q:\n%s", want, stdout.String())
		}
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	session, err = store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession after continue returned error: %v", err)
	}
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after continue returned error: %v", err)
	}
	if session.State != skillopt.TrainStateFeedbackSynced || iteration.State != skillopt.TrainStateFeedbackSynced {
		t.Fatalf("states = session %s iteration %s, want feedback_synced", session.State, iteration.State)
	}
	if !strings.Contains(iteration.MetadataJSON, `"feedback_sync"`) {
		t.Fatalf("iteration metadata missing feedback_sync: %s", iteration.MetadataJSON)
	}
}

func TestSkillOptTrainContinueResolvesRelativeOptimizerBinary(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate:     cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with relative binary-safe guidance."),
		lookPathValue: filepath.Join(".", "bin", "gitmoot-skillopt"),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--skillopt-bin", filepath.Join(".", "bin", "gitmoot-skillopt"),
		"--out-root", outRoot,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue relative optimizer exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("optimizer calls = %+v, want one", runner.calls)
	}
	wantCommand := filepath.Join(repoDir, "bin", "gitmoot-skillopt")
	if runner.calls[0].command != wantCommand {
		t.Fatalf("optimizer command = %q, want absolute %q", runner.calls[0].command, wantCommand)
	}
	if runner.calls[0].dir != outRoot {
		t.Fatalf("optimizer dir = %q, want %q", runner.calls[0].dir, outRoot)
	}
}

func TestSkillOptTrainContinueRunsLocalFakeOptimizerExecutable(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with executable smoke guidance.")
	candidateJSON, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent candidate returned error: %v", err)
	}
	fakeBin := filepath.Join(t.TempDir(), "gitmoot-skillopt")
	fakeScript := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1:-}" != "optimize" ]; then
  echo "expected optimize subcommand" >&2
  exit 64
fi
shift
candidate_output=""
artifact_dir=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --training-package)
      shift
      test -f "$1"
      ;;
    --candidate-output)
      shift
      candidate_output="$1"
      ;;
    --artifact-dir)
      shift
      artifact_dir="$1"
      ;;
    --out-root|--artifact-root|--gate-metric|--optimizer-model|--target-model)
      shift
      ;;
    --dry-run)
      ;;
  esac
  shift
done
if [ "$candidate_output" = "" ]; then
  echo "missing --candidate-output" >&2
  exit 65
fi
if [ "$artifact_dir" = "" ]; then
  echo "missing --artifact-dir" >&2
  exit 66
fi
mkdir -p "$artifact_dir"
mkdir -p "$(dirname "$candidate_output")"
cat > "$candidate_output" <<'GITMOOT_CANDIDATE_JSON'
%s
GITMOOT_CANDIDATE_JSON
echo "fake optimizer ok"
`, string(candidateJSON))
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatalf("WriteFile fake optimizer returned error: %v", err)
	}

	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = subprocess.ExecRunner{}
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--skillopt-bin", fakeBin,
		"--out-root", outRoot,
		"--gate", "soft",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue fake executable exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_created") || !strings.Contains(stdout.String(), "imported_candidate: planner@v2") {
		t.Fatalf("fake executable stdout = %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(outRoot, "training.json")); err != nil {
		t.Fatalf("training package was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outRoot, "candidate.json")); err != nil {
		t.Fatalf("candidate package was not written: %v", err)
	}
}

func TestSkillOptTrainContinuePublishesCandidateReviewPromotesAndStartsNext(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	session.WorkspaceRepo = "owner/workspace"
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession workspace returned error: %v", err)
	}
	run, err := store.GetEvalRun(context.Background(), "optimizer-train-review-001")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	run.OptionsCount = 4
	if err := store.UpsertEvalRun(context.Background(), run); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	optionBlobStore := artifact.NewStore(config.PathsForHome(home).ArtifactBlobs)
	for _, label := range []string{"a", "b", "c", "d"} {
		content := []byte("option " + label)
		blob, err := optionBlobStore.Put(content)
		if err != nil {
			t.Fatalf("Put option %s returned error: %v", label, err)
		}
		if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
			ID:        "option-" + label,
			Hash:      blob.Hash,
			MediaType: "text/markdown",
			SizeBytes: blob.Size,
			Driver:    "text",
		}); err != nil {
			t.Fatalf("UpsertEvalArtifact option %s returned error: %v", label, err)
		}
		if err := store.UpsertEvalReviewOption(context.Background(), db.EvalReviewOption{
			RunID:      run.ID,
			ItemID:     "item-001",
			Label:      label,
			ArtifactID: "option-" + label,
			Role:       "option",
		}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	ranking, err := json.Marshal([]string{"b", "a", "c", "d"})
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(context.Background(), db.RankedFeedbackEvent{
		RunID:        run.ID,
		ItemID:       "item-001",
		RankingJSON:  string(ranking),
		Winner:       "b",
		ContinueMode: db.EvalRunModeExplore,
		Reviewer:     "github:jerry",
		Source:       "github",
		SourceURL:    "https://github.com/owner/product/issues/1#issuecomment-ranked",
		CreatedAt:    "2026-06-02T10:01:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after options count update returned error: %v", err)
	}
	candidatePackage := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with candidate review guidance.")
	selectionScore := 0.73
	candidatePackage.Summary.Score = &selectionScore
	candidatePackage.Summary.Metadata = json.RawMessage(`{"best_origin":"candidate","total_accepts":2,"promotable":true,"no_candidate_reason":null}`)
	candidatePackage.EvalReport = json.RawMessage(`{"score":0.86,"hard":0.91,"soft":0.84,"best_selection_hard":0.77,"best_selection_soft":0.78,"baseline_selection_hard":0.55,"baseline_selection_soft":0.56,"test_hard":0.72,"test_soft":0.74,"baseline_test_hard":0.5,"baseline_test_soft":0.52,"dimension_scores":{"hero_quality":0.8},"gate_status":"passed","promotable":true,"no_candidate_reason":"stale_non_blocking_reason"}`)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: candidatePackage,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--promote", "planner@v2",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("premature promote exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "candidate decisions require train iteration at candidate_review_published") {
		t.Fatalf("premature promote stderr = %q", stderr.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("premature promote published github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}

	fakeGitHub.createIssueErr = errors.New("github unavailable")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue failed candidate review exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "github unavailable") {
		t.Fatalf("failed candidate review stderr = %q", stderr.String())
	}
	store = openCLIJobStore(t, home)
	session, err = store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession after failed publish returned error: %v", err)
	}
	failedIteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after failed publish returned error: %v", err)
	}
	if session.State != skillopt.TrainStateCandidateCreated || failedIteration.State != skillopt.TrainStateCandidateCreated {
		t.Fatalf("state after failed candidate review publish: session=%s iteration=%s", session.State, failedIteration.State)
	}
	if failedIteration.IssueNumber != 0 || strings.TrimSpace(failedIteration.IssueURL) != "" || strings.Contains(failedIteration.MetadataJSON, `"status":"published"`) {
		t.Fatalf("iteration recorded failed candidate review publish: %+v", failedIteration)
	}
	markerPath := skillOptCandidateReviewRecoveryPath(config.PathsForHome(home), session, failedIteration)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("failed candidate review recovery marker err=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after failed candidate review publish returned error: %v", err)
	}

	fakeGitHub.createIssueErr = nil
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue after ambiguous publish error exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "external post started") || !strings.Contains(stderr.String(), "SkillOpt candidate review: optimizer-train") {
		t.Fatalf("ambiguous publish retry stderr = %q", stderr.String())
	}
	if err := removeSkillOptCandidateReviewRecovery(config.PathsForHome(home), session, failedIteration); err != nil {
		t.Fatalf("remove ambiguous publish recovery marker returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || !strings.Contains(stdout.String(), "candidate_review: https://github.com/owner/workspace/issues/8") {
		t.Fatalf("candidate review stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "owner/workspace" {
		t.Fatalf("created issue repo = %+v", fakeGitHub.createdIssue.Repo)
	}
	for _, want := range []string{
		"SkillOpt Candidate Review",
		"### Artifacts",
		"`candidate-diff`",
		"### GitHub Files",
		"Best skill",
		"best_skill.md",
		"Base skill",
		"base_skill.md",
		"Candidate diff",
		"candidate.diff.md",
		"### Scores And Gate",
		"Selection score: `0.73`",
		"Best selection hard: `0.77`",
		"Best selection soft: `0.78`",
		"Baseline selection hard: `0.55`",
		"Baseline selection soft: `0.56`",
		"Test score: `0.86`",
		"Hard score: `0.91`",
		"Soft score: `0.84`",
		"Test hard: `0.72`",
		"Test soft: `0.74`",
		"Baseline test hard: `0.5`",
		"Baseline test soft: `0.52`",
		"Dimension scores: `hero_quality=0.8`",
		"Gate status: `passed`",
		"No-op status: `not detected; best_origin=candidate; total_accepts=2`",
		"Promotability: `promotable`",
		"planner@v2",
		skillOptTrainCandidateDecisionCommand(true, "optimizer-train", "--promote", "planner@v2", false),
		skillOptTrainCandidateDecisionCommand(true, "optimizer-train", "--reject", "planner@v2", true),
		"Wait: take no action",
		"Keep improving: reject with an actionable reason",
		skillOptTrainStartNextCommand(true, "optimizer-train"),
	} {
		if !strings.Contains(fakeGitHub.createdIssue.Body, want) {
			t.Fatalf("candidate review body missing %q:\n%s", want, fakeGitHub.createdIssue.Body)
		}
	}
	if len(fakeGitHub.upsertedFiles) != 3 {
		t.Fatalf("published candidate review files = %+v, want 3", fakeGitHub.upsertedFiles)
	}
	for _, want := range []string{
		"skillopt/runs/optimizer-train/optimizer-train-001/planner@v2/best_skill.md",
		"skillopt/runs/optimizer-train/optimizer-train-001/planner@v2/base_skill.md",
		"skillopt/runs/optimizer-train/optimizer-train-001/planner@v2/candidate.diff.md",
	} {
		if !skillOptFakeGitHubUpsertedPath(fakeGitHub.upsertedFiles, want) {
			t.Fatalf("candidate review did not publish %s; files=%+v", want, fakeGitHub.upsertedFiles)
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--promote", "planner@v2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_promoted") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") {
		t.Fatalf("promote stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--promote", "planner@v2",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue promote retry/start-next exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") || !strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") || !strings.Contains(stdout.String(), "base_version: planner@v2") {
		t.Fatalf("promote retry/start-next stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue duplicate start-next exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--start-next requires a promoted candidate, rejected candidate, or no-candidate optimizer result; current phase is items_ready") {
		t.Fatalf("duplicate start-next stderr = %q", stderr.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if current.VersionID != "planner@v2" {
		t.Fatalf("current template version = %q, want planner@v2", current.VersionID)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != "optimizer-train-002" || latest.BaseTemplateVersionID != "planner@v2" || latest.State != skillopt.TrainStateItemsReady || latest.Mode != db.EvalRunModeExplore {
		t.Fatalf("latest iteration = %+v", latest)
	}
	if strings.Contains(latest.MetadataJSON, `"optimizer"`) {
		t.Fatalf("next iteration inherited optimizer metadata: %s", latest.MetadataJSON)
	}
	gate, err := skillOptTrainOptimizerGate(latest, skillOptTrainOptimizerRequest{})
	if err != nil {
		t.Fatalf("skillOptTrainOptimizerGate returned error: %v", err)
	}
	if gate != "mixed" {
		t.Fatalf("next iteration optimizer gate = %q, want mixed", gate)
	}
	items, err := store.ListEvalReviewItems(context.Background(), latest.EvalRunID)
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 1 || items[0].BaselineArtifactID != "" || items[0].CandidateArtifactID != "" {
		t.Fatalf("next iteration items = %+v", items)
	}
	nextRun, err := store.GetEvalRun(context.Background(), latest.EvalRunID)
	if err != nil {
		t.Fatalf("GetEvalRun next returned error: %v", err)
	}
	if nextRun.OptionsCount != 4 {
		t.Fatalf("next run options count = %d, want 4", nextRun.OptionsCount)
	}
	if nextRun.Mode != db.EvalRunModeExplore {
		t.Fatalf("next run mode = %q, want explore", nextRun.Mode)
	}
	if strings.Contains(nextRun.MetadataJSON, `"optimizer"`) {
		t.Fatalf("next run inherited optimizer metadata: %s", nextRun.MetadataJSON)
	}
}

func TestSkillOptTrainContinueSyncsHumanCandidatePromotionAndStartsNext(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with externally promoted candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || fakeGitHub.createdIssue.Repo.FullName() != "owner/product" {
		t.Fatalf("candidate review stdout=%s github=%+v", stdout.String(), fakeGitHub.createdIssue)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "candidate", "promote",
		"--home", home,
		"planner@v2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("candidate promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	supersedeStore := openCLIJobStore(t, home)
	newerCandidate, err := supersedeStore.AddPendingAgentTemplateVersion(context.Background(), cliSkillOptTemplate("planner", "Plan with later promoted guidance."))
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion newer candidate returned error: %v", err)
	}
	if _, err := supersedeStore.PromoteAgentTemplateVersion(context.Background(), newerCandidate.ID); err != nil {
		t.Fatalf("PromoteAgentTemplateVersion newer candidate returned error: %v", err)
	}
	superseded, err := supersedeStore.GetAgentTemplateVersionByID(context.Background(), "planner@v2")
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID superseded train candidate returned error: %v", err)
	}
	if superseded.State != "superseded" {
		t.Fatalf("train candidate state after later promotion = %s, want superseded", superseded.State)
	}
	if err := supersedeStore.Close(); err != nil {
		t.Fatalf("Close supersede store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--promote", "planner@v2",
		"--reject", "planner@v2",
		"--reason", "conflicting",
		"--start-next",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue conflicting decision exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "accepts only one of --promote or --reject") {
		t.Fatalf("conflicting decision stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue sync/start-next exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") || !strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") {
		t.Fatalf("sync/start-next stdout = %s", stdout.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	previous, err := store.GetSkillOptTrainIteration(context.Background(), "optimizer-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration previous returned error: %v", err)
	}
	if previous.State != skillopt.TrainStateCandidatePromoted {
		t.Fatalf("previous iteration = %+v", previous)
	}
	if !strings.Contains(previous.MetadataJSON, `"source":"gitmoot skillopt train continue synced candidate state"`) {
		t.Fatalf("previous iteration metadata = %s", previous.MetadataJSON)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != "optimizer-train-002" || latest.BaseTemplateVersionID != "planner@v2" || latest.State != skillopt.TrainStateItemsReady {
		t.Fatalf("latest iteration = %+v", latest)
	}
}

func TestSkillOptTrainContinueSyncsHumanCandidatePromotionBeforeReviewPublish(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with pre-review human promotion guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_created") {
		t.Fatalf("optimizer stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "candidate", "promote",
		"--home", home,
		"planner@v2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("candidate promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue sync/start-next exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") || !strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") {
		t.Fatalf("sync/start-next stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("pre-review sync published github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	previous, err := store.GetSkillOptTrainIteration(context.Background(), "optimizer-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration previous returned error: %v", err)
	}
	if previous.State != skillopt.TrainStateCandidatePromoted || previous.IssueNumber != 0 || strings.TrimSpace(previous.IssueURL) != "" {
		t.Fatalf("previous iteration = %+v", previous)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != "optimizer-train-002" || latest.BaseTemplateVersionID != "planner@v2" || latest.State != skillopt.TrainStateItemsReady {
		t.Fatalf("latest iteration = %+v", latest)
	}
}

func TestSkillOptTrainContinueRequiresReasonForExternalCandidateRejection(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with externally rejected candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "candidate", "reject",
		"--home", home,
		"planner@v2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("candidate reject exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue external reject without reason exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "train candidate rejection requires --reason") {
		t.Fatalf("external reject without reason stderr = %q", stderr.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("external reject without reason published github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
	store := openCLIJobStore(t, home)
	previous, err := store.GetSkillOptTrainIteration(context.Background(), "optimizer-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration previous returned error: %v", err)
	}
	if previous.State != skillopt.TrainStateCandidateCreated || strings.TrimSpace(previous.DecisionReason) != "" {
		t.Fatalf("previous iteration after failed sync = %+v", previous)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after failed external reject sync returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--reject", "planner@v2",
		"--reason", "too broad",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue external reject with reason exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "rejected_candidate: planner@v2") || !strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") {
		t.Fatalf("external reject with reason stdout = %s", stdout.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	previous, err = store.GetSkillOptTrainIteration(context.Background(), "optimizer-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration previous after sync returned error: %v", err)
	}
	if previous.State != skillopt.TrainStateCandidateRejected || previous.DecisionReason != "too broad" {
		t.Fatalf("previous iteration after external reject sync = %+v", previous)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != "optimizer-train-002" || latest.BaseTemplateVersionID != baseVersionID || latest.State != skillopt.TrainStateItemsReady {
		t.Fatalf("latest iteration = %+v", latest)
	}
}

func TestSkillOptTrainContinuePublishesCandidateReviewWhenSkillFileUploadFails(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with reviewable candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{upsertFileErr: errors.New("contents write denied")}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue candidate review with file upload failure exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") {
		t.Fatalf("candidate review stdout = %s", stdout.String())
	}
	if !strings.Contains(fakeGitHub.createdIssue.Body, "File publish warning") ||
		!strings.Contains(fakeGitHub.createdIssue.Body, "contents write denied") {
		t.Fatalf("candidate review body missing file publish warning:\n%s", fakeGitHub.createdIssue.Body)
	}
	if len(fakeGitHub.upsertedFiles) != 0 {
		t.Fatalf("file upload failure recorded uploaded files: %+v", fakeGitHub.upsertedFiles)
	}
}

func TestSkillOptTrainCandidateReviewBodyMarksNoOpNotPromotable(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	store := openCLIJobStore(t, home)
	defer store.Close()

	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with a changed candidate.")
	version, err := skillopt.ImportCandidatePackage(context.Background(), store, candidate, "candidate.json")
	if err != nil {
		t.Fatalf("ImportCandidatePackage returned error: %v", err)
	}
	review, err := store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	review.EvalReportJSON = `{"score":0,"hard":0,"soft":0,"gate_status":"blocked","promotable":false,"no_candidate_reason":"best_origin_initial_skill"}`
	review.SummaryMetadataJSON = `{"best_origin":"initial_skill","total_accepts":0,"promotable":false,"no_candidate_reason":"best_origin_initial_skill"}`
	if err := store.UpsertAgentTemplateCandidateReview(context.Background(), review); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview returned error: %v", err)
	}
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	body, err := skillOptTrainCandidateReviewBody(context.Background(), store, session, db.SkillOptTrainIteration{
		ID:                    "optimizer-train-001",
		CandidateVersionID:    version.ID,
		BaseTemplateVersionID: baseVersionID,
	}, home, nil, nil)
	if err != nil {
		t.Fatalf("skillOptTrainCandidateReviewBody returned error: %v", err)
	}
	for _, want := range []string{
		"### Scores And Gate",
		"Test score: `0`",
		"Hard score: `0`",
		"Soft score: `0`",
		"Gate status: `blocked`",
		"No-op status: `blocked: best_origin_initial_skill`",
		"Promotability: `not promotable: best_origin_initial_skill`",
		"Promote: unavailable because best_origin_initial_skill.",
		"Wait: take no action",
		"Keep improving: reject with an actionable reason",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("candidate review body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "--promote "+version.ID) {
		t.Fatalf("candidate review body exposed promote command for no-op metadata:\n%s", body)
	}
	_, err = decideSkillOptTrainCandidate(context.Background(), store, session, db.SkillOptTrainIteration{
		ID:                 "optimizer-train-001",
		State:              skillopt.TrainStateCandidateReviewPublished,
		CandidateVersionID: version.ID,
	}, skillOptTrainContinueRequest{PromoteCandidate: version.ID})
	if err == nil || !strings.Contains(err.Error(), "candidate planner@v2 is not promotable: best_origin_initial_skill") {
		t.Fatalf("decideSkillOptTrainCandidate promote error = %v", err)
	}
	blockedVersion, err := store.GetAgentTemplateVersionByID(context.Background(), version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID after blocked promote returned error: %v", err)
	}
	if blockedVersion.State != "pending" {
		t.Fatalf("blocked promote changed version state = %+v", blockedVersion)
	}

	review.EvalReportJSON = `{"score":0,"hard":0,"soft":0,"gate_status":"blocked"}`
	review.SummaryMetadataJSON = `{"best_origin":"initial_skill","total_accepts":0}`
	if err := store.UpsertAgentTemplateCandidateReview(context.Background(), review); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview without reason returned error: %v", err)
	}
	body, err = skillOptTrainCandidateReviewBody(context.Background(), store, session, db.SkillOptTrainIteration{
		ID:                    "optimizer-train-001",
		CandidateVersionID:    version.ID,
		BaseTemplateVersionID: baseVersionID,
	}, home, nil, nil)
	if err != nil {
		t.Fatalf("skillOptTrainCandidateReviewBody without reason returned error: %v", err)
	}
	for _, want := range []string{
		"No-op status: `blocked: best_origin_initial_skill`",
		"Promotability: `not promotable: best_origin_initial_skill`",
		"Promote: unavailable because best_origin_initial_skill.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("candidate review body without reason missing %q:\n%s", want, body)
		}
	}
}

func TestSkillOptTrainContinuePublishesCandidateReviewAndRejects(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with rejectable candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{
		host:               "https://github.example.com",
		commentURLOverride: "https://github.example.com/api/v3/repos/owner/review/issues/comments/1",
	}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	legacyStore := openCLIJobStore(t, home)
	iteration, err := legacyStore.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after optimizer returned error: %v", err)
	}
	iteration.IssueNumber = 67
	iteration.IssueRepo = "owner/review"
	if err := legacyStore.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration legacy review issue returned error: %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("Close after legacy review issue update returned error: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(fakeGitHub.postedComments) != 1 || fakeGitHub.postedComments[0].Repo.FullName() != "owner/review" || fakeGitHub.postedComments[0].IssueNumber != 67 {
		t.Fatalf("candidate review comments = %+v", fakeGitHub.postedComments)
	}
	commentBody := fakeGitHub.postedComments[0].Body
	for _, want := range []string{
		"## SkillOpt Candidate Review",
		"### Artifacts",
		"`candidate-diff`",
		"Eval report: stored with the pending candidate review record.",
	} {
		if !strings.Contains(commentBody, want) {
			t.Fatalf("candidate review comment missing %q:\n%s", want, commentBody)
		}
	}
	for _, unwanted := range []string{
		"### Eval Report\n```json",
		"### Candidate Template Diff\n```diff",
	} {
		if strings.Contains(commentBody, unwanted) {
			t.Fatalf("candidate review comment contains %q:\n%s", unwanted, commentBody)
		}
	}
	commentStore := openCLIJobStore(t, home)
	publishedIteration, err := commentStore.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after existing issue publish returned error: %v", err)
	}
	if publishedIteration.IssueURL != "https://github.example.com/owner/review/issues/67" {
		t.Fatalf("published issue url = %q", publishedIteration.IssueURL)
	}
	if err := commentStore.Close(); err != nil {
		t.Fatalf("Close after existing issue publish returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--reject", "planner@v2",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue reject without reason exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires --reason") {
		t.Fatalf("reject without reason stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--reject", "planner@v2",
		"--reason", "too broad",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue reject exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_rejected") || !strings.Contains(stdout.String(), "rejected_candidate: planner@v2") {
		t.Fatalf("reject stdout = %s", stdout.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if current.VersionID != baseVersionID {
		t.Fatalf("current template version = %q, want %q", current.VersionID, baseVersionID)
	}
	finalIteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if finalIteration.State != skillopt.TrainStateCandidateRejected || finalIteration.DecisionReason != "too broad" {
		t.Fatalf("iteration after reject = %+v", finalIteration)
	}
}

func TestSkillOptTrainContinuePublishesCandidateReviewToExistingPullRequest(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with PR review guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{
		host:               "https://github.example.com",
		commentKinds:       map[int64]string{77: "pull"},
		commentURLOverride: "https://github.example.com/api/v3/repos/owner/review/issues/comments/1",
	}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after optimizer returned error: %v", err)
	}
	iteration.PullRequestNumber = 77
	iteration.PullRequestRepo = "owner/review"
	if err := store.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration PR review returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after PR review update returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue candidate PR review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(fakeGitHub.postedComments) != 1 || fakeGitHub.postedComments[0].Repo.FullName() != "owner/review" || fakeGitHub.postedComments[0].IssueNumber != 77 {
		t.Fatalf("candidate PR review comments = %+v", fakeGitHub.postedComments)
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" {
		t.Fatalf("candidate PR review created issue unexpectedly: %+v", fakeGitHub.createdIssue)
	}
	store = openCLIJobStore(t, home)
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after PR publish returned error: %v", err)
	}
	if iteration.PullRequestURL != "https://github.example.com/owner/review/pull/77" {
		t.Fatalf("published pull request url = %q", iteration.PullRequestURL)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after PR publish check returned error: %v", err)
	}
}

func TestSkillOptTrainContinueDoesNotRepostCandidateReviewWhilePublishing(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with publishing recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	marker := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", marker)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", marker)
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession marker returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue publishing marker exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "marked publishing") || !strings.Contains(stderr.String(), "SkillOpt candidate review: optimizer-train") {
		t.Fatalf("publishing marker stderr = %q", stderr.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("publishing marker reposted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}

	store = openCLIJobStore(t, home)
	session, err = store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession before recovery returned error: %v", err)
	}
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration before recovery returned error: %v", err)
	}
	marker["status"] = "published_external"
	marker["issue_number"] = int64(8)
	marker["issue_url"] = "https://github.com/owner/product/issues/8"
	marker["review_url"] = "https://github.com/owner/product/issues/8"
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", marker)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", marker)
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration recovery marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close recovery marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue recovery marker exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || !strings.Contains(stdout.String(), "candidate_review: https://github.com/owner/product/issues/8") {
		t.Fatalf("recovery marker stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("recovery marker reposted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
}

func TestSkillOptTrainContinueRetriesStaleCandidateReviewPublishingMarker(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with stale publishing recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	marker := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Add(-skillOptTrainCandidateReviewLockTTL - time.Minute).Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", marker)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", marker)
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration stale marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close stale marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue stale publishing marker exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || !strings.Contains(stdout.String(), "candidate_review: https://github.com/owner/product/issues/8") {
		t.Fatalf("stale publishing marker stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "owner/product" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("stale publishing marker github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
}

func TestSkillOptTrainContinueRetriesAfterCandidateReviewMarkerWriteFailure(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with marker failure recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	_, err = publishSkillOptTrainCandidateReview(context.Background(), config.Paths{}, store, session, iteration, home)
	if err == nil || !strings.Contains(err.Error(), "write candidate review pre-publish recovery marker") {
		t.Fatalf("publishSkillOptTrainCandidateReview marker write failure err = %v", err)
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("marker write failure posted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after marker failure returned error: %v", err)
	}
	if strings.Contains(iteration.MetadataJSON, `"status":"publishing"`) {
		t.Fatalf("marker write failure recorded publishing metadata: %+v", iteration)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close marker failure store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue retry after marker failure exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || fakeGitHub.createdIssue.Repo.FullName() == "" {
		t.Fatalf("retry after marker failure stdout=%s github=%+v", stdout.String(), fakeGitHub.createdIssue)
	}
}

func TestSkillOptTrainContinueRetriesInterruptedBeforeCandidateReviewExternalPost(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with interrupted pre-external recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	marker := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", marker)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", marker)
	if err := writeSkillOptCandidateReviewRecovery(config.PathsForHome(home), session, iteration, marker); err != nil {
		t.Fatalf("writeSkillOptCandidateReviewRecovery pre-external marker returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration pre-external marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close pre-external marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue pre-external retry exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || fakeGitHub.createdIssue.Repo.FullName() == "" {
		t.Fatalf("pre-external retry stdout=%s github=%+v", stdout.String(), fakeGitHub.createdIssue)
	}
}

func TestSkillOptTrainContinueBlocksInterruptedCandidateReviewExternalPost(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with interrupted external post recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	stalePublishing := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Add(-skillOptTrainCandidateReviewLockTTL - time.Minute).Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", stalePublishing)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", stalePublishing)
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration stale marker returned error: %v", err)
	}
	posting := map[string]any{
		"status":                   "posting_external",
		"candidate_version":        iteration.CandidateVersionID,
		"issue_repo":               "owner/product",
		"issue_title":              "SkillOpt candidate review: optimizer-train",
		"external_post_started_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
	}
	if err := writeSkillOptCandidateReviewRecovery(config.PathsForHome(home), session, iteration, posting); err != nil {
		t.Fatalf("writeSkillOptCandidateReviewRecovery posting marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close interrupted marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue interrupted external post exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "external post started") || !strings.Contains(stderr.String(), "SkillOpt candidate review: optimizer-train") {
		t.Fatalf("interrupted external post stderr = %q", stderr.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("interrupted external post reposted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
}

func TestSkillOptTrainContinueRecoversCandidateReviewFromSidecar(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with sidecar recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	publishing := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", publishing)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", publishing)
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration publishing marker returned error: %v", err)
	}
	sidecar := map[string]any{
		"status":            "published_external",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_number":      int64(8),
		"issue_url":         "https://github.com/owner/product/issues/8",
		"review_url":        "https://github.com/owner/product/issues/8",
	}
	paths := config.PathsForHome(home)
	if err := writeSkillOptCandidateReviewRecovery(paths, session, iteration, sidecar); err != nil {
		t.Fatalf("writeSkillOptCandidateReviewRecovery returned error: %v", err)
	}
	sidecarPath := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if err := store.Close(); err != nil {
		t.Fatalf("Close sidecar marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--promote", "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue sidecar recovery exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_promoted") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") {
		t.Fatalf("sidecar recovery stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("sidecar recovery reposted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
	if _, err := os.Stat(sidecarPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar still exists err=%v", err)
	}
}

func TestSkillOptCandidateReviewRecoveryNameAvoidsSanitizationCollisions(t *testing.T) {
	first := skillOptCandidateReviewRecoveryName("feature/foo", "iter")
	second := skillOptCandidateReviewRecoveryName("feature?foo", "iter")
	if first == "" || second == "" {
		t.Fatalf("recovery names are empty: first=%q second=%q", first, second)
	}
	if first == second {
		t.Fatalf("recovery names collided: %q", first)
	}
}

func TestSkillOptTrainContinueStartNextRejectsEvalRunCollision(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with collision guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--promote", "planner@v2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:         "optimizer-train-review-002",
		TemplateID: "planner",
		TargetRepo: "owner/product",
		State:      "review",
	}); err != nil {
		t.Fatalf("UpsertEvalRun collision returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close collision store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--start-next"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue start-next collision exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "eval run optimizer-train-review-002 already exists") {
		t.Fatalf("start-next collision stderr = %q", stderr.String())
	}
}

func TestSkillOptTrainContinueStartNextRejectsBusyLock(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with start-next lock guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--promote", "planner@v2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	now := time.Now().UTC()
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: skillOptTrainStartNextLockKey("optimizer-train"),
		OwnerJobID:  "test-start-next",
		OwnerToken:  "token",
		ExpiresAt:   now.Add(skillOptTrainStartNextLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not acquire start-next lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close lock store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--start-next"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue start-next busy exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "next iteration is already starting") {
		t.Fatalf("start-next busy stderr = %q", stderr.String())
	}
}

func TestSkillOptTrainContinueRecordsOptimizerFailureAtPackageGate(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{fail: true}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--out-root", outRoot,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue failing optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "optimizer failed") || !strings.Contains(stderr.String(), "optimizer stderr") {
		t.Fatalf("optimizer failure stderr = %q", stderr.String())
	}
	for _, want := range []string{
		"backend: custom",
		"optimizer_backend: openai_chat",
		"target_backend: openai_chat",
		"evaluator_backend: openai_chat",
		"backend_config_status: external_credentials_may_be_required",
		"optimizer_lock: acquired",
		"recovery_available: false",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("optimizer failure stdout missing %q:\n%s", want, stdout.String())
		}
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after optimizer failure = %+v", iteration)
	}
	if !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) || !strings.Contains(iteration.MetadataJSON, "optimizer stderr") {
		t.Fatalf("iteration metadata after failure = %s", iteration.MetadataJSON)
	}
	if _, err := store.GetAgentTemplateVersionByID(context.Background(), "planner@v2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("optimizer failure created candidate err=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after optimizer failure returned error: %v", err)
	}

	changedOutRoot := filepath.Join(t.TempDir(), "changed-optimizer")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--out-root", changedOutRoot,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue changed out-root retry exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "optimizer package already exported") {
		t.Fatalf("changed out-root retry stderr = %q", stderr.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("changed out-root retry ran optimizer: %+v", runner.calls)
	}

	runner.fail = false
	runner.candidate = cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with retry-safe candidate guidance.")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue retry exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 2 {
		t.Fatalf("optimizer calls after retry = %+v, want two", runner.calls)
	}
	retryCall := runner.calls[1]
	if argValue(retryCall.args, "--out-root") != outRoot || argValue(retryCall.args, "--training-package") != filepath.Join(outRoot, "training.json") {
		t.Fatalf("retry did not reuse persisted optimizer paths: %+v", retryCall.args)
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_created") || !strings.Contains(stdout.String(), "imported_candidate: planner@v2") {
		t.Fatalf("retry stdout = %s", stdout.String())
	}
}

func TestSkillOptTrainContinueReportsRecoveryAfterOptimizerFailureArtifacts(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate:                cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with recoverable candidate guidance."),
		failAfterCandidate:       true,
		failAfterCandidateStderr: "optimizer backend config stderr after candidate",
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--out-root", outRoot,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue failing optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "optimizer backend config stderr after candidate") {
		t.Fatalf("optimizer failure stderr = %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_available: true") {
		t.Fatalf("optimizer failure stdout did not report recovery:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(outRoot, "candidate.json")); err != nil {
		t.Fatalf("candidate package was not written before failure: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status after recoverable optimizer failure exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status_phase: recovery_available") ||
		!strings.Contains(stdout.String(), "recovery_available: true") ||
		!strings.Contains(stdout.String(), "optimizer_recovery_available: true") {
		t.Fatalf("train status did not advertise recovery:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status recovery json exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var statusJSON skillOptTrainStatusSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &statusJSON); err != nil {
		t.Fatalf("train status recovery json did not decode: %v\n%s", err, stdout.String())
	}
	if statusJSON.StatusPhase != "recovery_available" || !statusJSON.RecoveryAvailable || statusJSON.Verbose != nil {
		t.Fatalf("train status recovery json = %+v", statusJSON)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after recoverable optimizer failure = %+v", iteration)
	}
}

func TestSkillOptTrainRecoverImportsCandidateArtifacts(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate:          cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with recovered candidate guidance."),
		failAfterCandidate: true,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--out-root", outRoot}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue recoverable failure exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train recover candidate exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"recovery_state: completed_candidate",
		"current_phase: candidate_created",
		"candidate: planner@v2",
		"next: publish candidate diff and preview review",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train recover candidate stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateCandidateCreated || iteration.CandidateVersionID != "planner@v2" {
		t.Fatalf("iteration after candidate recovery = %+v", iteration)
	}
	if _, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v2"); err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview recovered candidate returned error: %v", err)
	}
}

func TestSkillOptTrainRecoverRecordsNoCandidateArtifacts(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan the work.")
	candidate.EvalReport = json.RawMessage(`{"promotable":true,"no_candidate_reason":"stale_non_blocking_reason","next_action":"stale next action"}`)
	candidate.Summary.Metadata = json.RawMessage(`{"promotable":true,"no_candidate_reason":"stale_non_blocking_reason","next_action":"stale next action"}`)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate:          candidate,
		failAfterCandidate: true,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--out-root", outRoot}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue no-candidate recoverable failure exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train recover no-candidate exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"recovery_state: completed_no_candidate",
		"current_phase: optimizer_completed_no_candidate",
		"no_candidate_reason: candidate content is unchanged from the base version",
		"next: do not publish a candidate review",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train recover no-candidate stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateOptimizerCompletedNoCandidate || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after no-candidate recovery = %+v", iteration)
	}
	if strings.Contains(iteration.MetadataJSON, "stale_non_blocking_reason") || strings.Contains(iteration.MetadataJSON, "stale next action") {
		t.Fatalf("iteration metadata used stale promotable no-candidate package metadata: %s", iteration.MetadataJSON)
	}
}

func TestSkillOptTrainRecoverReportsIncompleteArtifacts(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{fail: true}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--out-root", outRoot}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue failed optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if err := os.WriteFile(filepath.Join(outRoot, "summary.json"), []byte(`{"status":"failed","reason":"timeout","no_candidate_reason":null}`), 0o644); err != nil {
		t.Fatalf("write summary artifact returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train recover incomplete exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: incomplete_resumable") ||
		!strings.Contains(stderr.String(), "candidate.json is missing") {
		t.Fatalf("train recover incomplete stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after incomplete recovery = %+v", iteration)
	}
}

func TestSkillOptTrainRecoverRejectsCorruptedCandidateArtifacts(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{fail: true}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--out-root", outRoot}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue failed optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if err := os.WriteFile(filepath.Join(outRoot, "candidate.json"), []byte(`{not-json`), 0o644); err != nil {
		t.Fatalf("write corrupted candidate returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train recover corrupted exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: corrupted_unrecoverable") ||
		!strings.Contains(stderr.String(), "decode optimizer candidate package") {
		t.Fatalf("train recover corrupted stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after corrupted recovery = %+v", iteration)
	}
}

func TestSkillOptTrainRecoverLeavesStateOnMissingCandidateArtifact(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate:          cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with missing artifact recovery guidance."),
		failAfterCandidate: true,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--out-root", outRoot}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue recoverable failure exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if err := os.Remove(filepath.Join(outRoot, "artifacts", "candidate.diff.md")); err != nil {
		t.Fatalf("remove candidate diff artifact returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train recover missing artifact exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: corrupted_unrecoverable") ||
		!strings.Contains(stderr.String(), `candidate artifact "candidate-diff"`) {
		t.Fatalf("train recover missing artifact stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after missing artifact recovery = %+v", iteration)
	}
	if _, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v2"); err == nil {
		t.Fatalf("missing artifact recovery unexpectedly imported candidate review")
	}
}

func TestSkillOptTrainRecoverRejectsCandidateBeforePackageState(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	artifactDir := filepath.Join(outRoot, "artifacts")
	diffContent := []byte("candidate diff\n")
	diffSize := int64(len(diffContent))
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("create artifact dir returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), diffContent, 0o644); err != nil {
		t.Fatalf("write candidate diff artifact returned error: %v", err)
	}
	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with premature recovery guidance.")
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
		t.Fatalf("marshal candidate package returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outRoot, "candidate.json"), encoded, 0o644); err != nil {
		t.Fatalf("write candidate package returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "optimizer-train", "--out-root", outRoot}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train recover premature candidate exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: corrupted_unrecoverable") ||
		!strings.Contains(stderr.String(), "cannot recover optimizer artifacts while iteration is in state feedback_synced") {
		t.Fatalf("train recover premature candidate stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateFeedbackSynced || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after premature recovery = %+v", iteration)
	}
	if _, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v2"); err == nil {
		t.Fatalf("premature recovery unexpectedly imported candidate review")
	}
}

func TestSkillOptTrainRecoverRefusesActiveOptimizerLock(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate:          cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with locked recovery guidance."),
		failAfterCandidate: true,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--out-root", outRoot}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue recoverable failure exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	release, _, err := acquireSkillOptTrainOptimizerLock(context.Background(), store, "optimizer-train", "optimizer-train-001", time.Hour, skillOptTrainOptimizerRequest{OutRoot: outRoot})
	if err != nil {
		t.Fatalf("acquire optimizer lock returned error: %v", err)
	}
	defer store.Close()
	defer func() {
		_ = release(context.Background())
	}()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train recover locked optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: optimizer_active") ||
		!strings.Contains(stderr.String(), "skillopt train optimizer is already running") {
		t.Fatalf("train recover locked stdout=%s stderr=%s", stdout.String(), stderr.String())
	}

	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after locked recovery = %+v", iteration)
	}
	if _, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v2"); err == nil {
		t.Fatalf("locked recovery unexpectedly imported candidate review")
	}
}

func TestSkillOptTrainContinueRecordsOptimizerSetupFailure(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--gate", "unsupported",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue invalid gate exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `optimizer gate "unsupported" is not supported`) {
		t.Fatalf("invalid gate stderr = %q", stderr.String())
	}
	for _, want := range []string{
		"backend: custom",
		"optimizer_backend: openai_chat",
		"target_backend: openai_chat",
		"evaluator_backend: openai_chat",
		"backend_config_status: external_credentials_may_be_required",
		"optimizer_lock: acquired",
		"optimizer_command: -",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("optimizer setup failure stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("optimizer ran after setup failure: %+v", runner.calls)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after optimizer setup failure = %+v", iteration)
	}
	if !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) || !strings.Contains(iteration.MetadataJSON, "unsupported") {
		t.Fatalf("iteration metadata after setup failure = %s", iteration.MetadataJSON)
	}
}

func TestSkillOptTrainContinueRejectsCandidateForDifferentSessionTemplate(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	store := openCLIJobStore(t, home)
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("other", "Do different work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate other returned error: %v", err)
	}
	other, err := store.GetAgentTemplate(context.Background(), "other")
	if err != nil {
		t.Fatalf("GetAgentTemplate other returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after other template seed returned error: %v", err)
	}

	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "other", other.VersionID, "Do unrelated candidate work."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue mismatched candidate exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `optimizer candidate template_id "other" does not match train session template "planner"`) {
		t.Fatalf("mismatched candidate stderr = %q", stderr.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("optimizer calls = %+v, want one", runner.calls)
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateOptimizerCompleted || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after mismatched candidate = %+v", iteration)
	}
	if !strings.Contains(iteration.MetadataJSON, `"candidate_import"`) || !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) {
		t.Fatalf("iteration metadata after mismatched candidate = %s", iteration.MetadataJSON)
	}
	if _, err := store.GetAgentTemplateVersionByID(context.Background(), "other@v2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("mismatched candidate imported other version err=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after mismatched candidate returned error: %v", err)
	}

	runner.candidate = cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with recovered candidate guidance.")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue deterministic import retry exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("deterministic import retry reran optimizer: %+v", runner.calls)
	}

	staleOutRoot := argValue(runner.calls[0].args, "--out-root")
	if staleOutRoot == "" {
		t.Fatalf("first optimizer call did not include --out-root: %+v", runner.calls[0].args)
	}
	staleFiles := []string{
		filepath.Join(staleOutRoot, "candidate.json"),
		filepath.Join(staleOutRoot, "history.json"),
		filepath.Join(staleOutRoot, "summary.json"),
		filepath.Join(staleOutRoot, "runtime_state.json"),
		filepath.Join(staleOutRoot, "best_skill.md"),
		filepath.Join(staleOutRoot, "steps", "step_0001", "step_record.json"),
		filepath.Join(staleOutRoot, "artifacts", "stale.diff.md"),
	}
	for _, path := range staleFiles {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create stale optimizer artifact dir returned error: %v", err)
		}
		if err := os.WriteFile(path, []byte("stale optimizer artifact\n"), 0o644); err != nil {
			t.Fatalf("write stale optimizer artifact returned error: %v", err)
		}
	}
	unrelatedFile := filepath.Join(staleOutRoot, "unrelated", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(unrelatedFile), 0o755); err != nil {
		t.Fatalf("create unrelated out-root file dir returned error: %v", err)
	}
	if err := os.WriteFile(unrelatedFile, []byte("do not remove\n"), 0o644); err != nil {
		t.Fatalf("write unrelated out-root file returned error: %v", err)
	}
	trainingPackagePath := filepath.Join(staleOutRoot, "training.json")
	if _, err := os.Stat(trainingPackagePath); err != nil {
		t.Fatalf("expected persisted training package before rerun: %v", err)
	}
	runner.beforeRun = func(dir string, args []string) error {
		if len(runner.calls) != 2 {
			return nil
		}
		if dir != staleOutRoot || argValue(args, "--out-root") != staleOutRoot {
			return fmt.Errorf("rerun did not reuse optimizer out-root; dir=%s args=%v", dir, args)
		}
		if _, err := os.Stat(trainingPackagePath); err != nil {
			return fmt.Errorf("rerun cleanup removed training package: %w", err)
		}
		if _, err := os.Stat(unrelatedFile); err != nil {
			return fmt.Errorf("rerun cleanup removed unrelated out-root file: %w", err)
		}
		for _, path := range staleFiles {
			if path == trainingPackagePath {
				continue
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("stale optimizer artifact still exists before rerun: %s", path)
			}
		}
		return nil
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--rerun-optimizer",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue after explicit optimizer rerun exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 2 {
		t.Fatalf("optimizer calls after explicit optimizer rerun = %+v, want two", runner.calls)
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_created") || !strings.Contains(stdout.String(), "imported_candidate: planner@v2") {
		t.Fatalf("explicit optimizer rerun stdout = %s", stdout.String())
	}
}

func TestSkillOptTrainContinueOptimizerHandlesMissingIteration(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainSession(context.Background(), db.SkillOptTrainSession{
		ID:             "optimizer-train-empty",
		TemplateID:     "planner",
		TargetRepo:     "owner/product",
		RequestSummary: "Train planner outputs from human feedback.",
		TaskKind:       "custom",
		State:          skillopt.TrainStateFeedbackSynced,
	}); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train-empty",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue missing iteration exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "next: train session has no iteration to continue") {
		t.Fatalf("missing iteration stdout = %s", stdout.String())
	}
}

func TestSkillOptTrainOptimizerLockStatusBoundsExpiredPIDLiveness(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	lock := db.ResourceLock{
		OwnerPID:  int64(os.Getpid()),
		ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	if got := skillOptTrainOptimizerLockStatus(lock, now); got != "active_expired_heartbeat" {
		t.Fatalf("recent expired live-pid lock status = %q, want active_expired_heartbeat", got)
	}
	lock.ExpiresAt = now.Add(-skillOptTrainOptimizerExpiredHeartbeatGrace).Format(time.RFC3339Nano)
	if got := skillOptTrainOptimizerLockStatus(lock, now); got != "stale" {
		t.Fatalf("old expired live-pid lock status = %q, want stale", got)
	}
}

func TestSkillOptTrainContinueRefusesConcurrentOptimizer(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with lock-safe candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	store := openCLIJobStore(t, home)
	release, _, err := acquireSkillOptTrainOptimizerLock(context.Background(), store, "optimizer-train", "optimizer-train-001", time.Hour, skillOptTrainOptimizerRequest{Backend: "codex"})
	if err != nil {
		t.Fatalf("acquire optimizer lock returned error: %v", err)
	}
	lock, err := store.GetResourceLock(context.Background(), skillOptTrainOptimizerLockKey("optimizer-train", "optimizer-train-001"))
	if err != nil {
		t.Fatalf("GetResourceLock optimizer returned error: %v", err)
	}
	if lock.ResourceKey != "skillopt-train:optimizer-train:optimizer-train-001" ||
		lock.OwnerPID <= 0 ||
		strings.TrimSpace(lock.OwnerHostname) == "" ||
		strings.TrimSpace(lock.CommandHash) == "" {
		t.Fatalf("optimizer lock metadata = %+v", lock)
	}
	acquiredAt, ok := parseSkillOptStatusTime(lock.AcquiredAt)
	if !ok {
		t.Fatalf("optimizer lock acquired_at = %q, want parseable time", lock.AcquiredAt)
	}
	expiresAt, ok := parseSkillOptStatusTime(lock.ExpiresAt)
	if !ok {
		t.Fatalf("optimizer lock expires_at = %q, want parseable time", lock.ExpiresAt)
	}
	if lease := expiresAt.Sub(acquiredAt); lease <= 0 || lease > skillOptTrainOptimizerHeartbeatLeaseTTL+time.Second {
		t.Fatalf("optimizer lock lease = %s, want short heartbeat lease around %s", lease, skillOptTrainOptimizerHeartbeatLeaseTTL)
	}
	legacyLock, err := store.GetResourceLock(context.Background(), skillOptTrainLegacyOptimizerLockKey("optimizer-train", "optimizer-train-001"))
	if err != nil {
		t.Fatalf("GetResourceLock legacy optimizer returned error: %v", err)
	}
	if legacyLock.OwnerJobID != lock.OwnerJobID ||
		legacyLock.OwnerPID != lock.OwnerPID ||
		legacyLock.OwnerHostname != lock.OwnerHostname ||
		legacyLock.CommandHash != lock.CommandHash {
		t.Fatalf("legacy optimizer lock metadata = %+v, want owner metadata matching %+v", legacyLock, lock)
	}
	defer store.Close()
	defer func() {
		_ = release(context.Background())
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue locked optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "skillopt train optimizer is already running") {
		t.Fatalf("locked optimizer stderr = %q", stderr.String())
	}
	for _, want := range []string{
		"skillopt-train:optimizer-train:optimizer-train-001",
		"active owner=",
		"pid=",
		"heartbeat=",
		"hash=",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("locked optimizer stderr missing %q:\n%s", want, stderr.String())
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("optimizer ran while lock was held: %+v", runner.calls)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status with optimizer lock exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"status_phase: optimizer_running",
		"active_lock: optimizer skillopt-train:optimizer-train:optimizer-train-001 status=active",
		"active_lock: optimizer_legacy skillopt-train-optimizer:optimizer-train:optimizer-train-001 status=active",
		"owner=local-skillopt-train-optimizer-optimizer-train",
		"pid=",
		"heartbeat=",
		"expires=",
		"elapsed=",
		"hash=",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train status active lock stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptTrainContinueReportsStaleOptimizerLock(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with stale-lock-safe candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	store := openCLIJobStore(t, home)
	now := time.Now().UTC()
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey:   skillOptTrainOptimizerLockKey("optimizer-train", "optimizer-train-001"),
		OwnerJobID:    "stale-optimizer",
		OwnerToken:    "stale-token",
		OwnerPID:      0,
		OwnerHostname: "stale-host",
		CommandHash:   "stale-hash",
		ExpiresAt:     now.Add(-time.Minute).Format(time.RFC3339Nano),
	}, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatalf("AcquireResourceLock stale returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not create stale optimizer lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close stale lock store returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status stale lock exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status_phase: blocked_stale_lock") ||
		!strings.Contains(stdout.String(), "active_lock: optimizer skillopt-train:optimizer-train:optimizer-train-001 status=stale") {
		t.Fatalf("train status stale lock stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status stale lock json exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var statusJSON skillOptTrainStatusSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &statusJSON); err != nil {
		t.Fatalf("train status stale lock json did not decode: %v\n%s", err, stdout.String())
	}
	if statusJSON.StatusPhase != "blocked_stale_lock" || statusJSON.Verbose != nil {
		t.Fatalf("train status stale lock json = %+v", statusJSON)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--watch", "--verbose", "--poll", "1ms"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status watch stale lock exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "active_lock: optimizer skillopt-train:optimizer-train:optimizer-train-001 status=stale") ||
		!strings.Contains(stdout.String(), "status_phase: blocked_stale_lock") ||
		!strings.Contains(stdout.String(), "watch_state: waiting") {
		t.Fatalf("train status watch stale lock stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue stale lock recovery exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"optimizer_lock: recovered_stale",
		"current_phase: candidate_created",
		"imported_candidate: planner@v2",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue stale lock recovery stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("optimizer calls after stale lock recovery = %+v, want one", runner.calls)
	}
}

func TestSkillOptTrainContinueRefusesLegacyOptimizerLock(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with legacy-lock-safe candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	store := openCLIJobStore(t, home)
	now := time.Now().UTC()
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: skillOptTrainLegacyOptimizerLockKey("optimizer-train", "optimizer-train-001"),
		OwnerJobID:  "legacy-optimizer",
		OwnerToken:  "legacy-token",
		ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("AcquireResourceLock legacy returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not create legacy optimizer lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close legacy lock store returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue legacy lock exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "skillopt-train-optimizer:optimizer-train:optimizer-train-001") {
		t.Fatalf("legacy optimizer stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("optimizer ran while legacy lock was held: %+v", runner.calls)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status legacy lock exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "active_lock: optimizer_legacy skillopt-train-optimizer:optimizer-train:optimizer-train-001 status=active") {
		t.Fatalf("train status legacy lock stdout = %s", stdout.String())
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
		return subprocess.Result{Command: command, Args: args, Stdout: fmt.Sprintf(`{"gitmoot_result":{"decision":"implemented","summary":%q,"findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`+"\n", summary)}, nil
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
			"next_agents":  []any{},
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

func TestSkillOptTrainStartDryRunDoesNotInitializeFreshHome(t *testing.T) {
	home := t.TempDir()
	itemsPath := filepath.Join(t.TempDir(), "items.yml")
	if err := os.WriteFile(itemsPath, []byte(`items:
  - title: One
    brief: First item.
    output_type: markdown
  - title: Two
    brief: Second item.
    output_type: markdown
`), 0o644); err != nil {
		t.Fatalf("write items file: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--request", "Train landing page plans.",
		"--items-file", itemsPath,
		"--dry-run",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train start dry-run against fresh home exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "not initialized") {
		t.Fatalf("fresh-home dry-run stderr = %q", stderr.String())
	}
	if _, err := os.Stat(config.PathsForHome(home).Home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run initialized home, stat err=%v", err)
	}
}

func TestSkillOptTrainStartRejectsTooFewItemsAndWarnsOnHomogeneousItems(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("designer", "Design well.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	dir := t.TempDir()
	tooFewPath := filepath.Join(dir, "too-few.yml")
	if err := os.WriteFile(tooFewPath, []byte(`items:
  - title: Landing page
    brief: Create a product landing page.
    output_type: vue
`), 0o644); err != nil {
		t.Fatalf("write too-few items: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "designer",
		"--repo", "owner/product",
		"--request", "Train generic landing pages.",
		"--items-file", tooFewPath,
		"--yes",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("too-few train start exit code = %d, want 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "at least 2 items, got 1") {
		t.Fatalf("too-few stderr = %q", stderr.String())
	}

	homogeneousPath := filepath.Join(dir, "homogeneous.yml")
	if err := os.WriteFile(homogeneousPath, []byte(`items:
  - title: Landing page
    brief: Create a product landing page.
    output_type: vue
  - title: Landing page
    brief: Create a product landing page.
    output_type: vue
  - title: Landing page
    brief: Create a product landing page.
    output_type: vue
`), 0o644); err != nil {
		t.Fatalf("write homogeneous items: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "designer",
		"--repo", "owner/product",
		"--session", "homogeneous-train",
		"--request", "Train generic landing pages.",
		"--items-file", homogeneousPath,
		"--task-kind", "custom",
		"--yes",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("homogeneous train start exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "preferred_gate: hard_then_soft") || !strings.Contains(stdout.String(), "warning: duplicate item title") || !strings.Contains(stdout.String(), "warning: training items look homogeneous") {
		t.Fatalf("homogeneous stdout = %q", stdout.String())
	}
}

func TestReadSkillOptTrainItemsAcceptsTopLevelArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "items.yml")
	if err := os.WriteFile(path, []byte(`- item_id: alpha
  title: Alpha item
  brief: Create an alpha output.
  output_type: markdown
- item_id: beta
  title: Beta item
  brief: Create a beta output.
  output_type: markdown
`), 0o644); err != nil {
		t.Fatalf("write items file: %v", err)
	}
	items, warnings, err := readSkillOptTrainItems(path)
	if err != nil {
		t.Fatalf("readSkillOptTrainItems returned error: %v", err)
	}
	if len(items) != 2 || items[0].ItemID != "alpha" || len(warnings) != 0 {
		t.Fatalf("items=%+v warnings=%+v", items, warnings)
	}
}

func TestReadSkillOptTrainItemsRejectsMalformedWrappedItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "items.yml")
	if err := os.WriteFile(path, []byte(`items:
  - item_id: alpha
    title: Alpha item
    brief: Create an alpha output.
    output_type: markdown
    artifact_hints: scalar-not-list
`), 0o644); err != nil {
		t.Fatalf("write items file: %v", err)
	}
	if _, _, err := readSkillOptTrainItems(path); err == nil || !strings.Contains(err.Error(), "decode items-file") {
		t.Fatalf("readSkillOptTrainItems error = %v", err)
	}
}

func TestSkillOptTrainConfirmCommandStripsBoolFlagForms(t *testing.T) {
	command := skillOptTrainConfirmCommand([]string{
		"--template", "planner",
		"--repo", "owner/product",
		"--request", "Train landing pages.",
		"--dry-run=true",
		"-dry-run=false",
		"--yes=false",
	}, "generated-session")
	if strings.Contains(command, "dry-run") || strings.Contains(command, "--yes=false") {
		t.Fatalf("confirm command kept filtered bool flags: %s", command)
	}
	if !strings.Contains(command, "--session generated-session") {
		t.Fatalf("confirm command did not preserve generated session: %s", command)
	}
	if !strings.HasSuffix(command, "--yes") {
		t.Fatalf("confirm command did not append --yes: %s", command)
	}

	command = skillOptTrainConfirmCommand([]string{
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "explicit-session",
		"--request", "Train landing pages.",
	}, "generated-session")
	if strings.Contains(command, "generated-session") || !strings.Contains(command, "--session explicit-session") {
		t.Fatalf("confirm command did not preserve explicit session: %s", command)
	}
}

func TestSkillOptImportCandidateArtifacts(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	artifactDir := t.TempDir()
	diffContent := []byte("candidate diff\n")
	diffHash := artifact.ContentHash(diffContent)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), diffContent, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	diffSize := int64(len(diffContent))
	candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, "Plan with a concise risk section.")
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []skillopt.CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      diffHash,
		MediaType: "text/markdown",
		Driver:    "text",
		SizeBytes: &diffSize,
	}}
	candidatePath := filepath.Join(t.TempDir(), "candidate.json")
	encodedCandidate, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	if err := os.WriteFile(candidatePath, encodedCandidate, 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "import", "--home", home, "--file", candidatePath, "--artifact-dir", artifactDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported pending candidate planner@v2") {
		t.Fatalf("import stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "show", "--home", home, "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "diff_artifact: candidate-diff") {
		t.Fatalf("candidate show stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after import returned error: %v", err)
	}
	defer store.Close()
	stored, err := store.GetEvalArtifact(context.Background(), "candidate-diff")
	if err != nil {
		t.Fatalf("GetEvalArtifact returned error: %v", err)
	}
	if stored.Hash != diffHash || stored.SizeBytes != diffSize || stored.MediaType != "text/markdown" {
		t.Fatalf("stored artifact = %+v", stored)
	}
	blobContent, err := artifact.NewStore(paths.ArtifactBlobs).Read(diffHash)
	if err != nil {
		t.Fatalf("Read stored artifact returned error: %v", err)
	}
	if string(blobContent) != string(diffContent) {
		t.Fatalf("stored artifact content = %q", string(blobContent))
	}
}

func TestSkillOptImportCandidateArtifactFailuresDoNotCreatePendingCandidate(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		hash        string
		artifactDir bool
		writeFile   bool
		wantErr     string
	}{
		{
			name:        "missing artifact dir",
			path:        "candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: false,
			writeFile:   false,
			wantErr:     "candidate artifacts require --artifact-dir",
		},
		{
			name:        "invalid hash",
			path:        "candidate.diff.md",
			hash:        artifact.ContentHash([]byte("other")),
			artifactDir: true,
			writeFile:   true,
			wantErr:     "hash is",
		},
		{
			name:        "path traversal",
			path:        "../candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: true,
			writeFile:   false,
			wantErr:     "relative path inside artifact-dir",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
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
			installed, err := store.GetAgentTemplate(context.Background(), "planner")
			if err != nil {
				t.Fatalf("GetAgentTemplate returned error: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
			artifactDir := ""
			if tt.artifactDir {
				artifactDir = t.TempDir()
				if tt.writeFile {
					if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), []byte("candidate diff\n"), 0o644); err != nil {
						t.Fatalf("write diff artifact: %v", err)
					}
				}
			}
			candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, "Plan with a concise risk section.")
			candidate.Summary.DiffArtifactID = "candidate-diff"
			candidate.Artifacts = []skillopt.CandidateArtifactRef{{
				ID:        "candidate-diff",
				Path:      tt.path,
				Hash:      tt.hash,
				MediaType: "text/markdown",
				Driver:    "text",
			}}
			candidatePath := filepath.Join(t.TempDir(), "candidate.json")
			encodedCandidate, err := json.Marshal(candidate)
			if err != nil {
				t.Fatalf("marshal candidate: %v", err)
			}
			if err := os.WriteFile(candidatePath, encodedCandidate, 0o644); err != nil {
				t.Fatalf("write candidate: %v", err)
			}
			args := []string{"skillopt", "import", "--home", home, "--file", candidatePath}
			if artifactDir != "" {
				args = append(args, "--artifact-dir", artifactDir)
			}
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("skillopt import exit code = 0, stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}
			store, err = db.Open(paths.Database)
			if err != nil {
				t.Fatalf("Open after failed import returned error: %v", err)
			}
			defer store.Close()
			pending, err := store.ListPendingAgentTemplateVersions(context.Background(), "planner")
			if err != nil {
				t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
			}
			if len(pending) != 0 {
				t.Fatalf("pending versions = %+v, want none", pending)
			}
			if _, err := store.GetEvalArtifact(context.Background(), "candidate-diff"); err == nil {
				t.Fatalf("candidate artifact was registered despite failed import")
			}
		})
	}
}

func floatPtr(value float64) *float64 {
	return &value
}

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
	metadata := skillOptTrainStartMetadata("Train landing page reviews.", db.EvalRunModeExplore, db.ExplorationLevelHigh, 4, "soft", nil, nil, previewPolicy)
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
	code := Run([]string{"skillopt", "feedback", "github", "publish", "--home", home, "--run", "preview-train-review-001"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github publish default repo exit code = %d, stderr=%s", code, stderr.String())
	}
	if fake.createdIssue.Repo.FullName() != "owner/previews" {
		t.Fatalf("created issue repo = %s, want owner/previews", fake.createdIssue.Repo.FullName())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "github", "publish", "--home", home, "--run", "preview-train-review-001", "--repo", "Owner/Previews", "--pr", "9"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github publish matching repo exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(fake.postedComments) != 1 || !strings.EqualFold(fake.postedComments[0].Repo.FullName(), "owner/previews") || fake.postedComments[0].IssueNumber != 9 {
		t.Fatalf("posted comments = %+v, want owner/previews#9", fake.postedComments)
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
	code = Run([]string{"skillopt", "feedback", "github", "sync", "--home", home, "--run", "preview-train-review-001", "--issue", "8"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github sync default repo exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(fake.listedComments) != 1 || fake.listedComments[0].Repo.FullName() != "owner/previews" {
		t.Fatalf("listed comments = %+v, want owner/previews", fake.listedComments)
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

func TestSkillOptReviewWatcherImportsValidYAML(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "```yaml\nrun_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n```\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer
	paths := config.PathsForHome(home)
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}
	events, err := store.ListRankedFeedbackEvents(context.Background(), "watcher-review-001")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(events) != 2 || events[0].Reviewer != "alice" {
		t.Fatalf("ranked events = %+v", events)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusClosed || watch.LastSeenCommentID != 100 {
		t.Fatalf("watch = %+v", watch)
	}
	if len(fake.postedComments) != 1 || !strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchSuccessMarker) {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if len(fake.closedIssues) != 1 || fake.closedIssues[0].IssueNumber != 67 {
		t.Fatalf("closed issues = %+v", fake.closedIssues)
	}
	iteration, err := store.GetSkillOptTrainIteration(context.Background(), "watcher-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateFeedbackSynced {
		t.Fatalf("iteration state = %s, want %s", iteration.State, skillopt.TrainStateFeedbackSynced)
	}
	if !strings.Contains(stdout.String(), "imported 2 skillopt review feedback events") {
		t.Fatalf("stdout = %q; home=%s", stdout.String(), home)
	}
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("second pollSkillOptReviewWatches returned error: %v", err)
	}
	events, err = store.ListRankedFeedbackEvents(context.Background(), "watcher-review-001")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents after second poll returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ranked events after second poll = %+v", events)
	}
}

func TestSkillOptReviewWatcherCommentsInvalidYAMLDeduped(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(-time.Minute))
	paths := config.PathsForHome(home)
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if !strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchErrorMarker) ||
		!strings.Contains(fake.postedComments[0].Body, "missing feedback for expected item_id(s): item-002") {
		t.Fatalf("error comment = %q", fake.postedComments[0].Body)
	}
	if strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchStaleMarker) {
		t.Fatalf("invalid feedback produced stale notice = %q", fake.postedComments[0].Body)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusWatching || watch.LastSeenCommentID != 0 || watch.LastImportErrorHash == "" {
		t.Fatalf("watch after invalid import = %+v", watch)
	}
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("second pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments after second poll = %+v", fake.postedComments)
	}
	events, err := store.ListRankedFeedbackEvents(context.Background(), "watcher-review-001")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ranked events = %+v", events)
	}
	fake.comments[67][0].Body = "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n"
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("third pollSkillOptReviewWatches returned error: %v", err)
	}
	events, err = store.ListRankedFeedbackEvents(context.Background(), "watcher-review-001")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents after edit returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ranked events after edit = %+v", events)
	}
}

func TestSkillOptReviewWatcherPostsStaleNoticeOnce(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(-time.Minute))
	fake := &skillOptFakeGitHub{comments: map[int64][]github.IssueComment{67: {}}}
	var stdout bytes.Buffer
	paths := config.PathsForHome(home)

	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}

	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	body := fake.postedComments[0].Body
	for _, want := range []string{
		skillOptReviewWatchStaleMarker,
		"review_issue: `owner/previews#67`",
		"run_id: `watcher-review-001`",
		"waiting_for: complete YAML feedback for item_ids `item-001, item-002`",
		"I posted the SkillOpt review feedback for run watcher-review-001 on owner/previews#67.",
		"gitmoot skillopt train continue --session watcher-train",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stale notice missing %q:\n%s", want, body)
		}
	}
	if len(fake.closedIssues) != 0 {
		t.Fatalf("closed issues = %+v", fake.closedIssues)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusStaleNotified || !watch.StaleNotified {
		t.Fatalf("watch after stale notice = %+v", watch)
	}
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("second pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments after second poll = %+v", fake.postedComments)
	}

	watch.Status = db.SkillOptReviewWatchStatusWatching
	watch.StaleNotified = false
	if err := store.UpsertSkillOptReviewWatch(context.Background(), watch); err != nil {
		t.Fatalf("reset watch after stale notice returned error: %v", err)
	}
	fake.comments[67] = append(fake.comments[67], github.IssueComment{ID: 90, Body: body, Author: "gitmoot", CreatedAt: "2026-06-04T09:00:00Z"})
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("third pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments after remote stale marker = %+v", fake.postedComments)
	}
	watch, err = store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch after remote stale marker returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusStaleNotified || !watch.StaleNotified {
		t.Fatalf("watch after remote stale marker = %+v", watch)
	}

	fake.comments[67] = append(fake.comments[67], github.IssueComment{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"})
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("fourth pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 2 || !strings.Contains(fake.postedComments[1].Body, skillOptReviewWatchSuccessMarker) {
		t.Fatalf("posted comments after feedback = %+v", fake.postedComments)
	}
	if len(fake.closedIssues) != 1 {
		t.Fatalf("closed issues after feedback = %+v", fake.closedIssues)
	}
	watch, err = store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch after feedback returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusClosed {
		t.Fatalf("watch after stale feedback import = %+v", watch)
	}
}

func TestSkillOptReviewWatcherDoesNotStaleBeforeThreshold(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(time.Hour))
	fake := &skillOptFakeGitHub{comments: map[int64][]github.IssueComment{67: {}}}
	var stdout bytes.Buffer

	if _, err := pollSkillOptReviewWatches(context.Background(), config.PathsForHome(home), store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}

	if len(fake.postedComments) != 0 || len(fake.closedIssues) != 0 {
		t.Fatalf("posted=%+v closed=%+v", fake.postedComments, fake.closedIssues)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusWatching || watch.StaleNotified {
		t.Fatalf("watch before stale threshold = %+v", watch)
	}
}

func TestSkillOptReviewWatcherStalesAfterUnrelatedComment(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(-time.Minute))
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "Can someone explain what item-002 means?", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer

	if _, err := pollSkillOptReviewWatches(context.Background(), config.PathsForHome(home), store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}

	if len(fake.postedComments) != 1 || !strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchStaleMarker) {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusStaleNotified || !watch.StaleNotified {
		t.Fatalf("watch after unrelated stale comment = %+v", watch)
	}
}

func TestSkillOptReviewWatcherImportsFeedbackInsteadOfStaleNotice(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(-time.Minute))
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer

	if _, err := pollSkillOptReviewWatches(context.Background(), config.PathsForHome(home), store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}

	if len(fake.postedComments) != 1 || !strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchSuccessMarker) {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchStaleMarker) {
		t.Fatalf("unexpected stale notice = %q", fake.postedComments[0].Body)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusClosed || watch.StaleNotified {
		t.Fatalf("watch after valid stale-aged feedback = %+v", watch)
	}
}

func TestSkillOptReviewWatcherKeepsImportedWhenTrainReviewLockBusy(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	release, _, err := acquireSkillOptTrainReviewLock(context.Background(), store, "watcher-train", "watcher-train-001")
	if err != nil {
		t.Fatalf("acquireSkillOptTrainReviewLock returned error: %v", err)
	}
	defer func() {
		_ = release(context.Background())
	}()
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer
	if _, err := pollSkillOptReviewWatches(context.Background(), config.PathsForHome(home), store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 ||
		!strings.Contains(fake.postedComments[0].Body, "already active") ||
		!strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchSuccessMarker) {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if len(fake.closedIssues) != 0 {
		t.Fatalf("closed issues = %+v", fake.closedIssues)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusImported {
		t.Fatalf("watch status = %s, want imported while lock is active", watch.Status)
	}
	iteration, err := store.GetSkillOptTrainIteration(context.Background(), "watcher-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateReviewPublished {
		t.Fatalf("iteration state = %s, want review_published while lock is active", iteration.State)
	}
}

func TestSkillOptReviewWatcherRetriesImportedAckAndClose(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
		closeIssueErr: errors.New("temporary close failure"),
	}
	paths := config.PathsForHome(home)
	var stdout bytes.Buffer
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err == nil || !strings.Contains(err.Error(), "temporary close failure") {
		t.Fatalf("first poll error = %v, want close failure", err)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusImported {
		t.Fatalf("watch status after failed close = %s, want imported", watch.Status)
	}
	if len(fake.postedComments) != 1 || len(fake.closedIssues) != 0 {
		t.Fatalf("posted=%+v closed=%+v", fake.postedComments, fake.closedIssues)
	}
	fake.comments[67] = append(fake.comments[67], github.IssueComment{ID: 101, Body: fake.postedComments[0].Body, Author: "gitmoot"})
	fake.closeIssueErr = nil
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("second pollSkillOptReviewWatches returned error: %v", err)
	}
	watch, err = store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch after retry returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusClosed {
		t.Fatalf("watch status after retry = %s, want closed", watch.Status)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments after retry = %+v, want no duplicate success comment", fake.postedComments)
	}
	if len(fake.closedIssues) != 1 {
		t.Fatalf("closed issues = %+v", fake.closedIssues)
	}
}

func TestSkillOptReviewWatcherCloseDecisionKeepsBlockedReviewOpen(t *testing.T) {
	if skillOptReviewWatchShouldCloseAfterContinuation(skillOptReviewWatchContinuation{Phase: skillopt.TrainStateReviewPublished}) {
		t.Fatal("review_published continuation should keep the review issue open")
	}
	if !skillOptReviewWatchShouldCloseAfterContinuation(skillOptReviewWatchContinuation{Phase: skillopt.TrainStateFeedbackSynced}) {
		t.Fatal("advanced continuation should close the review issue")
	}
	if skillOptReviewWatchShouldCloseAfterContinuation(skillOptReviewWatchContinuation{Phase: skillopt.TrainStateReviewPublished, Busy: true, Err: errSkillOptTrainReviewBusy}) {
		t.Fatal("busy continuation should keep the review issue open so the watcher can retry")
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

func TestSkillOptReviewCreateAndStatus(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt help exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gitmoot skillopt review create") || !strings.Contains(stdout.String(), "gitmoot skillopt review status") {
		t.Fatalf("skillopt help missing review commands:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "gitmoot skillopt train continue --session <id> [--backend codex]") {
		t.Fatalf("skillopt help missing train backend preset:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "planner-ab-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review create exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created review planner-ab-1 for "+installed.VersionID) {
		t.Fatalf("review create stdout = %q", stdout.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after create returned error: %v", err)
	}
	run, err := store.GetEvalRun(context.Background(), "planner-ab-1")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.TemplateID != "planner" || run.TemplateVersionID != installed.VersionID || run.TargetRepo != "owner/repo" || run.State != "review" {
		t.Fatalf("eval run = %+v", run)
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
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "baseline",
		Hash:      baselineBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: baselineBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "candidate",
		Hash:      candidateBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: candidateBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               "planner-ab-1",
		ItemID:              "item-001",
		Title:               "README planning task",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     "planner-ab-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reasoning: "More concrete.",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after seed returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"run: planner-ab-1",
		"template: planner",
		"template_version: " + installed.VersionID,
		"repo: owner/repo",
		"state: review",
		"items: 1",
		"feedback: 1",
		"packet_blockers: 0",
		"training_blockers: 0",
		"ready_for_packet: true",
		"ready_for_training: true",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewItemAddStoresArtifacts(t *testing.T) {
	home := t.TempDir()
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
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "planner-ab-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review create exit code = %d, stderr=%s", code, stderr.String())
	}
	inputDir := t.TempDir()
	baselinePath := filepath.Join(inputDir, "baseline.md")
	candidatePath := filepath.Join(inputDir, "candidate.md")
	if err := os.WriteFile(baselinePath, []byte("# Baseline\n\nShort plan.\n"), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	if err := os.WriteFile(candidatePath, []byte("# Candidate\n\nShort plan with risks.\n"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-ab-1",
		"--item", "item-001",
		"--title", "README planning task",
		"--baseline", baselinePath,
		"--candidate", candidatePath,
		"--metadata-json", `{"path":"README.md"}`,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added review item item-001 to planner-ab-1") {
		t.Fatalf("review item add stdout = %q", stdout.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after item add returned error: %v", err)
	}
	items, err := store.ListEvalReviewItems(context.Background(), "planner-ab-1")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("review item count = %d, want 1", len(items))
	}
	item := items[0]
	if item.Title != "README planning task" || item.BaselineArtifactID != "planner-ab-1/item-001/baseline" || item.CandidateArtifactID != "planner-ab-1/item-001/candidate" || item.MetadataJSON != `{"path":"README.md"}` {
		t.Fatalf("review item = %+v", item)
	}
	baseline, err := store.GetEvalArtifact(context.Background(), item.BaselineArtifactID)
	if err != nil {
		t.Fatalf("GetEvalArtifact baseline returned error: %v", err)
	}
	candidate, err := store.GetEvalArtifact(context.Background(), item.CandidateArtifactID)
	if err != nil {
		t.Fatalf("GetEvalArtifact candidate returned error: %v", err)
	}
	if baseline.MediaType != "text/markdown" || candidate.MediaType != "text/markdown" || baseline.Driver != "text" || candidate.Driver != "text" {
		t.Fatalf("artifacts = %+v %+v", baseline, candidate)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after artifact check returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 1",
		"feedback: 0",
		"packet_blockers: 0",
		"training_blockers: 1",
		"ready_for_packet: true",
		"ready_for_training: false",
		"training_blocker: item item-001 has no imported feedback",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}

	packetDir := filepath.Join(t.TempDir(), "packet")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "export", "--home", home, "--run", "planner-ab-1", "--output", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown export exit code = %d, stderr=%s", code, stderr.String())
	}
	feedbackYAML, err := os.ReadFile(filepath.Join(packetDir, "feedback.yml"))
	if err != nil {
		t.Fatalf("read feedback.yml: %v", err)
	}
	if !strings.Contains(string(feedbackYAML), "item_id: item-001") || !strings.Contains(string(feedbackYAML), "choice:") {
		t.Fatalf("feedback.yml = %s", string(feedbackYAML))
	}
}

func TestSkillOptRankedReviewItemAddAndMarkdownExport(t *testing.T) {
	home := t.TempDir()
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
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "planner-explore-1",
		"--mode", "explore",
		"--exploration-level", "high",
		"--options", "4",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review create exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open before preseeded review item returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:        "planner-explore-1",
		ItemID:       "item-001",
		Title:        "Preplanned landing page",
		MetadataJSON: `{"brief":"Preserve this brief","output_type":"vue"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem preseed returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after preseeded review item returned error: %v", err)
	}

	inputDir := t.TempDir()
	optionPaths := map[string]string{}
	for _, label := range []string{"a", "b", "c", "d"} {
		path := filepath.Join(inputDir, label+".md")
		if err := os.WriteFile(path, []byte("# Option "+strings.ToUpper(label)+"\n\nReview content.\n"), 0o644); err != nil {
			t.Fatalf("write option %s: %v", label, err)
		}
		optionPaths[label] = path
	}
	missingOptionArgs := []string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-explore-1",
		"--item", "item-001",
		"--title", "Landing page",
		"--option", "a=" + optionPaths["a"],
		"--option", "b=" + optionPaths["b"],
		"--option", "c=" + optionPaths["c"],
		"--option", "d=" + filepath.Join(inputDir, "missing.md"),
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(missingOptionArgs, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "missing.md") {
		t.Fatalf("ranked item add with missing option: code=%d stderr=%s", code, stderr.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after failed ranked item add returned error: %v", err)
	}
	options, err := store.ListEvalReviewOptions(context.Background(), "planner-explore-1", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions after failed add returned error: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("failed ranked item add persisted partial options: %+v", options)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after failed ranked item add returned error: %v", err)
	}
	rankedItemArgs := []string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-explore-1",
		"--item", "item-001",
		"--title", "Landing page",
		"--option", "a=" + optionPaths["a"],
		"--option", "b=" + optionPaths["b"],
		"--option", "c=" + optionPaths["c"],
		"--option", "d=" + optionPaths["d"],
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(rankedItemArgs, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(rankedItemArgs, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add retry exit code = %d, stderr=%s", code, stderr.String())
	}
	replacementPath := filepath.Join(inputDir, "e.md")
	if err := os.WriteFile(replacementPath, []byte("# Option E\n\nReplacement content.\n"), 0o644); err != nil {
		t.Fatalf("write replacement option: %v", err)
	}
	rankedReplacementArgs := []string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-explore-1",
		"--item", "item-001",
		"--title", "Landing page",
		"--option", "a=" + optionPaths["a"],
		"--option", "b=" + optionPaths["b"],
		"--option", "c=" + optionPaths["c"],
		"--option", "e=" + replacementPath,
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(rankedReplacementArgs, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add replacement exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-explore-1",
		"--item", "item-ab",
		"--baseline", optionPaths["a"],
		"--candidate", optionPaths["b"],
	}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "is ranked mode; use repeated --option") {
		t.Fatalf("ranked run accepted A/B artifacts: code=%d stderr=%s", code, stderr.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after ranked item add returned error: %v", err)
	}
	run, err := store.GetEvalRun(context.Background(), "planner-explore-1")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.Mode != db.EvalRunModeExplore || run.ExplorationLevel != db.ExplorationLevelHigh || run.OptionsCount != 4 {
		t.Fatalf("run = %+v", run)
	}
	options, err = store.ListEvalReviewOptions(context.Background(), "planner-explore-1", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 4 || options[0].Label != "a" || options[3].Label != "e" || !strings.Contains(options[0].MetadataJSON, optionPaths["a"]) {
		t.Fatalf("options = %+v", options)
	}
	items, err := store.ListEvalReviewItems(context.Background(), "planner-explore-1")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Landing page" || !strings.Contains(items[0].MetadataJSON, "Preserve this brief") {
		t.Fatalf("item metadata was not preserved after option add: %+v", items)
	}
	for _, option := range options {
		if option.Label == "d" {
			t.Fatalf("replacement left stale option d: %+v", options)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after ranked checks returned error: %v", err)
	}

	packetDir := filepath.Join(t.TempDir(), "packet")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "export", "--home", home, "--run", "planner-explore-1", "--output", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown export exit code = %d, stderr=%s", code, stderr.String())
	}
	index, err := os.ReadFile(filepath.Join(packetDir, "index.md"))
	if err != nil {
		t.Fatalf("read index.md: %v", err)
	}
	if !strings.Contains(string(index), "ranking every option") || !strings.Contains(string(index), "A > B > C > E") {
		t.Fatalf("index.md = %s", string(index))
	}
	itemFiles, err := os.ReadDir(filepath.Join(packetDir, "items"))
	if err != nil {
		t.Fatalf("read items dir: %v", err)
	}
	if len(itemFiles) != 1 {
		t.Fatalf("item files = %d, want 1", len(itemFiles))
	}
	itemContent, err := os.ReadFile(filepath.Join(packetDir, "items", itemFiles[0].Name()))
	if err != nil {
		t.Fatalf("read item markdown: %v", err)
	}
	if !strings.Contains(string(itemContent), "| Option | Artifact | Reference |") || !strings.Contains(string(itemContent), "Option C") {
		t.Fatalf("item markdown = %s", string(itemContent))
	}
	feedbackYAML, err := os.ReadFile(filepath.Join(packetDir, "feedback.yml"))
	if err != nil {
		t.Fatalf("read feedback.yml: %v", err)
	}
	if !strings.Contains(string(feedbackYAML), "ranking:") ||
		!strings.Contains(string(feedbackYAML), "<replace with ranked option labels, best to worst>") ||
		strings.Contains(string(feedbackYAML), "- C") {
		t.Fatalf("feedback.yml = %s", string(feedbackYAML))
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "planner-validate-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("validate review create exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-validate-1",
		"--item", "item-ranked",
		"--option", "a=" + optionPaths["a"],
		"--option", "b=" + optionPaths["b"],
	}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "is validate/A/B mode; use --baseline and --candidate") {
		t.Fatalf("validate run accepted ranked options: code=%d stderr=%s", code, stderr.String())
	}
}

func TestSkillOptHumanFeedbackTrialSmoke(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "trial-smoke",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review create exit code = %d, stderr=%s", code, stderr.String())
	}

	inputDir := t.TempDir()
	baselinePath := filepath.Join(inputDir, "baseline.md")
	candidatePath := filepath.Join(inputDir, "candidate.md")
	if err := os.WriteFile(baselinePath, []byte("# Baseline\n\nPlan only the edit.\n"), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	if err := os.WriteFile(candidatePath, []byte("# Candidate\n\nPlan the edit, test, and rollback notes.\n"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "trial-smoke",
		"--item", "item-001",
		"--title", "README planning task",
		"--baseline", baselinePath,
		"--candidate", candidatePath,
		"--metadata-json", `{"path":"README.md"}`,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add exit code = %d, stderr=%s", code, stderr.String())
	}

	packetDir := filepath.Join(t.TempDir(), "packet")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "export", "--home", home, "--run", "trial-smoke", "--output", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown export exit code = %d, stderr=%s", code, stderr.String())
	}
	feedbackYAML := `run_id: trial-smoke
reviewer: jerry
items:
  - item_id: item-001
    choice: a
    reasoning: More complete execution plan.
`
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackYAML), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "import", "--home", home, "--packet", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 1 feedback events") {
		t.Fatalf("feedback import stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "review", "status", "--home", home, "--run", "trial-smoke"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 1",
		"feedback: 1",
		"ready_for_packet: true",
		"ready_for_training: true",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}

	trainingPath := filepath.Join(t.TempDir(), "training.json")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "export", "--home", home, "--run", "trial-smoke", "--output", trainingPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt export exit code = %d, stderr=%s", code, stderr.String())
	}
	trainingContent, err := os.ReadFile(trainingPath)
	if err != nil {
		t.Fatalf("read training package: %v", err)
	}
	var training skillopt.TrainingPackage
	if err := json.Unmarshal(trainingContent, &training); err != nil {
		t.Fatalf("decode training package: %v\n%s", err, string(trainingContent))
	}
	if training.EvalRun.ID != "trial-smoke" || len(training.Items) != 1 || len(training.Artifacts) != 2 || len(training.FeedbackEvents) != 1 {
		t.Fatalf("training package = %+v", training)
	}
	if training.FeedbackEvents[0].ItemID != "item-001" || training.FeedbackEvents[0].Choice != "b" || training.FeedbackEvents[0].Reasoning != "More complete execution plan." {
		t.Fatalf("training feedback = %+v", training.FeedbackEvents)
	}

	artifactDir := t.TempDir()
	diffContent := []byte("candidate diff\n")
	diffHash := artifact.ContentHash(diffContent)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), diffContent, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	diffSize := int64(len(diffContent))
	candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, "Plan with test and rollback notes.")
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []skillopt.CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      diffHash,
		MediaType: "text/markdown",
		Driver:    "text",
		SizeBytes: &diffSize,
	}}
	candidatePackagePath := filepath.Join(t.TempDir(), "candidate.json")
	encodedCandidate, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate package: %v", err)
	}
	if err := os.WriteFile(candidatePackagePath, encodedCandidate, 0o644); err != nil {
		t.Fatalf("write candidate package: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "import", "--home", home, "--file", candidatePackagePath, "--artifact-dir", artifactDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported pending candidate planner@v2") {
		t.Fatalf("candidate import stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "show", "--home", home, "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: pending") || !strings.Contains(stdout.String(), "diff_artifact: candidate-diff") {
		t.Fatalf("candidate show stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "reject", "--home", home, "planner@v2", "--reason", "trial smoke"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate reject exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "rejected candidate planner@v2") {
		t.Fatalf("candidate reject stdout = %q", stdout.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after trial smoke returned error: %v", err)
	}
	defer store.Close()
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if current.VersionID != installed.VersionID {
		t.Fatalf("current version = %q, want %q", current.VersionID, installed.VersionID)
	}
	rejected, err := store.GetAgentTemplateVersionByID(context.Background(), "planner@v2")
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if rejected.State != "rejected" {
		t.Fatalf("rejected candidate = %+v", rejected)
	}
	review, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v2")
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	if review.DecisionReason != "trial smoke" {
		t.Fatalf("candidate review = %+v", review)
	}
}

func TestSkillOptReviewItemAddRejectsInvalidInputs(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ab-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	inputDir := t.TempDir()
	baselinePath := filepath.Join(inputDir, "baseline.md")
	candidatePath := filepath.Join(inputDir, "candidate.md")
	binaryPath := filepath.Join(inputDir, "candidate.bin")
	if err := os.WriteFile(baselinePath, []byte("baseline\n"), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	if err := os.WriteFile(candidatePath, []byte("candidate\n"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte{0xff, 0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name: "missing run",
			args: []string{
				"skillopt", "review", "item", "add",
				"--home", home,
				"--run", "missing-run",
				"--item", "item-001",
				"--baseline", baselinePath,
				"--candidate", candidatePath,
			},
			wantErr: "review run missing-run not found",
		},
		{
			name: "invalid metadata",
			args: []string{
				"skillopt", "review", "item", "add",
				"--home", home,
				"--run", "planner-ab-1",
				"--item", "item-001",
				"--baseline", baselinePath,
				"--candidate", candidatePath,
				"--metadata-json", "{not-json",
			},
			wantErr: "metadata-json:",
		},
		{
			name: "binary without media type",
			args: []string{
				"skillopt", "review", "item", "add",
				"--home", home,
				"--run", "planner-ab-1",
				"--item", "item-001",
				"--baseline", baselinePath,
				"--candidate", binaryPath,
			},
			wantErr: "binary content requires --media-type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tt.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("exit code = 0, stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestSkillOptReviewStatusRequiresExportableArtifacts(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ab-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               "planner-ab-1",
		ItemID:              "item-001",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     "planner-ab-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 1",
		"feedback: 1",
		"packet_blockers: 2",
		"training_blockers: 1",
		"ready_for_packet: false",
		"ready_for_training: false",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewStatusShowsRankedPairwisePreferences(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	optionLabels := []string{"a", "b", "c", "d"}
	for _, label := range optionLabels {
		blob, err := blobStore.Put([]byte("option " + label))
		if err != nil {
			t.Fatalf("Put option %s returned error: %v", label, err)
		}
		artifactID := "option-" + label
		if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
			ID:        artifactID,
			Hash:      blob.Hash,
			MediaType: "text/markdown",
			SizeBytes: blob.Size,
			Driver:    "text",
		}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", artifactID, err)
		}
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ranked-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		Mode:              db.EvalRunModeExplore,
		ExplorationLevel:  db.ExplorationLevelHigh,
		OptionsCount:      4,
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:  "planner-ranked-1",
		ItemID: "item-001",
		Title:  "Landing page",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range optionLabels {
		if err := store.UpsertEvalReviewOption(context.Background(), db.EvalReviewOption{
			RunID:      "planner-ranked-1",
			ItemID:     "item-001",
			Label:      label,
			ArtifactID: "option-" + label,
		}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	ranking, err := json.Marshal([]string{"c", "a", "d", "b"})
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(context.Background(), db.RankedFeedbackEvent{
		RunID:       "planner-ranked-1",
		ItemID:      "item-001",
		RankingJSON: string(ranking),
		Winner:      "c",
		Reasoning:   "C explains the product most clearly.",
		Reviewer:    "jerry",
		Source:      "github",
		SourceURL:   "https://github.com/owner/repo/issues/1#issuecomment-1",
		CreatedAt:   "2026-06-02T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent returned error: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(context.Background(), db.RankedFeedbackEvent{
		RunID:       "planner-ranked-1",
		ItemID:      "item-001",
		RankingJSON: string(ranking),
		Winner:      "c",
		Reasoning:   "C is still strongest.",
		Reviewer:    "jerry",
		Source:      "github",
		SourceURL:   "https://github.com/owner/repo/issues/1#issuecomment-2",
		CreatedAt:   "2026-06-02T11:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent second returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ranked-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 1",
		"feedback: 2",
		"pairwise_preferences: 12",
		"mode: explore",
		"exploration_level: high",
		"ranking_stability: c 2/2",
		"recommended_next_mode: refine",
		"recommendation: recommend refine",
		"packet_blockers: 0",
		"training_blockers: 0",
		"ready_for_packet: true",
		"ready_for_training: true",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewStatusRequiresExportableMetadata(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
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
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "baseline",
		Hash:      baselineBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: baselineBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "candidate",
		Hash:      candidateBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: candidateBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ab-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               "planner-ab-1",
		ItemID:              "item-001",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
		MetadataJSON:        `{not-json`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     "planner-ab-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"packet_blockers: 0",
		"training_blockers: 1",
		"ready_for_packet: true",
		"ready_for_training: false",
		"training_blocker: training export failed: eval item item-001 metadata_json:",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewStatusRequiresFeedbackForEveryItem(t *testing.T) {
	home := t.TempDir()
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
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	for _, fixture := range []struct {
		id      string
		content string
	}{
		{id: "item-001-baseline", content: "item 1 baseline"},
		{id: "item-001-candidate", content: "item 1 candidate"},
		{id: "item-002-baseline", content: "item 2 baseline"},
		{id: "item-002-candidate", content: "item 2 candidate"},
	} {
		blob, err := blobStore.Put([]byte(fixture.content))
		if err != nil {
			t.Fatalf("Put %s returned error: %v", fixture.id, err)
		}
		if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
			ID:        fixture.id,
			Hash:      blob.Hash,
			MediaType: "text/markdown",
			SizeBytes: blob.Size,
			Driver:    "text",
		}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", fixture.id, err)
		}
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ab-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	for _, item := range []db.EvalReviewItem{
		{RunID: "planner-ab-1", ItemID: "item-001", BaselineArtifactID: "item-001-baseline", CandidateArtifactID: "item-001-candidate"},
		{RunID: "planner-ab-1", ItemID: "item-002", BaselineArtifactID: "item-002-baseline", CandidateArtifactID: "item-002-candidate"},
	} {
		if err := store.UpsertEvalReviewItem(context.Background(), item); err != nil {
			t.Fatalf("UpsertEvalReviewItem %s returned error: %v", item.ItemID, err)
		}
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     "planner-ab-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 2",
		"feedback: 1",
		"packet_blockers: 0",
		"training_blockers: 1",
		"ready_for_packet: true",
		"ready_for_training: false",
		"training_blocker: item item-002 has no imported feedback",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewCreateRejectsUnknownTemplate(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "missing-template",
		"--repo", "owner/repo",
		"--run", "planner-ab-1",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("skillopt review create exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent template missing-template is not installed") {
		t.Fatalf("review create stderr = %q", stderr.String())
	}
}

func TestSkillOptReviewStatusRejectsMissingRun(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "missing-run"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("skillopt review status exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "review run missing-run not found") {
		t.Fatalf("review status stderr = %q", stderr.String())
	}
}

type skillOptFakeGitHub struct {
	github.NoopClient

	createdIssue       github.CreateIssueInput
	postedComments     []skillOptPostedGitHubComment
	upsertedFiles      []skillOptUpsertedGitHubFile
	closedIssues       []skillOptClosedGitHubIssue
	listedComments     []skillOptListedGitHubComments
	comments           map[int64][]github.IssueComment
	createIssueErr     error
	postCommentErr     error
	upsertFileErr      error
	closeIssueErr      error
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
	metadata := skillOptTrainStartMetadata("Train planner outputs from human feedback.", db.EvalRunModeValidate, db.ExplorationLevelLow, 2, "hard_then_soft", nil, nil, previewPolicy)
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
	beforeRun                func(dir string, args []string) error
	calls                    []skillOptTrainFakeOptimizerCall
}

type skillOptTrainFakeOptimizerCall struct {
	dir     string
	command string
	args    []string
}

func (r *skillOptTrainFakeOptimizerRunner) Run(_ context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	r.calls = append(r.calls, skillOptTrainFakeOptimizerCall{dir: dir, command: command, args: append([]string{}, args...)})
	result := subprocess.Result{Command: command, Args: args}
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
	if strings.TrimSpace(r.lookPathValue) != "" {
		return r.lookPathValue, nil
	}
	return "/fake/bin/" + file, nil
}

type skillOptTrainFakePreviewRunner struct {
	failGitCommit bool
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
	return subprocess.ExecRunner{}.Run(ctx, dir, command, args...)
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
