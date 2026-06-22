package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// cliJudgeTemplate installs a planner template with an existing Evaluation map,
// including a pre-existing judge_prompt_templates sibling so promotion-merge can
// be asserted to preserve other task kinds.
func cliJudgeTemplate(id string) db.AgentTemplate {
	content := agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
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
		Evaluation: map[string]string{
			"evaluator_id":           "landing_page_v1",
			"evaluator_model":        "gpt-evaluator",
			"preferred_gate":         "pairwise",
			"judge_prompt_templates": `{"generic":"Existing generic prompt."}`,
			"judge_prompt_version":   "v0",
		},
	}, "# Planner\n\nPlan the work.\n")
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

func writeJudgePackage(t *testing.T, dir string) string {
	t.Helper()
	pkg := map[string]any{
		"kind":                      skillopt.JudgeCandidatePackageKind,
		"contract_version":          skillopt.ContractVersion,
		"judge_prompt_version_base": "v0",
		"n_labeled":                 6,
		"variants": map[string]any{
			"vue_landing_page": map[string]any{
				"task_kind":            "vue_landing_page",
				"n_items":              6,
				"baseline_agreement":   0.5,
				"best_agreement":       0.83,
				"best_origin":          "judge_reflect_vue_landing_page",
				"judge_prompt_version": "v0+judge2",
				"accepted":             true,
				"best_prompt":          "Judge the landing page strictly.",
			},
			"refused_kind": map[string]any{
				"task_kind":            "refused_kind",
				"n_items":              4,
				"baseline_agreement":   0.6,
				"best_agreement":       0.6,
				"best_origin":          "baseline_judge",
				"judge_prompt_version": "v0",
				"accepted":             false,
				"best_prompt":          "Should not be promoted.",
			},
		},
	}
	encoded, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal judge package: %v", err)
	}
	path := filepath.Join(dir, "judge-candidate.json")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write judge package: %v", err)
	}
	return path
}

func newJudgeTestHome(t *testing.T) (string, config.Paths) {
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
	if err := store.UpsertAgentTemplate(context.Background(), cliJudgeTemplate("planner")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	return home, paths
}

func TestSkillOptJudgePromotePreviewWritesNothing(t *testing.T) {
	home, paths := newJudgeTestHome(t)
	pkgPath := writeJudgePackage(t, t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "judge", "promote", "--home", home, "--template", "planner", "--task-kind", "vue_landing_page", "--file", pkgPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("preview exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "preview only") {
		t.Fatalf("preview stdout missing preview notice: %q", out)
	}
	if !strings.Contains(out, "0.500 → 0.830") {
		t.Fatalf("preview stdout missing agreement delta: %q", out)
	}
	if !strings.Contains(out, "Judge the landing page strictly.") {
		t.Fatalf("preview stdout missing prompt preview: %q", out)
	}

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON)
	if err != nil {
		t.Fatalf("UnmarshalMetadata returned error: %v", err)
	}
	if got := metadata.Evaluation["judge_prompt_version"]; got != "v0" {
		t.Fatalf("preview must not change metadata; judge_prompt_version = %q, want v0", got)
	}
	if strings.Contains(metadata.Evaluation["judge_prompt_templates"], "vue_landing_page") {
		t.Fatalf("preview must not write the new template: %q", metadata.Evaluation["judge_prompt_templates"])
	}
	outcomes, err := store.ListSkillOptJudgeOutcomes(context.Background(), "planner")
	if err != nil {
		t.Fatalf("ListSkillOptJudgeOutcomes returned error: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("preview must not write an audit row; got %d", len(outcomes))
	}
}

func TestSkillOptJudgePromoteRefusesNotAccepted(t *testing.T) {
	home, paths := newJudgeTestHome(t)
	pkgPath := writeJudgePackage(t, t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "judge", "promote", "--home", home, "--template", "planner", "--task-kind", "refused_kind", "--file", pkgPath, "--yes"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1 for non-accepted variant, got %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not accepted") {
		t.Fatalf("stderr missing accepted-gate message: %q", stderr.String())
	}

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON)
	if err != nil {
		t.Fatalf("UnmarshalMetadata returned error: %v", err)
	}
	if strings.Contains(metadata.Evaluation["judge_prompt_templates"], "refused_kind") {
		t.Fatalf("refused variant must not be written: %q", metadata.Evaluation["judge_prompt_templates"])
	}
}

func TestSkillOptJudgePromoteMissingTaskKind(t *testing.T) {
	home, _ := newJudgeTestHome(t)
	pkgPath := writeJudgePackage(t, t.TempDir())
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "judge", "promote", "--home", home, "--template", "planner", "--task-kind", "nonexistent", "--file", pkgPath, "--yes"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for missing task-kind, got 0 (stdout=%s)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "not found in package variants") {
		t.Fatalf("stderr missing not-found message: %q", stderr.String())
	}
}

