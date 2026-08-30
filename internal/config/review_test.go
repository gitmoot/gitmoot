package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
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
	if cfg.For("owner/repo").BlockingSeverity != reviewseverity.DefaultBlocking {
		t.Fatalf("missing config blocking severity = %q, want %q", cfg.For("owner/repo").BlockingSeverity, reviewseverity.DefaultBlocking)
	}

	cfg, err = LoadReviewConfig(writeReviewConfig(t, "[orchestrate]\ncockpit_mode = \"off\"\n"))
	if err != nil {
		t.Fatalf("LoadReviewConfig(no section) error: %v", err)
	}
	if cfg.For("owner/repo").NativeFanoutEnabled {
		t.Fatal("absent [review] section must default native fanout OFF")
	}
	if cfg.For("owner/repo").BlockingSeverity != reviewseverity.DefaultBlocking {
		t.Fatalf("absent [review] blocking severity = %q, want %q", cfg.For("owner/repo").BlockingSeverity, reviewseverity.DefaultBlocking)
	}
}

func TestLoadReviewConfigParsesGlobalFields(t *testing.T) {
	body := `
[review]
native_fanout_enabled = true
blocking_severity = "p2"
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
	if policy.BlockingSeverity != reviewseverity.P2 {
		t.Fatalf("blocking_severity = %q, want P2", policy.BlockingSeverity)
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
blocking_severity = "P2"
[repos."owner/enabled".review]
native_fanout_enabled = true
blocking_severity = "P1"
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
	if cfg.For("owner/disabled").BlockingSeverity != reviewseverity.P2 {
		t.Fatalf("repository without override blocking severity = %q, want P2", cfg.For("owner/disabled").BlockingSeverity)
	}
	if cfg.For("owner/enabled").BlockingSeverity != reviewseverity.P1 {
		t.Fatalf("repository override blocking severity = %q, want P1", cfg.For("owner/enabled").BlockingSeverity)
	}
}

func TestLoadReviewConfigRejectsBadBool(t *testing.T) {
	_, err := LoadReviewConfig(writeReviewConfig(t, "[review]\nnative_fanout_enabled = yes\n"))
	if err == nil {
		t.Fatal("expected error for non-bool native_fanout_enabled")
	}
}

func TestLoadReviewConfigRejectsBadBlockingSeverity(t *testing.T) {
	_, err := LoadReviewConfig(writeReviewConfig(t, "[review]\nblocking_severity = \"P4\"\n"))
	if err == nil || !strings.Contains(err.Error(), "P0, P1, P2, P3") {
		t.Fatalf("bad blocking severity error = %v, want canonical choices", err)
	}
}

func TestDefaultConfigDocumentsReviewBlockingSeverity(t *testing.T) {
	content := DefaultConfig(PathsForHome(t.TempDir()))
	for _, want := range []string{
		`# blocking_severity = "P3"`,
		`# [repos."owner/repo".review]`,
		`# blocking_severity = "P1"`,
		`findings remain posted`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("DefaultConfig missing %q", want)
		}
	}
}
