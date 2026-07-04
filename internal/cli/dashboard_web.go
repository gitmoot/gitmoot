package cli

import (
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
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/update"
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

	// mu guards the Health() caches below (Health can be called concurrently and
	// its resolvers run in goroutines). Everything else on the source is stateless
	// per call, so only the caches need protection.
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

// Jobs returns every job across every run, flattened into one JobSummary each,
// sorted newest-activity first. It is a single read-only ListJobs pass: each
// job's payload is parsed once for its title/repo/PR, its kind is derived the
// same way summarizeRuns derives a run's kind (from the id shape, else the Type
// column), its runtime is resolved through the agent registry with the ephemeral
// worker fallback, and its Run is its delegation-tree root. No cap — the Jobs
// page filters client-side.
func (d *webDataSource) Jobs(ctx context.Context) ([]dashboard.JobSummary, error) {
	out := []dashboard.JobSummary{}
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
		runtimeByAgent := agentRuntimeMap(ctx, store)

		out = make([]dashboard.JobSummary, 0, len(jobs))
		for _, j := range jobs {
			payload, _ := workflow.ParseJobPayload(j.Payload)
			// Kind mirrors summarizeRuns: parse the id's "<origin>-<kind>-<agent>-
			// <hash>" shape (root jobs), else fall back to the lowercased Type column
			// (delegation children, whose ids don't carry that shape).
			kind, _ := parseRunKindAgent(j.ID, j)
			started := parseJobTimeMillis(j.CreatedAt)
			updated := parseJobTimeMillis(j.UpdatedAt)
			var duration int64
			if started > 0 && updated > started {
				duration = updated - started
			}
			out = append(out, dashboard.JobSummary{
				ID:        j.ID,
				Title:     jobTitle(payload, j),
				Agent:     strings.TrimSpace(j.Agent),
				Runtime:   resolveJobRuntime(j, payload, runtimeByAgent),
				Repo:      strings.TrimSpace(payload.Repo),
				Kind:      kind,
				State:     mapNodeState(j.State),
				Depth:     j.DelegationDepth,
				Run:       jobRootID(jobByID, j.ID),
				PR:        payload.PullRequest,
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

// Agents lists the registered agents (one AgentSummary each) plus one synthetic
// rollup row for the fleet of ephemeral workers. It is a single read-only pass:
// ListAgents supplies the registered rows' identity/config, and the SAME
// ListJobs scan aggregates each agent's job counts and last-active time. Every
// job whose agent name carries the engine's "-ephemeral-" infix folds into the
// one rollup row (Ephemeral == true, blank Runtime). Registered rows sort
// most-recently-active first (never-active last, alphabetical); the ephemeral
// rollup is always last.
func (d *webDataSource) Agents(ctx context.Context) ([]dashboard.AgentSummary, error) {
	out := []dashboard.AgentSummary{}
	err := withStore(d.home, func(store *db.Store) error {
		agents, err := store.ListAgents(ctx)
		if err != nil {
			return err
		}
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			return err
		}

		// Aggregate per-agent job stats from the single ListJobs pass (shared with
		// Agent()). Ephemeral workers fold into one rollup.
		byAgent, ephemeral, hasEphemeral := aggregateAgentJobStats(jobs)

		out = make([]dashboard.AgentSummary, 0, len(agents)+1)
		for _, a := range agents {
			summary := newAgentSummary(a)
			if s := byAgent[a.Name]; s != nil {
				s.applyTo(&summary)
			}
			out = append(out, summary)
		}
		// Registered agents: most-recently-active first; never-active (LastActive
		// == 0) fall to the end, alphabetical. The ephemeral rollup is appended
		// after this sort so it is always last.
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].LastActive != out[j].LastActive {
				return out[i].LastActive > out[j].LastActive
			}
			return out[i].Name < out[j].Name
		})
		if hasEphemeral {
			rollup := dashboard.AgentSummary{Name: "ephemeral workers", Ephemeral: true}
			ephemeral.applyTo(&rollup)
			out = append(out, rollup)
		}
		return nil
	})
	return out, err
}

// Agent returns the click-through detail for a single agent by name: the same
// AgentSummary Agents() builds for that one row, plus its template (nil when the
// agent has no template or the template lookup fails — fail-open, never an error)
// and the template's version history newest-first. An unknown name maps to the
// dashboard's not-found sentinel (mirroring how Job() maps an unknown job id), so
// the API layer returns 404 rather than 500.
func (d *webDataSource) Agent(ctx context.Context, name string) (dashboard.AgentDetail, error) {
	detail := dashboard.AgentDetail{Versions: []dashboard.TemplateVersionInfo{}}
	err := withStore(d.home, func(store *db.Store) error {
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
		byAgent, _, _ := aggregateAgentJobStats(jobs)
		if s := byAgent[agent.Name]; s != nil {
			s.applyTo(&summary)
		}
		detail.AgentSummary = summary

		// Template + version history. Fail-open: a missing/broken template leaves the
		// detail's Template nil and Versions the initialized empty slice rather than
		// erroring the endpoint.
		if tid := strings.TrimSpace(agent.TemplateID); tid != "" {
			if tmpl, terr := store.GetAgentTemplate(ctx, tid); terr == nil {
				detail.Template = agentTemplateInfo(tmpl)
				if versions := agentTemplateVersions(ctx, store, tmpl); versions != nil {
					detail.Versions = versions
				}
			}
		}
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

// aggregateAgentJobStats folds a job slice into per-registered-agent tallies plus
// one rollup for the fleet of ephemeral workers (names carrying the "-ephemeral-"
// infix). It is the single aggregation both Agents() (which walks byAgent for
// every registered row and appends the ephemeral rollup) and Agent() (which picks
// one byAgent entry) share, so their counts can never diverge.
func aggregateAgentJobStats(jobs []db.Job) (byAgent map[string]*agentJobStat, ephemeral agentJobStat, hasEphemeral bool) {
	byAgent = map[string]*agentJobStat{}
	for _, j := range jobs {
		name := strings.TrimSpace(j.Agent)
		var s *agentJobStat
		if strings.Contains(name, "-ephemeral-") {
			s = &ephemeral
			hasEphemeral = true
		} else {
			s = byAgent[name]
			if s == nil {
				s = &agentJobStat{}
				byAgent[name] = s
			}
		}
		s.observe(j)
	}
	return byAgent, ephemeral, hasEphemeral
}

// agentTemplateInfo maps the store's AgentTemplate onto the dashboard's
// AgentTemplateInfo (identity + source/resolved provenance).
func agentTemplateInfo(tmpl db.AgentTemplate) *dashboard.AgentTemplateInfo {
	return &dashboard.AgentTemplateInfo{
		ID:             tmpl.ID,
		Name:           strings.TrimSpace(tmpl.Name),
		Description:    strings.TrimSpace(tmpl.Description),
		SourceRepo:     strings.TrimSpace(tmpl.SourceRepo),
		SourceRef:      strings.TrimSpace(tmpl.SourceRef),
		SourcePath:     strings.TrimSpace(tmpl.SourcePath),
		ResolvedCommit: strings.TrimSpace(tmpl.ResolvedCommit),
		// Content is the full resolved prompt body; passed verbatim (no trim) so the
		// detail view shows the exact template text an agent runs.
		Content: tmpl.Content,
	}
}

// agentTemplateVersions lists a template's version history newest-first (version
// number descending, id tie-break) and marks the version the template currently
// resolves to. Current is keyed on the template's current_version_id
// (tmpl.VersionID from GetAgentTemplate) — the version an agent pinned to the
// default "current" ref actually runs — NOT the latest_version_id, which only
// applies to an explicit "@latest" ref and can point at an unpromoted candidate.
// Timestamps go through parseJobTimeMillis (epoch ms, 0 when unknown). Returns nil
// on a lookup error so the caller can keep the detail's Versions empty (fail-open).
func agentTemplateVersions(ctx context.Context, store *db.Store, tmpl db.AgentTemplate) []dashboard.TemplateVersionInfo {
	rows, err := store.ListAgentTemplateVersions(ctx, tmpl.ID)
	if err != nil {
		return nil
	}
	currentID := strings.TrimSpace(tmpl.VersionID)
	out := make([]dashboard.TemplateVersionInfo, 0, len(rows))
	for _, v := range rows {
		out = append(out, dashboard.TemplateVersionInfo{
			ID:             v.ID,
			Number:         v.VersionNumber,
			State:          strings.TrimSpace(v.State),
			Name:           strings.TrimSpace(v.Name),
			Description:    strings.TrimSpace(v.Description),
			SourceRef:      strings.TrimSpace(v.SourceRef),
			ResolvedCommit: strings.TrimSpace(v.ResolvedCommit),
			CreatedAt:      parseJobTimeMillis(v.CreatedAt),
			PromotedAt:     parseJobTimeMillis(v.PromotedAt),
			CanarySample:   v.CanarySample,
			Current:        currentID != "" && v.ID == currentID,
			// Content is this version's full prompt body; passed verbatim (no trim).
			Content: v.Content,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Number != out[j].Number {
			return out[i].Number > out[j].Number
		}
		return out[i].ID > out[j].ID
	})
	return out
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

// resolveJobRuntime resolves a job's runtime family (codex|claude|kimi|shell)
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

// Graph returns the whole-history "galaxy" graph: a union of every job across
// every run in the store, plus synthetic repo/agent hub nodes that cluster the
// jobs. Links are delegation parentage, a per-parent-group sibling mesh (the
// density lever, capped so one huge fan-out can't dominate), and job->repo /
// job->agent spokes. A non-empty repo scopes the visible jobs (and their hubs)
// to that repository; Repos always lists the full distinct-repo set so the UI
// filter dropdown stays complete. Read-only; output is sorted for determinism.
func (d *webDataSource) Graph(ctx context.Context, repo string) (dashboard.Graph, error) {
	out := dashboard.Graph{Nodes: []dashboard.GraphNode{}, Links: []dashboard.GraphLink{}, Repos: []string{}}
	filter := strings.TrimSpace(repo)
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

		// Parse each job's payload once and record its repo/root.
		payloadByID := make(map[string]workflow.JobPayload, len(jobs))
		repoByID := make(map[string]string, len(jobs))
		allRepos := map[string]bool{}
		for _, j := range jobs {
			p, _ := workflow.ParseJobPayload(j.Payload)
			payloadByID[j.ID] = p
			r := strings.TrimSpace(p.Repo)
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

		// Job nodes + collect the hubs referenced by visible jobs.
		repoHubs := map[string]bool{}
		agentHubs := map[string]bool{}
		for _, j := range jobs {
			if !visible(j.ID) {
				continue
			}
			p := payloadByID[j.ID]
			r := repoByID[j.ID]
			agent := strings.TrimSpace(j.Agent)
			// Run points at the job's root. Under an active repo filter that root
			// may belong to a different repo and thus be absent from Nodes; fall
			// back to self-root so no node attribute references a missing node.
			run := jobRootID(jobByID, j.ID)
			if filter != "" && !visible(run) {
				run = j.ID
			}
			out.Nodes = append(out.Nodes, dashboard.GraphNode{
				ID:    j.ID,
				Type:  "job",
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
		}

		// Hub nodes (sorted, appended after the job nodes).
		hubRepos := sortedSetKeys(repoHubs)
		for _, r := range hubRepos {
			out.Nodes = append(out.Nodes, dashboard.GraphNode{ID: "repo::" + r, Type: "repo", Label: r, Repo: r})
		}
		for _, a := range sortedSetKeys(agentHubs) {
			out.Nodes = append(out.Nodes, dashboard.GraphNode{ID: "agent::" + a, Type: "agent", Label: a, Agent: a})
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
				out.Links = append(out.Links, dashboard.GraphLink{Source: j.ID, Target: "agent::" + a, Kind: "agent"})
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
		payload, _ := workflow.ParseJobPayload(j.Payload)

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

// Health returns the daemon liveness, fleet totals, held locks, wedged jobs and
// recent failures behind the Health page. The daemon block mirrors
// buildDashboardSnapshot (currentDaemonPID + daemon.json meta over d.home);
// everything else is a single read-only ListJobs pass plus the lock listings.
// Stuck reasons reuse loadStuckReason/deriveStuckReason unchanged.
func (d *webDataSource) Health(ctx context.Context) (dashboard.Health, error) {
	out := dashboard.Health{
		Locks:          []dashboard.HealthLock{},
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
	if pid, _, perr := currentDaemonPID(state); perr == nil && pid > 0 {
		out.Daemon.Running = true
		out.Daemon.PID = pid
		if meta, merr := readDaemonMeta(state); merr == nil {
			out.Daemon.StartedAt = parseJobTimeMillis(meta.StartedAt)
			executable = strings.TrimSpace(meta.Executable)
		}
	}

	// The daemon-version probe (execs the binary) and the update check (hits
	// GitHub) are the only slow parts of Health. Run them CONCURRENTLY — and with
	// the local store read — so a cold cache never serially stacks their 3s + 4s
	// timeouts past the health endpoint's latency budget. Both fail-open: a
	// resolution error leaves the field empty/nil, never an error. The update
	// check compares buildinfo.Current() (the exact documented shape); its
	// displayed Current is overridden with the running daemon's version below,
	// since that is the binary the operator actually runs.
	var probes sync.WaitGroup
	var daemonVersion string
	var updateInfo *dashboard.HealthUpdate
	probes.Add(2)
	go func() {
		defer probes.Done()
		daemonVersion = d.resolveDaemonVersion(ctx, executable)
	}()
	go func() {
		defer probes.Done()
		updateInfo = d.checkUpdate(ctx, executable)
	}()

	err = withStore(d.home, func(store *db.Store) error {
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			return err
		}

		// Non-branch resource locks (runtime sessions etc.) are listed ONCE and
		// shared: they feed both out.ResourceLocks below and every stuck job's
		// deriveStuckReason. Going through loadStuckReason instead would re-scan
		// the resource_locks table per stuck job on every 12s /api/health poll —
		// an N+1 that grows with the blocked-job count in unattended operation.
		resourceLocks, err := store.ListResourceLocks(ctx)
		if err != nil {
			return err
		}

		cutoff := time.Now().UTC().Add(-stuckQueuedThreshold).UnixMilli()
		var stuck []dashboard.HealthStuckJob
		var failures []dashboard.HealthFailure
		for _, j := range jobs {
			ns := mapNodeState(j.State)
			switch ns {
			case "queued":
				out.Totals.Queued++
			case "running":
				out.Totals.Running++
			case "blocked":
				out.Totals.Blocked++
			case "succeeded":
				out.Totals.Succeeded++
			case "failed":
				out.Totals.Failed++
			case "cancelled":
				out.Totals.Cancelled++
			}

			if since, isStuck := healthStuckSince(j, cutoff); isStuck {
				payload, _ := workflow.ParseJobPayload(j.Payload)
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
					State:  string(ns),
					Reason: reason,
					Since:  since,
				})
			}

			if ns == "failed" {
				payload, _ := workflow.ParseJobPayload(j.Payload)
				at := parseJobTimeMillis(j.UpdatedAt)
				if at == 0 {
					at = parseJobTimeMillis(j.CreatedAt)
				}
				failures = append(failures, dashboard.HealthFailure{
					ID:    j.ID,
					Title: jobTitle(payload, j),
					Agent: strings.TrimSpace(j.Agent),
					Repo:  strings.TrimSpace(payload.Repo),
					At:    at,
				})
			}
		}

		// Wedged jobs oldest-first (id tie-break).
		sort.SliceStable(stuck, func(i, j int) bool {
			if stuck[i].Since != stuck[j].Since {
				return stuck[i].Since < stuck[j].Since
			}
			return stuck[i].ID < stuck[j].ID
		})
		out.Stuck = stuck

		// Recent failures newest-first (id tie-break), capped at 10.
		sort.SliceStable(failures, func(i, j int) bool {
			if failures[i].At != failures[j].At {
				return failures[i].At > failures[j].At
			}
			return failures[i].ID > failures[j].ID
		})
		if len(failures) > 10 {
			failures = failures[:10]
		}
		out.RecentFailures = failures

		// Branch/checkout locks (all repos), oldest acquisition first.
		branchLocks, err := store.ListBranchLocksWithAge(ctx, "")
		if err != nil {
			return err
		}
		locks := make([]dashboard.HealthLock, 0, len(branchLocks))
		for _, l := range branchLocks {
			hl := dashboard.HealthLock{Repo: l.RepoFullName, Branch: l.Branch, Owner: l.Owner}
			if !l.CreatedAt.IsZero() {
				hl.AcquiredAt = l.CreatedAt.UnixMilli()
			}
			locks = append(locks, hl)
		}
		sort.SliceStable(locks, func(i, j int) bool {
			if locks[i].AcquiredAt != locks[j].AcquiredAt {
				return locks[i].AcquiredAt < locks[j].AcquiredAt
			}
			if locks[i].Repo != locks[j].Repo {
				return locks[i].Repo < locks[j].Repo
			}
			return locks[i].Branch < locks[j].Branch
		})
		out.Locks = locks

		// Non-branch resource locks (runtime sessions etc.), oldest acquisition
		// first — mapped from the slice preloaded before the jobs loop.
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

	// Join the concurrent probes. Prefer the running daemon's version for the
	// update check's displayed Current (that is what the operator runs); fall back
	// to the check's own current (buildinfo.Current().Version) when the daemon
	// version is unavailable.
	probes.Wait()
	out.Daemon.Version = daemonVersion
	out.Update = updateInfo
	if out.Update != nil && daemonVersion != "" {
		// The update check ran against this dashboard-web process's OWN compiled
		// version, but the operator runs the daemon binary — so make both the
		// displayed Current AND the availability verdict daemon-relative,
		// otherwise a divergent-binary deployment reports the wrong badge.
		out.Update.Current = daemonVersion
		out.Update.UpdateAvailable = out.Update.Latest != "" && !sameDaemonVersion(daemonVersion, out.Update.Latest)
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

// resolveDaemonVersion returns the running daemon binary's reported version, or
// "" when it cannot be determined (fail-open). It execs "<executable> version
// --json" (preferred) or the plain-text form, under a hard timeout, and caches
// the result keyed by the executable's path+mtime so SEQUENTIAL 12s health polls
// never re-exec an unchanged binary (there is no singleflight, so concurrent cold
// requests may each exec once). Only a regular, executable, existing file is ever
// run.
func (d *webDataSource) resolveDaemonVersion(ctx context.Context, executable string) string {
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

	version := execDaemonVersion(ctx, executable)

	d.mu.Lock()
	d.daemonVersionKey = key
	d.daemonVersion = version
	d.mu.Unlock()
	return version
}

// execDaemonVersion runs the binary's version subcommand with a hard timeout and
// returns the reported version. It prefers the JSON form ("version --json" ->
// {"version": "..."}); on any failure it falls back to the plain-text form
// ("version" -> "gitmoot <ver>", from which the "gitmoot" prefix is trimmed).
// Returns "" on any error (fail-open).
func execDaemonVersion(ctx context.Context, executable string) string {
	// Both attempts share ONE budget so the whole probe is bounded by
	// daemonVersionTimeout, never 2× it (a wedged binary must not stack).
	ctx, cancel := context.WithTimeout(ctx, daemonVersionTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, executable, "version", "--json").Output(); err == nil {
		var parsed struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(out, &parsed) == nil {
			if v := strings.TrimSpace(parsed.Version); v != "" {
				return v
			}
		}
	}
	out, err := exec.CommandContext(ctx, executable, "version").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	return strings.TrimSpace(strings.TrimPrefix(line, "gitmoot"))
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
		Runtime:  resolveJobRuntime(job, payload, runtimeByAgent),
		Model:    strings.TrimSpace(payload.Model),
		State:    mapNodeState(job.State),
		Depth:    job.DelegationDepth,
		Events:   []dashboard.Event{},
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
