package presence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

const (
	DaemonRunning = "running"
	DaemonStopped = "stopped"
	DaemonUnknown = "unknown"
)

const maxFormattedLocks = 5

type Snapshot struct {
	Repo       string
	Home       string
	Daemon     DaemonSnapshot
	Tasks      int
	TaskStates map[string]int
	Jobs       int
	JobStates  map[string]int
	Locks      []LockSnapshot
}

type DaemonSnapshot struct {
	State   string
	PID     int
	LogFile string
}

type LockSnapshot struct {
	Branch string
	Owner  string
}

type daemonProcessFiles struct {
	MetaFile string
}

type daemonMeta struct {
	PID        int      `json:"pid"`
	Args       []string `json:"args"`
	Executable string   `json:"executable"`
}

func BuildSnapshot(ctx context.Context, paths config.Paths, repoFullName string) (Snapshot, error) {
	repoFullName = strings.TrimSpace(repoFullName)
	if repoFullName == "" {
		return Snapshot{}, errors.New("repo full name is required")
	}
	paths = normalizePaths(paths)
	if strings.TrimSpace(paths.Database) == "" {
		return Snapshot{}, errors.New("gitmoot database path is required")
	}

	snapshot := Snapshot{
		Repo:       repoFullName,
		Home:       paths.Home,
		Daemon:     InspectDaemon(paths),
		TaskStates: map[string]int{},
		JobStates:  map[string]int{},
	}

	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open gitmoot database read-only: %w", err)
	}
	defer store.Close()

	tasks, err := store.ListTasksByRepo(ctx, repoFullName)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list tasks: %w", err)
	}
	snapshot.Tasks = len(tasks)
	for _, task := range tasks {
		incrementState(snapshot.TaskStates, task.State)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list jobs: %w", err)
	}
	for _, job := range jobs {
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil || strings.TrimSpace(payload.Repo) != repoFullName {
			continue
		}
		snapshot.Jobs++
		incrementState(snapshot.JobStates, job.State)
	}

	locks, err := store.ListBranchLocks(ctx, repoFullName)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list locks: %w", err)
	}
	snapshot.Locks = make([]LockSnapshot, 0, len(locks))
	for _, lock := range locks {
		snapshot.Locks = append(snapshot.Locks, LockSnapshot{
			Branch: lock.Branch,
			Owner:  lock.Owner,
		})
	}
	return snapshot, nil
}

// DaemonAuthSnapshot reports the running daemon's Claude auth env, best-effort.
// The signal that actually matters for background Claude jobs is the daemon's own
// environment (not the shell that ran `gitmoot doctor`), so this reads the live
// daemon process's environment. It is fail-open and OS-gated: outside Linux, or
// when /proc is unreadable, Detected is false and callers fall back to the
// shell-local check (issue #427).
type DaemonAuthSnapshot struct {
	// Running reports whether a daemon process was located at all.
	Running bool
	// PID is the daemon process id when Running.
	PID int
	// Detected reports whether the daemon's environment could be read. When false,
	// Auth is zero and callers must not treat it as "daemon has no token".
	Detected bool
	// Auth is the daemon's Claude auth env (only meaningful when Detected).
	Auth runtime.ClaudeAuthEnv
}

// InspectDaemonClaudeAuth locates the running daemon and reports its Claude auth
// environment, best-effort. It never returns an error: a missing daemon,
// unreadable /proc, or non-Linux host all degrade to Detected=false so the
// caller can fall back to the shell-local check. Secrets are never returned —
// only the presence/absence booleans on ClaudeAuthEnv.
func InspectDaemonClaudeAuth(paths config.Paths) DaemonAuthSnapshot {
	daemon := InspectDaemon(paths)
	snapshot := DaemonAuthSnapshot{
		PID:     daemon.PID,
		Running: daemon.State == DaemonRunning,
	}
	if !snapshot.Running || daemon.PID <= 0 {
		return snapshot
	}
	lookup, ok := readProcessEnviron(daemon.PID)
	if !ok {
		return snapshot
	}
	snapshot.Detected = true
	snapshot.Auth = runtime.InspectClaudeAuthEnv(lookup)
	return snapshot
}

// DaemonClaudeCredEnv returns the env entries needed to validate the token the
// running daemon will ACTUALLY use, for injecting into a live probe so the doctor
// checks the daemon's credential rather than the doctor process's own. It returns
// the daemon's set Claude credential VALUES as KEY=VALUE entries AND, critically,
// NAME= (empty) for each Claude credential var the daemon does NOT set — so when
// the entries are appended onto the doctor process's environment (RunEnv), they
// NEUTRALIZE any competing credential the doctor shell carries but the daemon
// lacks (claude treats empty as unset). Without this, a doctor-shell
// ANTHROPIC_API_KEY (which outranks OAuth) could validate the WRONG credential and
// mask a bad daemon token (#486). Unlike DaemonAuthSnapshot (set/unset booleans
// only) this returns secret VALUES — callers must use them solely as subprocess
// env and never print or persist them. It is best-effort and fail-open: a missing
// daemon, unreadable /proc, or non-Linux host yields (nil, false); a detected
// daemon with no Claude credential at all also yields (nil, false) so the caller
// falls back to the presence-only report.
func DaemonClaudeCredEnv(paths config.Paths) ([]string, bool) {
	daemon := InspectDaemon(paths)
	if daemon.State != DaemonRunning || daemon.PID <= 0 {
		return nil, false
	}
	lookup, ok := readProcessEnviron(daemon.PID)
	if !ok {
		return nil, false
	}
	return claudeCredEnvFromLookup(lookup)
}

