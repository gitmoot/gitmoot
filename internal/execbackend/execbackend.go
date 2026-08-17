// Package execbackend names the execution-backend seam (#1535 P0 contract,
// #1536 P1): WHERE a job's runtime subprocess executes. It is deliberately
// distinct from internal/sandbox and the `sandbox` CLI, which mean Landlock
// LOCAL confinement and are unrelated to backend selection.
//
// P1 ships exactly one backend — "local" — which is a byte-for-byte
// passthrough to the existing runner-composition pipeline (caller runner or
// nil → runtimeJobRunner/credgw → maybe Landlock WrappingRunner → adapter,
// with subprocess.GroupRunner{} innermost). The job-scoped
// Provision/SyncIn/Exec/Collect/Destroy lifecycle from the P0 contract lands
// with the first real backend (P2); this package only names the selector and
// guarantees selection NEVER falls back silently: an unknown value is always a
// loud error naming the offending value and the allowed set.
package execbackend

import (
	"fmt"
	"strings"
)

// Backend selects where a job's runtime subprocess executes.
type Backend string

const (
	// Local is the default and — until a remote backend lands — the only
	// implemented execution backend. It resolves to today's behaviour exactly:
	// the existing runner composition with subprocess.GroupRunner{} innermost.
	Local Backend = "local"
)

// AllowedNames is the canonical allowed set of backend names, in the order
// error messages render them. Keep it in sync with the Parse switch — both
// are single-sourced here so a P2+ backend adds exactly one case and one
// entry.
var AllowedNames = []string{string(Local)}

// Allowed renders the allowed set for error messages (e.g. "local").
func Allowed() string {
	return strings.Join(AllowedNames, ", ")
}

// UnknownError builds the canonical fail-loud selection error: it ALWAYS
// names the offending value AND the allowed set, so an operator never has to
// guess which backends this binary implements.
func UnknownError(value string) error {
	return fmt.Errorf("unknown execution backend %q (allowed: %s)", value, Allowed())
}

// Parse validates one backend name. An empty (or whitespace-only) value
// resolves to Local — the absent-config/absent-override default. Any
// non-empty value outside the allowed set fails loud; there is no silent
// fallback.
func Parse(value string) (Backend, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return Local, nil
	}
	switch Backend(trimmed) {
	case Local:
		return Local, nil
	default:
		return "", UnknownError(trimmed)
	}
}

// Resolve picks the effective backend for one job: a non-empty per-job
// override wins over the config-file value; both empty means Local. The
// override is validated with the same fail-loud rule as the config value.
func Resolve(configBackend, jobOverride string) (Backend, error) {
	if strings.TrimSpace(jobOverride) != "" {
		backend, err := Parse(jobOverride)
		if err != nil {
			return "", fmt.Errorf("invalid exec_backend job override: %w", err)
		}
		return backend, nil
	}
	return Parse(configBackend)
}
