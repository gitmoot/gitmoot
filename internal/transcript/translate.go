package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gitmootruntime "github.com/gitmoot/gitmoot/internal/runtime"
)

// Translator converts complete logical lines into normalized events. Flush
// drains any state a buffered format holds until the stream ends.
type Translator interface {
	Translate(line string) []Event
	Flush() []Event
}

// NewTranslator returns the translator for a registered runtime family.
func NewTranslator(runtimeName string) (Translator, error) {
	return newTranslator(runtimeName, time.Now)
}

// NewSnapshotTranslator is for deterministic replay/export of a retained log.
// Retained streams carry no event timestamps, so replay must not invent elapsed
// time from parser speed.
func NewSnapshotTranslator(runtimeName string) (Translator, error) {
	return newTranslator(runtimeName, func() time.Time { return time.Time{} })
}

func newTranslator(runtimeName string, now func() time.Time) (Translator, error) {
	switch strings.TrimSpace(runtimeName) {
	case gitmootruntime.OmpRuntime:
		return &ompTranslator{now: now, tools: make(map[string]pendingTool)}, nil
	case gitmootruntime.ShellRuntime:
		return shellTranslator{}, nil
	default:
		if gitmootruntime.IsRemovedRuntime(runtimeName) {
			// A job recorded on a runtime removed in #2587 renders its retained
			// log verbatim, so transcript export over old job rows still works.
			return shellTranslator{}, nil
		}
		return nil, fmt.Errorf("unsupported transcript runtime %q", runtimeName)
	}
}

// ompTranslator decodes omp's `--mode=json` TOOL events (#2371) and passes every
// other line through raw (#1428), so the retained bytes stay fully visible:
//
//	{"type":"tool_execution_start","toolCallId":..,"toolName":"bash","args":{"command":..}}
//	{"type":"tool_execution_end","toolCallId":..,"toolName":"bash","isError":..,
//	 "result":{"content":[{"type":"text","text":..}],"details":{"exitCode":1,..}}}
//
// omp reports details.exitCode on a failed shell command; a successful one
// carries isError=false and no exit code, which is recorded as 0. A `session`
// line becomes a lifecycle event, which is how a consumer can tell "the stream
// was read and no command ran" from "no stream was read at all".
//
// Only lines whose type field is a tool or session event are decoded; the
// check runs on a short prefix first, so a multi-megabyte `agent_end` line is
// never parsed here. Every registered runtime must keep a case
// (TestEveryRegisteredRuntimeHasATranslator).
type ompTranslator struct {
	now   func() time.Time
	tools map[string]pendingTool
}

type ompToolEvent struct {
	Type       string          `json:"type"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	IsError    bool            `json:"isError"`
	Result     struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Details struct {
			ExitCode *int `json:"exitCode"`
		} `json:"details"`
	} `json:"result"`
}

func (t *ompTranslator) Translate(line string) []Event {
	trimmed := strings.TrimSpace(line)
	kind := ompEventType(trimmed)
	switch kind {
	case "session":
		return []Event{{Kind: KindLifecycle, Phase: "session", Detail: "omp stream started", RawLine: line}}
	case "tool_execution_start", "tool_execution_end":
	default:
		return rawEvent(line)
	}
	var event ompToolEvent
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil || strings.TrimSpace(event.ToolCallID) == "" {
		return rawEvent(line)
	}
	if kind == "tool_execution_start" {
		presentation := classifyTool(event.ToolName)
		t.tools[event.ToolCallID] = pendingTool{name: event.ToolName, presentation: presentation, started: t.now()}
		return []Event{toolCallEvent(event.ToolCallID, event.ToolName, string(event.Args), presentation)}
	}
	pending, ok := t.tools[event.ToolCallID]
	if ok {
		delete(t.tools, event.ToolCallID)
	} else {
		pending = pendingTool{name: event.ToolName, presentation: classifyTool(event.ToolName)}
	}
	var output strings.Builder
	for _, part := range event.Result.Content {
		if part.Type == "text" {
			output.WriteString(part.Text)
		}
	}
	status := "ok"
	if event.IsError {
		status = "error"
	}
	result := toolResultEvent(event.ToolCallID, pending, status, output.String(), t.now())
	exitCode := 0
	if event.Result.Details.ExitCode != nil {
		exitCode = *event.Result.Details.ExitCode
	} else if event.IsError {
		exitCode = -1 // failed without a reported exit code (timeout, abort, refused)
	}
	result.ExitCode = &exitCode
	return []Event{result}
}

func (*ompTranslator) Flush() []Event { return nil }

// ompEventType reads the "type" value from the start of an omp NDJSON line
// without decoding the line. omp writes "type" first; anything else yields "".
func ompEventType(line string) string {
	const prefix = `{"type":"`
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	rest := line[len(prefix):]
	if len(rest) > 64 {
		rest = rest[:64]
	}
	end := strings.IndexByte(rest, '"')
	if end <= 0 {
		return ""
	}
	return rest[:end]
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

func toolCallEvent(id, name, input string, presentation toolPresentation) Event {
	return Event{Kind: KindToolCall, ToolID: id, Name: name, ToolIcon: presentation.icon, Preview: presentation.preview, PreviewLines: presentation.previewLines, InputDigest: input}
}

func toolResultEvent(id string, tool pendingTool, status, output string, now time.Time) Event {
	return Event{
		Kind:         KindToolResult,
		ToolID:       id,
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
