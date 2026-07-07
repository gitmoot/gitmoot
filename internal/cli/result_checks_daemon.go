package cli

import (
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// resultChecksMode resolves the [workflow] result_checks policy (#526) for a
// home the SAME dual-mode way the other home-scoped daemon seams do (via
// resolveConfigFile), so it works both when handed the RAW --home and when handed
// the already-RESOLVED <home>/.gitmoot root. It is fail-safe to the documented
// default (warn) on any config-load error, and maps the config-level mode string
// onto the workflow-level ResultCheckMode the engine/Mailbox consumes. An
// operator restores the exact pre-#526 terminal path with result_checks = off.
func resultChecksMode(home string) workflow.ResultCheckMode {
	var paths config.Paths
	if strings.TrimSpace(home) == "" {
		p, err := config.DefaultPaths()
		if err != nil {
			return workflow.ResultChecksWarn
		}
		paths = p
	} else {
		paths = config.Paths{ConfigFile: resolveConfigFile(home)}
	}
	mode, err := config.LoadResultChecksMode(paths)
	if err != nil {
		return workflow.ResultChecksWarn
	}
	return toWorkflowResultCheckMode(mode)
}

// toWorkflowResultCheckMode bridges the config-package mode enum to the
// workflow-package one (the two packages are intentionally decoupled — config
// owns parsing, workflow owns evaluation).
func toWorkflowResultCheckMode(mode config.ResultChecksMode) workflow.ResultCheckMode {
	switch mode {
	case config.ResultChecksOff:
		return workflow.ResultChecksOff
	case config.ResultChecksBlock:
		return workflow.ResultChecksBlock
	default:
		return workflow.ResultChecksWarn
	}
}
