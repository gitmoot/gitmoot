package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/buildinfo"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/update"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// runDashboardWeb serves the read-only web dashboard: a live orchestration-DAG
// UI backed by a dashboard.DataSource over the local store. It is strictly
// read-only (it never mutates workflow state) and is never invoked from the
// daemon path — it is a foreground `gitmoot dashboard --web` server that blocks
// until interrupted (SIGINT/SIGTERM).
func runDashboardWeb(home, addr string, stdout, stderr io.Writer) int {
	if _, err := initializedPaths(home); err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}
	// Open (and so migrate) the store once before listening, so no request or
	// background worktree inspection races this process's own schema upgrade
	// (#2639).
	if err := withStore(home, func(*db.Store) error { return nil }); err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}
	ds := &webDataSource{home: home, responseCache: newDashboardJSONCache(stderr)}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}
	srv := &http.Server{Handler: newDashboardWebHandler(ds)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// /api/health serves the worktree-usage block from this background
	// inspection; it lives exactly as long as the server.
	ds.startWorktreeRefresher()
	defer ds.stopWorktreeRefresher()

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	fmt.Fprintf(stdout, "gitmoot dashboard serving read-only at http://%s (Ctrl-C to stop)\n", ln.Addr())

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		fmt.Fprintln(stdout)
		return 0
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "dashboard: %v\n", err)
			return 1
		}
		return 0
	}
}

// newDashboardWebHandler shadows the bounded endpoints covered by the frozen
// #948/#956 cache policies and serves #958's widened workflow JSON
// (description/status) through the cached workflows route. Every other route
// remains owned by the pinned dashboard module.
//
// #2206 removed the Brain, Knowledge and Chat routes, and #2593 removed the
// single-run graph (/api/runs, /api/state, /events) and the Learning page
// (/api/learning/skills): gitmoot does not answer for pages that do not exist.
func newDashboardWebHandler(ds *webDataSource) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", ds.handleJobs)
	mux.HandleFunc("GET /api/charts", ds.handleCharts)
	mux.HandleFunc("GET /api/health", ds.handleHealth)
	// #2639: cached shadow of the module's galaxy route.
	mux.HandleFunc("GET /api/graph", ds.handleGraph)
	mux.HandleFunc("GET /api/overview", ds.handleOverview)
	mux.HandleFunc("GET /api/attention", ds.handleAttention)
	mux.HandleFunc("GET /api/reviewers", ds.handleAgents)
	mux.HandleFunc("GET /api/prs", ds.handleTasks)
	// #958 widens this cached shadow: workflowsJSON now emits description/status,
	// and the "workflows" cache key already includes the workflow-note-id so a
	// status change busts the cache.
	mux.HandleFunc("GET /api/campaigns", ds.handleWorkflows)
	// transition alias for one release (#2596); remove with the old command names
	mux.HandleFunc("GET /api/workflows", ds.handleWorkflows)
	registerDashboardCommsRoutes(mux, ds)
	mux.HandleFunc("GET /api/fleet/activity", ds.handleFleetActivity)
	mux.HandleFunc("GET /api/fleet/activity/events", ds.handleFleetActivityEvents)
	mux.HandleFunc("GET /assets/gitmoot-fleet-activity.css", handleFleetActivityCSS)
	mux.HandleFunc("GET /assets/gitmoot-fleet-activity.js", handleFleetActivityJS)
	mux.HandleFunc("GET /assets/gitmoot-stale-notifications.js", handleStaleNotificationsJS)
	mux.HandleFunc("GET /api/stale-notifications", ds.handleStaleNotificationsAPI)
	// #958 single-label detail widening (no module cache policy for this route).
	mux.HandleFunc("GET /api/campaign/{label}", ds.handleWorkflowAPI)
	mux.Handle("/", withDashboardCommsNav(withStaleNotificationAssets(withFleetActivityAssets(dashboard.Serve(ds)))))
	return mux
}

// dashboardCommsNavAnchor is the markup the Comms nav entry is injected AFTER.
// It is the tail of the module's Org entry, the last surviving FLEET item.
//
// It used to be the tail of the CHAT entry, and #2206 removing Chat from the
// dashboard module would have made this injection silently find nothing and drop
// the Comms entry from the rail with no error anywhere - caught by measuring the
// anchor against the new module before re-pinning, not by the page going missing.
// The Org tail is unique in BOTH the old and new module builds, so the anchor is
// not a fresh single-version bet.
const dashboardCommsNavAnchor = `      <span style="{{ orgNavDotStyle }}"></span>
    </div>`

type dashboardNavItem struct {
	Href  string
	Title string
	Group string
}

var dashboardNavManifest = []dashboardNavItem{
	{Href: "/", Title: "Overview", Group: "WORK"},
	{Href: "/prs", Title: "PRs", Group: "WORK"},
	{Href: "/campaigns", Title: "Campaigns", Group: "WORK"},
	{Href: "/runs", Title: "Runs", Group: "WORK"},
	{Href: "/reviewers", Title: "Reviewers", Group: "FLEET"},
	{Href: "/org", Title: "Org", Group: "FLEET"},
	{Href: "/comms", Title: "Comms", Group: "FLEET"},
	{Href: "/galaxy", Title: "Galaxy", Group: "INSIGHT"},
	{Href: "/charts", Title: "Charts", Group: "INSIGHT"},
	{Href: "/attention", Title: "Needs a human", Group: "SYSTEM"},
	{Href: "/health", Title: "Health", Group: "SYSTEM"},
	{Href: "/config", Title: "Config", Group: "SYSTEM"},
}

func dashboardNavItemForHref(href string) (dashboardNavItem, bool) {
	for _, item := range dashboardNavManifest {
		if item.Href == href {
			return item, true
		}
	}
	return dashboardNavItem{}, false
}

func dashboardCommsSPAItem() string {
	item, ok := dashboardNavItemForHref("/comms")
	if !ok {
		return ""
	}
	return fmt.Sprintf(`    <a href="%s" title="Org communication threads" style="display:flex;align-items:center;gap:11px;padding:9px 11px;border-radius:9px;font-size:13.5px;font-weight:500;cursor:pointer;margin-top:2px;background:transparent;color:#a8aecf;border:1px solid transparent;box-shadow:none;position:relative;text-decoration:none">
      <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round" stroke-linecap="round"><path d="M4 5h12a2 2 0 0 1 2 2v5a2 2 0 0 1-2 2H9l-4 3v-3H4a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2z"/><path d="M9 10h11a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2h-3l-3 2v-2"/></svg>
      <span style="flex:1">%s</span>
    </a>`, item.Href, item.Title)
}

