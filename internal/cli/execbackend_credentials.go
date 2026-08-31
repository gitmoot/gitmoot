package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

const (
	credentialGatewayConfigEnv = "GITMOOT_CREDENTIAL_GATEWAY_CURL_CONFIG"
	credentialGatewayURLEnv    = "GITMOOT_CREDENTIAL_GATEWAY_URL"
)

var (
	loadRemoteRuntimeAuth               = loadRuntimeAuthFile
	remoteModelGatewayAllowLoopbackHTTP bool
)

type remoteCredentialGatewayPlan struct {
	gateway      *credgw.Gateway
	home         string
	allowedHosts []string
}

// prepareRemoteCredentialGateway performs every check that can fail before a
// billed sandbox exists. It starts only the host listener and does not read the
// upstream provider credential.
func (w jobWorker) prepareRemoteCredentialGateway(remoteCfg config.RemoteExecConfig, ttl time.Duration) (remoteCredentialGatewayPlan, error) {
	paths, err := w.configPaths()
	if err != nil {
		return remoteCredentialGatewayPlan{}, err
	}
	credentialsCfg, err := config.LoadCredentialsConfig(paths)
	if errors.Is(err, os.ErrNotExist) {
		return remoteCredentialGatewayPlan{}, nil
	}
	if err != nil {
		return remoteCredentialGatewayPlan{}, fmt.Errorf("load credentials config: %w", err)
	}
	if !credentialsCfg.ModelGateway {
		return remoteCredentialGatewayPlan{}, nil
	}
	if strings.TrimSpace(remoteCfg.CredentialGatewayListen) == "" || strings.TrimSpace(remoteCfg.CredentialGatewayURL) == "" {
		return remoteCredentialGatewayPlan{}, errors.New("remote model gateway requires [remote_exec].credential_gateway_listen and credential_gateway_url")
	}
	if _, _, err := credgw.ValidateProxyPolicy(credgw.ProxyPolicy{
		Upstream: modelGatewayUpstreamURL, AuthKind: credgw.ProxyAuthResolved,
		AllowLoopbackHTTP: remoteModelGatewayAllowLoopbackHTTP,
		SandboxID:         "preflight", Runtime: runtime.ShellRuntime, ExpiresAt: time.Now().Add(ttl),
		AllowedHosts: append([]string(nil), credentialsCfg.ModelGatewayAllowHosts...),
	}); err != nil {
		return remoteCredentialGatewayPlan{}, fmt.Errorf("validate remote credential gateway policy: %w", err)
	}
	gateway, err := credgw.DefaultRegistry.RemoteGateway(paths.Home, credgw.DefaultLogf, credgw.RemoteListenerOptions{
		ListenAddress: remoteCfg.CredentialGatewayListen,
		AdvertiseURL:  remoteCfg.CredentialGatewayURL,
	})
	if err != nil {
		return remoteCredentialGatewayPlan{}, fmt.Errorf("start remote credential gateway: %w", err)
	}
	return remoteCredentialGatewayPlan{
		gateway: gateway, home: paths.Home,
		allowedHosts: append([]string(nil), credentialsCfg.ModelGatewayAllowHosts...),
	}, nil
}

// provisionRemoteCredentialGateway creates only the sandbox's ephemeral broker
// identity. The resolver closes over a host path and reads the provider key
// only after mTLS, capability, lease, runtime, and allowlist checks succeed.
func (w jobWorker) provisionRemoteCredentialGateway(ctx context.Context, backend execbackend.Backend, runtimeName, jobID string, ttl time.Duration, plan remoteCredentialGatewayPlan, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance) (*credgw.Lease, []string, error) {
	if backend != execbackend.Remote {
		return nil, nil, nil
	}
	if runtimeName != runtime.ShellRuntime {
		return nil, nil, fmt.Errorf("runtime %q cannot present the remote credential gateway mTLS identity; raw-key fallback is forbidden", runtimeName)
	}
	if plan.gateway == nil {
		return nil, nil, nil
	}
	policy := credgw.ProxyPolicy{
		Upstream: modelGatewayUpstreamURL, AuthKind: credgw.ProxyAuthResolved,
		AllowLoopbackHTTP: remoteModelGatewayAllowLoopbackHTTP,
		SandboxID:         instance.ID, Runtime: runtimeName, ExpiresAt: time.Now().Add(ttl),
		AllowedHosts: append([]string(nil), plan.allowedHosts...),
	}
	lease, err := plan.gateway.RegisterProxy(jobID, policy, lazyModelGatewayResolver(plan.home))
	if err != nil {
		return nil, nil, fmt.Errorf("register remote credential gateway lease: %w", err)
	}
	material := lease.RemoteMaterial()
	installer, ok := lifecycle.(execbackend.CredentialMaterialInstaller)
	if !ok {
		return lease, nil, fmt.Errorf("execution backend %q cannot install credential gateway material", lifecycle.Name())
	}
	clientConfig := material.CurlConfig(
		execbackend.CredentialCACertificatePath,
		execbackend.CredentialClientCertificatePath,
		execbackend.CredentialClientPrivateKeyPath,
	)
	err = installer.InstallCredentialMaterial(ctx, instance, execbackend.CredentialMaterial{
		CACertificate: material.CACertificate, ClientCertificate: material.ClientCertificate,
		ClientPrivateKey: material.ClientPrivateKey, ClientConfig: clientConfig,
	})
	if err != nil {
		return lease, nil, fmt.Errorf("install remote credential gateway material: %w", err)
	}
	return lease, []string{
		credentialGatewayConfigEnv + "=" + execbackend.CredentialClientConfigPath,
		credentialGatewayURLEnv + "=" + material.URL,
	}, nil
}

