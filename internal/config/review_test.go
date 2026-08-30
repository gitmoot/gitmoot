package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeReviewConfig(t *testing.T, body string) Paths {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Paths{ConfigFile: cfg}
}

func TestLoadReviewConfigDefaultsNativeFanoutOff(t *testing.T) {
	cfg, err := LoadReviewConfig(Paths{ConfigFile: filepath.Join(t.TempDir(), "missing.toml")})
	if err != nil {
		t.Fatalf("LoadReviewConfig(missing) error: %v", err)
	}
	if cfg.For("owner/repo").NativeFanoutEnabled {
		t.Fatal("missing config must default native fanout OFF")
	}
	if cfg.For("owner/repo").RiskTiersEnabled {
		t.Fatal("missing config must default risk tiers OFF")
	}

	cfg, err = LoadReviewConfig(writeReviewConfig(t, "[orchestrate]\ncockpit_mode = \"off\"\n"))
	if err != nil {
		t.Fatalf("LoadReviewConfig(no section) error: %v", err)
	}
	if cfg.For("owner/repo").NativeFanoutEnabled {
		t.Fatal("absent [review] section must default native fanout OFF")
	}
}

func TestLoadReviewConfigParsesGlobalFields(t *testing.T) {
	body := `
[review]
native_fanout_enabled = true
risk_tiers_enabled = true
high_risk_paths = ["**/auth/**", "cmd/**", "go.mod"]
risk_label_high = "sev:1"
risk_label_routine = "sev:routine"
`
	cfg, err := LoadReviewConfig(writeReviewConfig(t, body))
	if err != nil {
		t.Fatalf("LoadReviewConfig error: %v", err)
	}
	policy := cfg.For("owner/repo")
	if !policy.NativeFanoutEnabled || !policy.RiskTiersEnabled {
		t.Fatalf("parsed switches = %+v", policy)
	}
	if len(policy.HighRiskPaths) != 3 || policy.HighRiskPaths[1] != "cmd/**" {
		t.Fatalf("high_risk_paths = %v", policy.HighRiskPaths)
	}
	if policy.RiskLabelHigh != "sev:1" || policy.RiskLabelRoutine != "sev:routine" {
		t.Fatalf("labels = %q / %q", policy.RiskLabelHigh, policy.RiskLabelRoutine)
	}
}

func TestLoadReviewConfigRepoOverrideWins(t *testing.T) {
	body := `
[review]
native_fanout_enabled = false
[repos."owner/enabled".review]
native_fanout_enabled = true
`
	cfg, err := LoadReviewConfig(writeReviewConfig(t, body))
	if err != nil {
		t.Fatalf("LoadReviewConfig error: %v", err)
	}
	if cfg.For("owner/disabled").NativeFanoutEnabled {
		t.Fatal("repo without override must inherit global OFF")
	}
	if !cfg.For("owner/enabled").NativeFanoutEnabled {
		t.Fatal("repository override must enable native fanout")
	}
}

func TestLoadReviewConfigRejectsBadBool(t *testing.T) {
	_, err := LoadReviewConfig(writeReviewConfig(t, "[review]\nnative_fanout_enabled = yes\n"))
	if err == nil {
		t.Fatal("expected error for non-bool native_fanout_enabled")
	}
}