// withDashboardCommsNav adds the gitmoot-owned Comms route to the embedded
// dashboard module's sidebar. Comms is a standalone read-only page, so the
// sidebar entry is an ordinary link; the destination page owns its active-state
// treatment. The replacement is intentionally anchored to the Org entry's nav
// tail so module markup drift fails the rendered-page regression instead of
// placing the link in an arbitrary section.
func withDashboardCommsNav(next http.Handler) http.Handler {
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
			anchor := []byte(dashboardCommsNavAnchor)
			if bytes.Contains(body, anchor) {
				replacement := []byte(dashboardCommsNavAnchor + "\n" + dashboardCommsSPAItem())
				body = bytes.Replace(body, anchor, replacement, 1)
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

type dashboardWorkflowAPIView struct {
	dashboard.WorkflowView
	Description string `json:"description"`
	Status      string `json:"status"`
}

func (d *webDataSource) handleWorkflowAPI(w http.ResponseWriter, r *http.Request) {
	label := strings.TrimSpace(r.PathValue("label"))
	if label == "" {
		http.Error(w, "missing campaign label", http.StatusBadRequest)
		return
	}
	view, err := d.Workflow(r.Context(), label, dashboard.WorkflowQuery{
		RunCursor:  r.URL.Query().Get("rootCursor"),
		NoteCursor: r.URL.Query().Get("noteCursor"),
		MaxRuns:    dashboardAPIWorkflowLimit(r.URL.Query().Get("maxRoots"), dashboardWorkflowMaxRuns),
		MaxNotes:   dashboardAPIWorkflowLimit(r.URL.Query().Get("maxNotes"), dashboardWorkflowMaxNotes),
	})
	if err != nil {
		d.writeDashboardAPIJSON(w, nil, err)
		return
	}
	var meta db.WorkflowMeta
	err = withStore(d.home, func(store *db.Store) error {
		var err error
		meta, err = store.GetWorkflowMeta(r.Context(), label)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	d.writeDashboardAPIJSON(w, dashboardWorkflowAPIView{
		WorkflowView: view,
		Description:  workflowDisplayDescription(label, meta.Description),
		Status:       meta.Status,
	}, err)
}

func dashboardAPIWorkflowLimit(raw string, cap int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > cap {
		return cap
	}
	return value
}

func (d *webDataSource) writeDashboardAPIJSON(w http.ResponseWriter, value any, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, dashboard.ErrWorkflowNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	body, err := marshalDashboardJSON(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// webDataSource implements dashboard.DataSource over a local Gitmoot home. It
// reuses the store APIs for job and workflow details, so it does not duplicate
// store queries or mutate workflow state.
type webDataSource struct {
	home string

	cacheOnce     sync.Once
	responseCache *dashboardJSONCache

	fleetMu     sync.Mutex
	fleetPoller *fleetActivityPoller

	// These hooks keep the shared-poller concurrency tests fast and
	// deterministic. Production leaves them nil/zero.
	fleetSnapshot func(context.Context) (dashboardFleetActivity, error)
	fleetInterval time.Duration

	// mu guards the Health() caches below (Health can be called concurrently and
	// its resolvers run in goroutines). Everything else on the source is stateless
	// per call; the fleet-activity poller uses fleetMu and its own locks.
	mu sync.Mutex
	// daemonVersionKey/daemonVersion cache the running daemon binary's reported
	// version, keyed by (executable path, file mtime) so SEQUENTIAL 12s health
	// polls never re-exec an unchanged binary (there is no singleflight, so
	// concurrent cold requests may each exec once). An empty daemonVersion is a
	// valid (negative) cache entry — resolution is fail-open.
	daemonVersionKey string
	daemonVersion    string
	// updateResult/updateFetchedAt/updateOK cache the daemon-binary update check.
	// updateResult is the last SUCCESSFUL HealthUpdate (nil when the check yielded
	// no usable data); it is retained across failures so a stale-but-good result is
	// served while a refresh fails. A success is honored for updateSuccessTTL, a
	// failure re-tried after updateFailureTTL.
	updateResult    *dashboard.HealthUpdate
	updateFetchedAt time.Time
	updateOK        bool

	// worktrees computes /api/health's worktree-usage block in the background
	// (started and stopped by runDashboardWeb). Nil means it never runs, so
	// health reports the explicit pending value.
	worktrees *dashboardWorktreeRefresher
}

type dashboardServerBuild struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type dashboardHealthDaemon struct {
	dashboard.HealthDaemon
	VersionSource string `json:"versionSource,omitempty"`
}

type dashboardHealthResponse struct {
	dashboard.Health
	Daemon dashboardHealthDaemon `json:"daemon"`
	Server dashboardServerBuild  `json:"server"`
	// Worktrees is the last background inspection's result (#2639), never
	// computed on the request path. WorktreesStatus is "pending" until the first
	// inspection completes (Worktrees is then an empty value and
	// WorktreesComputedAt is 0) and "ready" afterwards. WorktreesComputedAt is
	// the epoch-ms time of that inspection, so a reader can judge its age.
	// WorktreesError is the latest run's failure; a failed run keeps the
	// previous value.
	Worktrees           delegationWorktreeUsage `json:"worktrees"`
	WorktreesStatus     string                  `json:"worktreesStatus"`
	WorktreesComputedAt int64                   `json:"worktreesComputedAt"`
	WorktreesError      string                  `json:"worktreesError,omitempty"`
}

// handleHealth shadows the module's /api/health to add the build of the process
// actually SERVING this response. Without it, a dashboard left on a stale binary
// is invisible: the payload reports only the daemon's build, so the page looks
// current while rendering old code's output (#932).
//
// It must otherwise reproduce the module's handler exactly — every list coerced
// non-nil, so clients that consume the documented shape never see a null where
// the contract promises an array.
func (d *webDataSource) handleHealth(w http.ResponseWriter, r *http.Request) {
	body, outcome, err := d.cacheForDashboard().get(r.Context(), "health", "", dashboardHealthCachePolicy, d.healthJSON)
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func (d *webDataSource) healthJSON(ctx context.Context) ([]byte, error) {
	health, err := d.Health(ctx)
	if err != nil {
		return nil, err
	}
	if health.ResourceLocks == nil {
		health.ResourceLocks = []dashboard.HealthResourceLock{}
	}
	if health.Stuck == nil {
		health.Stuck = []dashboard.HealthStuckJob{}
	}
	if health.RecentFailures == nil {
		health.RecentFailures = []dashboard.HealthFailure{}
	}
	worktrees := d.worktreeHealth()
	build := buildinfo.Current()
	versionSource := "unknown"
	if health.Daemon.Version != "" {
		versionSource = "recorded"
	}
	response := dashboardHealthResponse{
		Health: health,
		Daemon: dashboardHealthDaemon{
			HealthDaemon:  health.Daemon,
			VersionSource: versionSource,
		},
		Server:              dashboardServerBuild{Version: build.Version, Commit: build.Commit},
		Worktrees:           worktrees.usage,
		WorktreesStatus:     worktrees.status,
		WorktreesComputedAt: worktrees.computedAt,
		WorktreesError:      worktrees.err,
	}
	return marshalDashboardJSON(response)
}

var _ dashboard.DataSource = (*webDataSource)(nil)
var _ dashboard.WorkflowDataSource = (*webDataSource)(nil)
var _ dashboard.OverviewDataSource = (*webDataSource)(nil)
var _ dashboard.TasksDataSource = (*webDataSource)(nil)
var _ dashboard.ChangeCursorDataSource = (*webDataSource)(nil)

const (
	dashboardWorkflowMaxRuns  = 50
	dashboardWorkflowMaxNotes = 200
)

// Workflow returns one label's complete run forest and append-only note
// journal. Jobs are payload-scoped by the indexed workflow_id projection; run
// pagination is applied only after complete trees have been assembled.
func (d *webDataSource) Workflow(ctx context.Context, label string, q dashboard.WorkflowQuery) (dashboard.WorkflowView, error) {
	out := dashboard.WorkflowView{Runs: []dashboard.WorkflowRun{}, Notes: []dashboard.WorkflowNoteView{}}
	label = strings.TrimSpace(label)
	if label == "" {
		return out, dashboard.ErrWorkflowNotFound
	}
	q.MaxRuns = cappedWorkflowLimit(q.MaxRuns, dashboardWorkflowMaxRuns)
	q.MaxNotes = cappedWorkflowLimit(q.MaxNotes, dashboardWorkflowMaxNotes)

	err := withStore(d.home, func(store *db.Store) error {
		now := time.Now().UTC()
		summary, err := store.WorkflowSummary(ctx, label)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return dashboard.ErrWorkflowNotFound
			}
			return err
		}
		jobs, err := store.ListWorkflowGraphJobs(ctx, label)
		if err != nil {
			return err
		}
		notes, err := store.ListWorkflowNotes(ctx, label, 0)
		if err != nil {
			return err
		}

		out.Summary = dashboard.WorkflowSummary{
			Label: label, Jobs: summary.JobCount, Queued: summary.Queued,
			Running: summary.Running, Succeeded: summary.Succeeded,
			Failed: summary.Failed, Blocked: summary.Blocked,
			Cancelled: summary.Cancelled, Notes: summary.NoteCount,
			TokensIn: summary.InputTokens, TokensOut: summary.OutputTokens,
			FirstAt: parseJobTimeMillis(summary.FirstAt), LastAt: parseJobTimeMillis(summary.LastAt),
		}
		meta, metaErr := store.GetWorkflowMeta(ctx, label)
		if metaErr != nil && !errors.Is(metaErr, sql.ErrNoRows) {
			return metaErr
		}
		out.State, out.StalledForS = deriveDashboardWorkflowState(now, dashboardActivityFromSummary(summary, out.Summary.LastAt), meta.Status)
		description := workflowDisplayDescription(label, meta.Description)
		out.Summary.Summary = firstNonEmpty(meta.Summary, description)
		author := strings.TrimSpace(meta.Author)
		if author == "" {
			author = strings.TrimSpace(summary.LastHumanAuthor)
		}
		out.Coordinator = dashboard.WorkflowCoordinator{
			Author: author, Pane: strings.TrimSpace(meta.Pane), SessionID: dashboardWorkflowResumeSessionID(meta.SessionID),
		}
		out.WorkDir = strings.TrimSpace(meta.WorkDir)

		runs := buildDashboardWorkflowRuns(jobs, agentRuntimeMap(ctx, store), now)
		runStart := workflowRunCursorStart(runs, strings.TrimSpace(q.RunCursor))
		runEnd := runStart + q.MaxRuns
		if runEnd > len(runs) {
			runEnd = len(runs)
		}
		out.Runs = append(out.Runs, runs[runStart:runEnd]...)
		if runEnd < len(runs) && runEnd > runStart {
			out.NextRunCursor = runs[runEnd-1].RunID
		}

		noteViews := make([]dashboard.WorkflowNoteView, 0, len(notes))
		for _, note := range notes {
			noteViews = append(noteViews, dashboard.WorkflowNoteView{
				ID: note.ID, Author: note.Author, Body: note.Body, Repo: note.Repo,
				CreatedAt: parseJobTimeMillis(note.CreatedAt),
			})
		}
		noteStart := workflowNoteCursorStart(noteViews, strings.TrimSpace(q.NoteCursor))
		noteEnd := noteStart + q.MaxNotes
		if noteEnd > len(noteViews) {
			noteEnd = len(noteViews)
		}
		out.Notes = append(out.Notes, noteViews[noteStart:noteEnd]...)
		if noteEnd < len(noteViews) && noteEnd > noteStart {
			out.NextNoteCursor = dashboardWorkflowNoteCursor(noteViews[noteEnd-1])
		}
		out.Truncated = out.NextRunCursor != "" || out.NextNoteCursor != ""
		return nil
	})
	return out, err
}

// The dashboard module has no separate resume-command field. Its workflow
// detail client builds `claude --resume` whenever SessionID is non-empty, so the
// detail projection exposes only canonical full UUIDs. The index projection
// keeps the raw stored value visible as coordinator context.
func dashboardWorkflowResumeSessionID(value string) string {
	value = strings.TrimSpace(value)
	if !isFullWorkflowSessionUUID(value) {
		return ""
	}
	return value
}

func cappedWorkflowLimit(value, cap int) int {
	if value <= 0 || value > cap {
		return cap
	}
	return value
}

func workflowRunCursorStart(runs []dashboard.WorkflowRun, cursor string) int {
	if cursor == "" {
		return 0
	}
	for i := range runs {
		if runs[i].RunID == cursor {
			return i + 1
		}
	}
	return len(runs)
}

func dashboardWorkflowNoteCursor(note dashboard.WorkflowNoteView) string {
	return strconv.FormatInt(note.CreatedAt, 10) + ":" + strconv.FormatInt(note.ID, 10)
}

func workflowNoteCursorStart(notes []dashboard.WorkflowNoteView, cursor string) int {
	if cursor == "" {
		return 0
	}
	for i := range notes {
		if dashboardWorkflowNoteCursor(notes[i]) == cursor {
			return i + 1
		}
	}
	return len(notes)
}

// buildDashboardWorkflowRuns groups a label-bounded projection by its
// denormalized root_id and maps each row into the compact, event-free dashboard
// contract. The input query is ordered by created_at,id, so run and node order
// remain deterministic without another store read.
func buildDashboardWorkflowRuns(jobs []db.Job, runtimeByAgent map[string]string, now time.Time) []dashboard.WorkflowRun {
	if len(jobs) == 0 {
		return []dashboard.WorkflowRun{}
	}
	payloadByID := make(map[string]workflow.JobPayload, len(jobs))
	jobByID := make(map[string]db.Job, len(jobs))
	childrenByParent := map[string][]db.Job{}
	byRoot := map[string][]db.Job{}
	var rootOrder []string
	for _, job := range jobs {
		payload, _ := workflow.DecodeJobPayload(job)
		payloadByID[job.ID] = payload
		jobByID[job.ID] = job
		parentID := strings.TrimSpace(job.ParentJobID)
		if parentID == "" {
			parentID = strings.TrimSpace(payload.ParentJobID)
		}
		if parentID != "" {
			childrenByParent[parentID] = append(childrenByParent[parentID], job)
		}
		rootID := strings.TrimSpace(job.RootID)
		if rootID == "" {
			rootID = job.ID
		}
		if _, ok := byRoot[rootID]; !ok {
			rootOrder = append(rootOrder, rootID)
		}
		byRoot[rootID] = append(byRoot[rootID], job)
	}

	runs := make([]dashboard.WorkflowRun, 0, len(rootOrder))
	for _, rootID := range rootOrder {
		tree := byRoot[rootID]
		root, ok := jobByID[rootID]
		if !ok {
			root = tree[0]
		}
		states := make([]string, 0, len(tree))
		nodes := make([]dashboard.WorkflowNode, 0, len(tree))
		children := make([]dashboard.WorkflowChild, 0, max(0, len(tree)-1))
		var startedAt, endedAt int64
		rootRepo := strings.TrimSpace(payloadByID[root.ID].Repo)
		for _, job := range tree {
			payload := payloadByID[job.ID]
			deps, action := resolveDelegationEdges(job, payloadByID, childrenByParent)
			if len(payload.Deps) > 0 {
				deps = resolveWorkflowPayloadDeps(job, payload, childrenByParent)
			}
			node := dashboard.WorkflowNode{
				ID: job.ID, ParentID: strings.TrimSpace(job.ParentJobID), Deps: deps,
				Title: nodeTitle(payload, job, action), Agent: job.Agent,
				Runtime: resolveJobRuntime(job, payload, runtimeByAgent),
				Model:   strings.TrimSpace(job.Model), State: mapNodeState(job.State),
				StartedAt: parseJobTimeMillis(job.CreatedAt),
			}
			if node.ParentID == "" {
				node.ParentID = strings.TrimSpace(payload.ParentJobID)
			}
			if workflow.IsFinalJobState(strings.TrimSpace(job.State)) {
				node.EndedAt = parseJobTimeMillis(job.UpdatedAt)
			}
			nodes = append(nodes, node)
			states = append(states, job.State)
			if node.StartedAt > 0 && (startedAt == 0 || node.StartedAt < startedAt) {
				startedAt = node.StartedAt
			}
			updatedAt := parseJobTimeMillis(job.UpdatedAt)
			if updatedAt > endedAt {
				endedAt = updatedAt
			}
			if rootRepo == "" {
				rootRepo = strings.TrimSpace(payload.Repo)
			}
			if job.ID != root.ID {
				childAction := strings.TrimSpace(action)
				if childAction == "" {
					childAction = node.Title
				}
				children = append(children, dashboard.WorkflowChild{
					ID: job.ID, Action: childAction, Agent: job.Agent, Runtime: node.Runtime,
					State: node.State, ElapsedS: dashboardWorkflowElapsedSeconds(node.StartedAt, node.EndedAt, now),
				})
			}
		}
		runState := aggregateRunState(states)
		if runState == "running" || runState == "queued" {
			endedAt = 0
		}
		runs = append(runs, dashboard.WorkflowRun{
			RunID: rootID, Title: runTitle(payloadByID[root.ID], root),
			Agent: strings.TrimSpace(root.Agent), Runtime: resolveJobRuntime(root, payloadByID[root.ID], runtimeByAgent),
			Repo: rootRepo, State: runState, StartedAt: startedAt, EndedAt: endedAt,
			ElapsedS: dashboardWorkflowElapsedSeconds(startedAt, endedAt, now),
			Children: children, Nodes: nodes,
		})
	}
	return runs
}

func dashboardWorkflowElapsedSeconds(startedAt, endedAt int64, now time.Time) int64 {
	if startedAt <= 0 {
		return 0
	}
	if endedAt <= 0 {
		endedAt = now.UnixMilli()
	}
	return max(0, (endedAt-startedAt)/1000)
}

func resolveWorkflowPayloadDeps(job db.Job, payload workflow.JobPayload, childrenByParent map[string][]db.Job) []string {
	parentID := strings.TrimSpace(job.ParentJobID)
	if parentID == "" {
		parentID = strings.TrimSpace(payload.ParentJobID)
	}
	delegationJobs := delegationJobIDs(childrenByParent[parentID])
	deps := make([]string, 0, len(payload.Deps))
	for _, dep := range payload.Deps {
		dep = strings.TrimSpace(dep)
		if id := delegationJobs[dep]; id != "" {
			deps = append(deps, id)
		}
	}
	return deps
}

// Job returns a single node by job id, resolving its historical delegation deps
// against its sibling children (the same mapping State uses).
func (d *webDataSource) Job(ctx context.Context, jobID string) (dashboard.Node, error) {
	var node dashboard.Node
	err := withStore(d.home, func(store *db.Store) error {
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return dashboard.ErrJobNotFound
			}
			return err
		}
		payload, _ := workflow.DecodeJobPayload(job)
		events, _ := store.ListJobEvents(ctx, jobID)
		runtimeByAgent := agentRuntimeMap(ctx, store)

		var deps []string
		var action string
		if pj := strings.TrimSpace(job.ParentJobID); pj != "" && strings.TrimSpace(job.DelegationID) != "" {
			action = job.Type
			if siblings, serr := store.ListJobsByParent(ctx, pj); serr == nil {
				delegToJob := delegationJobIDs(siblings)
				for _, dep := range payload.Deps {
					if id := delegToJob[strings.TrimSpace(dep)]; id != "" {
						deps = append(deps, id)
					}
				}
			}
		}
		node = buildDashboardNode(job, payload, events, runtimeByAgent, deps, action)
		return nil
	})
	return node, err
}

// Jobs returns every job across every run, flattened into one JobSummary each,
// sorted newest-activity first. ListDashboardJobSummaries keeps full payloads
// out of this hot path: SQLite projects only the prompt/repo/PR/ephemeral-runtime
// values this response consumes and joins the registered runtime. No cap — the
// Jobs page filters client-side.
func (d *webDataSource) Jobs(ctx context.Context) ([]dashboard.JobSummary, error) {
	out := []dashboard.JobSummary{}
	err := withStore(d.home, func(store *db.Store) error {
		jobs, err := store.ListDashboardJobSummaries(ctx)
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return nil
		}
		jobByID := make(map[string]db.Job, len(jobs))
		for _, j := range jobs {
			jobByID[j.ID] = db.Job{ID: j.ID, Agent: j.Agent, Type: j.Type, ParentJobID: j.ParentJobID}
		}

		out = make([]dashboard.JobSummary, 0, len(jobs))
		for _, j := range jobs {
			// Kind mirrors summarizeRuns: parse the id's "<origin>-<kind>-<agent>-
			// <hash>" shape (root jobs), else fall back to the lowercased Type column
			// (delegation children, whose ids don't carry that shape).
			job := jobByID[j.ID]
			kind, _ := parseRunKindAgent(j.ID, job)
			started := parseJobTimeMillis(j.CreatedAt)
			updated := parseJobTimeMillis(j.UpdatedAt)
			var duration int64
			if started > 0 && updated > started {
				duration = updated - started
			}
			runtimeName := strings.TrimSpace(j.RegisteredRuntime)
			if runtimeName == "" {
				runtimeName = strings.TrimSpace(j.EphemeralRuntime)
			}
			title := firstInstructionLine(j.Instructions)
			if title == "" {
				title = j.ID
			}
			out = append(out, dashboard.JobSummary{
				ID:        j.ID,
				Title:     title,
				Agent:     strings.TrimSpace(j.Agent),
				Runtime:   runtimeName,
				Repo:      strings.TrimSpace(j.Repo),
				Kind:      kind,
				State:     mapNodeState(j.State),
				Depth:     j.DelegationDepth,
				Run:       jobRootID(jobByID, j.ID),
				PR:        j.PullRequest,
				Started:   started,
				Updated:   updated,
				Duration:  duration,
				TokensIn:  j.InputTokens,
				TokensOut: j.OutputTokens,
			})
		}
		// Newest activity first, deterministic tie-break on id.
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

// Agents lists the registered agents (one AgentSummary each). It is a single
// read-only pass: ListAgents supplies the rows' identity/config, and the SAME
// ListJobs scan aggregates each agent's job counts and last-active time. Rows
// sort most-recently-active first (never-active last, alphabetical).
func (d *webDataSource) Agents(ctx context.Context) ([]dashboard.AgentSummary, error) {
	out := []dashboard.AgentSummary{}
	err := withStoreAndPaths(d.home, func(paths config.Paths, store *db.Store) error {
		agents, err := store.ListAgents(ctx)
		if err != nil {
			return err
		}
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			return err
		}

		// Aggregate per-agent job stats from the single ListJobs pass (shared with
		// Agent()).
		byAgent := aggregateAgentJobStats(jobs)

		out = make([]dashboard.AgentSummary, 0, len(agents))
		for _, a := range agents {
			summary := newAgentSummary(a)
			if s := byAgent[a.Name]; s != nil {
				s.applyTo(&summary)
			}
			out = append(out, summary)
		}
		// Most-recently-active first; never-active (LastActive == 0) fall to the
		// end, alphabetical.
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].LastActive != out[j].LastActive {
				return out[i].LastActive > out[j].LastActive
			}
			return out[i].Name < out[j].Name
		})
		return nil
	})
	return out, err
}

// Agent returns the click-through detail for a single agent by name: the same
// AgentSummary Agents() builds for that one row, plus its [agents.<name>] config
// section. An unknown name maps to the dashboard's not-found sentinel (mirroring
// how Job() maps an unknown job id), so the API layer returns 404 rather than 500.
func (d *webDataSource) Agent(ctx context.Context, name string) (dashboard.AgentDetail, error) {
	var detail dashboard.AgentDetail
	err := withStoreAndPaths(d.home, func(paths config.Paths, store *db.Store) error {
		agent, err := store.GetAgent(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return dashboard.ErrAgentNotFound
			}
			return err
		}
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			return err
		}

		// Build the summary exactly as Agents() does for one row: identity from the
		// registered agent, tallies from the shared per-agent aggregation.
		summary := newAgentSummary(agent)
		byAgent := aggregateAgentJobStats(jobs)
		if s := byAgent[agent.Name]; s != nil {
			s.applyTo(&summary)
		}

		// Config-section visibility. Load the [agents.<name>] sections ONCE, fail-open
		// (a config-load error => no section, Config nil — never an endpoint error).
		// Config is nil unless this agent has its own section.
		if at, ok := loadAgentTypesFailOpen(paths)[agent.Name]; ok {
			detail.Config = agentConfigInfo(at)
		}
		detail.AgentSummary = summary
		return nil
	})
	if err != nil {
		return dashboard.AgentDetail{}, err
	}
	return detail, nil
}

