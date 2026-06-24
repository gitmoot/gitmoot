//go:build linux

package presence

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readProcessEnviron reads /proc/<pid>/environ and returns an env lookup over the
// running process's environment. It is best-effort and fail-open: a missing or
// unreadable /proc entry (different user, hardened kernel, process gone) reports
// detected=false rather than an error, mirroring the OS-gated /proc reads in
// process_unix.go. It is only built on Linux; other platforms get the no-op
// variant in daemon_env_other.go.
func readProcessEnviron(pid int) (func(string) (string, bool), bool) {
	if pid <= 0 {
		return nil, false
	}
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, false
	}
	return parseEnviron(contents), true
}

// parseEnviron turns the raw NUL-delimited contents of /proc/<pid>/environ into
// an env lookup. Each entry is a NUL-terminated key=value pair; the value may
// itself contain '=', so the split is on the first '=' only. Entries without an
// '=' (or empty trailing entries) are skipped. It is split from readProcessEnviron
// so the parser can be unit-tested on synthetic bytes (issue #427).
func parseEnviron(contents []byte) func(string) (string, bool) {
	env := map[string]string{}
	for _, entry := range strings.Split(string(contents), "\x00") {
		if entry == "" {
			continue
		}
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[name] = value
	}
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}
