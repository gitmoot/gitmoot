package skillopt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/db"
)

func TestImportCandidatePackageWithOptionsRejectsNoCandidateMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("planner", "Plan carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	candidateContent := testTemplateContent("planner", "Plan carefully with a concise risk section.")
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}

	_, err = ImportCandidatePackageWithOptions(ctx, store, CandidatePackage{
		Kind:            CandidatePackageKind,
		ContractVersion: ContractVersion,
		TemplateID:      "planner",
		BaseVersionID:   "planner@latest",
		Candidate: CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
		EvalReport: json.RawMessage(`{"promotable":false,"no_candidate_reason":"best_origin_initial_skill"}`),
		Summary: CandidateSummary{
			Metadata: json.RawMessage(`{"promotable":false,"no_candidate_reason":"best_origin_initial_skill"}`),
		},
	}, CandidateImportOptions{SourcePath: "candidate.json"})
	if !errors.Is(err, ErrNoCandidate) || !strings.Contains(err.Error(), "optimizer produced no candidate: best_origin_initial_skill") {
		t.Fatalf("ImportCandidatePackageWithOptions error = %v", err)
	}
}

func TestExportTrainingPackage(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("planner", "Plan carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	blobHash := artifact.ContentHash([]byte("baseline output"))
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{
		ID:        "baseline",
		Hash:      blobHash,
		MediaType: "text/markdown",
		SizeBytes: int64(len("baseline output")),
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "run-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "ready",
		MetadataJSON:      `{"driver":"planner"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:              "run-1",
		ItemID:             "item-001",
		Title:              "README",
		BaselineArtifactID: "baseline",
		MetadataJSON:       `{"path":"README.md"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
		RunID:     "run-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reasoning: "More specific.",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "run-1")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}

	if pkg.Kind != TrainingPackageKind || pkg.ContractVersion != ContractVersion {
		t.Fatalf("package header = %s v%d", pkg.Kind, pkg.ContractVersion)
	}
	if pkg.Template.ID != "planner" || pkg.Template.VersionID != installed.VersionID || pkg.Template.Content == "" {
		t.Fatalf("template snapshot = %+v", pkg.Template)
	}
	if len(pkg.Items) != 1 || pkg.Items[0].BaselineArtifactID != "baseline" {
		t.Fatalf("items = %+v", pkg.Items)
	}
	if len(pkg.Artifacts) != 1 || pkg.Artifacts[0].Hash != blobHash {
		t.Fatalf("artifacts = %+v", pkg.Artifacts)
	}
	if string(pkg.EvaluatorConfig) != `{"driver":"planner"}` {
		t.Fatalf("evaluator config = %s", string(pkg.EvaluatorConfig))
	}
	if len(pkg.FeedbackEvents) != 1 || pkg.FeedbackEvents[0].Choice != "b" {
		t.Fatalf("feedback events = %+v", pkg.FeedbackEvents)
	}
	if _, err := json.Marshal(pkg); err != nil {
		t.Fatalf("exported package did not marshal: %v", err)
	}
}

func TestExportTrainingPackageIncludesPreferredGateMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("designer", "Design carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "designer")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "train-run-1",
		TemplateID:        "designer",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		Mode:              db.EvalRunModeExplore,
		ExplorationLevel:  db.ExplorationLevelHigh,
		OptionsCount:      4,
		MetadataJSON:      `{"mode":"explore","evaluation":{"preferred_gate":"soft"},"source":"gitmoot skillopt train start"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:        "train-run-1",
		ItemID:       "item-001",
		Title:        "Landing page",
		MetadataJSON: `{"brief":"Create a landing page.","output_type":"vue"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "train-run-1")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	var evaluator map[string]any
	if err := json.Unmarshal(pkg.EvaluatorConfig, &evaluator); err != nil {
		t.Fatalf("decode evaluator config: %v", err)
	}
	if pkg.TrainingMode != db.EvalRunModeExplore {
		t.Fatalf("training mode = %q, want %q", pkg.TrainingMode, db.EvalRunModeExplore)
	}
	if evaluator["mode"] != nil {
		t.Fatalf("evaluator config leaked training mode = %s", string(pkg.EvaluatorConfig))
	}
	evaluation, ok := evaluator["evaluation"].(map[string]any)
	if !ok || evaluation["preferred_gate"] != "soft" {
		t.Fatalf("evaluator config = %s", string(pkg.EvaluatorConfig))
	}
}

func TestExportTrainingPackagePreservesEvaluatorMode(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("judge", "Judge carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "judge")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "judge-run-1",
		TemplateID:        "judge",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		Mode:              db.EvalRunModeRefine,
		MetadataJSON:      `{"mode":"llm_judge","evaluation":{"preferred_gate":"soft"}}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "judge-run-1")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	if pkg.TrainingMode != db.EvalRunModeRefine {
		t.Fatalf("training mode = %q, want %q", pkg.TrainingMode, db.EvalRunModeRefine)
	}
	var evaluator map[string]any
	if err := json.Unmarshal(pkg.EvaluatorConfig, &evaluator); err != nil {
		t.Fatalf("decode evaluator config: %v", err)
	}
	if evaluator["mode"] != "llm_judge" {
		t.Fatalf("evaluator config did not preserve evaluator mode: %s", string(pkg.EvaluatorConfig))
	}
}

func TestExportTrainingPackagePreservesEvaluatorConfigNumberFidelity(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("judge", "Judge carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "judge")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "judge-run-2",
		TemplateID:        "judge",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		Mode:              db.EvalRunModeExplore,
		MetadataJSON:      `{"mode":"explore","evaluation":{"seed":9007199254740993123}}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "judge-run-2")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	if strings.Contains(string(pkg.EvaluatorConfig), `"mode"`) {
		t.Fatalf("evaluator config leaked training mode = %s", string(pkg.EvaluatorConfig))
	}
	if !strings.Contains(string(pkg.EvaluatorConfig), `9007199254740993123`) {
		t.Fatalf("evaluator config rewrote large numeric value: %s", string(pkg.EvaluatorConfig))
	}
}

func TestExportTrainingPackageCarriesJudgePromptTemplates(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("designer", "Design carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "designer")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "judge-prompt-run",
		TemplateID:        "designer",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON: `{"evaluation":{"evaluator_id":"landing_page_v1","evaluator_model":"gpt-evaluator",` +
			`"judge_prompt_version":"jp-2",` +
			`"judge_prompt_templates":{"vue_landing_page":"Judge the landing page.","generic":"Judge the artifact."}}}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "judge-prompt-run")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	if pkg.EvaluatorProfile == nil || pkg.EvaluatorProfile.Judge == nil {
		t.Fatalf("evaluator profile/judge missing: %+v", pkg.EvaluatorProfile)
	}

	assertJudgePrompt := func(t *testing.T, judge *EvaluatorJudgeConfig) {
		t.Helper()
		payload := judge.JudgePromptConfig()
		if payload == nil {
			t.Fatalf("judge prompt config missing; judge.Config = %s", string(judge.Config))
		}
		if payload.JudgePromptVersion != "jp-2" {
			t.Fatalf("judge_prompt_version = %q, want jp-2", payload.JudgePromptVersion)
		}
		if len(payload.JudgePromptTemplates) != 2 ||
			payload.JudgePromptTemplates["vue_landing_page"] != "Judge the landing page." ||
			payload.JudgePromptTemplates["generic"] != "Judge the artifact." {
			t.Fatalf("judge_prompt_templates = %+v", payload.JudgePromptTemplates)
		}
	}
	assertJudgePrompt(t, pkg.EvaluatorProfile.Judge)

	// The fields must survive a JSON round-trip (export → marshal → unmarshal),
	// which is the boundary the Python judge reads across.
	encoded, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal package: %v", err)
	}
	var decoded TrainingPackage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal package: %v", err)
	}
	if decoded.EvaluatorProfile == nil || decoded.EvaluatorProfile.Judge == nil {
		t.Fatalf("decoded evaluator profile/judge missing: %+v", decoded.EvaluatorProfile)
	}
	assertJudgePrompt(t, decoded.EvaluatorProfile.Judge)

	// The Python side reads it at evaluator_profile.judge.config.* — verify the
	// on-wire JSON path is exactly that.
	if !strings.Contains(string(decoded.EvaluatorProfile.Judge.Config), `"judge_prompt_version":"jp-2"`) {
		t.Fatalf("judge.config did not carry judge_prompt_version on the wire: %s", string(decoded.EvaluatorProfile.Judge.Config))
	}

	// The raw evaluator config also passes through unchanged.
	if !strings.Contains(string(decoded.EvaluatorConfig), `"judge_prompt_version":"jp-2"`) ||
		!strings.Contains(string(decoded.EvaluatorConfig), `"vue_landing_page":"Judge the landing page."`) {
		t.Fatalf("evaluator_config did not carry judge prompt fields: %s", string(decoded.EvaluatorConfig))
	}
}

func TestExportTrainingPackageJudgePromptTemplatesAbsentIsBackwardCompatible(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("designer", "Design carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "designer")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "no-judge-prompt-run",
		TemplateID:        "designer",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"evaluation":{"evaluator_id":"landing_page_v1","evaluator_model":"gpt-evaluator"}}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "no-judge-prompt-run")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	if pkg.EvaluatorProfile == nil || pkg.EvaluatorProfile.Judge == nil {
		t.Fatalf("evaluator profile/judge missing: %+v", pkg.EvaluatorProfile)
	}
	// Absent fields must not synthesize a judge prompt payload and must not add
	// a judge config object to the wire format.
	if payload := pkg.EvaluatorProfile.Judge.JudgePromptConfig(); payload != nil {
		t.Fatalf("expected no judge prompt config, got %+v", payload)
	}
	if len(strings.TrimSpace(string(pkg.EvaluatorProfile.Judge.Config))) != 0 {
		t.Fatalf("judge config should be empty when absent, got %s", string(pkg.EvaluatorProfile.Judge.Config))
	}
	encoded, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal package: %v", err)
	}
	if strings.Contains(string(encoded), "judge_prompt_templates") || strings.Contains(string(encoded), "judge_prompt_version") {
		t.Fatalf("absent judge prompt fields leaked into package JSON: %s", string(encoded))
	}
}

func TestEvaluatorJudgeConfigJudgePromptConfigIgnoresMalformedShapes(t *testing.T) {
	tests := map[string]string{
		"templates not an object":      `{"judge_prompt_templates":["a","b"]}`,
		"templates values not strings": `{"judge_prompt_templates":{"vue":1}}`,
		"version not a string":         `{"judge_prompt_version":42}`,
		"config not an object":         `"just a string"`,
		"empty templates object":       `{"judge_prompt_templates":{}}`,
		"unrelated keys only":          `{"something_else":true}`,
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			judge := &EvaluatorJudgeConfig{Config: json.RawMessage(config)}
			if payload := judge.JudgePromptConfig(); payload != nil {
				t.Fatalf("malformed config %q should be ignored, got %+v", config, payload)
			}
		})
	}
	// A valid version alongside a malformed templates map still yields the
	// version (the malformed half is dropped, not fatal).
	judge := &EvaluatorJudgeConfig{Config: json.RawMessage(`{"judge_prompt_version":"jp-3","judge_prompt_templates":[1,2]}`)}
	payload := judge.JudgePromptConfig()
	if payload == nil || payload.JudgePromptVersion != "jp-3" || len(payload.JudgePromptTemplates) != 0 {
		t.Fatalf("expected version-only payload, got %+v", payload)
	}
}

func TestExportTrainingPackageBuildsEvaluatorProfileFromMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("designer", "Design carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "designer")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "train-run-1",
		TemplateID:        "designer",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"evaluation":{"evaluator_id":"landing_page_v1","evaluator_model":"gpt-evaluator"}}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "train-run-1")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	if pkg.EvaluatorProfile == nil ||
		pkg.EvaluatorProfile.ProfileID != "landing_page_v1" ||
		pkg.EvaluatorProfile.TaskKind != "vue_landing_page" ||
		pkg.EvaluatorProfile.ArtifactContract != "vue_vite_bundle" ||
		pkg.EvaluatorProfile.PreviewAdapter != "vue_vite" ||
		pkg.EvaluatorProfile.Judge == nil ||
		pkg.EvaluatorProfile.Judge.Model != "gpt-evaluator" {
		t.Fatalf("evaluator profile = %+v", pkg.EvaluatorProfile)
	}
}

func TestBuildEvaluatorProfilePreservesCustomEvaluatorID(t *testing.T) {
	profile := BuildEvaluatorProfile("CustomJudgeV2", "gpt-evaluator", json.RawMessage(`{"kind":"custom"}`))
	if profile == nil {
		t.Fatal("BuildEvaluatorProfile returned nil")
	}
	if profile.ProfileID != "CustomJudgeV2" {
		t.Fatalf("ProfileID = %q, want CustomJudgeV2", profile.ProfileID)
	}
	if profile.TaskKind != "generic" {
		t.Fatalf("TaskKind = %q, want generic", profile.TaskKind)
	}
	if profile.Judge == nil || profile.Judge.Model != "gpt-evaluator" {
		t.Fatalf("Judge = %+v", profile.Judge)
	}
}

func TestEvaluatorProfileAndFailurePacketContractsRoundTrip(t *testing.T) {
	hard := 0.0
	soft := 0.12
	baselineSoft := 0.78
	baselineGate := 0.89
	candidateSoft := 0.68
	candidateGate := 0.84
	training := TrainingPackage{
		Kind:            TrainingPackageKind,
		ContractVersion: ContractVersion,
		Template: TemplateSnapshot{
			ID:            "planner",
			VersionID:     "planner@v1",
			VersionNumber: 1,
			VersionState:  "active",
			ContentHash:   "sha256:abc",
			Metadata: agenttemplate.Metadata{
				ID:          "planner",
				Name:        "Planner",
				Description: "Plans work.",
				Kind:        "agent-template",
				Version:     1,
				Capabilities: []string{
					"ask",
				},
				RuntimeCompatibility: []string{
					"codex",
				},
				Tags: []string{
					"planning",
				},
				Inputs: []string{
					"request",
				},
				Outputs: []string{
					"plan",
				},
			},
			Content: "Plan carefully.",
		},
		EvalRun: EvalRun{
			ID:         "run-1",
			TemplateID: "planner",
			State:      "review",
		},
		EvaluatorProfile: &EvaluatorProfile{
			ProfileID:        "vue_landing_page_v1",
			TaskKind:         "vue_landing_page",
			ArtifactContract: "vue_vite_bundle",
			PreviewAdapter:   "vue_vite",
			Checks: []EvaluatorCheckConfig{
				{ID: "required_files", Type: "artifact_contract", Required: true},
				{ID: "render_smoke", Type: "playwright", When: "checks_pass"},
			},
			Judge:    &EvaluatorJudgeConfig{Type: "screenshot_llm", When: "checks_pass", Model: "gpt-evaluator"},
			Metadata: json.RawMessage(`{"source":"test"}`),
		},
	}
	candidate := CandidatePackage{
		Kind:            CandidatePackageKind,
		ContractVersion: ContractVersion,
		TemplateID:      "planner",
		Summary: CandidateSummary{
			EvaluatorScore: &EvaluatorScore{
				ProfileID:              "vue_landing_page_v1",
				TaskKind:               "vue_landing_page",
				ContractStatus:         "failed",
				QualityStatus:          "not_run",
				HumanFeedbackAlignment: json.RawMessage(`{"status":"feedback_available","required_improvements":["stronger product visuals"]}`),
				Hard:                   &hard,
				Soft:                   &soft,
				DimensionScores: map[string]float64{
					"artifact_contract": 0,
				},
				FailReason: "missing required artifact",
				Failure: &EvaluatorFailurePacket{
					PrimaryReason: "missing_required_artifact",
					HumanReason:   "The response did not include the required Vue/Vite preview bundle.",
					OptimizerHint: "Return serialized bundle JSON with required files.",
					FailedChecks: []EvaluatorCheckResult{
						{
							Check:    "artifact_contract.required_files",
							Severity: "hard_blocker",
							Reason:   "src/App.vue was not present.",
							Evidence: []string{"src/App.vue missing"},
						},
					},
					Evidence: []string{"bundle JSON shape missing"},
					StageStatus: []EvaluatorStageStatus{
						{Stage: "artifact_contract", Status: "failed", DurationMS: 7},
					},
				},
				GateRejection: &GateRejectionPacket{
					RejectionType: "candidate_score_regression",
					Retryable:     true,
					Baseline: GateRejectionScores{
						Hard:      &hard,
						Soft:      &baselineSoft,
						GateScore: &baselineGate,
					},
					Candidate: GateRejectionScores{
						Hard:      &hard,
						Soft:      &candidateSoft,
						GateScore: &candidateGate,
					},
					PrimaryReason:    "candidate_quality_regressed",
					HumanReason:      "The patch did not improve the requested design qualities.",
					OptimizerHint:    "Add guidance for branding, product visuals, animation, and mobile layout.",
					FailedDimensions: []string{"human_feedback_alignment", "visual_quality"},
					Evidence:         []string{"Candidate soft score dropped from 0.78 to 0.68."},
					AttemptedPatch:   "Hard Artifact Delivery",
					RetryAttempts:    "0/1",
					NextAction:       "Retry once with the gate rejection hint.",
				},
				StageStatus: []EvaluatorStageStatus{
					{Stage: "artifact_contract", Status: "failed"},
				},
			},
			GateRejection: &GateRejectionPacket{
				RejectionType: "candidate_score_regression",
				Retryable:     true,
				Baseline: GateRejectionScores{
					Hard:      &hard,
					Soft:      &baselineSoft,
					GateScore: &baselineGate,
				},
				Candidate: GateRejectionScores{
					Hard:      &hard,
					Soft:      &candidateSoft,
					GateScore: &candidateGate,
				},
				PrimaryReason:    "candidate_quality_regressed",
				HumanReason:      "Candidate lost selection against baseline.",
				OptimizerHint:    "Do not repeat the previous artifact-delivery-only patch.",
				FailedDimensions: []string{"human_feedback_alignment", "visual_quality"},
				Evidence:         []string{"candidate gate score 0.84 <= baseline gate score 0.89"},
				AttemptedPatch:   "Hard Artifact Delivery",
				RetryAttempts:    "0/1",
				NextAction:       "Retry once or collect more feedback.",
			},
		},
	}

	trainingBytes, err := json.Marshal(training)
	if err != nil {
		t.Fatalf("marshal training package: %v", err)
	}
	var decodedTraining TrainingPackage
	if err := json.Unmarshal(trainingBytes, &decodedTraining); err != nil {
		t.Fatalf("unmarshal training package: %v", err)
	}
	if decodedTraining.EvaluatorProfile == nil ||
		decodedTraining.EvaluatorProfile.ProfileID != "vue_landing_page_v1" ||
		decodedTraining.EvaluatorProfile.Checks[1].ID != "render_smoke" {
		t.Fatalf("decoded evaluator profile = %+v", decodedTraining.EvaluatorProfile)
	}

	candidateBytes, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate package: %v", err)
	}
	var decodedCandidate CandidatePackage
	if err := json.Unmarshal(candidateBytes, &decodedCandidate); err != nil {
		t.Fatalf("unmarshal candidate package: %v", err)
	}
	failure := decodedCandidate.Summary.EvaluatorScore.Failure
	if failure == nil ||
		failure.PrimaryReason != "missing_required_artifact" ||
		failure.FailedChecks[0].Check != "artifact_contract.required_files" ||
		*decodedCandidate.Summary.EvaluatorScore.Soft != soft ||
		decodedCandidate.Summary.EvaluatorScore.ContractStatus != "failed" ||
		decodedCandidate.Summary.EvaluatorScore.QualityStatus != "not_run" ||
		!strings.Contains(string(decodedCandidate.Summary.EvaluatorScore.HumanFeedbackAlignment), "stronger product visuals") {
		t.Fatalf("decoded evaluator failure = %+v", decodedCandidate.Summary.EvaluatorScore)
	}
	gateRejection := decodedCandidate.Summary.GateRejection
	if gateRejection == nil ||
		gateRejection.RejectionType != "candidate_score_regression" ||
		!gateRejection.Retryable ||
		gateRejection.Baseline.GateScore == nil ||
		*gateRejection.Baseline.GateScore != baselineGate ||
		gateRejection.Candidate.Soft == nil ||
		*gateRejection.Candidate.Soft != candidateSoft ||
		gateRejection.FailedDimensions[0] != "human_feedback_alignment" ||
		!strings.Contains(gateRejection.OptimizerHint, "artifact-delivery-only") {
		t.Fatalf("decoded gate rejection = %+v", gateRejection)
	}
	scoreGateRejection := decodedCandidate.Summary.EvaluatorScore.GateRejection
	if scoreGateRejection == nil ||
		scoreGateRejection.Baseline.GateScore == nil ||
		*scoreGateRejection.Baseline.GateScore != baselineGate ||
		scoreGateRejection.RetryAttempts != "0/1" {
		t.Fatalf("decoded evaluator score gate rejection = %+v", scoreGateRejection)
	}
}

func TestExportTrainingPackageIncludesRankedExplorationFeedback(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("planner", "Plan carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		content := []byte("option " + label)
		if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{
			ID:        "option-" + label,
			Hash:      artifact.ContentHash(content),
			MediaType: "text/markdown",
			SizeBytes: int64(len(content)),
			Driver:    "text",
		}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", label, err)
		}
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "ranked-1",
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
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:        "ranked-1",
		ItemID:       "item-001",
		Title:        "Landing page",
		MetadataJSON: `{"prompt":"build landing page"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{
			RunID:        "ranked-1",
			ItemID:       "item-001",
			Label:        label,
			ArtifactID:   "option-" + label,
			Role:         "option",
			MetadataJSON: `{"preview_url":"https://example.com/` + label + `"}`,
		}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	ranking, err := json.Marshal([]string{"c", "a", "d", "b"})
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	useful, err := json.Marshal(map[string][]string{"c": {"clearest explanation"}, "d": {"motion"}})
	if err != nil {
		t.Fatalf("marshal useful traits: %v", err)
	}
	rejected, err := json.Marshal(map[string][]string{"b": {"too generic"}})
	if err != nil {
		t.Fatalf("marshal rejected traits: %v", err)
	}
	required, err := json.Marshal([]string{"stronger visual identity", "responsive hero"})
	if err != nil {
		t.Fatalf("marshal required improvements: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(ctx, db.RankedFeedbackEvent{
		RunID:                    "ranked-1",
		ItemID:                   "item-001",
		RankingJSON:              string(ranking),
		Winner:                   "c",
		UsefulTraitsJSON:         string(useful),
		RejectedTraitsJSON:       string(rejected),
		RequiredImprovementsJSON: string(required),
		Quality:                  "acceptable",
		ContinueMode:             db.EvalRunModeRefine,
		Promote:                  "no",
		Reasoning:                "C is the clearest direction.",
		Reviewer:                 "jerry",
		Source:                   "github",
		SourceURL:                "https://github.com/owner/repo/issues/1#issuecomment-1",
		CreatedAt:                "2026-06-02T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "ranked-1")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}

	if pkg.EvalRun.Mode != db.EvalRunModeExplore || pkg.EvalRun.ExplorationLevel != db.ExplorationLevelHigh || pkg.EvalRun.OptionsCount != 4 {
		t.Fatalf("eval run = %+v", pkg.EvalRun)
	}
	if len(pkg.Items) != 1 || len(pkg.Items[0].Options) != 4 || pkg.Items[0].Options[2].ArtifactID != "option-c" {
		t.Fatalf("items = %+v", pkg.Items)
	}
	if len(pkg.Artifacts) != 4 {
		t.Fatalf("artifacts = %+v", pkg.Artifacts)
	}
	if len(pkg.RankedFeedbackEvents) != 1 || pkg.RankedFeedbackEvents[0].ID == "" || pkg.RankedFeedbackEvents[0].Winner != "c" || len(pkg.RankedFeedbackEvents[0].Ranking) != 4 {
		t.Fatalf("ranked feedback = %+v", pkg.RankedFeedbackEvents)
	}
	if !strings.Contains(string(pkg.RankedFeedbackEvents[0].UsefulTraits), "clearest explanation") || !strings.Contains(string(pkg.RankedFeedbackEvents[0].RejectedTraits), "too generic") {
		t.Fatalf("ranked traits useful=%s rejected=%s", pkg.RankedFeedbackEvents[0].UsefulTraits, pkg.RankedFeedbackEvents[0].RejectedTraits)
	}
	if !strings.Contains(string(pkg.RankedFeedbackEvents[0].RequiredImprovements), "stronger visual identity") || !strings.Contains(string(pkg.RankedFeedbackEvents[0].RequiredImprovements), "responsive hero") {
		t.Fatalf("ranked required improvements = %s", pkg.RankedFeedbackEvents[0].RequiredImprovements)
	}
	if pkg.RankedFeedbackEvents[0].Quality != "acceptable" || pkg.RankedFeedbackEvents[0].ContinueMode != db.EvalRunModeRefine || pkg.RankedFeedbackEvents[0].Promote != "no" {
		t.Fatalf("ranked feedback signals = %+v", pkg.RankedFeedbackEvents[0])
	}
	if len(pkg.PairwisePreferences) != 6 || pkg.PairwisePreferences[0].Preferred != "c" || pkg.PairwisePreferences[5].Rejected != "b" || pkg.PairwisePreferences[0].RankedEventID != pkg.RankedFeedbackEvents[0].ID {
		t.Fatalf("pairwise preferences = %+v", pkg.PairwisePreferences)
	}
	var feedbackContext map[string]any
	if err := json.Unmarshal(pkg.FeedbackContext, &feedbackContext); err != nil {
		t.Fatalf("feedback context did not unmarshal: %v", err)
	}
	if feedbackContext["feedback_source"] != "imported_human_review" || feedbackContext["feedback_target"] != "baseline_review_outputs" {
		t.Fatalf("feedback context source/target = %+v", feedbackContext)
	}
	if feedbackContext["review_issue"] != "owner/repo#1" || feedbackContext["review_run_id"] != "ranked-1" || feedbackContext["reviewed_skill_version"] != installed.VersionID {
		t.Fatalf("feedback context review metadata = %+v", feedbackContext)
	}
	if _, err := json.Marshal(pkg); err != nil {
		t.Fatalf("exported ranked package did not marshal: %v", err)
	}
}

func TestReviewIssueFromSourceURLSupportsIssueAndPullTargets(t *testing.T) {
	tests := map[string]string{
		"https://github.com/owner/repo/issues/1#issuecomment-1": "owner/repo#1",
		"https://github.com/owner/repo/pull/2#issuecomment-2":   "owner/repo#2",
		"https://example.com/owner/repo/issues/3":               "",
	}
	for sourceURL, want := range tests {
		if got := reviewIssueFromSourceURL(sourceURL); got != want {
			t.Fatalf("reviewIssueFromSourceURL(%q) = %q, want %q", sourceURL, got, want)
		}
	}
}

func TestImportCandidatePackageWithArtifacts(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	artifactDir := t.TempDir()
	diffContent := []byte("candidate diff\n")
	diffHash := artifact.ContentHash(diffContent)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), diffContent, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	blobStore := artifact.NewStore(filepath.Join(t.TempDir(), "blobs"))
	candidate := testCandidatePackage(t, "planner", current.VersionID, "Plan carefully with artifact-backed evidence.")
	diffSize := int64(len(diffContent))
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      diffHash,
		MediaType: "text/markdown",
		Driver:    "text",
		SizeBytes: &diffSize,
	}}

	version, err := ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{
		SourcePath:  "candidate.json",
		ArtifactDir: artifactDir,
		BlobStore:   blobStore,
	})
	if err != nil {
		t.Fatalf("ImportCandidatePackageWithOptions returned error: %v", err)
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	if review.DiffArtifactID != "candidate-diff" {
		t.Fatalf("review diff artifact id = %q", review.DiffArtifactID)
	}
	stored, err := store.GetEvalArtifact(ctx, "candidate-diff")
	if err != nil {
		t.Fatalf("GetEvalArtifact returned error: %v", err)
	}
	if stored.Hash != diffHash || stored.SizeBytes != diffSize || stored.MediaType != "text/markdown" || stored.Driver != "text" {
		t.Fatalf("stored artifact = %+v", stored)
	}
	storedContent, err := blobStore.Read(diffHash)
	if err != nil {
		t.Fatalf("Read stored blob returned error: %v", err)
	}
	if string(storedContent) != string(diffContent) {
		t.Fatalf("stored blob content = %q", string(storedContent))
	}
}

func TestImportCandidatePackageArtifactValidationFailsBeforeCandidateState(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		hash        string
		artifactDir string
		writeFile   bool
		wantErr     string
	}{
		{
			name:        "missing artifact dir",
			path:        "candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: "",
			writeFile:   true,
			wantErr:     "candidate artifacts require --artifact-dir",
		},
		{
			name:        "invalid hash",
			path:        "candidate.diff.md",
			hash:        artifact.ContentHash([]byte("other")),
			artifactDir: "set",
			writeFile:   true,
			wantErr:     "hash is",
		},
		{
			name:        "path traversal",
			path:        "../candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: "set",
			writeFile:   false,
			wantErr:     "relative path inside artifact-dir",
		},
		{
			name:        "absolute path",
			path:        "/tmp/candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: "set",
			writeFile:   false,
			wantErr:     "relative path inside artifact-dir",
		},
		{
			name:        "missing file",
			path:        "missing.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: "set",
			writeFile:   false,
			wantErr:     "no such file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatalf("Open returned error: %v", err)
			}
			defer store.Close()
			if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
				t.Fatalf("UpsertAgentTemplate returned error: %v", err)
			}
			current, err := store.GetAgentTemplate(ctx, "planner")
			if err != nil {
				t.Fatalf("GetAgentTemplate returned error: %v", err)
			}
			artifactDir := ""
			if tt.artifactDir == "set" {
				artifactDir = t.TempDir()
			}
			if tt.writeFile && artifactDir != "" {
				if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), []byte("candidate diff\n"), 0o644); err != nil {
					t.Fatalf("write diff artifact: %v", err)
				}
			}
			candidate := testCandidatePackage(t, "planner", current.VersionID, "Plan carefully with artifact-backed evidence.")
			candidate.Summary.DiffArtifactID = "candidate-diff"
			candidate.Artifacts = []CandidateArtifactRef{{
				ID:        "candidate-diff",
				Path:      tt.path,
				Hash:      tt.hash,
				MediaType: "text/markdown",
				Driver:    "text",
			}}

			_, err = ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{
				SourcePath:  "candidate.json",
				ArtifactDir: artifactDir,
				BlobStore:   artifact.NewStore(filepath.Join(t.TempDir(), "blobs")),
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ImportCandidatePackageWithOptions error = %v, want substring %q", err, tt.wantErr)
			}
			pending, err := store.ListPendingAgentTemplateVersions(ctx, "planner")
			if err != nil {
				t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
			}
			if len(pending) != 0 {
				t.Fatalf("pending versions = %+v, want none", pending)
			}
			if _, err := store.GetEvalArtifact(ctx, "candidate-diff"); err == nil {
				t.Fatalf("candidate artifact was registered despite failed import")
			}
		})
	}
}

