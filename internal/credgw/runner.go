package credgw

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const anthropicBaseURLEnv = "ANTHROPIC_BASE_URL"

type Lease struct {
	gateway     *Gateway
	placeholder string
}

func (l *Lease) Placeholder() string {
	if l == nil {
		return ""
	}
	return l.placeholder
}

func (l *Lease) Revoke() {
	if l != nil && l.gateway != nil {
		l.gateway.Revoke(l.placeholder)
	}
}

type leaseContextKey struct{}

type leaseContextValue struct {
	gateway     *Gateway
	placeholder string
}

func WithLease(ctx context.Context, lease *Lease) context.Context {
	if lease == nil {
		return ctx
	}
	return context.WithValue(ctx, leaseContextKey{}, leaseContextValue{
		gateway:     lease.gateway,
		placeholder: lease.placeholder,
	})
}

// Runner injects only the loopback route and per-job placeholder. The real
// credential remains in the gateway's in-memory lease entry.
type Runner struct {
	Inner      subprocess.Runner
	Gateway    *Gateway
	Credential Credential
	Policy     Policy
}

func (r *Runner) NewLease(jobID string) (*Lease, error) {
	if r == nil || r.Gateway == nil {
		return nil, errors.New("model gateway is not running")
	}
	placeholder, err := r.Gateway.Register(jobID, r.Credential, r.Policy)
	if err != nil {
		return nil, err
	}
	return &Lease{gateway: r.Gateway, placeholder: placeholder}, nil
}

func (r *Runner) Run(ctx context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	return r.runEnv(ctx, dir, nil, command, args...)
}

func (r *Runner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	return r.runEnv(ctx, dir, env, command, args...)
}

func (r *Runner) LookPath(file string) (string, error) {
	return r.inner().LookPath(file)
}

func (r *Runner) runEnv(ctx context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	placeholder, cleanup, err := r.placeholderForContext(ctx)
	if err != nil {
		return subprocess.Result{}, err
	}
	defer cleanup()
	gatewayEnv := []string{
		"CLAUDE_CODE_OAUTH_TOKEN=" + placeholder,
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_AUTH_TOKEN=",
		anthropicBaseURLEnv + "=" + r.Gateway.URL(),
	}
	merged := append(append([]string{}, env...), gatewayEnv...)
	inner, ok := r.inner().(subprocess.EnvRunner)
	if !ok {
		return subprocess.Result{}, errors.New("model gateway runner requires environment injection support")
	}
	return inner.RunEnv(ctx, dir, merged, command, args...)
}

func (r *Runner) placeholderForContext(ctx context.Context) (string, func(), error) {
	if value, ok := ctx.Value(leaseContextKey{}).(leaseContextValue); ok {
		if value.gateway != r.Gateway || value.placeholder == "" {
			return "", func() {}, errors.New("model gateway lease does not match runner")
		}
		return value.placeholder, func() {}, nil
	}
	lease, err := r.NewLease("runtime-call")
	if err != nil {
		return "", func() {}, fmt.Errorf("mint model gateway runtime lease: %w", err)
	}
	return lease.Placeholder(), lease.Revoke, nil
}

func (r *Runner) inner() subprocess.Runner {
	if r.Inner != nil {
		return r.Inner
	}
	return subprocess.GroupRunner{}
}
