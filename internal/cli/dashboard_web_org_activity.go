package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	fleetActivityPollInterval = 3 * time.Second
	fleetActivityHeartbeat    = 20 * time.Second
	fleetActivityMaxViewers   = 64
)

type dashboardFleetActivity struct {
	ObservedAt string                        `json:"observed_at,omitempty"`
	Source     dashboardFleetActivitySource  `json:"source"`
	Summary    dashboardFleetActivitySummary `json:"summary"`
	Roles      []dashboardFleetActivityRole  `json:"roles"`
}

type dashboardFleetActivitySource struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type dashboardFleetActivitySummary struct {
	Roles           int `json:"roles"`
	Sessions        int `json:"sessions"`
	Working         int `json:"working"`
	Blocked         int `json:"blocked"`
	InputPending    int `json:"input_pending"`
	JobsRunning     int `json:"jobs_running"`
	EscalationsOpen int `json:"escalations_open"`
}

type dashboardFleetActivityRole struct {
	Name              string                        `json:"name"`
	DisplayName       string                        `json:"display_name"`
	Parent            string                        `json:"parent,omitempty"`
	Status            string                        `json:"status"`
	StatusDetail      string                        `json:"status_detail,omitempty"`
	TaskTitle         string                        `json:"task_title"`
	PaneID            string                        `json:"pane_id,omitempty"`
	Agent             string                        `json:"agent,omitempty"`
	CurrentTurn       *int64                        `json:"current_turn,omitempty"`
	LastCompletedTurn *int64                        `json:"last_completed_turn,omitempty"`
	LastCompletedAt   string                        `json:"last_completed_at,omitempty"`
	Scope             []string                      `json:"scope"`
	WakeRoutes        []dashboardFleetActivityRoute `json:"wake_routes"`
}

type dashboardFleetActivityRoute struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Scope   string `json:"scope"`
	Match   string `json:"match,omitempty"`
	Enabled bool   `json:"enabled"`
}

