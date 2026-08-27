package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// parkOutstandingDirectivesForArchivedSeats parks every open directive
// obligation addressed to a seat in the archived set (#1635). Nothing calls it
// yet: the #1635 ingest invokes it once archived observations exist, the same
// late-supplier shape as loadOrgRosterObservations.
//
// PARK-ONLY BY DESIGN. Parking is safe under partial observation — the worst
// case is a directive that pauses nagging. Unparking is the dangerous
// direction: driven off an absent or degraded read it would resume nag ladders
// for every archived seat, the exact herdr-down failure the #1635 mirror
// answers forbid. Unpark therefore stays an explicit per-role act
// (UnparkOrgDirectivesForRole) keyed to an OBSERVED archived->active
// transition, which only the ingest can attest.
func parkOutstandingDirectivesForArchivedSeats(ctx context.Context, store *db.Store, archived map[string]orgArchivedObservation, now time.Time) (int64, error) {
	if store == nil || len(archived) == 0 {
		return 0, nil
	}
	roles := make([]string, 0, len(archived))
	for role := range archived {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var parked int64
	for _, role := range roles {
		observation := archived[role]
		reason := fmt.Sprintf("seat archived %s by %s", observation.At.UTC().Format(time.RFC3339), strings.TrimSpace(observation.By))
		if detail := strings.TrimSpace(observation.Reason); detail != "" {
			reason += ": " + detail
		}
		n, err := store.ParkOpenOrgDirectivesForRole(ctx, role, now, reason)
		if err != nil {
			return parked, fmt.Errorf("park directives for archived seat %q: %w", role, err)
		}
		parked += n
	}
	return parked, nil
}