// claudeCredEnvFromLookup builds the isolated Claude credential env from a
// lookup func (split out from DaemonClaudeCredEnv so the neutralization contract
// is testable without a live daemon / readable /proc). Returns (nil, false) when
// the lookup carries no non-empty Claude credential.
func claudeCredEnvFromLookup(lookup func(string) (string, bool)) ([]string, bool) {
	if lookup == nil {
		return nil, false
	}
	hasCred := false
	env := make([]string, 0, 3)
	for _, name := range []string{
		runtime.ClaudeOAuthTokenEnv,
		runtime.AnthropicAPIKeyEnv,
		runtime.AnthropicAuthTokenEnv,
	} {
		if value, present := lookup(name); present && strings.TrimSpace(value) != "" {
			env = append(env, name+"="+value)
			hasCred = true
			continue
		}
		// The daemon does not set this credential: neutralize the doctor's own value
		// so it cannot leak into the probe and validate the wrong credential.
		env = append(env, name+"=")
	}
	if !hasCred {
		return nil, false
	}
	return env, true
}

func InspectDaemon(paths config.Paths) DaemonSnapshot {
	paths = normalizePaths(paths)
	snapshot := DaemonSnapshot{
		State:   DaemonStopped,
		LogFile: filepath.Join(paths.Logs, "daemon.log"),
	}
	pidPath := filepath.Join(paths.Home, "daemon.pid")
	content, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot
	}
	if err != nil {
		snapshot.State = DaemonUnknown
		return snapshot
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil || pid <= 0 {
		snapshot.State = DaemonUnknown
		return snapshot
	}
	snapshot.PID = pid
	snapshot.State = probeDaemonProcess(pid, daemonProcessFiles{
		MetaFile: filepath.Join(paths.Home, "daemon.json"),
	})
	return snapshot
}

func FormatSnapshot(snapshot Snapshot) string {
	var b strings.Builder
	fmt.Fprintln(&b, "Current snapshot")
	fmt.Fprintf(&b, "- daemon: %s\n", formatDaemon(snapshot.Daemon))
	fmt.Fprintf(&b, "- tasks: %d%s\n", snapshot.Tasks, formatCounts(snapshot.TaskStates))
	fmt.Fprintf(&b, "- jobs: %d%s\n", snapshot.Jobs, formatCounts(snapshot.JobStates))
	fmt.Fprintf(&b, "- locks: %d\n", len(snapshot.Locks))
	for i, lock := range snapshot.Locks {
		if i >= maxFormattedLocks {
			fmt.Fprintf(&b, "  - ... %d more\n", len(snapshot.Locks)-i)
			break
		}
		fmt.Fprintf(&b, "  - %s by %s\n", strconv.Quote(strings.TrimSpace(lock.Branch)), strconv.Quote(strings.TrimSpace(lock.Owner)))
	}
	return strings.TrimRight(b.String(), "\n")
}

func FormatUnavailable() string {
	return "Current snapshot\n- unavailable: local Gitmoot state could not be read"
}

func formatDaemon(snapshot DaemonSnapshot) string {
	state := strings.TrimSpace(snapshot.State)
	if state == "" {
		state = DaemonUnknown
	}
	switch state {
	case DaemonRunning:
		if snapshot.PID > 0 {
			return fmt.Sprintf("running (pid %d)", snapshot.PID)
		}
		return "running"
	case DaemonStopped:
		if snapshot.PID > 0 {
			return fmt.Sprintf("stopped (stale pid %d)", snapshot.PID)
		}
		return "stopped"
	default:
		if snapshot.PID > 0 {
			return fmt.Sprintf("unknown (pid %d)", snapshot.PID)
		}
		return "unknown"
	}
}

func formatCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", key, counts[key]))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func incrementState(counts map[string]int, state string) {
	state = strings.TrimSpace(state)
	if state == "" {
		state = DaemonUnknown
	}
	counts[state]++
}

func normalizePaths(paths config.Paths) config.Paths {
	if strings.TrimSpace(paths.Home) == "" {
		return paths
	}
	if strings.TrimSpace(paths.Database) == "" {
		paths.Database = filepath.Join(paths.Home, config.DBName)
	}
	if strings.TrimSpace(paths.Logs) == "" {
		paths.Logs = filepath.Join(paths.Home, config.LogsDir)
	}
	return paths
}
