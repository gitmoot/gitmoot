package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	gitmootruntime "github.com/jerryfane/gitmoot/internal/runtime"
)

// Translator converts complete logical lines into normalized events. Flush is
// used by buffered formats (currently Claude's one final JSON envelope).
type Translator interface {
	Translate(line string) []Event
	Flush() []Event
}

// NewTranslator returns the translator for a registered runtime family.
func NewTranslator(runtimeName string) (Translator, error) {
	switch strings.TrimSpace(runtimeName) {
	case gitmootruntime.CodexRuntime:
		return codexTranslator{}, nil
	case gitmootruntime.ClaudeRuntime:
		return &claudeTranslator{}, nil
	case gitmootruntime.KimiRuntime, gitmootruntime.KimiCLIRuntime:
		return kimiTranslator{}, nil
	case gitmootruntime.ShellRuntime:
		return shellTranslator{}, nil
	default:
		return nil, fmt.Errorf("unsupported transcript runtime %q", runtimeName)
	}
}

type codexTranslator struct{}

func (codexTranslator) Translate(line string) []Event {
	event, err := gitmootruntime.ExtractCodexStreamEvent(strings.TrimSpace(line))
	if err != nil {
		return rawEvent(line)
	}
	switch event.Type {
	case "thread.started":
		return []Event{{Kind: KindLifecycle, Phase: "thread", Detail: "started"}}
	case "turn.started":
		return []Event{{Kind: KindLifecycle, Phase: "turn", Detail: "started"}}
	case "item.started":
		switch event.ItemType {
		case "command_execution":
			if event.CommandExecution != nil {
				return []Event{{Kind: KindToolCall, Name: commandName(event.CommandExecution.Command), InputDigest: event.CommandExecution.Command}}
			}
		case "file_change":
			if event.FileChange != nil {
				return []Event{{Kind: KindToolCall, Name: "file_change", InputDigest: fileChangeDigest(event.FileChange.Changes)}}
			}
		}
		return rawEvent(line)
	case "item.completed":
		switch event.ItemType {
		case "agent_message":
			return []Event{{Kind: KindAgentText, Text: event.Text}}
		case "reasoning":
			return []Event{{Kind: KindLifecycle, Phase: "reasoning", Detail: event.Text}}
		case "command_execution":
			if event.CommandExecution != nil {
				return []Event{{
					Kind:         KindToolResult,
					Status:       commandStatus(event.CommandExecution),
					OutputDigest: event.CommandExecution.AggregatedOutput,
				}}
			}
			return rawEvent(line)
		case "file_change":
			if event.FileChange != nil {
				return []Event{{
					Kind:         KindToolResult,
					Status:       event.FileChange.Status,
					OutputDigest: fileChangeDigest(event.FileChange.Changes),
				}}
			}
			return rawEvent(line)
		case "":
			return rawEvent(line)
		default:
			return []Event{{Kind: KindToolCall, Name: event.ItemType, InputDigest: compactJSON(event.ItemRaw)}}
		}
	case "turn.completed":
		return []Event{{Kind: KindUsage, InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens}}
	case "error":
		return []Event{{Kind: KindLifecycle, Phase: "error", Detail: event.Message}}
	case "turn.failed":
		return []Event{{Kind: KindLifecycle, Phase: "turn failed", Detail: event.ErrorMessage}}
	default:
		return rawEvent(line)
	}
}

func (codexTranslator) Flush() []Event { return nil }

type kimiTranslator struct{}

func (kimiTranslator) Translate(line string) []Event {
	event, err := gitmootruntime.ExtractKimiStreamEvent(strings.TrimSpace(line))
	if err != nil {
		return rawEvent(line)
	}
	var events []Event
	if event.Usage != nil {
		events = append(events, Event{Kind: KindUsage, InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens})
	}
	switch event.Role {
	case "assistant":
		for _, call := range event.ToolCalls {
			if call.Type == "function" {
				events = append(events, Event{Kind: KindToolCall, Name: call.Function.Name, InputDigest: call.Function.Arguments})
			}
		}
		if event.ContentText != "" {
			events = append(events, Event{Kind: KindAgentText, Text: event.ContentText})
		}
	case "tool":
		events = append(events, Event{Kind: KindToolResult, Status: "tool", OutputDigest: event.ContentText})
	case "meta":
		if event.Type == "session.resume_hint" {
			events = append(events, Event{Kind: KindLifecycle, Phase: "session", Detail: "resume hint reported"})
		} else if len(events) == 0 {
			return rawEvent(line)
		}
	default:
		if len(events) == 0 {
			return rawEvent(line)
		}
	}
	return events
}

func (kimiTranslator) Flush() []Event { return nil }

// Claude Code currently emits one final JSON envelope rather than JSONL. Hold
// every complete line until EOF so the transcript honestly remains silent until
// that envelope is available.
type claudeTranslator struct {
	lines []string
}

func (t *claudeTranslator) Translate(line string) []Event {
	t.lines = append(t.lines, line)
	return nil
}

func (t *claudeTranslator) Flush() []Event {
	if len(t.lines) == 0 {
		return nil
	}
	events := make([]Event, 0, len(t.lines)+1)
	for _, line := range t.lines {
		payload, err := gitmootruntime.ExtractClaudeResultEnvelope(strings.TrimSpace(line))
		if err != nil || (payload.Type != "result" && payload.Result == "") {
			events = append(events, rawEvent(line)...)
			continue
		}
		events = append(events,
			Event{Kind: KindAgentText, Text: payload.Result},
			Event{Kind: KindUsage, InputTokens: payload.Usage.InputTokens, OutputTokens: payload.Usage.OutputTokens},
		)
	}
	t.lines = nil
	return events
}

func commandName(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "bash"
	}
	name := filepath.Base(fields[0])
	if name == "" || name == "." {
		return fields[0]
	}
	return name
}

func commandStatus(command *gitmootruntime.CodexCommandExecution) string {
	status := strings.TrimSpace(command.Status)
	if command.ExitCode == nil {
		return status
	}
	if status == "" {
		return fmt.Sprintf("exit %d", *command.ExitCode)
	}
	return fmt.Sprintf("%s (exit %d)", status, *command.ExitCode)
}

const maxRenderedFileChanges = 8

func fileChangeDigest(changes []gitmootruntime.CodexFileChangeEntry) string {
	limit := len(changes)
	if limit > maxRenderedFileChanges {
		limit = maxRenderedFileChanges
	}
	parts := make([]string, 0, limit+1)
	for _, change := range changes[:limit] {
		parts = append(parts, strings.TrimSpace(change.Kind+" "+change.Path))
	}
	if remaining := len(changes) - limit; remaining > 0 {
		parts = append(parts, fmt.Sprintf("(+%d more)", remaining))
	}
	return strings.Join(parts, ", ")
}

type shellTranslator struct{}

func (shellTranslator) Translate(line string) []Event { return rawEvent(line) }
func (shellTranslator) Flush() []Event                { return nil }

func rawEvent(line string) []Event {
	return []Event{{Kind: KindRaw, RawLine: line}}
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if len(raw) == 0 || json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}
