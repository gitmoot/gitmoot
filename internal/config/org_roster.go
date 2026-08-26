package config

import (
	"strings"
	"time"
)

// Roster resolution lives HERE, next to the role registry, so that the raw
// role map has no exported bulk accessor at all (#1635, PR #1637 round 3).
// OrgConfig.roles is reachable outside this package only through
// ResolveOrgRoster, which applies seat-lifecycle classification. The former
// OrgConfig.Roles() accessor is unexported (sortedRoles()): a direct call, method
// value, method expression, or reflection invocation from another package is
// a compile error or a reflect refusal — not a test finding.
//
// STATED LIMITS, deliberately documented rather than left latent
// (the #1626 disposition — a reviewer producing one of these later is the
// statement working, not a finding):
//   - ResolveOrgRoster is exported (the cli seam must call it) and
//     ResolveOrgRoster(cfg, nil, nil).Members() is the unfiltered roster.
//     TestOrgRosterSeamIsTheOnlyResolveCallSite censuses every reference to
//     it and allows exactly the seam.
//   - A deliberate Roots()+Children() structural walk can rebuild the role
//     set. Those accessors serve chart/escalation-path structure and stay; a
//     reconstruction through them is a review-visible act, not an accident.
//   - Code inside this package can call sortedRoles() directly.

// OrgSeatStanding classifies a configured role's roster membership.
type OrgSeatStanding string

const (
	// OrgSeatActive: full roster member — appears everywhere, receives
	// sweeps, nudges, and automated wakes.
	OrgSeatActive OrgSeatStanding = "active"
	// OrgSeatPaused: soft exclusion — still a live roster member (chart,
	// health, presence, provider, dispatch, escalation routing) but excluded
	// from nudge ladders, idle/blocked alarms, and automated wakes.
	OrgSeatPaused OrgSeatStanding = "paused"
	// OrgSeatArchived: full exclusion — out of rotation entirely; the seat has
	// no pane and its open work is parked (rendered by `org seats archived`).
	OrgSeatArchived OrgSeatStanding = "archived"
)

// OrgArchivedObservation is one observed herdr archive record: the `archived`
// block herdr serializes per agent, plus when gitmoot observed it. Presence of
// the block means archived; absence means active.
type OrgArchivedObservation struct {
	At         time.Time
	By         string
	Reason     string
	ObservedAt time.Time
}

// OrgArchivedSeat pairs a configured role with its archive observation for the
// `org seats archived` view.
type OrgArchivedSeat struct {
	Role OrgRole
	OrgArchivedObservation
}

// OrgRoster is the lifecycle-classified view of the configured roles.
type OrgRoster struct {
	members   []OrgRole
	nudgeable []OrgRole
	archived  []OrgArchivedSeat
	standing  map[string]OrgSeatStanding
}

// ResolveOrgRoster classifies every configured role. It is the ONLY way to
// enumerate roles from outside this package, and its one production caller is
// the cli roster seam (loadOrgRoster) — enforced by the reference census in
// internal/cli/org_roster_guard_test.go. Observation maps are keyed by role
// name; lookups are case-insensitive on the role side and expect lower-case
// keys on the supplier side. paused maps role name -> reason. Nil observation
// maps reproduce the unclassified roster exactly (missing evidence means
// ACTIVE, the herdrup#173 deploy-in-either-order contract).
func ResolveOrgRoster(cfg OrgConfig, archived map[string]OrgArchivedObservation, paused map[string]string) OrgRoster {
	roster := OrgRoster{standing: map[string]OrgSeatStanding{}}
	for _, role := range cfg.sortedRoles() {
		key := strings.ToLower(strings.TrimSpace(role.Name))
		if observation, ok := archived[key]; ok {
			roster.standing[key] = OrgSeatArchived
			roster.archived = append(roster.archived, OrgArchivedSeat{Role: role, OrgArchivedObservation: observation})
			continue
		}
		roster.members = append(roster.members, role)
		if _, ok := paused[key]; ok {
			roster.standing[key] = OrgSeatPaused
			continue
		}
		roster.standing[key] = OrgSeatActive
		roster.nudgeable = append(roster.nudgeable, role)
	}
	return roster
}

// Members returns the live roster: active + paused seats. Chart, health,
// presence, provider construction, dispatch, and escalation routing use this
// view.
func (r OrgRoster) Members() []OrgRole { return r.members }

// Nudgeable returns active seats only. Sweeps, nudge ladders, idle/blocked
// alarms, and automated wakes use this view.
func (r OrgRoster) Nudgeable() []OrgRole { return r.nudgeable }

// Archived returns the observed archived seats. Zero production callers today
// by design: the `org seats archived` view arrives with the #1635 ingest PR.
func (r OrgRoster) Archived() []OrgArchivedSeat { return r.archived }

// Standing reports a role's classification. Unknown (unconfigured) roles read
// active: absence of lifecycle evidence must never exclude a seat. Zero
// production callers today by design: dispatch/recycle refusals arrive with
// the #1635 ingest PR.
func (r OrgRoster) Standing(role string) OrgSeatStanding {
	if s, ok := r.standing[strings.ToLower(strings.TrimSpace(role))]; ok {
		return s
	}
	return OrgSeatActive
}
