package cli

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/doctor"
)

// daemonLogStaleness compares the advertised daemon log's last write with the
// running daemon's recorded start time. A missing log is a definite stale
// signal; malformed metadata and other stat failures stay neutral.
func daemonLogStaleness(logPath, startedAt string) (doctor.DaemonLogStatus, bool) {
	status := doctor.DaemonLogStatus{
		DaemonRunning: true,
		LogPath:       strings.TrimSpace(logPath),
	}
	started, err := time.Parse(time.RFC3339, strings.TrimSpace(startedAt))
	if err != nil {
		return status, false
	}
	status.StartedAt = started

	info, err := os.Stat(status.LogPath)
	if errors.Is(err, os.ErrNotExist) {
		status.Determined = true
		status.Missing = true
		status.Stale = true
		return status, true
	}
	if err != nil {
		return status, false
	}

	status.Determined = true
	status.LastWrite = info.ModTime()
	status.Stale = status.LastWrite.Before(status.StartedAt)
	return status, true
}

// daemonLogStatus mirrors daemonBuildStatus: only a positively identified
// running daemon with readable metadata can produce a staleness verdict.
func daemonLogStatus(paths config.Paths) doctor.DaemonLogStatus {
	var status doctor.DaemonLogStatus
	// Avoid resolving daemon state relative to the cwd when DefaultPaths failed.
	if strings.TrimSpace(paths.Home) == "" {
		return status
	}
	state := daemonProcessState(paths)
	pid, _, err := currentDaemonPID(state)
	if err != nil || pid <= 0 {
		return status
	}
	status.DaemonRunning = true

	meta, err := readDaemonMeta(state)
	if err != nil {
		return status
	}
	observed, ok := daemonLogStaleness(state.LogFile, meta.StartedAt)
	if !ok {
		return status
	}
	return observed
}

func daemonLogWarning(logPath, startedAt string) string {
	status, ok := daemonLogStaleness(logPath, startedAt)
	if !ok {
		return ""
	}
	check := doctor.CheckDaemonLog(status)
	if check.OK {
		return ""
	}
	return check.Detail
}
