package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// runDashboardWeb serves the read-only web dashboard: a live orchestration-DAG
// UI backed by a dashboard.DataSource over the local store. It is strictly
// read-only (it never mutates workflow state) and is never invoked from the
// daemon path — it is a foreground `gitmoot dashboard --web` server that blocks
// until interrupted, mirroring the --watch loop's signal handling.
func runDashboardWeb(home, addr string, stdout, stderr io.Writer) int {
	if _, err := initializedPaths(home); err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}
	ds := &webDataSource{home: home}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}
	srv := &http.Server{Handler: dashboard.Serve(ds)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

// webDataSource implements dashboard.DataSource over a local Gitmoot home. It
// reuses the existing read paths only — buildDashboardSnapshot for the run list
// and the same store APIs the dashboard TUI reads (ListJobs / ListJobEvents /
// GetJob / workflow.ParseJobPayload) — so it never duplicates a store query or
// touches workflow state.
type webDataSource struct {
	home string
}

var _ dashboard.DataSource = (*webDataSource)(nil)

// Runs lists every orchestration run (delegation tree) rooted at an originating
// job, newest activity first. It reuses buildDashboardSnapshot so the run list
// is assembled from the same read path the plain/TUI dashboard uses.
func (d *webDataSource) Runs(ctx context.Context) ([]dashboard.RunSummary, error) {
	paths, err := initializedPaths(d.home)
	if err != nil {
		return nil, err
	}
	snapshot, err := buildDashboardSnapshot(d.home, paths)
	if err != nil {
		return nil, err
	}
	jobs := make([]db.Job, 0, len(snapshot.jobRows))
	for _, row := range snapshot.jobRows {
		jobs = append(jobs, row.Job)
	}
	return summarizeRuns(jobs), nil
}

// State returns a snapshot of one run's delegation graph. An empty runID selects
// the most-recent active (else most-recent) run. Nodes come from ListJobs scoped
// to the run's tree; edges are ParentID (delegation parentage) plus Deps (a
// child's declared delegation deps resolved to its sibling job IDs).
func (d *webDataSource) State(ctx context.Context, runID string) (dashboard.State, error) {
	out := dashboard.State{Nodes: []dashboard.Node{}}
	err := withStore(d.home, func(store *db.Store) error {
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return nil
		}
		jobByID := make(map[string]db.Job, len(jobs))
		for _, j := range jobs {
			jobByID[j.ID] = j
		}

		requested := strings.TrimSpace(runID)
		target := requested
		if target == "" {
			target = mostRecentRunRoot(jobs, jobByID)
		} else {
			target = jobRootID(jobByID, target)
		}
		if _, ok := jobByID[target]; target == "" || !ok {
			// An explicitly requested run that does not resolve to a real job is
			// a 404; an empty request against an empty store is just an empty
			// snapshot.
			if requested != "" {
				return dashboard.ErrRunNotFound
			}
			return nil
		}

		runtimeByAgent := agentRuntimeMap(ctx, store)

		// Scope to the run's tree and parse each job's payload once.
		var tree []db.Job
		payloadByID := make(map[string]workflow.JobPayload)
		childrenByParent := map[string][]db.Job{}
		for _, j := range jobs {
			if jobRootID(jobByID, j.ID) != target {
				continue
			}
			tree = append(tree, j)
			p, _ := workflow.ParseJobPayload(j.Payload)
			payloadByID[j.ID] = p
			if pj := strings.TrimSpace(j.ParentJobID); pj != "" {
				childrenByParent[pj] = append(childrenByParent[pj], j)
			}
		}

		out.RunID = target
		out.Title = runTitle(payloadByID[target], jobByID[target])
		for _, j := range tree {
			events, _ := store.ListJobEvents(ctx, j.ID)
			deps, action := resolveDelegationEdges(j, payloadByID, childrenByParent)
			out.Nodes = append(out.Nodes, buildDashboardNode(j, payloadByID[j.ID], events, runtimeByAgent, deps, action))
		}
		return nil
	})
	return out, err
}

// Job returns a single node by job id, resolving its delegation deps against the
// parent's settled result and sibling children (the same mapping State uses).
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
		payload, _ := workflow.ParseJobPayload(job.Payload)
		events, _ := store.ListJobEvents(ctx, jobID)
		runtimeByAgent := agentRuntimeMap(ctx, store)

		var deps []string
		var action string
		if pj := strings.TrimSpace(job.ParentJobID); pj != "" && strings.TrimSpace(job.DelegationID) != "" {
			if parent, perr := store.GetJob(ctx, pj); perr == nil {
				pp, _ := workflow.ParseJobPayload(parent.Payload)
				meta := parentDelegMeta(pp)[job.DelegationID]
				action = meta.action
				if siblings, serr := store.ListJobsByParent(ctx, pj); serr == nil {
					delegToJob := delegationJobIDs(siblings)
					for _, dep := range meta.deps {
						if id := delegToJob[strings.TrimSpace(dep)]; id != "" {
							deps = append(deps, id)
						}
					}
				}
			}
		}
		node = buildDashboardNode(job, payload, events, runtimeByAgent, deps, action)
		return nil
	})
	return node, err
}

