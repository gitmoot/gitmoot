package runtime

import (
	"bufio"
	"encoding/json"
	"strings"
)

// StreamUsage is the token-count shape shared by the runtime wire formats.
// It is internal to gitmoot; callers should treat missing usage as zero.
type StreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// CodexStreamEvent is the verified subset of a codex exec --json line. ItemRaw
// deliberately retains the complete item object so consumers can render
// unverified item types generically without inventing field schemas.
type CodexStreamEvent struct {
	Type             string
	ItemID           string
	ItemType         string
	Text             string
	ItemRaw          json.RawMessage
	CommandExecution *CodexCommandExecution
	FileChange       *CodexFileChange
	Usage            StreamUsage
	Message          string
	ErrorMessage     string
}

// CodexCommandExecution is the command_execution item shape verified by the
// captured codex --json fixture.
type CodexCommandExecution struct {
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Status           string `json:"status"`
}

// CodexFileChange is the file_change item shape verified by the captured codex
// --json fixture.
type CodexFileChange struct {
	Changes []CodexFileChangeEntry `json:"changes"`
	Status  string                 `json:"status"`
}

type CodexFileChangeEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// ExtractCodexStreamEvent owns the codex JSONL wire-format knowledge shared by
// result parsing and transcript rendering.
func ExtractCodexStreamEvent(line string) (CodexStreamEvent, error) {
	var wire struct {
		Type    string          `json:"type"`
		Item    json.RawMessage `json:"item"`
		Usage   StreamUsage     `json:"usage"`
		Message string          `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &wire); err != nil {
		return CodexStreamEvent{}, err
	}
	event := CodexStreamEvent{
		Type:         wire.Type,
		ItemRaw:      wire.Item,
		Usage:        wire.Usage,
		Message:      wire.Message,
		ErrorMessage: wire.Error.Message,
	}
	if len(wire.Item) != 0 {
		var item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(wire.Item, &item); err == nil {
			event.ItemID = item.ID
			event.ItemType = item.Type
			event.Text = item.Text
			switch item.Type {
			case "command_execution":
				var command CodexCommandExecution
				if err := json.Unmarshal(wire.Item, &command); err == nil {
					event.CommandExecution = &command
				}
			case "file_change":
				var change CodexFileChange
				if err := json.Unmarshal(wire.Item, &change); err == nil {
					event.FileChange = &change
				}
			}
		}
	}
	return event, nil
}

// ClaudeResultEnvelope is the verified subset of Claude Code's final
// --output-format json envelope.
type ClaudeResultEnvelope struct {
	Type           string      `json:"type"`
	Result         string      `json:"result"`
	Usage          StreamUsage `json:"usage"`
	DurationMS     int64       `json:"duration_ms"`
	IsError        bool        `json:"is_error"`
	APIErrorStatus int         `json:"api_error_status"`
}

// ExtractClaudeResultEnvelope owns Claude's final-envelope wire format. Claude
// normally emits one JSON object, but some versions/modes emit JSONL; in that
// case the last parseable result envelope is authoritative.
func ExtractClaudeResultEnvelope(stdout string) (ClaudeResultEnvelope, error) {
	var envelope ClaudeResultEnvelope
	wholeOutputErr := json.Unmarshal([]byte(stdout), &envelope)
	if wholeOutputErr == nil {
		return envelope, nil
	}

	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	found := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var candidate ClaudeResultEnvelope
		if json.Unmarshal([]byte(line), &candidate) == nil && candidate.Type == "result" {
			envelope = candidate
			found = true
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return ClaudeResultEnvelope{}, scanErr
	}
	if found {
		return envelope, nil
	}
	return ClaudeResultEnvelope{}, wholeOutputErr
}

// KimiStreamEvent is the verified subset of one Kimi stream-json line.
type KimiStreamEvent struct {
	Role        string
	Type        string
	ContentText string
	SessionID   string
	Usage       *StreamUsage
	ToolCalls   []KimiToolCall
	ToolCallID  string
}

type KimiToolCall struct {
	Type     string           `json:"type"`
	ID       string           `json:"id"`
	Function KimiFunctionCall `json:"function"`
}

type KimiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ExtractKimiStreamEvent owns Kimi's stream-json wire format, including its
// two observed content encodings (a string and an array of text parts).
func ExtractKimiStreamEvent(line string) (KimiStreamEvent, error) {
	var wire struct {
		Role       string          `json:"role"`
		Type       string          `json:"type"`
		Content    json.RawMessage `json:"content"`
		SessionID  string          `json:"session_id"`
		Usage      *StreamUsage    `json:"usage"`
		ToolCalls  []KimiToolCall  `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal([]byte(line), &wire); err != nil {
		return KimiStreamEvent{}, err
	}
	return KimiStreamEvent{
		Role:        wire.Role,
		Type:        wire.Type,
		ContentText: extractKimiContentText(wire.Content),
		SessionID:   wire.SessionID,
		Usage:       wire.Usage,
		ToolCalls:   wire.ToolCalls,
		ToolCallID:  wire.ToolCallID,
	}, nil
}

func extractKimiContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}