// TestSkillOptJudgePromoteApplyRoundTrip is the load-bearing acceptance test:
// promote --yes, then read the persisted metadata back through the production
// reader and assert judge_prompt_templates[task_kind] == best_prompt with the
// version bumped, while preserving the sibling task kind.
func TestSkillOptJudgePromoteApplyRoundTrip(t *testing.T) {
	home, paths := newJudgeTestHome(t)
	pkgPath := writeJudgePackage(t, t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "judge", "promote", "--home", home, "--template", "planner", "--task-kind", "vue_landing_page", "--file", pkgPath, "--yes", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("apply exit code = %d, stderr=%s", code, stderr.String())
	}
	var result struct {
		Applied               bool    `json:"applied"`
		TaskKind              string  `json:"task_kind"`
		PreviousPromptVersion string  `json:"previous_judge_prompt_version"`
		NewPromptVersion      string  `json:"new_judge_prompt_version"`
		AgreementDelta        float64 `json:"agreement_delta"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result JSON: %v\n%s", err, stdout.String())
	}
	if !result.Applied {
		t.Fatalf("result.Applied = false, want true: %+v", result)
	}
	if result.PreviousPromptVersion != "v0" || result.NewPromptVersion != "v0+judge2" {
		t.Fatalf("version bump = %q→%q", result.PreviousPromptVersion, result.NewPromptVersion)
	}

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON)
	if err != nil {
		t.Fatalf("UnmarshalMetadata returned error: %v", err)
	}

	// Version was bumped in the durable home.
	if got := metadata.Evaluation["judge_prompt_version"]; got != "v0+judge2" {
		t.Fatalf("judge_prompt_version = %q, want v0+judge2", got)
	}

	// Round-trip through the production reader: the persisted flat Evaluation map
	// expands into the nested config judgePromptConfigFromConfig consumes.
	config := skillopt.EvaluationConfigForReader(metadata.Evaluation)
	profile := skillopt.EvaluatorProfileFromConfig(config)
	if profile == nil || profile.Judge == nil {
		t.Fatalf("EvaluatorProfileFromConfig returned nil judge: %+v", profile)
	}
	payload := profile.Judge.JudgePromptConfig()
	if payload == nil {
		t.Fatal("JudgePromptConfig is nil; promoted prompt did not round-trip")
	}
	if got := payload.JudgePromptTemplates["vue_landing_page"]; got != "Judge the landing page strictly." {
		t.Fatalf("round-trip judge_prompt_templates[vue_landing_page] = %q", got)
	}
	// Sibling task kind preserved.
	if got := payload.JudgePromptTemplates["generic"]; got != "Existing generic prompt." {
		t.Fatalf("sibling task kind not preserved: generic = %q", got)
	}
	if payload.JudgePromptVersion != "v0+judge2" {
		t.Fatalf("round-trip judge_prompt_version = %q", payload.JudgePromptVersion)
	}

	// Audit row written.
	outcomes, err := store.ListSkillOptJudgeOutcomes(context.Background(), "planner")
	if err != nil {
		t.Fatalf("ListSkillOptJudgeOutcomes returned error: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("expected 1 judge outcome row, got %d", len(outcomes))
	}
	outcome := outcomes[0]
	if outcome.HumanDecision != "promoted" {
		t.Fatalf("human_decision = %q, want promoted", outcome.HumanDecision)
	}
	if outcome.JudgePromptVersion != "v0+judge2" {
		t.Fatalf("audit judge_prompt_version = %q", outcome.JudgePromptVersion)
	}
	if outcome.Direction != db.SkillOptJudgeDirectionAgreeAccept {
		t.Fatalf("audit direction = %q, want %q", outcome.Direction, db.SkillOptJudgeDirectionAgreeAccept)
	}
	if !strings.Contains(outcome.Reason, "v0") || !strings.Contains(outcome.Reason, "v0+judge2") {
		t.Fatalf("audit reason missing version delta: %q", outcome.Reason)
	}
	if !strings.Contains(outcome.Reason, "0.500") || !strings.Contains(outcome.Reason, "0.830") {
		t.Fatalf("audit reason missing agreement delta: %q", outcome.Reason)
	}
}
