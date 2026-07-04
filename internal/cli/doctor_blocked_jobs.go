package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/doctor"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// blockedBacklogDoctorThreshold is the age past which a blocked job (one paused
// awaiting a human) counts as a stale backlog worth surfacing in `gitmoot
// doctor`. A blocked job never ends on its own, so a month-old one is almost
// always abandonable with `job cancel --state blocked` (#631).
const blockedBacklogDoctorThreshold = 720 * time.Hour // 30 days

// blockedBacklogDoctorCheck counts blocked jobs older than the threshold and, if
// the home's store can be opened read-only, returns a doctor.Check reporting the
// backlog. It is best-effort — an unset database path or an unopenable store
// (no initialized home) yields ok=false so `gitmoot doctor` falls back to its
// other checks rather than going red on a box that has no Gitmoot home yet. The
// open is read-only so the diagnostic never creates or migrates a home.
func blockedBacklogDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	if strings.TrimSpace(paths.Database) == "" {
		return doctor.Check{}, false
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{}, false
	}
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		return doctor.Check{}, false
	}
	return buildBlockedBacklogCheck(jobs, time.Now(), blockedBacklogDoctorThreshold), true
}

// buildBlockedBacklogCheck renders the blocked-jobs doctor line from a job set:
// no blocked jobs older than the threshold is an ok line; any older ones are a
// non-required warn carrying the exact bulk-dismiss command (#631). Age uses the
// same updated_at basis (the blocked-transition timestamp, with a created_at
// fallback) as the bulk cancel selection, so doctor and `job cancel` agree on
// which jobs are "older than 30d".
func buildBlockedBacklogCheck(jobs []db.Job, now time.Time, threshold time.Duration) doctor.Check {
	cutoff := now.Add(-threshold).UnixMilli()
	count := 0
	for _, job := range jobs {
		if job.State != string(workflow.JobBlocked) {
			continue
		}
		age := blockedJobAgeMillis(job)
		if age > 0 && age <= cutoff {
			count++
		}
	}
	if count == 0 {
		return doctor.Check{Name: "blocked jobs", OK: true, Required: false, Detail: "no blocked jobs older than 30d"}
	}
	return doctor.Check{
		Name:     "blocked jobs",
		Required: false,
		Detail:   fmt.Sprintf("%d blocked jobs older than 30d — dismiss with: gitmoot job cancel --state blocked --older-than 30d --yes", count),
	}
}
