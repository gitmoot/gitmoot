package skillopt

import (
	"encoding/json"
	"strings"
	"testing"
)

func validJudgeCandidatePackageJSON() string {
	return `{
		"kind": "gitmoot-skillopt-judge-candidate",
		"contract_version": 1,
		"judge_prompt_version_base": "v0",
		"n_labeled": 6,
		"variants": {
			"vue_landing_page": {
				"task_kind": "vue_landing_page",
				"n_items": 6,
				"baseline_agreement": 0.5,
				"best_agreement": 0.83,
				"best_origin": "judge_reflect_vue_landing_page",
				"judge_prompt_version": "v0+judge2",
				"accepted": true,
				"best_prompt": "Judge the landing page strictly.",
				"history": [{"agreement": 0.5}, {"agreement": 0.83}]
			},
			"_global": {
				"task_kind": "",
				"n_items": 6,
				"baseline_agreement": 0.5,
				"best_agreement": 0.5,
				"best_origin": "baseline_judge",
				"judge_prompt_version": "v0",
				"accepted": false,
				"best_prompt": "Judge the artifact."
			}
		}
	}`
}

func TestParseJudgeCandidatePackage(t *testing.T) {
	pkg, err := ParseJudgeCandidatePackage([]byte(validJudgeCandidatePackageJSON()))
	if err != nil {
		t.Fatalf("ParseJudgeCandidatePackage returned error: %v", err)
	}
	if pkg.Kind != JudgeCandidatePackageKind {
		t.Fatalf("kind = %q, want %q", pkg.Kind, JudgeCandidatePackageKind)
	}
	if pkg.ContractVersion != ContractVersion {
		t.Fatalf("contract_version = %d, want %d", pkg.ContractVersion, ContractVersion)
	}
	if pkg.JudgePromptVersionBase != "v0" || pkg.NLabeled != 6 {
		t.Fatalf("base/n_labeled = %q/%d", pkg.JudgePromptVersionBase, pkg.NLabeled)
	}
	variant, ok := pkg.Variants["vue_landing_page"]
	if !ok {
		t.Fatalf("missing vue_landing_page variant: %+v", pkg.Variants)
	}
	if !variant.Accepted || variant.BestPrompt != "Judge the landing page strictly." {
		t.Fatalf("variant = %+v", variant)
	}
	if variant.JudgePromptVersion != "v0+judge2" {
		t.Fatalf("judge_prompt_version = %q", variant.JudgePromptVersion)
	}
	if variant.BaselineAgreement != 0.5 || variant.BestAgreement != 0.83 {
		t.Fatalf("agreement = %v→%v", variant.BaselineAgreement, variant.BestAgreement)
	}
	if global := pkg.Variants["_global"]; global.Accepted {
		t.Fatalf("_global should not be accepted: %+v", global)
	}
}

func TestParseJudgeCandidatePackageRejectsWrongKind(t *testing.T) {
	data := strings.Replace(validJudgeCandidatePackageJSON(), "gitmoot-skillopt-judge-candidate", "gitmoot-skillopt-candidate-package", 1)
	if _, err := ParseJudgeCandidatePackage([]byte(data)); err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

func TestParseJudgeCandidatePackageRejectsWrongContractVersion(t *testing.T) {
	data := strings.Replace(validJudgeCandidatePackageJSON(), `"contract_version": 1`, `"contract_version": 2`, 1)
	if _, err := ParseJudgeCandidatePackage([]byte(data)); err == nil {
		t.Fatal("expected error for contract_version mismatch")
	}
}

func TestParseJudgeCandidatePackageRejectsEmpty(t *testing.T) {
	if _, err := ParseJudgeCandidatePackage([]byte("  ")); err == nil {
		t.Fatal("expected error for empty package")
	}
}

func TestParseJudgeCandidatePackageRejectsNoVariants(t *testing.T) {
	data := `{"kind":"gitmoot-skillopt-judge-candidate","contract_version":1,"variants":{}}`
	if _, err := ParseJudgeCandidatePackage([]byte(data)); err == nil {
		t.Fatal("expected error for empty variants")
	}
}

// TestEvaluationConfigForReaderRoundTrip proves the write encoding used by
// `skillopt judge promote` (a flat Evaluation map[string]string where
// judge_prompt_templates is a JSON-encoded object string) is readable back by
// the production judge-prompt reader. EvaluationConfigForReader expands the flat
// map into the nested evaluator config that judgePromptConfigFromConfig /
// EvaluatorProfileFromConfig consume, mirroring the eval-run start nesting.
func TestEvaluationConfigForReaderRoundTrip(t *testing.T) {
	// Encode the templates exactly as the write path does.
	templates := map[string]string{
		"vue_landing_page": "Judge the landing page strictly.",
		"generic":          "Judge the artifact.",
	}
	encoded, err := json.Marshal(templates)
	if err != nil {
		t.Fatalf("marshal templates: %v", err)
	}
	evaluation := map[string]string{
		"driver":                 "code-review",
		"preferred_gate":         "pairwise",
		"evaluator_id":           "landing_page_v1",
		"evaluator_model":        "gpt-evaluator",
		"judge_prompt_templates": string(encoded),
		"judge_prompt_version":   "v0+judge2",
	}

	config := EvaluationConfigForReader(evaluation)
	if len(config) == 0 {
		t.Fatal("EvaluationConfigForReader returned empty config")
	}

	// Read back through the production path.
	profile := EvaluatorProfileFromConfig(config)
	if profile == nil {
		t.Fatal("EvaluatorProfileFromConfig returned nil")
	}
	if profile.Judge == nil {
		t.Fatal("profile.Judge is nil")
	}
	payload := profile.Judge.JudgePromptConfig()
	if payload == nil {
		t.Fatal("JudgePromptConfig is nil; templates did not round-trip")
	}
	if got := payload.JudgePromptTemplates["vue_landing_page"]; got != "Judge the landing page strictly." {
		t.Fatalf("judge_prompt_templates[vue_landing_page] = %q", got)
	}
	if got := payload.JudgePromptTemplates["generic"]; got != "Judge the artifact." {
		t.Fatalf("judge_prompt_templates[generic] = %q", got)
	}
	if payload.JudgePromptVersion != "v0+judge2" {
		t.Fatalf("judge_prompt_version = %q, want v0+judge2", payload.JudgePromptVersion)
	}
}

func TestEvaluationConfigForReaderEmpty(t *testing.T) {
	if config := EvaluationConfigForReader(nil); config != nil {
		t.Fatalf("expected nil for empty map, got %q", string(config))
	}
	if config := EvaluationConfigForReader(map[string]string{}); config != nil {
		t.Fatalf("expected nil for empty map, got %q", string(config))
	}
}
