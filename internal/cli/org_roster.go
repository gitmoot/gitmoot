package cli

import (
	"context"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// This file is THE roster choke-point (#1635, herdrup#173). Every production
// decision over "which org seats exist right now" draws from resolveOrgRoster /
// loadOrgRoster instead of iterating config.OrgConfig.Roles() directly, so that
// seat-lifecycle exclusions (archived, paused) are applied in exactly one place.
// TestOrgRosterSeamIsTheOnlyRolesCallSite pins that invariant: a new direct
// cfg.Roles() call site fails CI until it is routed through the roster.
//
// Lifecycle contract (herdrup#173): herdr owns archive state; gitmoot only
// READS it (an `archived` block on `herdr agent list --json`). A seat absent
// from the archived observations is ACTIVE — so nil observations reproduce the
// pre-#1635 roster exactly, and either side can deploy first.

// orgSeatStanding classifies a configured role's roster membership.
type orgSeatStanding string

const (
	// orgSeatActive: full roster member — appears everywhere, receives
	// sweeps, nudges, and automated wakes.
	orgSeatActive orgSeatStanding = "active"
	// orgSeatPaused: soft exclusion — still a live roster member (chart,
	// health, presence, provider, dispatch, escalation routing) but excluded
	// from nudge ladders, idle/blocked alarms, and automated wakes.
	orgSeatPaused orgSeatStanding = "paused"
	// orgSeatArchived: full exclusion — out of rotation entirely; the seat has
	// no pane and its open work is parked (rendered by `org seats archived`).
	orgSeatArchived orgSeatStanding = "archived"
)

// orgArchivedObservation is one observed herdr archive record: the `archived`
// block herdr serializes per agent, plus when gitmoot observed it. Presence of
// the block means archived; absence means active.
type orgArchivedObservation struct {
	At         time.Time
	By         string
	Reason     string
	ObservedAt time.Time
}

// orgArchivedSeat pairs a configured role with its archive observation for the
// `org seats archived` view.
type orgArchivedSeat struct {
	Role config.OrgRole
	orgArchivedObservation
}

type orgRoster struct {
	members   []config.OrgRole
	nudgeable []config.OrgRole
	archived  []orgArchivedSeat
	standing  map[string]orgSeatStanding
}

// resolveOrgRoster classifies every configured role. Observation maps are keyed
// by role name; lookups are case-insensitive on the role side and expect
// lower-case keys on the supplier side. paused maps role name -> reason.
func resolveOrgRoster(cfg config.OrgConfig, archived map[string]orgArchivedObservation, paused map[string]string) orgRoster {
	roster := orgRoster{standing: map[string]orgSeatStanding{}}
	for _, role := range cfg.Roles() {
		key := strings.ToLower(strings.TrimSpace(role.Name))
		if observation, ok := archived[key]; ok {
			roster.standing[key] = orgSeatArchived
			roster.archived = append(roster.archived, orgArchivedSeat{Role: role, orgArchivedObservation: observation})
			continue
		}
		roster.members = append(roster.members, role)
		if _, ok := paused[key]; ok {
			roster.standing[key] = orgSeatPaused
			continue
		}
		roster.standing[key] = orgSeatActive
		roster.nudgeable = append(roster.nudgeable, role)
	}
	return roster
}

// Members returns the live roster: active + paused seats. Chart, health,
// presence, provider construction, dispatch, and escalation routing use this
// view.
func (r orgRoster) Members() []config.OrgRole { return r.members }

// Nudgeable returns active seats only. Sweeps, nudge ladders, idle/blocked
// alarms, and automated wakes use this view.
func (r orgRoster) Nudgeable() []config.OrgRole { return r.nudgeable }

// Archived returns the observed archived seats for the `org seats archived`
// view.
func (r orgRoster) Archived() []orgArchivedSeat { return r.archived }

// Standing reports a role's classification. Unknown (unconfigured) roles read
// active: absence of lifecycle evidence must never exclude a seat.
func (r orgRoster) Standing(role string) orgSeatStanding {
	if s, ok := r.standing[strings.ToLower(strings.TrimSpace(role))]; ok {
		return s
	}
	return orgSeatActive
}

// loadOrgRosterObservations is the ONE supplier seam for lifecycle
// observations. It returns nothing today; the #1635 ingest (the herdr
// `agent list --json` reader and its durable mirror) lands inside this
// function without touching any roster consumer. A nil store must stay
// valid: several call sites resolve the roster without an open store, and
// missing observations mean an unfiltered (all-active) roster by contract.
func loadOrgRosterObservations(ctx context.Context, store *db.Store) (archived map[string]orgArchivedObservation, paused map[string]string) {
	_ = ctx
	_ = store
	return nil, nil
}

// loadOrgRoster resolves the roster with observations from the supplier seam.
// store may be nil (observation-less roster).
func loadOrgRoster(ctx context.Context, store *db.Store, cfg config.OrgConfig) orgRoster {
	archived, paused := loadOrgRosterObservations(ctx, store)
	return resolveOrgRoster(cfg, archived, paused)
}
