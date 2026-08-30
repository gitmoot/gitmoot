package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

const remoteBrokerTestKey = "anthropic-key-never-enters-sandbox-GITMOOT-IMPL"

func TestRemoteCredentialGatewaySuccessfulCallAndAllTeardownRevocation(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if got := r.Header.Get("X-Api-Key"); got != remoteBrokerTestKey {
			t.Fatalf("upstream key length = %d, want %d", len(got), len(remoteBrokerTestKey))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "brokered-success")
	}))
	defer upstream.Close()

	baseHome := t.TempDir()
	paths := config.PathsForHome(baseHome)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(baseHome, "e2b-key")
	if err := os.WriteFile(keyFile, []byte("e2b-control-key-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, runtimeAuthFileName), []byte(runtime.AnthropicAPIKeyEnv+"="+remoteBrokerTestKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listenAddress := unusedLoopbackAddress(t)
	configBody := fmt.Sprintf(`[credentials]
model_gateway = true
model_gateway_allow_hosts = ["127.0.0.1"]

[remote_exec]
backend = "remote"
e2b_api_key_file = %q
e2b_template = "template-test"
credential_gateway_listen = %q
credential_gateway_url = %q
`, keyFile, listenAddress, "https://"+listenAddress)
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	originalRegistry := credgw.DefaultRegistry
	originalUpstream := modelGatewayUpstreamURL
	originalLoad := loadRemoteRuntimeAuth
	originalLoopback := remoteModelGatewayAllowLoopbackHTTP
	credgw.DefaultRegistry = credgw.NewRegistry()
	modelGatewayUpstreamURL = upstream.URL
	remoteModelGatewayAllowLoopbackHTTP = true
	var authLoads atomic.Int32
	loadRemoteRuntimeAuth = func(home string) (runtimeAuthFile, error) {
		authLoads.Add(1)
		return loadRuntimeAuthFile(home)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = credgw.DefaultRegistry.CloseHome(ctx, paths.Home)
		credgw.DefaultRegistry = originalRegistry
		modelGatewayUpstreamURL = originalUpstream
		loadRemoteRuntimeAuth = originalLoad
		remoteModelGatewayAllowLoopbackHTTP = originalLoopback
	})

	inner := &credentialTestBackend{instance: &execbackend.Instance{ID: "sandbox-brokered", JobID: "job-brokered", Workspace: "/home/user/workspace"}, destroyErr: errors.New("provider delete failed")}
	lifecycle := &credentialRevokingExecutionBackend{inner: inner, home: paths.Home}
	worker := jobWorker{
		ConfigHome: baseHome, ConfigHomeExplicit: true,
		ExecutionBackendFactory: func(execbackend.Backend) (execbackend.ExecutionBackend, error) { return lifecycle, nil },
	}
	gotLifecycle, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, runtime.ShellRuntime, db.Job{ID: "job-brokered"}, time.Minute, "/checkout")
	if err != nil {
		t.Fatal(err)
	}
	if gotLifecycle != lifecycle || instance != inner.instance || lease == nil {
		t.Fatalf("provision result lifecycle=%T instance=%+v lease=%v", gotLifecycle, instance, lease)
	}
	if len(env) != 2 || env[0] != credentialGatewayConfigEnv+"="+execbackend.CredentialClientConfigPath || env[1] != credentialGatewayURLEnv+"="+lease.RemoteMaterial().URL {
		t.Fatalf("broker env = %v", env)
	}
	installedMaterial := bytes.Join([][]byte{
		inner.material.CACertificate,
		inner.material.ClientCertificate,
		inner.material.ClientPrivateKey,
		inner.material.ClientConfig,
		[]byte(strings.Join(env, "\n")),
	}, nil)
	if inner.installs != 1 || bytes.Contains(installedMaterial, []byte(remoteBrokerTestKey)) {
		t.Fatalf("installed broker material count=%d contains provider key=%v", inner.installs, bytes.Contains(installedMaterial, []byte(remoteBrokerTestKey)))
	}
	if got := authLoads.Load(); got != 0 {
		t.Fatalf("provider credential loaded during provisioning: %d", got)
	}

	material := lease.RemoteMaterial()
	client := credentialMaterialHTTPClient(t, material)
	call := func(capability string) int {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, material.URL+"/v1/messages", nil)
		if capability != "" {
			request.Header.Set(credgw.CapabilityHeader, capability)
		}
		request.Header.Set("Authorization", "Bearer "+material.Placeholder)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if status := call(""); status != http.StatusUnauthorized || authLoads.Load() != 0 || upstreamCalls.Load() != 0 {
		t.Fatalf("missing capability status=%d auth-loads=%d upstream=%d", status, authLoads.Load(), upstreamCalls.Load())
	}
	if status := call(material.Capability); status != http.StatusCreated || authLoads.Load() != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("brokered call status=%d auth-loads=%d upstream=%d", status, authLoads.Load(), upstreamCalls.Load())
	}

	if err := lifecycle.Destroy(context.Background(), instance); err == nil || !strings.Contains(err.Error(), "provider delete failed") {
		t.Fatalf("Destroy error = %v", err)
	}
	if status := call(material.Capability); status != http.StatusUnauthorized || authLoads.Load() != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("post-teardown status=%d auth-loads=%d upstream=%d", status, authLoads.Load(), upstreamCalls.Load())
	}

	gateway, err := credgw.DefaultRegistry.RemoteGateway(paths.Home, nil, credgw.RemoteListenerOptions{ListenAddress: listenAddress, AdvertiseURL: "https://" + listenAddress})
	if err != nil {
		t.Fatal(err)
	}
	cancelLease, err := gateway.RegisterProxy("job-cancelled", credgw.ProxyPolicy{
		Upstream: upstream.URL, AuthKind: credgw.ProxyAuthResolved, AllowLoopbackHTTP: true,
		SandboxID: "sandbox-cancelled", Runtime: runtime.ShellRuntime, ExpiresAt: time.Now().Add(time.Minute), AllowedHosts: []string{"127.0.0.1"},
	}, lazyModelGatewayResolver(paths.Home))
	if err != nil {
		t.Fatal(err)
	}
	cancelMaterial := cancelLease.RemoteMaterial()
	if err := lifecycle.Cancel(context.Background(), &execbackend.Instance{ID: "sandbox-cancelled"}); err == nil || !strings.Contains(err.Error(), "provider delete failed") {
		t.Fatalf("Cancel error = %v", err)
	}
	request, _ := http.NewRequest(http.MethodGet, cancelMaterial.URL, nil)
	request.Header.Set(credgw.CapabilityHeader, cancelMaterial.Capability)
	request.Header.Set("Authorization", "Bearer "+cancelMaterial.Placeholder)
	response, err := credentialMaterialHTTPClient(t, cancelMaterial).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || authLoads.Load() != 1 {
		t.Fatalf("post-cancel status=%d auth-loads=%d", response.StatusCode, authLoads.Load())
	}

	reapLease, err := gateway.RegisterProxy("job-reaped", credgw.ProxyPolicy{
		Upstream: upstream.URL, AuthKind: credgw.ProxyAuthResolved, AllowLoopbackHTTP: true,
		SandboxID: "sandbox-reaped", Runtime: runtime.ShellRuntime, ExpiresAt: time.Now().Add(time.Minute), AllowedHosts: []string{"127.0.0.1"},
	}, lazyModelGatewayResolver(paths.Home))
	if err != nil {
		t.Fatal(err)
	}
	reapMaterial := reapLease.RemoteMaterial()
	inner.report = execbackend.ReapReport{InventoryObserved: true, Destroyed: []string{"sandbox-reaped"}}
	inner.reportErr = errors.New("provider reap partially failed")
	if _, err := lifecycle.Reap(context.Background()); err == nil || !strings.Contains(err.Error(), "provider reap partially failed") {
		t.Fatalf("Reap error = %v", err)
	}
	request, _ = http.NewRequest(http.MethodGet, reapMaterial.URL, nil)
	request.Header.Set(credgw.CapabilityHeader, reapMaterial.Capability)
	request.Header.Set("Authorization", "Bearer "+reapMaterial.Placeholder)
	response, err = credentialMaterialHTTPClient(t, reapMaterial).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || authLoads.Load() != 1 {
		t.Fatalf("post-reap status=%d auth-loads=%d", response.StatusCode, authLoads.Load())
	}
}

