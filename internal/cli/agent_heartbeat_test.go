package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// TestAgentHeartbeatAddListShowEnableDisableRemove exercises the full write-side
// CLI round-trip through the config-edit seam, asserting each command reads back
// through config.LoadHeartbeats.
func TestAgentHeartbeatAddListShowEnableDisableRemove(t *testing.T) {
	home := t.TempDir()
	run := func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := Run(append(args, "--home", home), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}

	out, errOut, code := run("agent", "heartbeat", "add", "repo-maintainer", "daily",
		"--repo", "jerryfane/gitmoot", "--interval", "24h", "--jitter", "15m",
		"--prompt", "Review open issues and PRs.", "--enabled")
	if code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "configured heartbeat repo-maintainer/daily") {
		t.Fatalf("add stdout=%q", out)
	}

	out, errOut, code = run("agent", "heartbeat", "list")
	if code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "repo-maintainer/daily") || !strings.Contains(out, "enabled") || !strings.Contains(out, "24h") {
		t.Fatalf("list stdout=%q", out)
	}

	out, errOut, code = run("agent", "heartbeat", "show", "repo-maintainer", "daily")
	if code != 0 {
		t.Fatalf("show exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "enabled: true") || !strings.Contains(out, "interval: 24h") ||
		!strings.Contains(out, "action: ask") || !strings.Contains(out, "prompt: Review open issues and PRs.") {
		t.Fatalf("show stdout=%q", out)
	}

	if _, errOut, code = run("agent", "heartbeat", "disable", "repo-maintainer", "daily"); code != 0 {
		t.Fatalf("disable exit=%d stderr=%s", code, errOut)
	}
	out, _, _ = run("agent", "heartbeat", "show", "repo-maintainer", "daily")
	if !strings.Contains(out, "enabled: false") {
		t.Fatalf("expected disabled after disable, show=%q", out)
	}

	if _, errOut, code = run("agent", "heartbeat", "enable", "repo-maintainer", "daily"); code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, errOut)
	}
	out, _, _ = run("agent", "heartbeat", "show", "repo-maintainer", "daily")
	if !strings.Contains(out, "enabled: true") {
		t.Fatalf("expected enabled after enable, show=%q", out)
	}

	if _, errOut, code = run("agent", "heartbeat", "remove", "repo-maintainer", "daily"); code != 0 {
		t.Fatalf("remove exit=%d stderr=%s", code, errOut)
	}
	out, _, _ = run("agent", "heartbeat", "list")
	if strings.Contains(out, "repo-maintainer/daily") {
		t.Fatalf("expected empty list after remove, got %q", out)
	}
	// Removing again reports not found (exit 1).
	if _, _, code = run("agent", "heartbeat", "remove", "repo-maintainer", "daily"); code == 0 {
		t.Fatalf("expected non-zero exit removing missing heartbeat")
	}
}

// TestAgentHeartbeatAddPreservesAgentType asserts the no-clobber guard end-to-end:
// adding a heartbeat through the CLI never drops an existing agent-type block.
func TestAgentHeartbeatAddPreservesAgentType(t *testing.T) {
	home := t.TempDir()
	run := func(args ...string) (string, int) {
		var stdout, stderr bytes.Buffer
		code := Run(append(args, "--home", home), &stdout, &stderr)
		return stderr.String(), code
	}
	// Create an agent type first.
	if errOut, code := run("agent", "type", "set", "planner", "--runtime", "codex", "--max-background", "2"); code != 0 {
		t.Fatalf("type set exit=%d stderr=%s", code, errOut)
	}
	if errOut, code := run("agent", "heartbeat", "add", "planner", "daily",
		"--repo", "jerryfane/gitmoot", "--interval", "24h", "--prompt", "p"); code != 0 {
		t.Fatalf("heartbeat add exit=%d stderr=%s", code, errOut)
	}
	// Both the agent type and the heartbeat must survive.
	paths := config.PathsForHome(home)
	types, err := config.LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes: %v", err)
	}
	if entry, ok := types["planner"]; !ok || entry.MaxBackground != 2 {
		t.Fatalf("agent type clobbered by heartbeat add: %+v", types)
	}
	heartbeats, err := config.LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats: %v", err)
	}
	if len(heartbeats) != 1 || heartbeats[0].Name != "daily" {
		t.Fatalf("heartbeat not persisted: %+v", heartbeats)
	}
	// A subsequent agent-type edit must keep the heartbeat (reverse direction).
	if errOut, code := run("agent", "type", "set", "planner", "--runtime", "codex", "--max-background", "3"); code != 0 {
		t.Fatalf("type set 2 exit=%d stderr=%s", code, errOut)
	}
	if heartbeats, err = config.LoadHeartbeats(paths); err != nil || len(heartbeats) != 1 {
		t.Fatalf("heartbeat dropped by agent-type edit: %d err=%v", len(heartbeats), err)
	}
}

