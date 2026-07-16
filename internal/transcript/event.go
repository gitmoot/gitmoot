// Package transcript converts runtime tee logs into a small internal event
// model and renders a human-readable, redacted transcript.
package transcript

import "time"

// Kind identifies one normalized transcript event. This is an internal model,
// not a persisted or public wire schema.
type Kind string

const (
	KindAgentText  Kind = "agent_text"
	KindToolCall   Kind = "tool_call"
	KindToolResult Kind = "tool_result"
	KindUsage      Kind = "usage"
	KindLifecycle  Kind = "lifecycle"
	KindRaw        Kind = "raw"
)

// Event is the normalized representation shared by all runtime translators.
type Event struct {
	Kind         Kind
	Text         string
	Name         string
	ToolIcon     string
	Preview      PreviewMode
	PreviewLines int
	InputDigest  string
	Status       string
	OutputDigest string
	Duration     time.Duration
	InputTokens  int
	OutputTokens int
	Phase        string
	Detail       string
	RawLine      string
}

// PreviewMode tells the styled renderer which end of a long tool result is
// useful. The plain renderer intentionally ignores it to preserve its wire-like
// byte output for pipes and redirects.
type PreviewMode string

const (
	PreviewHead PreviewMode = "head"
	PreviewTail PreviewMode = "tail"
)
