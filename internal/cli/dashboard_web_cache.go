package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	dashboard "github.com/jerryfane/gitmoot-dashboard"
)

const (
	dashboardCacheHeader         = "X-Gitmoot-Cache"
	dashboardCacheReportInterval = time.Minute
)

// dashboardCachePolicy is the frozen endpoint policy for #948. Entries are
// keyed by endpoint/canonical parameter; cursor is stored on the entry rather
// than included in the flight key, which prevents overlapping generations.
type dashboardCachePolicy struct {
	endpoint            string
	keyKind             string
	retain              bool
	minRecompute        time.Duration
	maxAge              time.Duration
	expireAtUTCMidnight bool
}

var dashboardCachePolicies = []dashboardCachePolicy{
	{endpoint: "jobs", keyKind: "job-event-id", retain: true, minRecompute: time.Second, maxAge: 15 * time.Second},
	{endpoint: "charts", keyKind: "canonical-days+job-event-id", retain: true, minRecompute: 12 * time.Second, maxAge: 60 * time.Second, expireAtUTCMidnight: true},
	{endpoint: "health", keyKind: "singleflight-only", retain: false},
}

var (
	dashboardJobsCachePolicy   = dashboardCachePolicies[0]
	dashboardChartsCachePolicy = dashboardCachePolicies[1]
	dashboardHealthCachePolicy = dashboardCachePolicies[2]
)

type dashboardCacheEntry struct {
	cursor     string
	body       []byte
	computedAt time.Time
}

type dashboardCacheFlight struct {
	done    chan struct{}
	body    []byte
	err     error
	waiters int
}

type dashboardCacheCounters struct {
	hits   uint64
	misses uint64
	shared uint64
	bytes  uint64
}

// dashboardJSONCache retains final serialized response bytes. Its mutex is
// intentionally independent of webDataSource.mu, which guards health's binary
// and update probe caches.
type dashboardJSONCache struct {
	mu      sync.Mutex
	entries map[string]dashboardCacheEntry
	flights map[string]*dashboardCacheFlight
	stats   map[string]dashboardCacheCounters

	now        func() time.Time
	stderr     io.Writer
	lastReport time.Time
}

func newDashboardJSONCache(stderr io.Writer) *dashboardJSONCache {
	if stderr == nil {
		stderr = io.Discard
	}
	now := time.Now
	return &dashboardJSONCache{
		entries:    make(map[string]dashboardCacheEntry, 5),
		flights:    make(map[string]*dashboardCacheFlight, 6),
		stats:      make(map[string]dashboardCacheCounters, 3),
		now:        now,
		stderr:     stderr,
		lastReport: now(),
	}
}

// get returns a retained hit, joins the stable-key in-flight computation, or
// becomes the miss leader. Failed computations are broadcast to waiters but are
// never installed as entries.
func (c *dashboardJSONCache) get(
	ctx context.Context,
	key, cursor string,
	policy dashboardCachePolicy,
	compute func(context.Context) ([]byte, error),
) ([]byte, string, error) {
	now := c.now()
	c.mu.Lock()
	if policy.retain {
		if entry, ok := c.entries[key]; ok && dashboardCacheEntryValid(entry, cursor, now, policy) {
			report := c.recordLocked(policy.endpoint, "hit", len(entry.body), now)
			c.mu.Unlock()
			c.writeReport(report)
			return entry.body, "hit", nil
		}
	}
	if flight := c.flights[key]; flight != nil {
		flight.waiters++
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			now = c.now()
			c.mu.Lock()
			report := c.recordLocked(policy.endpoint, "shared", 0, now)
			c.mu.Unlock()
			c.writeReport(report)
			return nil, "shared", ctx.Err()
		case <-flight.done:
			now = c.now()
			c.mu.Lock()
			report := c.recordLocked(policy.endpoint, "shared", len(flight.body), now)
			c.mu.Unlock()
			c.writeReport(report)
			return flight.body, "shared", flight.err
		}
	}

	flight := &dashboardCacheFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()

	body, err := compute(ctx)
	computedAt := c.now()
	c.mu.Lock()
	flight.body, flight.err = body, err
	if err == nil && policy.retain {
		c.entries[key] = dashboardCacheEntry{cursor: cursor, body: body, computedAt: computedAt}
	}
	delete(c.flights, key)
	close(flight.done)
	report := c.recordLocked(policy.endpoint, "miss", len(body), computedAt)
	c.mu.Unlock()
	c.writeReport(report)
	return body, "miss", err
}

