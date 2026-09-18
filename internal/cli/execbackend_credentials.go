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
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/pipeline"
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
	gateway *credgw.Gateway
	home    string
	// keyName names the proxied keychain key supplying the upstream and
	// credential. Empty keeps the Anthropic default read from runtime-auth.env,
	// so an existing configuration behaves exactly as before.
	keyName       string
	allowLoopback bool
	upstream      string
	authKind      credgw.ProxyAuthKind
	authHeader    string
	allowedHosts  []string
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
	// A named keychain key supplies its OWN upstream and auth, validated when the
	// operator ran `gitmoot key configure`. Resolving it here, before a billed
	// sandbox exists, keeps a misconfigured key a preflight failure rather than a
	// 401 discovered inside a running review.
	upstream := modelGatewayUpstreamURL
	authKind := credgw.ProxyAuthResolved
	authHeader := ""
	keyName := strings.TrimSpace(credentialsCfg.ModelGatewayKey)
	if keyName != "" {
		key, err := w.modelGatewayKeychainKey(keyName)
		if err != nil {
			return remoteCredentialGatewayPlan{}, err
		}
		upstream = key.ProxyUpstream
		authKind = credgw.ProxyAuthKind(key.ProxyAuthKind)
		authHeader = key.ProxyHeader
	}
	if _, _, err := credgw.ValidateProxyPolicy(credgw.ProxyPolicy{
		Upstream: upstream, AuthKind: authKind, Header: authHeader,
		AllowLoopbackHTTP: remoteModelGatewayAllowLoopbackHTTP || credentialsCfg.ModelGatewayAllowLoopbackUpstream,
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
		gateway: gateway, home: paths.Home, keyName: keyName,
		allowLoopback: credentialsCfg.ModelGatewayAllowLoopbackUpstream,
		upstream:      upstream, authKind: authKind, authHeader: authHeader,
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
		Upstream: plan.upstream, AuthKind: plan.authKind, Header: plan.authHeader,
		AllowLoopbackHTTP: remoteModelGatewayAllowLoopbackHTTP || plan.allowLoopback,
		SandboxID:         instance.ID, Runtime: runtimeName, ExpiresAt: time.Now().Add(ttl),
		AllowedHosts: append([]string(nil), plan.allowedHosts...),
	}
	resolver := lazyModelGatewayResolver(plan.home)
	if plan.keyName != "" {
		resolver = w.keychainModelGatewayResolver(plan.keyName)
	}
	lease, err := plan.gateway.RegisterProxy(jobID, policy, resolver)
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

// modelGatewayKeychainKey loads the proxied keychain key named by
// [credentials].model_gateway_key and refuses anything that cannot serve as an
// upstream credential.
//
// The checks are deliberately strict AT PREFLIGHT, before a billed sandbox
// exists: a key in the wrong mode, or proxied but never configured, is an
// operator mistake that should surface as a refusal to start rather than as a
// 401 discovered inside a running review.
func (w jobWorker) modelGatewayKeychainKey(name string) (db.KeychainKey, error) {
	if w.Store == nil {
		return db.KeychainKey{}, fmt.Errorf("model gateway key %q requires a store", name)
	}
	key, found, err := w.Store.GetKeychainKey(context.Background(), name)
	if err != nil {
		return db.KeychainKey{}, fmt.Errorf("load model gateway key %q: %w", name, err)
	}
	if !found {
		return db.KeychainKey{}, fmt.Errorf("model gateway key %q is not in the keychain; add it with `gitmoot key add %s`", name, name)
	}
	if key.Mode != db.KeychainModeProxied {
		return db.KeychainKey{}, fmt.Errorf("model gateway key %q has mode %q; it must be %q so the value never enters a sandbox", name, key.Mode, db.KeychainModeProxied)
	}
	if !key.ProxyConfigured() {
		return db.KeychainKey{}, fmt.Errorf("model gateway key %q is proxied but unconfigured; run `gitmoot key configure %s`", name, name)
	}
	return key, nil
}

// keychainModelGatewayResolver returns the upstream credential from the
// keychain, re-reading it on EVERY request.
//
// Re-reading is the point: a key revoked, reconfigured or removed mid-job must
// stop working immediately rather than at the next daemon restart. This mirrors
// the pipeline proxied-key resolver, which is the existing proven path for
// serving a keychain credential through the gateway.
func (w jobWorker) keychainModelGatewayResolver(name string) credgw.CredentialResolver {
	return func(ctx context.Context) (credgw.ResolvedCredential, error) {
		key, err := w.modelGatewayKeychainKey(name)
		if err != nil {
			return credgw.ResolvedCredential{}, err
		}
		_, values, err := pipeline.LoadValidatedKeychainFile(ctx, w.Store, w.ConfigHome)
		if err != nil {
			return credgw.ResolvedCredential{}, fmt.Errorf("load keychain for model gateway key %q: %w", name, err)
		}
		value := strings.TrimSpace(values[name])
		if value == "" {
			return credgw.ResolvedCredential{}, fmt.Errorf("model gateway key %q has no value in the keychain file", name)
		}
		return credgw.ResolvedCredential{
			Value: value, Upstream: key.ProxyUpstream,
			AuthKind: credgw.ProxyAuthKind(key.ProxyAuthKind), Header: key.ProxyHeader,
		}, nil
	}
}
