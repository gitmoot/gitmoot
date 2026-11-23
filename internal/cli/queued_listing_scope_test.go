package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// queuedListingFixture is a healthy-disk daemon home with `repos` repositories
// of `perRepo` queued ask jobs each: owner/repo-<r>, job-<r>-<j>. Its worker's
// job scope is verified, as after the daemon's startup repair, so passes list
// by column.
func queuedListingFixture(tb testing.TB, repos, perRepo int) (context.Context, jobWorker) {
	tb.Helper()
	ctx, store, _, worker := diskGuardDispatchFixture(tb)
	replaceDiskGuardMeasurement(tb, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 100 << 30, FreeBytes: 50 << 30}, nil
	})
	if moved, err := store.TransitionJobState(ctx, "disk-guard-job", string(workflow.JobQueued), string(workflow.JobCancelled)); err != nil || !moved {
		tb.Fatalf("drop fixture job: moved=%v err=%v", moved, err)
	}
	for r := range repos {
		for j := range perRepo {
			id := fmt.Sprintf("job-%d-%d", r, j)
			payload := fmt.Sprintf(`{"repo":"owner/repo-%d","instructions":"%s"}`, r, strings.Repeat("x", 512))
			if err := store.CreateJobWithEvent(ctx, db.Job{ID: id, Agent: "worker", Type: "ask", State: string(workflow.JobQueued), Payload: payload},
				db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
				tb.Fatalf("CreateJobWithEvent %s: %v", id, err)
			}
		}
	}
	worker.jobScope = &jobScopeVerifier{}
	worker.jobScope.verified.Store(true)
	return ctx, worker
}

// countDecodes runs fn and returns how many job payloads it decoded.
func countDecodes(fn func()) int {
	decodes := 0
	previous := daemonJobPayloadObserver
	daemonJobPayloadObserver = func() { decodes++ }
	defer func() { daemonJobPayloadObserver = previous }()
	fn()
	return decodes
}

// A dispatch pass for one repository decodes only that repository's queued
// payloads, once each (#2735): before, it decoded every queued job in the home
// once in the disk guard and once more in the eligibility loop.
func TestListPendingQueuedJobsDecodesOnlyItsScope(t *testing.T) {
	const repos, perRepo = 4, 5
	ctx, worker := queuedListingFixture(t, repos, perRepo)
	var pending []db.Job
	decodes := countDecodes(func() {
		var err error
		if pending, err = listPendingQueuedJobs(ctx, worker, "owner/repo-2", "", true); err != nil {
			t.Fatal(err)
		}
	})
	if len(pending) != perRepo {
		t.Fatalf("listed %d jobs for owner/repo-2, want %d", len(pending), perRepo)
	}
	for _, job := range pending {
		if !strings.HasPrefix(job.ID, "job-2-") {
			t.Fatalf("listed %s for owner/repo-2", job.ID)
		}
	}
	if decodes > perRepo {
		t.Fatalf("one owner/repo-2 pass decoded %d payloads with %d queued in %d repos; want at most its own %d", decodes, repos*perRepo, repos, perRepo)
	}
	idle := countDecodes(func() {
		if _, err := listPendingQueuedJobs(ctx, worker, "owner/idle", "", true); err != nil {
			t.Fatal(err)
		}
	})
	if idle != 0 {
		t.Fatalf("a pass for a repository with nothing queued decoded %d payloads, want 0", idle)
	}
}

