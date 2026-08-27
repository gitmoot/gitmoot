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
	rows, err := listOrgRolesArchivedForDoctor(context.Background(), store)
	if err != nil {
		// Unreadable is not empty — the same sentence as the pending-ledger
		// branch below and the parser's missing-agents refusal (#1643 round
		// 8, codex: the round-7 fix named one field and not the adjacent one).
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: fmt.Sprintf("archived-seat mirror UNREADABLE: %v — exclusion state UNKNOWN, not healthy", err),
		}, true
	}
	pending, err := listOrgArchivePendingForDoctor(context.Background(), store)
	if err != nil {
		// Unreadable is not empty (#1643 round 7, the same sentence as the
		// parser's missing-agents refusal): a check that reports healthy when
		// it cannot see converts an unknown into a false negative.
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: fmt.Sprintf("pending archive ledger UNREADABLE: %v — freshness and drain state UNKNOWN, not healthy", err),
		}, true
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
	// Adversary 8 (#1643 round 8, opus): a seat carried on OMISSION alone is a
	// third state — not archived-confirmed, not active-observed, just UNPROVEN
	// — and the poll stamp answers "did this tick complete", not "is the
	// mirror true". Each row's observed_at is the last POSITIVE confirmation
	// (refreshed only when the archived block is re-seen, untouched by
	// omission ticks), so its age is the unproven state's own clock. Age and
	// count, condition named, no verdict on a young row.
	if warn, detail := unconfirmedExclusionsDetail(rows, now); warn {
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: detail,
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
	if last.After(now) {
		// A poll stamp in the future (clock step, corrupt row) would read as
		// negative age and pass the staleness gate as maximally fresh — the
		// third load-bearing timestamp inheriting adversary 9, found by the
		// round-9 checklist walk rather than a reviewer. Noncausal is UNKNOWN.
		return doctor.Check{
			Name: orgArchiveMirrorDoctorCheckName, OK: false, Required: false,
			Detail: fmt.Sprintf("%d archived seats (%s) but the poll-success stamp is in the FUTURE (%s vs now %s) — freshness UNKNOWN, not healthy", len(rows), roleList, last.Format(time.RFC3339), now.Format(time.RFC3339)),
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
	// Same adversary-9 rule as the exclusion check below: a pending row whose
	// observed_at cannot be used (unparseable, or in the future) has UNKNOWN
	// age — the err==nil fold treated an all-unparseable ledger as age zero.
	unusable, unusableRole := 0, ""
	oldest := now
	for _, row := range pending {
		observed, err := time.Parse(time.RFC3339Nano, row.ObservedAt)
		if err != nil || observed.After(now) {
			unusable++
			if unusableRole == "" {
				unusableRole = row.Role
			}
			continue
		}
		if observed.Before(oldest) {
			oldest = observed
		}
	}
	if unusable > 0 {
		return true, fmt.Sprintf("%d pending archive observation(s) with UNUSABLE timestamps (first: %s) — age UNKNOWN and cannot be bounded; not healthy", unusable, unusableRole)
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

// listOrgArchivePendingForDoctor is a fault-injection seam: the round-7 guard
// test makes the ledger unreadable and asserts the doctor reports UNKNOWN
// rather than healthy.
var listOrgArchivePendingForDoctor = func(ctx context.Context, store *db.Store) ([]db.OrgRoleArchived, error) {
	return store.ListOrgArchivePending(ctx)
}

// listOrgRolesArchivedForDoctor is a fault-injection seam: the round-8 guard
// test makes the mirror unreadable and asserts the doctor reports UNKNOWN
// rather than silently absenting itself.
var listOrgRolesArchivedForDoctor = func(ctx context.Context, store *db.Store) ([]db.OrgRoleArchived, error) {
	return store.ListOrgRolesArchived(ctx)
}

// unconfirmedExclusionsDetail warns when any mirrored exclusion has gone
// unconfirmed — its agent neither re-listed archived nor observed active —
// past the staleness threshold. Expected briefly (an archived agent can drop
// out of a list transiently); a problem when it persists, because the seat is
// then excluded on evidence aging without bound while the poll stamp keeps
// advancing.
func unconfirmedExclusionsDetail(rows []db.OrgRoleArchived, now time.Time) (bool, string) {
	// Adversary 9 (#1643 round 9, both families): observed_at is load-bearing
	// for this alarm, so an evidence timestamp the check cannot use — absent,
	// unparseable, or in the future (a backward clock step reads as negative
	// age) — is UNKNOWN, never "fresh". The prior skip-on-error form silenced
	// the alarm on exactly the rows that most need reporting: unreadable is
	// not young, the same sentence as the list-read rule above, one field down.
	unusable, unusableRole := 0, ""
	oldest := now
	oldestRole := ""
	count := 0
	for _, row := range rows {
		observed, err := time.Parse(time.RFC3339Nano, row.ObservedAt)
		if err != nil || observed.After(now) {
			unusable++
			if unusableRole == "" {
				unusableRole = row.Role
			}
			continue
		}
		if now.Sub(observed) <= orgArchiveStaleAfter {
			continue
		}
		count++
		if observed.Before(oldest) {
			oldest, oldestRole = observed, row.Role
		}
	}
	if unusable > 0 {
		return true, fmt.Sprintf("%d excluded seat(s) with UNUSABLE evidence timestamps (first: %s) — exclusion age UNKNOWN and cannot be bounded; not healthy", unusable, unusableRole)
	}
	if count == 0 {
		return false, ""
	}
	return true, fmt.Sprintf("%d excluded seat(s) UNCONFIRMED — neither re-listed archived nor observed active — oldest %s for %s; excluded on evidence aging without bound while polls keep succeeding", count, now.Sub(oldest).Round(time.Second), oldestRole)
}
