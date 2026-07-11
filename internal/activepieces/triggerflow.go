package activepieces

import (
	"encoding/json"
	"fmt"
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
// official IMAP new_email -> gitmoot run_pipeline. No trigger payload is passed.
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

func connectionExpression(externalID string) string {
	return "{{connections['" + externalID + "']}}"
}
