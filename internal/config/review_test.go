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

func TestLoadReviewPolicyDefaultsOff(t *testing.T) {
	// No file at all -> default (off), no error.
	policy, err := LoadReviewPolicy(Paths{ConfigFile: filepath.Join(t.TempDir(), "missing.toml")})
	if err != nil {
		t.Fatalf("LoadReviewPolicy(missing) error: %v", err)
	}
	if policy.RiskTiersEnabled {
		t.Fatal("missing config must default risk tiers OFF")
	}

	// A config with no [review] section -> default off.
	policy, err = LoadReviewPolicy(writeReviewConfig(t, "[orchestrate]\ncockpit_mode = \"off\"\n"))
	if err != nil {
		t.Fatalf("LoadReviewPolicy(no section) error: %v", err)
	}
	if policy.RiskTiersEnabled {
		t.Fatal("absent [review] section must default OFF")
	}
}

func TestLoadReviewPolicyParsesFields(t *testing.T) {
	body := `
[review]
risk_tiers_enabled = true
high_risk_paths = ["**/auth/**", "cmd/**", "go.mod"]
risk_label_high = "sev:1"
risk_label_routine = "sev:routine"
`
	policy, err := LoadReviewPolicy(writeReviewConfig(t, body))
	if err != nil {
		t.Fatalf("LoadReviewPolicy error: %v", err)
	}
	if !policy.RiskTiersEnabled {
		t.Fatal("risk_tiers_enabled = true not parsed")
	}
	if len(policy.HighRiskPaths) != 3 || policy.HighRiskPaths[1] != "cmd/**" {
		t.Fatalf("high_risk_paths = %v", policy.HighRiskPaths)
	}
	if policy.RiskLabelHigh != "sev:1" || policy.RiskLabelRoutine != "sev:routine" {
		t.Fatalf("labels = %q / %q", policy.RiskLabelHigh, policy.RiskLabelRoutine)
	}
}

func TestLoadReviewPolicyRejectsBadBool(t *testing.T) {
	_, err := LoadReviewPolicy(writeReviewConfig(t, "[review]\nrisk_tiers_enabled = yes\n"))
	if err == nil {
		t.Fatal("expected error for non-bool risk_tiers_enabled")
	}
}
