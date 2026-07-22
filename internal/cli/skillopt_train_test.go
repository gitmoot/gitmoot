package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func TestSkillOptTrainStartRequiresWorkspaceRepo(t *testing.T) {
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
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	itemsPath := filepath.Join(t.TempDir(), "items.yml")
	if err := os.WriteFile(itemsPath, []byte(`items:
  - item_id: hero
    title: Hero
    brief: Design a hero section.
  - item_id: proof
    title: Proof
    brief: Design a social proof section.
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
		"--yes",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("train start without workspace-repo exit code = %d, want 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires --workspace-repo") {
		t.Fatalf("missing workspace-repo error = %q", stderr.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after failed start returned error: %v", err)
	}
	defer store.Close()
	if _, err := store.GetSkillOptTrainSession(context.Background(), "owner-product"); err == nil {
		t.Fatalf("failed start created a train session")
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
		"--workspace-repo", "owner/workspace",
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
		"--workspace-repo", "owner/workspace",
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
		"--workspace-repo", "owner/workspace",
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
		"--workspace-repo", "owner/workspace",
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
		"--workspace-repo", "owner/workspace",
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

func TestSkillOptTrainStartLoadsInitConfig(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "config-start",
		"--template", "planner",
		"--review-repo", "jerryfane/gitmoot-previews",
		"--task-kind", "design",
		"--artifact-kind", "vue",
		"--preview", "vue",
		"--request", "Improve landing page previews.",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	scaffoldDir := filepath.Join(workspace, ".gitmoot", "skillopt", "config-start")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--workspace-repo", "owner/workspace",
		"--home", home,
		"--config", filepath.Join(scaffoldDir, "config.toml"),
		"--session", "config-start-session",
		"--yes",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train start --config exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	session, err := store.GetSkillOptTrainSession(context.Background(), "config-start-session")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	if session.TemplateID != "planner" || session.TargetRepo != "jerryfane/gitmoot-previews" || session.PreviewRepo != "jerryfane/gitmoot-previews" {
		t.Fatalf("session = %+v", session)
	}
	if skillOptMetadataString(session.MetadataJSON, "preview", "mode") != skillopt.TrainPreviewModeRequired || skillOptMetadataString(session.MetadataJSON, "preview", "renderer") != skillopt.TrainPreviewRendererVueVite || skillOptMetadataString(session.MetadataJSON, "preview", "publisher") != skillopt.TrainPreviewPublisherGitHubPages {
		t.Fatalf("preview metadata = %s", session.MetadataJSON)
	}
	if skillOptMetadataString(session.MetadataJSON, "optimizer_defaults", "backend") != "codex" || skillOptMetadataString(session.MetadataJSON, "optimizer_defaults", "skill_update_mode") != skillopt.TrainInitSkillUpdateModeFullRewrite {
		t.Fatalf("optimizer defaults metadata = %s", session.MetadataJSON)
	}
	var defaults skillOptTrainOptimizerRequest
	applySkillOptTrainOptimizerDefaultsFromMetadata(session.MetadataJSON, &defaults)
	if defaults.Backend != "codex" || defaults.OptimizerViews != 4 || !defaults.OptimizerViewsSet || defaults.GateRejectRetryBudget != 3 || !defaults.GateRejectRetryBudgetSet || defaults.RetryOptimizerViews != "auto" || !defaults.RetryOptimizerViewsSet {
		t.Fatalf("applied optimizer defaults = %+v", defaults)
	}
	run, err := store.GetEvalRun(context.Background(), "config-start-session-review-001")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.OptionsCount != 4 || run.ExplorationLevel != db.ExplorationLevelHigh {
		t.Fatalf("eval run options/exploration = %d/%q, want 4/%q", run.OptionsCount, run.ExplorationLevel, db.ExplorationLevelHigh)
	}
	items, err := store.ListEvalReviewItems(context.Background(), "config-start-session-review-001")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("review items = %d, want 2", len(items))
	}
}

func TestSkillOptTrainStartConfigPreviewModeOverrideDisablesVue(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "config-preview-override",
		"--template", "planner",
		"--review-repo", "jerryfane/gitmoot-previews",
		"--artifact-kind", "vue",
		"--preview", "vue",
		"--request", "Improve landing page previews.",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	scaffoldDir := filepath.Join(workspace, ".gitmoot", "skillopt", "config-preview-override")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--workspace-repo", "owner/workspace",
		"--home", home,
		"--config", filepath.Join(scaffoldDir, "config.toml"),
		"--preview-mode", "none",
		"--session", "config-preview-override-session",
		"--yes",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train start --config --preview-mode none exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	session, err := store.GetSkillOptTrainSession(context.Background(), "config-preview-override-session")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	if session.PreviewRepo != "" {
		t.Fatalf("preview repo = %q, want empty", session.PreviewRepo)
	}
	if skillOptMetadataString(session.MetadataJSON, "preview", "mode") != skillopt.TrainPreviewModeNone || skillOptMetadataString(session.MetadataJSON, "preview", "renderer") != skillopt.TrainPreviewRendererNone || skillOptMetadataString(session.MetadataJSON, "preview", "publisher") != skillopt.TrainPreviewPublisherNone {
		t.Fatalf("preview metadata = %s", session.MetadataJSON)
	}
}

func TestSkillOptTrainStartConfigPreviewFlagsOverrideNone(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "config-preview-enable",
		"--template", "planner",
		"--review-repo", "gitmoot/gitmoot",
		"--artifact-kind", "text",
		"--preview", "none",
		"--request", "Improve planner summaries.",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	scaffoldDir := filepath.Join(workspace, ".gitmoot", "skillopt", "config-preview-enable")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--workspace-repo", "owner/workspace",
		"--home", home,
		"--config", filepath.Join(scaffoldDir, "config.toml"),
		"--preview-repo", "jerryfane/gitmoot-previews",
		"--session", "config-preview-enable-session",
		"--yes",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train start --config --preview-repo exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	session, err := store.GetSkillOptTrainSession(context.Background(), "config-preview-enable-session")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	if session.PreviewRepo != "jerryfane/gitmoot-previews" {
		t.Fatalf("preview repo = %q, want jerryfane/gitmoot-previews", session.PreviewRepo)
	}
	if skillOptMetadataString(session.MetadataJSON, "preview", "mode") != skillopt.TrainPreviewModeRequired || skillOptMetadataString(session.MetadataJSON, "preview", "renderer") != skillopt.TrainPreviewRendererVueVite || skillOptMetadataString(session.MetadataJSON, "preview", "publisher") != skillopt.TrainPreviewPublisherGitHubPages {
		t.Fatalf("preview metadata = %s", session.MetadataJSON)
	}
}

func TestSkillOptTrainStartConfigRepoOverrideDrivesVuePreviewRepo(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	restore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restore()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "init",
		"--home", home,
		"--name", "config-repo-override",
		"--template", "planner",
		"--review-repo", "jerryfane/gitmoot-previews",
		"--artifact-kind", "vue",
		"--preview", "vue",
		"--request", "Improve landing page previews.",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train init exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	scaffoldDir := filepath.Join(workspace, ".gitmoot", "skillopt", "config-repo-override")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "start",
		"--workspace-repo", "owner/workspace",
		"--home", home,
		"--config", filepath.Join(scaffoldDir, "config.toml"),
		"--repo", "gitmoot/gitmoot",
		"--session", "config-repo-override-session",
		"--yes",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train start --config --repo exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open store returned error: %v", err)
	}
	defer store.Close()
	session, err := store.GetSkillOptTrainSession(context.Background(), "config-repo-override-session")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	if session.TargetRepo != "gitmoot/gitmoot" || session.PreviewRepo != "gitmoot/gitmoot" {
		t.Fatalf("session repos = target %q preview %q", session.TargetRepo, session.PreviewRepo)
	}
	if skillOptMetadataString(session.MetadataJSON, "review", "expected_repo") != "gitmoot/gitmoot" {
		t.Fatalf("expected review repo metadata = %s", session.MetadataJSON)
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

func TestSkillOptTrainContinueFromGitHubURL(t *testing.T) {
	cases := []struct {
		phase string
		url   string
		want  string
	}{
		{"review_published", "https://github.com/o/r/issues/1", "https://github.com/o/r/issues/1"},
		{"candidate_review_published", "https://github.com/o/r/issues/2", "https://github.com/o/r/issues/2"},
		{"items_ready", "https://github.com/o/r/issues/1", ""},
		{"optimizer_running", "https://github.com/o/r/issues/1", ""},
		{"review_published", "  ", ""},
	}
	for _, tc := range cases {
		if got := skillOptTrainContinueFromGitHubURL(tc.phase, tc.url); got != tc.want {
			t.Fatalf("skillOptTrainContinueFromGitHubURL(%q,%q) = %q, want %q", tc.phase, tc.url, got, tc.want)
		}
	}
}

func TestSkillOptTrainStatusSnapshotPrintsContinueFromGitHub(t *testing.T) {
	// review_published with an issue → the line is present.
	var present bytes.Buffer
	printSkillOptTrainStatusSnapshot(&present, skillOptTrainStatusSnapshot{
		SessionID:    "s",
		CurrentPhase: "review_published",
		IssueURL:     "https://github.com/o/r/issues/7",
	}, false)
	if !strings.Contains(present.String(), "continue_from_github: https://github.com/o/r/issues/7") {
		t.Fatalf("expected continue_from_github line:\n%s", present.String())
	}

	// A non-review phase → the line is absent (byte-stability for other phases).
	var absent bytes.Buffer
	printSkillOptTrainStatusSnapshot(&absent, skillOptTrainStatusSnapshot{
		SessionID:    "s",
		CurrentPhase: "items_ready",
		IssueURL:     "https://github.com/o/r/issues/7",
	}, false)
	if strings.Contains(absent.String(), "continue_from_github:") {
		t.Fatalf("continue_from_github must not appear at items_ready:\n%s", absent.String())
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
		"--workspace-repo", "owner/workspace",
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
		"--workspace-repo", "owner/workspace",
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
		"--workspace-repo", "owner/workspace",
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

func TestSkillOptTrainLockPhase(t *testing.T) {
	cases := []struct {
		name   string
		status string
		lock   string
		want   string
		ok     bool
	}{
		{name: "generation active", lock: "generation", status: "active", want: "generating_options", ok: true},
		{name: "generation heartbeat stale", lock: "generation", status: "active_expired_heartbeat", want: "generating_options_heartbeat_stale", ok: true},
		{name: "generation stale", lock: "generation", status: "stale", want: "blocked_stale_lock", ok: true},
		{name: "optimizer active", lock: "optimizer", status: "active", want: "optimizer_running", ok: true},
		{name: "review lock ignored", lock: "review", status: "active", want: "", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := skillOptTrainLockPhase([]skillOptTrainStatusLock{{Name: tc.lock, Status: tc.status}})
			if got != tc.want || ok != tc.ok {
				t.Fatalf("skillOptTrainLockPhase = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSkillOptTrainGeneratingOptionsPhase(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	ctx := context.Background()
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainSession(ctx, db.SkillOptTrainSession{ID: "gen-phase", TemplateID: "planner", TargetRepo: "owner/repo", State: skillopt.TrainStateItemsReady}); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{ID: "gen-phase-review-001", TemplateID: "planner", TargetRepo: "owner/repo", State: "review", Mode: db.EvalRunModeExplore}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, db.SkillOptTrainIteration{ID: "gen-phase-001", SessionID: "gen-phase", EvalRunID: "gen-phase-review-001", Mode: db.EvalRunModeExplore, State: skillopt.TrainStateItemsReady}); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration returned error: %v", err)
	}
	now := time.Now().UTC()
	key := skillOptTrainGenerationLockKey("gen-phase", "gen-phase-001")
	if _, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  "gen-job",
		OwnerToken:  "gen-token",
		OwnerPID:    int64(os.Getpid()),
		ExpiresAt:   now.Add(time.Hour).Format(time.RFC3339Nano),
	}, now); err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "status", "--home", home, "--session", "gen-phase", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train status exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "\"status_phase\": \"generating_options\"") {
		t.Fatalf("train status did not report generating_options:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"dashboard", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit = %d, stderr=%s", code, stderr.String())
	}
	var snapshot dashboardSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	found := false
	for _, train := range snapshot.TrainSessions {
		if train.ID == "gen-phase" {
			found = true
			if train.Phase != "generating_options" {
				t.Fatalf("dashboard train phase = %q, want generating_options", train.Phase)
			}
		}
	}
	if !found {
		t.Fatalf("dashboard did not include the train session: %+v", snapshot.TrainSessions)
	}
}
