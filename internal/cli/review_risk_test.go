package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestApplyReviewPolicyOffByDefault(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A config with no [review] section.
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte("[orchestrate]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)
	if engine.RiskTiersEnabled {
		t.Fatal("applyReviewPolicy must leave risk tiers OFF when [review] is absent")
	}
	if engine.NativeReviewFanoutEnabled == nil || engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("applyReviewPolicy must install native fanout OFF when [review] is absent")
	}
}

func TestApplyReviewPolicyEnabledFromConfig(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[review]\nnative_fanout_enabled = true\nrisk_tiers_enabled = true\nhigh_risk_paths = [\"cmd/**\"]\nrisk_label_high = \"sev:1\"\n[repos.\"owner/off\".review]\nnative_fanout_enabled = false\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)
	if !engine.RiskTiersEnabled {
		t.Fatal("applyReviewPolicy must enable risk tiers from [review].risk_tiers_enabled")
	}
	if len(engine.HighRiskPaths) != 1 || engine.HighRiskPaths[0] != "cmd/**" {
		t.Fatalf("HighRiskPaths = %v", engine.HighRiskPaths)
	}
	if engine.RiskLabelHigh != "sev:1" {
		t.Fatalf("RiskLabelHigh = %q", engine.RiskLabelHigh)
	}
	if engine.NativeReviewFanoutEnabled == nil || !engine.NativeReviewFanoutEnabled("owner/on") {
		t.Fatal("global native_fanout_enabled = true was not applied")
	}
	if engine.NativeReviewFanoutEnabled("owner/off") {
		t.Fatal("repository native_fanout_enabled = false override was not applied")
	}
}

func TestApplyReviewPolicyEmptyHomeIsOff(t *testing.T) {
	var engine workflow.Engine
	applyReviewPolicy(&engine, "")
	if engine.RiskTiersEnabled {
		t.Fatal("empty home must resolve to risk tiers OFF")
	}
	if engine.NativeReviewFanoutEnabled == nil || engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("empty home must resolve native fanout OFF")
	}
}
