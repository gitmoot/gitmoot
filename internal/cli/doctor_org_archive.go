package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
)

const orgArchiveMirrorDoctorCheckName = "org archived-seat mirror"

// orgArchiveMirrorDoctorCheck is the LOUD half of the #1635 staleness answer
// (PLAN v2, note 85481): the mirror never silently expires toward inclusion —
// exclusions hold through a herdr outage — and THIS check is what makes that
// outage visible instead of quiet. It warns when archived rows exist and the
// last successful `herdr agent list` poll is older than orgArchiveStaleAfter
// (~15 missed one-minute lane ticks). No archived rows -> the check is absent:
// a herdr-less deployment must not see it.
func orgArchiveMirrorDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	if strings.TrimSpace(paths.Database) == "" {
		return doctor.Check{}, false
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{}, false
	}
	defer store.Close()
	rows, err := store.ListOrgRolesArchived(context.Background())
	if err != nil {
		return doctor.Check{}, false
	}
	pending, err := store.ListOrgArchivePending(context.Background())
	if err != nil {
		pending = nil
	}
	if len(rows) == 0 && len(pending) == 0 {
		return doctor.Check{}, false
	}
	last, ok, err := store.OrgArchivePollLastSuccess(context.Background())
	if err != nil {
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: fmt.Sprintf("%d archived seats mirrored but the poll-success stamp is unreadable: %v (exclusions preserved)", len(rows), err),
		}, true
	}
	return buildOrgArchiveMirrorDoctorCheck(rows, pending, last, ok, time.Now().UTC()), true
}

func buildOrgArchiveMirrorDoctorCheck(rows []db.OrgRoleArchived, pending []db.OrgRoleArchived, last time.Time, everSucceeded bool, now time.Time) doctor.Check {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Role)
	}
	roleList := strings.Join(names, ", ")
	// The downgrade-visibility line (#1643 ruling, directive 86986): aged
	// pending rows are EXPECTED during a binary-rollback window — an old
	// binary preserves but cannot drain them — and are a problem only if they
	// persist while a current binary runs. So the line reports AGE and COUNT
	// and names that condition; it never renders a verdict on a young ledger.
	if warn, detail := pendingObservationsDetail(pending, now); warn {
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: detail + pendingMirrorSuffix(len(rows), roleList),
		}
	}
	if !everSucceeded {
		// Rows without any recorded success means the stamp was lost or the
		// schema is younger than the rows — unknown freshness, never "fresh".
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: fmt.Sprintf("%d archived seats (%s) mirrored with NO recorded successful herdr poll; freshness unknown (exclusions preserved)", len(rows), roleList),
		}
	}
	age := now.Sub(last)
	if age > orgArchiveStaleAfter {
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: fmt.Sprintf("%d archived seats (%s) held on a STALE observation — herdr unreachable since %s (%s ago); exclusions preserved by design, unarchives will not be seen until herdr returns", len(rows), roleList, last.Format(time.RFC3339), age.Round(time.Second)),
		}
	}
	return doctor.Check{
		Name: orgArchiveMirrorDoctorCheckName, OK: true, Required: false,
		Detail: fmt.Sprintf("%d archived seats (%s); last successful herdr poll %s ago", len(rows), roleList, age.Round(time.Second)),
	}
}

// pendingObservationsDetail reports whether the pending ledger warrants a
// warning: only when the OLDEST undrained observation has persisted past the
// staleness threshold. Returns the honest age-and-count line either way.
func pendingObservationsDetail(pending []db.OrgRoleArchived, now time.Time) (bool, string) {
	if len(pending) == 0 {
		return false, ""
	}
	oldest := now
	for _, row := range pending {
		if observed, err := time.Parse(time.RFC3339Nano, row.ObservedAt); err == nil && observed.Before(oldest) {
			oldest = observed
		}
	}
	age := now.Sub(oldest)
	if age <= orgArchiveStaleAfter {
		return false, ""
	}
	return true, fmt.Sprintf("%d undrained pending archive observation(s), oldest %s old — expected during a binary rollback window (an older binary preserves but cannot drain them); a problem only if they persist while the current binary is running", len(pending), age.Round(time.Second))
}

func pendingMirrorSuffix(mirrored int, roleList string) string {
	if mirrored == 0 {
		return ""
	}
	return fmt.Sprintf("; %d archived seats mirrored (%s)", mirrored, roleList)
}
