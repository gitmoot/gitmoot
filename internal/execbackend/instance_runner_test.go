package execbackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type failingExecBackend struct {
	ExecutionBackend
	err error
}

func (b failingExecBackend) Exec(context.Context, *Instance, Command) (Stream, error) {
	return failingStream{err: b.err}, nil
}

type failingStream struct{ err error }

func (s failingStream) Wait() (ExecResult, error) { return ExecResult{}, s.err }

type exitedError struct{}

func (exitedError) Error() string { return "remote command exited with code 1" }
func (exitedError) ExitCode() int { return 1 }

// TestInstanceRunnerNamesSandboxLifetimeExpiry pins #2331: a cloud sandbox
// retired at the end of its fixed lifetime surfaces only as a dropped stream,
// which reads like transport loss. Once the lifetime has run out the failure
// must name the limit instead; before it, or when the process reported its own
// exit or the caller gave up, the original error stands.
func TestInstanceRunnerNamesSandboxLifetimeExpiry(t *testing.T) {
	lost := errors.New(`envd Start terminal error: {"error":{"code":"unavailable","message":"the connection to sandbox sbx ended before the stream completed"}}`)
	expired := time.Now().Add(-time.Hour - 2*time.Second)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name        string
		ctx         context.Context
		started     time.Time
		err         error
		wantExpired bool
	}{
		{name: "stream lost after the lifetime", ctx: context.Background(), started: expired, err: lost, wantExpired: true},
		{name: "stream lost well inside the lifetime", ctx: context.Background(), started: time.Now().Add(-10 * time.Minute), err: lost},
		{name: "process exited after the lifetime", ctx: context.Background(), started: expired, err: exitedError{}},
		{name: "caller cancelled after the lifetime", ctx: cancelled, started: expired, err: lost},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := InstanceRunner{
				Backend:  failingExecBackend{err: test.err},
				Instance: &Instance{ID: "sbx"},
				Lifetime: time.Hour,
				Started:  test.started,
			}
			_, err := runner.Run(test.ctx, "/home/user/workspace", "omp")
			var lifetime *SandboxLifetimeExceededError
			if got := errors.As(err, &lifetime); got != test.wantExpired {
				t.Fatalf("sandbox lifetime expiry = %v, want %v: %v", got, test.wantExpired, err)
			}
			if !errors.Is(err, test.err) {
				t.Fatalf("error %v does not wrap the original failure %v", err, test.err)
			}
			if test.wantExpired && (!strings.HasPrefix(err.Error(), "sandbox_ttl_exceeded: ") || !strings.Contains(err.Error(), "the provider's 1h sandbox limit")) {
				t.Fatalf("error = %q, want the sandbox_ttl_exceeded reason naming the 1h limit", err)
			}
		})
	}
}
