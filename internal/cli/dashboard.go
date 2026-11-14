package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cli/style"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// dashboardListCap is how many entries each list shows in styled mode before
// truncating with a "… N more" line; --all overrides it.
const dashboardListCap = 8

// dashboardSnapshot is a read-only view of local Gitmoot state. It is assembled
// entirely from existing store/status sources; the dashboard never owns
// workflow state.
type dashboardSnapshot struct {
	Home           string                  `json:"home"`
	DatabasePath   string                  `json:"database_path"`
	DatabaseExists bool                    `json:"database_exists"`
	Daemon         dashboardDaemon         `json:"daemon"`
	Repos          []dashboardRepo         `json:"repos"`
	Agents         []dashboardAgent        `json:"reviewers"`
	ActiveJobs     []dashboardActiveJob    `json:"active_runs"`
	Jobs           dashboardJobs           `json:"runs"`
	Worktrees      []dashboardWorktree     `json:"worktrees"`
	ResourceLocks  []dashboardResourceLock `json:"resource_locks"`

	// jobRows carries per-job rows for the web dashboard's run summary. The
	// latest-event message it also carried went with the attention grouping the
	// TUI owned (#1787 review F2). Unexported so --json and the plain renderer
	// stay byte-stable.
	jobRows []dashboardJobRow
	// daemonDetail carries the persisted daemon flags/workdir and a tail of
	// recent log errors. Unexported for the same reason.
	daemonDetail dashboardDaemonDetail
}

// dashboardDaemonDetail is the extra daemon info the Health page shows beyond
// running/pid (read from daemon.json + the log file; never serialized).
type dashboardDaemonDetail struct {
	Flags     []string
	WorkDir   string
	StartedAt string
	LogErrors []string
}

// dashboardJobRow is one job as the web dashboard's run summary consumes it.
//
// It carried LatestEvent, Repo and PreflightFailed for the attention grouping
// in the deleted TUI. The only remaining consumer, webDataSource.Runs, projects
// each row to row.Job and discards everything else, so those three fields were
// write-only after the deletion and are gone rather than hardened - along with
// the two per-refresh DB round-trips and the per-job payload unmarshal that
// existed solely to populate them (#1787 review F2).
type dashboardJobRow struct {
	db.Job
}

type dashboardResourceLock struct {
	Key   string `json:"key"`
	Owner string `json:"owner,omitempty"`
	Stale bool   `json:"stale"`
}

type dashboardDaemon struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	LogFile string `json:"log_file,omitempty"`
}

type dashboardRepo struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type dashboardAgent struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	Role    string `json:"role,omitempty"`
	Health  string `json:"health,omitempty"`
}

type dashboardJobs struct {
	Total   int            `json:"total"`
	ByState map[string]int `json:"by_state"`
}

// dashboardActiveJob is one in-flight job (queued or running) surfaced in the
// dashboard's live "active work" view (#505). Without it a running `@agent ask`
// is invisible — the jobs table otherwise holds mostly terminal rows. It is
// built from the already-loaded jobs slice (no extra DB round-trip); the held
// runtime-session lock (resource_locks key prefix "runtime:") is the reliable
// "session busy" signal and is already surfaced in the resource_locks section.
type dashboardActiveJob struct {
	ID    string `json:"id"`
	Agent string `json:"reviewer"`
	Repo  string `json:"repo,omitempty"`
	Type  string `json:"type"`
	State string `json:"state"`
}

type dashboardWorktree struct {
	Task string `json:"pr"`
	Repo string `json:"repo,omitempty"`
	Path string `json:"path"`
}

// runDashboardWebFn is the --web entry point, replaceable in tests so the
// early-return invariant can be observed without starting a blocking server.
var runDashboardWebFn = runDashboardWeb