// TestAgentHeartbeatAddReviewRejectsMissingCapability proves the write path
// refuses a review heartbeat for an agent lacking the review capability.
func TestAgentHeartbeatAddReviewRejectsMissingCapability(t *testing.T) {
	home := t.TempDir()
	// Register an agent WITHOUT review capability.
	if err := withStore(home, func(store *db.Store) error {
		return store.UpsertAgent(context.Background(), db.Agent{
			Name: "asker", Runtime: "codex", RepoScope: "jerryfane/gitmoot",
			Capabilities: []string{"ask"}, RuntimeRef: "last",
		})
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "heartbeat", "add", "asker", "stale",
		"--repo", "jerryfane/gitmoot", "--interval", "12h", "--action", "review",
		"--prompt", "p", "--home", home}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for review heartbeat without capability")
	}
	if !strings.Contains(stderr.String(), "review capability") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	// Nothing should have been written.
	heartbeats, err := config.LoadHeartbeats(config.PathsForHome(home))
	if err != nil {
		t.Fatalf("LoadHeartbeats: %v", err)
	}
	if len(heartbeats) != 0 {
		t.Fatalf("review heartbeat was written despite missing capability: %+v", heartbeats)
	}
}

// TestAgentHeartbeatAddReviewSucceedsWithCapability is the positive counterpart.
func TestAgentHeartbeatAddReviewSucceedsWithCapability(t *testing.T) {
	home := t.TempDir()
	if err := withStore(home, func(store *db.Store) error {
		return store.UpsertAgent(context.Background(), db.Agent{
			Name: "reviewer", Runtime: "codex", RepoScope: "jerryfane/gitmoot",
			Capabilities: []string{"ask", "review"}, RuntimeRef: "last",
		})
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"agent", "heartbeat", "add", "reviewer", "stale",
		"--repo", "jerryfane/gitmoot", "--interval", "12h", "--action", "review",
		"--prompt", "Review stale PRs.", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("review add exit=%d stderr=%s", code, stderr.String())
	}
	heartbeats, err := config.LoadHeartbeats(config.PathsForHome(home))
	if err != nil || len(heartbeats) != 1 || heartbeats[0].Action != "review" {
		t.Fatalf("review heartbeat not persisted: %+v err=%v", heartbeats, err)
	}
}

// TestDaemonHeartbeatLinesOffByDefault asserts the status observability surface is
// empty when no heartbeats are configured (off-by-default: status is unchanged).
func TestDaemonHeartbeatLinesOffByDefault(t *testing.T) {
	home := t.TempDir()
	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatalf("initializedPaths: %v", err)
	}
	if lines := daemonHeartbeatLines(paths, home); len(lines) != 0 {
		t.Fatalf("expected no heartbeat status lines off-by-default, got %v", lines)
	}
}

// TestDaemonHeartbeatLinesSurfacesState asserts a configured heartbeat with a
// persisted state row is rendered with its last_status and next_due.
func TestDaemonHeartbeatLinesSurfacesState(t *testing.T) {
	home := t.TempDir()
	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatalf("initializedPaths: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+`
[agents.repo-maintainer.heartbeats.daily]
enabled = true
repo = "jerryfane/gitmoot"
interval = "24h"
prompt = "p"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := withStore(home, func(store *db.Store) error {
		return store.UpsertHeartbeatState(context.Background(), db.HeartbeatState{
			Agent: "repo-maintainer", Name: "daily", LastStatus: "enqueued", LastJobID: "job-1",
		})
	}); err != nil {
		t.Fatalf("UpsertHeartbeatState: %v", err)
	}
	lines := daemonHeartbeatLines(paths, home)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "heartbeats: 1 configured") ||
		!strings.Contains(joined, "repo-maintainer/daily") ||
		!strings.Contains(joined, "last_status=enqueued") {
		t.Fatalf("status lines missing expected detail: %q", joined)
	}
}