// newAgentSummary builds the identity portion of an AgentSummary (everything but
// the job tallies) from a registered agent row. Shared by Agents() and Agent()
// so the two views map the same fields identically.
func newAgentSummary(a db.Agent) dashboard.AgentSummary {
	return dashboard.AgentSummary{
		Name:           a.Name,
		Role:           strings.TrimSpace(a.Role),
		Runtime:        strings.TrimSpace(a.Runtime),
		RepoScope:      splitRepoScope(a.RepoScope),
		Model:          strings.TrimSpace(a.Model),
		Capabilities:   a.Capabilities,
		AutonomyPolicy: strings.TrimSpace(a.AutonomyPolicy),
		Health:         strings.TrimSpace(a.HealthStatus),
	}
}

// loadAgentTypesFailOpen loads the [agents.<name>] config sections for the config
// chip / config panel, returning nil on ANY error (missing/unreadable/malformed
// config) so both Agents() and Agent() degrade to "no config visibility" rather
// than failing the endpoint. Indexing the nil result is safe (a missing key
// yields the zero AgentType, ok=false), so callers gate on the comma-ok.
func loadAgentTypesFailOpen(paths config.Paths) map[string]config.AgentType {
	types, err := config.LoadAgentTypes(paths)
	if err != nil {
		return nil
	}
	return types
}