func runDashboard(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "write the snapshot as JSON")
	all := fs.Bool("all", false, "show full lists without truncation or grouping")
	web := fs.Bool("web", false, "serve the read-only web dashboard (reviewers, PRs, runs, campaigns) until interrupted")
	webAddr := fs.String("addr", "127.0.0.1:8080", "address for --web to listen on")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "dashboard does not accept positional arguments")
		return 2
	}
	// The web dashboard is a separate read-only HTTP server (never the daemon
	// path); branch out before the one-shot snapshot.
	//
	// The indirection is a TEST SEAM, not indirection for its own sake: --web
	// blocks until interrupted, so the early return cannot be pinned by calling
	// it. A mutant that deleted this branch survived the whole package (#1787
	// review N2) precisely because nothing could observe the branch being taken.
	if *web {
		return runDashboardWebFn(*home, *webAddr, stdout, stderr)
	}

	// Everything else prints the styled one-shot snapshot.
	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}

	snapshot, err := buildDashboardSnapshot(*home, paths)
	if err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, snapshot); err != nil {
			fmt.Fprintf(stderr, "dashboard: %v\n", err)
			return 1
		}
		return 0
	}
	printDashboardSnapshot(stdout, style.For(stdout), snapshot, *home, *all)
	return 0
}

// buildDashboardDaemonDetail reads the persisted daemon meta (flags, workdir,
// start time) and a tail of recent error-ish log lines for the Health page.
// Both are cheap file reads with no side effects; absent files yield zero
// values (the daemon may never have run).
func buildDashboardDaemonDetail(state daemonState) dashboardDaemonDetail {
	detail := dashboardDaemonDetail{}
	if meta, err := readDaemonMeta(state); err == nil {
		detail.Flags = daemonStartArgsFromRunArgs(meta.Args)
		detail.WorkDir = meta.WorkingDir
		detail.StartedAt = meta.StartedAt
	}
	detail.LogErrors = tailDaemonLogErrors(state.LogFile, 8)
	return detail
}

// daemonLogTailBytes bounds the tail read so the Health-page log scan costs
// the same on a fresh log and a multi-GB one (this runs on the snapshot tick).
const daemonLogTailBytes = 64 * 1024

// tailDaemonLogErrors returns up to limit of the most recent error-shaped
// lines from the END of the daemon log (best effort; missing/unreadable log →
// nil). It reads only the last daemonLogTailBytes, so cost is independent of
// log size — the plan requires this stay off the unbounded path.
func tailDaemonLogErrors(logFile string, limit int) []string {
	if strings.TrimSpace(logFile) == "" {
		return nil
	}
	file, err := os.Open(logFile)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	size := info.Size()
	start := int64(0)
	if size > daemonLogTailBytes {
		start = size - daemonLogTailBytes
	}
	buf := make([]byte, size-start)
	if _, err := file.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil
	}
	lines := strings.Split(string(buf), "\n")
	// Drop a leading partial line when we seeked into the middle of the file.
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	var matches []string
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "panic") || strings.Contains(lower, "failed") {
			matches = append(matches, strings.TrimSpace(line))
			if len(matches) > limit {
				matches = matches[1:]
			}
		}
	}
	return matches
}

