package skillopt

import (
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