// agentConfigInfo maps a resolved config.AgentType onto the dashboard's
// AgentConfigInfo. The values are the [agents.<name>] section as LoadAgentTypes
// resolves it — parse-time defaults INCLUDED (capabilities default ["ask"],
// job_timeout 10m) — surfaced as-is per the contract. Only ever called for an
// agent that has a config section (a non-nil return is meaningful presence).
func agentConfigInfo(at config.AgentType) *dashboard.AgentConfigInfo {
	return &dashboard.AgentConfigInfo{
		JobTimeout:   strings.TrimSpace(at.JobTimeout),
		Model:        strings.TrimSpace(at.Model),
		Capabilities: at.Capabilities,
	}
}

// aggregateAgentJobStats folds a job slice into per-agent tallies keyed by agent
// name. It is the single aggregation both Agents() (which walks byAgent for every
// registered row) and Agent() (which picks one byAgent entry) share, so their
// counts can never diverge.
func aggregateAgentJobStats(jobs []db.Job) map[string]*agentJobStat {
	byAgent := map[string]*agentJobStat{}
	for _, j := range jobs {
		name := strings.TrimSpace(j.Agent)
		s := byAgent[name]
		if s == nil {
			s = &agentJobStat{}
			byAgent[name] = s
		}
		s.observe(j)
	}
	return byAgent
}

// agentJobStat accumulates one agent's job tallies across the ListJobs pass:
// total jobs, live/terminal counts, and the most-recent update time (epoch ms).
type agentJobStat struct {
	jobCount, running, succeeded, failed int
	lastActive                           int64
}

// observe folds one job's state and update time into the running tally.
func (s *agentJobStat) observe(job db.Job) {
	s.jobCount++
	switch mapNodeState(job.State) {
	case "running":
		s.running++
	case "succeeded":
		s.succeeded++
	case "failed":
		s.failed++
	}
	if u := parseJobTimeMillis(job.UpdatedAt); u > s.lastActive {
		s.lastActive = u
	}
}

// applyTo copies the accumulated tallies onto an AgentSummary.
func (s *agentJobStat) applyTo(summary *dashboard.AgentSummary) {
	summary.JobCount = s.jobCount
	summary.RunningCount = s.running
	summary.SucceededCount = s.succeeded
	summary.FailedCount = s.failed
	summary.LastActive = s.lastActive
}

