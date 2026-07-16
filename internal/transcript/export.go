package transcript

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

const ExportSchemaVersion = 1

type ExportMetadata struct {
	JobID        string
	RootJobID    string
	ParentJobID  string
	DelegationID string
	Runtime      string
	Agent        string
	Action       string
	Repo         string
	Outcome      string
	Decision     string
	AttemptCount int
	CreatedAt    string
	EndedAt      string
}

// ExportRow is the stable P1 trajectory schema. Declaration order is
// intentional: encoding/json preserves it and schema_version must be first.
type ExportRow struct {
	SchemaVersion int    `json:"schema_version"`
	JobID         string `json:"job_id"`
	RootJobID     string `json:"root_job_id"`
	ParentJobID   string `json:"parent_job_id"`
	DelegationID  string `json:"delegation_id"`
	StepIndex     int    `json:"step_index"`
	Kind          Kind   `json:"kind"`
	Text          string `json:"text"`
	Tool          string `json:"tool"`
	Status        string `json:"status"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	DurationMS    int64  `json:"duration_ms"`
	Runtime       string `json:"runtime"`
	Agent         string `json:"agent"`
	Action        string `json:"action"`
	Repo          string `json:"repo"`
	Outcome       string `json:"outcome"`
	Decision      string `json:"decision"`
	AttemptCount  int    `json:"attempt_count"`
	CreatedAt     string `json:"created_at"`
	EndedAt       string `json:"ended_at"`
}

// SanitizeEvent redacts every runtime- or user-provided text field. Raw retained
// logs stay untouched; portable exports never bypass this best-effort sanitizer.
func SanitizeEvent(event Event) Event {
	event.Text = workflow.RedactCommentText(event.Text)
	event.Name = workflow.RedactCommentText(event.Name)
	event.InputDigest = workflow.RedactCommentText(event.InputDigest)
	event.Status = workflow.RedactCommentText(event.Status)
	event.OutputDigest = workflow.RedactCommentText(event.OutputDigest)
	event.Phase = workflow.RedactCommentText(event.Phase)
	event.Detail = workflow.RedactCommentText(event.Detail)
	event.RawLine = workflow.RedactCommentText(event.RawLine)
	return event
}

func ExportJSONL(w io.Writer, metadata ExportMetadata, events []Event) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	for index, event := range events {
		if err := encoder.Encode(exportRow(metadata, index, SanitizeEvent(event))); err != nil {
			return err
		}
	}
	return nil
}

// Export metadata is deliberately NOT redacted: every field is a
// gitmoot-issued identifier, enum, or timestamp — never free text — and the
// generic credential patterns false-positive on them (a job id like
// local-ask-x matches the sk- key pattern and would collapse every corpus row
// to the same mangled id). Free text only flows through Event fields, which
// sanitizeExportEvent masks above.

func exportRow(metadata ExportMetadata, index int, event Event) ExportRow {
	row := ExportRow{
		SchemaVersion: ExportSchemaVersion,
		JobID:         metadata.JobID, RootJobID: metadata.RootJobID,
		ParentJobID: metadata.ParentJobID, DelegationID: metadata.DelegationID,
		StepIndex: index, Kind: event.Kind,
		Runtime: metadata.Runtime, Agent: metadata.Agent, Action: metadata.Action,
		Repo: metadata.Repo, Outcome: metadata.Outcome, Decision: metadata.Decision,
		AttemptCount: metadata.AttemptCount, CreatedAt: metadata.CreatedAt, EndedAt: metadata.EndedAt,
	}
	switch event.Kind {
	case KindAgentText:
		row.Text = event.Text
	case KindToolCall:
		row.Text, row.Tool = event.InputDigest, event.Name
	case KindToolResult:
		row.Text, row.Tool, row.Status = event.OutputDigest, event.Name, event.Status
		row.DurationMS = durationMillis(event.Duration)
	case KindUsage:
		row.Text = fmt.Sprintf("input_tokens=%d output_tokens=%d", event.InputTokens, event.OutputTokens)
		row.InputTokens, row.OutputTokens = event.InputTokens, event.OutputTokens
		row.DurationMS = durationMillis(event.Duration)
	case KindLifecycle:
		row.Text = event.Phase
		if event.Detail != "" {
			if row.Text != "" {
				row.Text += ": "
			}
			row.Text += event.Detail
		}
	case KindRaw:
		row.Text = event.RawLine
	default:
		row.Text = event.RawLine
	}
	return row
}

func durationMillis(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

// ScanSnapshot avoids bufio.Scanner's oversized-token failure. Oversized lines
// are capped, marked by ReadSnapshotEvents, and never prevent later lines from
// being exported.
func ScanSnapshot(r io.Reader, onLine func(line string, truncated bool) error) error {
	buffer := make([]byte, 32*1024)
	line := make([]byte, 0, 64*1024)
	truncated := false
	emit := func() error {
		if err := onLine(string(line), truncated); err != nil {
			return err
		}
		line = line[:0]
		truncated = false
		return nil
	}
	for {
		n, err := r.Read(buffer)
		for _, ch := range buffer[:n] {
			if ch == '\n' {
				if emitErr := emit(); emitErr != nil {
					return emitErr
				}
				continue
			}
			if len(line) < MaxLogicalLineBytes {
				line = append(line, ch)
			} else {
				truncated = true
			}
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			if len(line) > 0 || truncated {
				return emit()
			}
			return nil
		}
	}
}

func ReadSnapshotEvents(r io.Reader, translator Translator) ([]Event, error) {
	var events []Event
	err := ScanSnapshot(r, func(line string, truncated bool) error {
		if truncated {
			events = append(events, Event{Kind: KindRaw, RawLine: line + " [truncated: logical line exceeded 1048576 bytes]"})
			return nil
		}
		events = append(events, translator.Translate(line)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	events = append(events, translator.Flush()...)
	return events, nil
}
