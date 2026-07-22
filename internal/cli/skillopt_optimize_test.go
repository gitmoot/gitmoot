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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func TestBuildOptimizerCommandDefaultsCodexModel(t *testing.T) {
	prev := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = &skillOptTrainFakeOptimizerRunner{}
	defer func() { skillOptTrainOptimizerRunner = prev }()

	// A configured codex model the default should pick up.
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"gpt-5.5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	build := func(req skillOptTrainOptimizerRequest) []string {
		req.SkillOptBin = "gitmoot-skillopt"
		_, args, err := buildSkillOptTrainOptimizerCommand(db.SkillOptTrainIteration{}, req, skillOptTrainOptimizerPaths{})
		if err != nil {
			t.Fatalf("buildSkillOptTrainOptimizerCommand: %v", err)
		}
		return args
	}

	// codex preset, empty model → optimizer AND target default to the codex model.
	// The preset resolves the optimizer to "codex" and the target to "codex_exec";
	// both pass an explicit model to codex, so both need the override. The
	// evaluator inherits the optimizer model in gitmoot-skillopt (no flag emitted).
	args := build(skillOptTrainOptimizerRequest{Backend: "codex"})
	for _, flag := range []string{"--optimizer-model", "--target-model"} {
		if got := argValue(args, flag); got != "gpt-5.5" {
			t.Fatalf("codex+empty: %s = %q, want gpt-5.5 (args=%v)", flag, got, args)
		}
	}
	if got := argValue(args, "--evaluator-model"); got != "" {
		t.Fatalf("evaluator should inherit the optimizer model, not get an explicit flag: %q", got)
	}

	// An explicit model still wins for optimizer + target.
	args = build(skillOptTrainOptimizerRequest{Backend: "codex", Model: "o3"})
	for _, flag := range []string{"--optimizer-model", "--target-model"} {
		if got := argValue(args, flag); got != "o3" {
			t.Fatalf("explicit model overridden: %s = %q, want o3", flag, got)
		}
	}

	// A non-codex backend gets no codex-derived default.
	args = build(skillOptTrainOptimizerRequest{OptimizerBackend: "openai_chat"})
	if got := argValue(args, "--optimizer-model"); got == "gpt-5.5" {
		t.Fatalf("non-codex backend must not pick up the codex model: args=%v", args)
	}
}

func TestRemoveSkillOptTrainTargetAgents(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("init: %v", err)
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	sid := "train-plan-20260101-000000-111"
	otherTarget := "skillopt-target-train-other-20260101-000000-222-item-001-generator-ccc"
	for _, name := range []string{
		"skillopt-target-" + sid + "-item-001-generator-aaa",
		"skillopt-target-" + sid + "-item-002-generator-bbb",
		"planner",   // a real user agent
		otherTarget, // another session's plumbing
	} {
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: "codex"}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}

	removeSkillOptTrainTargetAgents(ctx, store, sid)

	got, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := map[string]bool{}
	for _, a := range got {
		names[a.Name] = true
	}
	// This session's target agents are gone; the user agent and the other
	// session's plumbing remain.
	if names["skillopt-target-"+sid+"-item-001-generator-aaa"] || names["skillopt-target-"+sid+"-item-002-generator-bbb"] {
		t.Fatalf("session target agents should be removed: %v", names)
	}
	if !names["planner"] || !names[otherTarget] {
		t.Fatalf("unrelated agents must remain: %v", names)
	}
}

func TestBuildAgentOptimizeFieldsSkipsTemplate(t *testing.T) {
	fields := buildAgentOptimizeFields("", nil)
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Name == "template" {
			t.Fatal("the optimize form must not ask for the template (pre-filled from the agent)")
		}
		names = append(names, f.Name)
	}
	want := []string{"name", "review_repo", "workspace_repo", "items", "artifact_kind", "preview", "request", "backend", "model"}
	if len(names) != len(want) {
		t.Fatalf("fields = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("fields = %v, want %v", names, want)
		}
	}
}

func TestAgentOptimizeInterpretModelOptional(t *testing.T) {
	if value, status := agentOptimizeInterpret("model", "  "); status != "ok" || value != "" {
		t.Fatalf("empty model = (%q, %q), want optional ok", value, status)
	}
	if _, status := agentOptimizeInterpret("name", " "); status != "reask" {
		t.Fatalf("empty name should reask, got %q", status)
	}
	if _, status := agentOptimizeInterpret("workspace_repo", "not-a-repo"); status != "reask" {
		t.Fatalf("malformed workspace repo should reask, got %q", status)
	}
}

func TestStartAgentOptimizeSessionPersistsBackendAndModel(t *testing.T) {
	home := t.TempDir()
	_ = chdirTemp(t)
	restoreFetcher := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restoreFetcher()
	fakeGH := &repoCreateFakeGitHub{}
	restoreGH := replaceSkillOptGitHubClient(fakeGH)
	defer restoreGH()

	sessionID, err := startAgentOptimizeSession(home, "planner", map[string]string{
		"name":           "opt-planner",
		"review_repo":    "owner/review",
		"workspace_repo": "owner/workspace",
		"items":          "4",
		"artifact_kind":  "text",
		"preview":        "none",
		"request":        "Make plans sharper.",
		"backend":        "claude",
		"model":          "claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("startAgentOptimizeSession: %v", err)
	}
	if sessionID == "" {
		t.Fatal("expected a session id")
	}

	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	session, err := store.GetSkillOptTrainSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession: %v", err)
	}
	if session.TemplateID != "planner" || session.TargetRepo != "owner/review" {
		t.Fatalf("session = %+v", session)
	}
	for key, want := range map[string]string{
		"optimizer_backend": "claude",
		"target_backend":    "claude",
		"evaluator_backend": "claude",
		"optimizer_model":   "claude-opus-4-8",
		"target_model":      "claude-opus-4-8",
	} {
		if got := skillOptMetadataString(session.MetadataJSON, "optimizer_defaults", key); got != want {
			t.Fatalf("optimizer_defaults.%s = %q, want %q (metadata=%s)", key, got, want, session.MetadataJSON)
		}
	}

	// The continue path picks the choices up when flags are absent…
	var request skillOptTrainOptimizerRequest
	applySkillOptTrainOptimizerDefaultsFromMetadata(session.MetadataJSON, &request)
	if request.OptimizerBackend != "claude" || request.TargetBackend != "claude" {
		t.Fatalf("applied backends = %+v", request)
	}
	if request.OptimizerModel != "claude-opus-4-8" || request.TargetModel != "claude-opus-4-8" {
		t.Fatalf("applied models = %+v", request)
	}
	// …and explicit flags still win.
	explicit := skillOptTrainOptimizerRequest{Backend: "codex", Model: "gpt-5"}
	applySkillOptTrainOptimizerDefaultsFromMetadata(session.MetadataJSON, &explicit)
	if explicit.Backend != "codex" || explicit.OptimizerBackend != "" {
		t.Fatalf("explicit backend overridden: %+v", explicit)
	}
	if explicit.Model != "gpt-5" || explicit.OptimizerModel != "" || explicit.TargetModel != "" {
		t.Fatalf("explicit model overridden: %+v", explicit)
	}

	// --create-repos records the repos against the session, so deleting the
	// session later offers their cleanup.
	records, err := store.ListCreatedReposForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListCreatedReposForSession: %v", err)
	}
	repos := map[string]bool{}
	for _, record := range records {
		repos[record.Repo] = true
	}
	if !repos["owner/review"] || !repos["owner/workspace"] {
		t.Fatalf("created repo records = %v", records)
	}

	// The requested item count flows through the scaffold into the session.
	items, err := store.ListEvalReviewItems(ctx, sessionID+"-review-001")
	if err != nil {
		t.Fatalf("ListEvalReviewItems: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("review items = %d, want 4", len(items))
	}
}

