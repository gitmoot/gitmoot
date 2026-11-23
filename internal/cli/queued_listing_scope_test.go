package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// queuedListingFixture is a healthy-disk daemon home with `repos` repositories
// of `perRepo` queued ask jobs each: owner/repo-<r>, job-<r>-<j>.
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
