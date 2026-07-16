package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
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

func TestWebDataSourceConfigKeychainProjection(t *testing.T) {
	home := dashboardTestHome(t)
	path, _ := seedDashboardConfigKeychain(t, home)
	snapshot, err := (&webDataSource{home: home}).Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Keychain.File.Path != path || snapshot.Keychain.File.Status != pipelineEnvFileStatusOK {
		t.Fatalf("keychain file = %+v", snapshot.Keychain.File)
	}
	wantNames := []string{"GH_TOKEN", "OPENAI_API_KEY", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID"}
	if len(snapshot.Keychain.Keys) != len(wantNames) {
		t.Fatalf("keys = %+v", snapshot.Keychain.Keys)
	}
	for i, want := range wantNames {
		if snapshot.Keychain.Keys[i].Name != want || snapshot.Keychain.Keys[i].Grants == nil || snapshot.Keychain.Keys[i].CreatedAt == "" {
			t.Fatalf("key[%d] = %+v, want %s with non-nil grants and created_at", i, snapshot.Keychain.Keys[i], want)
		}
	}
	ghGrants := snapshot.Keychain.Keys[0].Grants
	wantGrants := []dashboard.KeychainGrantView{
		{ConsumerKind: "pipeline", ConsumerID: "alpha"},
		{ConsumerKind: "pipeline", ConsumerID: "zeta"},
	}
	if !reflect.DeepEqual(ghGrants, wantGrants) {
		t.Fatalf("GH_TOKEN grants = %+v, want %+v", ghGrants, wantGrants)
	}
	proxied := snapshot.Keychain.Keys[1]
	if proxied.Mode != db.KeychainModeProxied || proxied.ProxyUpstream != "https://api.example.test/v1" || proxied.ProxyAuth != "header:X-Service-Key" {
		t.Fatalf("proxied key = %+v", proxied)
	}
	for _, key := range []dashboard.KeychainKeyView{snapshot.Keychain.Keys[0], snapshot.Keychain.Keys[2], snapshot.Keychain.Keys[3]} {
		if key.Mode != db.KeychainModeInjected || key.ProxyUpstream != "" || key.ProxyAuth != "" || len(key.Grants) == 0 {
			t.Fatalf("injected key = %+v", key)
		}
	}
}

func TestWebDataSourceConfigKeychainLiveDriftKeepsRegistry(t *testing.T) {
	home := dashboardTestHome(t)
	path, _ := seedDashboardConfigKeychain(t, home)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&webDataSource{home: home}).Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Keychain.File.Path != path || snapshot.Keychain.File.Status != pipelineEnvFileStatusMissing || len(snapshot.Keychain.Keys) != 4 {
		t.Fatalf("drifted keychain = %+v", snapshot.Keychain)
	}
	if got := snapshot.Keychain.Keys[0].Grants; !reflect.DeepEqual(got, []dashboard.KeychainGrantView{
		{ConsumerKind: "pipeline", ConsumerID: "alpha"},
		{ConsumerKind: "pipeline", ConsumerID: "zeta"},
	}) {
		t.Fatalf("registry grants changed after file drift: %+v", got)
	}
}

func TestWebDataSourceConfigKeychainNeverLeaksValues(t *testing.T) {
	home := dashboardTestHome(t)
	_, sentinels := seedDashboardConfigKeychain(t, home)
	snapshot, err := (&webDataSource{home: home}).Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range sentinels {
		if strings.Contains(string(raw), sentinel) {
			t.Fatalf("config payload leaked keychain value %q: %s", sentinel, raw)
		}
	}
}

func TestDashboardConfigKeychainHTTPShape(t *testing.T) {
	home := dashboardTestHome(t)
	_, sentinels := seedDashboardConfigKeychain(t, home)
	server := httptest.NewServer(dashboard.Serve(&webDataSource{home: home}))
	defer server.Close()
	resp, err := http.Get(server.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{`"keychain"`, `"proxyUpstream"`, `"proxyAuth"`, `"consumerKind"`, `"consumerID"`, `"createdAt"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("config wire payload missing %s: %s", want, body)
		}
	}
	for _, unwanted := range []string{`"proxy_upstream"`, `"proxy_auth"`, `"consumer_kind"`, `"consumer_id"`, `"created_at"`} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("config wire payload contains non-camelCase key %s: %s", unwanted, body)
		}
	}
	for _, sentinel := range sentinels {
		if strings.Contains(text, sentinel) {
			t.Fatalf("config wire payload leaked keychain value %q: %s", sentinel, body)
		}
	}
	var snapshot dashboard.ConfigSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Keychain.Keys == nil || len(snapshot.Keychain.Keys) != 4 || snapshot.Keychain.Keys[0].Grants == nil {
		t.Fatalf("non-null array contract failed: %+v", snapshot.Keychain)
	}
}

func seedDashboardConfigKeychain(t *testing.T, home string) (string, []string) {
	t.Helper()
	sentinels := []string{
		"sentinel-gh-982-value",
		"sentinel-openai-982-value",
		"sentinel-telegram-bot-982-value",
		"sentinel-telegram-chat-982-value",
	}
	path := writeDefaultKeychain(t, home, strings.Join([]string{
		"GH_TOKEN=" + sentinels[0],
		"OPENAI_API_KEY=" + sentinels[1],
		"TELEGRAM_BOT_TOKEN=" + sentinels[2],
		"TELEGRAM_CHAT_ID=" + sentinels[3],
	}, "\n")+"\n")
	store := openPipelineTestStore(t, home)
	defer store.Close()
	for _, name := range []string{"zeta", "alpha", "beta"} {
		seedTestPipeline(t, store, db.Pipeline{Name: name, SpecYAML: "name: " + name + "\nstages:\n  - {id: run, cmd: echo}\n"})
	}
	for _, key := range []struct{ name, mode string }{
		{"TELEGRAM_CHAT_ID", db.KeychainModeInjected},
		{"GH_TOKEN", db.KeychainModeInjected},
		{"OPENAI_API_KEY", db.KeychainModeProxied},
		{"TELEGRAM_BOT_TOKEN", db.KeychainModeInjected},
	} {
		if _, err := store.AddKeychainKey(context.Background(), key.name, key.mode); err != nil {
			t.Fatalf("AddKeychainKey %s: %v", key.name, err)
		}
	}
	if _, err := store.ConfigureKeychainProxy(context.Background(), "OPENAI_API_KEY", "https://api.example.test/v1", db.KeychainProxyAuthHeader, "X-Service-Key"); err != nil {
		t.Fatal(err)
	}
	for _, grant := range []struct{ key, pipeline string }{
		{"GH_TOKEN", "zeta"},
		{"GH_TOKEN", "alpha"},
		{"OPENAI_API_KEY", "alpha"},
		{"TELEGRAM_BOT_TOKEN", "beta"},
		{"TELEGRAM_CHAT_ID", "zeta"},
	} {
		if _, err := store.GrantKeychainKey(context.Background(), db.KeychainConsumerPipeline, grant.pipeline, grant.key); err != nil {
			t.Fatalf("GrantKeychainKey %s/%s: %v", grant.pipeline, grant.key, err)
		}
	}
	return path, sentinels
}
