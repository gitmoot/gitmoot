package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// cancelWhileRunning enqueues an ask job for repo and cancels it from running,
// the state startup's cancelled recovery settles.
func cancelWhileRunning(t *testing.T, store *db.Store, id, repo string) {
	t.Helper()
	ctx := context.Background()
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, id, string(workflow.JobRunning)); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.CancelJob(ctx, store, id); err != nil {
		t.Fatal(err)
	}
}

// Startup's cancelled recovery reads the cancelled jobs once and decodes each
// once, however many repos are enabled; it used to read the whole jobs table
// and decode every cancelled job once per enabled repo (#2740). It still
// settles exactly the enabled repos' jobs, repo by repo in ListRepos order and
// by id within a repo, and skips a disabled repo's.
func TestRecoverCancelledRunningJobsForEnabledReposReadsCancelledJobsOnce(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	repos := []string{"owner/repo-a", "owner/repo-b", "owner/repo-c", "owner/repo-off"}
	for _, full := range repos {
		owner, name, _ := strings.Cut(full, "/")
		if err := store.UpsertRepo(ctx, db.Repo{Owner: owner, Name: name, CheckoutPath: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetRepoEnabled(ctx, full, full != "owner/repo-off"); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"2", "1"} {
			cancelWhileRunning(t, store, "job-"+strings.TrimPrefix(full, "owner/repo-")+"-"+n, full)
		}
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "queued-" + name, Agent: "audit", Action: "ask", Repo: full, Branch: "main", PullRequest: 2})
	}
	// The cancelled index yields updated_at order; job-a-1 updated last must
	// still settle before job-a-2, as the id-ordered whole-table read did.
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET updated_at = '2999-01-01 00:00:00' WHERE id = 'job-a-1'`); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	decodes := countDecodes(func() {
		if err := recoverCancelledRunningJobsForEnabledRepos(ctx, store, "", &out); err != nil {
			t.Fatal(err)
		}
	})
	if decodes != 8 {
		t.Fatalf("recovery decoded %d payloads, want 8: each of the 8 cancelled jobs once", decodes)
	}
	want := ""
	for _, id := range []string{"job-a-1", "job-a-2", "job-b-1", "job-b-2", "job-c-1", "job-c-2"} {
		want += "settled cancelled running job " + id + "\n"
	}
	if out.String() != want {
		t.Fatalf("recovery output:\n%s\nwant:\n%s", out.String(), want)
	}
	for _, id := range []string{"job-off-1", "job-off-2"} {
		events, err := store.ListJobEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if daemonWorkerHasEvent(events, "cancel_settled") {
			t.Fatalf("%s of the disabled repo was settled", id)
		}
	}
}

// The single-repo supervisor's recovery keeps the session filter: with a root
// filter only that session's cancelled jobs settle.
func TestRecoverCancelledRunningJobsForRepoKeepsTheSessionFilter(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for _, id := range []string{"job-1", "job-2"} {
		cancelWhileRunning(t, store, id, "owner/repo")
	}
	var out strings.Builder
	if err := recoverCancelledRunningJobsForRepo(ctx, store, &out, "owner/repo", "job-2"); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "settled cancelled running job job-2\n"; got != want {
		t.Fatalf("recovery output %q, want %q", got, want)
	}
}

// BenchmarkRecoverCancelledRunningJobsForEnabledRepos times one startup
// cancelled recovery over a home shaped like the one in #2740: 91 enabled
// repos and 30,000 jobs with about 12 KB of payload each, 1,554 of them
// cancelled. GITMOOT_BENCH_RECOVERY_JOBS overrides the job count.
func BenchmarkRecoverCancelledRunningJobsForEnabledRepos(b *testing.B) {
	const repoCount, cancelled = 91, 1554
	jobCount := 30000
	if v, err := strconv.Atoi(os.Getenv("GITMOOT_BENCH_RECOVERY_JOBS")); err == nil && v > 0 {
		jobCount = v
	}
	ctx := context.Background()
	paths := config.PathsForHome(b.TempDir())
	if err := config.Initialize(paths); err != nil {
		b.Fatal(err)
	}
	store, err := dbtest.Open(b, paths.Database)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	for r := range repoCount {
		if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: fmt.Sprintf("repo-%02d", r), CheckoutPath: b.TempDir()}); err != nil {
			b.Fatal(err)
		}
		if err := store.SetRepoEnabled(ctx, fmt.Sprintf("owner/repo-%02d", r), true); err != nil {
			b.Fatal(err)
		}
	}
	padding := strings.Repeat("x", 12<<10)
	for i := range jobCount {
		state := string(workflow.JobSucceeded)
		if i%(jobCount/cancelled) == 0 && i/(jobCount/cancelled) < cancelled {
			state = string(workflow.JobCancelled)
		}
		payload := fmt.Sprintf(`{"repo":"owner/repo-%02d","instructions":%q}`, i%repoCount, padding)
		if err := store.CreateJob(ctx, db.Job{ID: fmt.Sprintf("job-%06d", i), Agent: "worker", Type: "ask", State: state, Payload: payload}); err != nil {
			b.Fatal(err)
		}
	}
	for b.Loop() {
		if err := recoverCancelledRunningJobsForEnabledRepos(ctx, store, "", io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