func TestStarterReviewItemsYAMLCount(t *testing.T) {
	values := skillOptTrainInitInputs{ArtifactKind: "text"}
	// N=2 stays byte-identical to the historical fixed output.
	if got, want := string(skillOptTrainInitStarterReviewItemsYAMLN(values, 2)), string(skillOptTrainInitStarterReviewItemsYAML(values)); got != want {
		t.Fatalf("N=2 output drifted:\n%s", got)
	}
	four := string(skillOptTrainInitStarterReviewItemsYAMLN(values, 4))
	for _, want := range []string{"item-001", "item-002", "item-003", "item-004", "Variation scenario 4"} {
		if !strings.Contains(four, want) {
			t.Fatalf("N=4 missing %q:\n%s", want, four)
		}
	}
	// Below the floor clamps to 2.
	if got := string(skillOptTrainInitStarterReviewItemsYAMLN(values, 1)); strings.Contains(got, "item-003") || !strings.Contains(got, "item-002") {
		t.Fatalf("floor clamp wrong:\n%s", got)
	}
}

func TestRepoPickerChoices(t *testing.T) {
	if skillOptRepoPickerChoices(nil) != nil {
		t.Fatal("no repos → free text (nil choices)")
	}
	choices := skillOptRepoPickerChoices([]string{"o/a", "o/b"})
	if len(choices) != 3 || choices[0].Value != "o/a" || choices[1].Value != "o/b" {
		t.Fatalf("choices = %+v", choices)
	}
	last := choices[2]
	if !last.Custom || last.Placeholder != "owner/repo" {
		t.Fatalf("trailing custom entry wrong: %+v", last)
	}
}

func TestAgentOptimizeInterpretItems(t *testing.T) {
	if _, status := agentOptimizeInterpret("items", "1"); status != "reask" {
		t.Fatal("items below 2 must reask")
	}
	if _, status := agentOptimizeInterpret("items", "abc"); status != "reask" {
		t.Fatal("non-numeric items must reask")
	}
	if value, status := agentOptimizeInterpret("items", " 5 "); status != "ok" || value != "5" {
		t.Fatalf("items 5 = (%q, %q)", value, status)
	}
}

