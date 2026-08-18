package cli

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/org"
)

func TestStoreOrgLiveSourcePersistedFreshness(t *testing.T) {
	t.Parallel()

	fresh := time.Now().UTC().Add(-time.Minute)
	tests := []struct {
		name       string
		state      org.LifecycleState
		observedAt time.Time
		episode    bool // seed an open org_blocked_episodes row for role:review
		want       org.LifecycleState
	}{
		// Blocked is authoritative from org_blocked_episodes (keeps the dot in
		// agreement with the blocked_since badge / Health.Blocked), NOT from a
		// live-presence "blocked" row.
		{name: "blocked from episode", episode: true, want: org.StateBlocked},
		{name: "episode wins over live working", state: org.StateWorking, observedAt: fresh, episode: true, want: org.StateBlocked},
		{name: "live blocked without episode is not blocked", state: org.StateBlocked, observedAt: fresh, want: org.StateUnknown},
		{name: "fresh working", state: org.StateWorking, observedAt: fresh, want: org.StateWorking},
		{name: "fresh idle", state: org.StateIdle, observedAt: fresh, want: org.StateIdle},
		{name: "stale", state: org.StateWorking, observedAt: fresh.Add(-storeOrgLivePresenceMaxAge), want: org.StateUnknown},
		{name: "done", state: org.StateDone, observedAt: fresh, want: org.StateUnknown},
		{name: "unknown", state: org.StateUnknown, observedAt: fresh, want: org.StateUnknown},
		{name: "absent", want: org.StateUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, paths := setupOrgHome(t)
			store, err := dbtest.Open(t, paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			if !test.observedAt.IsZero() {
				if err := store.UpsertRoleLivePresence(ctx, "review", string(test.state), test.observedAt); err != nil {
					t.Fatal(err)
				}
			}
			if test.episode {
				if err := store.UpsertBlockedEpisode(ctx, "role:review", fresh, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			shared, err := loadOrgSharedState(ctx, paths, store, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			states, observedAt, version, err := storeOrgLiveSource(&shared)(ctx, shared.Config)
			if err != nil {
				t.Fatal(err)
			}
			if got := states["review"].State; got != test.want {
				t.Fatalf("review state = %q, want %q", got, test.want)
			}
			if version != "store" {
				t.Fatalf("source metadata = observed %v version %q", observedAt, version)
			}
			if test.want == org.StateUnknown && !observedAt.IsZero() && test.state != org.StateDone && test.state != org.StateUnknown {
				t.Fatalf("unknown source observedAt = %v, want zero", observedAt)
			}
		})
	}
}

func TestLoadOrgSharedStateMissedWakeAgeOut(t *testing.T) {
	_, paths := setupOrgHome(t)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[orchestrate]\nmax_consecutive_missed_wakes = 1\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	for role, updatedAt := range map[string]time.Time{
		"review": now.Add(-missedWakeStaleAfter - time.Second),
		"owner":  now.Add(-time.Hour),
		"broken": now.Add(-time.Hour),
	} {
		if err := store.IncrementRoleMissedWake(ctx, role, updatedAt); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE org_role_missed_wakes SET updated_at = 'not-a-timestamp' WHERE role = 'broken'`); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	shared, err := loadOrgSharedState(ctx, paths, store, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := shared.MissedWakes["review"]; exists {
		t.Fatalf("stale review miss remained visible: %+v", shared.MissedWakes)
	}
	if shared.MissedWakes["owner"] != 1 {
		t.Fatalf("fresh owner miss = %d, want 1", shared.MissedWakes["owner"])
	}
	if shared.MissedWakes["broken"] != 1 {
		t.Fatalf("malformed timestamp hid miss: %+v", shared.MissedWakes)
	}
	if missedWakeIsStale(now.Add(-missedWakeStaleAfter).Format(db.BlockedEpisodeTimeLayout), now) {
		t.Fatal("miss exactly at stale boundary was hidden")
	}
}

func TestBuildOrgStatusRowsActiveJobsFailureDegradesOnce(t *testing.T) {
	_, paths := setupOrgHome(t)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`
[org.roles."recyclable"]
parent = "owner"
scope = ["gitmoot/*"]
recycle_after = "1ns"
`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.TouchOrgRolePresence(ctx, "recyclable", "test"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	shared, err := loadOrgSharedState(ctx, paths, store, time.Now().UTC())
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	source := func(context.Context, config.OrgConfig) (map[string]org.RoleLiveState, time.Time, string, error) {
		return map[string]org.RoleLiveState{
			"owner":      {State: org.StateIdle},
			"review":     {State: org.StateIdle},
			"recyclable": {State: org.StateIdle},
		}, time.Time{}, "fixture", nil
	}

	rows, err := buildOrgStatusRows(ctx, &shared, source, "status", true)
	if err != nil {
		t.Fatalf("buildOrgStatusRows() error = %v, want best-effort rows", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want owner, review, and recyclable", rows)
	}
	foundRecyclable := false
	for _, row := range rows {
		if row.ActiveJobs != 0 {
			t.Fatalf("row[%s].ActiveJobs = %d, want 0", row.Role, row.ActiveJobs)
		}
		if row.Role == "recyclable" {
			foundRecyclable = true
			recycleAfter := shared.Config.RecycleAfterFor(row.Role)
			if got := orgRecycleStatus(row.LastSeenAt, time.Now().UTC(), row.ProviderState, 0, recycleAfter); got != "eligible" {
				t.Fatalf("recyclable fixture status with known-zero jobs = %q, want eligible", got)
			}
			if row.RecycleStatus != "" || row.RecycleAfter != "" {
				t.Fatalf("recyclable row exposed recycle=%q after=%q with unknown job counts", row.RecycleStatus, row.RecycleAfter)
			}
		}
	}
	if !foundRecyclable {
		t.Fatal("recyclable role missing from rows")
	}
	if len(shared.Warnings) != 1 || !strings.Contains(shared.Warnings[0], "active-jobs counts unavailable") {
		t.Fatalf("warnings = %+v, want one active-jobs warning", shared.Warnings)
	}
	cachedErr := shared.jobCountsErr
	if cachedErr == nil {
		t.Fatal("job-count failure was not cached")
	}
	counts, err := shared.loadJobCounts(ctx)
	if err != cachedErr || counts != nil {
		t.Fatalf("cached loadJobCounts() = (%+v, %v), want (nil, %v)", counts, err, cachedErr)
	}

	withoutActiveJobs := shared
	withoutActiveJobs.jobCountsLoaded = false
	withoutActiveJobs.jobCountsErr = nil
	withoutActiveJobs.Warnings = nil
	rows, err = buildOrgStatusRows(ctx, &withoutActiveJobs, source, "status", false)
	if err != nil {
		t.Fatalf("buildOrgStatusRows(includeActiveJobs=false) error = %v", err)
	}
	if withoutActiveJobs.jobCountsLoaded {
		t.Fatal("includeActiveJobs=false queried active-job counts")
	}
	if len(withoutActiveJobs.Warnings) != 0 {
		t.Fatalf("includeActiveJobs=false warnings = %+v, want none", withoutActiveJobs.Warnings)
	}
	for _, row := range rows {
		if row.ActiveJobs != 0 {
			t.Fatalf("row[%s].ActiveJobs = %d with counts excluded, want 0", row.Role, row.ActiveJobs)
		}
	}
}
