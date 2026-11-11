package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestApplyIsolatedToolCacheGrantsCodexAutonomyGate pins the #1113 finder
// finding: codex only honors WritablePaths under workspace-write, so a
// read-only (or unrecognized) codex job must get neither the grant nor the env
// -- pointing its tools at an unwritable shared dir would be worse than doing
// nothing. workspace-write and danger-full-access proceed.
func TestApplyIsolatedToolCacheGrantsCodexAutonomyGate(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		wantNoop bool
	}{
		{name: "read-only skipped", policy: runtime.AutonomyPolicyReadOnly, wantNoop: true},
		{name: "unrecognized policy skipped", policy: "bogus", wantNoop: true},
		{name: "empty policy skipped", policy: "", wantNoop: true},
		{name: "workspace-write proceeds", policy: runtime.AutonomyPolicyWorkspaceWrite, wantNoop: false},
		{name: "danger-full-access proceeds", policy: runtime.AutonomyPolicyDangerFullAccess, wantNoop: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := config.PathsForHome(t.TempDir())
			agent := runtime.Agent{Runtime: runtime.CodexRuntime, AutonomyPolicy: test.policy}
			payload := workflow.JobPayload{WorktreePath: filepath.Join(paths.Home, "worktrees", "w1")}
			env, err := applyIsolatedToolCacheGrants(paths, payload, &agent)
			if err != nil {
				t.Fatalf("applyIsolatedToolCacheGrants: %v", err)
			}
			if test.wantNoop {
				if len(env) != 0 || len(agent.WritablePaths) != 0 {
					t.Fatalf("policy %q must be a no-op: env=%v writable=%v", test.policy, env, agent.WritablePaths)
				}
				return
			}
			if len(env) != len(toolCacheEnvSubdirs) || len(agent.WritablePaths) != 1 {
				t.Fatalf("policy %q must grant: env=%v writable=%v", test.policy, env, agent.WritablePaths)
			}
		})
	}
}

func TestApplyIsolatedToolCacheGrantsNonIsolatedNoop(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	agent := runtime.Agent{}
	env, err := applyIsolatedToolCacheGrants(paths, workflow.JobPayload{}, &agent)
	if err != nil {
		t.Fatalf("applyIsolatedToolCacheGrants: %v", err)
	}
	if len(env) != 0 || len(agent.WritablePaths) != 0 {
		t.Fatalf("non-isolated job must be a no-op: env=%v writable=%v", env, agent.WritablePaths)
	}
}

func TestApplyIsolatedToolCacheGrantsDisabledNoop(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[cache]\nenabled = false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	agent := runtime.Agent{}
	env, err := applyIsolatedToolCacheGrants(paths, workflow.JobPayload{WorktreePath: filepath.Join(paths.Home, "worktrees", "w1")}, &agent)
	if err != nil {
		t.Fatalf("applyIsolatedToolCacheGrants: %v", err)
	}
	if len(env) != 0 || len(agent.WritablePaths) != 0 {
		t.Fatalf("disabled config must be a no-op: env=%v writable=%v", env, agent.WritablePaths)
	}
}

func TestApplyIsolatedToolCacheGrantsCreatesDirsEnvAndGrant(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	agent := runtime.Agent{WritablePaths: []string{"/already/granted"}}
	payload := workflow.JobPayload{WorktreePath: filepath.Join(paths.Home, "worktrees", "w1")}

	env, err := applyIsolatedToolCacheGrants(paths, payload, &agent)
	if err != nil {
		t.Fatalf("applyIsolatedToolCacheGrants: %v", err)
	}
	if len(env) != len(toolCacheEnvSubdirs) {
		t.Fatalf("env = %v, want %d entries", env, len(toolCacheEnvSubdirs))
	}
	wantRoot := filepath.Join(paths.Home, "cache", "tools")
	envSet := map[string]string{}
	for _, kv := range env {
		key, val, ok := splitEnvKV(kv)
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		envSet[key] = val
	}
	for _, e := range toolCacheEnvSubdirs {
		wantDir := filepath.Join(wantRoot, e.subdir)
		if envSet[e.env] != wantDir {
			t.Fatalf("env[%s] = %q, want %q", e.env, envSet[e.env], wantDir)
		}
		if info, statErr := os.Stat(wantDir); statErr != nil || !info.IsDir() {
			t.Fatalf("subdir %s not created: %v", wantDir, statErr)
		}
	}
	// The existing produce grant is preserved (append, not overwrite), and the
	// shared cache root is added exactly once.
	if len(agent.WritablePaths) != 2 || agent.WritablePaths[0] != "/already/granted" || agent.WritablePaths[1] != wantRoot {
		t.Fatalf("WritablePaths = %v, want [/already/granted %s]", agent.WritablePaths, wantRoot)
	}

	// A second call (e.g. a retried delivery) must not duplicate the grant.
	if _, err := applyIsolatedToolCacheGrants(paths, payload, &agent); err != nil {
		t.Fatalf("second applyIsolatedToolCacheGrants: %v", err)
	}
	if len(agent.WritablePaths) != 2 {
		t.Fatalf("WritablePaths duplicated on second call: %v", agent.WritablePaths)
	}
}

