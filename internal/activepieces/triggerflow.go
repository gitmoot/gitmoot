package activepieces

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/pipeline"
)

const GeneratedTriggerFlowPrefix = "gitmoot: "

type triggerFlow struct {
	DisplayName   string            `json:"displayName"`
	SchemaVersion string            `json:"schemaVersion"`
	Trigger       triggerFlowAction `json:"trigger"`
}

type triggerFlowAction struct {
	Name            string              `json:"name"`
	Valid           bool                `json:"valid"`
	DisplayName     string              `json:"displayName"`
	Type            string              `json:"type"`
	LastUpdatedDate string              `json:"lastUpdatedDate,omitempty"`
	Settings        triggerFlowSettings `json:"settings"`
	NextAction      *triggerFlowAction  `json:"nextAction,omitempty"`
}

type triggerFlowSettings struct {
	PieceName            string         `json:"pieceName"`
	PieceVersion         string         `json:"pieceVersion"`
	TriggerName          string         `json:"triggerName,omitempty"`
	ActionName           string         `json:"actionName,omitempty"`
	Input                map[string]any `json:"input"`
	PropertySettings     map[string]any `json:"propertySettings"`
	ErrorHandlingOptions *struct{}      `json:"errorHandlingOptions,omitempty"`
}

// BuildTriggerFlow materializes the minimal receive-only email glue flow:
// official IMAP new_email -> gitmoot run_pipeline. A declared map is compiled
// from closed selectors into Activepieces expressions; raw expressions never
// enter the pipeline spec.
func BuildTriggerFlow(pipelineName string, t pipeline.Trigger, bridgeConnectionExternalID, imapPieceVersion, gitmootPieceVersion string) (string, json.RawMessage, error) {
	pipelineName = strings.TrimSpace(pipelineName)
	if pipelineName == "" {
		return "", nil, fmt.Errorf("pipeline name is required")
	}
	if t.Connection == "" {
		t.Connection = "gmail-imap"
	}
	if t.Mailbox == "" {
		t.Mailbox = "INBOX"
	}
	displayName := GeneratedTriggerFlowPrefix + pipelineName
	action := &triggerFlowAction{
		Name: "step_1", Valid: true, DisplayName: "Run Gitmoot pipeline", Type: "PIECE",
		Settings: triggerFlowSettings{
			PieceName: "@gitmoot/piece-gitmoot", PieceVersion: gitmootPieceVersion, ActionName: "run_pipeline",
			Input:            map[string]any{"pipeline_name": pipelineName, "auth": connectionExpression(bridgeConnectionExternalID)},
			PropertySettings: map[string]any{}, ErrorHandlingOptions: &struct{}{},
		},
	}
	if len(t.Map) > 0 {
		payload := make(map[string]string, len(t.Map))
		keys := make([]string, 0, len(t.Map))
		for key := range t.Map {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			expression, err := triggerSelectorExpression(t.Map[key])
			if err != nil {
				return "", nil, fmt.Errorf("trigger map output %q: %w", key, err)
			}
			payload[key] = expression
		}
		action.Settings.Input["payload"] = payload
	}
	flow := triggerFlow{
		DisplayName: displayName, SchemaVersion: "20",
		Trigger: triggerFlowAction{
			Name: "trigger", Valid: true, DisplayName: "Receive email over IMAP", Type: "PIECE_TRIGGER",
			LastUpdatedDate: "2026-01-01T00:00:00.000Z",
			Settings: triggerFlowSettings{
				PieceName: "@activepieces/piece-imap", PieceVersion: imapPieceVersion, TriggerName: "new_email",
				Input: map[string]any{"auth": connectionExpression(t.Connection), "mailbox": t.Mailbox}, PropertySettings: map[string]any{},
			},
			NextAction: action,
		},
	}
	raw, err := json.Marshal(flow)
	if err != nil {
		return "", nil, fmt.Errorf("marshal Activepieces trigger flow: %w", err)
	}
	return displayName, raw, nil
}

func triggerSelectorExpression(selector string) (string, error) {
	switch selector {
	case "subject":
		return "{{trigger['subject']}}", nil
	case "from_address":
		return "{{trigger['from']['value'][0]['address']}}", nil
	case "text":
		return "{{trigger['text']}}", nil
	case "message_id":
		return "{{trigger['messageId']}}", nil
	case "date":
		return "{{trigger['date']}}", nil
	default:
		return "", fmt.Errorf("unsupported selector %q", selector)
	}
}

func connectionExpression(externalID string) string {
	return "{{connections['" + externalID + "']}}"
}
