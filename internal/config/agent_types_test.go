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
max_background = 2
idle_timeout = "15m"
job_timeout = "5m"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	planner := types["planner"]
	if planner.Runtime != "codex" || planner.Template != "planner" || planner.Role != "planner" || planner.AutonomyPolicy != "workspace-write" || planner.MaxBackground != 2 || planner.IdleTimeout != "15m" || strings.Join(planner.Capabilities, ",") != "ask,review" {
		t.Fatalf("planner type = %+v", planner)
	}

	planner.MaxBackground = 3
	planner.Capabilities = []string{"ask"}
	planner.AutonomyPolicy = "read-only"
	if err := SaveAgentType(paths, planner); err != nil {
		t.Fatalf("SaveAgentType returned error: %v", err)
	}
	updated, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes after save returned error: %v", err)
	}
	if updated["planner"].MaxBackground != 3 || updated["planner"].AutonomyPolicy != "read-only" || strings.Join(updated["planner"].Capabilities, ",") != "ask" {
		t.Fatalf("updated planner type = %+v", updated["planner"])
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
	if got := types["planner"].Template; got != "" {
		t.Fatalf("retired template alias loaded template %q, want empty", got)
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

// TestSaveAgentTypePreservesHeartbeatSection is the regression guard for #533:
// an unrelated `agent type set` rewrite (SaveAgentType) must NOT delete a
// pre-existing [agents.<agent>.heartbeats.<name>] subsection. Before the fix
// removeAgentTypeBlocks stripped every "agents." section (including heartbeats),
// silently dropping the heartbeat with no error.
func TestSaveAgentTypePreservesHeartbeatSection(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[agents.repo-maintainer]
runtime = "codex"
role = "repo-maintainer"
max_background = 1

[agents.repo-maintainer.heartbeats.daily]
enabled = true
repo = "gitmoot/gitmoot"
interval = "24h"
prompt = "Review open issues and PRs."
max_concurrent = 1
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	before, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats before save: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("expected 1 heartbeat before save, got %d", len(before))
	}

	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes: %v", err)
	}
	maintainer := types["repo-maintainer"]
	maintainer.MaxBackground = 2
	if err := SaveAgentType(paths, maintainer); err != nil {
		t.Fatalf("SaveAgentType: %v", err)
	}

	after, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats after save: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("heartbeat dropped by SaveAgentType: got %d heartbeats", len(after))
	}
	if after[0].Agent != "repo-maintainer" || after[0].Name != "daily" || after[0].Prompt != "Review open issues and PRs." {
		t.Fatalf("heartbeat mangled by SaveAgentType: %+v", after[0])
	}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(content), "[agents.repo-maintainer.heartbeats.daily]") {
		t.Fatalf("heartbeat section missing from rewritten config:\n%s", string(content))
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
