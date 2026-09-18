package cli

import (
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// requireWorkflowPolicyResolver accepts the raw --home value. In particular, a
// raw directory named .gitmoot is still a home directory, so its config lives
// under .gitmoot/.gitmoot rather than being mistaken for an already-resolved root.
func requireWorkflowPolicyResolver(home string) func(string) workflow.RequireWorkflowPolicy {
	if strings.TrimSpace(home) == "" {
		paths, err := pathsFromFlag("")
		if err != nil {
			return func(repo string) workflow.RequireWorkflowPolicy { return requireWorkflowFailOpenPolicy(repo) }
		}
		return requireWorkflowPolicyResolverPaths(paths)
	}
	return requireWorkflowPolicyResolverPaths(config.PathsForHome(home))
}

// requireWorkflowPolicyResolverRoot is for daemon and engine seams that already
// carry the canonical <home>/.gitmoot root. Keeping this separate prevents the
// #446/#459 double-resolution class while preserving raw --home semantics above.
func requireWorkflowPolicyResolverRoot(root string) func(string) workflow.RequireWorkflowPolicy {
	return requireWorkflowPolicyResolverPaths(config.Paths{ConfigFile: configFileAtRoot(root)})
}

func configFileAtRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	return filepath.Join(strings.TrimSpace(root), config.ConfigName)
}

// requireWorkflowFailOpenPolicy is the policy applied when config cannot be read
// (absent/unreadable/unresolvable home). It returns the built-in DEFAULT policy
// — not a disabled zero value — so a transient read error can't silently drop
// the labeling the default promises. The default is auto (never rejects), so
// fail-open never enforces strict on an unreadable config. Reusing For() on an
// empty config keeps this in lockstep with the single default source.
func requireWorkflowFailOpenPolicy(repo string) workflow.RequireWorkflowPolicy {
	def := config.RequireWorkflowConfig{}.For(repo)
	return workflow.RequireWorkflowPolicy{Enabled: def.Enabled, Mode: def.Mode}
}

// requireWorkflowPolicyResolverPaths keeps config ownership in CLI while giving
// each enqueue producer a current policy. Invalid or unreadable config is
// fail-open here (to the built-in default via requireWorkflowFailOpenPolicy);
// config edit/init validation remains the operator-facing error path.
func requireWorkflowPolicyResolverPaths(paths config.Paths) func(string) workflow.RequireWorkflowPolicy {
	return func(repo string) workflow.RequireWorkflowPolicy {
		cfg, err := config.LoadRequireWorkflow(paths)
		if err != nil {
			return requireWorkflowFailOpenPolicy(repo)
		}
		p := cfg.For(repo)
		return workflow.RequireWorkflowPolicy{Enabled: p.Enabled, Mode: p.Mode}
	}
}
