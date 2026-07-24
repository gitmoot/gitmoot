package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// toolCacheEnvSubdirs maps each tool's cache-directory env var to the
// subdirectory it gets under the shared tool-cache root.
var toolCacheEnvSubdirs = []struct{ env, subdir string }{
	{"UV_CACHE_DIR", "uv"},
	{"PIP_CACHE_DIR", "pip"},
	{"npm_config_cache", "npm"},
	{"GOCACHE", "go-build"},
	{"GOMODCACHE", "go-mod"},
}

// applyIsolatedToolCacheGrants widens agent.WritablePaths with ONE shared,
// host-level tool-cache root for isolated-worktree jobs, and returns the env
// vars pointing each tool's cache at that shared root instead of the
// per-worktree fallback that duplicates gigabytes of immutable, content-
// addressed packages per job (#1113 lever 1).
//
// MUST be called AFTER applyProduceRuntimeGrants: that call unconditionally
// OVERWRITES agent.WritablePaths for produce jobs, so appending before it would
// be silently discarded. Appending (not replacing) here preserves whatever
// applyProduceRuntimeGrants set.
//
// Scope: isolated jobs only (payload.WorktreePath != "") — a non-isolated job
// uses the persistent registered checkout, which does not duplicate caches per
// job. Widening WritablePaths reaches codex (its sandbox is always on and reads
// the agent value fresh at Deliver time, so this is what actually makes the
// cache dir writable there) and claude/kimi "produce" jobs (whose Landlock grant
// is computed from this same field, later, in wrapProduceSandboxAdapter).
// Non-produce claude/kimi and shell runtimes run unsandboxed already, so
// widening WritablePaths for them is a harmless no-op — nothing reads it — and
// the env vars alone suffice to redirect their cache.
//
// codex only honors WritablePaths under the workspace-write autonomy policy
// (codexSandboxArgs' --add-dir loop; read-only mode grants nothing). Pointing
// UV_CACHE_DIR/GOCACHE/etc. at the shared dir for a read-only codex job would
// redirect tools to a directory their sandbox cannot write, breaking Go builds
// and degrading uv/pip/npm — worse than leaving the env unset. So a read-only
// (or unrecognized) codex autonomy policy is a no-op here; danger-full-access
// is unrestricted and proceeds like any other job. A ChatSeat is ALSO a no-op:
// codexSandboxArgs returns workspace-write for a ChatSeat WITHOUT ever reaching
// the --add-dir loop (its only implicit writable roots are workdir/tmp), so a
// chat seat never gets the shared directory writable regardless of policy —
// injecting the env for it would reproduce the exact same-directory-unwritable
// failure this gate exists to prevent (#1113 finder, confirmed against
// TestCodexDeliverChatSeatSandbox).
//
// Errors here are the caller's to treat as fail-open: this is disk hygiene, not
// a security precondition, and must never fail a job.
func applyIsolatedToolCacheGrants(paths config.Paths, payload workflow.JobPayload, agent *runtime.Agent) ([]string, error) {
	if strings.TrimSpace(payload.WorktreePath) == "" {
		return nil, nil
	}
	if agent.Runtime == runtime.CodexRuntime {
		if agent.ChatSeat {
			return nil, nil
		}
		switch runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy) {
		case runtime.AutonomyPolicyWorkspaceWrite, runtime.AutonomyPolicyDangerFullAccess:
			// proceeds below
		default:
			return nil, nil
		}
	}
	policy, err := config.LoadToolCache(paths)
	if err != nil {
		return nil, fmt.Errorf("load tool cache config: %w", err)
	}
	if !policy.Enabled || strings.TrimSpace(policy.Dir) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(policy.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create shared tool cache dir %s: %w", policy.Dir, err)
	}
	env := make([]string, 0, len(toolCacheEnvSubdirs))
	for _, e := range toolCacheEnvSubdirs {
		dir := filepath.Join(policy.Dir, e.subdir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create shared tool cache subdir %s: %w", dir, err)
		}
		env = append(env, e.env+"="+dir)
	}
	agent.WritablePaths = appendUniquePath(agent.WritablePaths, policy.Dir)
	return env, nil
}

func appendUniquePath(paths []string, add string) []string {
	for _, p := range paths {
		if p == add {
			return paths
		}
	}
	return append(paths, add)
}

// injectDeliveryAdapterEnv wraps an already-composed adapter's runner with an
// EnvInjectingRunner carrying env, mirroring appendDeliveryAdapterOutput's
// per-adapter-type rewrap (transcript_retention.go) so the wrap composes safely
// with whatever the adapter already carries (Landlock, credential curation,
// process-group kill).
func injectDeliveryAdapterEnv(adapter workflow.DeliveryAdapter, env []string) (workflow.DeliveryAdapter, error) {
	if adapter == nil || len(env) == 0 {
		return adapter, nil
	}
	switch a := adapter.(type) {
	case modelGatewayRuntimeAdapter:
		inner, err := injectDeliveryAdapterEnv(a.Adapter, env)
		if err != nil {
			return nil, err
		}
		runtimeAdapter, ok := inner.(runtime.Adapter)
		if !ok {
			return nil, fmt.Errorf("tool cache env inject returned incompatible %T model-gateway adapter", inner)
		}
		a.Adapter = runtimeAdapter
		return a, nil
	case runtime.CodexAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case *runtime.CodexAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case runtime.ClaudeAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case *runtime.ClaudeAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case runtime.KimiAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case *runtime.KimiAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case runtime.KimiCLIAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case *runtime.KimiCLIAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case runtime.ShellAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	case *runtime.ShellAdapter:
		a.Runner = subprocess.EnvInjectingRunner{Inner: a.Runner, Env: env}
		return a, nil
	default:
		return nil, fmt.Errorf("tool cache env inject cannot wrap adapter %T", adapter)
	}
}