func lazyModelGatewayResolver(home string) credgw.CredentialResolver {
	return func(context.Context) (credgw.ResolvedCredential, error) {
		auth, err := loadRemoteRuntimeAuth(home)
		if err != nil {
			return credgw.ResolvedCredential{}, err
		}
		credential, err := modelGatewayCredential(auth)
		if err != nil {
			return credgw.ResolvedCredential{}, err
		}
		resolved := credgw.ResolvedCredential{Value: credential.Value, Upstream: modelGatewayUpstreamURL}
		switch credential.Kind {
		case credgw.CredentialAPIKey:
			resolved.AuthKind = credgw.ProxyAuthHeader
			resolved.Header = "X-Api-Key"
		case credgw.CredentialBearer:
			resolved.AuthKind = credgw.ProxyAuthBearer
		default:
			return credgw.ResolvedCredential{}, errors.New("unsupported remote model credential kind")
		}
		return resolved, nil
	}
}

// credentialRevokingExecutionBackend makes route revocation structural for
// normal teardown, cancellation, and provider startup reap. Revocation runs
// even when provider deletion returns an error.
type credentialRevokingExecutionBackend struct {
	inner execbackend.ExecutionBackend
	home  string
}

func (b *credentialRevokingExecutionBackend) Name() execbackend.Backend { return b.inner.Name() }
func (b *credentialRevokingExecutionBackend) Provision(ctx context.Context, scope execbackend.JobScope) (*execbackend.Instance, error) {
	return b.inner.Provision(ctx, scope)
}
func (b *credentialRevokingExecutionBackend) Attach(ctx context.Context, id string) (*execbackend.Instance, error) {
	return b.inner.Attach(ctx, id)
}
func (b *credentialRevokingExecutionBackend) SyncIn(ctx context.Context, instance *execbackend.Instance, material execbackend.Materials) error {
	return b.inner.SyncIn(ctx, instance, material)
}
func (b *credentialRevokingExecutionBackend) InstallCredentialMaterial(ctx context.Context, instance *execbackend.Instance, material execbackend.CredentialMaterial) error {
	installer, ok := b.inner.(execbackend.CredentialMaterialInstaller)
	if !ok {
		return fmt.Errorf("execution backend %q cannot install credential gateway material", b.inner.Name())
	}
	return installer.InstallCredentialMaterial(ctx, instance, material)
}
func (b *credentialRevokingExecutionBackend) Exec(ctx context.Context, instance *execbackend.Instance, command execbackend.Command) (execbackend.Stream, error) {
	return b.inner.Exec(ctx, instance, command)
}
func (b *credentialRevokingExecutionBackend) Collect(ctx context.Context, instance *execbackend.Instance) (execbackend.ChangeSet, error) {
	return b.inner.Collect(ctx, instance)
}
func (b *credentialRevokingExecutionBackend) Cancel(ctx context.Context, instance *execbackend.Instance) error {
	defer b.revoke(instance)
	return b.inner.Cancel(ctx, instance)
}
func (b *credentialRevokingExecutionBackend) Destroy(ctx context.Context, instance *execbackend.Instance) error {
	defer b.revoke(instance)
	return b.inner.Destroy(ctx, instance)
}
func (b *credentialRevokingExecutionBackend) Reap(ctx context.Context) ([]string, error) {
	report, err := b.ReapInventory(ctx)
	return report.Destroyed, err
}
func (b *credentialRevokingExecutionBackend) ReapInventory(ctx context.Context) (execbackend.ReapReport, error) {
	reaper, ok := b.inner.(execbackend.InventoryReaper)
	if !ok {
		return execbackend.ReapReport{}, fmt.Errorf("execution backend %q does not expose provider inventory", b.inner.Name())
	}
	report, err := reaper.ReapInventory(ctx)
	for _, sandboxID := range report.Destroyed {
		credgw.DefaultRegistry.RevokeSandbox(b.home, sandboxID)
	}
	return report, err
}
func (b *credentialRevokingExecutionBackend) revoke(instance *execbackend.Instance) {
	if instance != nil {
		credgw.DefaultRegistry.RevokeSandbox(b.home, instance.ID)
	}
}
