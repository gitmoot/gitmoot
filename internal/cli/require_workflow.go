package cli

import (
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// requireWorkflowPolicyResolver keeps config ownership in CLI while giving each
// enqueue producer a home-aware, current policy. Invalid or unreadable config is
// fail-open here; config edit/init validation remains the operator-facing error
// path and the legacy default is disabled.
func requireWorkflowPolicyResolver(home string) func(string) workflow.RequireWorkflowPolicy {
	paths := requireWorkflowPaths(home)
	return func(repo string) workflow.RequireWorkflowPolicy {
		cfg, err := config.LoadRequireWorkflow(paths)
		if err != nil {
			return workflow.RequireWorkflowPolicy{}
		}
		p := cfg.For(repo)
		return workflow.RequireWorkflowPolicy{Enabled: p.Enabled, Mode: p.Mode}
	}
}

// requireWorkflowPaths accepts both the raw --home directory and the resolved
// <home>/.gitmoot root used by daemon workers; do not resolve the latter twice.
func requireWorkflowPaths(home string) config.Paths {
	if filepath.Base(strings.TrimSpace(home)) != config.DirName {
		return config.PathsForHome(home)
	}
	root := strings.TrimSpace(home)
	return config.Paths{
		Home: root, ConfigFile: filepath.Join(root, config.ConfigName), Database: filepath.Join(root, config.DBName),
		Logs: filepath.Join(root, config.LogsDir), Workspaces: filepath.Join(root, config.WorkDir),
		Evals: filepath.Join(root, config.EvalsDir), ArtifactBlobs: filepath.Join(root, config.EvalsDir, config.BlobsDir),
	}
}
