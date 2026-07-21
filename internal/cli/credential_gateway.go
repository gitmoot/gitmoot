package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

var (
	modelGatewayUpstreamURL = credgw.DefaultAnthropicUpstream
)

func buildModelGatewayRunner(home string, cfg config.CredentialsConfig, auth runtimeAuthFile, inner subprocess.Runner) (*credgw.Runner, error) {
	credential, err := modelGatewayCredential(auth)
	if err != nil {
		return nil, err
	}
	gateway, err := credgw.DefaultRegistry.Gateway(home, credgw.DefaultLogf)
	if err != nil {
		return nil, fmt.Errorf("start Claude model gateway: %w", err)
	}
	// Without a credential-free config dir the child reads ~/.claude/.credentials.json
	// and ignores the placeholder — the gateway would then 401 every job (#936).
	configDir, err := buildClaudeGatewayConfigDir(home)
	if err != nil {
		return nil, fmt.Errorf("prepare Claude gateway config dir: %w", err)
	}
	return &credgw.Runner{
		Inner:      inner,
		Gateway:    gateway,
		Credential: credential,
		Policy: credgw.Policy{
			Upstream:     modelGatewayUpstreamURL,
			AllowedHosts: append([]string{}, cfg.ModelGatewayAllowHosts...),
		},
		ChildConfigDir: configDir,
	}, nil
}

func modelGatewayCredential(auth runtimeAuthFile) (credgw.Credential, error) {
	if !auth.Exists || len(auth.Values) == 0 {
		return credgw.Credential{}, fmt.Errorf("Claude model gateway requires a populated %s", runtimeAuthFileName)
	}
	// Preserve Claude's effective preference when more than one managed value is
	// present: API key first, then Anthropic bearer token, then Claude OAuth.
	if value := strings.TrimSpace(auth.Values[runtime.AnthropicAPIKeyEnv]); value != "" {
		return credgw.Credential{Kind: credgw.CredentialAPIKey, Value: value}, nil
	}
	if value := strings.TrimSpace(auth.Values[runtime.AnthropicAuthTokenEnv]); value != "" {
		return credgw.Credential{Kind: credgw.CredentialBearer, Value: value}, nil
	}
	if value := strings.TrimSpace(auth.Values[runtime.ClaudeOAuthTokenEnv]); value != "" {
		return credgw.Credential{Kind: credgw.CredentialBearer, Value: value}, nil
	}
	return credgw.Credential{}, fmt.Errorf("Claude model gateway requires a populated %s", runtimeAuthFileName)
}

type modelGatewayRuntimeAdapter struct {
	runtime.Adapter
	runner *credgw.Runner
}

func (a modelGatewayRuntimeAdapter) Start(ctx context.Context, request runtime.StartRequest) (runtime.StartResult, error) {
	lease, err := a.runner.NewLease(modelGatewayLeaseID("start-"+request.Agent.Name, ""))
	if err != nil {
		return runtime.StartResult{}, err
	}
	defer lease.Revoke()
	return a.Adapter.Start(credgw.WithLease(ctx, lease), request)
}

func (a modelGatewayRuntimeAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	lease, err := a.runner.NewLease(modelGatewayLeaseID(job.ID, agent.Name))
	if err != nil {
		return runtime.Result{}, err
	}
	defer lease.Revoke()
	return a.Adapter.Deliver(credgw.WithLease(ctx, lease), agent, job)
}

func modelGatewayLeaseID(primary, fallback string) string {
	if value := strings.TrimSpace(primary); value != "" {
		return value
	}
	if value := strings.TrimSpace(fallback); value != "" {
		return "delivery-" + value
	}
	return "runtime-delivery"
}

func wrapModelGatewayAdapter(adapter runtime.Adapter, runner *credgw.Runner) runtime.Adapter {
	if runner == nil {
		return adapter
	}
	return modelGatewayRuntimeAdapter{Adapter: adapter, runner: runner}
}

func closeModelGatewayHome(home string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = credgw.DefaultRegistry.CloseHome(ctx, home)
}