// BenchmarkListPendingQueuedJobs measures one dispatch pass for one repository
// of a home with 10 repositories; decodes/op is the payload decodes per pass.
func BenchmarkListPendingQueuedJobs(b *testing.B) {
	for _, perRepo := range []int{2, 20} {
		b.Run(fmt.Sprintf("repos=10/queued=%d", 10*perRepo), func(b *testing.B) {
			ctx, worker := queuedListingFixture(b, 10, perRepo)
			b.ResetTimer()
			decodes := countDecodes(func() {
				for range b.N {
					if _, err := listPendingQueuedJobs(ctx, worker, "owner/repo-3", "", true); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.ReportMetric(float64(decodes)/float64(b.N), "decodes/op")
		})
	}
}

// The SQL scope plus queuedJobsInScope selects exactly the jobs the payload
// predicates (queuedJobMatchesRepo/Session) select from the whole queue, for
// every repo and session filter, including rows whose projected columns are
// empty, undecodable payloads, case and whitespace variants of a repo, and a
// job that is a session root while naming another root.
func TestQueuedJobsInScopeMatchesThePayloadPredicates(t *testing.T) {
	ctx, store, _, _ := diskGuardDispatchFixture(t)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	type row struct{ id, payload, wipe string }
	var rows []row
	n := 0
	for _, repo := range []string{"owner/a", "owner/b", "Owner/A", " owner/a", ""} {
		for _, root := range []string{"", "root-1", "root-2"} {
			for _, wipe := range []string{"", "repo", "root", "both"} {
				n++
				rows = append(rows, row{fmt.Sprintf("job-%02d", n), fmt.Sprintf(`{"repo":%q,"root_job_id":%q}`, repo, root), wipe})
			}
		}
	}
	rows = append(rows,
		row{"root-1", `{"repo":"owner/a"}`, ""},
		row{"root-2", `{"repo":"owner/a","root_job_id":"root-1"}`, ""},
		row{"root-3", `{"repo":"owner/b","root_job_id":"root-1"}`, "root"},
		row{"broken", `{`, ""},
		row{"broken-root", `{`, "both"},
	)
	for _, r := range rows {
		if err := store.CreateJobWithEvent(ctx, db.Job{ID: r.id, Agent: "worker", Type: "ask", State: string(workflow.JobQueued), Payload: r.payload},
			db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent %s: %v", r.id, err)
		}
		set := map[string]string{"repo": "repo = ''", "root": "root_id = ''", "both": "repo = '', root_id = ''"}[r.wipe]
		if set == "" {
			continue
		}
		if _, err := raw.ExecContext(ctx, `UPDATE jobs SET `+set+` WHERE id = ?`, r.id); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, repoFilter := range []string{"", "owner/a", " owner/a ", "Owner/A", "owner/b", "nope"} {
		for _, rootFilter := range []string{"", "root-1", "root-2", "root-3", "broken-root", "nope"} {
			var want []string
			for _, job := range all {
				if queuedJobMatchesRepo(job, repoFilter) && queuedJobMatchesSession(job, rootFilter) {
					want = append(want, job.ID)
				}
			}
			scoped, err := store.ListQueuedJobsInScope(ctx, repoFilter, rootFilter)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, job := range queuedJobsInScope(scoped, repoFilter, rootFilter) {
				got = append(got, job.ID)
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("repo %q root %q:\n got %v\nwant %v", repoFilter, rootFilter, got, want)
			}
		}
	}
}

// A per-repo pass reads only its repository's queue, so it must not forget a
// hold on another repository's review it cannot see (#2735).
func TestReviewHoldLogKeepsHoldsOutsideThePassScope(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("other-running", "other/repo", 12, overlapHeadA)
	h.startRunning("other-running")
	h.enqueue("other-held", "other/repo", 12, overlapHeadB)
	holds := newReviewHoldLog()
	worker := h.worker
	worker.reviewHolds = holds
	if _, err := listPendingQueuedJobs(h.ctx, worker, "other/repo", "", true); err != nil {
		t.Fatal(err)
	}
	if got := holds.held["other-held"].holder; got != "other-running" {
		t.Fatalf("hold log entry = %q, want other-running", got)
	}
	if _, err := listPendingQueuedJobs(h.ctx, worker, "owner/repo", "", true); err != nil {
		t.Fatal(err)
	}
	if got := holds.held["other-held"].holder; got != "other-running" {
		t.Fatalf("an owner/repo pass dropped other/repo's hold: entry = %q", got)
	}
}

// scopedIDs lists, through the scheduler's listing, the queued job ids of one
// repo and session pass.
func scopedIDs(t *testing.T, ctx context.Context, worker jobWorker, repoFilter, rootFilter string) string {
	t.Helper()
	jobs, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, false)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	return strings.Join(ids, ",")
}

// A queued child whose payload update moves it from session-a to session-b is
// listed by the session-b pass and no longer by session-a's: UpdateJobPayload
// re-projects root_id from the payload it writes, as every payload writer does
// (#2735 review, P2).
func TestUpdatedPayloadListsAQueuedJobInItsNewSession(t *testing.T) {
	ctx, worker := queuedListingFixture(t, 0, 0)
	store := worker.Store
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "child", Agent: "worker", Type: "ask", State: string(workflow.JobQueued),
		Payload: `{"repo":"owner/repo","root_job_id":"session-a"}`}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatal(err)
	}
	if got := scopedIDs(t, ctx, worker, "", "session-a"); got != "child" {
		t.Fatalf("session-a pass before the update = %q, want child", got)
	}
	if err := store.UpdateJobPayload(ctx, "child", `{"repo":"owner/repo","root_job_id":"session-b"}`); err != nil {
		t.Fatal(err)
	}
	if got := scopedIDs(t, ctx, worker, "", "session-b"); got != "child" {
		t.Fatalf("session-b pass after the update = %q, want child", got)
	}
	if got := scopedIDs(t, ctx, worker, "", "session-a"); got != "" {
		t.Fatalf("session-a pass after the update = %q, want none", got)
	}
}

// A legacy row whose repo column the #1066 SQL backfill projected with
// json_extract ("repo" only, case-sensitively) disagrees with the scheduler's
// decode, which also matches "Repo" and keeps the last key. The reviewer's
// probe (#2735 round 2): another connection holds the write lock while the
// daemon starts, so the repair fails and retries. Until it succeeds, the
// owner/b pass still lists the row, because an unverified worker reads the
// whole queue and scopes it by payload. Once the lock is released the retry
// re-projects the row, the scope is verified, and the column-scoped owner/b
// pass lists the row while the owner/a pass does not.
func TestJobScopeVerifierListsALegacyRowBeforeAndAfterTheRepair(t *testing.T) {
	ctx, store, _, worker := diskGuardDispatchFixture(t)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 100 << 30, FreeBytes: 50 << 30}, nil
	})
	backoff := jobScopeRepairBackoff
	jobScopeRepairBackoff = [2]time.Duration{10 * time.Millisecond, 50 * time.Millisecond}
	t.Cleanup(func() { jobScopeRepairBackoff = backoff })
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "dup", Agent: "worker", Type: "ask", State: string(workflow.JobQueued),
		Payload: `{"repo":"owner/a","Repo":"owner/b"}`}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// The row as it stands after a pre-projection insert and the #1066 backfill
	// (store_migrations.go), statement for statement.
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET repo = '' WHERE id = 'dup'`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET repo = json_extract(payload, '$.repo')
WHERE repo = '' AND json_valid(payload) AND COALESCE(json_extract(payload, '$.repo'), '') != ''`); err != nil {
		t.Fatal(err)
	}
	lock, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lock.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var out lockedBuffer
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	worker.jobScope = startJobScopeVerifier(runCtx, store, &out)
	if got := scopedIDs(t, ctx, worker, "owner/b", ""); got != "dup" {
		t.Fatalf("owner/b pass while the repair is pending = %q, want dup", got)
	}
	// The repair waits out the busy timeout, fails, and retries.
	waitFor(t, "the first failed repair attempt", 90*time.Second, func() bool {
		return strings.Contains(out.String(), "job scope repair attempt 1 failed")
	})
	if worker.jobScope.trusted() {
		t.Fatalf("job scope verified while the repair could not write; output %q", out.String())
	}
	if got := scopedIDs(t, ctx, worker, "owner/b", ""); got != "dup" {
		t.Fatalf("owner/b pass after a failed repair = %q, want dup", got)
	}
	if _, err := lock.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	locked = false

	waitFor(t, "the repair to succeed", 90*time.Second, worker.jobScope.trusted)
	if !strings.Contains(out.String(), "job scope verified: re-projected repo/session columns of 1 live job(s)") {
		t.Fatalf("verifier output %q does not report the one repaired job", out.String())
	}
	var repo string
	if err := raw.QueryRowContext(ctx, `SELECT repo FROM jobs WHERE id = 'dup'`).Scan(&repo); err != nil || repo != "owner/b" {
		t.Fatalf("repo column after the repair = %q (err %v), want owner/b", repo, err)
	}
	if got := scopedIDs(t, ctx, worker, "owner/b", ""); got != "dup" {
		t.Fatalf("owner/b pass after the repair = %q, want dup", got)
	}
	if got := scopedIDs(t, ctx, worker, "owner/a", ""); got != "" {
		t.Fatalf("owner/a pass after the repair = %q, want none", got)
	}
}

// The hold log stays bounded with no pass to prune it: a new hold past
// reviewHoldLogCap evicts the oldest entry (#2735 review, P3).
func TestReviewHoldLogEvictsTheOldestHoldAtItsCap(t *testing.T) {
	ctx, store, _, _ := diskGuardDispatchFixture(t)
	holds := newReviewHoldLog()
	for i := range reviewHoldLogCap {
		holds.seq++
		holds.held[fmt.Sprintf("held-%d", i)] = reviewHoldLogEntry{holder: "holder", seq: holds.seq}
	}
	if err := holds.record(ctx, store, "disk-guard-job", "holder", workflow.JobPayload{Repo: "owner/repo"}, "held"); err != nil {
		t.Fatal(err)
	}
	if len(holds.held) != reviewHoldLogCap {
		t.Fatalf("hold log size = %d, want the cap %d", len(holds.held), reviewHoldLogCap)
	}
	if _, kept := holds.held["held-0"]; kept {
		t.Fatal("the oldest hold was kept past the cap")
	}
	if holds.held["disk-guard-job"].holder != "holder" || holds.held["held-1"].holder != "holder" {
		t.Fatal("the new hold or the next-oldest one is missing")
	}
}
