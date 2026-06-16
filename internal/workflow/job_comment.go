package workflow

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const maxCommentFieldRunes = 2000
const maxCommentBodyRunes = 60000

type JobResultComment struct {
	AgentName  string
	Runtime    string
	JobID      string
	JobState   string
	Payload    JobPayload
	Result     *AgentResult
	Diagnostic string
}

func RenderJobResultComment(comment JobResultComment) string {
	var builder strings.Builder
	builder.WriteString("> Agent: `")
	builder.WriteString(markdownInline(comment.AgentName))
	builder.WriteString("`\n")
	builder.WriteString("> Runtime: `")
	builder.WriteString(markdownInline(comment.Runtime))
	builder.WriteString("`\n")
	if strings.TrimSpace(comment.Payload.TemplateID) != "" {
		builder.WriteString("> Template: `")
		builder.WriteString(markdownInline(comment.Payload.TemplateID))
		builder.WriteString("`\n")
	}
	builder.WriteString("> Job: `")
	builder.WriteString(markdownInline(comment.JobID))
	builder.WriteString("`\n\n")

	decision := strings.TrimSpace(comment.JobState)
	summary := strings.TrimSpace(comment.Diagnostic)
	if comment.Result != nil {
		decision = strings.TrimSpace(comment.Result.Decision)
		summary = strings.TrimSpace(comment.Result.Summary)
	}
	if decision == "" {
		decision = "unknown"
	}
	if summary == "" {
		summary = "No summary provided."
	}
	writeScalar(&builder, "Decision", "`"+markdownInline(decision)+"`")
	writeScalar(&builder, "Summary", limitCommentText(summary))

	if comment.Result != nil {
		writeFindings(&builder, comment.Result.Findings)
		writeList(&builder, "Changes Made", comment.Result.ChangesMade)
		writeList(&builder, "Tests Run", comment.Result.TestsRun)
		writeList(&builder, "Needs", comment.Result.Needs)
		writeList(&builder, "Delegations", delegationAgentNames(comment.Result.Delegations))
	}
	if strings.TrimSpace(comment.Diagnostic) != "" && (comment.Result == nil || strings.TrimSpace(comment.Diagnostic) != strings.TrimSpace(comment.Result.Summary)) {
		writeScalar(&builder, "Diagnostics", limitCommentText(comment.Diagnostic))
	}
	if len(comment.Payload.RawOutputs) > 0 {
		builder.WriteString("\nRaw runtime output was retained in local Gitmoot state and is not posted here.\n")
	}
	return limitCommentBody(builder.String())
}

func delegationAgentNames(delegations []Delegation) []string {
	names := make([]string, 0, len(delegations))
	for _, d := range delegations {
		name := strings.TrimSpace(d.Agent)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func writeScalar(builder *strings.Builder, label string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	builder.WriteString("**")
	builder.WriteString(label)
	builder.WriteString(":** ")
	builder.WriteString(value)
	builder.WriteString("\n")
}

func writeFindings(builder *strings.Builder, findings []json.RawMessage) {
	if len(findings) == 0 {
		return
	}
	builder.WriteString("\n**Findings**\n")
	for _, finding := range findings {
		text := formatFinding(finding)
		if text == "" {
			continue
		}
		builder.WriteString("- ")
		builder.WriteString(limitCommentText(text))
		builder.WriteByte('\n')
	}
}

func writeList(builder *strings.Builder, title string, values []string) {
	items := compactStrings(values)
	if len(items) == 0 {
		return
	}
	builder.WriteString("\n**")
	builder.WriteString(title)
	builder.WriteString("**\n")
	for _, item := range items {
		builder.WriteString("- ")
		builder.WriteString(limitCommentText(item))
		builder.WriteByte('\n')
	}
}

func formatFinding(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return string(raw)
}

// LimitCommentText applies the same redaction, truncation, and command
// neutralization used for GitHub job result comments.
func LimitCommentText(value string) string {
	return limitCommentText(value)
}

// LimitCommentBody applies the same total body cap used for GitHub job result
// comments, including redaction and command neutralization.
func LimitCommentBody(value string) string {
	return limitCommentBody(value)
}

func limitCommentText(value string) string {
	value = RedactCommentText(value)
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxCommentFieldRunes {
		return neutralizeGitmootCommandLines(value)
	}
	return neutralizeGitmootCommandLines(completeTrailingRedactionMarker(string(runes[:maxCommentFieldRunes]))) + "\n[truncated]"
}

func limitCommentBody(value string) string {
	value = neutralizeGitmootCommandLines(RedactCommentText(strings.TrimSpace(value)))
	runes := []rune(value)
	if len(runes) <= maxCommentBodyRunes {
		return value + "\n"
	}
	const suffix = "\n\n[comment truncated; full result is retained in local Gitmoot state]\n"
	limit := maxCommentBodyRunes - len([]rune(suffix))
	if limit < 0 {
		limit = 0
	}
	return neutralizeGitmootCommandLines(completeTrailingRedactionMarker(string(runes[:limit]))) + suffix
}

func markdownInline(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "`", "'")
}

func completeTrailingRedactionMarker(value string) string {
	const marker = "[REDACTED]"
	for i := 1; i < len(marker); i++ {
		if strings.HasSuffix(value, marker[:i]) {
			return strings.TrimSuffix(value, marker[:i]) + marker
		}
	}
	return value
}

func neutralizeGitmootCommandLines(value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "/gitmoot") {
			lines[i] = strings.TrimSuffix(line, trimmed) + `\/gitmoot` + strings.TrimPrefix(trimmed, "/gitmoot")
		}
	}
	return strings.Join(lines, "\n")
}

var commentRedactors = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`(?i)github_pat_[A-Za-z0-9_]{20,}`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)gh[opsur]_[A-Za-z0-9_]{20,}`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)sk-[A-Za-z0-9_-]{20,}`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)(aws_access_key_id\s*[:=]\s*)[A-Z0-9]{16,}`), "${1}[REDACTED]"},
	{regexp.MustCompile(`(?i)("(?:aws_)?secret_access_key"\s*:\s*)(?:"[^"]*"|'[^']*'|[^\s,;}]+)`), `${1}"[REDACTED]"`},
	{regexp.MustCompile(`(?i)((?:aws_)?secret_access_key\s*[:=]\s*)(?:"[^"]+"|'[^']+'|[^\s,;]+)`), "${1}[REDACTED]"},
	{regexp.MustCompile(`(?i)("(?:api[_-]?key|token|secret|password)"\s*:\s*)(?:"[^"]*"|'[^']*'|[^\s,;}]+)`), `${1}"[REDACTED]"`},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|password)\s*[:=]\s*)(?:"[^"]+"|'[^']+'|[^\s,;]+)`), "${1}[REDACTED]"},
}

func RedactCommentText(value string) string {
	for _, redactor := range commentRedactors {
		value = redactor.pattern.ReplaceAllString(value, redactor.replace)
	}
	return value
}
