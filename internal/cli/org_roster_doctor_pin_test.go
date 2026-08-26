package cli

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// Pins the doctor's VIEW CHOICE (#1637 round 3, ordered by GITMOOT-COC): the
// idle-activity check monitors Nudgeable() — a paused seat is deliberately
// idle, so warning that it is idle is noise. Reverting
// monitoredOrgActivityRoles to Members() must fail here; before this pin that
// revert broke no test.
func TestMonitoredOrgActivityRolesExcludePausedSeats(t *testing.T) {
	cfg := loadOrgActivityTestConfig(t)
	original := loadOrgRosterObservations
	loadOrgRosterObservations = func(context.Context, *db.Store) (map[string]orgArchivedObservation, map[string]string) {
		return nil, map[string]string{"owner": "deliberately idle"}
	}
	t.Cleanup(func() { loadOrgRosterObservations = original })

	roles := monitoredOrgActivityRoles(cfg)
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
	}
	if len(names) != 1 || names[0] != "review" {
		t.Fatalf("monitored roles = %v, want only the active review seat — a paused seat must not be idle-warned", names)
	}
	// And the supplier restored: with no observations both monitored roles
	// return, proving the exclusion above came from the paused classification
	// rather than from the recycle_after filter.
	loadOrgRosterObservations = original
	roles = monitoredOrgActivityRoles(cfg)
	if len(roles) != 2 {
		t.Fatalf("baseline monitored roles = %d, want 2 — the fixture no longer exercises the paused exclusion", len(roles))
	}
}