func TestSkillOptTrainOptimizerDefaultsDoNotOverrideExplicitControls(t *testing.T) {
	metadata := `{"optimizer_defaults":{"backend":"codex","optimizer_backend":"openai_chat","target_backend":"openai_chat","evaluator_backend":"openai_chat","optimizer_views":4,"retry_optimizer_views":"auto"}}`
	request := skillOptTrainOptimizerRequest{OptimizerBackend: "openai_chat"}
	applySkillOptTrainOptimizerDefaultsFromMetadata(metadata, &request)
	if request.Backend != "" || request.OptimizerBackend != "openai_chat" {
		t.Fatalf("backend defaults overrode explicit backend controls: %+v", request)
	}
	if request.OptimizerViews != 4 || !request.OptimizerViewsSet || request.RetryOptimizerViews != "auto" || !request.RetryOptimizerViewsSet {
		t.Fatalf("non-conflicting defaults were not applied: %+v", request)
	}

	request = skillOptTrainOptimizerRequest{Backend: "codex"}
	applySkillOptTrainOptimizerDefaultsFromMetadata(metadata, &request)
	if request.OptimizerBackend != "" || request.TargetBackend != "" || request.EvaluatorBackend != "" {
		t.Fatalf("preset backend inherited conflicting backend defaults: %+v", request)
	}
	resolved, _, err := resolveSkillOptTrainBackendRequest(request)
	if err != nil {
		t.Fatalf("resolve codex override returned error: %v", err)
	}
	if resolved.OptimizerBackend != "codex" || resolved.TargetBackend != "codex_exec" || resolved.EvaluatorBackend != "codex" {
		t.Fatalf("resolved codex backend = %+v", resolved)
	}

	request = skillOptTrainOptimizerRequest{RetryOptimizerViews: "8", RetryOptimizerViewsSet: true}
	applySkillOptTrainOptimizerDefaultsFromMetadata(metadata, &request)
	if err := validateSkillOptTrainOptimizerRequestAfterDefaults(&request); err == nil || !strings.Contains(err.Error(), "--retry-optimizer-views cannot exceed --optimizer-views") {
		t.Fatalf("retry/optimizer views validation error = %v, request=%+v", err, request)
	}

	config := skillopt.DefaultTrainInitConfig()
	config.Optimizer.OptimizerBackend = "openai_chat"
	config.Optimizer.TargetBackend = "openai_chat"
	config.Optimizer.InternalTargetAdapter = skillopt.TrainInitInternalTargetAdapterCodex
	config.Optimizer.EvaluatorBackend = "openai_chat"
	defaults := skillOptTrainOptimizerDefaultsFromInitConfig(config)
	if defaults.TargetBackend != "openai_chat" {
		t.Fatalf("custom target backend was not preserved: %+v", defaults)
	}

	request = skillOptTrainOptimizerRequest{}
	applySkillOptTrainOptimizerDefaultsFromMetadata(`{"optimizer_defaults":{"final_eval":true}}`, &request)
	if !request.FinalEval {
		t.Fatalf("final eval default was not applied: %+v", request)
	}
	request = skillOptTrainOptimizerRequest{FinalEvalSet: true}
	applySkillOptTrainOptimizerDefaultsFromMetadata(`{"optimizer_defaults":{"final_eval":true}}`, &request)
	if request.FinalEval {
		t.Fatalf("explicit final-eval=false was overridden: %+v", request)
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
		"--optimizer-views", "4",
		"--retry-optimizer-views", "inherit",
		"--gate", "mixed",
		"--out-root", outRoot,
		"--timeout", "5m",
		"--final-eval",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "launching optimizer dry run") {
		t.Fatalf("missing optimizer launch announcement: %q", stderr.String())
	}
	attemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	for _, want := range []string{
		"current_phase: candidate_created",
		"candidate: planner@v2",
		"continue_ready: true",
		"training_package: " + filepath.Join(attemptRoot, "training.json"),
		"optimizer_root: " + outRoot,
		"optimizer_attempt: attempt-001",
		"optimizer_attempt_path: " + attemptRoot,
		"candidate_package: " + filepath.Join(attemptRoot, "candidate.json"),
		"optimizer_views: 4",
		"retry_optimizer_views: inherit",
		"optimizer_dry_run: true",
		"final_eval: true",
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
		"--training-package", filepath.Join(attemptRoot, "training.json"),
		"--artifact-root", config.PathsForHome(home).ArtifactBlobs,
		"--out-root", attemptRoot,
		"--candidate-output", filepath.Join(attemptRoot, "candidate.json"),
		"--artifact-dir", filepath.Join(attemptRoot, "artifacts"),
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
		"--optimizer-views", "4",
		"--retry-optimizer-views", "inherit",
		"--eval-test",
		"--dry-run",
	} {
		if !containsString(call.args, want) {
			t.Fatalf("optimizer args missing %q: %+v", want, call.args)
		}
	}
	trainingPackage, err := os.ReadFile(filepath.Join(attemptRoot, "training.json"))
	if err != nil {
		t.Fatalf("read training package: %v", err)
	}
	if !strings.Contains(string(trainingPackage), `"kind": "gitmoot-skillopt-training-package"`) {
		t.Fatalf("training package = %s", string(trainingPackage))
	}
	var decodedTrainingPackage skillopt.TrainingPackage
	if err := json.Unmarshal(trainingPackage, &decodedTrainingPackage); err != nil {
		t.Fatalf("decode training package: %v", err)
	}
	if decodedTrainingPackage.EvaluatorProfile == nil ||
		decodedTrainingPackage.EvaluatorProfile.ProfileID != "landing_page_v1" ||
		decodedTrainingPackage.EvaluatorProfile.ArtifactContract != "vue_vite_bundle" ||
		decodedTrainingPackage.EvaluatorProfile.PreviewAdapter != "vue_vite" ||
		decodedTrainingPackage.EvaluatorProfile.Judge == nil ||
		decodedTrainingPackage.EvaluatorProfile.Judge.Model != "gpt-evaluator" {
		t.Fatalf("training package evaluator profile = %+v", decodedTrainingPackage.EvaluatorProfile)
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

func TestSkillOptTrainContinueExportOnlyStopsBeforeOptimizer(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
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
		"--out-root", outRoot,
		"--export-only",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("export-only continue exit code = %d, stderr=%s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("optimizer ran during export-only: %+v", runner.calls)
	}
	if strings.Contains(stderr.String(), "launching optimizer") {
		t.Fatalf("export-only should not announce an optimizer launch: %q", stderr.String())
	}
	for _, want := range []string{
		"current_phase: training_package_created",
		"training_package: ",
		"next: run train continue without --export-only to launch the optimizer",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("export-only stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated {
		t.Fatalf("iteration state = %q, want training_package_created", iteration.State)
	}
}

func TestSkillOptTrainContinueExportOnlyRejectsConflicts(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--export-only",
		"--rerun-optimizer",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("export-only with rerun-optimizer exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--export-only cannot be combined with --rerun-optimizer") {
		t.Fatalf("conflict stderr = %q", stderr.String())
	}
}

func TestSkillOptTrainContinueBlocksMissingOptimizerBinary(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		lookPathErr: errors.New("executable file not found in $PATH"),
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
		t.Fatalf("train continue missing optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.preflightCalls) != 0 || len(runner.calls) != 0 {
		t.Fatalf("optimizer should not run when binary lookup fails: preflight=%+v calls=%+v", runner.preflightCalls, runner.calls)
	}
	for _, want := range []string{
		"optimizer_lock: acquired",
		"optimizer_command: gitmoot-skillopt",
		"next: install gitmoot-skillopt and rerun train continue",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing optimizer stdout missing %q:\n%s", want, stdout.String())
		}
	}
	for _, want := range []string{
		"gitmoot-skillopt is required",
		"find executable failed",
		"pipx install " + skillOptTrainSkillOptWheelURL,
		"gitmoot-skillopt optimize --help",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("missing optimizer stderr missing %q:\n%s", want, stderr.String())
		}
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateFeedbackSynced || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after missing optimizer = %+v", iteration)
	}
	if !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) ||
		!strings.Contains(iteration.MetadataJSON, `"next_action":"install gitmoot-skillopt and rerun train continue"`) ||
		!strings.Contains(iteration.MetadataJSON, "executable file not found") {
		t.Fatalf("iteration metadata after missing optimizer = %s", iteration.MetadataJSON)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status after missing optimizer exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status_phase": "blocked_config"`) {
		t.Fatalf("train status did not report blocked_config:\n%s", stdout.String())
	}
}