// splitRepoScope splits the store's comma-separated repo_scope string into a
// trimmed, non-empty slice for the AgentSummary contract's []string RepoScope.
// Returns nil (an omitted field) when the scope is empty.
func splitRepoScope(scope string) []string {
	var out []string
	for _, part := range strings.Split(scope, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// jobTitle titles a JobSummary from its payload: the first non-empty line of the
// job's prompt, falling back to the job id when there is none — the JobSummary
// contract's "first line of prompt, fallback id".
func jobTitle(payload workflow.JobPayload, job db.Job) string {
	if t := firstInstructionLine(payload.Instructions); t != "" {
		return t
	}
	return job.ID
}

// resolveJobRuntime resolves a job's runtime (omp|shell, or a removed name)
// from the agent registry, falling back to the payload's ephemeral worker spec
// for a delegated ephemeral worker that has no registered agent row. Returns ""
// when neither source names a runtime.
func resolveJobRuntime(job db.Job, payload workflow.JobPayload, runtimeByAgent map[string]string) string {
	rt := strings.TrimSpace(runtimeByAgent[job.Agent])
	if rt == "" && payload.Ephemeral != nil {
		rt = strings.TrimSpace(payload.Ephemeral.Runtime)
	}
	return rt
}

// dashboardGalaxyWindow bounds the galaxy graph to jobs created in the trailing
// 30 days. The full history (29.7k jobs on the live store in #2639) made the
// response 21 MB and the request 11-15 s; the window keeps both proportional to
// recent activity instead of to the store's age.
const dashboardGalaxyWindow = 30 * 24 * time.Hour

// Graph returns the "galaxy" graph: a union of every job created within
// dashboardGalaxyWindow across every run, plus synthetic repo/agent hub nodes
// that cluster the jobs. Links are delegation parentage, a per-parent-group
// sibling mesh (the density lever, capped so one huge fan-out can't dominate),
// and job->repo / job->agent spokes. A non-empty repo scopes the visible jobs
// (and their hubs) to that repository; Repos always lists every repo in the
// window so the UI filter dropdown stays complete. Jobs are read without their
// payload: repo is the denormalized column and the title fields are projected
// in SQL. Read-only; output is sorted for determinism.
func (d *webDataSource) Graph(ctx context.Context, repo string) (dashboard.Graph, error) {
	out := dashboard.Graph{Nodes: []dashboard.GraphNode{}, Links: []dashboard.GraphLink{}, Repos: []string{}}
	filter := strings.TrimSpace(repo)
	since := dashboardSQLiteTime(time.Now().Add(-dashboardGalaxyWindow))
	err := withStore(d.home, func(store *db.Store) error {
		rows, err := store.ListDashboardGraphJobs(ctx, since)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		jobs := make([]db.Job, 0, len(rows))
		jobByID := make(map[string]db.Job, len(rows))
		// The two payload fields a title can use, and each job's repo.
		payloadByID := make(map[string]workflow.JobPayload, len(rows))
		repoByID := make(map[string]string, len(rows))
		allRepos := map[string]bool{}
		for _, row := range rows {
			j := db.Job{
				ID: row.ID, Agent: row.Agent, Type: row.Type, State: row.State,
				ParentJobID: row.ParentJobID, DelegationID: row.DelegationID, WorkflowID: row.WorkflowID,
				Repo: row.Repo, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			}
			jobs = append(jobs, j)
			jobByID[j.ID] = j
			payloadByID[j.ID] = workflow.JobPayload{TaskTitle: row.TaskTitle, Instructions: row.Instructions}
			r := strings.TrimSpace(row.Repo)
			repoByID[j.ID] = r
			if r != "" {
				allRepos[r] = true
			}
		}

		// The visible job set: everything, or only jobs in the filtered repo.
		visible := func(id string) bool {
			if filter == "" {
				return true
			}
			return repoByID[id] == filter
		}

		// Job nodes + collect the hubs referenced by visible jobs. Workflow
		// rollups are accumulated in this same pass so the galaxy never
		// performs a second jobs scan.
		repoHubs := map[string]bool{}
		agentHubs := map[string]bool{}
		type workflowRollup struct {
			jobs, tokensIn, tokensOut int
		}
		workflowHubs := map[string]*workflowRollup{}
		for _, j := range jobs {
			if !visible(j.ID) {
				continue
			}
			p := payloadByID[j.ID]
			r := repoByID[j.ID]
			agent := strings.TrimSpace(j.Agent)
			// Run points at the job's root. That root may be older than the
			// galaxy window, or under an active repo filter belong to a different
			// repo, and thus be absent from Nodes; fall back to self-root so no
			// node attribute references a missing node.
			run := jobRootID(jobByID, j.ID)
			if _, inWindow := jobByID[run]; !inWindow || (filter != "" && !visible(run)) {
				run = j.ID
			}
			out.Nodes = append(out.Nodes, dashboard.GraphNode{
				ID:    j.ID,
				Type:  "run",
				Label: nodeTitle(p, j, ""),
				State: mapNodeState(j.State),
				Agent: agent,
				Repo:  r,
				Run:   run,
			})
			if r != "" {
				repoHubs[r] = true
			}
			if agent != "" {
				agentHubs[agent] = true
			}
			if label := strings.TrimSpace(j.WorkflowID); label != "" {
				rollup := workflowHubs[label]
				if rollup == nil {
					rollup = &workflowRollup{}
					workflowHubs[label] = rollup
				}
				rollup.jobs++
				rollup.tokensIn += j.InputTokens
				rollup.tokensOut += j.OutputTokens
			}
		}

		// Hub nodes (sorted, appended after the job nodes).
		hubRepos := sortedSetKeys(repoHubs)
		for _, r := range hubRepos {
			out.Nodes = append(out.Nodes, dashboard.GraphNode{ID: "repo::" + r, Type: "repo", Label: r, Repo: r})
		}
		for _, a := range sortedSetKeys(agentHubs) {
			out.Nodes = append(out.Nodes, dashboard.GraphNode{ID: "agent::" + a, Type: "reviewer", Label: a, Agent: a})
		}
		workflowLabels := make([]string, 0, len(workflowHubs))
		for label := range workflowHubs {
			workflowLabels = append(workflowLabels, label)
		}
		sort.Strings(workflowLabels)
		noteCounts, err := store.WorkflowNoteCounts(ctx, workflowLabels)
		if err != nil {
			return err
		}
		for _, label := range workflowLabels {
			rollup := workflowHubs[label]
			out.Nodes = append(out.Nodes, dashboard.GraphNode{
				ID: "workflow::" + label, Type: "campaign", Label: label,
				JobCount: rollup.jobs, NoteCount: noteCounts[label],
				TokensIn: rollup.tokensIn, TokensOut: rollup.tokensOut,
			})
		}

		// Delegation (parent) links + repo/agent spokes.
		for _, j := range jobs {
			if !visible(j.ID) {
				continue
			}
			if pj := strings.TrimSpace(j.ParentJobID); pj != "" {
				if _, ok := jobByID[pj]; ok && visible(pj) {
					out.Links = append(out.Links, dashboard.GraphLink{Source: pj, Target: j.ID, Kind: "parent"})
				}
			}
			if r := repoByID[j.ID]; r != "" {
				out.Links = append(out.Links, dashboard.GraphLink{Source: j.ID, Target: "repo::" + r, Kind: "repo"})
			}
			if a := strings.TrimSpace(j.Agent); a != "" {
				out.Links = append(out.Links, dashboard.GraphLink{Source: j.ID, Target: "agent::" + a, Kind: "reviewer"})
			}
			if label := strings.TrimSpace(j.WorkflowID); label != "" {
				out.Links = append(out.Links, dashboard.GraphLink{Source: j.ID, Target: "workflow::" + label, Kind: "campaign"})
			}
		}

		// Sibling mesh: group visible jobs by (root, parent) and add a dep link
		// between every pair in each group of >=2 (capped at siblingMeshCap so a
		// single huge fan-out can't dominate the density).
		type groupKey struct{ root, parent string }
		groups := map[groupKey][]string{}
		for _, j := range jobs {
			if !visible(j.ID) {
				continue
			}
			key := groupKey{root: jobRootID(jobByID, j.ID), parent: strings.TrimSpace(j.ParentJobID)}
			groups[key] = append(groups[key], j.ID)
		}
		for _, members := range groups {
			if len(members) < 2 {
				continue
			}
			sort.Strings(members)
			if len(members) > siblingMeshCap {
				members = members[:siblingMeshCap]
			}
			for i := 0; i < len(members); i++ {
				for k := i + 1; k < len(members); k++ {
					out.Links = append(out.Links, dashboard.GraphLink{Source: members[i], Target: members[k], Kind: "dep"})
				}
			}
		}

		out.Repos = sortedSetKeys(allRepos)

		sort.SliceStable(out.Nodes, func(i, j int) bool {
			return graphNodeLess(out.Nodes[i], out.Nodes[j])
		})
		sort.SliceStable(out.Links, func(i, j int) bool {
			a, b := out.Links[i], out.Links[j]
			if a.Source != b.Source {
				return a.Source < b.Source
			}
			if a.Target != b.Target {
				return a.Target < b.Target
			}
			return a.Kind < b.Kind
		})
		return nil
	})
	return out, err
}

// siblingMeshCap bounds the number of members of a single (root, parent) group
// that participate in the sibling mesh, so one large fan-out cannot dominate the
// galaxy graph's edge density.
const siblingMeshCap = 24

// graphNodeLess orders galaxy nodes deterministically: job nodes first (by id),
// then the repo/agent hubs, so a stable graph is returned regardless of store
// iteration order.
func graphNodeLess(a, b dashboard.GraphNode) bool {
	rank := func(t string) int {
		if t == "job" {
			return 0
		}
		return 1
	}
	if ra, rb := rank(a.Type), rank(b.Type); ra != rb {
		return ra < rb
	}
	return a.ID < b.ID
}

// sortedSetKeys returns the keys of a string-set as a sorted slice.
func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Charts returns the per-day time series plus top-agent/top-repo/totals
// breakdowns for the Charts page. It is a single read-only ListJobs pass handed
// to buildCharts: each job buckets into its CreatedAt UTC day, states map via
// mapNodeState, and InputTokens/OutputTokens sum into that day's bucket. days
// selects the window (7/30/90 => the last N days ending today UTC; 0 => the full
// earliest..latest history envelope) which buildCharts zero-fills continuously.
func (d *webDataSource) Charts(ctx context.Context, days int) (dashboard.Charts, error) {
	out := dashboard.Charts{Days: []dashboard.ChartDay{}, Agents: []dashboard.ChartAgent{}, Repos: []dashboard.ChartRepo{}}
	err := withStore(d.home, func(store *db.Store) error {
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			return err
		}
		out = buildCharts(jobs, days, time.Now().UTC(), agentRuntimeMap(ctx, store))
		return nil
	})
	return out, err
}

// chartTopN bounds the per-agent and per-repo breakdowns so a busy home does not
// return an unbounded leaderboard.
const chartTopN = 12

// buildCharts aggregates a job slice into the Charts contract: a continuous,
// zero-filled per-day series (oldest->newest), the top-N agents/repos by job
// count, and range totals. It is pure (now and the agent-runtime map are passed
// in) so it is unit-tested directly, mirroring summarizeRuns. A job with no
// parseable CreatedAt has no day bucket and is omitted from the series/totals.
func buildCharts(jobs []db.Job, days int, now time.Time, runtimeByAgent map[string]string) dashboard.Charts {
	out := dashboard.Charts{Days: []dashboard.ChartDay{}, Agents: []dashboard.ChartAgent{}, Repos: []dashboard.ChartRepo{}}

	// Resolve each job's UTC day-start from CreatedAt once, tracking the observed
	// min/max day for the all-history (days==0) window.
	type jobDay struct {
		job db.Job
		day time.Time
		ok  bool
	}
	parsed := make([]jobDay, 0, len(jobs))
	var minDay, maxDay time.Time
	haveRange := false
	for _, j := range jobs {
		jd := jobDay{job: j}
		if ms := parseJobTimeMillis(j.CreatedAt); ms > 0 {
			jd.day = utcDayStart(time.UnixMilli(ms))
			jd.ok = true
			if !haveRange || jd.day.Before(minDay) {
				minDay = jd.day
			}
			if !haveRange || jd.day.After(maxDay) {
				maxDay = jd.day
			}
			haveRange = true
		}
		parsed = append(parsed, jd)
	}

	// Resolve the window [start, end] as UTC day-starts. days>0 is the last N days
	// ending today; days==0 is the full earliest..latest envelope (empty when no
	// job carries a parseable day), extended to today so the all-history view
	// shares its right edge with the windowed views (and the fake feed) even on
	// an idle day with no jobs created today.
	var start, end time.Time
	switch {
	case days > 0:
		end = utcDayStart(now)
		start = end.AddDate(0, 0, -(days - 1))
	case haveRange:
		start, end = minDay, maxDay
		if today := utcDayStart(now); today.After(end) {
			end = today
		}
	default:
		return out
	}
	inWindow := func(day time.Time) bool { return !day.Before(start) && !day.After(end) }

	dayBuckets := map[string]*dashboard.ChartDay{}
	agentAgg := map[string]*dashboard.ChartAgent{}
	repoAgg := map[string]int{}
	activeAgents := map[string]bool{}
	var totals dashboard.ChartTotals

	for _, jd := range parsed {
		if !jd.ok || !inWindow(jd.day) {
			continue
		}
		j := jd.job
		payload, _ := workflow.DecodeJobPayload(j)

		key := jd.day.Format("2006-01-02")
		b := dayBuckets[key]
		if b == nil {
			b = &dashboard.ChartDay{Date: key}
			dayBuckets[key] = b
		}
		switch mapNodeState(j.State) {
		case "succeeded":
			b.Succeeded++
			totals.Succeeded++
		case "failed":
			b.Failed++
			totals.Failed++
		case "cancelled":
			b.Cancelled++
		case "blocked":
			b.Blocked++
		case "queued":
			b.Queued++
		case "running":
			b.Running++
		}
		b.TokensIn += j.InputTokens
		b.TokensOut += j.OutputTokens

		totals.Jobs++
		totals.TokensIn += j.InputTokens
		totals.TokensOut += j.OutputTokens

		if name := strings.TrimSpace(j.Agent); name != "" {
			activeAgents[name] = true
			a := agentAgg[name]
			if a == nil {
				// Runtime is resolved once (first job seen for the name) via the same
				// registry+ephemeral-fallback path Jobs() uses; a registered agent's
				// runtime is stable across its jobs.
				a = &dashboard.ChartAgent{Name: name, Runtime: resolveJobRuntime(j, payload, runtimeByAgent)}
				agentAgg[name] = a
			}
			a.Jobs++
			a.TokensOut += j.OutputTokens
		}
		if repo := strings.TrimSpace(payload.Repo); repo != "" {
			repoAgg[repo]++
		}
	}
	totals.ActiveAgents = len(activeAgents)

	// Continuous zero-filled series, oldest->newest.
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if b := dayBuckets[key]; b != nil {
			out.Days = append(out.Days, *b)
		} else {
			out.Days = append(out.Days, dashboard.ChartDay{Date: key})
		}
	}

	// Top-N agents by jobs desc, name tie-break.
	agents := make([]dashboard.ChartAgent, 0, len(agentAgg))
	for _, a := range agentAgg {
		agents = append(agents, *a)
	}
	sort.SliceStable(agents, func(i, j int) bool {
		if agents[i].Jobs != agents[j].Jobs {
			return agents[i].Jobs > agents[j].Jobs
		}
		return agents[i].Name < agents[j].Name
	})
	if len(agents) > chartTopN {
		agents = agents[:chartTopN]
	}
	out.Agents = agents

	// Top-N repos by jobs desc, repo tie-break.
	repos := make([]dashboard.ChartRepo, 0, len(repoAgg))
	for repo, n := range repoAgg {
		repos = append(repos, dashboard.ChartRepo{Repo: repo, Jobs: n})
	}
	sort.SliceStable(repos, func(i, j int) bool {
		if repos[i].Jobs != repos[j].Jobs {
			return repos[i].Jobs > repos[j].Jobs
		}
		return repos[i].Repo < repos[j].Repo
	})
	if len(repos) > chartTopN {
		repos = repos[:chartTopN]
	}
	out.Repos = repos

	out.Totals = totals
	return out
}