func buildDashboardSnapshot(home string, paths config.Paths) (dashboardSnapshot, error) {
	snapshot := dashboardSnapshot{
		Home:          paths.Home,
		DatabasePath:  paths.Database,
		Jobs:          dashboardJobs{ByState: map[string]int{}},
		Repos:         []dashboardRepo{},
		Agents:        []dashboardAgent{},
		ActiveJobs:    []dashboardActiveJob{},
		Worktrees:     []dashboardWorktree{},
		ResourceLocks: []dashboardResourceLock{},
	}
	now := time.Now().UTC()
	if info, err := os.Stat(paths.Database); err == nil && !info.IsDir() {
		snapshot.DatabaseExists = true
	}

	state := daemonProcessState(paths)
	if pid, _, err := currentDaemonPID(state); err == nil && pid > 0 {
		snapshot.Daemon = dashboardDaemon{Running: true, PID: pid, LogFile: state.LogFile}
	} else {
		snapshot.Daemon = dashboardDaemon{Running: false, LogFile: state.LogFile}
	}
	snapshot.daemonDetail = buildDashboardDaemonDetail(state)

	err := withStore(home, func(store *db.Store) error {
		ctx := context.Background()
		repos, err := store.ListRepos(ctx)
		if err != nil {
			return err
		}
		for _, repo := range repos {
			snapshot.Repos = append(snapshot.Repos, dashboardRepo{Name: repo.FullName(), Enabled: repo.Enabled})
		}
		agents, err := store.ListAgents(ctx)
		if err != nil {
			return err
		}
		for _, agent := range agents {
			if isEphemeralAgentName(agent.Name) {
				continue // transient spawn-from-spec workers are not part of the registry
			}
			snapshot.Agents = append(snapshot.Agents, dashboardAgent{Name: agent.Name, Runtime: agent.Runtime, Role: agent.Role, Health: agent.HealthStatus})
		}

		jobs, err := store.ListJobs(ctx)
		if err != nil {
			return err
		}
		snapshot.Jobs.Total = len(jobs)
		// Live "active work" view (#505): reuse the already-loaded jobs slice (no
		// extra DB round-trip) so a running @agent ask is visible while it runs.
		snapshot.ActiveJobs = buildDashboardActiveJobs(jobs)
		for _, job := range jobs {
			state := job.State
			if strings.TrimSpace(state) == "" {
				state = "unknown"
			}
			snapshot.Jobs.ByState[state]++
			snapshot.jobRows = append(snapshot.jobRows, dashboardJobRow{Job: job})
		}
		// Newest-first for dashboard run listings (ISO timestamps sort lexically;
		// id breaks ties deterministically). jobRows is unexported, so the --json
		// aggregate stays byte-stable.
		sort.SliceStable(snapshot.jobRows, func(i, j int) bool {
			a, b := snapshot.jobRows[i], snapshot.jobRows[j]
			if a.UpdatedAt != b.UpdatedAt {
				return a.UpdatedAt > b.UpdatedAt
			}
			return a.ID > b.ID
		})
		tasks, err := store.ListTasks(ctx)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if strings.TrimSpace(task.WorktreePath) != "" {
				snapshot.Worktrees = append(snapshot.Worktrees, dashboardWorktree{Task: task.ID, Repo: task.RepoFullName, Path: task.WorktreePath})
			}
		}
		resourceLocks, err := store.ListResourceLocks(ctx)
		if err != nil {
			return err
		}
		for _, lock := range resourceLocks {
			snapshot.ResourceLocks = append(snapshot.ResourceLocks, dashboardResourceLock{
				Key:   lock.ResourceKey,
				Owner: lock.OwnerJobID,
				Stale: dashboardLockStale(lock.ExpiresAt, now),
			})
		}
		return nil
	})
	if err != nil {
		return dashboardSnapshot{}, err
	}
	return snapshot, nil
}

// buildDashboardActiveJobs projects the already-loaded jobs slice into the live
// active-work view (#505), keeping only in-flight jobs (queued/running) and
// excluding terminal rows. The repo is parsed from the job payload (the same
// shape used elsewhere in the snapshot). It is pure (no store access) so it is
// unit-tested directly. The result is always non-nil so the --json output is
// `[]` rather than null when nothing is in flight.
func buildDashboardActiveJobs(jobs []db.Job) []dashboardActiveJob {
	active := []dashboardActiveJob{}
	for _, job := range jobs {
		if job.State != "queued" && job.State != "running" {
			continue
		}
		entry := dashboardActiveJob{ID: job.ID, Agent: job.Agent, Type: job.Type, State: job.State}
		var p struct {
			Repo string `json:"repo"`
		}
		if err := json.Unmarshal([]byte(job.Payload), &p); err == nil {
			entry.Repo = strings.TrimSpace(p.Repo)
		}
		active = append(active, entry)
	}
	return active
}

