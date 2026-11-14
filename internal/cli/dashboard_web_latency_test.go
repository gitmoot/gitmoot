package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type healthWorktreesBody struct {
	Worktrees struct {
		Stale int              `json:"stale"`
		Roots *json.RawMessage `json:"roots"`
	} `json:"worktrees"`
	WorktreesStatus     string `json:"worktreesStatus"`
	WorktreesComputedAt int64  `json:"worktreesComputedAt"`
	WorktreesError      string `json:"worktreesError"`
}

func getHealthWorktrees(t *testing.T, ds *webDataSource) healthWorktreesBody {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		ds.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		done <- recorder
	}()
	var recorder *httptest.ResponseRecorder
	select {
	case recorder = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("/api/health blocked on the worktree inspection")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body healthWorktreesBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// /api/health never waits for the worktree inspection (#2639): while the first
// background run is still walking, the response carries an explicit pending,
// non-null empty block; once a run completes it carries that value and when it
// was computed; a failed later run keeps the last value and reports the error.
func TestDashboardHealthServesBackgroundWorktreeUsageWithoutBlocking(t *testing.T) {
	home := dashboardTestHome(t)
	release := make(chan struct{})
	calls := make(chan struct{}, 4)
	fail := false
	ds := &webDataSource{home: home}
	ds.worktrees = newDashboardWorktreeRefresher(time.Hour, func(ctx context.Context) (delegationWorktreeUsage, error) {
		calls <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return delegationWorktreeUsage{}, ctx.Err()
		}
		if fail {
			return delegationWorktreeUsage{}, errors.New("walk failed")
		}
		return delegationWorktreeUsage{Stale: 3, Roots: []string{"/w"}}, nil
	})
	ds.worktrees.start()
	t.Cleanup(func() { ds.worktrees.stop(time.Second) })
	<-calls // the first inspection is running and blocked

	pending := getHealthWorktrees(t, ds)
	if pending.WorktreesStatus != dashboardWorktreesPending || pending.WorktreesComputedAt != 0 || pending.Worktrees.Stale != 0 {
		t.Fatalf("health before the first inspection = %+v, want pending/0/empty", pending)
	}
	if pending.Worktrees.Roots == nil || string(*pending.Worktrees.Roots) != "[]" {
		t.Fatalf("pending worktrees.roots = %v, want []", pending.Worktrees.Roots)
	}

	before := time.Now().UnixMilli()
	release <- struct{}{}
	deadline := time.Now().Add(10 * time.Second)
	for ds.worktrees.current().computedAt.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("first inspection never recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ready := getHealthWorktrees(t, ds)
	if ready.WorktreesStatus != dashboardWorktreesReady || ready.Worktrees.Stale != 3 || ready.WorktreesComputedAt < before || ready.WorktreesError != "" {
		t.Fatalf("health after the first inspection = %+v, want ready, stale 3, computedAt >= %d", ready, before)
	}

	// A failing refresh keeps serving the last good value.
	fail = true
	ds.worktrees.refreshForTest(t, release, calls)
	failed := getHealthWorktrees(t, ds)
	if failed.Worktrees.Stale != 3 || failed.WorktreesComputedAt != ready.WorktreesComputedAt || failed.WorktreesError != "walk failed" {
		t.Fatalf("health after a failed refresh = %+v, want the previous value plus the error", failed)
	}
}

// refreshForTest runs one more inspection synchronously through the same
// refresh path the loop uses, releasing the gated inspect function.
func (r *dashboardWorktreeRefresher) refreshForTest(t *testing.T, release chan struct{}, calls chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.refresh(context.Background())
	}()
	<-calls
	release <- struct{}{}
	<-done
}

// A data source whose refresher never started (tests, or a run that has not
// begun) still answers with the explicit pending block.
func TestDashboardHealthWithoutRefresherReportsPending(t *testing.T) {
	ds := &webDataSource{home: dashboardTestHome(t)}
	body := getHealthWorktrees(t, ds)
	if body.WorktreesStatus != dashboardWorktreesPending || body.WorktreesComputedAt != 0 {
		t.Fatalf("health without a refresher = %+v, want pending", body)
	}
}