func TestSkillOptTrainContinueBlocksBrokenOptimizerBinary(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		versionErr:    errors.New("exit status 1"),
		versionStderr: "ModuleNotFoundError: No module named 'openai'",
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
		t.Fatalf("train continue broken optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.preflightCalls) != 1 || len(runner.calls) != 0 {
		t.Fatalf("optimizer calls after broken version check: preflight=%+v calls=%+v", runner.preflightCalls, runner.calls)
	}
	if runner.preflightCalls[0].args[0] != "--version" {
		t.Fatalf("first preflight call = %+v, want --version", runner.preflightCalls[0])
	}
	for _, want := range []string{
		"optimizer_lock: acquired",
		"optimizer_command: /fake/bin/gitmoot-skillopt",
		"next: install gitmoot-skillopt and rerun train continue",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("broken optimizer stdout missing %q:\n%s", want, stdout.String())
		}
	}
	for _, want := range []string{
		"version check failed",
		"ModuleNotFoundError: No module named 'openai'",
		"pipx install " + skillOptTrainSkillOptWheelURL,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("broken optimizer stderr missing %q:\n%s", want, stderr.String())
		}
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateFeedbackSynced || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after broken optimizer = %+v", iteration)
	}
	if !strings.Contains(iteration.MetadataJSON, `"status":"failed"`) ||
		!strings.Contains(iteration.MetadataJSON, "ModuleNotFoundError") ||
		!strings.Contains(iteration.MetadataJSON, `"next_action":"install gitmoot-skillopt and rerun train continue"`) {
		t.Fatalf("iteration metadata after broken optimizer = %s", iteration.MetadataJSON)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status after broken optimizer exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status_phase": "blocked_config"`) {
		t.Fatalf("train status did not report blocked_config:\n%s", stdout.String())
	}
}

func TestSkillOptTrainRerunRefreshesEvaluatorProfilePackage(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	paths := config.PathsForHome(home)
	ctx := context.Background()
	store := openCLIJobStore(t, home)
	defer store.Close()
	session, err := store.GetSkillOptTrainSession(ctx, "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(ctx, "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}

	request := skillOptTrainOptimizerRequest{
		OutRoot:        filepath.Join(t.TempDir(), "optimizer"),
		EvaluatorID:    "landing_page_v1",
		EvaluatorModel: "initial-evaluator",
	}
	firstPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, iteration, request)
	if err != nil {
		t.Fatalf("resolve first optimizer paths: %v", err)
	}
	firstMetadata, err := exportSkillOptTrainPackage(ctx, store, iteration, firstPaths, request)
	if err != nil {
		t.Fatalf("export first training package: %v", err)
	}
	session.State = skillopt.TrainStateOptimizerCompleted
	iteration.State = skillopt.TrainStateOptimizerCompleted
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", firstMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", firstMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		t.Fatalf("persist completed optimizer state: %v", err)
	}

	rerunRequest := request
	rerunRequest.RerunOptimizer = true
	rerunRequest.EvaluatorModel = "rerun-evaluator"
	rerunPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, iteration, rerunRequest)
	if err != nil {
		t.Fatalf("resolve rerun optimizer paths: %v", err)
	}
	if rerunPaths.TrainingPackagePath == firstPaths.TrainingPackagePath {
		t.Fatalf("rerun reused first training package path: %s", rerunPaths.TrainingPackagePath)
	}
	if _, err := exportSkillOptTrainPackage(ctx, store, iteration, rerunPaths, rerunRequest); err != nil {
		t.Fatalf("export rerun training package: %v", err)
	}
	content, err := os.ReadFile(rerunPaths.TrainingPackagePath)
	if err != nil {
		t.Fatalf("read rerun training package: %v", err)
	}
	var pkg skillopt.TrainingPackage
	if err := json.Unmarshal(content, &pkg); err != nil {
		t.Fatalf("decode rerun training package: %v", err)
	}
	if pkg.EvaluatorProfile == nil ||
		pkg.EvaluatorProfile.ProfileID != "landing_page_v1" ||
		pkg.EvaluatorProfile.Judge == nil ||
		pkg.EvaluatorProfile.Judge.Model != "rerun-evaluator" {
		t.Fatalf("rerun training package evaluator profile = %+v", pkg.EvaluatorProfile)
	}
}

