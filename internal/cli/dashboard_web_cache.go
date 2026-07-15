package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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

// dashboardCachePolicy is the frozen endpoint policy for #948 and #956. Entries are
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
	{endpoint: "overview", keyKind: "full-cursor", retain: true, minRecompute: 5 * time.Second, maxAge: 15 * time.Second},
	{endpoint: "attention", keyKind: "full-cursor", retain: true, minRecompute: time.Second, maxAge: 5 * time.Second},
	{endpoint: "agents", keyKind: "job-event-id", retain: true, minRecompute: 5 * time.Second, maxAge: 30 * time.Second},
	{endpoint: "tasks", keyKind: "task-event-id", retain: true, minRecompute: 2 * time.Second, maxAge: 15 * time.Second},
	{endpoint: "workflows", keyKind: "job-event-id+workflow-note-id", retain: true, minRecompute: 5 * time.Second, maxAge: 15 * time.Second},
}

var (
	dashboardJobsCachePolicy      = dashboardCachePolicies[0]
	dashboardChartsCachePolicy    = dashboardCachePolicies[1]
	dashboardHealthCachePolicy    = dashboardCachePolicies[2]
	dashboardOverviewCachePolicy  = dashboardCachePolicies[3]
	dashboardAttentionCachePolicy = dashboardCachePolicies[4]
	dashboardAgentsCachePolicy    = dashboardCachePolicies[5]
	dashboardTasksCachePolicy     = dashboardCachePolicies[6]
	dashboardWorkflowsCachePolicy = dashboardCachePolicies[7]
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
		entries: make(map[string]dashboardCacheEntry, len(dashboardCachePolicies)),
		flights: make(map[string]*dashboardCacheFlight, len(dashboardCachePolicies)+1),
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

	// The shared compute serves every waiter, so it must not die with the
	// leader: detach from the leader's request context (a client disconnect
	// otherwise aborts all waiters) and convert a compute panic into an error
	// (an escaped panic would skip close(flight.done) and strand waiters
	// forever).
	body, err := func() (b []byte, e error) {
		defer func() {
			if r := recover(); r != nil {
				e = fmt.Errorf("dashboard cache compute panicked: %v", r)
			}
		}()
		return compute(context.WithoutCancel(ctx))
	}()
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
	for _, policy := range dashboardCachePolicies {
		s := c.stats[policy.endpoint]
		line += fmt.Sprintf(" %s hits=%d misses=%d shared=%d bytes=%d;", policy.endpoint, s.hits, s.misses, s.shared, s.bytes)
	}
	c.stats = make(map[string]dashboardCacheCounters, len(dashboardCachePolicies))
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

func (d *webDataSource) taskEventCursor(ctx context.Context) (string, error) {
	cursor, err := d.ChangeCursor(ctx)
	if err != nil {
		return "", err
	}
	_, remainder, _ := strings.Cut(cursor, ".")
	_, taskEventID, _ := strings.Cut(remainder, ".")
	return taskEventID, nil
}

func (d *webDataSource) workflowEventCursor(ctx context.Context) (string, error) {
	cursor, err := d.ChangeCursor(ctx)
	if err != nil {
		return "", err
	}
	jobEventID, remainder, _ := strings.Cut(cursor, ".")
	workflowNoteID, _, _ := strings.Cut(remainder, ".")
	return jobEventID + "." + workflowNoteID, nil
}

func (d *webDataSource) fullDashboardCursor(ctx context.Context) (string, error) {
	return d.ChangeCursor(ctx)
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

func (d *webDataSource) handleOverview(w http.ResponseWriter, r *http.Request) {
	cursor, err := d.fullDashboardCursor(r.Context())
	if err != nil {
		w.Header().Set(dashboardCacheHeader, "miss")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, outcome, err := d.cacheForDashboard().get(r.Context(), "overview", cursor, dashboardOverviewCachePolicy, d.overviewJSON)
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func (d *webDataSource) overviewJSON(ctx context.Context) ([]byte, error) {
	overview, err := d.Overview(ctx)
	if err != nil {
		return nil, err
	}
	if overview.NeedsYou == nil {
		overview.NeedsYou = []dashboard.OverviewNeedsYou{}
	}
	if overview.Activity.Workflows == nil {
		overview.Activity.Workflows = []dashboard.OverviewWorkflowActivity{}
	}
	for i := range overview.Activity.Workflows {
		overview.Activity.Workflows[i].Namespace, overview.Activity.Workflows[i].Campaign = splitDashboardWorkflowLabel(overview.Activity.Workflows[i].Label)
		if overview.Activity.Workflows[i].Agents == nil {
			overview.Activity.Workflows[i].Agents = []string{}
		}
		sort.Strings(overview.Activity.Workflows[i].Agents)
	}
	if overview.Today.Notable == nil {
		overview.Today.Notable = []dashboard.OverviewNotable{}
	}
	if overview.Scheduled == nil {
		overview.Scheduled = []dashboard.OverviewScheduled{}
	}
	if overview.Fleet == nil {
		overview.Fleet = []dashboard.OverviewFleet{}
	}
	sortDashboardOverview(&overview)
	if len(overview.Today.Notable) > 5 {
		overview.Today.Notable = overview.Today.Notable[:5]
	}
	return marshalDashboardJSON(overview)
}

func (d *webDataSource) handleAttention(w http.ResponseWriter, r *http.Request) {
	cursor, err := d.fullDashboardCursor(r.Context())
	if err != nil {
		w.Header().Set(dashboardCacheHeader, "miss")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, outcome, err := d.cacheForDashboard().get(r.Context(), "attention", cursor, dashboardAttentionCachePolicy, d.attentionJSON)
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func (d *webDataSource) attentionJSON(ctx context.Context) ([]byte, error) {
	attention, err := d.Attention(ctx)
	if err != nil {
		return nil, err
	}
	if attention.Gates == nil {
		attention.Gates = []dashboard.AttentionGate{}
	}
	if attention.SynthItems == nil {
		attention.SynthItems = []dashboard.AttentionSynthItem{}
	}
	if attention.Candidates == nil {
		attention.Candidates = []dashboard.AttentionCandidate{}
	}
	return marshalDashboardJSON(attention)
}

func (d *webDataSource) handleAgents(w http.ResponseWriter, r *http.Request) {
	cursor, err := d.jobEventCursor(r.Context())
	if err != nil {
		w.Header().Set(dashboardCacheHeader, "miss")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, outcome, err := d.cacheForDashboard().get(r.Context(), "agents", cursor, dashboardAgentsCachePolicy, func(ctx context.Context) ([]byte, error) {
		agents, err := d.Agents(ctx)
		if err != nil {
			return nil, err
		}
		if agents == nil {
			agents = []dashboard.AgentSummary{}
		}
		return marshalDashboardJSON(agents)
	})
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func (d *webDataSource) handleTasks(w http.ResponseWriter, r *http.Request) {
	cursor, err := d.taskEventCursor(r.Context())
	if err != nil {
		w.Header().Set(dashboardCacheHeader, "miss")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, outcome, err := d.cacheForDashboard().get(r.Context(), "tasks", cursor, dashboardTasksCachePolicy, d.tasksJSON)
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func (d *webDataSource) tasksJSON(ctx context.Context) ([]byte, error) {
	tasks, err := d.Tasks(ctx)
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		tasks = []dashboard.TaskSummary{}
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if ir, jr := dashboardWireTaskStateRank(tasks[i].State), dashboardWireTaskStateRank(tasks[j].State); ir != jr {
			return ir < jr
		}
		if tasks[i].UpdatedAt != tasks[j].UpdatedAt {
			return tasks[i].UpdatedAt > tasks[j].UpdatedAt
		}
		return tasks[i].ID < tasks[j].ID
	})
	return marshalDashboardJSON(tasks)
}

func dashboardWireTaskStateRank(state string) int {
	switch state {
	case "planned":
		return 0
	case "implementing":
		return 1
	case "pr_open":
		return 2
	case "blocked":
		return 3
	case "merged":
		return 4
	default:
		return 5
	}
}

func (d *webDataSource) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	cursor, err := d.workflowEventCursor(r.Context())
	if err != nil {
		w.Header().Set(dashboardCacheHeader, "miss")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, outcome, err := d.cacheForDashboard().get(r.Context(), "workflows", cursor, dashboardWorkflowsCachePolicy, d.workflowsJSON)
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func (d *webDataSource) workflowsJSON(ctx context.Context) ([]byte, error) {
	entries, err := d.Workflows(ctx)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []dashboard.WorkflowIndexEntry{}
	}
	for i := range entries {
		entries[i].Namespace, entries[i].Campaign = splitDashboardWorkflowLabel(entries[i].Label)
		if entries[i].Repos == nil {
			entries[i].Repos = []string{}
		}
	}
	// Sort with the pinned module's exact semantics (api.go workflowStateRank:
	// stalled(0) < active(1) < settled(2) < default(3) — 'recent' is a gitmoot
	// extension the module does not rank, so it must sort LAST for byte parity;
	// dashboardWorkflowIndexLess ranks it between active and settled and must
	// not be used here).
	moduleRank := func(state string) int {
		switch state {
		case "stalled":
			return 0
		case "active":
			return 1
		case "settled":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := moduleRank(entries[i].State), moduleRank(entries[j].State)
		if ri != rj {
			return ri < rj
		}
		if entries[i].State == "stalled" && entries[i].StalledForS != entries[j].StalledForS {
			return entries[i].StalledForS > entries[j].StalledForS
		}
		if entries[i].LastAt != entries[j].LastAt {
			return entries[i].LastAt > entries[j].LastAt
		}
		return entries[i].Label < entries[j].Label
	})
	return marshalDashboardJSON(entries)
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
