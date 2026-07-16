package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	gitmootruntime "github.com/gitmoot/gitmoot/internal/runtime"
)

// Translator converts complete logical lines into normalized events. Flush is
// used by buffered formats (currently Claude's one final JSON envelope).
type Translator interface {
	Translate(line string) []Event
	Flush() []Event
}

// NewTranslator returns the translator for a registered runtime family.
func NewTranslator(runtimeName string) (Translator, error) {
	return newTranslator(runtimeName, time.Now)
}

// NewSnapshotTranslator is for deterministic replay/export of a retained log.
// Codex and Kimi streams carry no event timestamps, so replay must not invent
// elapsed time from parser speed. Claude's reported duration_ms still survives.
func NewSnapshotTranslator(runtimeName string) (Translator, error) {
	return newTranslator(runtimeName, func() time.Time { return time.Time{} })
}

func newTranslator(runtimeName string, now func() time.Time) (Translator, error) {
	switch strings.TrimSpace(runtimeName) {
	case gitmootruntime.CodexRuntime:
		return &codexTranslator{now: now, tools: make(map[string]pendingTool)}, nil
	case gitmootruntime.ClaudeRuntime:
		return &claudeTranslator{}, nil
	case gitmootruntime.KimiRuntime, gitmootruntime.KimiCLIRuntime:
		return &kimiTranslator{now: now, tools: make(map[string]pendingTool)}, nil
	case gitmootruntime.ShellRuntime:
		return shellTranslator{}, nil
	default:
		return nil, fmt.Errorf("unsupported transcript runtime %q", runtimeName)
	}
}

type pendingTool struct {
	name         string
	presentation toolPresentation
	started      time.Time
}

type toolPresentation struct {
	icon         string
	preview      PreviewMode
	previewLines int
}

type codexTranslator struct {
	now         func() time.Time
	tools       map[string]pendingTool
	turnStarted time.Time
}

func (t *codexTranslator) Translate(line string) []Event {
	event, err := gitmootruntime.ExtractCodexStreamEvent(strings.TrimSpace(line))
	if err != nil {
		return rawEvent(line)
	}
	switch event.Type {
	case "thread.started":
		return []Event{{Kind: KindLifecycle, Phase: "thread", Detail: "started"}}
	case "turn.started":
		t.turnStarted = t.now()
		return []Event{{Kind: KindLifecycle, Phase: "turn", Detail: "started"}}
	case "item.started":
		switch event.ItemType {
		case "command_execution":
			if event.CommandExecution != nil {
				return []Event{t.startTool(event.ItemID, commandName(event.CommandExecution.Command), event.CommandExecution.Command)}
			}
		case "file_change":
			if event.FileChange != nil {
				return []Event{t.startTool(event.ItemID, "file_change", fileChangeDigest(event.FileChange.Changes))}
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
				return []Event{t.finishTool(event.ItemID, commandName(event.CommandExecution.Command), commandStatus(event.CommandExecution), event.CommandExecution.AggregatedOutput)}
			}
			return rawEvent(line)
		case "file_change":
			if event.FileChange != nil {
				return []Event{t.finishTool(event.ItemID, "file_change", event.FileChange.Status, fileChangeDigest(event.FileChange.Changes))}
			}
			return rawEvent(line)
		case "":
			return rawEvent(line)
		default:
			return []Event{{Kind: KindToolCall, Name: event.ItemType, InputDigest: compactJSON(event.ItemRaw)}}
		}
	case "turn.completed":
		duration := elapsed(t.turnStarted, t.now())
		t.turnStarted = time.Time{}
		return []Event{{Kind: KindUsage, InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens, Duration: duration}}
	case "error":
		return []Event{{Kind: KindLifecycle, Phase: "error", Detail: event.Message}}
	case "turn.failed":
		return []Event{{Kind: KindLifecycle, Phase: "turn failed", Detail: event.ErrorMessage}}
	default:
		return rawEvent(line)
	}
}

func (*codexTranslator) Flush() []Event { return nil }

func (t *codexTranslator) startTool(id, name, input string) Event {
	presentation := classifyTool(name)
	t.tools[id] = pendingTool{name: name, presentation: presentation, started: t.now()}
	return toolCallEvent(name, input, presentation)
}

