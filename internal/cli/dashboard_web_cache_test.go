package cli

import (
	"bytes"
	"context"
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
	}
	if !reflect.DeepEqual(dashboardCachePolicies, want) {
		t.Fatalf("policies = %#v, want %#v", dashboardCachePolicies, want)
	}
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
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateJob(t, store, db.Job{ID: "malformed", Agent: "builder", Type: "ask", State: "failed", Payload: `{"instructions":`}, "failed", "bad payload")
	_ = store.Close()

	cached := newDashboardWebHandler(&webDataSource{home: home, responseCache: newDashboardJSONCache(nil)})
	uncached := dashboard.Serve(&webDataSource{home: home})
	for _, path := range []string{"/api/jobs", "/api/charts?days=30", "/api/charts?days=0"} {
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
