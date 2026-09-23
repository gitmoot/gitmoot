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
e2b_omp_template = "omp-template-test"
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
		ExecutionBackendFactory: func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
			return lifecycle, nil
		},
	}
	gotLifecycle, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, executionBackendConfigForTest(t, worker), runtime.ShellRuntime, db.Job{ID: "job-brokered"}, time.Minute, "/checkout")
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

	hostOmp := filepath.Join(t.TempDir(), "omp")
	if err := os.WriteFile(hostOmp, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	originalLookPath := lookPathRemoteRuntime
	lookPathRemoteRuntime = func(string) (string, error) { return hostOmp, nil }
	t.Cleanup(func() { lookPathRemoteRuntime = originalLookPath })

	ompInner := &credentialTestBackend{instance: &execbackend.Instance{ID: "sandbox-omp", JobID: "job-omp", Workspace: "/home/user/workspace"}}
	ompLifecycle := &credentialRevokingExecutionBackend{inner: ompInner, home: paths.Home}
	ompWorker := worker
	ompWorker.ExecutionBackendFactory = func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		return ompLifecycle, nil
	}
	_, ompInstance, ompLease, ompEnv, err := ompWorker.provisionExecutionBackend(context.Background(), execbackend.Remote, executionBackendConfigForTest(t, ompWorker), runtime.OmpRuntime, db.Job{ID: "job-omp"}, time.Minute, "/checkout")
	if err != nil {
		t.Fatalf("provision remote omp: %v", err)
	}
	if ompInstance != ompInner.instance || ompLease == nil || ompInner.installs != 1 {
		t.Fatalf("remote omp instance=%+v lease=%v credential-installs=%d", ompInstance, ompLease, ompInner.installs)
	}
	if ompInner.runtimeFiles[execbackend.RuntimeOmpExecutablePath] != 0o700 ||
		ompInner.runtimeFiles[remoteOmpForwarderPath] != 0o700 ||
		ompInner.runtimeFiles[remoteOmpModelsPath] != 0o600 {
		t.Fatalf("remote omp runtime files = %v", ompInner.runtimeFiles)
	}
	if len(ompInner.execCalls) != 1 || ompInner.execCalls[0].Name != "python3" {
		t.Fatalf("remote omp forwarder starts = %+v", ompInner.execCalls)
	}
	if args := strings.Join(ompInner.execCalls[0].Args, " "); !strings.Contains(args, "--curl-config "+execbackend.CredentialClientConfigPath) {
		t.Fatalf("remote omp forwarder args omit capability config: %v", ompInner.execCalls[0].Args)
	}
	joinedOmpEnv := strings.Join(ompEnv, "\n")
	for _, want := range []string{
		credentialGatewayConfigEnv + "=" + execbackend.CredentialClientConfigPath,
		credentialGatewayURLEnv + "=" + ompLease.RemoteMaterial().URL,
		"ANTHROPIC_BASE_URL=" + remoteOmpForwarderURL,
		"ANTHROPIC_API_KEY=gitmoot-job-gateway",
		"PI_CODING_AGENT_DIR=" + remoteOmpAgentDir,
		"PATH=" + remoteOmpRuntimePATH,
	} {
		if !strings.Contains(joinedOmpEnv, want) {
			t.Fatalf("remote omp env missing %q: %v", want, ompEnv)
		}
	}
	if catalog := ompInner.runtimeContents[remoteOmpModelsPath]; !bytes.Contains(catalog, []byte("devin:")) ||
		!bytes.Contains(catalog, []byte("id: swe-2")) ||
		!bytes.Contains(catalog, []byte("openai-codex:")) ||
		!bytes.Contains(catalog, []byte("transport: pi-native")) ||
		!bytes.Contains(catalog, []byte(remoteOmpForwarderURL)) {
		t.Fatalf("remote omp fallback catalog is not routed through the job gateway: %s", catalog)
	}
	if bytes.Contains(bytes.Join([][]byte{
		ompInner.material.CACertificate,
		ompInner.material.ClientCertificate,
		ompInner.material.ClientPrivateKey,
		ompInner.material.ClientConfig,
		[]byte(joinedOmpEnv),
		ompInner.runtimeContents[remoteOmpModelsPath],
	}, nil), []byte(remoteBrokerTestKey)) {
		t.Fatal("remote omp sandbox material contains the provider credential")
	}
	t.Cleanup(func() { _ = ompLifecycle.Destroy(context.Background(), ompInstance) })

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
	worker := jobWorker{ExecutionBackendFactory: func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		factoryCalls.Add(1)
		return nil, errors.New("must not construct")
	}}
	_, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, executionBackendConfigForTest(t, worker), runtime.ClaudeRuntime, db.Job{ID: "job-no-fallback"}, time.Minute, "/checkout")
	if err == nil || !strings.Contains(err.Error(), `runtime "claude" is not supported`) || !strings.Contains(err.Error(), "supported runtimes are shell and omp") || instance != nil || lease != nil || len(env) != 0 || factoryCalls.Load() != 0 {
		t.Fatalf("refusal instance=%+v lease=%v env=%v factory=%d err=%v", instance, lease, env, factoryCalls.Load(), err)
	}
}

