package cli

import (
	"context"
	"errors"
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
// 2026-11-03), only while the config still reads the old binding, and the
// seat's parent is told. Any other unresolved binding is never rebound; the
// parent is told once per unresolved binding instead, because messages to the
// seat only queue until someone rebinds it.
func followRenamedOrgSeats(ctx context.Context, store *db.Store, configFile string, roles []config.OrgRole, snapshot org.Snapshot, stdout io.Writer, now time.Time) {
	if store == nil || configFile == "" {
		return
	}
	for _, role := range roles {
		binding := strings.TrimSpace(role.Pane)
		name, ok := strings.CutPrefix(binding, "agent:")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if _, observed := snapshot.States[role.Name]; !observed {
			// Not in this snapshot (archived or not a roster member): not ours
			// to judge.
			continue
		}
		resolved := snapshot.PaneBindings[role.Name]
		if resolved.PaneID != "" && !resolved.Ambiguous {
			// Any resolution replaces the proof and ends an alert episode; one
			// without a session leaves no proof, so it is never followed.
			if err := store.RecordOrgSeatAgentIdentity(ctx, role.Name, resolved.AgentName, resolved.PaneID, resolved.Session, now); err != nil {
				writeLine(stdout, "org seat %s identity record failed: %v", role.Name, err)
			}
			continue
		}
		known, found, err := store.GetOrgSeatAgentIdentity(ctx, role.Name)
		if err != nil {
			writeLine(stdout, "org seat %s identity read failed: %v", role.Name, err)
			continue
		}
		if found && known.AgentName == name {
			if renamed, ok := renamedSeatAgent(snapshot.Agents, known); ok {
				followSeatRename(ctx, store, configFile, role, name, renamed, known, stdout, now)
				continue
			}
		}
		alertUnreachableSeat(ctx, store, role, name, known, found && known.AgentName == name, snapshot.Agents, stdout, now)
	}
}

func followSeatRename(ctx context.Context, store *db.Store, configFile string, role config.OrgRole, name, renamed string, known db.OrgSeatAgentIdentity, stdout io.Writer, now time.Time) {
	to := "agent:" + renamed
	if err := config.RebindOrgSeatPane(config.Paths{ConfigFile: configFile}, role.Name, role.Pane, to); err != nil {
		if errors.Is(err, config.ErrOrgSeatBindingChanged) {
			// An operator changed or removed the seat meanwhile: theirs wins;
			// the next tick works from the new config.
			writeLine(stdout, "org seat %s rename not followed: %v", role.Name, err)
			return
		}
		writeLine(stdout, "org seat %s rebind to %s failed: %v", role.Name, to, err)
		return
	}
	if err := store.RecordOrgSeatAgentIdentity(ctx, role.Name, renamed, known.PaneID, known.Session, now); err != nil {
		writeLine(stdout, "org seat %s identity record failed: %v", role.Name, err)
	}
	writeLine(stdout, "org seat %s followed its agent renamed %s -> %s (pane %s, same session)", role.Name, name, renamed, known.PaneID)
	if lead := strings.TrimSpace(role.Parent); lead != "" {
		if _, err := store.CreateMessage(ctx, db.Message{Sender: db.MessageSystemSender, Recipient: lead, Body: fmt.Sprintf(
			"gitmoot: the Herdr agent of seat %s was renamed from %q to %q (same pane %s and session). The seat's org config binding now reads %s; queued messages are delivered to it. No action needed.",
			role.Name, name, renamed, known.PaneID, to)}); err != nil {
			writeLine(stdout, "org seat %s rename notice to %s failed: %v", role.Name, lead, err)
		}
	}
}

func alertUnreachableSeat(ctx context.Context, store *db.Store, role config.OrgRole, name string, known db.OrgSeatAgentIdentity, matchesBinding bool, agents []org.LiveAgent, stdout io.Writer, now time.Time) {
	lead := strings.TrimSpace(role.Parent)
	if lead == "" {
		return
	}
	where := "gitmoot has no record of where it last ran"
	if matchesBinding && known.PaneID != "" {
		occupant := "no agent runs there now"
		for _, agent := range agents {
			if agent.PaneID == known.PaneID {
				occupant = fmt.Sprintf("agent %q runs there now, not provably the same agent", agent.Name)
				break
			}
		}
		where = fmt.Sprintf("it last ran in pane %s; %s", known.PaneID, occupant)
	}
	body := fmt.Sprintf(
		"gitmoot: seat %s is unreachable: no live agent is named %q (its binding); %s. Messages to %s are queued until it is rebound: gitmoot org seat bind --name %s --agent NAME.",
		role.Name, name, where, role.Name, role.Name)
	if _, err := store.AlertOrgSeatUnreachableOnce(ctx, role.Name, strings.TrimSpace(role.Pane), lead, body, now); err != nil {
		writeLine(stdout, "org seat %s unreachable notice to %s failed (retried next tick): %v", role.Name, lead, err)
	}
}

// renamedSeatAgent returns the new name of the recorded agent when exactly one
// live agent has its pane and session and that name resolves uniquely.
func renamedSeatAgent(agents []org.LiveAgent, known db.OrgSeatAgentIdentity) (string, bool) {
	if strings.TrimSpace(known.Session) == "" || strings.TrimSpace(known.PaneID) == "" {
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