func (d *webDataSource) handleFleetActivity(w http.ResponseWriter, r *http.Request) {
	body := d.fleetActivityPayload(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (d *webDataSource) handleFleetActivityEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	events, unsubscribe, err := d.subscribeFleetActivity()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer unsubscribe()

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(fleetActivityHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case body, open := <-events:
			if !open {
				return
			}
			_, _ = fmt.Fprintf(w, "event: activity\ndata: %s\n\n", body)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (d *webDataSource) fleetActivityPayload(ctx context.Context) []byte {
	activity, err := d.fleetActivitySnapshot(ctx)
	if err != nil {
		activity = dashboardFleetActivity{
			Source: dashboardFleetActivitySource{
				State:  "down",
				Detail: "Fleet activity source unavailable: " + dashboardShortLine(err.Error(), 180),
			},
			Roles: []dashboardFleetActivityRole{},
		}
	}
	if activity.Roles == nil {
		activity.Roles = []dashboardFleetActivityRole{}
	}
	body, marshalErr := json.Marshal(activity)
	if marshalErr != nil {
		return []byte(`{"source":{"state":"down","detail":"Fleet activity response unavailable"},"summary":{},"roles":[]}`)
	}
	return body
}

func (d *webDataSource) fleetActivitySnapshot(ctx context.Context) (dashboardFleetActivity, error) {
	if d.fleetSnapshot != nil {
		return d.fleetSnapshot(ctx)
	}
	paths, err := pathsFromFlag(d.home)
	if err != nil {
		return dashboardFleetActivity{}, err
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		return dashboardFleetActivity{}, fmt.Errorf("load org registry: %w", err)
	}
	if !cfg.Enabled() {
		return dashboardFleetActivity{
			Source: dashboardFleetActivitySource{State: "up", Detail: "Organization registry is not configured"},
			Roles:  []dashboardFleetActivityRole{},
		}, nil
	}

	var live org.Snapshot
	var sourceErr error
	provider := newOrgProvider(cfg.Roles())
	if provider == nil {
		sourceErr = errors.New("Herdr organization provider is not configured")
	} else {
		live, sourceErr = provider.Snapshot(ctx)
	}
	now := time.Now().UTC()
	var out dashboardFleetActivity
	err = withReadOnlyStore(d.home, func(store *db.Store) error {
		var buildErr error
		out, buildErr = buildDashboardFleetActivity(ctx, cfg, live, sourceErr, store, now)
		return buildErr
	})
	return out, err
}

func buildDashboardFleetActivity(
	ctx context.Context,
	cfg config.OrgConfig,
	live org.Snapshot,
	sourceErr error,
	store *db.Store,
	now time.Time,
) (dashboardFleetActivity, error) {
	out := dashboardFleetActivity{
		Source: dashboardFleetActivitySource{State: "up"},
		Roles:  []dashboardFleetActivityRole{},
	}
	if sourceErr != nil {
		out.Source = dashboardFleetActivitySource{
			State:  "down",
			Detail: "Herdr session source unavailable: " + dashboardShortLine(sourceErr.Error(), 180),
		}
	} else {
		observedAt := live.ObservedAt
		if observedAt.IsZero() {
			observedAt = now
		}
		out.ObservedAt = observedAt.UTC().Format(time.RFC3339)
	}

	rules, err := store.ListEventRules(ctx)
	if err != nil {
		return dashboardFleetActivity{}, err
	}
	routesByRole := make(map[string][]dashboardFleetActivityRoute)
	for _, rule := range rules {
		role := strings.ToLower(strings.TrimSpace(rule.WakeRole))
		routesByRole[role] = append(routesByRole[role], dashboardFleetActivityRoute{
			ID: rule.ID, Kind: rule.OnKind, Scope: string(rule.Scope),
			Match: rule.MatchFilter, Enabled: rule.Enabled,
		})
	}

	activeJobs, err := store.ListDashboardActiveJobs(ctx)
	if err != nil {
		return dashboardFleetActivity{}, err
	}
	for _, job := range activeJobs {
		if job.State == "running" {
			out.Summary.JobsRunning++
		}
	}

	escalations, err := store.ListWorkflowNotesByBodyPrefix(ctx, workflow.OrgEscalatePrefix, dashboardOrgNoteLimit)
	if err != nil {
		return dashboardFleetActivity{}, err
	}
	resolved, _, err := dashboardResolvedEscalationIDs(ctx, store, escalations)
	if err != nil {
		return dashboardFleetActivity{}, err
	}
	out.Summary.EscalationsOpen = len(dashboardOrgEscalations(escalations, "", resolved))

	for _, role := range cfg.Roles() {
		name := strings.ToLower(strings.TrimSpace(role.Name))
		item := dashboardFleetActivityRole{
			Name: role.Name, DisplayName: dashboardOrgDisplayName(cfg, role.Name),
			Parent: role.Parent, TaskTitle: "No active agent session",
			Scope:      append([]string{}, role.Scope...),
			WakeRoutes: append([]dashboardFleetActivityRoute{}, routesByRole[name]...),
		}
		if item.Scope == nil {
			item.Scope = []string{}
		}
		if item.WakeRoutes == nil {
			item.WakeRoutes = []dashboardFleetActivityRoute{}
		}

		switch {
		case sourceErr != nil:
			item.Status = "source_down"
			item.StatusDetail = out.Source.Detail
			item.TaskTitle = "Session source unavailable"
		default:
			session, present := live.Sessions[role.Name]
			if !present {
				item.Status = "no_session"
				if binding := live.PaneBindings[role.Name]; binding.Detail != "" {
					item.StatusDetail = binding.Detail
				} else {
					item.StatusDetail = "No active agent session is attached to the configured pane"
				}
				break
			}
			item.Status = dashboardFleetSessionStatus(live.States[role.Name].State)
			item.StatusDetail = live.States[role.Name].Detail
			item.PaneID = session.PaneID
			item.Agent = session.Agent
			item.CurrentTurn = session.CurrentTurn
			item.TaskTitle = strings.TrimSpace(session.TaskTitle)
			if item.TaskTitle == "" {
				item.TaskTitle = "Task title unavailable"
			}
			if activity := live.States[role.Name].Activity; activity != nil && !activity.CompletedAt.IsZero() {
				completedTurn := activity.Turn
				item.LastCompletedTurn = &completedTurn
				item.LastCompletedAt = activity.CompletedAt.UTC().Format(time.RFC3339)
			}
			out.Summary.Sessions++
			switch item.Status {
			case "working":
				out.Summary.Working++
			case "blocked":
				out.Summary.Blocked++
			case "input_pending":
				out.Summary.InputPending++
			}
		}
		out.Roles = append(out.Roles, item)
	}
	sort.Slice(out.Roles, func(i, j int) bool {
		left := strings.Join(cfg.Path(out.Roles[i].Name), "/")
		right := strings.Join(cfg.Path(out.Roles[j].Name), "/")
		return left < right
	})
	out.Summary.Roles = len(out.Roles)
	return out, nil
}

func dashboardFleetSessionStatus(state org.LifecycleState) string {
	switch state {
	case org.StateIdle, org.StateWorking, org.StateBlocked, org.StateInputPending, org.StateDone:
		return string(state)
	default:
		return "unknown"
	}
}

type fleetActivityPoller struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.Mutex
	subs      map[int]chan []byte
	seq       int
	refs      int
	last      []byte
	haveState bool
}

func (d *webDataSource) subscribeFleetActivity() (<-chan []byte, func(), error) {
	d.fleetMu.Lock()
	poller := d.fleetPoller
	created := poller == nil
	var pollCtx context.Context
	if created {
		poller = &fleetActivityPoller{subs: make(map[int]chan []byte), done: make(chan struct{})}
		pollCtx, poller.cancel = context.WithCancel(context.Background())
		d.fleetPoller = poller
	}
	poller.mu.Lock()
	if len(poller.subs) >= fleetActivityMaxViewers {
		poller.mu.Unlock()
		d.fleetMu.Unlock()
		return nil, nil, errors.New("dashboard: too many fleet activity viewers")
	}
	id := poller.seq
	poller.seq++
	ch := make(chan []byte, 1)
	poller.subs[id] = ch
	poller.refs++
	poller.mu.Unlock()
	d.fleetMu.Unlock()

	if created {
		go d.runFleetActivityPoller(pollCtx, poller)
	}

	poller.mu.Lock()
	if poller.haveState && len(ch) == 0 {
		ch <- bytes.Clone(poller.last)
	}
	poller.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			d.fleetMu.Lock()
			poller.mu.Lock()
			if sub, present := poller.subs[id]; present {
				delete(poller.subs, id)
				close(sub)
				poller.refs--
				if poller.refs == 0 {
					poller.cancel()
					if d.fleetPoller == poller {
						d.fleetPoller = nil
					}
				}
			}
			poller.mu.Unlock()
			d.fleetMu.Unlock()
		})
	}
	return ch, unsubscribe, nil
}

