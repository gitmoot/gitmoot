package transcript

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/workflow"
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
	sgrReset = "\x1b[0m"
	sgrBold  = "\x1b[1m"
	sgrDim   = "\x1b[90m"
	sgrRed   = "\x1b[31m"
	sgrCyan  = "\x1b[36m"
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
		line = "\u25cf " + lines[0]
		for _, cont := range lines[1:] {
			line += "\n  " + cont
		}
	case KindToolCall:
		line = sgrCyan + "\u25b8 " + sgrReset + sgrBold + cleanField(event.Name, renderDigestLimit) + sgrReset
		if digest := cleanField(event.InputDigest, renderDigestLimit); digest != "" {
			line += " " + sgrDim + digest + sgrReset
		}
	case KindToolResult:
		status := cleanField(event.Status, renderDigestLimit)
		digest := cleanTailField(event.OutputDigest, renderDigestLimit)
		if strings.Contains(status, "fail") {
			line = sgrRed + "\u25c2 " + status + sgrReset
		} else {
			line = sgrDim + "\u25c2 " + status + sgrReset
		}
		if digest != "" {
			line += "\n" + sgrDim + "  \u21b3 " + digest + sgrReset
		}
	case KindUsage:
		line = sgrDim + fmt.Sprintf("\u2191%s \u2193%s tokens (latest reported)", formatTokens(event.InputTokens), formatTokens(event.OutputTokens)) + sgrReset
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
