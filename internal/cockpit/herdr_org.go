package cockpit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/org"
)

type herdrOrgProvider struct {
	run   runner
	roles []string
	now   func() time.Time
}

const (
	herdrOrgRecycleTimeoutMS = 30000
	herdrOrgRecycleDeadline  = 35 * time.Second
)

// NewHerdrOrgProvider returns the v1 organization live-state provider. Role
// identity is exact pane-label equality; runtime/agent names are never inferred.
func NewHerdrOrgProvider(roles []string) org.Provider {
	return newHerdrOrgProvider(newExecRunner("herdr"), roles, time.Now)
}

func newHerdrOrgProvider(run runner, roles []string, now func() time.Time) *herdrOrgProvider {
	roles = append([]string(nil), roles...)
	sort.Strings(roles)
	return &herdrOrgProvider{run: run, roles: roles, now: now}
}

type herdrOrgSnapshotResult struct {
	Result struct {
		Snapshot struct {
			Version json.RawMessage `json:"version"`
			Panes   []struct {
				Label       string `json:"label"`
				AgentStatus string `json:"agent_status"`
			} `json:"panes"`
		} `json:"snapshot"`
	} `json:"result"`
}

func (p *herdrOrgProvider) Snapshot(ctx context.Context) (org.Snapshot, error) {
	if p == nil || p.run == nil {
		return org.Snapshot{}, fmt.Errorf("herdr org provider is not configured")
	}
	out, err := p.run(ctx, "api", "snapshot")
	if err != nil {
		return org.Snapshot{}, fmt.Errorf("herdr api snapshot: %w", err)
	}
	var decoded herdrOrgSnapshotResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		return org.Snapshot{}, fmt.Errorf("parse herdr api snapshot: %w", err)
	}
	version, err := herdrProviderVersion(decoded.Result.Snapshot.Version)
	if err != nil {
		return org.Snapshot{}, err
	}
	if version == "" || decoded.Result.Snapshot.Panes == nil {
		return org.Snapshot{}, fmt.Errorf("herdr api snapshot returned an incomplete snapshot shape")
	}

	byLabel := map[string][]string{}
	for _, pane := range decoded.Result.Snapshot.Panes {
		label := pane.Label
		if label == "" {
			continue
		}
		byLabel[label] = append(byLabel[label], pane.AgentStatus)
	}
	states := make(map[string]org.RoleLiveState, len(p.roles))
	for _, role := range p.roles {
		matches := byLabel[role]
		switch len(matches) {
		case 0:
			states[role] = org.RoleLiveState{State: org.StateUnknown, Detail: "no Herdr pane has this exact role label"}
		case 1:
			states[role] = mapHerdrAgentStatus(matches[0])
		default:
			states[role] = org.RoleLiveState{State: org.StateUnknown, Detail: "multiple Herdr panes have this exact role label"}
		}
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	return org.Snapshot{States: states, ObservedAt: now().UTC(), ProviderVersion: version}, nil
}

// Recycle starts a fresh interactive agent in a pane that has already returned
// to its shell prompt. Herdr cannot safely prove or cause that transition, so
// winding down the prior agent remains an explicit operator precondition.
func (p *herdrOrgProvider) Recycle(ctx context.Context, req org.RecycleRequest) error {
	if p == nil || p.run == nil {
		return fmt.Errorf("herdr org provider is not configured")
	}
	role := strings.TrimSpace(req.Role)
	pane := strings.TrimSpace(req.Pane)
	kind := strings.TrimSpace(req.Kind)
	agentName := strings.TrimSpace(req.AgentName)
	if role == "" || pane == "" || kind == "" || agentName == "" || strings.TrimSpace(req.BootPrompt) == "" {
		return fmt.Errorf("herdr recycle requires role, pane, kind, agent name, and boot prompt")
	}
	bounded, cancel := context.WithTimeout(ctx, herdrOrgRecycleDeadline)
	defer cancel()
	_, err := p.run(bounded, "agent", "start", agentName, "--kind", kind, "--pane", pane, "--timeout", strconv.Itoa(herdrOrgRecycleTimeoutMS), "--", req.BootPrompt)
	if err != nil {
		return fmt.Errorf("herdr agent start for org role %q (pane %q must already be at an interactive shell prompt): %w", role, pane, err)
	}
	return nil
}

func mapHerdrAgentStatus(raw string) org.RoleLiveState {
	switch raw {
	case string(org.StateIdle):
		return org.RoleLiveState{State: org.StateIdle}
	case string(org.StateWorking):
		return org.RoleLiveState{State: org.StateWorking}
	case string(org.StateBlocked):
		return org.RoleLiveState{State: org.StateBlocked}
	case string(org.StateDone):
		return org.RoleLiveState{State: org.StateDone}
	case "":
		return org.RoleLiveState{State: org.StateUnknown, Detail: "Herdr pane has no agent_status"}
	default:
		return org.RoleLiveState{State: org.StateUnknown, Detail: fmt.Sprintf("unknown Herdr agent_status %q", raw)}
	}
}

func herdrProviderVersion(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text), nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String(), nil
	}
	return "", fmt.Errorf("parse herdr snapshot version")
}
