package prompts

import (
	"fmt"
	"strings"
)

type JobPrompt struct {
	Repo         string
	Branch       string
	PullRequest  int
	Task         string
	Sender       string
	Action       string
	Instructions string
	Constraints  []string
}

func RenderJob(prompt JobPrompt) string {
	var builder strings.Builder
	builder.WriteString("Gitmoot job\n\n")
	writeField(&builder, "Repo", prompt.Repo)
	writeField(&builder, "Branch", prompt.Branch)
	if prompt.PullRequest > 0 {
		writeField(&builder, "Pull request", fmt.Sprintf("#%d", prompt.PullRequest))
	}
	writeField(&builder, "Task", prompt.Task)
	writeField(&builder, "Sender", prompt.Sender)
	writeField(&builder, "Requested action", prompt.Action)
	writeField(&builder, "Instructions", prompt.Instructions)

	if len(prompt.Constraints) > 0 {
		builder.WriteString("\nConstraints:\n")
		for _, constraint := range prompt.Constraints {
			constraint = strings.TrimSpace(constraint)
			if constraint != "" {
				builder.WriteString("- ")
				builder.WriteString(constraint)
				builder.WriteByte('\n')
			}
		}
	}

	builder.WriteString("\nRequired output:\n")
	builder.WriteString("Return exactly one JSON object containing a top-level gitmoot_result field.\n")
	builder.WriteString("Use this shape:\n")
	builder.WriteString(`{"gitmoot_result":{"decision":"approved|changes_requested|blocked|implemented|failed","summary":"...","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`)
	builder.WriteByte('\n')
	return builder.String()
}

func RenderRepairPrompt(rawOutput string, parseError error) string {
	var builder strings.Builder
	builder.WriteString("Your previous response did not contain a valid gitmoot_result JSON object.\n")
	if parseError != nil {
		writeField(&builder, "Parse error", parseError.Error())
	}
	builder.WriteString("\nReturn only one JSON object in this exact shape:\n")
	builder.WriteString(`{"gitmoot_result":{"decision":"approved|changes_requested|blocked|implemented|failed","summary":"...","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`)
	builder.WriteString("\n\nPrevious raw output:\n")
	builder.WriteString(trimRawOutput(rawOutput, 12000))
	builder.WriteByte('\n')
	return builder.String()
}

func writeField(builder *strings.Builder, label string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	builder.WriteString(label)
	builder.WriteString(": ")
	builder.WriteString(value)
	builder.WriteByte('\n')
}

func trimRawOutput(output string, limit int) string {
	output = strings.TrimSpace(output)
	if len(output) <= limit {
		return output
	}
	return output[:limit] + "\n[truncated]"
}