func TestImportCandidatePackageRejectsDuplicateCandidateArtifactIDs(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	artifactDir := t.TempDir()
	content := []byte("candidate diff\n")
	hash := artifact.ContentHash(content)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), content, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	candidate := testCandidatePackage(t, "planner", current.VersionID, "Plan carefully with artifact-backed evidence.")
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []CandidateArtifactRef{
		{ID: "candidate-diff", Path: "candidate.diff.md", Hash: hash, MediaType: "text/markdown", Driver: "text"},
		{ID: "candidate-diff", Path: "candidate.diff.md", Hash: hash, MediaType: "text/markdown", Driver: "text"},
	}

	_, err = ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{
		SourcePath:  "candidate.json",
		ArtifactDir: artifactDir,
		BlobStore:   artifact.NewStore(filepath.Join(t.TempDir(), "blobs")),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("ImportCandidatePackageWithOptions error = %v, want duplicate id", err)
	}
	pending, err := store.ListPendingAgentTemplateVersions(ctx, "planner")
	if err != nil {
		t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending versions = %+v, want none", pending)
	}
}

func TestImportCandidatePackageRejectsExistingCandidateArtifactID(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	originalHash := artifact.ContentHash([]byte("old diff\n"))
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{
		ID:        "candidate-diff",
		Hash:      originalHash,
		MediaType: "text/markdown",
		SizeBytes: int64(len("old diff\n")),
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	artifactDir := t.TempDir()
	content := []byte("new diff\n")
	hash := artifact.ContentHash(content)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), content, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	candidate := testCandidatePackage(t, "planner", current.VersionID, "Plan carefully with artifact-backed evidence.")
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      hash,
		MediaType: "text/markdown",
		Driver:    "text",
	}}

	_, err = ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{
		SourcePath:  "candidate.json",
		ArtifactDir: artifactDir,
		BlobStore:   artifact.NewStore(filepath.Join(t.TempDir(), "blobs")),
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("ImportCandidatePackageWithOptions error = %v, want existing artifact rejection", err)
	}
	stored, err := store.GetEvalArtifact(ctx, "candidate-diff")
	if err != nil {
		t.Fatalf("GetEvalArtifact returned error: %v", err)
	}
	if stored.Hash != originalHash {
		t.Fatalf("stored artifact hash = %q, want original %q", stored.Hash, originalHash)
	}
	pending, err := store.ListPendingAgentTemplateVersions(ctx, "planner")
	if err != nil {
		t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending versions = %+v, want none", pending)
	}
}

