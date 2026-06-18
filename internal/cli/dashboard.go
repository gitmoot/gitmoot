package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jerryfane/gitmoot/internal/cli/style"
	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// dashboardListCap is how many entries each list shows in styled mode before
// truncating with a "… N more" line; --all overrides it.
const dashboardListCap = 8

// dashboardSnapshot is a read-only view of local Gitmoot state. It is assembled
// entirely from existing store/status sources; the dashboard never owns
// workflow state.
type dashboardSnapshot struct {
	Home            string                  `json:"home"`
	DatabasePath    string                  `json:"database_path"`
	DatabaseExists  bool                    `json:"database_exists"`
	Daemon          dashboardDaemon         `json:"daemon"`
	Repos           []dashboardRepo         `json:"repos"`
	Agents          []dashboardAgent        `json:"agents"`
	RuntimeSessions []dashboardSession      `json:"runtime_sessions"`
	Jobs            dashboardJobs           `json:"jobs"`
	Worktrees       []dashboardWorktree     `json:"worktrees"`
	BranchLocks     []dashboardBranchLock   `json:"branch_locks"`
	TrainSessions   []dashboardTrainSession `json:"train_sessions"`
	ResourceLocks   []dashboardResourceLock `json:"resource_locks"`
	PendingPrompts  []dashboardPrompt       `json:"pending_prompts"`

	// promptDetails carries the full pending interactive-prompt records for the
	// TUI's inline answer flow. Unexported so it never appears in --json output
	// or the plain renderer, keeping those contracts byte-stable.
	promptDetails []db.InteractivePrompt
	// jobRows carries per-job rows (and, for blocked/failed jobs, the latest
	// event message) for the TUI's Jobs page. Unexported for the same reason.
	jobRows []dashboardJobRow
	// daemonDetail carries the persisted daemon flags/workdir and a tail of
	// recent log errors for the TUI's Health page. Unexported so --json and
	// the plain renderer stay byte-stable.
	daemonDetail dashboardDaemonDetail
	// configView carries the parsed config sections for the TUI's Config page.
	// Unexported for the same byte-stability reason.
	configView tui.ConfigView
}

// dashboardDaemonDetail is the extra daemon info the Health page shows beyond
// running/pid (read from daemon.json + the log file; never serialized).
type dashboardDaemonDetail struct {
	Flags     []string
	WorkDir   string
	StartedAt string
	LogErrors []string
}

// dashboardJobRow is one job with the context the TUI needs to act on it.
type dashboardJobRow struct {
	db.Job
	LatestEvent string
	Repo        string // parsed from the payload for reportable jobs (attention grouping)
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
	// templateID feeds the TUI's agent detail; unexported so the --json output
	// stays byte-identical.
	templateID string
}

type dashboardSession struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	Repo    string `json:"repo,omitempty"`
	State   string `json:"state,omitempty"`

	// Detail fields shown only in the interactive session detail view. They are
	// unexported so the --json/--plain output stays byte-stable (mirrors how
	// dashboardAgent.templateID is carried).
	sessionType string
	role        string
	templateID  string
	lastUsedAt  string
	expiresAt   string
}

type dashboardJobs struct {
	Total   int            `json:"total"`
	ByState map[string]int `json:"by_state"`
}

type dashboardWorktree struct {
	Task string `json:"task"`
	Repo string `json:"repo,omitempty"`
	Path string `json:"path"`
}

type dashboardBranchLock struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Owner  string `json:"owner,omitempty"`
}

type dashboardTrainSession struct {
	ID        string `json:"id"`
	Phase     string `json:"phase"`
	Candidate string `json:"candidate_version,omitempty"`
	Repo      string `json:"repo,omitempty"`
}

type dashboardPrompt struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