// The galaxy shows only jobs created inside dashboardGalaxyWindow (#2639): an
// older job is absent from nodes, hubs, links and Repos, a newer one is present,
// and a job whose root fell out of the window self-roots instead of naming a
// missing node.
func TestWebDataSourceGraphWindowExcludesOlderJobs(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	jobs := []struct {
		job db.Job
		age time.Duration
	}{
		{db.Job{ID: "old-root", Agent: "veteran", Type: "ask", State: "succeeded",
			Payload: mustJSON(t, workflow.JobPayload{Repo: "acme/old", TaskTitle: "old work"})}, dashboardGalaxyWindow + 24*time.Hour},
		{db.Job{ID: "new-child", Agent: "builder", Type: "implement", State: "succeeded", ParentJobID: "old-root",
			Payload: mustJSON(t, workflow.JobPayload{Repo: "acme/new", RootJobID: "old-root", Instructions: "\n\n  Ship the window\nmore"})}, dashboardGalaxyWindow - 24*time.Hour},
	}
	for _, item := range jobs {
		if err := store.CreateJob(ctx, item.job); err != nil {
			t.Fatalf("CreateJob(%s): %v", item.job.ID, err)
		}
		stamp := dashboardSQLiteTime(time.Now().Add(-item.age))
		if err := store.ExecForTest(ctx, `UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, item.job.ID); err != nil {
			t.Fatalf("backdate %s: %v", item.job.ID, err)
		}
	}
	store.Close()

	g, err := (&webDataSource{home: home}).Graph(ctx, "")
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	ids := map[string]string{}
	for _, n := range g.Nodes {
		ids[n.ID] = n.Run
		if n.ID == "new-child" && n.Label != "Ship the window" {
			t.Fatalf("new-child label = %q, want the first instruction line", n.Label)
		}
	}
	for _, gone := range []string{"old-root", "repo::acme/old", "agent::veteran"} {
		if _, ok := ids[gone]; ok {
			t.Fatalf("node %s is older than the galaxy window but present", gone)
		}
	}
	if run, ok := ids["new-child"]; !ok || run != "new-child" {
		t.Fatalf("new-child run = %q (present=%v), want present and self-rooted", run, ok)
	}
	for _, l := range g.Links {
		if l.Source == "old-root" || l.Target == "old-root" {
			t.Fatalf("link %+v references a job outside the window", l)
		}
	}
	if len(g.Repos) != 1 || g.Repos[0] != "acme/new" {
		t.Fatalf("Repos = %v, want [acme/new]", g.Repos)
	}
}

// The gitmoot shadow of /api/graph returns the module's bytes and caches them
// on the job-event cursor; a repo filter naming no job's repo is answered but
// never retained, so arbitrary ?repo= values cannot grow the cache.
func TestDashboardGraphShadowMatchesModuleAndCaches(t *testing.T) {
	home := dashboardTestHome(t)
	seedWebDashboardTree(t, home)
	ds := &webDataSource{home: home}
	shadow := newDashboardWebHandler(ds)
	module := dashboard.Serve(&webDataSource{home: home})
	get := func(h http.Handler, target string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, recorder.Code, recorder.Body.String())
		}
		return recorder
	}
	for _, target := range []string{"/api/graph", "/api/graph?repo=jerryfane/noted"} {
		first := get(shadow, target)
		if outcome := first.Header().Get(dashboardCacheHeader); outcome != "miss" {
			t.Fatalf("first GET %s cache = %q, want miss", target, outcome)
		}
		if want := get(module, target).Body.String(); first.Body.String() != want {
			t.Fatalf("GET %s shadow body differs from the module's\ngot  %s\nwant %s", target, first.Body.String(), want)
		}
		if outcome := get(shadow, target).Header().Get(dashboardCacheHeader); outcome != "hit" {
			t.Fatalf("repeat GET %s cache = %q, want hit", target, outcome)
		}
	}
	for range 2 {
		if outcome := get(shadow, "/api/graph?repo=nobody/nothing").Header().Get(dashboardCacheHeader); outcome != "miss" {
			t.Fatalf("unknown repo filter cache = %q, want miss every time", outcome)
		}
	}
}