func TestApplyIsolatedToolCacheGrantsReadOnlySeatUsesPerWorktreeRoot(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	firstWorktree := filepath.Join(paths.Home, "worktrees", "one")
	secondWorktree := filepath.Join(paths.Home, "worktrees", "two")
	for _, worktree := range []string{firstWorktree, secondWorktree} {
		if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	first := runtime.Agent{Runtime: runtime.CodexRuntime, ReadOnlySeat: true}
	second := runtime.Agent{Runtime: runtime.CodexRuntime, ReadOnlySeat: true}

	firstEnv, err := applyIsolatedToolCacheGrants(paths, workflow.JobPayload{WorktreePath: firstWorktree}, &first)
	if err != nil {
		t.Fatal(err)
	}
	secondEnv, err := applyIsolatedToolCacheGrants(paths, workflow.JobPayload{WorktreePath: secondWorktree}, &second)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.WritablePaths) != 1 || len(second.WritablePaths) != 1 {
		t.Fatalf("read-only cache grants = %v and %v, want one each", first.WritablePaths, second.WritablePaths)
	}
	if first.WritablePaths[0] == second.WritablePaths[0] {
		t.Fatalf("different worktrees share writable cache %q", first.WritablePaths[0])
	}
	sharedRoot := filepath.Join(paths.Home, "cache", "tools")
	if first.WritablePaths[0] == sharedRoot || second.WritablePaths[0] == sharedRoot {
		t.Fatalf("read-only seat received shared writable cache root %q", sharedRoot)
	}
	if len(firstEnv) != len(toolCacheEnvSubdirs) || len(secondEnv) != len(toolCacheEnvSubdirs) {
		t.Fatalf("cache env lengths = %d and %d", len(firstEnv), len(secondEnv))
	}
}

func TestApplyIsolatedToolCacheGrantsReadOnlyRejectsProtectedPathBeforeMutation(t *testing.T) {
	base := t.TempDir()
	checkout := filepath.Join(base, "review-worktree")
	commonDir := filepath.Join(base, "main", ".git")
	gitDir := filepath.Join(commonDir, "worktrees", "review")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatalf("mkdir linked metadata: %v", err)
	}
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatalf("mkdir checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
		t.Fatalf("write gitdir file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatalf("write commondir: %v", err)
	}

	for _, test := range []struct {
		name      string
		cacheRoot string
	}{
		{name: "checkout", cacheRoot: filepath.Join(checkout, "cache")},
		{name: "linked gitdir", cacheRoot: filepath.Join(gitDir, "cache")},
		{name: "common git directory", cacheRoot: filepath.Join(commonDir, "cache")},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := config.PathsForHome(t.TempDir())
			if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
				t.Fatalf("mkdir config home: %v", err)
			}
			configBody := "[cache]\nenabled = true\ndir = " + strconv.Quote(test.cacheRoot) + "\n"
			if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			agent := runtime.Agent{Runtime: runtime.CodexRuntime, ReadOnlySeat: true}

			_, err := applyIsolatedToolCacheGrants(paths, workflow.JobPayload{WorktreePath: checkout}, &agent)
			if err == nil || !strings.Contains(err.Error(), "overlaps") {
				t.Errorf("applyIsolatedToolCacheGrants error = %v, want protected-path rejection", err)
			}
			if _, statErr := os.Lstat(test.cacheRoot); !os.IsNotExist(statErr) {
				t.Fatalf("unsafe review cache was mutated before validation: %v", statErr)
			}
		})
	}
}

func splitEnvKV(kv string) (key, val string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}

// TestInjectDeliveryAdapterEnvRealSubprocess proves the env-injection wrap
// actually reaches a real subprocess (no LLM): a shell job echoes the injected
// var, exactly the mechanism codex/claude/kimi's shared runner infrastructure
// uses (#1113 lever 1).
func TestInjectDeliveryAdapterEnvRealSubprocess(t *testing.T) {
	adapter := runtime.ShellAdapter{Dir: t.TempDir(), Runner: subprocess.ExecRunner{}}
	wrapped, err := injectDeliveryAdapterEnv(adapter, []string{"GITMOOT_TEST_TOOL_CACHE=shared-cache-value"})
	if err != nil {
		t.Fatalf("injectDeliveryAdapterEnv: %v", err)
	}
	shellAdapter, ok := wrapped.(runtime.ShellAdapter)
	if !ok {
		t.Fatalf("wrapped adapter type = %T, want runtime.ShellAdapter", wrapped)
	}
	agent := runtime.Agent{Name: "custom", Role: "reviewer", Runtime: runtime.ShellRuntime, RuntimeRef: `echo "$GITMOOT_TEST_TOOL_CACHE"`, RepoScope: "gitmoot/gitmoot"}
	result, err := shellAdapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if result.Summary != "shared-cache-value" {
		t.Fatalf("Summary = %q, want %q (injected env did not reach the subprocess)", result.Summary, "shared-cache-value")
	}
}

func TestInjectDeliveryAdapterEnvNoopWithoutEnv(t *testing.T) {
	adapter := runtime.ShellAdapter{Runner: subprocess.ExecRunner{}}
	wrapped, err := injectDeliveryAdapterEnv(adapter, nil)
	if err != nil {
		t.Fatalf("injectDeliveryAdapterEnv: %v", err)
	}
	if wrapped != workflow.DeliveryAdapter(adapter) {
		t.Fatalf("empty env must return the adapter unchanged")
	}
}
