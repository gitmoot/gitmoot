package config

import (
	"os"
	"testing"
)

// TestLoadSkillOptPolicyGateDefaultsDisabled: the replay gate is OFF by default and
// carries no corpus/replay defaults (#627).
func TestLoadSkillOptPolicyGateDefaultsDisabled(t *testing.T) {
	policy := DefaultSkillOptPolicy()
	if policy.Gate || policy.GateEnabled() {
		t.Fatalf("replay gate must default OFF, got %+v", policy)
	}
	if policy.GateCorpusPath != "" || policy.GateReplayCommand != "" {
		t.Fatalf("gate corpus/replay must default empty, got %q / %q", policy.GateCorpusPath, policy.GateReplayCommand)
	}
}

// TestLoadSkillOptPolicyGateParsesKnobs: gate_enabled/gate_corpus/gate_replay_command
// parse; the gate is standalone (no auto_trace dependency) so GateEnabled() is true
// with gate_enabled alone (#627).
func TestLoadSkillOptPolicyGateParsesKnobs(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[skillopt]
gate_enabled = true
gate_corpus = /etc/gitmoot/corpus.json
gate_replay_command = sh replay.sh
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		t.Fatalf("LoadSkillOptPolicy returned error: %v", err)
	}
	if !policy.Gate || !policy.GateEnabled() {
		t.Fatalf("gate_enabled = true should turn the gate on standalone, got %+v", policy)
	}
	if policy.GateCorpusPath != "/etc/gitmoot/corpus.json" {
		t.Fatalf("gate_corpus = %q, want /etc/gitmoot/corpus.json", policy.GateCorpusPath)
	}
	if policy.GateReplayCommand != "sh replay.sh" {
		t.Fatalf("gate_replay_command = %q, want sh replay.sh", policy.GateReplayCommand)
	}
}