func TestRemoteOmpRequiresDedicatedTemplateBeforeProviderSpend(t *testing.T) {
	var factoryCalls atomic.Int32
	worker := jobWorker{ExecutionBackendFactory: func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		factoryCalls.Add(1)
		return nil, errors.New("must not construct")
	}}
	cfg := config.DefaultRemoteExecConfig()
	cfg.E2BTemplate = "shell-template"
	_, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, cfg, runtime.OmpRuntime, db.Job{ID: "job-omp-no-template"}, time.Minute, "/checkout")
	if err == nil || !strings.Contains(err.Error(), "e2b_omp_template") || instance != nil || lease != nil || len(env) != 0 || factoryCalls.Load() != 0 {
		t.Fatalf("refusal instance=%+v lease=%v env=%v factory=%d err=%v", instance, lease, env, factoryCalls.Load(), err)
	}
}

func TestRemoteOmpRequiresGatewayBeforeProviderSpend(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatal(err)
	}
	var factoryCalls atomic.Int32
	worker := jobWorker{ConfigHome: home, ConfigHomeExplicit: true, ExecutionBackendFactory: func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		factoryCalls.Add(1)
		return nil, errors.New("must not construct")
	}}
	cfg := executionBackendConfigForTest(t, worker)
	cfg.E2BOMPTemplate = "omp-template-test"
	_, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, cfg, runtime.OmpRuntime, db.Job{ID: "job-omp-no-gateway"}, time.Minute, "/checkout")
	if err == nil || !strings.Contains(err.Error(), "remote omp requires the model credential gateway") || instance != nil || lease != nil || len(env) != 0 || factoryCalls.Load() != 0 {
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
				ExecutionBackendFactory: func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
					factoryCalls.Add(1)
					return nil, errors.New("must not construct")
				},
			}
			_, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Remote, executionBackendConfigForTest(t, worker), runtime.ShellRuntime, db.Job{ID: "job-invalid-gateway"}, time.Minute, "/checkout")
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
	if _, err := lifecycle.InstallInstanceFile(context.Background(), &execbackend.Instance{ID: "sandbox-wrapped"}, execbackend.RuntimeOmpExecutablePath, strings.NewReader("omp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if inner.runtimeFiles[execbackend.RuntimeOmpExecutablePath] != 0o700 {
		t.Fatalf("wrapped runtime files = %v", inner.runtimeFiles)
	}
}

type credentialTestBackend struct {
	instance        *execbackend.Instance
	material        execbackend.CredentialMaterial
	installs        int
	runtimeFiles    map[string]os.FileMode
	runtimeContents map[string][]byte
	execCalls       []execbackend.Command
	destroyErr      error
	report          execbackend.ReapReport
	reportErr       error
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
func (b *credentialTestBackend) InstallInstanceFile(_ context.Context, _ *execbackend.Instance, destination string, reader io.Reader, mode os.FileMode) (string, error) {
	if b.runtimeFiles == nil {
		b.runtimeFiles = make(map[string]os.FileMode)
		b.runtimeContents = make(map[string][]byte)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	b.runtimeFiles[destination] = mode
	b.runtimeContents[destination] = content
	return destination, nil
}
func (b *credentialTestBackend) Exec(_ context.Context, _ *execbackend.Instance, command execbackend.Command) (execbackend.Stream, error) {
	b.execCalls = append(b.execCalls, command)
	return credentialTestStream{result: execbackend.ExecResult{Stdout: "123\n"}}, nil
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

type credentialTestStream struct {
	result execbackend.ExecResult
	err    error
}

func (s credentialTestStream) Wait() (execbackend.ExecResult, error) {
	return s.result, s.err
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
