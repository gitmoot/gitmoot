package transcript

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// RenderMarkdown exports a deterministic, ANSI-free conversation suitable for
// issue and pull-request comments. It is intentionally separate from Renderer:
// terminal styling and the pipe-stable plain format must not affect exports.
func RenderMarkdown(w io.Writer, header Header, events []Event) error {
	if _, err := fmt.Fprintln(w, "# Job Transcript"); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"Action", header.Action},
		{"Agent", header.Agent},
		{"Runtime", header.Runtime},
		{"Model", header.Model},
		{"Workflow", header.Workflow},
	} {
		if strings.TrimSpace(field.value) != "" {
			if _, err := fmt.Fprintf(w, "- %s: %s\n", field.name, cleanMarkdownField(field.value, renderDigestLimit)); err != nil {
				return err
			}
		}
	}
	if prompt := cleanMarkdownBlock(header.Prompt, renderFieldLimit); prompt != "" {
		if _, err := fmt.Fprintf(w, "\n## User\n\n%s\n", prompt); err != nil {
			return err
		}
	}

	turns := markdownTurns(events)
	for _, turn := range turns {
		if _, err := fmt.Fprintf(w, "\n%s\n", markdownAssistantHeader(header.Model, turn.duration)); err != nil {
			return err
		}
		for _, event := range turn.events {
			if err := renderMarkdownEvent(w, event); err != nil {
				return err
			}
		}
	}
	return nil
}

type markdownTurn struct {
	events   []Event
	duration time.Duration
}

func markdownTurns(events []Event) []markdownTurn {
	turns := make([]markdownTurn, 0, 1)
	current := markdownTurn{}
	flush := func() {
		if len(current.events) == 0 {
			return
		}
		turns = append(turns, current)
		current = markdownTurn{}
	}
	for _, event := range events {
		if event.Kind == KindLifecycle && event.Phase == "turn" && event.Detail == "started" {
			flush()
			continue
		}
		if event.Kind == KindLifecycle && event.Phase == "thread" && event.Detail == "started" {
			continue
		}
		if event.Kind == KindUsage && event.Duration > 0 {
			current.duration = event.Duration
		}
		current.events = append(current.events, event)
	}
	flush()
	return turns
}

func markdownAssistantHeader(model string, duration time.Duration) string {
	details := make([]string, 0, 2)
	if model = cleanMarkdownField(model, renderDigestLimit); model != "" {
		details = append(details, model)
	}
	if duration > 0 {
		details = append(details, formatDuration(duration))
	}
	if len(details) == 0 {
		return "## Assistant"
	}
	return "## Assistant (" + strings.Join(details, " · ") + ")"
}

func renderMarkdownEvent(w io.Writer, event Event) error {
	switch event.Kind {
	case KindAgentText:
		text := cleanMarkdownBlock(event.Text, renderFieldLimit)
		if text == "" {
			return nil
		}
		_, err := fmt.Fprintf(w, "\n%s\n", text)
		return err
	case KindToolCall:
		if _, err := fmt.Fprintf(w, "\n**Tool: %s**\n", cleanMarkdownField(event.Name, renderDigestLimit)); err != nil {
			return err
		}
		if input := cleanMarkdownBlock(event.InputDigest, renderFieldLimit); input != "" {
			if _, err := fmt.Fprintln(w, "\nInput:"); err != nil {
				return err
			}
			return writeMarkdownFence(w, input)
		}
	case KindToolResult:
		status := cleanMarkdownField(event.Status, renderDigestLimit)
		if _, err := fmt.Fprintf(w, "\nOutput (%s):\n", status); err != nil {
			return err
		}
		if output := cleanMarkdownBlock(event.OutputDigest, renderFieldLimit); output != "" {
			if err := writeMarkdownFence(w, output); err != nil {
				return err
			}
		}
		if event.Duration > 0 {
			_, err := fmt.Fprintf(w, "\n_Took %s_\n", formatDuration(event.Duration))
			return err
		}
	case KindUsage:
		_, err := fmt.Fprintf(w, "\n_Tokens: %s in · %s out (latest reported)_\n", formatTokens(event.InputTokens), formatTokens(event.OutputTokens))
		return err
	case KindLifecycle:
		phase := cleanMarkdownField(event.Phase, renderDigestLimit)
		detail := cleanMarkdownField(event.Detail, renderFieldLimit)
		if detail != "" {
			phase += ": " + detail
		}
		if phase != "" {
			_, err := fmt.Fprintf(w, "\n> %s\n", phase)
			return err
		}
	case KindRaw:
		if raw := cleanMarkdownBlock(event.RawLine, renderRawLimit); raw != "" {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
			return writeMarkdownFence(w, raw)
		}
	}
	return nil
}

func cleanMarkdownBlock(value string, limit int) string {
	return stripANSI(cleanBlock(value, limit))
}

func cleanMarkdownField(value string, limit int) string {
	return stripANSI(cleanField(value, limit))
}

func stripANSI(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != 0x1b {
			out.WriteByte(value[i])
			i++
			continue
		}
		i++
		if i >= len(value) {
			break
		}
		switch value[i] {
		case '[':
			i++
			for i < len(value) {
				ch := value[i]
				i++
				if ch >= 0x40 && ch <= 0x7e {
					break
				}
			}
		case ']':
			i++
			for i < len(value) {
				if value[i] == '\a' {
					i++
					break
				}
				if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
		default:
			i++
		}
	}
	return out.String()
}

func writeMarkdownFence(w io.Writer, content string) error {
	fence := "```"
	for strings.Contains(content, fence) {
		fence += "`"
	}
	_, err := fmt.Fprintf(w, "%stext\n%s\n%s\n", fence, content, fence)
	return err
}