func TestSkillOptTrainRerunFromPackageCreatedExportsFreshPackage(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	paths := config.PathsForHome(home)
	ctx := context.Background()
	store := openCLIJobStore(t, home)
	defer store.Close()
	session, err := store.GetSkillOptTrainSession(ctx, "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(ctx, "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}

	request := skillOptTrainOptimizerRequest{OutRoot: filepath.Join(t.TempDir(), "optimizer")}
	firstPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, iteration, request)
	if err != nil {
		t.Fatalf("resolve first optimizer paths: %v", err)
	}
	firstMetadata, err := exportSkillOptTrainPackage(ctx, store, iteration, firstPaths, request)
	if err != nil {
		t.Fatalf("export first training package: %v", err)
	}
	session.State = skillopt.TrainStateTrainingPackageCreated
	iteration.State = skillopt.TrainStateTrainingPackageCreated
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", firstMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", firstMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		t.Fatalf("persist package-created state: %v", err)
	}

	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with rerun candidate guidance."),
	}
	runner.beforeRun = func(dir string, args []string) error {
		trainingPackage := argValue(args, "--training-package")
		if trainingPackage == "" {
			return errors.New("missing --training-package")
		}
		content, err := os.ReadFile(trainingPackage)
		if err != nil {
			return fmt.Errorf("read rerun training package: %w", err)
		}
		var pkg skillopt.TrainingPackage
		if err := json.Unmarshal(content, &pkg); err != nil {
			return fmt.Errorf("decode rerun training package: %w", err)
		}
		if pkg.EvaluatorProfile == nil ||
			pkg.EvaluatorProfile.Judge == nil ||
			pkg.EvaluatorProfile.Judge.Model != "rerun-evaluator" {
			return fmt.Errorf("rerun training package evaluator profile = %+v", pkg.EvaluatorProfile)
		}
		return nil
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	_, err = continueSkillOptTrainOptimizer(ctx, paths, store, session, iteration, skillOptTrainOptimizerRequest{
		OutRoot:        request.OutRoot,
		RerunOptimizer: true,
		EvaluatorID:    "landing_page_v1",
		EvaluatorModel: "rerun-evaluator",
	}, nil)
	if err != nil {
		t.Fatalf("continueSkillOptTrainOptimizer rerun returned error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("optimizer calls = %+v, want one", runner.calls)
	}
	if argValue(runner.calls[0].args, "--training-package") == firstPaths.TrainingPackagePath {
		t.Fatalf("rerun reused first training package path: %s", firstPaths.TrainingPackagePath)
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
		"--feedback-direct-mode", "auto",
		"--gate-reject-retry-budget", "3",
		"--noop-retry-budget", "1",
		"--wrong-artifact-retry-budget", "1",
		"--target-artifact-retry-budget", "2",
		"--hard-failure-retry-budget", "3",
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
		"feedback_direct_mode: auto",
		"target_artifact_retry_budget: 2",
		"hard_failure_retry_budget: 3",
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
		"--gate-reject-retry-budget", "3",
		"--noop-retry-budget", "1",
		"--wrong-artifact-retry-budget", "1",
		"--target-artifact-retry-budget", "2",
		"--hard-failure-retry-budget", "3",
		"--feedback-direct-mode", "auto",
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

func TestSkillOptTrainContinueRejectsInvalidOptimizerControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "zero optimizer views",
			args: []string{"--optimizer-views", "0"},
			want: "--optimizer-views must be greater than zero",
		},
		{
			name: "invalid retry optimizer views",
			args: []string{"--retry-optimizer-views", "bogus"},
			want: "--retry-optimizer-views must be auto, inherit, or a positive integer",
		},
		{
			name: "zero retry optimizer views",
			args: []string{"--retry-optimizer-views", "0"},
			want: "--retry-optimizer-views must be auto, inherit, or a positive integer",
		},
		{
			name: "retry optimizer views exceeds optimizer views",
			args: []string{"--optimizer-views", "2", "--retry-optimizer-views", "3"},
			want: "--retry-optimizer-views cannot exceed --optimizer-views",
		},
		{
			name: "negative noop retry budget",
			args: []string{"--noop-retry-budget", "-1"},
			want: "--noop-retry-budget must be zero or greater",
		},
		{
			name: "negative target artifact retry budget",
			args: []string{"--target-artifact-retry-budget", "-1"},
			want: "--target-artifact-retry-budget must be zero or greater",
		},
		{
			name: "negative hard failure retry budget",
			args: []string{"--hard-failure-retry-budget", "-1"},
			want: "--hard-failure-retry-budget must be zero or greater",
		},
		{
			name: "invalid feedback direct mode",
			args: []string{"--feedback-direct-mode", "bogus"},
			want: "--feedback-direct-mode must be auto, on, or off",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{
				"skillopt", "train", "continue",
				"--session", "optimizer-train",
			}, tc.args...)
			code := Run(args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("invalid optimizer control exit code = %d, want 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("invalid optimizer control stderr = %q, want %q", stderr.String(), tc.want)
			}
		})
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
		"no_candidate_reason": "gate_rejected_best_origin_initial_skill",
		"no_candidate_details": {
			"attempted_patch": "artifact delivery only",
			"baseline_gate": 0.89,
				"candidate_gate": 0.84,
				"retry_attempts": "1/1",
				"retry_budget": "1",
				"duplicate_retry_detected": true,
			"diagnostic_categories": [
				"old_review_training_signal",
				"candidate_feedback_unresolved",
				"retry_budget_exhausted"
			],
			"selection_gate_relation": "candidate_below_baseline",
				"retry_budget_exhausted": true,
				"retry_stop_reasons": ["budget_exhausted"],
				"optimizer_context_items": ["item-001", "item-002"],
				"score_gap": 0.05,
				"score_gap_handling": "retry_context",
				"hard_score_handling": "retryable_if_actionable",
				"stop_reason": "budget_exhausted",
				"feedback_themes": ["MoonAI-style premium branding", "scroll animations"],
			"evaluator_reason": "Candidate was valid but had weaker imagery.",
			"optimizer_hint": "Resolve imported review themes and artifact contract.",
			"failed_dimensions": ["human_feedback_alignment", "artifact_contract"],
			"human_feedback_context": {
				"feedback_source": "imported_human_review",
				"feedback_target": "baseline_review_outputs",
				"review_issue": "jerryfane/gitmoot-previews#21",
				"review_run_id": "landing-page-preview-trial-005-review-004",
				"reviewed_skill_version": "landing-page-builder@v10",
				"source_item_ids": ["item-001"],
				"rankings": ["D > B > C > A"],
				"themes": ["MoonAI-style premium branding", "scroll animations"],
				"preserve": ["D: clean hero"],
				"improve": ["MoonAI-style premium branding", "scroll animations"],
				"avoid": ["C: overlapping text"]
			},
			"next_action": [
				"collect more feedback",
				"rerun with higher retry budget",
				"manually revise skill direction"
			]
		}
	}`)
	candidate.Summary.Metadata = json.RawMessage(`{
		"promotable": false,
		"no_candidate_reason": "gate_rejected_best_origin_initial_skill",
		"next_action": "Do not import or publish a candidate review; collect more feedback, rerun with gate-reject retry if budget remains, or inspect the candidate package."
	}`)
	baselineGateScore := 0.89
	candidateGateScore := 0.84
	humanFeedbackContext := json.RawMessage(`{
		"feedback_source": "imported_human_review",
		"feedback_target": "baseline_review_outputs",
		"review_issue": "jerryfane/gitmoot-previews#21",
		"review_run_id": "landing-page-preview-trial-005-review-004",
		"reviewed_skill_version": "landing-page-builder@v10",
		"source_item_ids": ["item-001"],
		"rankings": ["D > B > C > A"],
		"themes": ["MoonAI-style premium branding", "scroll animations"],
		"preserve": ["D: clean hero"],
		"improve": ["MoonAI-style premium branding", "scroll animations"],
		"avoid": ["C: overlapping text"]
	}`)
	candidate.Summary.GateRejection = &skillopt.GateRejectionPacket{
		RejectionType:        "candidate_score_regression",
		PrimaryReason:        "candidate_quality_regressed",
		OptimizerHint:        "Resolve imported review themes and artifact contract.",
		FailedDimensions:     []string{"human_feedback_alignment", "artifact_contract"},
		HumanFeedbackContext: humanFeedbackContext,
		AttemptedPatch:       "artifact delivery only",
		RetryAttempts:        "1/1",
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
		"--optimizer-views", "4",
		"--retry-optimizer-views", "inherit",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue no-candidate exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"current_phase: optimizer_completed_no_candidate",
		"candidate: -",
		"continue_ready: true",
		"optimizer_attempt: attempt-001",
		"optimizer_attempt_path: " + filepath.Join(outRoot, "attempts", "attempt-001"),
		"candidate_package: " + filepath.Join(outRoot, "attempts", "attempt-001", "candidate.json"),
		"optimizer_views: 4",
		"retry_optimizer_views: inherit",
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
		!strings.Contains(iteration.MetadataJSON, `"attempted_patch":"artifact delivery only"`) ||
		!strings.Contains(iteration.MetadataJSON, `"feedback_target":"baseline_review_outputs"`) ||
		!strings.Contains(iteration.MetadataJSON, `"score_basis":"feedback_resolution"`) ||
		!strings.Contains(iteration.MetadataJSON, `"retry_budget":"1"`) ||
		!strings.Contains(iteration.MetadataJSON, `"duplicate_retry_detected":true`) ||
		!strings.Contains(iteration.MetadataJSON, `"evaluator_reason":"Candidate was valid but had weaker imagery."`) ||
		!strings.Contains(iteration.MetadataJSON, `"optimizer_hint":"Resolve imported review themes and artifact contract."`) ||
		!strings.Contains(iteration.MetadataJSON, `"optimizer_context_items":["item-001","item-002"]`) ||
		!strings.Contains(iteration.MetadataJSON, `"score_gap_handling":"retry_context"`) ||
		!strings.Contains(iteration.MetadataJSON, `"hard_score_handling":"retryable_if_actionable"`) ||
		!strings.Contains(iteration.MetadataJSON, `"source_item_ids":["item-001"]`) {
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
		statusJSON.NoCandidateDetails["attempted_patch"] != "artifact delivery only" ||
		statusJSON.NoCandidateDetails["feedback_target"] != "baseline_review_outputs" ||
		statusJSON.NoCandidateDetails["score_basis"] != "feedback_resolution" ||
		statusJSON.NoCandidateDetails["retry_budget"] != "1" ||
		statusJSON.NoCandidateDetails["duplicate_retry_detected"] != true ||
		statusJSON.NoCandidateDetails["selection_gate_relation"] != "candidate_below_baseline" ||
		statusJSON.NoCandidateDetails["retry_budget_exhausted"] != true ||
		statusJSON.NoCandidateDetails["score_gap_handling"] != "retry_context" ||
		statusJSON.NoCandidateDetails["hard_score_handling"] != "retryable_if_actionable" ||
		statusJSON.NoCandidateDetails["optimizer_hint"] != "Resolve imported review themes and artifact contract." ||
		statusJSON.NoCandidateDetails["human_feedback_context"] == nil ||
		statusJSON.Verbose == nil ||
		statusJSON.Verbose.Optimizer["optimizer_attempt"] != "attempt-001" ||
		statusJSON.Verbose.Optimizer["optimizer_attempt_state"] != "completed_no_candidate" ||
		statusJSON.Verbose.Optimizer["retry_optimizer_views"] != "inherit" {
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
		"optimizer_attempt: attempt-001",
		"optimizer_attempt_state: completed_no_candidate",
		"optimizer_attempt_path: " + filepath.Join(outRoot, "attempts", "attempt-001"),
		"optimizer_views: 4",
		"retry_optimizer_views: inherit",
		"feedback_source: imported_human_review",
		"feedback_target: baseline_review_outputs",
		"review_issue: jerryfane/gitmoot-previews#21",
		"review_run_id: landing-page-preview-trial-005-review-004",
		"reviewed_skill_version: landing-page-builder@v10",
		"score_basis: feedback_resolution",
		"attempted_patch: artifact delivery only",
		"baseline_gate: 0.89",
		"candidate_gate: 0.84",
		"retry_attempts: 1/1",
		"retry_budget: 1",
		"duplicate_retry_detected: true",
		"diagnostic_categories: old_review_training_signal,candidate_feedback_unresolved,retry_budget_exhausted",
		"selection_gate_relation: candidate_below_baseline",
		"retry_budget_exhausted: true",
		"retry_stop_reasons: budget_exhausted",
		"optimizer_context_items: item-001,item-002",
		"score_gap: 0.05",
		"score_gap_handling: retry_context",
		"hard_score_handling: retryable_if_actionable",
		"stop_reason: budget_exhausted",
		"evaluator_reason: Candidate was valid but had weaker imagery.",
		"optimizer_hint: Resolve imported review themes and artifact contract.",
		"failed_dimensions: human_feedback_alignment,artifact_contract",
		"feedback_themes: MoonAI-style premium branding; scroll animations",
		"human_feedback_feedback_source: imported_human_review",
		"human_feedback_feedback_target: baseline_review_outputs",
		"human_feedback_review_issue: jerryfane/gitmoot-previews#21",
		"human_feedback_review_run_id: landing-page-preview-trial-005-review-004",
		"human_feedback_reviewed_skill_version: landing-page-builder@v10",
		"human_feedback_source_item_ids: item-001",
		"human_feedback_rankings: D > B > C > A",
		"human_feedback_themes: MoonAI-style premium branding; scroll animations",
		"human_feedback_preserve: D: clean hero",
		"human_feedback_improve: MoonAI-style premium branding; scroll animations",
		"human_feedback_avoid: C: overlapping text",
		"next_action_option: collect more feedback",
		"next_action_option: rerun with higher retry budget",
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

func TestSkillOptNoCandidatePackageMetadataPreservesTopLevelDiagnostics(t *testing.T) {
	candidate := cliSkillOptCandidatePackage(t, "planner", "planner@v1", "Plan the work.")
	candidate.EvalReport = json.RawMessage(`{
		"promotable": false,
		"no_candidate_reason": "no_meaningful_skill_change",
		"next_action": "Do not import or publish a candidate review; continue training with revised feedback or stop the run.",
		"no_candidate_diagnostics": {
			"categories": ["retry_budget_exhausted", "candidate_content_unchanged"],
			"selection_gate_relation": "unknown",
			"retry_budget_exhausted": true,
			"retry_stop_reasons": ["noop_retry_budget_exhausted"]
		},
		"feedback_themes": ["better mobile layout", "stronger product visuals"]
	}`)
	candidate.Summary.Metadata = json.RawMessage(`{
		"promotable": false,
		"no_candidate_reason": "no_meaningful_skill_change"
	}`)
	path := filepath.Join(t.TempDir(), "candidate.json")
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	reason, nextAction, details := skillOptNoCandidatePackageMetadata(path)
	if reason != "no_meaningful_skill_change" ||
		!strings.Contains(nextAction, "continue training") ||
		metadataBoolPtr(details, "retry_budget_exhausted") == nil ||
		!*metadataBoolPtr(details, "retry_budget_exhausted") ||
		metadataString(details, "selection_gate_relation") != "unknown" {
		t.Fatalf("metadata = reason=%q next=%q details=%+v", reason, nextAction, details)
	}
	if got := metadataStringSlice(details, "diagnostic_categories"); strings.Join(got, ",") != "retry_budget_exhausted,candidate_content_unchanged" {
		t.Fatalf("diagnostic categories = %v", got)
	}
	if got := metadataStringSlice(details, "retry_stop_reasons"); strings.Join(got, ",") != "noop_retry_budget_exhausted" {
		t.Fatalf("retry stop reasons = %v", got)
	}
	if got := metadataStringSlice(details, "feedback_themes"); strings.Join(got, "; ") != "better mobile layout; stronger product visuals" {
		t.Fatalf("feedback themes = %v", got)
	}
}

func TestSkillOptNoCandidatePackageMetadataPreservesFeedbackContextWithoutDiagnostics(t *testing.T) {
	candidate := cliSkillOptCandidatePackage(t, "planner", "planner@v1", "Plan the work.")
	candidate.EvalReport = json.RawMessage(`{
		"promotable": false,
		"no_candidate_reason": "gate_rejected_best_origin_initial_skill",
		"human_feedback_context": {
			"feedback_source": "imported_human_review",
			"feedback_target": "baseline_review_outputs",
			"review_issue": "jerryfane/gitmoot-previews#21",
			"themes": ["better mobile layout"]
		}
	}`)
	path := filepath.Join(t.TempDir(), "candidate.json")
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	reason, _, details := skillOptNoCandidatePackageMetadata(path)
	if reason != "gate_rejected_best_origin_initial_skill" {
		t.Fatalf("reason = %q", reason)
	}
	if metadataString(details, "feedback_target") != "baseline_review_outputs" ||
		metadataString(details, "score_basis") != "feedback_resolution" ||
		metadataString(details, "review_issue") != "jerryfane/gitmoot-previews#21" {
		t.Fatalf("feedback context details = %+v", details)
	}
	if got := metadataStringSlice(details, "feedback_themes"); strings.Join(got, "; ") != "better mobile layout" {
		t.Fatalf("feedback themes = %v", got)
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
	firstAttemptRoot := argValue(runner.calls[0].args, "--out-root")
	if firstAttemptRoot != filepath.Join(outRoot, "attempts", "attempt-001") {
		t.Fatalf("first optimizer attempt out-root = %q, want attempt-001 under %q", firstAttemptRoot, outRoot)
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
	secondAttemptRoot := argValue(runner.calls[1].args, "--out-root")
	if secondAttemptRoot != filepath.Join(outRoot, "attempts", "attempt-002") {
		t.Fatalf("second optimizer attempt out-root = %q, want attempt-002 under %q", secondAttemptRoot, outRoot)
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_created") ||
		!strings.Contains(stdout.String(), "imported_candidate: planner@v2") ||
		!strings.Contains(stdout.String(), "optimizer_attempt: attempt-002") {
		t.Fatalf("rerun after no-candidate stdout = %s", stdout.String())
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
	wantAttemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	if runner.calls[0].dir != wantAttemptRoot {
		t.Fatalf("optimizer dir = %q, want %q", runner.calls[0].dir, wantAttemptRoot)
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
if [ "${1:-}" = "--version" ]; then
  echo "gitmoot-skillopt 0.2.0b1"
  exit 0
fi
if [ "${1:-}" = "optimize" ] && [ "${2:-}" = "--help" ]; then
  echo "usage: gitmoot-skillopt optimize"
  exit 0
fi
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
	attemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	if _, err := os.Stat(filepath.Join(attemptRoot, "training.json")); err != nil {
		t.Fatalf("training package was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(attemptRoot, "candidate.json")); err != nil {
		t.Fatalf("candidate package was not written: %v", err)
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
	attemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	if argValue(retryCall.args, "--out-root") != attemptRoot || argValue(retryCall.args, "--training-package") != filepath.Join(attemptRoot, "training.json") {
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
	attemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	if _, err := os.Stat(filepath.Join(attemptRoot, "candidate.json")); err != nil {
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
	attemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	if err := os.WriteFile(filepath.Join(attemptRoot, "summary.json"), []byte(`{"status":"failed","reason":"timeout","no_candidate_reason":null}`), 0o644); err != nil {
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
	attemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	if err := os.WriteFile(filepath.Join(attemptRoot, "candidate.json"), []byte(`{not-json`), 0o644); err != nil {
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
	attemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	if err := os.Remove(filepath.Join(attemptRoot, "artifacts", "candidate.diff.md")); err != nil {
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
	attemptRoot := filepath.Join(outRoot, "attempts", "attempt-001")
	artifactDir := filepath.Join(attemptRoot, "artifacts")
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
	if err := os.WriteFile(filepath.Join(attemptRoot, "candidate.json"), encoded, 0o644); err != nil {
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

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--rerun-optimizer",
		"--gate", "unsupported",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue rerun setup failure exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `optimizer gate "unsupported" is not supported`) {
		t.Fatalf("rerun setup failure stderr = %q", stderr.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("rerun setup failure started optimizer: %+v", runner.calls)
	}
	store = openCLIJobStore(t, home)
	afterSetupFailure, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after rerun setup failure returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after rerun setup failure returned error: %v", err)
	}
	beforeMetadata := decodedSkillOptMetadata(iteration.MetadataJSON)
	afterMetadata := decodedSkillOptMetadata(afterSetupFailure.MetadataJSON)
	delete(decodedSkillOptMetadataValue(beforeMetadata["candidate_import"]), "completed_at")
	delete(decodedSkillOptMetadataValue(afterMetadata["candidate_import"]), "completed_at")
	if afterSetupFailure.State != skillopt.TrainStateOptimizerCompleted || !reflect.DeepEqual(afterMetadata, beforeMetadata) {
		t.Fatalf("rerun setup failure changed completed optimizer state: before=%+v after=%+v", iteration, afterSetupFailure)
	}

	firstAttemptRoot := argValue(runner.calls[0].args, "--out-root")
	if firstAttemptRoot == "" {
		t.Fatalf("first optimizer call did not include --out-root: %+v", runner.calls[0].args)
	}
	staleFiles := []string{
		filepath.Join(firstAttemptRoot, "candidate.json"),
		filepath.Join(firstAttemptRoot, "history.json"),
		filepath.Join(firstAttemptRoot, "summary.json"),
		filepath.Join(firstAttemptRoot, "runtime_state.json"),
		filepath.Join(firstAttemptRoot, "best_skill.md"),
		filepath.Join(firstAttemptRoot, "steps", "step_0001", "step_record.json"),
		filepath.Join(firstAttemptRoot, "artifacts", "stale.diff.md"),
	}
	for _, path := range staleFiles {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create stale optimizer artifact dir returned error: %v", err)
		}
		if err := os.WriteFile(path, []byte("stale optimizer artifact\n"), 0o644); err != nil {
			t.Fatalf("write stale optimizer artifact returned error: %v", err)
		}
	}
	unrelatedFile := filepath.Join(firstAttemptRoot, "unrelated", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(unrelatedFile), 0o755); err != nil {
		t.Fatalf("create unrelated out-root file dir returned error: %v", err)
	}
	if err := os.WriteFile(unrelatedFile, []byte("do not remove\n"), 0o644); err != nil {
		t.Fatalf("write unrelated out-root file returned error: %v", err)
	}
	trainingPackagePath := filepath.Join(firstAttemptRoot, "training.json")
	if _, err := os.Stat(trainingPackagePath); err != nil {
		t.Fatalf("expected persisted training package before rerun: %v", err)
	}
	secondAttemptRoot := filepath.Join(filepath.Dir(firstAttemptRoot), "attempt-002")
	runner.beforeRun = func(dir string, args []string) error {
		if len(runner.calls) != 2 {
			return nil
		}
		if dir != secondAttemptRoot || argValue(args, "--out-root") != secondAttemptRoot {
			return fmt.Errorf("rerun did not use isolated optimizer attempt; dir=%s args=%v", dir, args)
		}
		if _, err := os.Stat(trainingPackagePath); err != nil {
			return fmt.Errorf("rerun removed previous attempt training package: %w", err)
		}
		if _, err := os.Stat(unrelatedFile); err != nil {
			return fmt.Errorf("rerun removed previous attempt unrelated file: %w", err)
		}
		for _, path := range staleFiles {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("previous attempt artifact was not preserved before rerun: %s: %w", path, err)
			}
		}
		if _, err := os.Stat(filepath.Join(secondAttemptRoot, "candidate.json")); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("new attempt unexpectedly contains stale candidate before rerun")
		}
		store := openCLIJobStore(t, home)
		iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
		if err != nil {
			_ = store.Close()
			return fmt.Errorf("GetLatestSkillOptTrainIteration during rerun returned error: %w", err)
		}
		if err := store.Close(); err != nil {
			return fmt.Errorf("Close during rerun returned error: %w", err)
		}
		if iteration.State != skillopt.TrainStateTrainingPackageCreated {
			return fmt.Errorf("active optimizer rerun state = %s, want %s", iteration.State, skillopt.TrainStateTrainingPackageCreated)
		}
		optimizer := decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["optimizer"])
		if metadataString(optimizer, "status") != "running" ||
			metadataString(optimizer, "optimizer_attempt") != "attempt-002" ||
			metadataString(optimizer, "optimizer_attempt_path") != secondAttemptRoot {
			return fmt.Errorf("active optimizer attempt was not persisted before rerun launch: %s", iteration.MetadataJSON)
		}
		candidateImport := decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["candidate_import"])
		if attemptState := skillOptTrainOptimizerAttemptState(skillopt.NormalizeTrainState(iteration.State), optimizer, candidateImport); attemptState != "running" {
			return fmt.Errorf("active optimizer attempt state = %q, want running; metadata=%s", attemptState, iteration.MetadataJSON)
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
	if !strings.Contains(stdout.String(), "current_phase: candidate_created") ||
		!strings.Contains(stdout.String(), "imported_candidate: planner@v2") ||
		!strings.Contains(stdout.String(), "optimizer_attempt: attempt-002") {
		t.Fatalf("explicit optimizer rerun stdout = %s", stdout.String())
	}
	for _, path := range staleFiles {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("previous attempt artifact was not preserved after rerun: %s: %v", path, err)
		}
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

func TestSkillOptTrainOptimizerHeartbeat(t *testing.T) {
	prev := skillOptTrainOptimizerProgressInterval
	skillOptTrainOptimizerProgressInterval = 10 * time.Millisecond
	defer func() { skillOptTrainOptimizerProgressInterval = prev }()

	var buf bytes.Buffer
	stop := startSkillOptTrainOptimizerHeartbeat(&buf)
	time.Sleep(45 * time.Millisecond)
	stop() // joins the heartbeat goroutine, so reading buf afterwards is race-free
	if !strings.Contains(buf.String(), "optimizer running - ") {
		t.Fatalf("expected at least one heartbeat line, got %q", buf.String())
	}
	// A nil writer is a no-op and must not panic.
	startSkillOptTrainOptimizerHeartbeat(nil)()
}