func testTemplate(id string, body string) db.AgentTemplate {
	content := testTemplateContent(id, body)
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

func testCandidatePackage(t *testing.T, templateID string, baseVersionID string, body string) CandidatePackage {
	t.Helper()
	candidateContent := testTemplateContent(templateID, body)
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	return CandidatePackage{
		Kind:            CandidatePackageKind,
		ContractVersion: ContractVersion,
		TemplateID:      templateID,
		BaseVersionID:   baseVersionID,
		Candidate: CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
		EvalReport: json.RawMessage(`{"score":0.82}`),
		Summary: CandidateSummary{
			PreferenceSummary: "Candidate is more actionable.",
		},
	}
}

func testTemplateContent(id string, body string) string {
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

func TestTemplateMetadataSynthesizedForLegacyEmptyRows(t *testing.T) {
	// Templates installed before frontmatter became mandatory carry an empty
	// metadata_json; export must synthesize a valid record instead of
	// dead-ending the training run.
	template := db.AgentTemplate{
		ID:          "legacy-template",
		Name:        "Legacy Template",
		Description: "Installed before metadata was mandatory.",
		Content:     "# Legacy Template\n\nDo the work.",
	}
	snapshot, err := templateSnapshot(template)
	if err != nil {
		t.Fatalf("templateSnapshot: %v", err)
	}
	meta := snapshot.Metadata
	if meta.ID != "legacy-template" || meta.Name != "Legacy Template" {
		t.Fatalf("synthesized identity wrong: %+v", meta)
	}
	// Pin the exact synthesized values: the Python-side candidate validation
	// compares these against the candidate frontmatter, so drift breaks the
	// cross-repo contract.
	if len(meta.Capabilities) != 1 || meta.Capabilities[0] != "ask" {
		t.Fatalf("capabilities = %v, want [ask]", meta.Capabilities)
	}
	if len(meta.RuntimeCompatibility) != 2 || meta.RuntimeCompatibility[0] != "codex" || meta.RuntimeCompatibility[1] != "claude" {
		t.Fatalf("runtime_compatibility = %v", meta.RuntimeCompatibility)
	}
	if meta.Kind != agenttemplate.TemplateKind || meta.Version != agenttemplate.TemplateVersion {
		t.Fatalf("kind/version = %q/%d", meta.Kind, meta.Version)
	}
	if len(meta.Tags) == 0 || len(meta.Inputs) == 0 || len(meta.Outputs) == 0 {
		t.Fatalf("synthesized metadata incomplete: %+v", meta)
	}
	// Content frontmatter beats synthesis: a legacy row whose CONTENT carries
	// frontmatter must export that, never the generic defaults.
	withFM := testTemplate("with-fm", "Body.")
	withFM.MetadataJSON = ""
	fmSnapshot, err := templateSnapshot(withFM)
	if err != nil {
		t.Fatalf("templateSnapshot with frontmatter content: %v", err)
	}
	parsed, err := agenttemplate.ParseTemplateContent(withFM.Content)
	if err != nil {
		t.Fatalf("ParseTemplateContent: %v", err)
	}
	if !reflect.DeepEqual(fmSnapshot.Metadata, parsed.Metadata) {
		t.Fatalf("content frontmatter not preferred:\ngot  %+v\nwant %+v", fmSnapshot.Metadata, parsed.Metadata)
	}
	// Name/description fall back when the row is bare.
	bare, err := templateSnapshot(db.AgentTemplate{ID: "bare"})
	if err != nil {
		t.Fatalf("templateSnapshot bare: %v", err)
	}
	if bare.Metadata.Name != "bare" || bare.Metadata.Description == "" {
		t.Fatalf("bare fallbacks wrong: %+v", bare.Metadata)
	}
	// Non-empty but corrupt metadata still fails loudly.
	if _, err := templateSnapshot(db.AgentTemplate{ID: "corrupt", MetadataJSON: "{not json"}); err == nil {
		t.Fatal("corrupt metadata must still error")
	}
}

func TestEvaluatorScoreJudgePromptFieldsRoundTrip(t *testing.T) {
	score := EvaluatorScore{
		ProfileID:          "judge-default",
		JudgePromptVersion: "2026-06-19.1",
		JudgeEvaluatorID:   "evaluator-abc",
		JudgePromptHash:    "sha256:deadbeef",
	}
	encoded, err := json.Marshal(score)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var decoded EvaluatorScore
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if decoded.JudgePromptVersion != score.JudgePromptVersion {
		t.Errorf("JudgePromptVersion = %q, want %q", decoded.JudgePromptVersion, score.JudgePromptVersion)
	}
	if decoded.JudgeEvaluatorID != score.JudgeEvaluatorID {
		t.Errorf("JudgeEvaluatorID = %q, want %q", decoded.JudgeEvaluatorID, score.JudgeEvaluatorID)
	}
	if decoded.JudgePromptHash != score.JudgePromptHash {
		t.Errorf("JudgePromptHash = %q, want %q", decoded.JudgePromptHash, score.JudgePromptHash)
	}
}

func TestEvaluatorScoreJudgePromptFieldsOmitEmpty(t *testing.T) {
	encoded, err := json.Marshal(EvaluatorScore{ProfileID: "judge-default"})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	for _, field := range []string{"judge_prompt_version", "judge_evaluator_id", "judge_prompt_hash"} {
		if strings.Contains(string(encoded), field) {
			t.Errorf("empty EvaluatorScore must omit %q, got %s", field, encoded)
		}
	}
}

func projectFloatPtr(v float64) *float64 { return &v }

func TestProjectSignalScalar(t *testing.T) {
	tests := []struct {
		name    string
		score   *EvaluatorScore
		want    float64
		wantHas bool
	}{
		{name: "nil score is absent", score: nil, wantHas: false},
		{name: "empty score is absent", score: &EvaluatorScore{}, wantHas: false},
		{
			name:    "hard-fail zero is an authoritative 0",
			score:   &EvaluatorScore{Hard: projectFloatPtr(0), Soft: projectFloatPtr(0.12)},
			want:    0,
			wantHas: true,
		},
		{
			name:    "soft wins over dimensions and hard",
			score:   &EvaluatorScore{Hard: projectFloatPtr(1), Soft: projectFloatPtr(0.42), DimensionScores: map[string]float64{"a": 0.1}},
			want:    0.42,
			wantHas: true,
		},
		{
			name:    "mean of dimensions when soft absent",
			score:   &EvaluatorScore{DimensionScores: map[string]float64{"a": 0.4, "b": 0.8}},
			want:    0.6,
			wantHas: true,
		},
		{
			name:    "hard above zero used when only signal",
			score:   &EvaluatorScore{Hard: projectFloatPtr(1.0)},
			want:    1.0,
			wantHas: true,
		},
		{
			name:    "soft clamped to upper bound",
			score:   &EvaluatorScore{Soft: projectFloatPtr(1.4)},
			want:    1.0,
			wantHas: true,
		},
		{
			name:    "soft clamped to lower bound",
			score:   &EvaluatorScore{Soft: projectFloatPtr(-0.2)},
			want:    0.0,
			wantHas: true,
		},
		{
			name:    "dimension mean clamped",
			score:   &EvaluatorScore{DimensionScores: map[string]float64{"a": 1.5, "b": 1.5}},
			want:    1.0,
			wantHas: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signal := ProjectSignal(tt.score, nil, nil)
			if signal.HasScore != tt.wantHas {
				t.Fatalf("HasScore = %v, want %v", signal.HasScore, tt.wantHas)
			}
			if diff := signal.Score - tt.want; tt.wantHas && (diff > 1e-9 || diff < -1e-9) {
				t.Fatalf("Score = %v, want %v", signal.Score, tt.want)
			}
			if signal.Score < 0 || signal.Score > 1 {
				t.Fatalf("Score %v out of [0,1]", signal.Score)
			}
		})
	}
}

func TestProjectSignalAbsentScoreIsNotFabricatedMidpoint(t *testing.T) {
	signal := ProjectSignal(&EvaluatorScore{}, nil, nil)
	if signal.HasScore {
		t.Fatalf("HasScore = true, want false for absent score")
	}
	if signal.Score == 0.5 {
		t.Fatalf("absent score must not fabricate a neutral 0.5")
	}
}

func TestProjectSignalTextualOrderAndContent(t *testing.T) {
	useful, err := json.Marshal(map[string][]string{"c": {"clearest explanation"}, "a": {"crisp hero"}})
	if err != nil {
		t.Fatalf("marshal useful traits: %v", err)
	}
	rejected, err := json.Marshal(map[string][]string{"b": {"too generic"}})
	if err != nil {
		t.Fatalf("marshal rejected traits: %v", err)
	}
	required, err := json.Marshal([]string{"stronger visual identity", "responsive hero"})
	if err != nil {
		t.Fatalf("marshal required improvements: %v", err)
	}
	ranked := &RankedFeedbackEvent{
		UsefulTraits:         useful,
		RejectedTraits:       rejected,
		RequiredImprovements: required,
		Reasoning:            "C is the clearest direction.",
	}
	failure := &EvaluatorFailurePacket{OptimizerHint: "Return serialized bundle JSON."}

	feedback := ProjectSignal(nil, ranked, failure).Feedback

	for _, section := range []string{"Optimizer hint:", "Required improvements:", "Useful traits:", "Rejected traits:", "Reasoning:"} {
		if !strings.Contains(feedback, section) {
			t.Fatalf("feedback missing section %q:\n%s", section, feedback)
		}
	}
	order := []string{"Optimizer hint:", "Required improvements:", "Useful traits:", "Rejected traits:", "Reasoning:"}
	last := -1
	for _, section := range order {
		idx := strings.Index(feedback, section)
		if idx <= last {
			t.Fatalf("section %q out of order in:\n%s", section, feedback)
		}
		last = idx
	}
	// Sorted map keys: useful traits label "a" must render before "c".
	usefulIdx := strings.Index(feedback, "Useful traits:")
	aIdx := strings.Index(feedback[usefulIdx:], "a: crisp hero")
	cIdx := strings.Index(feedback[usefulIdx:], "c: clearest explanation")
	if aIdx == -1 || cIdx == -1 || aIdx > cIdx {
		t.Fatalf("useful trait keys not sorted:\n%s", feedback)
	}
	// Required improvements bulleted.
	if !strings.Contains(feedback, "- stronger visual identity") || !strings.Contains(feedback, "- responsive hero") {
		t.Fatalf("required improvements not bulleted:\n%s", feedback)
	}
}

func TestProjectSignalTextualDeterministic(t *testing.T) {
	useful, err := json.Marshal(map[string][]string{"z": {"z trait"}, "m": {"m trait"}, "a": {"a trait"}})
	if err != nil {
		t.Fatalf("marshal useful traits: %v", err)
	}
	ranked := &RankedFeedbackEvent{UsefulTraits: useful}
	first := ProjectSignal(nil, ranked, nil).Feedback
	for i := 0; i < 20; i++ {
		if got := ProjectSignal(nil, ranked, nil).Feedback; got != first {
			t.Fatalf("feedback not deterministic across runs:\n%q\n!=\n%q", got, first)
		}
	}
	// Explicit sorted order a < m < z.
	aIdx := strings.Index(first, "a: a trait")
	mIdx := strings.Index(first, "m: m trait")
	zIdx := strings.Index(first, "z: z trait")
	if !(aIdx >= 0 && aIdx < mIdx && mIdx < zIdx) {
		t.Fatalf("trait keys not in sorted order:\n%s", first)
	}
}

func TestProjectSignalTextualSkipsUndecodable(t *testing.T) {
	ranked := &RankedFeedbackEvent{
		// A JSON string is valid JSON but neither map[string][]string nor
		// []string, so the section must be skipped, not dumped raw.
		UsefulTraits:         json.RawMessage(`"not an object"`),
		RequiredImprovements: json.RawMessage(`{"oops":"not a list"}`),
		RejectedTraits:       json.RawMessage(`not even json`),
		Reasoning:            "Only reasoning survives.",
	}
	feedback := ProjectSignal(nil, ranked, nil).Feedback
	if strings.Contains(feedback, "Useful traits:") || strings.Contains(feedback, "Required improvements:") || strings.Contains(feedback, "Rejected traits:") {
		t.Fatalf("undecodable sections were not skipped:\n%s", feedback)
	}
	if strings.ContainsAny(feedback, "{}") {
		t.Fatalf("feedback leaked raw braces:\n%s", feedback)
	}
	if !strings.Contains(feedback, "Reasoning:\nOnly reasoning survives.") {
		t.Fatalf("decodable section dropped:\n%s", feedback)
	}
}

func TestProjectSignalTextualTruncatesAtCap(t *testing.T) {
	improvements := make([]string, 0, 4000)
	for i := 0; i < 4000; i++ {
		improvements = append(improvements, "improve the responsive hero layout substantially")
	}
	required, err := json.Marshal(improvements)
	if err != nil {
		t.Fatalf("marshal required improvements: %v", err)
	}
	feedback := ProjectSignal(nil, &RankedFeedbackEvent{RequiredImprovements: required}, nil).Feedback
	if len(feedback) > feedbackByteCap {
		t.Fatalf("feedback length %d exceeds cap %d", len(feedback), feedbackByteCap)
	}
	if !strings.HasSuffix(feedback, feedbackTruncationMarker) {
		t.Fatalf("truncated feedback missing marker, suffix = %q", feedback[len(feedback)-len(feedbackTruncationMarker):])
	}
	if !utf8.ValidString(feedback) {
		t.Fatalf("truncated feedback is not valid UTF-8")
	}
}

func TestProjectSignalAllEmpty(t *testing.T) {
	signal := ProjectSignal(nil, &RankedFeedbackEvent{}, &EvaluatorFailurePacket{})
	if signal.Feedback != "" {
		t.Fatalf("Feedback = %q, want empty", signal.Feedback)
	}
	if signal.HasScore {
		t.Fatalf("HasScore = true, want false for all-empty inputs")
	}
	encoded, err := json.Marshal(signal)
	if err != nil {
		t.Fatalf("marshal NormalizedSignal: %v", err)
	}
	if strings.Contains(string(encoded), "feedback") {
		t.Fatalf("empty Feedback must be omitted from JSON, got %s", encoded)
	}
}

// TestProjectSignalDoesNotChangeWirePackages pins the exact JSON bytes of an
// unchanged TrainingPackage and CandidatePackage. ProjectSignal is a return type
// only; it adds no field to any wire struct, so these golden bytes must remain
// byte-identical and must never carry the NormalizedSignal keys (score/
// has_score). This guards the additive / byte-identical optimizer-input contract.
func TestProjectSignalDoesNotChangeWirePackages(t *testing.T) {
	hard := 0.0
	soft := 0.12
	training := TrainingPackage{
		Kind:            TrainingPackageKind,
		ContractVersion: ContractVersion,
		Template:        TemplateSnapshot{ID: "planner"},
		EvalRun:         EvalRun{ID: "run-1", TemplateID: "planner", State: "review"},
		RankedFeedbackEvents: []RankedFeedbackEvent{{
			ID:                   "ranked-1",
			RunID:                "run-1",
			ItemID:               "item-1",
			Ranking:              []string{"a", "b"},
			Winner:               "a",
			UsefulTraits:         json.RawMessage(`{"a":["clear"]}`),
			RejectedTraits:       json.RawMessage(`{"b":["generic"]}`),
			RequiredImprovements: json.RawMessage(`["stronger hero"]`),
			Reasoning:            "A is clearer.",
			Reviewer:             "jerry",
			Source:               "github",
			CreatedAt:            "2026-06-02T10:00:00Z",
		}},
	}
	candidate := CandidatePackage{
		Kind:            CandidatePackageKind,
		ContractVersion: ContractVersion,
		TemplateID:      "planner",
		Candidate:       CandidateTemplate{Content: "Plan carefully."},
		Summary: CandidateSummary{
			EvaluatorScore: &EvaluatorScore{
				Hard:            &hard,
				Soft:            &soft,
				DimensionScores: map[string]float64{"artifact_contract": 0},
				Failure:         &EvaluatorFailurePacket{OptimizerHint: "Return bundle JSON."},
			},
		},
	}

	const wantTraining = `{"kind":"gitmoot-skillopt-training-package","contract_version":1,"template":{"id":"planner","version_id":"","version_number":0,"version_state":"","content_hash":"","source_repo":"","source_ref":"","source_path":"","resolved_commit":"","metadata":{"id":"","name":"","description":"","kind":"","version":0,"capabilities":null,"runtime_compatibility":null,"tags":null,"inputs":null,"outputs":null},"content":""},"eval_run":{"id":"run-1","template_id":"planner","template_version_id":"","target_repo":"","state":"review"},"items":null,"artifacts":null,"feedback_events":null,"ranked_feedback_events":[{"id":"ranked-1","run_id":"run-1","item_id":"item-1","ranking":["a","b"],"winner":"a","useful_traits":{"a":["clear"]},"rejected_traits":{"b":["generic"]},"required_improvements":["stronger hero"],"reasoning":"A is clearer.","reviewer":"jerry","source":"github","created_at":"2026-06-02T10:00:00Z"}]}`
	const wantCandidate = `{"kind":"gitmoot-skillopt-candidate-package","contract_version":1,"template_id":"planner","candidate":{"content":"Plan carefully.","metadata":{"id":"","name":"","description":"","kind":"","version":0,"capabilities":null,"runtime_compatibility":null,"tags":null,"inputs":null,"outputs":null}},"summary":{"evaluator_score":{"hard":0,"soft":0.12,"dimension_scores":{"artifact_contract":0},"failure":{"optimizer_hint":"Return bundle JSON."}}}}`

	trainingBytes, err := json.Marshal(training)
	if err != nil {
		t.Fatalf("marshal training package: %v", err)
	}
	if string(trainingBytes) != wantTraining {
		t.Fatalf("training package bytes changed:\n got %s\nwant %s", trainingBytes, wantTraining)
	}
	candidateBytes, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate package: %v", err)
	}
	if string(candidateBytes) != wantCandidate {
		t.Fatalf("candidate package bytes changed:\n got %s\nwant %s", candidateBytes, wantCandidate)
	}
	for _, key := range []string{`"has_score"`} {
		if strings.Contains(string(trainingBytes), key) || strings.Contains(string(candidateBytes), key) {
			t.Fatalf("NormalizedSignal key %s leaked into a wire package", key)
		}
	}

	// The projection still reads exactly these fields (sanity: it is callable and
	// derives from the same bytes) without mutating them.
	signal := ProjectSignal(candidate.Summary.EvaluatorScore, &training.RankedFeedbackEvents[0], candidate.Summary.EvaluatorScore.Failure)
	if !signal.HasScore || signal.Score != 0 {
		t.Fatalf("ProjectSignal over hard-fail score = %+v", signal)
	}
	reMarshaled, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("re-marshal candidate package: %v", err)
	}
	if string(reMarshaled) != wantCandidate {
		t.Fatalf("ProjectSignal mutated the candidate package")
	}
}

