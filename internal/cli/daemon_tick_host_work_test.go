package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// The worker tick runs every second over every enabled repo, and before #2530
// each repo's pass redid host-wide work: the blocked_ttl reaper listed every
// blocked job, the blocked-since guard listed every event rule (twice), and the
// advancement and comment retry passes read and JSON-decoded every host-wide
// candidate only to discard those owned by other repos. With 89 enabled repos
// that was most of the daemon's CPU.
//
// The cost is observed as allocations because it is nothing else a test can
// see: each repo added to the tick must cost about the same whether the host
// holds a handful of blocked jobs, rules and retry candidates or hundreds. The
// old per-repo rescans grow the per-repo cost with that host-wide population
// and fail by an order of magnitude, not by noise.
func TestWorkerTickPerRepoCostDoesNotScaleWithHostWideState(t *testing.T) {
	small := workerTickAllocsPerExtraRepo(t, 5)
	large := workerTickAllocsPerExtraRepo(t, 200)
	t.Logf("allocations per extra enabled repo: %.0f with 5 host-wide jobs, %.0f with 200", small, large)
	if large > 2*small+500 {
		t.Fatalf("each enabled repo costs %.0f allocations with 200 host-wide blocked jobs, rules and retry candidates, but %.0f with 5: the tick rescans host-wide state once per repo", large, small)
	}
}

// workerTickAllocsPerExtraRepo seeds population blocked jobs (each also an
// advancement retry candidate) and enabled event rules, all owned by a repo the
// daemon does not poll, then returns how many allocations each additional
// enabled repo adds to one worker tick.
func workerTickAllocsPerExtraRepo(t *testing.T, population int) float64 {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	// Both host-wide sweeps opted in, with windows nothing here reaches, so the
	// tick reads every blocked job and rule but changes nothing.
	if err := os.WriteFile(paths.ConfigFile, []byte("[orchestrate]\nblocked_ttl = \"1000h\"\nblocked_role_wake_after = \"1000h\"\n"), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for i := range population {
		id := fmt.Sprintf("job-elsewhere-%03d", i)
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/elsewhere", Branch: "main", PullRequest: i + 1})
		if err := store.UpdateJobState(ctx, id, string(workflow.JobBlocked)); err != nil {
			t.Fatalf("UpdateJobState returned error: %v", err)
		}
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: id, Kind: "advance_retry", Message: "seeded retry candidate"}); err != nil {
			t.Fatalf("AddJobEvent returned error: %v", err)
		}
		if err := store.AddEventRule(ctx, db.EventRule{ID: fmt.Sprintf("rule-%03d", i), OnKind: "blocked", WakeRole: "owner", Enabled: true}); err != nil {
			t.Fatalf("AddEventRule returned error: %v", err)
		}
	}

	worker := defaultJobWorker(store, io.Discard, home)
	tickAllocs := func() float64 {
		return testing.AllocsPerRun(3, func() {
			if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
				t.Fatalf("runEnabledRepoWorkerTicksTracked returned error: %v", err)
			}
		})
	}
	seedDaemonWorkerRepo(t, store, "owner/repo00", t.TempDir())
	one := tickAllocs()
	const extra = 8
	for i := 1; i <= extra; i++ {
		seedDaemonWorkerRepo(t, store, fmt.Sprintf("owner/repo%02d", i), t.TempDir())
	}
	many := tickAllocs()
	return (many - one) / extra
}