func runDashboard(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "write the snapshot as JSON")
	all := fs.Bool("all", false, "show full lists without truncation or grouping")
	answerID := fs.String("answer", "", "pending prompt id to answer before showing the snapshot")
	answerValue := fs.String("value", "", "answer value to use with --answer")
	answerSource := fs.String("source", "dashboard", "answer source recorded with --answer")
	dismissID := fs.String("dismiss", "", "prompt id to delete before showing the snapshot")
	plain := fs.Bool("plain", false, "print a one-shot snapshot instead of launching the interactive TUI")
	watch := fs.Bool("watch", false, "refresh the snapshot on an interval until interrupted (terminal only)")
	interval := fs.Duration("interval", 5*time.Second, "refresh interval for --watch")
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
	if *watch {
		if *jsonOutput || strings.TrimSpace(*answerID) != "" || strings.TrimSpace(*dismissID) != "" {
			fmt.Fprintln(stderr, "dashboard --watch cannot be combined with --json, --answer, or --dismiss")
			return 2
		}
		if !style.IsTerminal(stdout) {
			fmt.Fprintln(stderr, "dashboard --watch requires a terminal")
			return 2
		}
		if *interval <= 0 {
			fmt.Fprintln(stderr, "dashboard --interval must be greater than zero")
			return 2
		}
		return runDashboardWatch(stdout, *home, *all, *interval)
	}

	// On a real terminal with no machine-output or mutation flags, launch the
	// interactive TUI. Every other case (pipes, --json/--all/--plain/--answer/
	// --dismiss, non-terminal stdin) falls through to the one-shot snapshot, so
	// scripts, tests, and CI are unaffected.
	flags := dashboardFlags{
		plain:      *plain,
		jsonOutput: *jsonOutput,
		all:        *all,
		watch:      *watch,
		answerID:   *answerID,
		dismissID:  *dismissID,
	}
	if shouldLaunchTUI(flags, style.IsTerminal(stdout), stdinIsCharDevice()) {
		return runDashboardTUI(*home, *interval, stdout, stderr)
	}

	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}

	// The only state the dashboard may mutate is a pending prompt: answering it
	// (same store API as `gitmoot interactive answer`) or dismissing it (same as
	// `gitmoot interactive clear`).
	if strings.TrimSpace(*answerID) != "" {
		if err := withStore(*home, func(store *db.Store) error {
			_, err := store.AnswerInteractivePrompt(context.Background(), *answerID, *answerValue, *answerSource)
			return err
		}); err != nil {
			fmt.Fprintf(stderr, "dashboard: answer prompt: %v\n", err)
			return 1
		}
	}
	if strings.TrimSpace(*dismissID) != "" {
		if err := withStore(*home, func(store *db.Store) error {
			return store.DeleteInteractivePrompt(context.Background(), *dismissID)
		}); err != nil {
			fmt.Fprintf(stderr, "dashboard: dismiss prompt: %v\n", err)
			return 1
		}
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

// runDashboardWatch redraws the dashboard every interval until interrupted. Each
// frame is rendered to a buffer and written once (after a cursor-home + clear)
// to avoid flicker; transient store errors are shown in the frame rather than
// ending the loop.
func runDashboardWatch(stdout io.Writer, home string, all bool, interval time.Duration) int {
	st := style.For(stdout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	first := true
	for {
		var body bytes.Buffer
		if paths, err := initializedPaths(home); err != nil {
			fmt.Fprintf(&body, "dashboard: %v\n", err)
		} else if snapshot, err := buildDashboardSnapshot(home, paths); err != nil {
			fmt.Fprintf(&body, "dashboard: %v\n", err)
		} else {
			printDashboardSnapshot(&body, st, snapshot, home, all)
		}
		_, _ = stdout.Write(dashboardWatchFrame(body.Bytes(), first))
		first = false
		select {
		case <-ctx.Done():
			fmt.Fprintln(stdout)
			return 0
		case <-time.After(interval):
		}
	}
}

// dashboardWatchFrame wraps a rendered snapshot body with the terminal control
// codes for an in-place redraw: clear-screen on the first frame, then
// cursor-home + clear-below on every frame.
func dashboardWatchFrame(body []byte, first bool) []byte {
	var frame bytes.Buffer
	if first {
		frame.WriteString("\x1b[2J")
	}
	frame.WriteString("\x1b[H\x1b[0J")
	frame.Write(body)
	return frame.Bytes()
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
		Home:            paths.Home,
		DatabasePath:    paths.Database,
		Jobs:            dashboardJobs{ByState: map[string]int{}},
		Repos:           []dashboardRepo{},
		Agents:          []dashboardAgent{},
		RuntimeSessions: []dashboardSession{},
		Worktrees:       []dashboardWorktree{},
		BranchLocks:     []dashboardBranchLock{},
		TrainSessions:   []dashboardTrainSession{},
		ResourceLocks:   []dashboardResourceLock{},
		PendingPrompts:  []dashboardPrompt{},
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
	snapshot.configView = buildDashboardConfigView(paths, snapshot.daemonDetail)

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
			snapshot.Agents = append(snapshot.Agents, dashboardAgent{Name: agent.Name, Runtime: agent.Runtime, Role: agent.Role, Health: agent.HealthStatus, templateID: agent.TemplateID})
		}
		instances, err := store.ListAgentInstances(ctx)
		if err != nil {
			return err
		}
		for _, instance := range instances {
			snapshot.RuntimeSessions = append(snapshot.RuntimeSessions, dashboardSession{
				Name:        instance.Name,
				Runtime:     instance.Runtime,
				Repo:        instance.RepoFullName,
				State:       instance.State,
				sessionType: instance.Type,
				role:        instance.Role,
				templateID:  instance.TemplateID,
				lastUsedAt:  instance.LastUsedAt,
				expiresAt:   instance.ExpiresAt,
			})
		}
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			return err
		}
		snapshot.Jobs.Total = len(jobs)
		// One batched read of every job's latest event; blocked/failed rows
		// surface WHY in the attention list.
		latestEvents, err := store.LatestJobEvents(ctx)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			state := job.State
			if strings.TrimSpace(state) == "" {
				state = "unknown"
			}
			snapshot.Jobs.ByState[state]++
			row := dashboardJobRow{Job: job}
			if job.State == "blocked" || job.State == "failed" {
				row.LatestEvent = latestEvents[job.ID].Message
			}
			// Reportable jobs surface on the Attention page grouped by repo; the
			// repo lives in the payload, so parse it only for that small subset to
			// keep the refresh tick cheap.
			if job.State == "blocked" || job.State == "failed" || job.State == "cancelled" {
				var p struct {
					Repo string `json:"repo"`
				}
				if err := json.Unmarshal([]byte(job.Payload), &p); err == nil {
					row.Repo = strings.TrimSpace(p.Repo)
				}
			}
			snapshot.jobRows = append(snapshot.jobRows, row)
		}
		// Newest-first for the interactive Jobs list (ISO timestamps sort
		// lexically; id breaks ties deterministically). jobRows is unexported and
		// feeds only the TUI, so the --json/--plain aggregate stays byte-stable.
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
		locks, err := store.ListBranchLocks(ctx, "")
		if err != nil {
			return err
		}
		for _, lock := range locks {
			snapshot.BranchLocks = append(snapshot.BranchLocks, dashboardBranchLock{Repo: lock.RepoFullName, Branch: lock.Branch, Owner: lock.Owner})
		}
		trainSessions, err := store.ListSkillOptTrainSessions(ctx)
		if err != nil {
			return err
		}
		for _, session := range trainSessions {
			entry := dashboardTrainSession{ID: session.ID, Phase: session.State, Repo: session.TargetRepo}
			iteration, err := store.GetLatestSkillOptTrainIteration(ctx, session.ID)
			switch {
			case err == nil:
				summary := skillopt.BuildTrainStatusSummary(session, &iteration, skillopt.TrainStatusCounts{})
				entry.Phase = summary.CurrentPhase
				entry.Candidate = summary.CandidateVersion
				// Override with the live lock-derived phase (e.g. generating_options,
				// optimizer_running) using the same helper as `train status`.
				locks, lockErr := skillOptTrainActiveLocks(ctx, store, session.ID, iteration.ID)
				if lockErr != nil {
					return lockErr
				}
				if phase, ok := skillOptTrainLockPhase(locks); ok {
					entry.Phase = phase
				}
			case errors.Is(err, sql.ErrNoRows):
				// No iteration yet — keep the session-state fallback.
			default:
				return err
			}
			snapshot.TrainSessions = append(snapshot.TrainSessions, entry)
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
		prompts, err := store.ListInteractivePrompts(ctx, db.InteractivePromptStatePending)
		if err != nil {
			return err
		}
		for _, prompt := range prompts {
			snapshot.PendingPrompts = append(snapshot.PendingPrompts, dashboardPrompt{ID: prompt.ID, Question: prompt.Question})
		}
		snapshot.promptDetails = prompts
		return nil
	})
	if err != nil {
		return dashboardSnapshot{}, err
	}
	return snapshot, nil
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

	dashboardSectionHeader(stdout, st, "agents", len(snapshot.Agents))
	agents, hidden := dashboardTruncate(st, all, snapshot.Agents)
	for _, agent := range agents {
		writeLine(stdout, "  %s [%s]%s", agent.Name, agent.Runtime, dashboardSuffix(agent.Role, agent.Health))
	}
	dashboardMore(stdout, st, hidden)

	dashboardSectionHeader(stdout, st, "runtime_sessions", len(snapshot.RuntimeSessions))
	if st.Enabled() && !all {
		for _, line := range groupedRuntimeSessions(snapshot.RuntimeSessions) {
			writeLine(stdout, "  %s", line)
		}
	} else {
		for _, session := range snapshot.RuntimeSessions {
			writeLine(stdout, "  %s [%s] %s %s", session.Name, session.Runtime, emptyText(session.Repo), session.State)
		}
	}

	dashboardSectionHeader(stdout, st, "jobs", snapshot.Jobs.Total)
	for _, state := range sortedKeys(snapshot.Jobs.ByState) {
		writeLine(stdout, "  %s: %d", dashboardJobStateColor(st, state), snapshot.Jobs.ByState[state])
	}

	dashboardSectionHeader(stdout, st, "worktrees", len(snapshot.Worktrees))
	worktrees, hidden := dashboardTruncate(st, all, snapshot.Worktrees)
	for _, worktree := range worktrees {
		writeLine(stdout, "  %s %s", worktree.Task, worktree.Path)
	}
	dashboardMore(stdout, st, hidden)

	dashboardSectionHeader(stdout, st, "branch_locks", len(snapshot.BranchLocks))
	for _, lock := range snapshot.BranchLocks {
		writeLine(stdout, "  %s@%s %s", lock.Repo, lock.Branch, emptyText(lock.Owner))
	}

	dashboardSectionHeader(stdout, st, "train_sessions", len(snapshot.TrainSessions))
	trains, hidden := dashboardTruncate(st, all, snapshot.TrainSessions)
	for _, train := range trains {
		line := fmt.Sprintf("%s phase=%s candidate=%s", train.ID, train.Phase, emptyText(train.Candidate))
		if dashboardDeadTrainPhase(train.Phase) {
			line = st.Dim(line)
		}
		writeLine(stdout, "  %s", line)
	}
	dashboardMore(stdout, st, hidden)

	dashboardSectionHeader(stdout, st, "pending_prompts", len(snapshot.PendingPrompts))
	for _, prompt := range snapshot.PendingPrompts {
		writeLine(stdout, "  %s\t%s", prompt.ID, prompt.Question)
	}
}

