package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/update"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestDashboardCachePolicyTable(t *testing.T) {
	want := []dashboardCachePolicy{
		{endpoint: "jobs", keyKind: "job-event-id", retain: true, minRecompute: time.Second, maxAge: 15 * time.Second},
		{endpoint: "charts", keyKind: "canonical-days+job-event-id", retain: true, minRecompute: 12 * time.Second, maxAge: 60 * time.Second, expireAtUTCMidnight: true},
		{endpoint: "health", keyKind: "singleflight-only", retain: false},
		{endpoint: "overview", keyKind: "full-cursor", retain: true, minRecompute: 5 * time.Second, maxAge: 15 * time.Second},
		{endpoint: "attention", keyKind: "full-cursor", retain: true, minRecompute: time.Second, maxAge: 5 * time.Second},
		{endpoint: "agents", keyKind: "job-event-id", retain: true, minRecompute: 5 * time.Second, maxAge: 30 * time.Second},
		{endpoint: "tasks", keyKind: "task-event-id", retain: true, minRecompute: 2 * time.Second, maxAge: 15 * time.Second},
		{endpoint: "workflows", keyKind: "job-event-id+workflow-note-id", retain: true, minRecompute: 5 * time.Second, maxAge: 15 * time.Second},
	}
	if !reflect.DeepEqual(dashboardCachePolicies, want) {
		t.Fatalf("policies = %#v, want %#v", dashboardCachePolicies, want)
	}
}

