package cli

import (
	"strings"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

// pipelineStageResultCmd is a shell command that ignores its input and echoes a
// valid gitmoot_result with the given decision (and, for a block, the given
// needs). It is the SHELL-runtime session body a stage job runs as
// `sh -c <cmd> gitmoot <prompt>`, so the whole pipeline chain runs with NO LLM and
// NO network — fully deterministic offline (the #446/#533 shell-runtime idiom).
func pipelineStageResultCmd(decision, summary string, needs []string) string {
	return pipelineStageResultCmdWithSeverity(decision, summary, needs, "")
}

func pipelineStageResultCmdWithSeverity(decision, summary string, needs []string, severity string) string {
	needsJSON := "[]"
	if len(needs) > 0 {
		quoted := make([]string, 0, len(needs))
		for _, n := range needs {
			quoted = append(quoted, `"`+n+`"`)
		}
		needsJSON = "[" + strings.Join(quoted, ",") + "]"
	}
	severityJSON := ""
	if severity != "" {
		severityJSON = `,"severity":"` + severity + `"`
	}
	return `printf '%s' '{"gitmoot_result":{"decision":"` + decision + `"` + severityJSON + `,"summary":"` + summary +
		`","findings":[],"changes_made":[],"tests_run":[],"needs":` + needsJSON + `,"delegations":[]}}'`
}

// pipelineE2EStage renders one stage block with the shell cmd as a literal YAML
// block scalar (so the JSON-bearing single-quoted command survives YAML parsing).
func pipelineE2EStage(id, cmd, needs string) string {
	block := "  - id: " + id + "\n    cmd: |\n      " + cmd + "\n"
	if needs != "" {
		block += "    needs: [" + needs + "]\n"
	}
	return block
}
