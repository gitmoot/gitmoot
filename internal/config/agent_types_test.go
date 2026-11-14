package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestLoadAndSaveAgentTypes(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.planner]
runtime = "codex"
template = "planner"
role = "planner"
capabilities = ["ask", "review"]
autonomy_policy = " workspace-write "
job_timeout = "5m"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	planner := types["planner"]
	if planner.Runtime != "codex" || planner.Role != "planner" || planner.AutonomyPolicy != "workspace-write" || planner.JobTimeout != "5m" || strings.Join(planner.Capabilities, ",") != "ask,review" {
		t.Fatalf("planner type = %+v", planner)
	}

	planner.JobTimeout = "7m"
	planner.Capabilities = []string{"ask"}
	planner.AutonomyPolicy = "read-only"
	if err := SaveAgentType(paths, planner); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	updated, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes after save returned error: %v", err)
	}
	if updated["planner"].JobTimeout != "7m" || updated["planner"].AutonomyPolicy != "read-only" || strings.Join(updated["planner"].Capabilities, ",") != "ask" {
		t.Fatalf("updated planner type = %+v", updated["planner"])
	}
	// #2589: agent templates are gone, so a legacy `template` key is ignored on
	// load and dropped on save.
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(content), `template = "planner"`) {
		t.Fatalf("SaveAgentType kept the removed template key:\n%s", content)
	}
}

// TestRetiredWorkerSessionKeysAreIgnored pins that a home written before #2590
// keeps loading. Its config still carries the retired instance-pool knobs
// ([agents.<type>] max_background and idle_timeout, present in every live
// agent block) and the [parallel_sessions] temp-worker policy section. Every
// loader accepts the file, the retired keys are dropped from an agent block the
// next time gitmoot rewrites it, and nothing writes them back.
func TestRetiredWorkerSessionKeysAreIgnored(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[parallel_sessions]
same_session = "fork_temp_session"
merge_back = "summary"
max_temp_sessions_per_agent = 4
eligible_actions = ["ask", "review", "implement"]

[agents.planner]
runtime = "omp"
role = "planner"
capabilities = ["ask", "review"]
autonomy_policy = "read-only"
max_background = 2
idle_timeout = "15m"
job_timeout = "5m"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := validateConfigFile(paths); err != nil {
		t.Fatalf("an old home with retired worker-session keys no longer loads: %v", err)
	}
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes: %v", err)
	}
	planner, ok := types["planner"]
	if len(types) != 1 || !ok || planner.Runtime != "omp" || planner.JobTimeout != "5m" || planner.AutonomyPolicy != "read-only" {
		t.Fatalf("agent types = %+v, want only planner with its kept keys", types)
	}
	if err := SaveAgentType(paths, planner); err != nil {
		t.Fatalf("SaveAgentType: %v", err)
	}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, retired := range []string{"max_background", "idle_timeout"} {
		if strings.Contains(string(content), retired) {
			t.Fatalf("rewritten config still carries retired key %s:\n%s", retired, content)
		}
	}
	if !strings.Contains(string(content), `job_timeout = "5m"`) {
		t.Fatalf("rewrite dropped a kept key:\n%s", content)
	}
}

func TestLoadAndSaveAgentTypeModel(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.x]
runtime = "claude"
model = "claude-opus"
effort = "high"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	if got := types["x"].Model; got != "claude-opus" {
		t.Fatalf("model = %q, want claude-opus", got)
	}
	if got := types["x"].Effort; got != "high" {
		t.Fatalf("effort = %q, want high", got)
	}

	// writeAgentTypeBlock round-trips the model.
	var builder strings.Builder
	writeAgentTypeBlock(&builder, types["x"])
	if !strings.Contains(builder.String(), `model = "claude-opus"`) {
		t.Fatalf("writeAgentTypeBlock did not write model:\n%s", builder.String())
	}
	if !strings.Contains(builder.String(), `effort = "high"`) {
		t.Fatalf("writeAgentTypeBlock did not write effort:\n%s", builder.String())
	}

	// SaveAgentType round-trips the model through the config file.
	if err := SaveAgentType(paths, types["x"]); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	reloaded, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes after save returned error: %v", err)
	}
	if got := reloaded["x"].Model; got != "claude-opus" {
		t.Fatalf("reloaded model = %q, want claude-opus", got)
	}
	if got := reloaded["x"].Effort; got != "high" {
		t.Fatalf("reloaded effort = %q, want high", got)
	}

	// An empty model omits the model line entirely.
	empty := reloaded["x"]
	empty.Model = ""
	empty.Effort = ""
	var emptyBuilder strings.Builder
	writeAgentTypeBlock(&emptyBuilder, empty)
	if strings.Contains(emptyBuilder.String(), "model =") {
		t.Fatalf("writeAgentTypeBlock wrote model for empty value:\n%s", emptyBuilder.String())
	}
	if strings.Contains(emptyBuilder.String(), "effort =") {
		t.Fatalf("writeAgentTypeBlock wrote effort for empty value:\n%s", emptyBuilder.String())
	}
}

