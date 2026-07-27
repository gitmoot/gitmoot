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
				PaneID            string                  `json:"pane_id"`
				Label             string                  `json:"label"`
				AgentStatus       string                  `json:"agent_status"`
				LastCompletedTurn *herdrCompletedTurnWire `json:"last_completed_turn"`
			} `json:"panes"`
		} `json:"snapshot"`
	} `json:"result"`
}

type herdrCompletedTurnWire struct {
	Turn            *json.Number `json:"turn"`
	TurnEpoch       *json.Number `json:"turn_epoch"`
	CompletedUnixMS *json.Number `json:"completed_unix_ms"`
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

	byLabel := map[string][]org.RoleLiveState{}
	labelToPaneIDs := map[string][]string{}
	stateByPaneID := map[string]org.RoleLiveState{}
	for _, pane := range decoded.Result.Snapshot.Panes {
		state := mapHerdrAgentStatus(pane.AgentStatus)
		state.Activity = mapHerdrCompletedTurn(pane.LastCompletedTurn)
		// Mirror the wake resolver (herdr.go resolvePaneByLabel, which matches only
		// `p.PaneID != ""`): a pane with an empty pane_id is not a resolvable
		// target. Seeding it would collide every empty-id pane on the "" key of
		// stateByPaneID and let a binding read the wrong pane's state.
		if pane.PaneID != "" {
			stateByPaneID[pane.PaneID] = state
			if pane.Label != "" {
				labelToPaneIDs[pane.Label] = append(labelToPaneIDs[pane.Label], pane.PaneID)
			}
		}
		if pane.Label != "" {
			byLabel[pane.Label] = append(byLabel[pane.Label], state)
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
			state, present := stateByPaneID[paneID]
			if !present {
				states[role.Name] = org.RoleLiveState{State: org.StateUnknown, Detail: fmt.Sprintf("no Herdr pane bound as %q", binding)}
				continue
			}
			states[role.Name] = state
			continue
		}

		matches := byLabel[role.Name]
		switch len(matches) {
		case 0:
			states[role.Name] = org.RoleLiveState{State: org.StateUnknown, Detail: "no Herdr pane has this exact role label"}
		case 1:
			states[role.Name] = matches[0]
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

func mapHerdrCompletedTurn(turn *herdrCompletedTurnWire) *org.RoleActivity {
	if turn == nil {
		return nil
	}
	turnNumber, turnOK := positiveHerdrInt64(turn.Turn)
	turnEpoch, epochOK := positiveHerdrInt64(turn.TurnEpoch)
	completedUnixMS, completedOK := positiveHerdrInt64(turn.CompletedUnixMS)
	if !turnOK || !epochOK || !completedOK {
		return nil
	}
	return &org.RoleActivity{
		Turn:        turnNumber,
		TurnEpoch:   turnEpoch,
		CompletedAt: time.UnixMilli(completedUnixMS).UTC(),
	}
}

func positiveHerdrInt64(value *json.Number) (int64, bool) {
	if value == nil {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value.String(), 10, 64)
	return parsed, err == nil && parsed > 0
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
	args := []string{"agent", "start", agentName, "--kind", kind, "--pane", pane, "--timeout", strconv.Itoa(herdrOrgRecycleTimeoutMS), "--"}
	// Herdr snapshots do not expose the running model, so this can enforce the
	// configured pin at recycle but cannot detect live-vs-pinned drift until
	// Herdr provides that signal. Only inject --model for kinds gitmoot has
	// verified support it (its own codex/claude/kimi runtimes) — herdr's other
	// ~18 agent kinds are unverified and a rejected flag would break recycle
	// outright, so an unsupported kind silently ignores the pin.
	if model := strings.TrimSpace(req.Model); model != "" && herdrKindSupportsModelFlag(kind) {
		args = append(args, "--model", model)
	}
	args = append(args, req.BootPrompt)
	_, err := p.run(bounded, args...)
	if err != nil {
		return fmt.Errorf("herdr agent start for org role %q (pane %q must already be at an interactive shell prompt): %w", role, pane, err)
	}
	return nil
}

// herdrKindSupportsModelFlag reports whether the named herdr agent kind's CLI
// is verified to accept a startup `-m/--model <value>` flag. Restricted to
// gitmoot's own three driven runtimes (codex, claude, kimi) — herdr supports
// many more kinds, but their model-flag support has not been checked.
func herdrKindSupportsModelFlag(kind string) bool {
	switch kind {
	case "codex", "claude", "kimi":
		return true
	default:
		return false
	}
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
