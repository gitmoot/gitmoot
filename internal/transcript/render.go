package transcript

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	renderFieldLimit  = 4096
	renderDigestLimit = 320
	renderRawLimit    = 4096
)

// ANSI SGR fragments for the styled renderer. 16-color codes only, chosen for
// tmux-pane portability (borrowed from the opencode/pi TUI conventions: dim =
// finished machinery and metadata, red = failure, everything else stays calm).
const (
	sgrReset  = "\x1b[0m"
	sgrBold   = "\x1b[1m"
	sgrDim    = "\x1b[90m"
	sgrRed    = "\x1b[31m"
	sgrCyan   = "\x1b[36m"
	sgrYellow = "\x1b[33m"
)

// Renderer writes normalized events as redacted, bounded human-readable lines.
// The plain form is byte-stable for pipes and tests; the styled form adds ANSI
// color and spacing for live terminal panes.
type Renderer struct {
	w        io.Writer
	styled   bool
	wroteAny bool
}

func NewRenderer(w io.Writer) *Renderer { return &Renderer{w: w} }

// NewStyledRenderer renders with ANSI styling and turn spacing. Callers must
// only pick it for interactive terminals; piped output stays on NewRenderer.
func NewStyledRenderer(w io.Writer) *Renderer { return &Renderer{w: w, styled: true} }

func (r *Renderer) Render(events ...Event) error {
	for _, event := range events {
		if err := r.render(event); err != nil {
			return err
		}
	}
	return nil
}

func (r *Renderer) render(event Event) error {
	if r.styled {
		return r.renderStyled(event)
	}
	var line string
	switch event.Kind {
	case KindAgentText:
		line = "\u25cf " + cleanField(event.Text, renderFieldLimit)
	case KindToolCall:
		line = "\u25b8 " + cleanField(event.Name, renderDigestLimit)
		if digest := cleanField(event.InputDigest, renderDigestLimit); digest != "" {
			line += " " + digest
		}
	case KindToolResult:
		line = "\u25c2 " + cleanField(event.Status, renderDigestLimit)
		if digest := cleanTailField(event.OutputDigest, renderDigestLimit); digest != "" {
			line += " " + digest
		}
	case KindUsage:
		line = fmt.Sprintf("usage (latest reported usage): in=%d out=%d", event.InputTokens, event.OutputTokens)
	case KindLifecycle:
		line = "\u2022 " + cleanField(event.Phase, renderDigestLimit)
		if detail := cleanField(event.Detail, renderFieldLimit); detail != "" {
			line += ": " + detail
		}
	case KindRaw:
		line = cleanField(event.RawLine, renderRawLimit)
	default:
		line = cleanField(event.RawLine, renderRawLimit)
	}
	_, err := fmt.Fprintln(r.w, line)
	return err
}

// renderStyled applies the pane look: agent text keeps its line breaks and gets
// a blank line of breathing room, tool calls pack tight with the name in bold,
// completed machinery and metadata render dim, failures render red.
func (r *Renderer) renderStyled(event Event) error {
	var line string
	switch event.Kind {
	case KindAgentText:
		if r.wroteAny {
			if _, err := fmt.Fprintln(r.w); err != nil {
				return err
			}
		}
		lines := strings.Split(cleanBlock(event.Text, renderFieldLimit), "\n")
		line = "\u25cf " + styledNarrationLine(lines[0])
		for _, cont := range lines[1:] {
			line += "\n  " + styledNarrationLine(cont)
		}
	case KindToolCall:
		icon := event.ToolIcon
		if icon == "" {
			icon = "\u2699"
		}
		line = sgrCyan + icon + " " + sgrReset + sgrBold + cleanField(event.Name, renderDigestLimit) + sgrReset
		if digest := cleanField(event.InputDigest, renderDigestLimit); digest != "" {
			line += " " + sgrDim + digest + sgrReset
		}
	case KindToolResult:
		status := cleanField(event.Status, renderDigestLimit)
		statusLower := strings.ToLower(status)
		cancelled := strings.Contains(statusLower, "cancel")
		if cancelled {
			line = sgrYellow + "\u25c2 " + status + sgrReset
		} else if strings.Contains(statusLower, "fail") || strings.Contains(statusLower, "error") {
			line = sgrRed + "\u25c2 " + status + sgrReset
		} else {
			line = sgrDim + "\u25c2 " + status + sgrReset
		}
		preview := styledOutputPreview(event.OutputDigest, event.Preview, event.PreviewLines)
		for i, outputLine := range preview {
			prefix := "    "
			if i == 0 {
				prefix = "  \u21b3 "
			}
			line += "\n" + sgrDim + prefix + outputLine + sgrReset
		}
		if event.Duration > 0 {
			durationLine := "  Took " + formatDuration(event.Duration)
			if cancelled {
				durationLine += " (cancelled)"
				line += "\n" + sgrYellow + durationLine + sgrReset
			} else {
				line += "\n" + sgrDim + durationLine + sgrReset
			}
		}
	case KindUsage:
		line = sgrDim + fmt.Sprintf("\u2191%s \u2193%s tokens (latest reported)", formatTokens(event.InputTokens), formatTokens(event.OutputTokens)) + sgrReset
		if event.Duration > 0 {
			line += " " + sgrDim + "\u00b7 Took " + formatDuration(event.Duration) + sgrReset
		}
	case KindLifecycle:
		line = "\u2022 " + cleanField(event.Phase, renderDigestLimit)
		if detail := cleanField(event.Detail, renderFieldLimit); detail != "" {
			line += ": " + detail
		}
		line = sgrDim + line + sgrReset
	case KindRaw:
		line = cleanField(event.RawLine, renderRawLimit)
	default:
		line = cleanField(event.RawLine, renderRawLimit)
	}
	r.wroteAny = true
	_, err := fmt.Fprintln(r.w, line)
	return err
}