// printDashboardAttention prints, before the regular sections, the things a
// human most likely needs to act on. It is additive and shown in both styled and
// plain modes; it prints nothing when there is nothing to flag.
func printDashboardAttention(stdout io.Writer, st style.Style, snapshot dashboardSnapshot, home string) {
	lines := []string{}
	for _, prompt := range snapshot.PendingPrompts {
		lines = append(lines, st.Yellow("prompt")+" "+prompt.ID)
		lines = append(lines, "  "+st.Dim(dashboardAnswerCommand(home, prompt.ID)))
	}
	if blocked := snapshot.Jobs.ByState["blocked"]; blocked > 0 {
		lines = append(lines, st.Red(fmt.Sprintf("%d blocked job(s)", blocked)))
	}
	if failed := snapshot.Jobs.ByState["failed"]; failed > 0 {
		lines = append(lines, st.Red(fmt.Sprintf("%d failed job(s)", failed)))
	}
	for _, lock := range snapshot.BranchLocks {
		lines = append(lines, fmt.Sprintf("branch lock %s@%s %s", lock.Repo, lock.Branch, emptyText(lock.Owner)))
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

func dashboardAnswerCommand(home, id string) string {
	if strings.TrimSpace(home) != "" {
		return fmt.Sprintf("gitmoot interactive answer --home %s %s <value>", home, id)
	}
	return fmt.Sprintf("gitmoot interactive answer %s <value>", id)
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

func dashboardDeadTrainPhase(phase string) bool {
	switch phase {
	case "run_abandoned", "candidate_rejected", "candidate_promoted":
		return true
	default:
		return false
	}
}

// groupedRuntimeSessions collapses generated "<type>-bg-<hex>" sessions sharing
// a type/runtime/state into a single counted line, leaving other sessions
// individual.
func groupedRuntimeSessions(sessions []dashboardSession) []string {
	type groupKey struct{ prefix, runtime, state string }
	order := []groupKey{}
	counts := map[groupKey]int{}
	singles := []dashboardSession{}
	for _, session := range sessions {
		if prefix, ok := style.GroupSuffix(session.Name); ok {
			key := groupKey{prefix: prefix, runtime: session.Runtime, state: session.State}
			if counts[key] == 0 {
				order = append(order, key)
			}
			counts[key]++
		} else {
			singles = append(singles, session)
		}
	}
	lines := make([]string, 0, len(order)+len(singles))
	for _, key := range order {
		lines = append(lines, fmt.Sprintf("%s [%s] ×%d %s", key.prefix, key.runtime, counts[key], key.state))
	}
	for _, session := range singles {
		lines = append(lines, fmt.Sprintf("%s [%s] %s %s", session.Name, session.Runtime, emptyText(session.Repo), session.State))
	}
	return lines
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