// TestHumanRunFeedbackContextByteIdentical pins the exact feedback_context bytes
// of a human-ranked run's exported TrainingPackage (#465 acceptance: the Mode-A
// auto-trace feedback_source override must NEVER leak into a human run). A human
// run's eval_run metadata never carries the feedback_source key, so runFeedbackSource
// falls back to imported_human_review and the bytes are unchanged from before the
// harvester existed. The companion auto-trace assertion lives in
// TestExportAutoTraceRunFeedbackSource.
func TestHumanRunFeedbackContextByteIdentical(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	// A human run carries NO feedback_source in metadata_json.
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "human-run-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "ready",
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:  "human-run-1",
		ItemID: "item-001",
		Title:  "README",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
		RunID:     "human-run-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reasoning: "More specific.",
		Reviewer:  "jerry",
		Source:    "markdown",
		SourceURL: "https://github.com/owner/repo/pull/1",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	pkg, err := ExportTrainingPackage(ctx, store, "human-run-1")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	// Pinned bytes: the historical human feedback_context, unchanged.
	want := fmt.Sprintf(
		`{"feedback_source":"imported_human_review","feedback_target":"baseline_review_outputs","review_issue":"owner/repo#1","review_run_id":"human-run-1","reviewed_skill_version":%q,"target_repo":"owner/repo"}`,
		installed.VersionID,
	)
	if string(pkg.FeedbackContext) != want {
		t.Fatalf("human feedback_context bytes changed:\n got %s\nwant %s", pkg.FeedbackContext, want)
	}
	if strings.Contains(string(pkg.FeedbackContext), FeedbackSourceAutomaticTrace) {
		t.Fatalf("auto-trace feedback_source leaked into a human run: %s", pkg.FeedbackContext)
	}
}