func printDashboardSnapshot(stdout io.Writer, st style.Style, snapshot dashboardSnapshot, home string, all bool) {
	printDashboardAttention(stdout, st, snapshot, home)

	writeLine(stdout, "home: %s", snapshot.Home)
	writeLine(stdout, "database: %s (%s)", snapshot.DatabasePath, presentOrMissing(snapshot.DatabaseExists))
	if snapshot.Daemon.Running {
		writeLine(stdout, "daemon: running pid %d", snapshot.Daemon.PID)
	} else {
		writeLine(stdout, "daemon: stopped")
	}

	dashboardSectionHeader(stdout, st, "repos", len(snapshot.Repos))
	repos, hidden := dashboardTruncate(st, all, snapshot.Repos)
	for _, repo := range repos {
		status := enabledOrDisabled(repo.Enabled)
		if repo.Enabled {
			status = st.Green(status)
		} else {
			status = st.Dim(status)
		}
		writeLine(stdout, "  %s (%s)", repo.Name, status)
	}
	dashboardMore(stdout, st, hidden)

	dashboardSectionHeader(stdout, st, "reviewers", len(snapshot.Agents))
	agents, hidden := dashboardTruncate(st, all, snapshot.Agents)
	for _, agent := range agents {
		writeLine(stdout, "  %s [%s]%s", agent.Name, agent.Runtime, dashboardSuffix(agent.Role, agent.Health))
	}
	dashboardMore(stdout, st, hidden)

	dashboardSectionHeader(stdout, st, "active_runs", len(snapshot.ActiveJobs))
	activeJobs, hidden := dashboardTruncate(st, all, snapshot.ActiveJobs)
	for _, job := range activeJobs {
		writeLine(stdout, "  %s %s [%s] %s %s", job.ID, job.Agent, job.Type, emptyText(job.Repo), dashboardJobStateColor(st, job.State))
	}
	dashboardMore(stdout, st, hidden)

	dashboardSectionHeader(stdout, st, "runs", snapshot.Jobs.Total)
	for _, state := range sortedKeys(snapshot.Jobs.ByState) {
		writeLine(stdout, "  %s: %d", dashboardJobStateColor(st, state), snapshot.Jobs.ByState[state])
	}

	dashboardSectionHeader(stdout, st, "worktrees", len(snapshot.Worktrees))
	worktrees, hidden := dashboardTruncate(st, all, snapshot.Worktrees)
	for _, worktree := range worktrees {
		writeLine(stdout, "  %s %s", worktree.Task, worktree.Path)
	}
	dashboardMore(stdout, st, hidden)
}

// printDashboardAttention prints, before the regular sections, the things a
// human most likely needs to act on. It is additive and shown in both styled and
// plain modes; it prints nothing when there is nothing to flag.
func printDashboardAttention(stdout io.Writer, st style.Style, snapshot dashboardSnapshot, home string) {
	lines := []string{}
	if blocked := snapshot.Jobs.ByState["blocked"]; blocked > 0 {
		lines = append(lines, st.Red(fmt.Sprintf("%d blocked run(s)", blocked)))
	}
	if failed := snapshot.Jobs.ByState["failed"]; failed > 0 {
		lines = append(lines, st.Red(fmt.Sprintf("%d failed run(s)", failed)))
	}
	for _, lock := range snapshot.ResourceLocks {
		if lock.Stale {
			lines = append(lines, st.Red("stale lock")+" "+lock.Key)
		}
	}
	if len(lines) == 0 {
		return
	}
	writeLine(stdout, "%s", st.Bold("needs attention:"))
	for _, line := range lines {
		writeLine(stdout, "  %s", line)
	}
	writeLine(stdout, "")
}

func dashboardSectionHeader(stdout io.Writer, st style.Style, name string, count int) {
	fmt.Fprintf(stdout, "%s %d\n", st.Bold(name+":"), count)
}

// dashboardTruncate limits a list to dashboardListCap in styled mode; --all and
// plain mode keep everything.
func dashboardTruncate[T any](st style.Style, all bool, items []T) ([]T, int) {
	if all || !st.Enabled() {
		return items, 0
	}
	return style.TopN(items, dashboardListCap)
}

func dashboardMore(stdout io.Writer, st style.Style, hidden int) {
	if hidden > 0 {
		writeLine(stdout, "  %s", st.Dim(fmt.Sprintf("… %d more (use --all)", hidden)))
	}
}

func dashboardJobStateColor(st style.Style, state string) string {
	switch state {
	case "failed", "blocked":
		return st.Red(state)
	case "succeeded":
		return st.Green(state)
	case "running":
		return st.Cyan(state)
	default:
		return state
	}
}

func dashboardLockStale(expiresAt string, now time.Time) bool {
	expiry, err := time.Parse("2006-01-02T15:04:05.000000000Z", strings.TrimSpace(expiresAt))
	if err != nil {
		return false
	}
	return expiry.Before(now)
}

func presentOrMissing(present bool) string {
	if present {
		return "present"
	}
	return "missing"
}

func enabledOrDisabled(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func dashboardSuffix(role, health string) string {
	parts := []string{}
	if strings.TrimSpace(role) != "" {
		parts = append(parts, role)
	}
	if strings.TrimSpace(health) != "" {
		parts = append(parts, health)
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
