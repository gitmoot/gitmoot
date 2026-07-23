package cockpit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/org"
)

type herdrOrgProvider struct {
	run   runner
	roles []config.OrgRole
	now   func() time.Time
}

const (
	herdrOrgRecycleTimeoutMS = 30000
	herdrOrgRecycleDeadline  = 35 * time.Second
)

// NewHerdrOrgProvider returns the v1 organization live-state provider.
func NewHerdrOrgProvider(roles []config.OrgRole) org.Provider {
	return newHerdrOrgProvider(newExecRunner("herdr"), roles, time.Now)
}

func newHerdrOrgProvider(run runner, roles []config.OrgRole, now func() time.Time) *herdrOrgProvider {
	roles = append([]config.OrgRole(nil), roles...)
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	return &herdrOrgProvider{run: run, roles: roles, now: now}
}

type herdrOrgSnapshotResult struct {
	Result struct {
		Snapshot struct {
			Version json.RawMessage `json:"version"`
			Panes   []struct {
				PaneID      string `json:"pane_id"`
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
	labelToPaneIDs := map[string][]string{}
	statusByPaneID := map[string]string{}
	for _, pane := range decoded.Result.Snapshot.Panes {
		// Mirror the wake resolver (herdr.go resolvePaneByLabel, which matches only
		// `p.PaneID != ""`): a pane with an empty pane_id is not a resolvable
		// target. Seeding it would collide every empty-id pane on the "" key of
		// statusByPaneID and let a binding read the wrong pane's status.
		if pane.PaneID != "" {
			statusByPaneID[pane.PaneID] = pane.AgentStatus
			if pane.Label != "" {
				labelToPaneIDs[pane.Label] = append(labelToPaneIDs[pane.Label], pane.PaneID)
			}
		}
		if pane.Label != "" {
			byLabel[pane.Label] = append(byLabel[pane.Label], pane.AgentStatus)
		}
	}
	states := make(map[string]org.RoleLiveState, len(p.roles))
	for _, role := range p.roles {
		binding := strings.TrimSpace(role.Pane)
		if binding != "" {
			if len(labelToPaneIDs[binding]) > 1 {
				states[role.Name] = org.RoleLiveState{State: org.StateUnknown, Detail: fmt.Sprintf("multiple Herdr panes labeled %q", binding)}
				continue
			}
			paneID, _ := config.ResolveRolePaneBinding(ctx, binding, func(_ context.Context, label string) (string, bool) {
				ids := labelToPaneIDs[label]
				if len(ids) == 1 {
					return ids[0], true
				}
				return "", false
			})
			status, present := statusByPaneID[paneID]
			if !present {
				states[role.Name] = org.RoleLiveState{State: org.StateUnknown, Detail: fmt.Sprintf("no Herdr pane bound as %q", binding)}
				continue
			}
			states[role.Name] = mapHerdrAgentStatus(status)
			continue
		}

		matches := byLabel[role.Name]
		switch len(matches) {
		case 0:
			states[role.Name] = org.RoleLiveState{State: org.StateUnknown, Detail: "no Herdr pane has this exact role label"}
		case 1:
			states[role.Name] = mapHerdrAgentStatus(matches[0])
		default:
			states[role.Name] = org.RoleLiveState{State: org.StateUnknown, Detail: "multiple Herdr panes have this exact role label"}
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
