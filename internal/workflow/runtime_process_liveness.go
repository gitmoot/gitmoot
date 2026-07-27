package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RuntimeProcessIdentity returns the Linux /proc starttime identity for pid.
// Empty means the identity is unavailable. Starttime is stable for a process's
// lifetime and changes when the kernel recycles a PID.
func RuntimeProcessIdentity(pid int) string {
	identity, _ := runtimeProcessIdentity(pid, "/proc")
	return identity
}

// RuntimeProcessLiveness reports whether pid still names the exact process whose
// starttime identity was recorded at dispatch. known=false is deliberately
// neutral: callers must not turn missing /proc access or missing identity into a
// dead-process verdict.
func RuntimeProcessLiveness(pid int, identity string) (live bool, known bool) {
	return runtimeProcessLiveness(pid, identity, "/proc")
}

func runtimeProcessLiveness(pid int, identity, procRoot string) (live bool, known bool) {
	if pid <= 0 {
		return false, false
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return false, false
	}
	// Distinguish an unavailable process table (for example macOS has no /proc)
	// from a readable process table whose specific PID entry is gone. Only the
	// latter confirms that a process with a previously recorded identity died.
	if _, err := os.Stat(procRoot); err != nil {
		return false, false
	}
	current, err := runtimeProcessIdentity(pid, procRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, true
		}
		return false, false
	}
	return current == identity, true
}

func runtimeProcessIdentity(pid int, procRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	// /proc/<pid>/stat's second field is parenthesized comm and may contain
	// spaces or ')' characters. Split after its final ')' so fields[0] is field
	// 3 (state); starttime is field 22, therefore fields[19].
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 {
		return "", errors.New("process stat missing command terminator")
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	if len(fields) <= 19 {
		return "", errors.New("process stat missing starttime")
	}
	startTime := strings.TrimSpace(fields[19])
	if startTime == "" {
		return "", errors.New("process stat has empty starttime")
	}
	return startTime, nil
}