func TestDashboardCacheCursorSelection(t *testing.T) {
	home := dashboardTestHome(t)
	ds := &webDataSource{home: home}
	ctx := context.Background()
	assertCursors := func(job, task, workflow, full string) {
		t.Helper()
		checks := []struct {
			name string
			fn   func(context.Context) (string, error)
			want string
		}{
			{name: "job", fn: ds.jobEventCursor, want: job},
			{name: "task", fn: ds.taskEventCursor, want: task},
			{name: "workflow", fn: ds.workflowEventCursor, want: workflow},
			{name: "full", fn: ds.fullDashboardCursor, want: full},
		}
		for _, check := range checks {
			got, err := check.fn(ctx)
			if err != nil || got != check.want {
				t.Errorf("%s cursor = %q, %v, want %q", check.name, got, err, check.want)
			}
		}
	}
	assertCursors("0", "0", "0.0", "0.0.0")

	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "cursor-job", Agent: "worker", Type: "ask", State: "queued"}, db.JobEvent{Kind: "queued"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{WorkflowID: "cache/cursors", Body: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "cursor-task", State: "implementing"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{TaskID: "cursor-task", Kind: "started"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	assertCursors("1", "1", "1.1", "1.1.1")
}

func TestDashboardCacheCursorFloorAndHardMax(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	now := base
	cache := newDashboardJSONCache(nil)
	cache.now = func() time.Time { return now }
	cache.lastReport = now
	var computes int
	compute := func(context.Context) ([]byte, error) {
		computes++
		return []byte{byte('0' + computes)}, nil
	}

	body, outcome, err := cache.get(context.Background(), "jobs", "1", dashboardJobsCachePolicy, compute)
	if err != nil || outcome != "miss" || string(body) != "1" {
		t.Fatalf("cold = body %q outcome %q err %v", body, outcome, err)
	}
	if _, outcome, _ = cache.get(context.Background(), "jobs", "1", dashboardJobsCachePolicy, compute); outcome != "hit" {
		t.Fatalf("warm outcome = %q, want hit", outcome)
	}
	now = base.Add(500 * time.Millisecond)
	if body, outcome, _ = cache.get(context.Background(), "jobs", "2", dashboardJobsCachePolicy, compute); outcome != "hit" || string(body) != "1" {
		t.Fatalf("floor = body %q outcome %q, want retained hit", body, outcome)
	}
	now = base.Add(time.Second)
	if body, outcome, _ = cache.get(context.Background(), "jobs", "2", dashboardJobsCachePolicy, compute); outcome != "miss" || string(body) != "2" {
		t.Fatalf("cursor expiry = body %q outcome %q, want recomputed miss", body, outcome)
	}
	now = base.Add(16 * time.Second)
	if body, outcome, _ = cache.get(context.Background(), "jobs", "2", dashboardJobsCachePolicy, compute); outcome != "miss" || string(body) != "3" {
		t.Fatalf("hard max = body %q outcome %q, want recomputed miss", body, outcome)
	}
	if computes != 3 {
		t.Fatalf("computes = %d, want 3", computes)
	}
}

func TestDashboardChartsCacheFloorMaxAndUTCMidnight(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	now := base
	cache := newDashboardJSONCache(nil)
	cache.now = func() time.Time { return now }
	cache.lastReport = now
	var computes int
	compute := func(context.Context) ([]byte, error) {
		computes++
		return []byte{byte('0' + computes)}, nil
	}

	_, _, _ = cache.get(context.Background(), "charts:30", "1", dashboardChartsCachePolicy, compute)
	now = base.Add(11 * time.Second)
	if body, outcome, _ := cache.get(context.Background(), "charts:30", "2", dashboardChartsCachePolicy, compute); outcome != "hit" || string(body) != "1" {
		t.Fatalf("chart floor = body %q outcome %q", body, outcome)
	}
	now = base.Add(12 * time.Second)
	if body, outcome, _ := cache.get(context.Background(), "charts:30", "2", dashboardChartsCachePolicy, compute); outcome != "miss" || string(body) != "2" {
		t.Fatalf("chart cursor expiry = body %q outcome %q", body, outcome)
	}
	now = base.Add(72 * time.Second)
	if _, outcome, _ := cache.get(context.Background(), "charts:30", "2", dashboardChartsCachePolicy, compute); outcome != "miss" {
		t.Fatalf("chart max-age outcome = %q, want miss", outcome)
	}

	beforeMidnight := time.Date(2026, 7, 15, 23, 59, 59, 0, time.UTC)
	now = beforeMidnight
	_, _, _ = cache.get(context.Background(), "charts:7", "9", dashboardChartsCachePolicy, compute)
	now = time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	if _, outcome, _ := cache.get(context.Background(), "charts:7", "9", dashboardChartsCachePolicy, compute); outcome != "miss" {
		t.Fatalf("midnight outcome = %q, want miss", outcome)
	}
}

func TestDashboardChartsCanonicalKeys(t *testing.T) {
	for raw, want := range map[string]int{"": 30, "bogus": 30, "-1": 30, "30": 30, "0": 0, "7": 7, "90": 90} {
		if got := canonicalDashboardChartDays(raw); got != want {
			t.Errorf("canonicalDashboardChartDays(%q) = %d, want %d", raw, got, want)
		}
	}

	home := dashboardTestHome(t)
	ds := &webDataSource{home: home, responseCache: newDashboardJSONCache(nil)}
	handler := newDashboardWebHandler(ds)
	for _, path := range []string{
		"/api/charts", "/api/charts?days=invalid", "/api/charts?days=30",
		"/api/charts?days=0", "/api/charts?days=7", "/api/charts?days=90",
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d: %s", path, rr.Code, rr.Body.String())
		}
	}
	ds.responseCache.mu.Lock()
	keys := make([]string, 0, len(ds.responseCache.entries))
	for key := range ds.responseCache.entries {
		keys = append(keys, key)
	}
	ds.responseCache.mu.Unlock()
	sort.Strings(keys)
	wantKeys := []string{"charts:0", "charts:30", "charts:7", "charts:90"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("cache keys = %v, want %v", keys, wantKeys)
	}
}

func TestDashboardCacheErrorsAreNeverStored(t *testing.T) {
	cache := newDashboardJSONCache(nil)
	var computes int
	compute := func(context.Context) ([]byte, error) {
		computes++
		return nil, errors.New("boom")
	}
	for i := 0; i < 2; i++ {
		if _, outcome, err := cache.get(context.Background(), "jobs", "1", dashboardJobsCachePolicy, compute); outcome != "miss" || err == nil {
			t.Fatalf("attempt %d outcome=%q err=%v", i, outcome, err)
		}
	}
	if computes != 2 || len(cache.entries) != 0 {
		t.Fatalf("computes=%d entries=%d, want 2 and 0", computes, len(cache.entries))
	}
}

func TestDashboardCacheConcurrentMissesSingleflight(t *testing.T) {
	cache := newDashboardJSONCache(nil)
	const requests = 50
	var computes atomic.Int32
	release := make(chan struct{})
	compute := func(context.Context) ([]byte, error) {
		computes.Add(1)
		<-release
		return []byte("shared body"), nil
	}

	start := make(chan struct{})
	outcomes := make(chan string, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body, outcome, err := cache.get(context.Background(), "jobs", "1", dashboardJobsCachePolicy, compute)
			if err != nil || string(body) != "shared body" {
				t.Errorf("body=%q outcome=%q err=%v", body, outcome, err)
			}
			outcomes <- outcome
		}()
	}
	close(start)
	waitForDashboardCacheWaiters(t, cache, "jobs", requests-1)
	close(release)
	wg.Wait()
	close(outcomes)
	counts := map[string]int{}
	for outcome := range outcomes {
		counts[outcome]++
	}
	if computes.Load() != 1 || counts["miss"] != 1 || counts["shared"] != requests-1 {
		t.Fatalf("computes=%d outcomes=%v", computes.Load(), counts)
	}
}

