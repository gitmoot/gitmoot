package skillopt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/db"
)

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
		MetadataJSON:      `{"evaluation":{"preferred_gate":"soft"},"source":"gitmoot skillopt train start"}`,
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
	evaluation, ok := evaluator["evaluation"].(map[string]any)
	if !ok || evaluation["preferred_gate"] != "soft" {
		t.Fatalf("evaluator config = %s", string(pkg.EvaluatorConfig))
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
	if _, err := json.Marshal(pkg); err != nil {
		t.Fatalf("exported ranked package did not marshal: %v", err)
	}
}

func TestImportCandidatePackageCreatesPendingVersion(t *testing.T) {
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
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{
		ID:        "candidate-diff",
		Hash:      artifact.ContentHash([]byte("diff")),
		MediaType: "text/plain",
		SizeBytes: int64(len("diff")),
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	candidateContent := testTemplateContent("planner", "Plan carefully with a concise risk section.")
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}

	version, err := ImportCandidatePackage(ctx, store, CandidatePackage{
		Kind:            CandidatePackageKind,
		ContractVersion: ContractVersion,
		TemplateID:      "planner",
		BaseVersionID:   "planner@latest",
		Candidate: CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
		EvalReport: json.RawMessage(`{"score":0.82}`),
		Summary: CandidateSummary{
			DiffArtifactID:    "candidate-diff",
			PreferenceSummary: "Candidate is more actionable.",
		},
	}, "candidate.json")
	if err != nil {
		t.Fatalf("ImportCandidatePackage returned error: %v", err)
	}

	if version.State != "pending" || version.TemplateID != "planner" {
		t.Fatalf("candidate version = %+v", version)
	}
	after, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate after import returned error: %v", err)
	}
	if after.VersionID != current.VersionID || after.Content != current.Content {
		t.Fatalf("current template changed: before=%+v after=%+v", current, after)
	}
	latest, err := store.GetAgentTemplateReference(ctx, "planner@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest returned error: %v", err)
	}
	if latest.VersionID != version.ID || latest.Content != candidateContent {
		t.Fatalf("latest template = %+v", latest)
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	if review.BaseVersionID != current.VersionID || review.DiffArtifactID != "candidate-diff" || review.PreferenceSummary != "Candidate is more actionable." || review.EvalReportJSON != `{"score":0.82}` {
		t.Fatalf("candidate review = %+v", review)
	}
}

func TestImportCandidatePackageRejectsNoCandidateMetadata(t *testing.T) {
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

	_, err = ImportCandidatePackage(ctx, store, CandidatePackage{
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
	}, "candidate.json")
	if !errors.Is(err, ErrNoCandidate) || !strings.Contains(err.Error(), "optimizer produced no candidate: best_origin_initial_skill") {
		t.Fatalf("ImportCandidatePackage error = %v", err)
	}
}

func TestImportCandidatePackageRejectsUnchangedContent(t *testing.T) {
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
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	parsed, err := agenttemplate.ParseTemplateContent(current.Content)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}

	_, err = ImportCandidatePackage(ctx, store, CandidatePackage{
		Kind:            CandidatePackageKind,
		ContractVersion: ContractVersion,
		TemplateID:      "planner",
		BaseVersionID:   current.VersionID,
		Candidate: CandidateTemplate{
			Content:  current.Content,
			Metadata: parsed.Metadata,
		},
		Summary: CandidateSummary{},
	}, "candidate.json")
	if !errors.Is(err, ErrNoCandidate) || !strings.Contains(err.Error(), "optimizer produced no candidate: candidate content is unchanged") {
		t.Fatalf("ImportCandidatePackage error = %v", err)
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
