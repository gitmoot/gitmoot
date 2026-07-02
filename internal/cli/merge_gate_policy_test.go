package cli

import (
	"os"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestApplyMergeGatePolicyOffByDefault(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	gate := workflow.PolicyMergeGate{}
	applyMergeGatePolicy(&gate, paths.Home, "jerryfane/noted")
	if gate.RequireExternalCI {
		t.Fatalf("RequireExternalCI = true, want off by default")
	}
	if gate.MinCIWait != config.DefaultMinCIWait {
		t.Fatalf("MinCIWait = %v, want default %v", gate.MinCIWait, config.DefaultMinCIWait)
	}
	if gate.MaxCIWait != config.DefaultMaxCIWait {
		t.Fatalf("MaxCIWait = %v, want default %v", gate.MaxCIWait, config.DefaultMaxCIWait)
	}
}

func TestApplyMergeGatePolicyReadsPerRepoKnob(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+`
[merge_gate]
min_ci_wait = "45s"
max_ci_wait = "7m"

[repos."jerryfane/noted".merge_gate]
require_external_ci = true
`), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	noted := workflow.PolicyMergeGate{}
	applyMergeGatePolicy(&noted, paths.Home, "jerryfane/noted")
	if !noted.RequireExternalCI {
		t.Fatalf("noted RequireExternalCI = false, want true from per-repo override")
	}
	if noted.MinCIWait != 45*time.Second {
		t.Fatalf("noted MinCIWait = %v, want inherited 45s", noted.MinCIWait)
	}
	if noted.MaxCIWait != 7*time.Minute {
		t.Fatalf("noted MaxCIWait = %v, want inherited 7m", noted.MaxCIWait)
	}

	other := workflow.PolicyMergeGate{}
	applyMergeGatePolicy(&other, paths.Home, "jerryfane/gitmoot")
	if other.RequireExternalCI {
		t.Fatalf("non-override repo RequireExternalCI = true, want false")
	}
	if other.MinCIWait != 45*time.Second {
		t.Fatalf("non-override repo MinCIWait = %v, want global 45s", other.MinCIWait)
	}
	if other.MaxCIWait != 7*time.Minute {
		t.Fatalf("non-override repo MaxCIWait = %v, want global 7m", other.MaxCIWait)
	}
}

func TestApplyMergeGatePolicyEmptyHomeIsNoop(t *testing.T) {
	gate := workflow.PolicyMergeGate{}
	applyMergeGatePolicy(&gate, "", "jerryfane/noted")
	if gate.RequireExternalCI || gate.MinCIWait != 0 || gate.MaxCIWait != 0 {
		t.Fatalf("empty home must leave the gate untouched, got %+v", gate)
	}
}
