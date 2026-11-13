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

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
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
	state, err := ProbeDaemonProcess(pid, filepath.Join(paths.Home, "daemon.json"))
	if err != nil {
		snapshot.State = DaemonUnknown
		return snapshot
	}
	snapshot.State = state
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
