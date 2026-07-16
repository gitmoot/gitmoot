package transcript

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const jobLogHashBytes = 6

// LegacyLogName is the historical lossy job-id slug. It remains exported only
// for locating cockpit logs written before collision-proof names were added.
func LegacyLogName(jobID string) string {
	key := strings.TrimSpace(jobID)
	if key == "" {
		return "seat"
	}
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "seat"
	}
	return b.String()
}

// JobLogName returns a flat collision-proof filename component. IDs already
// safe on disk retain their historical name; sanitized IDs gain a short hash so
// values such as "a/b" and "a_b" cannot address the same retained log.
func JobLogName(jobID string) string {
	legacy := LegacyLogName(jobID)
	if legacy == jobID {
		return legacy
	}
	sum := sha256.Sum256([]byte(jobID))
	return fmt.Sprintf("%s-%x", legacy, sum[:jobLogHashBytes])
}

func JobLogPath(logsDir, jobID string) string {
	return filepath.Join(logsDir, "jobs", JobLogName(jobID)+".log")
}

func LegacyJobLogPath(logsDir, jobID string) string {
	return filepath.Join(logsDir, "jobs", LegacyLogName(jobID)+".log")
}

// ResolveJobLogPath prefers the collision-proof path and falls back to a
// historical lossy path when it exists. When neither exists it returns the new
// canonical path so callers retain a useful ENOENT location.
func ResolveJobLogPath(logsDir, jobID string) string {
	canonical := JobLogPath(logsDir, jobID)
	if _, err := os.Stat(canonical); err == nil || !os.IsNotExist(err) {
		return canonical
	}
	legacy := LegacyJobLogPath(logsDir, jobID)
	if legacy != canonical {
		if _, err := os.Stat(legacy); err == nil || !os.IsNotExist(err) {
			return legacy
		}
	}
	return canonical
}