func TestRemoteModelRuntimeRefusesBeforeProviderSpend(t *testing.T) {
	var factoryCalls atomic.Int32
	worker := jobWorker{ExecutionBackendFactory: func(execbackend.Backend) (execbackend.ExecutionBackend, error) {
		factoryCalls.Add(1)
		return nil, errors.New("must not construct")
	}}
	_, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, runtime.ClaudeRuntime, db.Job{ID: "job-no-fallback"}, time.Minute, "/checkout")
	if err == nil || !strings.Contains(err.Error(), "raw-key fallback is forbidden") || instance != nil || lease != nil || len(env) != 0 || factoryCalls.Load() != 0 {
		t.Fatalf("refusal instance=%+v lease=%v env=%v factory=%d err=%v", instance, lease, env, factoryCalls.Load(), err)
	}
}

func TestRemoteCredentialGatewayConfigurationRefusesBeforeProviderSpend(t *testing.T) {
	for _, test := range []struct {
		name              string
		gatewayConfig     func(*testing.T) string
		wantErrorContains string
	}{
		{name: "missing listener pair", wantErrorContains: "credential_gateway_listen and credential_gateway_url"},
		{name: "listener cannot bind", gatewayConfig: occupiedGatewayConfig, wantErrorContains: "start remote credential gateway"},
	} {
		t.Run(test.name, func(t *testing.T) {
			baseHome := t.TempDir()
			paths := config.PathsForHome(baseHome)
			if err := os.MkdirAll(paths.Home, 0o700); err != nil {
				t.Fatal(err)
			}
			keyFile := filepath.Join(baseHome, "e2b-key")
			if err := os.WriteFile(keyFile, []byte("e2b-control-key-test"), 0o600); err != nil {
				t.Fatal(err)
			}
			gatewayConfig := ""
			if test.gatewayConfig != nil {
				gatewayConfig = test.gatewayConfig(t)
			}
			configBody := fmt.Sprintf(`[credentials]
model_gateway = true

[remote_exec]
backend = "remote"
e2b_api_key_file = %q
e2b_template = "template-test"
%s
`, keyFile, gatewayConfig)
			if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
				t.Fatal(err)
			}
			var factoryCalls atomic.Int32
			worker := jobWorker{
				ConfigHome: baseHome, ConfigHomeExplicit: true,
				ExecutionBackendFactory: func(execbackend.Backend) (execbackend.ExecutionBackend, error) {
					factoryCalls.Add(1)
					return nil, errors.New("must not construct")
				},
			}
			_, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, runtime.ShellRuntime, db.Job{ID: "job-invalid-gateway"}, time.Minute, "/checkout")
			if err == nil || !strings.Contains(err.Error(), test.wantErrorContains) || instance != nil || lease != nil || len(env) != 0 || factoryCalls.Load() != 0 {
				t.Fatalf("preflight instance=%+v lease=%v env=%v factory=%d err=%v", instance, lease, env, factoryCalls.Load(), err)
			}
		})
	}
}