func TestDashboardHealthSingleflightWithoutRetention(t *testing.T) {
	cache := newDashboardJSONCache(nil)
	var computes atomic.Int32
	release := make(chan struct{})
	compute := func(context.Context) ([]byte, error) {
		computes.Add(1)
		<-release
		return []byte("health"), nil
	}
	const requests = 12
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _, err := cache.get(context.Background(), "health", "", dashboardHealthCachePolicy, compute)
			if err != nil || string(body) != "health" {
				t.Errorf("body=%q err=%v", body, err)
			}
		}()
	}
	waitForDashboardCacheWaiters(t, cache, "health", requests-1)
	close(release)
	wg.Wait()

	if computes.Load() != 1 || len(cache.entries) != 0 {
		t.Fatalf("after burst computes=%d entries=%d", computes.Load(), len(cache.entries))
	}
	if _, outcome, err := cache.get(context.Background(), "health", "", dashboardHealthCachePolicy, func(context.Context) ([]byte, error) {
		computes.Add(1)
		return []byte("next"), nil
	}); err != nil || outcome != "miss" {
		t.Fatalf("next outcome=%q err=%v", outcome, err)
	}
	if computes.Load() != 2 || len(cache.entries) != 0 {
		t.Fatalf("after next computes=%d entries=%d", computes.Load(), len(cache.entries))
	}
}

func waitForDashboardCacheWaiters(t *testing.T, cache *dashboardJSONCache, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cache.mu.Lock()
		flight := cache.flights[key]
		got := 0
		if flight != nil {
			got = flight.waiters
		}
		cache.mu.Unlock()
		if got == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("flight %q did not reach %d waiters", key, want)
}

