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
		if binding == "" {
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
		if resolved.Unavailable {
			// Herdr already failed to restart this seat's agent, whatever the
			// binding form: no restart is in progress, so the parent is told
			// now, with Herdr's reason (#2554).
			alertUnavailableSeat(ctx, store, role, resolved.Detail, stdout, now)
			continue
		}
		name, ok := strings.CutPrefix(binding, "agent:")
		if !ok || strings.TrimSpace(name) == "" {
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
			if restartingInPlace(snapshot.Agents, known, now) {
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

// alertUnavailableSeat tells the seat's parent, once per binding, that Herdr
// lists the seat's agent but nothing runs in its pane (#2554).
func alertUnavailableSeat(ctx context.Context, store *db.Store, role config.OrgRole, detail string, stdout io.Writer, now time.Time) {
	lead := strings.TrimSpace(role.Parent)
	if lead == "" {
		return
	}
	body := fmt.Sprintf(
		"gitmoot: seat %s is unreachable: %s. Messages to %s are queued until its agent runs again; resume its session in a pane under the same name.",
		role.Name, detail, role.Name)
	if _, err := store.AlertOrgSeatUnreachableOnce(ctx, role.Name, strings.TrimSpace(role.Pane), lead, body, now); err != nil {
		writeLine(stdout, "org seat %s unreachable notice to %s failed (retried next tick): %v", role.Name, lead, err)
	}
}

// seatRestartGrace is how long after its last resolution a seat may sit in an
// in-place restart before its parent is told it is unreachable (#2546).
// `herdr agent restart` keeps the pane and session but drops the name until
// the agent is detected again and renamed; messages only queue meanwhile.
const seatRestartGrace = 5 * time.Minute

// restartingInPlace reports whether the recorded pane looks like an in-place
// restart still inside the grace period: either no agent is in it yet, or
// only an unnamed one whose session is the recorded one or not yet reported.
// A named or different-session occupant is never a restart.
func restartingInPlace(agents []org.LiveAgent, known db.OrgSeatAgentIdentity, now time.Time) bool {
	if strings.TrimSpace(known.PaneID) == "" || strings.TrimSpace(known.Session) == "" {
		return false
	}
	resolvedAt, err := time.Parse(time.RFC3339Nano, known.ObservedAt)
	if err != nil || now.Sub(resolvedAt) >= seatRestartGrace {
		return false
	}
	for _, agent := range agents {
		if agent.PaneID != known.PaneID {
			continue
		}
		if strings.TrimSpace(agent.Name) != "" {
			return false
		}
		if session := strings.TrimSpace(agent.Session); session != "" && session != known.Session {
			return false
		}
	}
	return true
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
