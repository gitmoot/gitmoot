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
	if err != nil || len(rows) == 0 {
		return doctor.Check{}, false
	}
	last, ok, err := store.OrgArchivePollLastSuccess(context.Background())
	if err != nil {
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: fmt.Sprintf("%d archived seats mirrored but the poll-success stamp is unreadable: %v (exclusions preserved)", len(rows), err),
		}, true
	}
	return buildOrgArchiveMirrorDoctorCheck(rows, last, ok, time.Now().UTC()), true
}

func buildOrgArchiveMirrorDoctorCheck(rows []db.OrgRoleArchived, last time.Time, everSucceeded bool, now time.Time) doctor.Check {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Role)
	}
	roleList := strings.Join(names, ", ")
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