// utcDayStart returns the UTC midnight that begins t's day, the bucket key for
// the per-day chart series.
func utcDayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// stuckQueuedThreshold is how long a job may sit queued before the Health page
// treats it as wedged (blocked jobs are surfaced regardless of age).
const stuckQueuedThreshold = 10 * time.Minute

// dashboardHealthRecentFailures is the number of newest failed jobs the Health
// page lists (the module's "last 10 failed, newest first").
const dashboardHealthRecentFailures = 10

// Health returns the daemon liveness, fleet totals, held resource locks, wedged jobs and
// recent failures behind the Health page. The daemon block mirrors
// buildDashboardSnapshot (currentDaemonPID + daemon.json meta over d.home);
// everything else is bounded store reads (state counts, the blocked/queued rows,
// the newest failures) plus the resource lock listing.
// Stuck reasons reuse loadStuckReason/deriveStuckReason unchanged.
func (d *webDataSource) Health(ctx context.Context) (dashboard.Health, error) {
	out := dashboard.Health{
		ResourceLocks:  []dashboard.HealthResourceLock{},
		Stuck:          []dashboard.HealthStuckJob{},
		RecentFailures: []dashboard.HealthFailure{},
	}
	paths, err := initializedPaths(d.home)
	if err != nil {
		return out, err
	}

	// Daemon liveness — same readers buildDashboardSnapshot uses. StartedAt comes
	// off the running daemon's persisted meta (RFC3339 -> epoch ms); a stopped
	// daemon reports 0. The persisted meta also carries the daemon binary's path,
	// which the version probe execs.
	state := daemonProcessState(paths)
	var executable string
	var recordedDaemonVersion string
	if pid, _, perr := currentDaemonPID(state); perr == nil && pid > 0 {
		out.Daemon.Running = true
		out.Daemon.PID = pid
		if meta, merr := readDaemonMeta(state); merr == nil {
			out.Daemon.StartedAt = parseJobTimeMillis(meta.StartedAt)
			executable = strings.TrimSpace(meta.Executable)
			recordedDaemonVersion = meta.Version
		}
	}

	// The on-disk probe (execs the binary) and the update check (hits GitHub) are
	// the only slow parts of Health. Run them CONCURRENTLY — and with the local
	// store read — so a cold cache never serially stacks their 3s + 4s timeouts
	// past the health endpoint's latency budget. Both fail-open: a resolution
	// error leaves the field empty/nil, never an error.
	//
	// Two DIFFERENT builds matter here and must not be conflated (#932):
	//   - the build the daemon PROCESS is running (recorded in daemon.json),
	//     which is what Daemon.Version reports; and
	//   - the build of the binary now ON DISK at the daemon's path, which is what
	//     `gitmoot update` replaces and therefore what the update badge compares.
	// Pinning the badge to the running daemon instead would make it sticky: after
	// an update, it would keep claiming an update is available until someone
	// happened to restart the daemon.
	var probes sync.WaitGroup
	var onDiskVersion string
	var updateInfo *dashboard.HealthUpdate
	probes.Add(2)
	go func() {
		defer probes.Done()
		onDiskVersion = d.resolveOnDiskVersion(ctx, executable)
	}()
	go func() {
		defer probes.Done()
		updateInfo = d.checkUpdate(ctx, executable)
	}()

	// Every jobs read below is bounded (#2639): totals are one GROUP BY, the
	// stuck candidates are the blocked/queued rows, and recent failures are the
	// newest dashboardHealthRecentFailures failed rows with their title projected
	// in SQL. Only the stuck rows, which the response returns, have their payload
	// decoded; the terminal backlog is never read.
	err = withStore(d.home, func(store *db.Store) error {
		counts, err := store.CountDashboardJobsByState(ctx)
		if err != nil {
			return err
		}
		for state, count := range counts {
			switch mapNodeState(state) {
			case "queued":
				out.Totals.Queued += count
			case "running":
				out.Totals.Running += count
			case "blocked":
				out.Totals.Blocked += count
			case "succeeded":
				out.Totals.Succeeded += count
			case "failed":
				out.Totals.Failed += count
			case "cancelled":
				out.Totals.Cancelled += count
			}
		}

		// Resource locks (runtime sessions etc.) are listed ONCE and
		// shared: they feed both out.ResourceLocks below and every stuck job's
		// deriveStuckReason. Going through loadStuckReason instead would re-scan
		// the resource_locks table per stuck job on every 12s /api/health poll —
		// an N+1 that grows with the blocked-job count in unattended operation.
		resourceLocks, err := store.ListResourceLocks(ctx)
		if err != nil {
			return err
		}

		// Only blocked and queued rows (an empty state maps to queued) can be
		// stuck; healthStuckSince applies the queued threshold.
		var candidates []db.Job
		for _, state := range []string{"blocked", "queued", ""} {
			jobs, err := store.ListJobsByState(ctx, state)
			if err != nil {
				return err
			}
			candidates = append(candidates, jobs...)
		}
		cutoff := time.Now().UTC().Add(-stuckQueuedThreshold).UnixMilli()
		var stuck []dashboard.HealthStuckJob
		for _, j := range candidates {
			since, isStuck := healthStuckSince(j, cutoff)
			if !isStuck {
				continue
			}
			payload, _ := workflow.DecodeJobPayload(j)
			// Mirror loadStuckReason but reuse the preloaded resource locks
			// (per-job events are inherently per-job and stay).
			reason := ""
			if events, eerr := store.ListJobEvents(ctx, j.ID); eerr == nil {
				ev, ok := latestReasonEvent(events)
				reason = deriveStuckReason(j, ev, ok, resourceLocks).Reason
			}
			stuck = append(stuck, dashboard.HealthStuckJob{
				ID:     j.ID,
				Title:  jobTitle(payload, j),
				Agent:  strings.TrimSpace(j.Agent),
				Repo:   strings.TrimSpace(payload.Repo),
				State:  string(mapNodeState(j.State)),
				Reason: reason,
				Since:  since,
			})
		}

		// Wedged jobs oldest-first (id tie-break).
		sort.SliceStable(stuck, func(i, j int) bool {
			if stuck[i].Since != stuck[j].Since {
				return stuck[i].Since < stuck[j].Since
			}
			return stuck[i].ID < stuck[j].ID
		})
		out.Stuck = stuck

		// Recent failures newest-first (id tie-break), capped. The store orders
		// by the stored timestamp text, which is the same order as its parsed
		// time because every jobs timestamp is CURRENT_TIMESTAMP-formatted.
		failedRows, err := store.ListDashboardRecentFailures(ctx, dashboardHealthRecentFailures)
		if err != nil {
			return err
		}
		failures := make([]dashboard.HealthFailure, 0, len(failedRows))
		for _, row := range failedRows {
			at := parseJobTimeMillis(row.UpdatedAt)
			if at == 0 {
				at = parseJobTimeMillis(row.CreatedAt)
			}
			failures = append(failures, dashboard.HealthFailure{
				ID:    row.ID,
				Title: jobTitle(workflow.JobPayload{Instructions: row.Instructions}, db.Job{ID: row.ID}),
				Agent: strings.TrimSpace(row.Agent),
				Repo:  strings.TrimSpace(row.Repo),
				At:    at,
			})
		}
		out.RecentFailures = failures

		// Non-branch resource locks (runtime sessions etc.), oldest acquisition
		// first — mapped from the slice preloaded before the stuck-job loop.
		rlocks := make([]dashboard.HealthResourceLock, 0, len(resourceLocks))
		for _, l := range resourceLocks {
			rlocks = append(rlocks, dashboard.HealthResourceLock{
				Key:        l.ResourceKey,
				Owner:      strings.TrimSpace(l.OwnerJobID),
				AcquiredAt: parseJobTimeMillis(l.AcquiredAt),
				ExpiresAt:  parseJobTimeMillis(l.ExpiresAt),
			})
		}
		sort.SliceStable(rlocks, func(i, j int) bool {
			if rlocks[i].AcquiredAt != rlocks[j].AcquiredAt {
				return rlocks[i].AcquiredAt < rlocks[j].AcquiredAt
			}
			return rlocks[i].Key < rlocks[j].Key
		})
		out.ResourceLocks = rlocks
		return nil
	})

	probes.Wait()

	// What the daemon PROCESS is running: its recorded build. A daemon started by
	// an older gitmoot recorded none, so its running build remains unknown. The
	// binary now sitting at its path may have been replaced and cannot answer for
	// the already-running process (#1003).
	out.Daemon.Version = recordedDaemonVersion

	// What an update would replace: the binary on disk. Keeping the badge
	// on-disk-relative is what stops it from sticking on after `gitmoot update`
	// until an unrelated daemon restart happens to clear it.
	out.Update = updateInfo
	badgeVersion := onDiskVersion
	if badgeVersion == "" {
		badgeVersion = out.Daemon.Version
	}
	if out.Update != nil && badgeVersion != "" {
		// The update check ran against this serving process's OWN compiled
		// version, which need not be the deployed binary at all — so make both the
		// displayed Current AND the availability verdict relative to the binary on
		// disk, otherwise a divergent-binary deployment reports the wrong badge.
		out.Update.Current = badgeVersion
		out.Update.UpdateAvailable = out.Update.Latest != "" && !sameDaemonVersion(badgeVersion, out.Update.Latest)
	}
	return out, err
}

