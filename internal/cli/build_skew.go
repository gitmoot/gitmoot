package cli

import (
	"context"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/doctor"
)

// daemonBuildStatus answers the only build question an operator can act on: is
// the running daemon executing the binary that is on disk right now?
//
// It compares the build the daemon RECORDED at startup (what the process is
// actually running) against the build of the binary now at the daemon's
// executable path (what a restart would load). The build of the binary invoking
// this command is deliberately not part of the comparison — you may be running
// `doctor` from a binary that is not the daemon's at all, and warning about that
// would be noise, not signal.
func daemonBuildStatus(paths config.Paths) doctor.BuildStatus {
	var status doctor.BuildStatus
	// DefaultPaths is best-effort at the call sites (a missing HOME yields zero
	// Paths). daemonProcessState would then resolve daemon.pid/daemon.json
	// RELATIVE to the cwd — and currentDaemonPID removes a pidfile it cannot
	// parse — so refuse rather than touch a stranger's files.
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
	status.Daemon = doctor.BuildInfoFromValues(meta.Version, meta.Commit)
	status.OnDiskPath = strings.TrimSpace(meta.Executable)
	if status.OnDiskPath == "" {
		return status
	}
	version, commit := execBinaryBuildFn(context.Background(), status.OnDiskPath)
	status.OnDisk = doctor.BuildInfoFromValues(version, commit)
	return status
}

func daemonBuildCheck(paths config.Paths) doctor.Check {
	return doctor.CheckBuild(daemonBuildStatus(paths))
}

// daemonBuildLabel names the build the daemon PROCESS is running, for `daemon
// status`. A daemon started by an older gitmoot recorded none.
func daemonBuildLabel(meta daemonMeta) string {
	version := strings.TrimSpace(meta.Version)
	if version == "" {
		return "unknown"
	}
	if commit := strings.TrimSpace(meta.Commit); commit != "" && !strings.EqualFold(commit, "unknown") {
		if len(commit) > 8 {
			commit = commit[:8]
		}
		return version + " (" + commit + ")"
	}
	return version
}