func occupiedGatewayConfig(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().String()
	return fmt.Sprintf("credential_gateway_listen = %q\ncredential_gateway_url = %q", address, "https://"+address)
}

func TestRemoteCredentialMaterialTraversesLifecycleWrappers(t *testing.T) {
	inner := &credentialTestBackend{}
	ledgered := &ledgeredExecutionBackend{inner: inner}
	lifecycle := &credentialRevokingExecutionBackend{inner: ledgered}
	material := execbackend.CredentialMaterial{
		CACertificate: []byte("ca"), ClientCertificate: []byte("certificate"),
		ClientPrivateKey: []byte("ephemeral-private-key"), ClientConfig: []byte("curl-config"),
	}
	if err := lifecycle.InstallCredentialMaterial(context.Background(), &execbackend.Instance{ID: "sandbox-wrapped"}, material); err != nil {
		t.Fatal(err)
	}
	if materialMatch := reflect.DeepEqual(inner.material, material); inner.installs != 1 || !materialMatch {
		t.Fatalf("wrapped install count=%d material-match=%t", inner.installs, materialMatch)
	}
}

type credentialTestBackend struct {
	instance   *execbackend.Instance
	material   execbackend.CredentialMaterial
	installs   int
	destroyErr error
	report     execbackend.ReapReport
	reportErr  error
}

func (*credentialTestBackend) Name() execbackend.Backend { return execbackend.Remote }
func (b *credentialTestBackend) Provision(context.Context, execbackend.JobScope) (*execbackend.Instance, error) {
	return b.instance, nil
}
func (*credentialTestBackend) Attach(context.Context, string) (*execbackend.Instance, error) {
	return nil, errors.New("not attached")
}
func (*credentialTestBackend) SyncIn(context.Context, *execbackend.Instance, execbackend.Materials) error {
	return nil
}
func (b *credentialTestBackend) InstallCredentialMaterial(_ context.Context, _ *execbackend.Instance, material execbackend.CredentialMaterial) error {
	b.installs++
	b.material = material
	return nil
}
func (*credentialTestBackend) Exec(context.Context, *execbackend.Instance, execbackend.Command) (execbackend.Stream, error) {
	return nil, errors.New("not executed")
}
func (*credentialTestBackend) Collect(context.Context, *execbackend.Instance) (execbackend.ChangeSet, error) {
	return execbackend.ChangeSet{}, nil
}
func (b *credentialTestBackend) Cancel(context.Context, *execbackend.Instance) error {
	return b.destroyErr
}
func (b *credentialTestBackend) Destroy(context.Context, *execbackend.Instance) error {
	return b.destroyErr
}
func (b *credentialTestBackend) ReapInventory(context.Context) (execbackend.ReapReport, error) {
	return b.report, b.reportErr
}

func unusedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func credentialMaterialHTTPClient(t *testing.T, material credgw.RemoteMaterial) *http.Client {
	t.Helper()
	certificate, err := tls.X509KeyPair(material.ClientCertificate, material.ClientPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(material.CACertificate) {
		t.Fatal("append gateway CA")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate},
	}}}
}