func TestLoadAgentTypesIgnoresRetiredTemplateAlias(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	retiredAlias := "pre" + "set"
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.planner]
runtime = "codex"
`+retiredAlias+` = "planner"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	if err := SaveAgentType(paths, types["planner"]); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config returned error: %v", err)
	}
	if strings.Contains(string(content), retiredAlias+" =") {
		t.Fatalf("SaveAgentType preserved retired template alias:\n%s", string(content))
	}
	if strings.Contains(string(content), `template = "planner"`) {
		t.Fatalf("SaveAgentType wrote template from retired alias:\n%s", string(content))
	}
}

// TestRetiredHeartbeatSectionIsIgnored pins that a config.toml still carrying a
// retired [agents.<agent>.heartbeats.<name>] subsection (#2585) keeps loading:
// the dotted subsection registers no phantom agent, absorbs none of its keys
// into the parent agent, and survives an unrelated agent-type rewrite and a
// validated config write untouched.
func TestRetiredHeartbeatSectionIsIgnored(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.repo-maintainer]
runtime = "codex"
role = "repo-maintainer"
job_timeout = "5m"

[agents.repo-maintainer.heartbeats.daily]
enabled = true
repo = "gitmoot/gitmoot"
interval = "24h"
prompt = "Review open issues and PRs."
max_concurrent = 1
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes: %v", err)
	}
	if len(types) != 1 {
		t.Fatalf("agent types = %v, want only repo-maintainer (no phantom subsection agent)", types)
	}
	maintainer := types["repo-maintainer"]
	if maintainer.JobTimeout != "5m" || maintainer.Runtime != "codex" {
		t.Fatalf("repo-maintainer absorbed retired subsection keys: %+v", maintainer)
	}
	maintainer.JobTimeout = "6m"
	if err := SaveAgentType(paths, maintainer); err != nil {
		t.Fatalf("SaveAgentType: %v", err)
	}
	if err := SetConfigScalar(paths, []string{"agents", "repo-maintainer", "job_timeout"}, StringScalar("7m")); err != nil {
		t.Fatalf("validated config write with a retired subsection present: %v", err)
	}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(content), "[agents.repo-maintainer.heartbeats.daily]") {
		t.Fatalf("retired subsection dropped by an unrelated rewrite:\n%s", string(content))
	}
}

func TestLoadAgentTypesRejectsInvalidAutonomyPolicy(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.planner]
runtime = "codex"
autonomy_policy = "read_only"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	_, err := LoadAgentTypes(paths)
	if err == nil || !strings.Contains(err.Error(), "unsupported autonomy policy") {
		t.Fatalf("LoadAgentTypes error = %v, want unsupported autonomy policy", err)
	}
}

// TestSaveAgentTypePreservesTheOriginalWhenTheWriteIsCutShort is the review
// probe for #2134 F1, kept as a regression test.
//
// SaveAgentType used plain os.WriteFile, which truncates the live file before
// writing and can return after a short write. With a file-size limit imposed,
// the command returned an error while config.toml had already been cut in half
// and no longer parsed. Every caller that reads a returned error as "nothing
// was written" was wrong, and this change's cross-plane barrier rests entirely
// on that reading.
//
// RLIMIT_FSIZE is process-wide, so the failing write runs in a re-executed
// child of this test binary rather than in the test process.
func TestSaveAgentTypePreservesTheOriginalWhenTheWriteIsCutShort(t *testing.T) {
	const limit = 64 << 10

	if home := os.Getenv("GITMOOT_SHORT_WRITE_HOME"); home != "" {
		// Child. Cap the file size, then attempt a save larger than the cap.
		var rlim syscall.Rlimit
		rlim.Cur = limit
		rlim.Max = limit
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &rlim); err != nil {
			fmt.Fprintf(os.Stderr, "setrlimit: %v\n", err)
			os.Exit(3)
		}
		err := SaveAgentType(PathsForHome(home), AgentType{
			Name: "victim", Runtime: "codex", Role: "worker", AutonomyPolicy: "read-only",
		})
		if err == nil {
			fmt.Fprintln(os.Stderr, "save unexpectedly succeeded under the file-size limit")
			os.Exit(4)
		}
		os.Exit(0)
	}

	home := t.TempDir()
	paths := PathsForHome(home)
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// A config comfortably larger than the limit, so the rewrite cannot fit.
	var builder strings.Builder
	builder.WriteString(DefaultConfig(paths))
	for i := 0; i < 900; i++ {
		fmt.Fprintf(&builder, "\n[agents.filler%03d]\nruntime = \"codex\"\nrole = \"worker\"\nautonomy_policy = \"read-only\"\ntemplate = \"padding-%03d\"\n", i, i)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	original, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if len(original) <= limit {
		t.Fatalf("fixture config is %d bytes, must exceed the %d byte limit or the write would succeed",
			len(original), limit)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "GITMOOT_SHORT_WRITE_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}

	after, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config after: %v", err)
	}
	if !bytes.Equal(original, after) {
		t.Errorf("config changed after a failed write: %d bytes before, %d after; "+
			"a write that reports failure must leave the live plane untouched",
			len(original), len(after))
	}
	if _, err := LoadAgentTypes(paths); err != nil {
		t.Errorf("config no longer parses after a failed write: %v", err)
	}
}
