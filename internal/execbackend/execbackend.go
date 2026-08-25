// Package execbackend names the execution-backend seam (#1535 P0 contract,
// #1536 P1): WHERE a job's runtime subprocess executes. It is deliberately
// distinct from internal/sandbox and the `sandbox` CLI, which mean Landlock
// LOCAL confinement and are unrelated to backend selection.
//
// The default backend is "local". P1 introduced its selector as a passthrough;
// P2b adds the job-scoped lifecycle in lifecycle.go and executes a daemon job in
// a distinct same-filesystem Git worktree. Host-side checkout/finalizer commands
// retain their existing runner while runtime delivery uses InstanceRunner and
// returns changes through BuildChangeSet/ImportChangeSet.
// Where P1 carries a resolved selection into adapter construction, that route
// consumes it through Consume, whose only positive implementation is Local; any
// other parsed backend is refused at runtime. Adding a backend to
// ParseImplemented alone still compiles and then fails closed at consumption. If
// P2 extends Consume with a required positional builder, that signature change
// will make its existing callers fail to compile until they supply it. Adapter
// builds without a selector retain the Local default. Job-associated git,
// GitHub CLI, verifier, and read-only-diff subprocesses consume the resolved
// runner through Consume and fail closed when the backend has no execution
// implementation. Operator tooling and daemon supervision/session paths
// deliberately retain host-side runners. When supervision advances a completed
// job, it resolves that job's stored backend and rebuilds the workflow engine
// through the same Consume-backed runner seam before any delegation worktree or
// other job-associated subprocess can execute.
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
	// Remote reserves the brokered-credential classification for a future
	// execution backend. It is intentionally absent from AllowedNames until an
	// implementation exists, so Parse continues to reject it.
	Remote Backend = "remote"
)

// AllowedNames is the canonical allowed set of backend names, in the order
// error messages render them. Parse accepts names exclusively from this list,
// so a backend cannot be accepted without also being advertised.
var AllowedNames = []string{string(Local)}

// RequiresBrokeredCredentials classifies every declared execution backend.
// There is intentionally no default arm: a newly declared backend is
// unclassified until its credential handling is explicitly decided.
func RequiresBrokeredCredentials(backend Backend) (bool, error) {
	switch backend {
	case Local:
		return false, nil
	case Remote:
		return true, nil
	}
	return false, fmt.Errorf("execution backend %q has no brokered credential classification", backend)
}

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

// Parse validates one explicitly supplied backend name. Empty and
// whitespace-only values are invalid; callers represent an absent selector
// separately and apply Local as the default before calling Parse.
func Parse(value string) (Backend, error) {
	trimmed := strings.TrimSpace(value)
	for _, allowed := range AllowedNames {
		if trimmed == allowed {
			return Backend(trimmed), nil
		}
	}
	return "", UnknownError(trimmed)
}

// ParseImplemented validates both halves of the selector contract: the name
// must be advertised and this binary must have an execution implementation for
// it. Keeping this switch explicit prevents a newly advertised backend from
// inheriting Local's runner composition by omission.
func ParseImplemented(value string) (Backend, error) {
	backend, err := Parse(value)
	if err != nil {
		return "", err
	}
	switch backend {
	case Local:
		return backend, nil
	default:
		return "", fmt.Errorf("execution backend %q is advertised but not implemented", backend)
	}
}

// Consume dispatches an already-resolved backend to its execution
// implementation. The positive Local arm is deliberately separate from
// ParseImplemented: making a future backend parseable cannot make it inherit
// Local's runner pipeline. When P2 adds an implementation, it must add a
// positional builder here; changing this signature then makes every
// construction route fail to compile until it supplies that backend's builder.
func Consume[T any](backend Backend, local func() (T, error)) (T, error) {
	switch backend {
	case Local:
		return local()
	default:
		var zero T
		return zero, fmt.Errorf("execution backend %q has no execution implementation", backend)
	}
}

// Resolve picks the effective backend for one job. A present per-job override
// wins even when its value is blank, which then fails loudly. A nil override
// means absent. Empty configBackend is the absent-config default and resolves
// to Local; config loaders reject an explicitly configured blank before here.
func Resolve(configBackend string, jobOverride *string) (Backend, error) {
	if jobOverride != nil {
		backend, err := ParseImplemented(*jobOverride)
		if err != nil {
			return "", fmt.Errorf("invalid exec_backend job override: %w", err)
		}
		return backend, nil
	}
	if strings.TrimSpace(configBackend) == "" {
		configBackend = string(Local)
	}
	return ParseImplemented(configBackend)
}