// daemonVersionTimeout and updateCheckTimeout bound the two slow Health probes.
// They run concurrently, so the health endpoint's cold-cache wall-clock is the
// max of the two (< 5s), never their sum.
const (
	daemonVersionTimeout = 3 * time.Second
	updateCheckTimeout   = 4 * time.Second
	// updateSuccessTTL/updateFailureTTL age the cached update check: a good result
	// is trusted for an hour; a failed refresh is retried after ten minutes (the
	// last good result is served meanwhile).
	updateSuccessTTL = time.Hour
	updateFailureTTL = 10 * time.Minute
)

// resolveOnDiskVersion returns the version of the binary NOW SITTING at the
// daemon's executable path — what a restart (or an update) would pick up, which
// is not necessarily what the daemon process is running. It execs
// "<executable> version --json" (preferred) or the plain-text form under a hard
// timeout, and caches by executable path+mtime so SEQUENTIAL 12s health polls
// never re-exec an unchanged binary (there is no singleflight, so concurrent
// cold requests may each exec once). Only a regular, executable, existing file
// is ever run. Returns "" when it cannot be determined (fail-open).
func (d *webDataSource) resolveOnDiskVersion(ctx context.Context, executable string) string {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return ""
	}
	info, err := os.Stat(executable)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	key := fmt.Sprintf("%s\x00%d", executable, info.ModTime().UnixNano())

	d.mu.Lock()
	if d.daemonVersionKey == key {
		v := d.daemonVersion
		d.mu.Unlock()
		return v
	}
	d.mu.Unlock()

	version, _ := execBinaryBuildFn(ctx, executable)

	d.mu.Lock()
	d.daemonVersionKey = key
	d.daemonVersion = version
	d.mu.Unlock()
	return version
}

// execBinaryBuildFn is the seam the on-disk build probe goes through, so tests
// can choose what the binary at a path reports without exec'ing anything (the
// same pattern as updateCheckFn). The real exec path stays covered by
// TestWebDataSourceDaemonVersionCache / ...TextFallback.
var execBinaryBuildFn = execBinaryBuild

// execBinaryBuild asks the binary AT THIS PATH what build it is — which is what a
// daemon restart would load, and therefore the thing worth comparing the running
// daemon against. The plain-text fallback yields no commit, so callers must treat
// an empty commit as unknown rather than as "no commit".
func execBinaryBuild(ctx context.Context, executable string) (version, commit string) {
	// Both attempts share ONE budget so the whole probe is bounded by
	// daemonVersionTimeout, never 2× it (a wedged binary must not stack).
	ctx, cancel := context.WithTimeout(ctx, daemonVersionTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, executable, "version", "--json").Output(); err == nil {
		var parsed struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		}
		if json.Unmarshal(out, &parsed) == nil {
			if v := strings.TrimSpace(parsed.Version); v != "" {
				return v, strings.TrimSpace(parsed.Commit)
			}
		}
	}
	out, err := exec.CommandContext(ctx, executable, "version").Output()
	if err != nil {
		return "", ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	return strings.TrimSpace(strings.TrimPrefix(line, "gitmoot")), ""
}

// updateCheckFn is the seam the Health update check goes through so tests can
// stub the GitHub release lookup without a network. It defaults to update.Check
// against the default repo.
var updateCheckFn = func(ctx context.Context, current buildinfo.Info, executable string) (update.CheckResult, error) {
	return update.Check(ctx, update.GhReleaseClient{}, update.DefaultRepo, current, "", "", executable)
}

// checkUpdate returns the daemon-binary update check for the Health page, served
// from a TTL cache (updateSuccessTTL on success, updateFailureTTL on failure) so
// SEQUENTIAL 12s polls never re-hit GitHub (there is no singleflight, so
// concurrent cold requests may each fetch once) and a fresh success is reused for
// an hour.
// It is fail-open and never blocks past updateCheckTimeout: on a failed refresh
// it serves the last good result (nil if there was none), and a "no release"
// answer is cached as a definite nil (no data). The returned pointer is always a
// fresh copy — the internal cached value is never handed out — so callers may
// mutate it (e.g. to override Current) safely.
func (d *webDataSource) checkUpdate(ctx context.Context, executable string) *dashboard.HealthUpdate {
	d.mu.Lock()
	ttl := updateFailureTTL
	if d.updateOK {
		ttl = updateSuccessTTL
	}
	if !d.updateFetchedAt.IsZero() && time.Since(d.updateFetchedAt) < ttl {
		cached := cloneUpdate(d.updateResult)
		d.mu.Unlock()
		return cached
	}
	d.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	res, err := updateCheckFn(checkCtx, buildinfo.Current(), executable)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.updateFetchedAt = time.Now()
	if err != nil {
		// Fail-open: keep the last good result and re-try after updateFailureTTL.
		d.updateOK = false
		return cloneUpdate(d.updateResult)
	}
	d.updateOK = true
	d.updateResult = buildHealthUpdate(res, d.updateFetchedAt)
	return cloneUpdate(d.updateResult)
}

// sameDaemonVersion reports whether two version strings denote the same release,
// ignoring a leading "v" and surrounding whitespace; both must be non-empty (an
// unknown version never counts as "same"). It mirrors update.sameVersion (which
// is unexported) so Health can make the update verdict daemon-relative.
func sameDaemonVersion(a, b string) bool {
	a = strings.TrimPrefix(strings.TrimSpace(a), "v")
	b = strings.TrimPrefix(strings.TrimSpace(b), "v")
	return a != "" && b != "" && a == b
}

// buildHealthUpdate maps a settled update-check result onto the HealthUpdate
// contract, or nil when there is no usable data ("no release" / no latest tag) so
// the field is omitted entirely. UpdateAvailable is true only when a real newer
// release exists (!UpToDate, with a latest tag). Current defaults to the check's
// current version; the caller overrides it with the daemon's version.
func buildHealthUpdate(res update.CheckResult, at time.Time) *dashboard.HealthUpdate {
	if res.NoRelease {
		return nil
	}
	latest := strings.TrimSpace(res.LatestVersion)
	if latest == "" {
		return nil
	}
	return &dashboard.HealthUpdate{
		Current:         strings.TrimSpace(res.CurrentVersion),
		Latest:          latest,
		ReleaseURL:      strings.TrimSpace(res.ReleaseURL),
		UpdateAvailable: !res.UpToDate,
		CheckedAt:       at.UnixMilli(),
	}
}

// cloneUpdate returns a shallow copy of a HealthUpdate (or nil), so the cached
// value is never aliased into a response the caller may mutate.
func cloneUpdate(u *dashboard.HealthUpdate) *dashboard.HealthUpdate {
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

// healthStuckSince reports whether a job is wedged for the Health page and its
// "since" epoch-ms timestamp (UpdatedAt, else CreatedAt). A blocked job is always
// stuck; a queued job is stuck once its since falls at/behind cutoffMillis (the
// 10-minute threshold). Any other state is never stuck. Pure, so the threshold is
// unit-tested directly.
func healthStuckSince(job db.Job, cutoffMillis int64) (since int64, stuck bool) {
	since = parseJobTimeMillis(job.UpdatedAt)
	if since == 0 {
		since = parseJobTimeMillis(job.CreatedAt)
	}
	switch mapNodeState(job.State) {
	case "blocked":
		return since, true
	case "queued":
		if since > 0 && since <= cutoffMillis {
			return since, true
		}
	}
	return since, false
}

// agentRuntimeMap builds a name->runtime lookup from the agent registry so a
// node can report its runtime (omp|shell, or a removed name). A lookup error yields
// an empty map; ephemeral/delegated agents absent from the registry fall back to
// their payload's ephemeral spec in buildDashboardNode.
func agentRuntimeMap(ctx context.Context, store *db.Store) map[string]string {
	out := map[string]string{}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return out
	}
	for _, a := range agents {
		out[a.Name] = a.Runtime
	}
	return out
}

