package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

// followRenamedOrgSeats keeps "agent:NAME" seat bindings attached to their
// agent across a Herdr rename (#2502). Herdr has no stable agent id, so the
// daemon records the pane and session reference each binding resolves to.
// When the name stops resolving but exactly one live agent sits in that same
// pane with that same session, it is the same agent under a new name: the
// seat's binding in the org config is rewritten to it (owner decision,
// 2026-11-03) and the seat's lead is told. Anything less certain is never
// rebound; the lead is told once per unresolved episode instead, because
// messages to the seat only queue until someone rebinds it.
func followRenamedOrgSeats(ctx context.Context, store *db.Store, configFile string, roles []config.OrgRole, snapshot org.Snapshot, stdout io.Writer, now time.Time) {
	if store == nil || configFile == "" {
		return
	}
	for _, role := range roles {
		name, ok := strings.CutPrefix(strings.TrimSpace(role.Pane), "agent:")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if _, observed := snapshot.States[role.Name]; !observed {
			// Not in this snapshot (archived or not a roster member): not ours
			// to judge.
			continue
		}
		binding := snapshot.PaneBindings[role.Name]
		if binding.PaneID != "" && !binding.Ambiguous {
			if binding.Session != "" {
				if err := store.RecordOrgSeatAgentIdentity(ctx, role.Name, binding.AgentName, binding.PaneID, binding.Session, now); err != nil {
					writeLine(stdout, "org seat %s identity record failed: %v", role.Name, err)
				}
			}
			continue
		}
		known, found, err := store.GetOrgSeatAgentIdentity(ctx, role.Name)
		if err != nil {
			writeLine(stdout, "org seat %s identity read failed: %v", role.Name, err)
			continue
		}
		if !found || known.AgentName != name {
			// Nothing recorded for this binding (never resolved since it was
			// set): no evidence to follow or report.
			continue
		}
		if renamed, ok := renamedSeatAgent(snapshot.Agents, known); ok {
			desired := role
			desired.Pane = "agent:" + renamed
			if _, _, err := config.UpsertOrgSeatRole(config.Paths{ConfigFile: configFile}, desired, role.Pane); err != nil {
				writeLine(stdout, "org seat %s rebind to %s failed: %v", role.Name, desired.Pane, err)
				continue
			}
			if err := store.RecordOrgSeatAgentIdentity(ctx, role.Name, renamed, known.PaneID, known.Session, now); err != nil {
				writeLine(stdout, "org seat %s identity record failed: %v", role.Name, err)
			}
			writeLine(stdout, "org seat %s followed its agent renamed %s -> %s (pane %s, same session)", role.Name, name, renamed, known.PaneID)
			notifyOrgSeatLead(ctx, store, role, stdout, fmt.Sprintf(
				"gitmoot: the Herdr agent of seat %s was renamed from %q to %q (same pane %s and session). The seat's org config binding now reads %s; queued messages are delivered to it. No action needed.",
				role.Name, name, renamed, known.PaneID, desired.Pane))
			continue
		}
		first, err := store.MarkOrgSeatIdentityAlerted(ctx, role.Name, now)
		if err != nil || !first {
			continue
		}
		occupant := "no agent runs there now"
		for _, agent := range snapshot.Agents {
			if agent.PaneID == known.PaneID {
				occupant = fmt.Sprintf("agent %q runs there now, in a different session", agent.Name)
				break
			}
		}
		notifyOrgSeatLead(ctx, store, role, stdout, fmt.Sprintf(
			"gitmoot: seat %s is unreachable: no live agent is named %q (its binding). It last ran in pane %s; %s. Messages to %s are queued until it is rebound: gitmoot org seat bind --name %s --agent NAME.",
			role.Name, name, known.PaneID, occupant, role.Name, role.Name))
	}
}

// renamedSeatAgent returns the new name of the recorded agent when exactly one
// live agent has its pane and session and that name resolves uniquely.
func renamedSeatAgent(agents []org.LiveAgent, known db.OrgSeatAgentIdentity) (string, bool) {
	if strings.TrimSpace(known.Session) == "" {
		return "", false
	}
	match := ""
	for _, agent := range agents {
		if agent.PaneID == known.PaneID && agent.Session == known.Session {
			if match != "" {
				return "", false
			}
			match = agent.Name
		}
	}
	if strings.TrimSpace(match) == "" || match == known.AgentName {
		return "", false
	}
	named := 0
	for _, agent := range agents {
		if agent.Name == match {
			named++
		}
	}
	return match, named == 1
}

func notifyOrgSeatLead(ctx context.Context, store *db.Store, role config.OrgRole, stdout io.Writer, body string) {
	lead := strings.TrimSpace(role.Parent)
	if lead == "" {
		return
	}
	if _, err := store.CreateMessage(ctx, db.Message{Sender: db.MessageSystemSender, Recipient: lead, Body: body}); err != nil {
		writeLine(stdout, "org seat %s notice to %s failed: %v", role.Name, lead, err)
	}
}