func (t *codexTranslator) finishTool(id, fallbackName, status, output string) Event {
	pending, ok := t.tools[id]
	if ok {
		delete(t.tools, id)
	} else {
		pending = pendingTool{name: fallbackName, presentation: classifyTool(fallbackName)}
	}
	return toolResultEvent(pending, status, output, t.now())
}

type kimiTranslator struct {
	now     func() time.Time
	tools   map[string]pendingTool
	started time.Time
}

func (t *kimiTranslator) Translate(line string) []Event {
	event, err := gitmootruntime.ExtractKimiStreamEvent(strings.TrimSpace(line))
	if err != nil {
		return rawEvent(line)
	}
	if t.started.IsZero() {
		t.started = t.now()
	}
	var events []Event
	if event.Usage != nil {
		events = append(events, Event{Kind: KindUsage, InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens, Duration: elapsed(t.started, t.now())})
	}
	switch event.Role {
	case "assistant":
		for _, call := range event.ToolCalls {
			if call.Type == "function" {
				presentation := classifyTool(call.Function.Name)
				t.tools[call.ID] = pendingTool{name: call.Function.Name, presentation: presentation, started: t.now()}
				events = append(events, toolCallEvent(call.Function.Name, call.Function.Arguments, presentation))
			}
		}
		if event.ContentText != "" {
			events = append(events, Event{Kind: KindAgentText, Text: event.ContentText})
		}
	case "tool":
		pending, ok := t.tools[event.ToolCallID]
		if ok {
			delete(t.tools, event.ToolCallID)
		} else {
			pending = pendingTool{name: "tool", presentation: classifyTool("tool")}
		}
		events = append(events, toolResultEvent(pending, "tool", event.ContentText, t.now()))
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

func (*kimiTranslator) Flush() []Event { return nil }

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
			Event{Kind: KindUsage, InputTokens: payload.Usage.InputTokens, OutputTokens: payload.Usage.OutputTokens, Duration: time.Duration(payload.DurationMS) * time.Millisecond},
		)
	}
	t.lines = nil
	return events
}

func toolCallEvent(name, input string, presentation toolPresentation) Event {
	return Event{Kind: KindToolCall, Name: name, ToolIcon: presentation.icon, Preview: presentation.preview, PreviewLines: presentation.previewLines, InputDigest: input}
}

func toolResultEvent(tool pendingTool, status, output string, now time.Time) Event {
	return Event{
		Kind:         KindToolResult,
		Name:         tool.name,
		ToolIcon:     tool.presentation.icon,
		Preview:      tool.presentation.preview,
		PreviewLines: tool.presentation.previewLines,
		Status:       status,
		OutputDigest: output,
		Duration:     elapsed(tool.started, now),
	}
}

func elapsed(started, finished time.Time) time.Duration {
	if started.IsZero() || !finished.After(started) {
		return 0
	}
	return finished.Sub(started)
}

// classifyTool is the single translator-layer icon and preview policy. Keep
// this names-only: runtime-specific payload values never influence rendering.
func classifyTool(name string) toolPresentation {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch normalized {
	case "bash", "sh", "shell", "zsh", "powershell", "command", "command_execution", "exec", "exec_command", "terminal":
		return toolPresentation{icon: "$", preview: PreviewTail, previewLines: 5}
	case "read", "read_file", "cat", "view", "open":
		return toolPresentation{icon: "→", preview: PreviewHead, previewLines: 10}
	case "write", "write_file", "edit", "apply_patch", "patch", "file_change":
		return toolPresentation{icon: "←", preview: PreviewHead, previewLines: 10}
	case "glob", "grep", "rg", "find", "search", "search_files":
		return toolPresentation{icon: "✱", preview: PreviewHead, previewLines: 15}
	case "websearch", "web_search", "search_query":
		return toolPresentation{icon: "◈", preview: PreviewHead, previewLines: 10}
	case "webfetch", "web_fetch", "fetch_url":
		return toolPresentation{icon: "%", preview: PreviewHead, previewLines: 10}
	default:
		return toolPresentation{icon: "⚙", preview: PreviewHead, previewLines: 10}
	}
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
