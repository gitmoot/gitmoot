package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestDoctorBlockedBacklogCheckClassifies pins the blocked-jobs doctor line
// (#631): a job set with no blocked jobs older than the threshold is an ok line;
// any older ones are a non-required warn carrying the exact bulk-dismiss command.
// Recent blocked jobs and old non-blocked jobs never count.
func TestDoctorBlockedBacklogCheckClassifies(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const layout = "2006-01-02 15:04:05"
	at := func(age time.Duration) string { return now.Add(-age).Format(layout) }

	t.Run("no stale blocked jobs is ok", func(t *testing.T) {
		jobs := []db.Job{
			{ID: "blocked-recent", State: string(workflow.JobBlocked), UpdatedAt: at(24 * time.Hour)},
			{ID: "failed-old", State: string(workflow.JobFailed), UpdatedAt: at(90 * 24 * time.Hour)},
		}
		check := buildBlockedBacklogCheck(jobs, now, blockedBacklogDoctorThreshold)
		if check.Name != "blocked jobs" || !check.OK || check.Required {
			t.Fatalf("check = %+v, want optional ok 'blocked jobs'", check)
		}
		if check.Detail != "no blocked jobs older than 30d" {
			t.Fatalf("ok detail = %q", check.Detail)
		}
	})

	t.Run("stale blocked jobs warn with the dismiss command", func(t *testing.T) {
		jobs := []db.Job{
			{ID: "blocked-old-a", State: string(workflow.JobBlocked), UpdatedAt: at(31 * 24 * time.Hour)},
			{ID: "blocked-old-b", State: string(workflow.JobBlocked), UpdatedAt: at(90 * 24 * time.Hour)},
			{ID: "blocked-recent", State: string(workflow.JobBlocked), UpdatedAt: at(24 * time.Hour)},
			{ID: "failed-old", State: string(workflow.JobFailed), UpdatedAt: at(90 * 24 * time.Hour)},
		}
		check := buildBlockedBacklogCheck(jobs, now, blockedBacklogDoctorThreshold)
		if check.OK || check.Required {
			t.Fatalf("check = %+v, want a non-required warn", check)
		}
		if !strings.Contains(check.Detail, "2 blocked jobs older than 30d") {
			t.Fatalf("warn detail = %q, want the stale count", check.Detail)
		}
		if !strings.Contains(check.Detail, "gitmoot job cancel --state blocked --older-than 30d --yes") {
			t.Fatalf("warn detail = %q, want the bulk-dismiss command", check.Detail)
		}
	})
}

// TestDoctorBlockedBacklogCheckReadsStore exercises the store-backed wrapper end
// to end: it seeds a home with a stale blocked job and a recent one, opens the
// store read-only, and asserts doctor warns about exactly the stale one. A missing
// home is fail-open (ok=false) so doctor never goes red on an uninitialized box.
func TestDoctorBlockedBacklogCheckReadsStore(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	for _, f := range []struct {
		id, state string
		age       time.Duration
	}{
		{"blocked-stale", string(workflow.JobBlocked), 45 * 24 * time.Hour},
		{"blocked-fresh", string(workflow.JobBlocked), 2 * 24 * time.Hour},
	} {
		seedCLIJob(t, store, db.Job{
			ID:      f.id,
			Agent:   "audit",
			Type:    "ask",
			State:   f.state,
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main"}),
		}, f.state)
	}
	store.Close()
	now := time.Now().UTC()
	const layout = "2006-01-02 15:04:05"
	setJobTimes(t, home, "blocked-stale", now.Add(-45*24*time.Hour).Format(layout), now.Add(-45*24*time.Hour).Format(layout))
	setJobTimes(t, home, "blocked-fresh", now.Add(-2*24*time.Hour).Format(layout), now.Add(-2*24*time.Hour).Format(layout))

	check, ok := blockedBacklogDoctorCheck(config.PathsForHome(home))
	if !ok {
		t.Fatal("blockedBacklogDoctorCheck ok=false for an initialized home, want a check")
	}
	if check.OK || check.Required {
		t.Fatalf("check = %+v, want a non-required warn for one stale blocked job", check)
	}
	if !strings.Contains(check.Detail, "1 blocked jobs older than 30d") {
		t.Fatalf("warn detail = %q, want exactly the stale count", check.Detail)
	}

	if _, ok := blockedBacklogDoctorCheck(config.PathsForHome(t.TempDir())); ok {
		t.Fatal("blockedBacklogDoctorCheck ok=true for an uninitialized home, want fail-open skip")
	}
}