func dashboardCacheEntryValid(entry dashboardCacheEntry, cursor string, now time.Time, policy dashboardCachePolicy) bool {
	age := now.Sub(entry.computedAt)
	if age < 0 || policy.maxAge <= 0 || age >= policy.maxAge {
		return false
	}
	if policy.expireAtUTCMidnight && !now.Before(nextUTCMidnight(entry.computedAt)) {
		return false
	}
	return cursor == entry.cursor || age < policy.minRecompute
}

func nextUTCMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

func (c *dashboardJSONCache) recordLocked(endpoint, outcome string, bodyBytes int, now time.Time) string {
	counters := c.stats[endpoint]
	switch outcome {
	case "hit":
		counters.hits++
	case "miss":
		counters.misses++
	case "shared":
		counters.shared++
	}
	if bodyBytes > 0 {
		counters.bytes += uint64(bodyBytes)
	}
	c.stats[endpoint] = counters
	if now.Sub(c.lastReport) < dashboardCacheReportInterval {
		return ""
	}
	line := "dashboard cache:"
	for _, name := range []string{"jobs", "charts", "health"} {
		s := c.stats[name]
		line += fmt.Sprintf(" %s hits=%d misses=%d shared=%d bytes=%d;", name, s.hits, s.misses, s.shared, s.bytes)
	}
	c.stats = make(map[string]dashboardCacheCounters, 3)
	c.lastReport = now
	return strings.TrimSuffix(line, ";") + "\n"
}

func (c *dashboardJSONCache) writeReport(report string) {
	if report != "" {
		_, _ = io.WriteString(c.stderr, report)
	}
}

func canonicalDashboardChartDays(raw string) int {
	switch raw {
	case "0":
		return 0
	case "7":
		return 7
	case "90":
		return 90
	default:
		return 30
	}
}

func marshalDashboardJSON(v any) ([]byte, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("internal error: %w", err)
	}
	return append(body, '\n'), nil
}

func (d *webDataSource) cacheForDashboard() *dashboardJSONCache {
	d.cacheOnce.Do(func() {
		if d.responseCache == nil {
			d.responseCache = newDashboardJSONCache(io.Discard)
		}
	})
	return d.responseCache
}

func (d *webDataSource) jobEventCursor(ctx context.Context) (string, error) {
	cursor, err := d.ChangeCursor(ctx)
	if err != nil {
		return "", err
	}
	jobEventID, _, _ := strings.Cut(cursor, ".")
	return jobEventID, nil
}

func (d *webDataSource) handleJobs(w http.ResponseWriter, r *http.Request) {
	cursor, err := d.jobEventCursor(r.Context())
	if err != nil {
		w.Header().Set(dashboardCacheHeader, "miss")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, outcome, err := d.cacheForDashboard().get(r.Context(), "jobs", cursor, dashboardJobsCachePolicy, func(ctx context.Context) ([]byte, error) {
		jobs, err := d.Jobs(ctx)
		if err != nil {
			return nil, err
		}
		if jobs == nil {
			jobs = []dashboard.JobSummary{}
		}
		return marshalDashboardJSON(jobs)
	})
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func (d *webDataSource) handleCharts(w http.ResponseWriter, r *http.Request) {
	days := canonicalDashboardChartDays(r.URL.Query().Get("days"))
	cursor, err := d.jobEventCursor(r.Context())
	if err != nil {
		w.Header().Set(dashboardCacheHeader, "miss")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	key := "charts:" + strconv.Itoa(days)
	body, outcome, err := d.cacheForDashboard().get(r.Context(), key, cursor, dashboardChartsCachePolicy, func(ctx context.Context) ([]byte, error) {
		charts, err := d.Charts(ctx, days)
		if err != nil {
			return nil, err
		}
		if charts.Days == nil {
			charts.Days = []dashboard.ChartDay{}
		}
		if charts.Agents == nil {
			charts.Agents = []dashboard.ChartAgent{}
		}
		if charts.Repos == nil {
			charts.Repos = []dashboard.ChartRepo{}
		}
		return marshalDashboardJSON(charts)
	})
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func (d *webDataSource) writeCachedDashboardJSON(w http.ResponseWriter, outcome string, body []byte, err error) {
	w.Header().Set(dashboardCacheHeader, outcome)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