func styledOutputPreview(value string, mode PreviewMode, limit int) []string {
	value = workflow.RedactCommentText(value)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.TrimRight(value, "\n")
	if value == "" {
		return nil
	}
	lines := strings.Split(value, "\n")
	if limit <= 0 {
		limit = 10
	}
	if len(lines) <= limit {
		return cleanPreviewLines(lines)
	}
	omitted := len(lines) - limit
	if mode == PreviewTail {
		result := []string{fmt.Sprintf("... (%d earlier lines)", omitted)}
		return append(result, cleanPreviewLines(lines[len(lines)-limit:])...)
	}
	result := cleanPreviewLines(lines[:limit])
	return append(result, fmt.Sprintf("... (%d more lines)", omitted))
}

func cleanPreviewLines(lines []string) []string {
	cleaned := make([]string, len(lines))
	for i, line := range lines {
		cleaned[i] = truncateUTF8(line, renderDigestLimit)
	}
	return cleaned
}

// styledNarrationLine implements the deliberately small markdown subset used
// in scrollback panes. It is line-oriented and cannot consume surrounding text.
func styledNarrationLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if headingText, ok := styledHeading(trimmed); ok {
		return sgrBold + styleInlineCode(headingText) + sgrReset
	}
	if markerLen := bulletMarkerLen(line); markerLen > 0 {
		return sgrCyan + line[:markerLen] + sgrReset + styleInlineCode(line[markerLen:])
	}
	return styleInlineCode(line)
}

func styledHeading(line string) (string, bool) {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(line) || line[i] != ' ' {
		return "", false
	}
	return line[i+1:], true
}

func bulletMarkerLen(line string) int {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return 2
	}
	i := 0
	for i < len(line) && unicode.IsDigit(rune(line[i])) {
		i++
	}
	if i > 0 && i+1 < len(line) && line[i] == '.' && line[i+1] == ' ' {
		return i + 2
	}
	return 0
}

func styleInlineCode(line string) string {
	parts := strings.Split(line, "`")
	if len(parts) == 1 {
		return line
	}
	var out strings.Builder
	for i, part := range parts {
		if i > 0 {
			out.WriteByte('`')
		}
		if i%2 == 1 {
			out.WriteString(sgrCyan)
			out.WriteString(part)
			out.WriteString(sgrReset)
		} else {
			out.WriteString(part)
		}
	}
	return out.String()
}

func formatDuration(duration time.Duration) string {
	if duration < time.Millisecond {
		return "<1ms"
	}
	if duration < time.Second {
		return fmt.Sprintf("%dms", duration.Round(time.Millisecond)/time.Millisecond)
	}
	if duration < 10*time.Second {
		return fmt.Sprintf("%.1fs", duration.Seconds())
	}
	if duration < time.Minute {
		return fmt.Sprintf("%.0fs", duration.Seconds())
	}
	minutes := int(duration / time.Minute)
	seconds := int((duration % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

// formatTokens compacts counts the way agent TUIs do: 812, 1.2k, 42k, 1.7M.
func formatTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%dM", n/1_000_000)
	}
}

// cleanBlock is cleanField except line breaks survive, so styled agent text
// keeps its paragraph shape. Redaction still happens before truncation.
func cleanBlock(value string, limit int) string {
	value = workflow.RedactCommentText(value)
	value = strings.ReplaceAll(value, "\r", "")
	return truncateUTF8(value, limit)
}

func cleanTailField(value string, limit int) string {
	value = workflow.RedactCommentText(value)
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	value = value[len(value)-limit:]
	for !utf8.ValidString(value) {
		value = value[1:]
	}
	return value
}

// cleanField redacts before truncating so a secret that begins before the cap
// but ends after it cannot be partially exposed by truncation.
func cleanField(value string, limit int) string {
	value = workflow.RedactCommentText(value)
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return truncateUTF8(value, limit)
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// headerPromptLimit caps how much of the job prompt the transcript header
// shows; the full prompt lives on the job payload, the header is orientation.
const headerPromptLimit = 400

// Header describes the job a transcript belongs to. It renders once, before
// the event stream, so a pane (or a saved plain transcript) is self-describing.
type Header struct {
	Action   string
	Agent    string
	Runtime  string
	Model    string
	Workflow string
	Prompt   string
}

// RenderHeader writes the job orientation block: one metadata line, then the
// redacted, capped prompt, then a separating blank line. Styled mode dims the
// metadata and truncation marker so the prompt reads as the lead.
func (r *Renderer) RenderHeader(h Header) error {
	meta := "▶ " + h.Action + " · " + h.Agent + " · " + h.Runtime
	if h.Model != "" {
		meta += "/" + h.Model
	}
	if h.Workflow != "" {
		meta += " · workflow " + h.Workflow
	}
	prompt := cleanField(h.Prompt, headerPromptLimit)
	marker := ""
	if over := len(workflow.RedactCommentText(h.Prompt)) - len(prompt); over > 0 {
		marker = fmt.Sprintf("… (+%d more chars)", over)
	}
	var out string
	if r.styled {
		out = sgrDim + meta + sgrReset + "\n"
		if prompt != "" {
			out += "  " + prompt
			if marker != "" {
				out += " " + sgrDim + marker + sgrReset
			}
			out += "\n"
		}
	} else {
		out = meta + "\n"
		if prompt != "" {
			out += "  " + prompt
			if marker != "" {
				out += " " + marker
			}
			out += "\n"
		}
	}
	r.wroteAny = true
	_, err := fmt.Fprintln(r.w, out)
	return err
}
