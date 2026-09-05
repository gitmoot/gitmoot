package cli

import (
	"context"
	"errors"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"io"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

type p2ProbeSubprocessRunner struct {
	calls int
}

func (r *p2ProbeSubprocessRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	r.calls++
	return subprocess.Result{}, errP2ProbeSubprocessReached
}

func (r *p2ProbeSubprocessRunner) RunEnv(ctx context.Context, dir string, _ []string, command string, args ...string) (subprocess.Result, error) {
	return r.Run(ctx, dir, command, args...)
}

func (r *p2ProbeSubprocessRunner) RunExactEnv(ctx context.Context, dir string, _ []string, _, _ io.Writer, command string, args ...string) error {
	_, err := r.Run(ctx, dir, command, args...)
	return err
}

func (r *p2ProbeSubprocessRunner) LookPath(string) (string, error) {
	r.calls++
	return "", errP2ProbeSubprocessReached
}

var errP2ProbeSubprocessReached = errors.New("p2-probe subprocess runner reached")
