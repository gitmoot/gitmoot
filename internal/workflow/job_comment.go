package workflow

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
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
		writeFinding(builder, finding)
	}
}

// writeFinding renders a single finding. String findings render as a plain
// bullet; object findings render as a bold heading plus an indented key/value
// sub-list; anything unrenderable falls back to a pretty-printed fenced JSON
// block. All emitted text is passed through limitCommentText for redaction and
// truncation.
func writeFinding(builder *strings.Builder, raw json.RawMessage) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}

	// String finding -> plain bullet (unchanged behavior).
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return
		}
		builder.WriteString("- ")
		builder.WriteString(limitCommentText(text))
		builder.WriteByte('\n')
		return
	}

	// Object finding -> heading + indented key/value sub-list.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil && len(obj) > 0 {
		writeFindingObject(builder, obj)
		return
	}

	// Anything else (arrays, scalars, malformed) -> pretty-printed JSON block.
	writeFindingJSONFallback(builder, raw)
}

// findingHeadingKeys are the conventional keys, in priority order, used to
// derive a finding's bold heading. None is mandatory.
var findingHeadingKeys = []string{"title", "approach", "name", "summary", "finding"}

// findingSourceKeys are rendered as markdown links when present.
var findingSourceKeys = map[string]bool{"source_url": true, "url": true, "source": true}

func writeFindingObject(builder *strings.Builder, obj map[string]json.RawMessage) {
	headingKey := ""
	for _, key := range findingHeadingKeys {
		if _, ok := obj[key]; ok {
			if h := scalarFindingValue(obj[key]); strings.TrimSpace(h) != "" {
				headingKey = key
				break
			}
		}
	}

	heading := ""
	if headingKey != "" {
		heading = scalarFindingValue(obj[headingKey])
	}

	// A recommendation/severity-style qualifier shown alongside the heading.
	qualifier := ""
	for _, key := range []string{"recommendation", "severity", "status"} {
		if _, ok := obj[key]; ok && key != headingKey {
			if q := scalarFindingValue(obj[key]); strings.TrimSpace(q) != "" {
				qualifier = q
				break
			}
		}
	}

	builder.WriteString("- ")
	if strings.TrimSpace(heading) != "" {
		builder.WriteString("**")
		builder.WriteString(limitCommentText(heading))
		builder.WriteString("**")
		if strings.TrimSpace(qualifier) != "" {
			builder.WriteString(" (")
			builder.WriteString(limitCommentText(qualifier))
			builder.WriteString(")")
		}
	} else if strings.TrimSpace(qualifier) != "" {
		builder.WriteString("**")
		builder.WriteString(limitCommentText(qualifier))
		builder.WriteString("**")
	} else {
		builder.WriteString("Finding")
	}
	builder.WriteByte('\n')

	for _, key := range sortedFindingKeys(obj) {
		if key == headingKey {
			continue
		}
		writeFindingField(builder, "  ", key, obj[key], 0)
	}
}

// writeFindingField renders a single key/value pair as an indented bullet.
func writeFindingField(builder *strings.Builder, indent, key string, raw json.RawMessage, depth int) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}

	// Nested array -> nested bullet sub-list.
	if len(raw) > 0 && raw[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil {
			if len(arr) == 0 {
				return
			}
			builder.WriteString(indent)
			builder.WriteString("- ")
			builder.WriteString(limitCommentText(key))
			builder.WriteString(":\n")
			if depth >= maxFindingDepth {
				builder.WriteString(indent)
				builder.WriteString("  - ")
				builder.WriteString(limitCommentText(compactJSON(raw)))
				builder.WriteByte('\n')
				return
			}
			for _, item := range arr {
				val := scalarFindingValue(item)
				builder.WriteString(indent)
				builder.WriteString("  - ")
				builder.WriteString(limitCommentText(val))
				builder.WriteByte('\n')
			}
			return
		}
	}

	// Nested object -> key: value lines (depth-bounded).
	if len(raw) > 0 && raw[0] == '{' {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err == nil {
			builder.WriteString(indent)
			builder.WriteString("- ")
			builder.WriteString(limitCommentText(key))
			builder.WriteString(":\n")
			if depth >= maxFindingDepth || len(nested) == 0 {
				builder.WriteString(indent)
				builder.WriteString("  - ")
				builder.WriteString(limitCommentText(compactJSON(raw)))
				builder.WriteByte('\n')
				return
			}
			for _, nk := range sortedFindingKeys(nested) {
				writeFindingField(builder, indent+"  ", nk, nested[nk], depth+1)
			}
			return
		}
	}

	value := scalarFindingValue(raw)
	if strings.TrimSpace(value) == "" {
		return
	}

	builder.WriteString(indent)
	builder.WriteString("- ")
	if findingSourceKeys[strings.ToLower(strings.TrimSpace(key))] {
		// Sanitize the URL on its own, then render it as a markdown link. URLs
		// are not field-prefixed secrets, so per-fragment redaction is safe.
		link := limitCommentText(strings.TrimSpace(value))
		builder.WriteString(limitCommentText(key))
		builder.WriteString(": [")
		builder.WriteString(link)
		builder.WriteString("](")
		builder.WriteString(link)
		builder.WriteString(")")
		builder.WriteByte('\n')
		return
	}

	// Render "key: value" as one fragment so redactors keyed on the field name
	// (e.g. token=, password:) see the prefix and value together.
	builder.WriteString(limitCommentText(key + ": " + value))
	builder.WriteByte('\n')
}

func writeFindingJSONFallback(builder *strings.Builder, raw json.RawMessage) {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		pretty.Reset()
		pretty.WriteString(compactJSON(raw))
	}
	builder.WriteString("- \n  ```json\n")
	builder.WriteString(limitCommentText(pretty.String()))
	builder.WriteString("\n  ```\n")
}

const maxFindingDepth = 2

// scalarFindingValue converts a JSON value to a single-line string: JSON
// strings unwrap to their text, numbers/bools render literally, and
// objects/arrays compact to JSON.
func scalarFindingValue(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return compactJSON(raw)
}

func compactJSON(raw json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return string(raw)
}

func sortedFindingKeys(obj map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
