package prompts

//go:generate go run ./contractgen

import (
	"fmt"
	"path/filepath"
	"strings"
)

type JobPrompt struct {
	Repo                   string
	Branch                 string
	PullRequest            int
	Task                   string
	Sender                 string
	Action                 string
	Instructions           string
	Constraints            []string
	DelegationArtifactDir  string
	TemplateID             string
	TemplateResolvedCommit string
	TemplateInstructions   string
}

func RenderJob(prompt JobPrompt) string {
	var builder strings.Builder
	builder.WriteString("Gitmoot job\n\n")

	if strings.TrimSpace(prompt.TemplateInstructions) != "" {
		writeField(&builder, "Template", prompt.TemplateID)
		writeField(&builder, "Template source commit", prompt.TemplateResolvedCommit)
		builder.WriteString("Template instructions:\n")
		builder.WriteString(strings.TrimSpace(prompt.TemplateInstructions))
		builder.WriteString("\n\n")
	}

	builder.WriteString("Job context:\n")
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

	if dir := strings.TrimSpace(prompt.DelegationArtifactDir); dir != "" {
		builder.WriteString("\nDelegation artifacts:\n")
		builder.WriteString("This job was delegated by a coordinator that shared context on disk.\n")
		builder.WriteString("Read these files for the shared brief and the wider delegation batch before acting:\n")
		builder.WriteString("- ")
		builder.WriteString(filepath.Join(dir, "brief.md"))
		builder.WriteByte('\n')
		builder.WriteString("- ")
		builder.WriteString(filepath.Join(dir, "context-manifest.json"))
		builder.WriteByte('\n')
	}

	builder.WriteString("\nRequired output:\n")
	builder.WriteString("Return exactly one JSON object containing a top-level gitmoot_result field.\n")
	builder.WriteString("Use this shape:\n")
	builder.WriteString(resultContractShape)
	builder.WriteByte('\n')
	builder.WriteString(delegationSchemaHelp)
	return builder.String()
}

func RenderRepairPrompt(rawOutput string, parseError error) string {
	var builder strings.Builder
	builder.WriteString("Your previous response did not contain a valid gitmoot_result JSON object.\n")
	if parseError != nil {
		builder.WriteString("Validation errors (fix every line):\n")
		for _, line := range strings.Split(parseError.Error(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			builder.WriteString("- ")
			builder.WriteString(line)
			builder.WriteByte('\n')
		}
	}
	builder.WriteString("\nReturn only one JSON object in this exact shape:\n")
	builder.WriteString(resultContractShape)
	builder.WriteString("\n")
	builder.WriteString(delegationSchemaHelp)
	builder.WriteString("\nPrevious raw output:\n")
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
