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

// Renderer writes normalized events as redacted, bounded human-readable lines.
type Renderer struct {
	w io.Writer
}

func NewRenderer(w io.Writer) *Renderer { return &Renderer{w: w} }

func (r *Renderer) Render(events ...Event) error {
	for _, event := range events {
		if err := r.render(event); err != nil {
			return err
		}
	}
	return nil
}

func (r *Renderer) render(event Event) error {
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
