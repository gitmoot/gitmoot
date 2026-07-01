package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// daemon_runtime_auth.go persists the daemon's runtime auth so a `daemon
// restart` (or a plain start) from a shell that lacks the token cannot silently
// disable Claude-runtime auth — the #559 root cause. #581 only WARNS; this
// PERSISTS (#578): on start the live token is written to an owner-only 0600 file
// in the daemon home, and on a later (re)start that inherits an environment
// WITHOUT the token, the persisted value is loaded and injected into the child
// daemon's environment so auth is preserved automatically.
//
// SECURITY: the token value NEVER touches stdout/stderr, the log, or the
// world-readable daemon.json/meta. It lives only in this 0600 file and in the
// child process environment. Diagnostics name the env var and the file, never
// the value.

// daemonRuntimeAuthEnvVars is the single source of truth for which runtime auth
// environment variables the daemon persists and recovers. Only Claude/Anthropic
// credentials are runtime auth secrets today (Codex/Kimi authenticate out of
// band); ordering is deterministic so the persisted file and injected child env
// are stable.
var daemonRuntimeAuthEnvVars = []string{
	runtime.ClaudeOAuthTokenEnv,
	runtime.AnthropicAPIKeyEnv,
	runtime.AnthropicAuthTokenEnv,
}

// daemonRuntimeAuthFileName is the owner-only (0600) file, under the daemon home
// (config.Paths.Home = <home>/.gitmoot), that carries the persisted tokens.
const daemonRuntimeAuthFileName = "daemon-runtime.env"

// daemonRuntimeAuthFilePerm is the required mode: owner read/write only. It is a
// non-negotiable security invariant asserted by a test — the file carries live
// credentials and must never be group/world readable.
const daemonRuntimeAuthFilePerm os.FileMode = 0o600

func daemonRuntimeAuthFilePath(homeDir string) string {
	return filepath.Join(homeDir, daemonRuntimeAuthFileName)
}

// collectRuntimeAuthEnv returns the runtime auth vars that are present AND
// non-empty in the given environment lookup, keyed by env-var name. A nil lookup
// or an unset/blank value contributes nothing.
func collectRuntimeAuthEnv(lookup func(string) (string, bool)) map[string]string {
	present := map[string]string{}
	if lookup == nil {
		return present
	}
	for _, name := range daemonRuntimeAuthEnvVars {
		if v, ok := lookup(name); ok && strings.TrimSpace(v) != "" {
			present[name] = v
		}
	}
	return present
}

// persistDaemonRuntimeAuth writes the runtime auth tokens present in the live
// environment to the 0600 daemon-runtime.env file under homeDir.
//
// Invariants (#578):
//   - Only persist when a token is actually present: an environment with NO
//     runtime auth token leaves any existing file untouched — a good persisted
//     token is NEVER overwritten with an empty/unset env.
//   - Prefer the live env over the file: when a var is set in both, the live
//     value wins; vars only in the file are preserved (merge, not replace).
//   - The file is created/re-secured to 0600, owner read/write only.
func persistDaemonRuntimeAuth(homeDir string, lookup func(string) (string, bool)) error {
	present := collectRuntimeAuthEnv(lookup)
	if len(present) == 0 {
		// Nothing to persist; never clobber a previously-persisted good token.
		return nil
	}
	path := daemonRuntimeAuthFilePath(homeDir)
	merged := loadDaemonRuntimeAuthFile(path) // best-effort; missing/unreadable => empty
	for name, value := range present {         // live env wins over the file
		merged[name] = value
	}
	return writeDaemonRuntimeAuthFile(path, merged)
}

// recoverDaemonChildAuthEnv returns the KEY=VALUE env entries to inject into the
// (re)started daemon child: for each runtime auth var that the launching
// environment LACKS but the persisted file HAS, one entry is returned. Vars
// already present in the live env are omitted (the child inherits them via
// os.Environ()), honoring "prefer the live env over the file". The result is
// sorted for determinism and is empty when nothing needs recovering.
func recoverDaemonChildAuthEnv(homeDir string, lookup func(string) (string, bool)) []string {
	persisted := loadDaemonRuntimeAuthFile(daemonRuntimeAuthFilePath(homeDir))
	if len(persisted) == 0 {
		return nil
	}
	live := collectRuntimeAuthEnv(lookup)
	var extra []string
	for _, name := range daemonRuntimeAuthEnvVars {
		if _, inLive := live[name]; inLive {
			continue
		}
		if value, ok := persisted[name]; ok {
			extra = append(extra, name+"="+value)
		}
	}
	sort.Strings(extra)
	return extra
}

// loadDaemonRuntimeAuthFile parses the persisted KEY=VALUE file into a map,
// keeping only recognized runtime auth vars. It is best-effort: a missing or
// unreadable file yields an empty (non-nil) map so callers can merge into it.
func loadDaemonRuntimeAuthFile(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	recognized := map[string]bool{}
	for _, name := range daemonRuntimeAuthEnvVars {
		recognized[name] = true
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if recognized[key] && strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	return out
}

// writeDaemonRuntimeAuthFile writes vars as KEY=VALUE lines and enforces mode
// 0600. It opens with O_CREATE|O_TRUNC and then explicitly Chmods, so an existing
// file with looser permissions is re-secured (OpenFile does not change the mode
// of a pre-existing file). Keys are emitted in daemonRuntimeAuthEnvVars order for
// a stable file.
func writeDaemonRuntimeAuthFile(path string, vars map[string]string) error {
	var b strings.Builder
	for _, name := range daemonRuntimeAuthEnvVars {
		if value, ok := vars[name]; ok {
			b.WriteString(name)
			b.WriteString("=")
			b.WriteString(value)
			b.WriteString("\n")
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, daemonRuntimeAuthFilePerm)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Re-assert 0600 in case the file pre-existed with looser bits or umask
	// interfered: the mode is a security invariant, not an approximation.
	if err := os.Chmod(path, daemonRuntimeAuthFilePerm); err != nil {
		return fmt.Errorf("securing %s: %w", daemonRuntimeAuthFileName, err)
	}
	return nil
}