func (d *webDataSource) runFleetActivityPoller(ctx context.Context, poller *fleetActivityPoller) {
	defer close(poller.done)
	interval := d.fleetInterval
	if interval <= 0 {
		interval = fleetActivityPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	publish := func() {
		body := d.fleetActivityPayload(ctx)
		poller.mu.Lock()
		changed := !poller.haveState || !bytes.Equal(body, poller.last)
		poller.last = bytes.Clone(body)
		poller.haveState = true
		if changed {
			for _, sub := range poller.subs {
				select {
				case <-sub:
				default:
				}
				select {
				case sub <- bytes.Clone(body):
				default:
				}
			}
		}
		poller.mu.Unlock()
	}

	publish()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

type dashboardBufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *dashboardBufferedResponse) Header() http.Header {
	return r.header
}

func (r *dashboardBufferedResponse) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *dashboardBufferedResponse) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(body)
}

func withFleetActivityAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !dashboardDocumentPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		buffered := &dashboardBufferedResponse{header: make(http.Header)}
		next.ServeHTTP(buffered, r)
		for key, values := range buffered.header {
			w.Header()[key] = append([]string(nil), values...)
		}
		status := buffered.status
		if status == 0 {
			status = http.StatusOK
		}
		body := buffered.body.Bytes()
		if status == http.StatusOK && strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			body = bytes.Replace(body, []byte("</head>"), []byte(`<link rel="stylesheet" href="/assets/gitmoot-fleet-activity.css">`+"\n</head>"), 1)
			body = bytes.Replace(body, []byte("</body>"), []byte(`<script defer src="/assets/gitmoot-fleet-activity.js"></script>`+"\n</body>"), 1)
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func dashboardDocumentPath(requestPath string) bool {
	if requestPath == "/" {
		return true
	}
	if requestPath == "/events" || strings.HasPrefix(requestPath, "/api/") || strings.HasPrefix(requestPath, "/receipts/") {
		return false
	}
	return path.Ext(requestPath) == ""
}
