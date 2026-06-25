package config

import (
	"os"
	"strings"
	"testing"
)

func TestDefaultAdmissionPolicyAllOff(t *testing.T) {
	policy := DefaultAdmissionPolicy()
	if policy.MaxConcurrentSessions != 0 {
		t.Fatalf("MaxConcurrentSessions = %d, want 0 (off)", policy.MaxConcurrentSessions)
	}
	if policy.MaxMemoryGB != 0 {
		t.Fatalf("MaxMemoryGB = %v, want 0 (off)", policy.MaxMemoryGB)
	}
	if policy.Enabled() {
		t.Fatalf("default policy must be disabled (both caps 0)")
	}
	// Priors are present so the memory gate has a per-runtime estimate when an
	// operator turns it on without setting each runtime explicitly.
	if policy.CodexMemoryGB != 0.2 || policy.ClaudeMemoryGB != 0.85 || policy.KimiMemoryGB != 0.5 || policy.DefaultMemoryGB != 0.5 {
		t.Fatalf("unexpected default priors: %+v", policy)
	}
}

func TestLoadAdmissionPolicyAbsentKeepsDefaults(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	policy, err := LoadAdmissionPolicy(paths)
	if err != nil {
		t.Fatalf("LoadAdmissionPolicy returned error: %v", err)
	}
	if policy != DefaultAdmissionPolicy() {
		t.Fatalf("absent [admission] section = %+v, want defaults %+v", policy, DefaultAdmissionPolicy())
	}
	if policy.Enabled() {
		t.Fatalf("absent [admission] section must leave the budget disabled")
	}
}

func TestLoadAdmissionPolicyParsesCaps(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[admission]
max_concurrent_sessions = 3
max_memory_gb = 4.5
codex_memory_gb = 0.1
claude_memory_gb = 1.2
kimi_memory_gb = 0.4
default_memory_gb = 0.7
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadAdmissionPolicy(paths)
	if err != nil {
		t.Fatalf("LoadAdmissionPolicy returned error: %v", err)
	}
	if policy.MaxConcurrentSessions != 3 {
		t.Fatalf("MaxConcurrentSessions = %d, want 3", policy.MaxConcurrentSessions)
	}
	if policy.MaxMemoryGB != 4.5 {
		t.Fatalf("MaxMemoryGB = %v, want 4.5", policy.MaxMemoryGB)
	}
	if policy.CodexMemoryGB != 0.1 || policy.ClaudeMemoryGB != 1.2 || policy.KimiMemoryGB != 0.4 || policy.DefaultMemoryGB != 0.7 {
		t.Fatalf("per-runtime estimates = %+v", policy)
	}
	if !policy.Enabled() {
		t.Fatalf("policy with caps set must report Enabled()")
	}
}

func TestValidateAdmissionPolicyRejectsNegative(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "max_concurrent_sessions",
			body: `
[admission]
max_concurrent_sessions = -1
`,
			wantErr: "max_concurrent_sessions must be 0",
		},
		{
			name: "max_memory_gb",
			body: `
[admission]
max_memory_gb = -2.5
`,
			wantErr: "max_memory_gb must be 0",
		},
		{
			name: "codex_memory_gb",
			body: `
[admission]
codex_memory_gb = -0.1
`,
			wantErr: "admission.codex_memory_gb must be 0 or positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize returned error: %v", err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+tt.body), 0o600); err != nil {
				t.Fatalf("write config returned error: %v", err)
			}

			_, err := LoadAdmissionPolicy(paths)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadAdmissionPolicy error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
