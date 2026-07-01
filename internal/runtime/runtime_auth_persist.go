package runtime

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// RuntimeAuthEnvVars is the set of runtime-auth environment variables the daemon
// persists so a `daemon restart` launched from a shell that no longer carries
// them cannot silently disable runtime auth (#578, the #559 root cause; #581
// only WARNS). These are exactly the Claude/Anthropic credential vars
// InspectClaudeAuthEnv inspects — token values that MUST be treated as secrets:
// never logged, printed, or written to the world-readable daemon.json/meta.
var RuntimeAuthEnvVars = []string{
	ClaudeOAuthTokenEnv,
	AnthropicAPIKeyEnv,
	AnthropicAuthTokenEnv,
}

// runtimeAuthFileHeader documents the file for a human who finds it; it is
// deliberately value-free so nothing sensitive lives in a comment.
const runtimeAuthFileHeader = "# gitmoot daemon runtime auth — persisted by `gitmoot daemon start` (#578).\n" +
	"# 0600, owner read/write only. Contains runtime auth tokens; do not share or log.\n"

// PersistRuntimeAuthEnv writes the runtime-auth variables currently present in
// the environment (via lookup) to path, created 0600 (owner read/write only).
//
// It NEVER overwrites a good persisted token with an empty/unset env: a value
// already in the file is retained unless the live env supplies a non-empty
// replacement, and the live env always wins when both are present. It writes
// nothing — and removes nothing — when no token is present in either the env or
// the existing file, so a plain start on a Codex/Kimi-only box leaves no file.
//
// It returns whether a file was written. The token value is never logged; on
// error the returned error carries only the path/OS reason, not the secret.
func PersistRuntimeAuthEnv(path string, lookup func(string) (string, bool)) (bool, error) {
	if lookup == nil {
		return false, nil
	}
	existing, err := LoadPersistedRuntimeAuthEnv(path)
	if err != nil {
		return false, err
	}
	merged := make(map[string]string, len(RuntimeAuthEnvVars))
	for _, key := range RuntimeAuthEnvVars {
		// Prefer the live env when it supplies a non-empty value; otherwise retain
		// any previously persisted value so an unset env never erases a good token.
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			merged[key] = v
			continue
		}
		if v, ok := existing[key]; ok && strings.TrimSpace(v) != "" {
			merged[key] = v
		}
	}
	if len(merged) == 0 {
		// Nothing worth persisting; never create or truncate a file with no token.
		return false, nil
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(runtimeAuthFileHeader)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(merged[k])
		b.WriteString("\n")
	}

	// Write via a 0600 temp file + rename so the destination is atomic and its
	// mode is 0600 regardless of any pre-existing file's permissions. os.WriteFile
	// would keep an existing file's (possibly looser) mode; an explicit Chmod makes
	// the 0600 guarantee hold even under a permissive umask.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return false, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// LoadPersistedRuntimeAuthEnv reads path (which may not exist) and returns the
// KEY=VALUE map of persisted runtime-auth vars. A missing file returns an empty
// map and a nil error. Only keys in RuntimeAuthEnvVars are honored; blank lines,
// comments, malformed lines, and empty values are ignored.
func LoadPersistedRuntimeAuthEnv(path string) (map[string]string, error) {
	result := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return nil, err
	}
	allowed := make(map[string]bool, len(RuntimeAuthEnvVars))
	for _, k := range RuntimeAuthEnvVars {
		allowed[k] = true
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, val, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !allowed[key] || strings.TrimSpace(val) == "" {
			continue
		}
		result[key] = val
	}
	return result, nil
}

// RuntimeAuthChildEnv returns the KEY=VALUE entries to ADD to a restarted
// daemon child's environment: for each runtime-auth var that is absent (or
// empty) in the live env (lookup) but present in the persisted file, the
// persisted value is injected. The live env always wins, so a var already set is
// never overridden — this preserves the pre-#578 behavior of inheriting the
// launching shell's tokens and only recovers auth the shell has since lost.
//
// The returned slice is safe to append to os.Environ(); it is nil when there is
// nothing to recover. Values are secrets and must not be logged.
func RuntimeAuthChildEnv(path string, lookup func(string) (string, bool)) ([]string, error) {
	persisted, err := LoadPersistedRuntimeAuthEnv(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, key := range RuntimeAuthEnvVars {
		if lookup != nil {
			if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
				continue // live env wins; never override an already-set token
			}
		}
		if v, ok := persisted[key]; ok && strings.TrimSpace(v) != "" {
			out = append(out, fmt.Sprintf("%s=%s", key, v))
		}
	}
	return out, nil
}
