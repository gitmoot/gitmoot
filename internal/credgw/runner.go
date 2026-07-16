package credgw

import (
	"context"
	"errors"
	"fmt"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const (
	anthropicBaseURLEnv = "ANTHROPIC_BASE_URL"
	// claudeConfigDirEnv points the Claude CLI at a config directory. The gateway
	// sets it to a store WITHOUT a cached credential so the child authenticates
	// from the injected placeholder instead of ~/.claude/.credentials.json, which
	// Claude otherwise prefers over CLAUDE_CODE_OAUTH_TOKEN (#936).
	claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"
)

type Lease struct {
	gateway     *Gateway
	placeholder string
	route       string
}

func (l *Lease) URL() string {
	if l == nil || l.gateway == nil || l.route == "" {
		return ""
	}
	return l.gateway.URL() + l.route
}

func (l *Lease) Placeholder() string {
	if l == nil {
		return ""
	}
	return l.placeholder
}

func (l *Lease) Revoke() {
	if l != nil && l.gateway != nil {
		if l.route != "" {
			l.gateway.revokeProxy(l.route, l.placeholder)
		} else {
			l.gateway.Revoke(l.placeholder)
		}
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

// Runner injects the loopback route, the per-job placeholder, and — so the child
// cannot fall back to a cached credential store — a credential-free
// CLAUDE_CONFIG_DIR. The real credential remains in the gateway's in-memory lease
// entry.
type Runner struct {
	Inner      subprocess.Runner
	Gateway    *Gateway
	Credential Credential
	Policy     Policy
	// ChildConfigDir, when set, is injected as CLAUDE_CONFIG_DIR. It must be a
	// Claude config directory that contains no .credentials.json, or the child
	// will authenticate from the cached credential and ignore the placeholder
	// (#936). Empty leaves CLAUDE_CONFIG_DIR untouched (pre-#936 behavior).
	ChildConfigDir string
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
	if r.ChildConfigDir != "" {
		// Last, so it wins over any CLAUDE_CONFIG_DIR the caller passed in env.
		gatewayEnv = append(gatewayEnv, claudeConfigDirEnv+"="+r.ChildConfigDir)
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
