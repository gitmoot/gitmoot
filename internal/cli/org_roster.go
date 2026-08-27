package cli

import (
	"context"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// This file is THE roster choke-point (#1635, herdrup#173). Every production
// decision over "which org seats exist right now" draws from loadOrgRoster.
// Roster resolution itself lives in internal/config (config.ResolveOrgRoster),
// where the raw role registry has NO exported bulk accessor: a consumer that
// tries to enumerate roles without lifecycle classification does not compile.
// This file holds the one production ResolveOrgRoster call site and the
// observation supplier; TestOrgRosterSeamIsTheOnlyResolveCallSite censuses
// every reference to ResolveOrgRoster and allows exactly this file.
//
// Lifecycle contract (herdrup#173): herdr owns archive state; gitmoot only
// READS it (an `archived` block on `herdr agent list`, JSON by default). A
// seat absent from the archived observations is ACTIVE — so nil observations
// reproduce the pre-#1635 roster exactly, and either side can deploy first.
//
// STATED LIMITS of the boundary (kept in sync with config/org_roster.go, the
// #1626 disposition): ResolveOrgRoster stays exported and nil-fed calls yield
// the unfiltered roster (census-guarded); a deliberate Roots()+Children()
// structural walk can rebuild the role set (review-visible, accepted);
// config-internal code can call sortedRoles() directly.

type orgSeatStanding = config.OrgSeatStanding

const (
	orgSeatActive   = config.OrgSeatActive
	orgSeatPaused   = config.OrgSeatPaused
	orgSeatArchived = config.OrgSeatArchived
)

type orgArchivedObservation = config.OrgArchivedObservation

type orgArchivedSeat = config.OrgArchivedSeat

type orgRoster = config.OrgRoster

// resolveOrgRoster is the seam's pure half, kept as a local name so tests and
// the stacked parking work keep one spelling. It is the single production
// reference to config.ResolveOrgRoster.
func resolveOrgRoster(cfg config.OrgConfig, archived map[string]orgArchivedObservation, paused map[string]string) orgRoster {
	return config.ResolveOrgRoster(cfg, archived, paused)
}

// loadOrgRosterObservations is the ONE supplier seam for lifecycle
// observations: it reads the org_role_archived mirror, which the daemon
// one-minute lane refreshes from successful `herdr agent list` reads
// (refreshOrgArchiveMirror — the single write site). A nil store must stay
// valid: several call sites resolve the roster without an open store, and
// missing observations mean an unfiltered (all-active) roster by contract.
// A mirror READ error also yields the unfiltered roster — exclusion requires
// evidence, and a local store that cannot be read is a loud systemic failure
// elsewhere; the doctor's staleness check covers the quiet variant. Paused
// still has no source (its config field is a separate, owner-visible
// decision). A package var so tests can classify seats without an ingest.
var loadOrgRosterObservations = func(ctx context.Context, store *db.Store) (archived map[string]orgArchivedObservation, paused map[string]string) {
	if store == nil {
		return nil, nil
	}
	rows, err := store.ListOrgRolesArchived(ctx)
	if err != nil || len(rows) == 0 {
		return nil, nil
	}
	archived = make(map[string]orgArchivedObservation, len(rows))
	for _, row := range rows {
		at, _ := time.Parse(time.RFC3339Nano, row.ArchivedAt)
		observedAt, _ := time.Parse(time.RFC3339Nano, row.ObservedAt)
		archived[row.Role] = orgArchivedObservation{
			At: at, By: row.ArchivedBy, Reason: row.Reason, ObservedAt: observedAt,
		}
	}
	return archived, nil
}

// loadOrgRoster resolves the roster with observations from the supplier seam.
// store may be nil (observation-less roster).
func loadOrgRoster(ctx context.Context, store *db.Store, cfg config.OrgConfig) orgRoster {
	archived, paused := loadOrgRosterObservations(ctx, store)
	return resolveOrgRoster(cfg, archived, paused)
}