// TestExportAutoTraceRunFeedbackSource asserts the companion: an eval_run whose
// metadata_json carries feedback_source=automatic_trace exports with that
// run-level feedback_source (and no spurious evaluator_config), without any
// contract field or ContractVersion change (#465).
func TestExportAutoTraceRunFeedbackSource(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "auto-trace:" + installed.VersionID,
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "ready",
		Mode:              db.EvalRunModeValidate,
		MetadataJSON:      `{"feedback_source":"automatic_trace","mode":"validate"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:  "auto-trace:" + installed.VersionID,
		ItemID: "owner/repo#7",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
		RunID:     "auto-trace:" + installed.VersionID,
		ItemID:    "owner/repo#7",
		Choice:    "a",
		Reviewer:  "gitmoot-auto",
		Source:    "auto-trace",
		SourceURL: "https://github.com/owner/repo/pull/7",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	pkg, err := ExportTrainingPackage(ctx, store, "auto-trace:"+installed.VersionID)
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	var fc map[string]any
	if err := json.Unmarshal(pkg.FeedbackContext, &fc); err != nil {
		t.Fatalf("feedback context did not unmarshal: %v", err)
	}
	if fc["feedback_source"] != FeedbackSourceAutomaticTrace {
		t.Fatalf("auto-trace feedback_source = %v, want %q", fc["feedback_source"], FeedbackSourceAutomaticTrace)
	}
	if len(pkg.EvaluatorConfig) != 0 {
		t.Fatalf("auto-trace run leaked evaluator_config = %s", string(pkg.EvaluatorConfig))
	}
}
