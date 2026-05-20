package workflow

import (
	"errors"
	"fmt"
	"strings"
)

type PullRequestBody struct {
	TaskID          string
	AgentNames      []string
	What            string
	Why             string
	Changes         []string
	Results         []string
	Risk            string
	RawReviewOutput string
}

func RenderPullRequestBody(body PullRequestBody) (string, error) {
	if strings.TrimSpace(body.TaskID) == "" {
		return "", errors.New("task id is required")
	}
	if strings.TrimSpace(body.RawReviewOutput) == "" {
		return "", errors.New("raw final review output is required")
	}
	var builder strings.Builder
	writeSection(&builder, "WHAT", requiredText(body.What, "No summary provided."))
	writeSection(&builder, "WHY", requiredText(body.Why, "No rationale provided."))
	writeListSection(&builder, "CHANGES", body.Changes)
	writeListSection(&builder, "RESULTS", body.Results)
	writeSection(&builder, "RISK", requiredText(body.Risk, "No residual risk reported."))
	writeSection(&builder, "TASK", strings.TrimSpace(body.TaskID))
	writeListSection(&builder, "AGENTS", body.AgentNames)
	fence := rawOutputFence(body.RawReviewOutput)
	builder.WriteString("RAW FINAL REVIEW OUTPUT:\n")
	builder.WriteString(fence)
	builder.WriteString("text\n")
	builder.WriteString(body.RawReviewOutput)
	if !strings.HasSuffix(body.RawReviewOutput, "\n") {
		builder.WriteString("\n")
	}
	builder.WriteString(fence)
	builder.WriteString("\n")
	return builder.String(), nil
}

func writeSection(builder *strings.Builder, title string, value string) {
	builder.WriteString(title)
	builder.WriteString(":\n")
	builder.WriteString(value)
	builder.WriteString("\n\n")
}

func writeListSection(builder *strings.Builder, title string, values []string) {
	builder.WriteString(title)
	builder.WriteString(":\n")
	values = compactStrings(values)
	if len(values) == 0 {
		builder.WriteString("- None reported.\n\n")
		return
	}
	for _, value := range values {
		fmt.Fprintf(builder, "- %s\n", value)
	}
	builder.WriteString("\n")
}

func requiredText(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func rawOutputFence(value string) string {
	longest := 0
	current := 0
	for _, char := range value {
		if char == '`' {
			current++
			if current > longest {
				longest = current
			}
			continue
		}
		current = 0
	}
	if longest < 3 {
		longest = 3
	}
	return strings.Repeat("`", longest+1)
}