func TestDashboardCachedHandlersMatchModuleBytes(t *testing.T) {
	home := dashboardTestHome(t)
	seedWebDashboardTree(t, home)
	seedAttentionHome(t, home)
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateJob(t, store, db.Job{ID: "malformed", Agent: "builder", Type: "ask", State: "failed", Payload: `{"instructions":`}, "failed", "bad payload")
	if err := store.UpsertTask(context.Background(), db.Task{ID: "cache-task", RepoFullName: "acme/cache", Title: "Cache tier two", State: "implementing", Branch: "feat/cache"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{WorkflowID: "cache/tier-two", Author: "lead", Body: "byte parity"}); err != nil {
		t.Fatal(err)
	}
	// A second labeled workflow whose note is backdated below: it derives
	// 'settled' while cache/tier-two (fresh note) derives 'recent'. The pinned
	// module does not rank 'recent' (default bucket, AFTER settled) while
	// gitmoot's index comparator ranks it between active and settled — only a
	// mixed recent+settled list makes the byte compare guard module parity.
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{WorkflowID: "cache/settled-old", Author: "lead", Body: "aged out"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Zero wall-clock-derived ages so the module and shadow calls cannot differ
	// when the test happens to cross a whole-second boundary.
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE jobs SET created_at = '', updated_at = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE tasks SET updated_at = ''`); err != nil {
		t.Fatal(err)
	}
	// Backdate ONLY the settled-old note past the active window so exactly one
	// workflow derives 'settled' alongside tier-two's 'recent' (neither state
	// carries server-side age fields, so wall-clock boundaries cannot
	// desynchronize the byte compare).
	if _, err := raw.Exec(`UPDATE workflow_notes SET created_at = '2026-06-01 00:00:00' WHERE workflow_id = 'cache/settled-old'`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	cached := newDashboardWebHandler(&webDataSource{home: home, responseCache: newDashboardJSONCache(nil)})
	uncached := dashboard.Serve(&webDataSource{home: home})
	for _, path := range []string{
		"/api/jobs", "/api/charts?days=30", "/api/charts?days=0",
		"/api/overview", "/api/attention", "/api/agents", "/api/tasks", "/api/workflows",
	} {
		want := httptest.NewRecorder()
		uncached.ServeHTTP(want, httptest.NewRequest(http.MethodGet, path, nil))
		for _, wantOutcome := range []string{"miss", "hit"} {
			got := httptest.NewRecorder()
			cached.ServeHTTP(got, httptest.NewRequest(http.MethodGet, path, nil))
			if got.Code != want.Code || got.Header().Get("Content-Type") != want.Header().Get("Content-Type") || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
				t.Fatalf("%s %s differs: status %d/%d content-type %q/%q\nwant:\n%s\ngot:\n%s", path, wantOutcome,
					got.Code, want.Code, got.Header().Get("Content-Type"), want.Header().Get("Content-Type"), want.Body.Bytes(), got.Body.Bytes())
			}
			if outcome := got.Header().Get(dashboardCacheHeader); outcome != wantOutcome {
				t.Fatalf("%s cache outcome = %q, want %q", path, outcome, wantOutcome)
			}
		}
	}

	legacy, err := legacyDashboardJobs(home)
	if err != nil {
		t.Fatal(err)
	}
	wantLegacy, err := marshalDashboardJSON(legacy)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	uncached.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	if !bytes.Equal(rr.Body.Bytes(), wantLegacy) {
		t.Fatalf("projected jobs differ from the pre-change full-unmarshal path\nwant:\n%s\ngot:\n%s", wantLegacy, rr.Body.Bytes())
	}
}

func TestDashboardCachedTierTwoHandlersMatchModuleBytesEmpty(t *testing.T) {
	home := dashboardTestHome(t)
	cached := newDashboardWebHandler(&webDataSource{home: home, responseCache: newDashboardJSONCache(nil)})
	module := dashboard.Serve(&webDataSource{home: home})
	for _, path := range []string{"/api/overview", "/api/attention", "/api/agents", "/api/tasks", "/api/workflows"} {
		t.Run(strings.TrimPrefix(path, "/api/"), func(t *testing.T) {
			want := httptest.NewRecorder()
			module.ServeHTTP(want, httptest.NewRequest(http.MethodGet, path, nil))
			for _, wantOutcome := range []string{"miss", "hit"} {
				got := httptest.NewRecorder()
				cached.ServeHTTP(got, httptest.NewRequest(http.MethodGet, path+"?ignored=value", nil))
				if got.Code != want.Code || got.Header().Get("Content-Type") != want.Header().Get("Content-Type") || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
					t.Fatalf("%s %s differs: status %d/%d content-type %q/%q\nwant:\n%s\ngot:\n%s", path, wantOutcome,
						got.Code, want.Code, got.Header().Get("Content-Type"), want.Header().Get("Content-Type"), want.Body.Bytes(), got.Body.Bytes())
				}
				if outcome := got.Header().Get(dashboardCacheHeader); outcome != wantOutcome {
					t.Fatalf("cache outcome = %q, want %q", outcome, wantOutcome)
				}
			}
		})
	}
}

func TestDashboardAttentionCacheGateSatisfactionUsesHardMax(t *testing.T) {
	home := dashboardTestHome(t)
	ctx := context.Background()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJobWithEvent(ctx,
		db.Job{ID: "blocked", Agent: "worker", Type: "ask", State: "blocked"},
		db.JobEvent{Kind: "blocked"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordJobGates(ctx, "blocked", []string{"human:approve"}); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	now := base
	cache := newDashboardJSONCache(nil)
	cache.now = func() time.Time { return now }
	ds := &webDataSource{home: home, responseCache: cache}
	handler := newDashboardWebHandler(ds)
	read := func(wantOutcome string) dashboard.Attention {
		t.Helper()
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/attention", nil))
		if rr.Code != http.StatusOK || rr.Header().Get(dashboardCacheHeader) != wantOutcome {
			t.Fatalf("attention status=%d outcome=%q body=%s", rr.Code, rr.Header().Get(dashboardCacheHeader), rr.Body.String())
		}
		var attention dashboard.Attention
		if err := json.Unmarshal(rr.Body.Bytes(), &attention); err != nil {
			t.Fatal(err)
		}
		return attention
	}
	if got := read("miss"); len(got.Gates) != 1 {
		t.Fatalf("initial gates = %+v", got.Gates)
	}
	before, err := ds.fullDashboardCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SatisfyJobGate(ctx, "blocked", "human:approve"); err != nil || !ok {
		t.Fatalf("SatisfyJobGate = %v, %v", ok, err)
	}
	after, err := ds.fullDashboardCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("gate satisfaction moved cursor from %q to %q", before, after)
	}
	if got := read("hit"); len(got.Gates) != 1 {
		t.Fatalf("pre-expiry gates = %+v, want retained gate", got.Gates)
	}
	now = base.Add(dashboardAttentionCachePolicy.maxAge)
	if got := read("miss"); len(got.Gates) != 0 || got.Total != 0 {
		t.Fatalf("post-expiry attention = %+v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardAgentsCacheRegistryMaxAndJobCursor(t *testing.T) {
	home := dashboardTestHome(t)
	ctx := context.Background()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "worker", Runtime: "codex"}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	now := base
	cache := newDashboardJSONCache(nil)
	cache.now = func() time.Time { return now }
	handler := newDashboardWebHandler(&webDataSource{home: home, responseCache: cache})
	read := func(wantOutcome string) dashboard.AgentSummary {
		t.Helper()
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
		if rr.Code != http.StatusOK || rr.Header().Get(dashboardCacheHeader) != wantOutcome {
			t.Fatalf("agents status=%d outcome=%q body=%s", rr.Code, rr.Header().Get(dashboardCacheHeader), rr.Body.String())
		}
		var agents []dashboard.AgentSummary
		if err := json.Unmarshal(rr.Body.Bytes(), &agents); err != nil {
			t.Fatal(err)
		}
		if len(agents) != 1 {
			t.Fatalf("agents = %+v", agents)
		}
		return agents[0]
	}
	if got := read("miss"); got.Runtime != "codex" {
		t.Fatalf("initial agent = %+v", got)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "worker", Runtime: "claude"}); err != nil {
		t.Fatal(err)
	}
	now = base.Add(dashboardAgentsCachePolicy.maxAge - time.Nanosecond)
	if got := read("hit"); got.Runtime != "codex" {
		t.Fatalf("pre-expiry agent = %+v", got)
	}
	now = base.Add(dashboardAgentsCachePolicy.maxAge)
	if got := read("miss"); got.Runtime != "claude" {
		t.Fatalf("post-expiry agent = %+v", got)
	}
	if err := store.CreateJobWithEvent(ctx,
		db.Job{ID: "agent-job", Agent: "worker", Type: "ask", State: "running"},
		db.JobEvent{Kind: "running"}); err != nil {
		t.Fatal(err)
	}
	now = base.Add(dashboardAgentsCachePolicy.maxAge + dashboardAgentsCachePolicy.minRecompute - time.Nanosecond)
	if got := read("hit"); got.JobCount != 0 {
		t.Fatalf("cursor floor agent = %+v", got)
	}
	now = base.Add(dashboardAgentsCachePolicy.maxAge + dashboardAgentsCachePolicy.minRecompute)
	if got := read("miss"); got.JobCount != 1 || got.RunningCount != 1 {
		t.Fatalf("cursor refresh agent = %+v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardTierTwoCachesUncoveredWritesUseHardMax(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		path   string
		maxAge time.Duration
		seed   func(*testing.T, *db.Store)
		mutate func(*testing.T, *db.Store)
		value  func(*testing.T, []byte) string
		before string
		after  string
	}{
		{
			name: "overview pipeline schedule", path: "/api/overview", maxAge: dashboardOverviewCachePolicy.maxAge,
			seed: func(t *testing.T, store *db.Store) {
				seedTestPipeline(t, store, db.Pipeline{Name: "nightly", Repo: "acme/app", SpecYAML: diamondSpecYAML, Enabled: true, Interval: "1h"})
				if err := store.UpdatePipelineScheduleState(context.Background(), db.PipelineScheduleState{Name: "nightly", NextDueAt: time.Now().Add(time.Hour), LastStatus: "queued"}); err != nil {
					t.Fatal(err)
				}
			},
			mutate: func(t *testing.T, store *db.Store) {
				if err := store.UpdatePipelineScheduleState(context.Background(), db.PipelineScheduleState{Name: "nightly", NextDueAt: time.Now().Add(time.Hour), LastStatus: "succeeded"}); err != nil {
					t.Fatal(err)
				}
			},
			value: func(t *testing.T, body []byte) string {
				var overview dashboard.Overview
				if err := json.Unmarshal(body, &overview); err != nil {
					t.Fatal(err)
				}
				if len(overview.Scheduled) != 1 {
					t.Fatalf("scheduled = %+v", overview.Scheduled)
				}
				return overview.Scheduled[0].LastStatus
			},
			before: "queued", after: "succeeded",
		},
		{
			name: "task upsert", path: "/api/tasks", maxAge: dashboardTasksCachePolicy.maxAge,
			seed: func(t *testing.T, store *db.Store) {
				if err := store.UpsertTask(context.Background(), db.Task{ID: "task", RepoFullName: "acme/app", Title: "before", State: "implementing"}); err != nil {
					t.Fatal(err)
				}
			},
			mutate: func(t *testing.T, store *db.Store) {
				if err := store.UpsertTask(context.Background(), db.Task{ID: "task", RepoFullName: "acme/app", Title: "after", State: "implementing"}); err != nil {
					t.Fatal(err)
				}
			},
			value: func(t *testing.T, body []byte) string {
				var tasks []dashboard.TaskSummary
				if err := json.Unmarshal(body, &tasks); err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 1 {
					t.Fatalf("tasks = %+v", tasks)
				}
				return tasks[0].Title
			},
			before: "before", after: "after",
		},
		{
			name: "workflow direct job transition", path: "/api/workflows", maxAge: dashboardWorkflowsCachePolicy.maxAge,
			seed: func(t *testing.T, store *db.Store) {
				payload := mustJSON(t, workflow.JobPayload{WorkflowID: "cache/workflow", Repo: "acme/app"})
				if err := store.CreateJob(context.Background(), db.Job{ID: "workflow-job", Agent: "worker", Type: "ask", State: "queued", Payload: payload}); err != nil {
					t.Fatal(err)
				}
			},
			mutate: func(t *testing.T, store *db.Store) {
				if ok, err := store.TransitionJobState(context.Background(), "workflow-job", "queued", "succeeded"); err != nil || !ok {
					t.Fatalf("TransitionJobState = %v, %v", ok, err)
				}
			},
			value: func(t *testing.T, body []byte) string {
				var entries []dashboard.WorkflowIndexEntry
				if err := json.Unmarshal(body, &entries); err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 {
					t.Fatalf("workflows = %+v", entries)
				}
				if entries[0].Counts.Queued == 1 {
					return "queued"
				}
				if entries[0].Counts.Succeeded == 1 {
					return "succeeded"
				}
				return "unexpected"
			},
			before: "queued", after: "succeeded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := dashboardTestHome(t)
			store, err := db.Open(config.PathsForHome(home).Database)
			if err != nil {
				t.Fatal(err)
			}
			tc.seed(t, store)
			now := base
			cache := newDashboardJSONCache(nil)
			cache.now = func() time.Time { return now }
			handler := newDashboardWebHandler(&webDataSource{home: home, responseCache: cache})
			read := func(wantOutcome string) string {
				t.Helper()
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
				if rr.Code != http.StatusOK || rr.Header().Get(dashboardCacheHeader) != wantOutcome {
					t.Fatalf("status=%d outcome=%q body=%s", rr.Code, rr.Header().Get(dashboardCacheHeader), rr.Body.String())
				}
				return tc.value(t, rr.Body.Bytes())
			}
			if got := read("miss"); got != tc.before {
				t.Fatalf("initial value = %q, want %q", got, tc.before)
			}
			tc.mutate(t, store)
			if got := read("hit"); got != tc.before {
				t.Fatalf("retained value = %q, want %q", got, tc.before)
			}
			now = base.Add(tc.maxAge)
			if got := read("miss"); got != tc.after {
				t.Fatalf("post-expiry value = %q, want %q", got, tc.after)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDashboardCacheMetricsReportUsesPolicyTable(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cache := newDashboardJSONCache(nil)
	cache.lastReport = base
	cache.mu.Lock()
	report := cache.recordLocked("overview", "hit", 42, base.Add(dashboardCacheReportInterval))
	cache.mu.Unlock()
	want := "dashboard cache: jobs hits=0 misses=0 shared=0 bytes=0; charts hits=0 misses=0 shared=0 bytes=0; health hits=0 misses=0 shared=0 bytes=0; overview hits=1 misses=0 shared=0 bytes=42; attention hits=0 misses=0 shared=0 bytes=0; agents hits=0 misses=0 shared=0 bytes=0; tasks hits=0 misses=0 shared=0 bytes=0; workflows hits=0 misses=0 shared=0 bytes=0\n"
	if report != want {
		t.Fatalf("metrics report:\n%s\nwant:\n%s", report, want)
	}
}

func legacyDashboardJobs(home string) ([]dashboard.JobSummary, error) {
	out := []dashboard.JobSummary{}
	err := withStore(home, func(store *db.Store) error {
		jobs, err := store.ListJobs(context.Background())
		if err != nil {
			return err
		}
		jobByID := make(map[string]db.Job, len(jobs))
		for _, job := range jobs {
			jobByID[job.ID] = job
		}
		runtimes := agentRuntimeMap(context.Background(), store)
		for _, job := range jobs {
			payload, _ := workflow.ParseJobPayload(job.Payload)
			kind, _ := parseRunKindAgent(job.ID, job)
			started := parseJobTimeMillis(job.CreatedAt)
			updated := parseJobTimeMillis(job.UpdatedAt)
			var duration int64
			if started > 0 && updated > started {
				duration = updated - started
			}
			out = append(out, dashboard.JobSummary{
				ID: job.ID, Title: jobTitle(payload, job), Agent: strings.TrimSpace(job.Agent),
				Runtime: resolveJobRuntime(job, payload, runtimes), Repo: strings.TrimSpace(payload.Repo),
				Kind: kind, State: mapNodeState(job.State), Depth: job.DelegationDepth,
				Run: jobRootID(jobByID, job.ID), PR: payload.PullRequest,
				Started: started, Updated: updated, Duration: duration,
				TokensIn: job.InputTokens, TokensOut: job.OutputTokens,
			})
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Updated != out[j].Updated {
				return out[i].Updated > out[j].Updated
			}
			return out[i].ID < out[j].ID
		})
		return nil
	})
	return out, err
}

func TestDashboardHealthSeesLockMutationOnNextResponse(t *testing.T) {
	original := updateCheckFn
	t.Cleanup(func() { updateCheckFn = original })
	updateCheckFn = func(context.Context, buildinfo.Info, string) (update.CheckResult, error) {
		return update.CheckResult{}, errors.New("offline")
	}

	home := dashboardTestHome(t)
	ds := &webDataSource{home: home, responseCache: newDashboardJSONCache(nil)}
	handler := newDashboardWebHandler(ds)
	readLocks := func() []dashboard.HealthLock {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("health status = %d: %s", rr.Code, rr.Body.String())
		}
		if outcome := rr.Header().Get(dashboardCacheHeader); outcome != "miss" {
			t.Fatalf("health cache outcome = %q, want miss", outcome)
		}
		var response dashboardHealthResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Locks
	}
	if locks := readLocks(); len(locks) != 0 {
		t.Fatalf("initial locks = %+v", locks)
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.CreateLock(context.Background(), db.BranchLock{RepoFullName: "acme/repo", Branch: "feat/cache", Owner: "worker"}); err != nil || !created {
		t.Fatalf("CreateLock created=%v err=%v", created, err)
	}
	_ = store.Close()
	locks := readLocks()
	if len(locks) != 1 || locks[0].Repo != "acme/repo" || locks[0].Branch != "feat/cache" {
		t.Fatalf("next locks = %+v", locks)
	}
}

func TestDashboardCacheComputePanicReleasesWaiters(t *testing.T) {
	cache := newDashboardJSONCache(nil)
	policy := dashboardCachePolicy{endpoint: "jobs", retain: true, maxAge: time.Minute}
	_, _, err := cache.get(context.Background(), "k", "c1", policy, func(context.Context) ([]byte, error) {
		panic("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic not converted to error: %v", err)
	}
	// The flight must be released: a follow-up compute succeeds normally.
	body, state, err := cache.get(context.Background(), "k", "c1", policy, func(context.Context) ([]byte, error) {
		return []byte("ok"), nil
	})
	if err != nil || string(body) != "ok" || state != "miss" {
		t.Fatalf("post-panic serve = %q, %q, %v", body, state, err)
	}
}

func TestDashboardCacheLeaderDisconnectDoesNotAbortCompute(t *testing.T) {
	cache := newDashboardJSONCache(nil)
	policy := dashboardCachePolicy{endpoint: "jobs", retain: true, maxAge: time.Minute}
	leaderCtx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() { <-started; cancel() }()
	body, _, err := cache.get(leaderCtx, "k2", "c1", policy, func(ctx context.Context) ([]byte, error) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []byte("survived"), nil
	})
	if err != nil || string(body) != "survived" {
		t.Fatalf("leader cancellation aborted the shared compute: %q, %v", body, err)
	}
}
