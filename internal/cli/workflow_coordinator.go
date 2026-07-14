package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const workflowHerdrLookupTimeout = 2 * time.Second

type workflowCoordinatorIdentity struct {
	Pane      string
	SessionID string
	WorkDir   string
}

// detectWorkflowCoordinatorIdentity is deliberately fail-open: workflow notes
// remain useful even when Herdr is absent, unavailable, or returns bad data.
func detectWorkflowCoordinatorIdentity(ctx context.Context) workflowCoordinatorIdentity {
	if strings.TrimSpace(os.Getenv("HERDR_SOCKET_PATH")) == "" && os.Getenv("HERDR_ENV") != "1" {
		return workflowCoordinatorIdentity{}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, workflowHerdrLookupTimeout)
	defer cancel()
	result, err := (subprocess.GroupRunner{}).Run(lookupCtx, "", "herdr", "pane", "current", "--current")
	if err != nil {
		return workflowCoordinatorIdentity{}
	}

	var envelope struct {
		Result struct {
			Pane struct {
				Label         string `json:"label"`
				PaneID        string `json:"pane_id"`
				CWD           string `json:"cwd"`
				ForegroundCWD string `json:"foreground_cwd"`
				AgentSession  struct {
					Value string `json:"value"`
				} `json:"agent_session"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &envelope); err != nil {
		return workflowCoordinatorIdentity{}
	}
	pane := strings.TrimSpace(envelope.Result.Pane.Label)
	if pane == "" {
		pane = strings.TrimSpace(envelope.Result.Pane.PaneID)
	}
	workDir := strings.TrimSpace(envelope.Result.Pane.CWD)
	if workDir == "" {
		workDir = strings.TrimSpace(envelope.Result.Pane.ForegroundCWD)
	}
	return workflowCoordinatorIdentity{
		Pane:      pane,
		SessionID: strings.TrimSpace(envelope.Result.Pane.AgentSession.Value),
		WorkDir:   workDir,
	}
}

func isFullWorkflowSessionUUID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 36 {
		return false
	}
	for i := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		if !isWorkflowUUIDHex(value[i]) {
			return false
		}
	}
	return true
}

func isWorkflowUUIDHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}
