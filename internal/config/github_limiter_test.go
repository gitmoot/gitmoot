package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeGitHubConfig(t *testing.T, body string) Paths {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Paths{ConfigFile: file}
}

func TestLoadGitHubLimiterPolicyDefaultsWhenAbsent(t *testing.T) {
	paths := writeGitHubConfig(t, "[paths]\ndatabase = \"x\"\n")
	policy, err := LoadGitHubLimiterPolicy(paths)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := DefaultGitHubLimiterPolicy()
	if policy != want {
		t.Fatalf("policy = %+v, want default %+v", policy, want)
	}
	// Default is safe: no proactive smoothing, reactive backoff on.
	if !policy.SecondaryBackoffEnabled {
		t.Fatalf("default policy must enable secondary backoff")
	}
	if !policy.ConditionalRequests || policy.CallsPerHourWarn != 0 {
		t.Fatalf("conditional/accounting defaults = %t/%d, want true/0", policy.ConditionalRequests, policy.CallsPerHourWarn)
	}
}

func TestLoadGitHubLimiterPolicyParsesSection(t *testing.T) {
	paths := writeGitHubConfig(t, `[github]
max_concurrent = 6
min_interval = "250ms"
secondary_backoff = false
backoff_base = "30s"
backoff_max = "2m"
conditional_requests = false
calls_per_hour_warn = 4000
`)
	policy, err := LoadGitHubLimiterPolicy(paths)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if policy.MaxConcurrent != 6 {
		t.Fatalf("MaxConcurrent = %d, want 6", policy.MaxConcurrent)
	}
	if policy.MinInterval != 250*time.Millisecond {
		t.Fatalf("MinInterval = %s, want 250ms", policy.MinInterval)
	}
	if policy.SecondaryBackoffEnabled {
		t.Fatalf("secondary_backoff should parse false")
	}
	if policy.BackoffBase != 30*time.Second || policy.BackoffMax != 2*time.Minute {
		t.Fatalf("backoff bounds = %s/%s, want 30s/2m", policy.BackoffBase, policy.BackoffMax)
	}
	if policy.ConditionalRequests || policy.CallsPerHourWarn != 4000 {
		t.Fatalf("conditional/accounting = %t/%d, want false/4000", policy.ConditionalRequests, policy.CallsPerHourWarn)
	}
}

func TestLoadGitHubLimiterPolicyBareSecondsDuration(t *testing.T) {
	paths := writeGitHubConfig(t, "[github]\nmin_interval = 2\nbackoff_base = 45\n")
	policy, err := LoadGitHubLimiterPolicy(paths)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if policy.MinInterval != 2*time.Second {
		t.Fatalf("MinInterval = %s, want 2s", policy.MinInterval)
	}
	if policy.BackoffBase != 45*time.Second {
		t.Fatalf("BackoffBase = %s, want 45s", policy.BackoffBase)
	}
}

func TestLoadGitHubLimiterPolicyRejectsNegative(t *testing.T) {
	paths := writeGitHubConfig(t, "[github]\nmax_concurrent = -1\n")
	if _, err := LoadGitHubLimiterPolicy(paths); err == nil {
		t.Fatalf("expected error on negative max_concurrent")
	}
}

func TestLoadGitHubLimiterPolicyRejectsNegativeCallWarning(t *testing.T) {
	paths := writeGitHubConfig(t, "[github]\ncalls_per_hour_warn = -1\n")
	if _, err := LoadGitHubLimiterPolicy(paths); err == nil {
		t.Fatal("expected error on negative calls_per_hour_warn")
	}
}

func TestLoadGitHubLimiterPolicyIgnoresUnknownKeys(t *testing.T) {
	paths := writeGitHubConfig(t, "[github]\nfuture_knob = \"x\"\nmax_concurrent = 3\n")
	policy, err := LoadGitHubLimiterPolicy(paths)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if policy.MaxConcurrent != 3 {
		t.Fatalf("MaxConcurrent = %d, want 3 (unknown key ignored)", policy.MaxConcurrent)
	}
}