// Subscribe polls State for the requested run and pushes a fresh snapshot to the
// returned channel whenever it changes (plus one initial snapshot). It is a
// read-only poller — no store writes — and stops when the caller invokes the
// returned cancel func or the parent context is cancelled.
func (d *webDataSource) Subscribe(ctx context.Context, runID string) (<-chan dashboard.State, func(), error) {
	ch := make(chan dashboard.State, 1)
	pollCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var last string
		push := func() {
			state, err := d.State(pollCtx, runID)
			if err != nil {
				return
			}
			key := stateFingerprint(state)
			if key == last {
				return
			}
			last = key
			select {
			case ch <- state:
			case <-pollCtx.Done():
			}
		}
		push()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
				push()
			}
		}
	}()
	return ch, cancel, nil
}

// agentRuntimeMap builds a name->runtime lookup from the agent registry so a
// node can report its runtime (codex|claude|kimi|shell). A lookup error yields
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
		Runtime:  runtimeByAgent[job.Agent],
		Model:    strings.TrimSpace(payload.Model),
		State:    mapNodeState(job.State),
		Depth:    job.DelegationDepth,
		Events:   []dashboard.Event{},
	}
	if node.Runtime == "" && payload.Ephemeral != nil {
		node.Runtime = strings.TrimSpace(payload.Ephemeral.Runtime)
	}
	if node.Model == "" && payload.Ephemeral != nil {
		node.Model = strings.TrimSpace(payload.Ephemeral.Model)
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
	if t := parseJobTimeMillis(job.UpdatedAt); t > 0 && isTerminalJobState(job.State) {
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

// resolveDelegationEdges returns a child job's sibling-job-id deps and its
// delegation action, read from the parent coordinator's settled result. A
// non-delegation job (originating coordinator / continuation) yields no deps.
func resolveDelegationEdges(job db.Job, payloadByID map[string]workflow.JobPayload, childrenByParent map[string][]db.Job) ([]string, string) {
	pj := strings.TrimSpace(job.ParentJobID)
	delegID := strings.TrimSpace(job.DelegationID)
	if pj == "" || delegID == "" {
		return nil, ""
	}
	meta := parentDelegMeta(payloadByID[pj])[delegID]
	delegToJob := delegationJobIDs(childrenByParent[pj])
	var deps []string
	for _, dep := range meta.deps {
		if id := delegToJob[strings.TrimSpace(dep)]; id != "" {
			deps = append(deps, id)
		}
	}
	return deps, meta.action
}

// delegMeta is a delegation's declared action and deps, read off the parent's
// settled result (the same source buildDelegationTree uses).
type delegMeta struct {
	action string
	deps   []string
}

// parentDelegMeta indexes a coordinator's settled delegations by delegation id.
func parentDelegMeta(parent workflow.JobPayload) map[string]delegMeta {
	m := map[string]delegMeta{}
	if parent.Result != nil {
		for _, dgn := range parent.Result.Delegations {
			m[dgn.ID] = delegMeta{action: dgn.Action, deps: dgn.Deps}
		}
	}
	return m
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

// summarizeRuns groups jobs into delegation trees (rooted at each originating
// job) and returns one RunSummary per tree, newest activity first. Pure so it is
// unit-tested directly.
func summarizeRuns(jobs []db.Job) []dashboard.RunSummary {
	jobByID := make(map[string]db.Job, len(jobs))
	for _, j := range jobs {
		jobByID[j.ID] = j
	}
	type agg struct {
		updated  string
		created  string
		states   []string
		maxDepth int
		done     int
		root     db.Job
	}
	byRoot := map[string]*agg{}
	var order []string
	for _, j := range jobs {
		root := jobRootID(jobByID, j.ID)
		a := byRoot[root]
		if a == nil {
			a = &agg{}
			byRoot[root] = a
			order = append(order, root)
		}
		a.states = append(a.states, j.State)
		if j.UpdatedAt > a.updated {
			a.updated = j.UpdatedAt
		}
		if a.created == "" || (j.CreatedAt != "" && j.CreatedAt < a.created) {
			a.created = j.CreatedAt
		}
		if j.DelegationDepth > a.maxDepth {
			a.maxDepth = j.DelegationDepth
		}
		if isTerminalJobState(j.State) {
			a.done++
		}
		if j.ID == root {
			a.root = j
		}
	}
	out := make([]dashboard.RunSummary, 0, len(order))
	for _, root := range order {
		a := byRoot[root]
		payload, _ := workflow.ParseJobPayload(a.root.Payload)
		nodeCount := len(a.states)
		// An orchestration is a genuine delegation tree: a coordinator delegated to
		// children, so some job sits below the root (DelegationDepth > 0). Native
		// review-fanout jobs and ask continuations share the root but add no
		// delegation depth, so counting nodes alone would mislabel a one-shot
		// implement/ask (which spawns review jobs) as an orchestration.
		significance := "one-shot"
		if a.maxDepth > 0 {
			significance = "orchestration"
		}
		kind, agent := parseRunKindAgent(root, a.root)
		started := parseJobTimeMillis(a.created)
		updated := parseJobTimeMillis(a.updated)
		var duration int64
		if started > 0 && updated > started {
			duration = updated - started
		}
		out = append(out, dashboard.RunSummary{
			RunID:        root,
			Title:        runTitle(payload, a.root),
			State:        aggregateRunState(a.states),
			Kind:         kind,
			Significance: significance,
			Agent:        agent,
			Repo:         strings.TrimSpace(payload.Repo),
			PR:           payload.PullRequest,
			NodeCount:    nodeCount,
			Depth:        a.maxDepth + 1, // delegation levels (root = level 1)
			DoneCount:    a.done,
			Snippet:      firstInstructionLine(payload.Instructions),
			Started:      started,
			Updated:      updated,
			Duration:     duration,
		})
	}
	// Active (non-terminal) runs sort first, then most-recent activity, then id
	// for a deterministic tiebreak. The list is capped so a long history never
	// swamps the run picker; the cap keeps the freshest/active runs.
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := runStateActive(out[i].State), runStateActive(out[j].State)
		if ai != aj {
			return ai
		}
		if out[i].Updated != out[j].Updated {
			return out[i].Updated > out[j].Updated
		}
		return out[i].RunID < out[j].RunID
	})
	if len(out) > maxRunSummaries {
		out = out[:maxRunSummaries]
	}
	return out
}

// maxRunSummaries caps the run list returned by Runs()/summarizeRuns so the web
// dashboard's run picker stays bounded on a home with a long orchestration
// history.
const maxRunSummaries = 60

// runStateActive reports whether an aggregated run state is still live (not a
// settled terminal state), so active runs can be surfaced ahead of finished
// ones in the run list.
func runStateActive(state dashboard.NodeState) bool {
	switch string(state) {
	case "succeeded", "failed", "cancelled":
		return false
	default:
		return true
	}
}

// mostRecentRunRoot returns the run root to show when no run is requested: the
// most-recently-updated run that has live (queued/running) work, else the
// most-recently-updated run overall. Deterministic on ties (root id).
func mostRecentRunRoot(jobs []db.Job, jobByID map[string]db.Job) string {
	type agg struct {
		updated string
		active  bool
	}
	byRoot := map[string]*agg{}
	for _, j := range jobs {
		root := jobRootID(jobByID, j.ID)
		a := byRoot[root]
		if a == nil {
			a = &agg{}
			byRoot[root] = a
		}
		if j.UpdatedAt > a.updated {
			a.updated = j.UpdatedAt
		}
		if activityJobActive(j.State) {
			a.active = true
		}
	}
	roots := make([]string, 0, len(byRoot))
	for root := range byRoot {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	best := ""
	var bestA *agg
	for _, root := range roots {
		a := byRoot[root]
		if bestA == nil {
			best, bestA = root, a
			continue
		}
		if a.active != bestA.active {
			if a.active {
				best, bestA = root, a
			}
			continue
		}
		if a.updated > bestA.updated {
			best, bestA = root, a
		}
	}
	return best
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

// isTerminalJobState reports whether a job has settled (used to stamp EndedAt).
func isTerminalJobState(state string) bool {
	switch strings.TrimSpace(state) {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
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
// as parts[1]. Multi-token internal actions (e.g. "skillopt-train-candidate-
// review") are deliberately absent, so their ids fall through to the root job's
// Type/Agent columns instead of being mis-split (kind="skillopt", agent absorbs
// the rest).
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
	const maxMsg = 120
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

// stateFingerprint is a cheap change signal for the SSE poller: it changes
// whenever any node's identity, state, or event count changes.
func stateFingerprint(state dashboard.State) string {
	var b strings.Builder
	b.WriteString(state.RunID)
	for _, n := range state.Nodes {
		fmt.Fprintf(&b, "|%s:%s:%d", n.ID, n.State, len(n.Events))
	}
	return b.String()
}
