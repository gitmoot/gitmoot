package cli

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestDashboardConfigProjectionAllowlist(t *testing.T) {
	want := []string{
		"chat.auto_respond",
		"github.max_concurrent",
		"github.min_interval",
		"memory.cluster_depth_cap",
		"memory.cluster_fanout",
		"memory.cluster_fanout_keep",
		"memory.default_enroll",
		"memory.disabled",
		"memory.distill_all_jobs",
		"memory.distill_at_terminal",
		"memory.distill_max_per_job",
		"memory.distill_successes",
		"memory.groom_llm_total_max_per_run",
		"memory.groom_quality",
		"memory.groom_quality_max_per_run",
		"memory.groom_quality_min_age",
		"memory.groom_split_llm",
		"memory.groom_split_llm_max_per_run",
		"memory.groom_split_llm_model",
		"memory.groom_split_llm_runtime",
		"memory.groom_stale",
		"memory.groom_stale_age",
		"memory.harvest_effort",
		"memory.harvest_enabled",
		"memory.harvest_max_jobs_per_sweep",
		"memory.harvest_max_per_job",
		"memory.harvest_model",
		"memory.harvest_runtime",
		"memory.ingest_auto_confirm",
		"memory.max_entries",
		"memory.token_budget",
		"memory.pipelines.groom_propose",
		"memory.pipelines.groom_propose_jitter",
		"memory.pipelines.ingest_sweep",
		"memory.pipelines.ingest_sweep_jitter",
		"orchestrate.blocked_ttl",
		"skillopt.auto_promote",
		"skillopt.auto_promote_canary",
		"skillopt.auto_promote_require_external_ci",
		"skillopt.auto_promote_require_measured_judge",
		"skillopt.auto_trace_enabled",
		"skillopt.cross_family_review_enabled",
		"skillopt.deterministic_checkers_enabled",
		"skillopt.gate_enabled",
		"skillopt.hard_verifiers_enabled",
		"skillopt.mode_b_judge_enabled",
		"skillopt.pace_enabled",
		"workflow.implement_base",
	}

	got := make([]string, 0, len(dashboardConfigProjection))
	validKinds := map[string]bool{"flag": true, "int": true, "string": true, "duration": true, "list": true}
	for i, row := range dashboardConfigProjection {
		if row.section == "" || row.key == "" || row.kind == "" || row.doc == "" || row.value == nil || row.defaultValue == nil {
			t.Fatalf("projection row %d is incomplete: %+v", i, row)
		}
		if !validKinds[row.kind] {
			t.Fatalf("projection row %s.%s has invalid kind %q", row.section, row.key, row.kind)
		}
		got = append(got, row.section+"."+row.key)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projection keys =\n%q\nwant\n%q", got, want)
	}
	for i := 1; i < len(dashboardConfigProjection); i++ {
		prev, next := dashboardConfigProjection[i-1], dashboardConfigProjection[i]
		if prev.section > next.section || (prev.section == next.section && prev.key >= next.key) {
			t.Fatalf("projection rows %d and %d are not in section/key order: %s.%s then %s.%s", i-1, i, prev.section, prev.key, next.section, next.key)
		}
	}

	defaults := dashboardConfigSettings{
		memory: config.DefaultMemorySettings(), chat: config.DefaultChatSettings(),
		orchestrate: config.DefaultOrchestratePolicy(), github: config.DefaultGitHubLimiterPolicy(),
		skillopt: config.DefaultSkillOptPolicy(),
	}
	if first, second := projectDashboardConfig(defaults, defaults), projectDashboardConfig(defaults, defaults); !reflect.DeepEqual(first, second) {
		t.Fatal("unchanged config projection is not deterministic")
	}
}

func TestWebDataSourceConfigOverridesAgentsMetadataAndSecretSafety(t *testing.T) {
	home := dashboardTestHome(t)
	paths := config.PathsForHome(home)
	base, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	const secret = "ghp_this_must_never_leave_the_config_file"
	contents := string(base) + `

[memory]
groom_split_llm = true

[agents.worker]
runtime = "codex"
model = "gpt-test"
memory = true
chat_autorespond = true
capabilities = ["ask", "implement"]
autonomy_policy = "workspace-write"
max_background = 9

[experimental]
private_token = "` + secret + `"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	wantMod := time.Unix(1_800_000_000, 123_000_000)
	if err := os.Chtimes(paths.ConfigFile, wantMod, wantMod); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name: "worker", Runtime: "claude", Model: "db-model", Capabilities: []string{"review"}, AutonomyPolicy: "read-only",
	}); err != nil {
		store.Close()
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	snapshot, err := (&webDataSource{home: home}).Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if snapshot.ContractVersion != 1 || !snapshot.Exists || snapshot.Path != paths.ConfigFile {
		t.Fatalf("snapshot metadata = version %d exists %v path %q", snapshot.ContractVersion, snapshot.Exists, snapshot.Path)
	}
	if snapshot.ModifiedAt != wantMod.UnixMilli() {
		t.Fatalf("modified_at = %d, want %d", snapshot.ModifiedAt, wantMod.UnixMilli())
	}

	knob := dashboardConfigKnob(t, snapshot, "memory", "groom_split_llm")
	if value, ok := knob.Value.(bool); !ok || !value || knob.IsDefault {
		t.Fatalf("groom_split_llm = value %#v default %#v is_default %v", knob.Value, knob.Default, knob.IsDefault)
	}
	if defaultValue, ok := knob.Default.(bool); !ok || defaultValue {
		t.Fatalf("groom_split_llm default = %#v, want false", knob.Default)
	}

	if len(snapshot.Agents) != 1 {
		t.Fatalf("agents = %+v, want one worker", snapshot.Agents)
	}
	agent := snapshot.Agents[0]
	if agent.Name != "worker" || agent.Runtime != "codex" || agent.Model != "gpt-test" || !agent.Memory || !agent.ChatAutorespond || agent.MaxBackground != 9 {
		t.Fatalf("worker config = %+v", agent)
	}
	if !reflect.DeepEqual(agent.Capabilities, []string{"ask", "implement"}) || agent.AutonomyPolicy != "workspace-write" {
		t.Fatalf("worker behavior config = %+v", agent)
	}

	if !containsString(snapshot.UnknownKeys, "experimental.private_token") {
		t.Fatalf("unknown keys = %v, want experimental.private_token", snapshot.UnknownKeys)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("snapshot leaked secret-looking unknown value: %s", encoded)
	}
}

func dashboardConfigKnob(t *testing.T, snapshot dashboard.ConfigSnapshot, section, key string) dashboard.ConfigKnob {
	t.Helper()
	for _, candidateSection := range snapshot.Sections {
		if candidateSection.Name != section {
			continue
		}
		for _, knob := range candidateSection.Knobs {
			if knob.Key == key {
				return knob
			}
		}
	}
	t.Fatalf("missing config knob %s.%s", section, key)
	return dashboard.ConfigKnob{}
}
