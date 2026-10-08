package config

import (
	"os"
	"strings"
	"testing"
)

func loadMergeGateText(t *testing.T, text string) (MergeGateConfig, error) {
	t.Helper()
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+text), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	return LoadMergeGatePolicy(paths)
}

func TestLoadMergeGatePolicyLowRiskRepoOverride(t *testing.T) {
	cfg, err := loadMergeGateText(t, `
[merge_gate]
auto_merge = false

[repos."gitmoot/test-check".merge_gate]
auto_merge = "low_risk"
`)
	if err != nil {
		t.Fatalf("LoadMergeGatePolicy: %v", err)
	}
	if got := cfg.For("gitmoot/test-check"); !got.AutoMerge || !got.LowRiskOnly {
		t.Fatalf("opted-in repo policy = %+v, want low-risk auto-merge", got)
	}
	if got := cfg.For("jerryfane/noted"); got.AutoMerge || got.LowRiskOnly {
		t.Fatalf("non-opted repo policy = %+v, want the global kill switch unchanged", got)
	}
}

func TestLoadMergeGatePolicyRefusesLowRiskForGitmootGitmoot(t *testing.T) {
	_, err := loadMergeGateText(t, `
[repos."gitmoot/gitmoot".merge_gate]
auto_merge = "low_risk"
`)
	if err == nil || !strings.Contains(err.Error(), "always requires review") {
		t.Fatalf("err = %v, want gitmoot/gitmoot low_risk refused", err)
	}
}

func TestLoadMergeGatePolicyRejectsLowRiskGloballyAndUnknownStrings(t *testing.T) {
	for _, text := range []string{"\n[merge_gate]\nauto_merge = \"low_risk\"\n", "\n[repos.\"o/r\".merge_gate]\nauto_merge = \"sometimes\"\n"} {
		if _, err := loadMergeGateText(t, text); err == nil {
			t.Fatalf("LoadMergeGatePolicy accepted %q", text)
		}
	}
}
