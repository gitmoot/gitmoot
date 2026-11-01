//go:build linux

package presence

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// processIsThreadGroupLeader distinguishes a process from a non-leader thread
// (#2463). Linux kill(2) and /proc/<id>/cmdline both accept thread IDs, so a
// persisted daemon PID later reused as a thread of another process — including
// the starting daemon itself — otherwise passes the liveness and argv checks.
// known is false when /proc does not report the identity; callers then keep
// their existing conservative checks rather than guessing.
func processIsThreadGroupLeader(pid int) (leader bool, known bool) {
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false, false
	}
	for _, line := range strings.Split(string(status), "\n") {
		value, ok := strings.CutPrefix(line, "Tgid:")
		if !ok {
			continue
		}
		tgid, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return false, false
		}
		return tgid == pid, true
	}
	return false, false
}