// buildDashboardNode maps one gitmoot job (plus its events and resolved deps)
// into a dashboard.Node. It is pure so it can be unit-tested directly.
func buildDashboardNode(job db.Job, payload workflow.JobPayload, events []db.JobEvent, runtimeByAgent map[string]string, deps []string, action string) dashboard.Node {
	node := dashboard.Node{
		ID:       job.ID,
		ParentID: strings.TrimSpace(job.ParentJobID),
		Deps:     deps,
		Title:    nodeTitle(payload, job, action),
		Agent:    job.Agent,
		Runtime:  resolveJobRuntime(job, payload, runtimeByAgent),
		Model:    strings.TrimSpace(job.Model),
		State:    mapNodeState(job.State),
		Depth:    job.DelegationDepth,
		Events:   []dashboard.Event{},
	}
	if payload.PullRequest > 0 && strings.TrimSpace(payload.Repo) != "" {
		node.PRURL = fmt.Sprintf("https://github.com/%s/pull/%d", payload.Repo, payload.PullRequest)
	}
	node.Prompt = strings.TrimSpace(payload.Instructions)
	if payload.Result != nil && strings.TrimSpace(payload.Result.Summary) != "" {
		node.Output = strings.TrimSpace(payload.Result.Summary)
	} else if len(payload.RawOutputs) > 0 {
		node.Output = strings.TrimSpace(payload.RawOutputs[len(payload.RawOutputs)-1])
	}
	if t := parseJobTimeMillis(job.CreatedAt); t > 0 {
		node.StartedAt = t
	}
	// EndedAt uses FINAL (resumability) semantics: a blocked job is deliberately
	// NOT stamped with an end time because it can resume via RetryJob (#632).
	if t := parseJobTimeMillis(job.UpdatedAt); t > 0 && workflow.IsFinalJobState(strings.TrimSpace(job.State)) {
		node.EndedAt = t
	}
	// Event.T is epoch millis from the event's created_at when the row carries
	// one, so the UI can order a node's timeline by real wall-clock time. When a
	// timestamp is absent/unparseable we fall back to the event id ordering
	// (ListJobEvents ORDER BY id) as a 1-based monotonic sequence.
	for i, e := range events {
		t := parseJobTimeMillis(e.CreatedAt)
		if t <= 0 {
			t = int64(i + 1)
		}
		node.Events = append(node.Events, dashboard.Event{T: t, Label: eventLabel(e)})
	}
	return node
}

// resolveDelegationEdges returns a historical delegation child's sibling-job-id
// deps (from its own payload's recorded Deps) and its action (the job type). A
// non-delegation job yields no deps.
func resolveDelegationEdges(job db.Job, payloadByID map[string]workflow.JobPayload, childrenByParent map[string][]db.Job) ([]string, string) {
	pj := strings.TrimSpace(job.ParentJobID)
	if pj == "" || strings.TrimSpace(job.DelegationID) == "" {
		return nil, ""
	}
	delegToJob := delegationJobIDs(childrenByParent[pj])
	var deps []string
	for _, dep := range payloadByID[job.ID].Deps {
		if id := delegToJob[strings.TrimSpace(dep)]; id != "" {
			deps = append(deps, id)
		}
	}
	return deps, job.Type
}

// delegationJobIDs maps each child's delegation id to its job id so declared
// delegation deps (which reference delegation ids) resolve to sibling node ids.
func delegationJobIDs(siblings []db.Job) map[string]string {
	m := map[string]string{}
	for _, s := range siblings {
		if d := strings.TrimSpace(s.DelegationID); d != "" {
			m[d] = s.ID
		}
	}
	return m
}

// jobRootID walks a job's parent chain to the originating job (empty
// ParentJobID), which identifies its run. Cycle-safe: a repeated id stops the
// walk. ListJobs does not populate the denormalized RootID column, so the run is
// derived from ParentJobID here rather than read off the row.
func jobRootID(jobByID map[string]db.Job, id string) string {
	seen := map[string]bool{}
	cur := id
	for {
		job, ok := jobByID[cur]
		if !ok {
			return cur
		}
		parent := strings.TrimSpace(job.ParentJobID)
		if parent == "" || seen[cur] {
			return cur
		}
		seen[cur] = true
		cur = parent
	}
}

// aggregateRunState collapses a run's job states into a single run state,
// preferring live work (running > queued) then problems (failed > blocked) then
// success, so an active run never reads as done.
func aggregateRunState(states []string) dashboard.NodeState {
	var running, queued, failed, blocked, succeeded, cancelled bool
	for _, s := range states {
		switch strings.TrimSpace(s) {
		case "running":
			running = true
		case "queued":
			queued = true
		case "failed":
			failed = true
		case "blocked":
			blocked = true
		case "succeeded":
			succeeded = true
		case "cancelled":
			cancelled = true
		}
	}
	switch {
	case running:
		return "running"
	case queued:
		return "queued"
	case failed:
		return "failed"
	case blocked:
		return "blocked"
	case succeeded:
		return "succeeded"
	case cancelled:
		return "cancelled"
	default:
		return "queued"
	}
}

// mapNodeState maps a gitmoot job state to a dashboard.NodeState. The two
// vocabularies already coincide (queued/running/succeeded/failed/blocked/
// cancelled); an empty/unknown state defaults to queued.
func mapNodeState(state string) dashboard.NodeState {
	switch strings.TrimSpace(state) {
	case "":
		return "queued"
	default:
		return dashboard.NodeState(strings.TrimSpace(state))
	}
}

// nodeTitle picks the most descriptive title available so a card reads as a
// task rather than a bare action. Preference order: an explicit task title; a
// humanized delegation id ("task-3-pairing-agent-auth" -> "Task 3: pairing
// agent auth"); the first non-empty line of the job's instructions (capped);
// then the existing action / job-type / id fallback. Deterministic and safe on
// empty inputs.
func nodeTitle(payload workflow.JobPayload, job db.Job, action string) string {
	if t := strings.TrimSpace(payload.TaskTitle); t != "" {
		return t
	}
	if t := humanizeDelegationID(job.DelegationID); t != "" {
		return t
	}
	if t := firstInstructionLine(payload.Instructions); t != "" {
		return t
	}
	if a := strings.TrimSpace(action); a != "" {
		return a
	}
	if t := strings.TrimSpace(job.Type); t != "" {
		return t
	}
	return job.ID
}

// humanizeDelegationID turns a slug-style delegation id into a readable title.
// A leading "task-<n>" segment becomes "Task <n>: <rest>"; otherwise the words
// (split on '-'/'_') are joined with spaces and the first letter is upcased.
// Returns "" for an empty id so nodeTitle can fall through.
func humanizeDelegationID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	parts := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	if len(parts) == 0 {
		return ""
	}
	if len(parts) >= 2 && strings.EqualFold(parts[0], "task") && isAllDigits(parts[1]) {
		head := "Task " + parts[1]
		rest := strings.Join(parts[2:], " ")
		if rest == "" {
			return head
		}
		return head + ": " + rest
	}
	return capitalizeFirst(strings.Join(parts, " "))
}

// firstInstructionLine returns the first non-empty, trimmed line of an
// instructions blob, capped to ~60 runes (appending an ellipsis when cut), so a
// title stays a single readable clause. Returns "" when there is no content.
func firstInstructionLine(instructions string) string {
	for _, line := range strings.Split(instructions, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const maxRunes = 60
		runes := []rune(line)
		if len(runes) > maxRunes {
			return strings.TrimSpace(string(runes[:maxRunes])) + "…"
		}
		return line
	}
	return ""
}

// parseRunKindAgent extracts a run's entrypoint kind (ask/review/implement/
// orchestrate/goal) and coordinator/agent name from the root job id, which the
// engine mints as "<origin>-<kind>-<agent...>-<hash>" (origin is local|gh, hash
// is a 12+ hex suffix; the agent segment may itself contain hyphens). IDs that
// don't fit that shape (e.g. task-rooted runs) fall back to the root job's Type
// and Agent columns.
// knownRunKinds is the set of single-token run entrypoints the id parser trusts
// as parts[1]. Multi-token internal actions are deliberately absent, so their ids
// fall through to the root job's Type/Agent columns instead of being mis-split.
// Retired kinds (historical "produce" rows) take the same fallback, which yields
// the identical kind and agent from the columns.
var knownRunKinds = map[string]bool{"ask": true, "review": true, "implement": true, "orchestrate": true, "goal": true}

func parseRunKindAgent(rootID string, root db.Job) (kind, agent string) {
	parts := strings.Split(strings.TrimSpace(rootID), "-")
	if len(parts) >= 4 && (parts[0] == "local" || parts[0] == "gh") && isHexHash(parts[len(parts)-1]) && knownRunKinds[parts[1]] {
		kind = parts[1]
		agent = strings.Join(parts[2:len(parts)-1], "-")
	}
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(root.Type))
	}
	if agent == "" {
		agent = strings.TrimSpace(root.Agent)
	}
	return kind, agent
}

// isHexHash reports whether s is a run-id hash suffix: 12+ lowercase hex digits.
func isHexHash(s string) bool {
	if len(s) < 12 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// isAllDigits reports whether s is non-empty and every rune is an ASCII digit.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// capitalizeFirst upcases the first rune of s, leaving the rest untouched.
func capitalizeFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// runTitle titles a whole run from its root job.
func runTitle(payload workflow.JobPayload, root db.Job) string {
	if t := strings.TrimSpace(payload.TaskTitle); t != "" {
		return t
	}
	if a := strings.TrimSpace(root.Agent); a != "" {
		if t := strings.TrimSpace(root.Type); t != "" {
			return a + " / " + t
		}
		return a
	}
	return root.ID
}

// eventLabel renders a job event as a concise timeline label.
func eventLabel(e db.JobEvent) string {
	kind := strings.TrimSpace(e.Kind)
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		return kind
	}
	maxMsg := 120
	if kind == "failed" {
		maxMsg = 600
	}
	if len(msg) > maxMsg {
		msg = msg[:maxMsg] + "…"
	}
	if kind == "" {
		return msg
	}
	return kind + ": " + msg
}

// parseJobTimeMillis parses a stored job timestamp into epoch milliseconds
// (the dashboard contract's timestamp unit), tolerating the SQLite
// CURRENT_TIMESTAMP form and the ISO forms other update paths write. Returns 0
// when absent or unparseable.
func parseJobTimeMillis(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.000000000Z",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC().UnixMilli()
		}
	}
	return 0
}
