package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// sandboxdLinuxGuestGateway is the host credential gateway as a Linux
// Firecracker guest sees it: slirp4netns's host alias 10.0.2.2, which the
// gateway forwards to the host's loopback.
const sandboxdLinuxGuestGatewayHost = "10.0.2.2"

const (
	testELFMachineAARCH64 = 183
	testELFMachineX86_64  = 62
)

// writeTestOmpELF writes a minimal Linux ELF64 executable header for machine,
// distinct per call so an upload identifies which file it came from.
func writeTestOmpELF(t *testing.T, dir, name string, machine uint16) (string, []byte) {
	t.Helper()
	header := make([]byte, 64)
	copy(header, "\x7fELF")
	header[4], header[5], header[6], header[7] = 2, 1, 1, 3
	binary.LittleEndian.PutUint16(header[16:], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(header[18:], machine)
	binary.LittleEndian.PutUint32(header[20:], 1)
	binary.LittleEndian.PutUint16(header[52:], 64)
	header = append(header, []byte(name)...)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, header, 0o700); err != nil {
		t.Fatal(err)
	}
	return path, header
}

// sandboxdPairHome is a home that declares cloud E2B, the Mac gateway
// [remote_exec.sandboxd] and the Linux gateway [remote_exec."sandboxd-linux"]
// side by side, each provider with its own endpoint, key, template, ceiling,
// guest-visible credential gateway origin and OMP upload.
type sandboxdPairHome struct {
	home          string
	paths         config.Paths
	listenAddress string
	linuxGateway  string
	macOmp        []byte
	linuxOmp      []byte
}

func writeSandboxdPairHome(t *testing.T, macBase, linuxBase string, macCeiling, linuxCeiling int) sandboxdPairHome {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for name, key := range map[string]string{"e2b": "private-e2b-key", "sandboxd": fakeSandboxdAPIKey, "sandboxd-linux": fakeSandboxdAPIKey} {
		keys[name] = filepath.Join(home, name+"-api-key")
		if err := os.WriteFile(keys[name], []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	macOmpPath, macOmp := writeTestOmpELF(t, home, "omp-linux-arm64", testELFMachineAARCH64)
	linuxOmpPath, linuxOmp := writeTestOmpELF(t, home, "omp-linux-amd64", testELFMachineX86_64)
	listenAddress := unusedLoopbackAddress(t)
	_, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		t.Fatal(err)
	}
	linuxGateway := "https://" + net.JoinHostPort(sandboxdLinuxGuestGatewayHost, port)
	body := fmt.Sprintf(`
[credentials]
model_gateway = true
model_gateway_allow_hosts = ["127.0.0.1"]

[remote_exec]
backend = "local"
e2b_api_key_file = %q
e2b_template = "base"
credential_gateway_listen = %q
credential_gateway_url = %q

[remote_exec.sandboxd]
api_key_file = %q
template = "review-arm64"
omp_template = "review-arm64"
base_url = %q
envd_base_url = %q
omp_linux_arm64_file = %q
credential_gateway_url = "https://192.168.128.1:43181"
max_concurrent = %d

[remote_exec."sandboxd-linux"]
api_key_file = %q
template = "review-amd64"
omp_template = "review-amd64"
base_url = %q
envd_base_url = %q
omp_linux_amd64_file = %q
credential_gateway_url = %q
max_concurrent = %d
`, keys["e2b"], listenAddress, "https://"+listenAddress,
		keys["sandboxd"], macBase, macBase, macOmpPath, macCeiling,
		keys["sandboxd-linux"], linuxBase, linuxBase, linuxOmpPath, linuxGateway, linuxCeiling)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return sandboxdPairHome{home: home, paths: paths, listenAddress: listenAddress, linuxGateway: linuxGateway, macOmp: macOmp, linuxOmp: linuxOmp}
}

// sandboxdPairJob seeds a running review job that opted into provider and
// returns the backend and provider view the daemon resolves for it.
func sandboxdPairJob(t *testing.T, worker jobWorker, id, provider string) (db.Job, execbackend.Backend, config.RemoteExecConfig) {
	t.Helper()
	ctx := context.Background()
	payload := fmt.Sprintf(`{"repo":"o/r","exec_backend":"remote","exec_provider":%q}`, provider)
	if err := worker.Store.CreateJobWithEvent(ctx, db.Job{ID: id, Agent: "a", Type: "implement", State: string(workflow.JobRunning), Payload: payload},
		db.JobEvent{Kind: string(workflow.JobRunning), Message: "seed"}); err != nil {
		t.Fatal(err)
	}
	job, err := worker.Store.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	backend, cfg, err := worker.resolveExecutionBackendForJob(job, decoded)
	if err != nil || backend != execbackend.Remote {
		t.Fatalf("resolve %s: backend=%q err=%v", id, backend, err)
	}
	return job, backend, cfg
}

// `--exec-provider sandboxd-linux` is accepted and stored as itself, and a
// job carrying it provisions against the Linux gateway's base URL, never the
// Mac gateway's. Each gateway admits up to its own reported capacity and
// ceiling, and the ledger keeps the two providers' rows apart: filling one
// gateway neither consumes nor frees the other's slots.
func TestSandboxdLinuxProviderRoutesToItsGatewayWithItsOwnCap(t *testing.T) {
	provider, backendFlag, err := requestExecProvider("sandboxd-linux", nil)
	if err != nil || provider != config.RemoteExecProviderSandboxdLinux || backendFlag == nil || *backendFlag != string(execbackend.Remote) {
		t.Fatalf("--exec-provider sandboxd-linux = %q, %v, %v; want sandboxd-linux on the remote backend", provider, backendFlag, err)
	}
	var stderr strings.Builder
	if _, ok := parseAgentRunOptions("review", []string{"reviewer", "review this", "--pr", "7", "--exec-provider", "sandboxd-linux"}, &stderr); !ok || stderr.Len() != 0 {
		t.Fatalf("agent review --exec-provider sandboxd-linux refused or warned: %q", stderr.String())
	}

	mac, linux := newFakeSandboxd(t), newFakeSandboxd(t)
	mac.reportCapacity(4, map[string]int{"review-arm64": 4})
	linux.reportCapacity(2, map[string]int{"review-amd64": 2})
	// The Mac ceiling (1) is below the Linux one (2); were the caps mixed, a
	// second Linux provision or the Mac provision after two Linux ones would
	// be refused.
	pair := writeSandboxdPairHome(t, mac.server.URL, linux.server.URL, 1, 2)
	for _, name := range []string{config.RemoteExecProviderSandboxd, config.RemoteExecProviderSandboxdLinux} {
		if err := validateRequestExecProvider(pair.home, name); err != nil {
			t.Fatalf("configured provider %q refused at request time: %v", name, err)
		}
	}
	store := openExecBackendLedgerTestStore(t)
	worker := jobWorker{Store: store, ConfigHome: pair.home, ConfigHomeExplicit: true, Stdout: io.Discard}

	provision := func(id, provider string) error {
		job, backend, cfg := sandboxdPairJob(t, worker, id, provider)
		lifecycle, err := worker.defaultExecutionBackend(backend, cfg)
		if err != nil {
			t.Fatalf("construct %s backend: %v", provider, err)
		}
		_, err = lifecycle.Provision(context.Background(), execbackend.JobScope{JobID: job.ID, LifecycleGeneration: job.LifecycleGeneration, TTL: time.Minute})
		return err
	}
	for _, id := range []string{"linux-1", "linux-2"} {
		if err := provision(id, config.RemoteExecProviderSandboxdLinux); err != nil {
			t.Fatalf("provision %s on sandboxd-linux: %v", id, err)
		}
	}
	if _, posts := linux.counts(); posts != 2 {
		t.Fatalf("Linux gateway saw %d creates; want 2", posts)
	}
	if _, posts := mac.counts(); posts != 0 {
		t.Fatalf("Mac gateway saw %d creates for sandboxd-linux jobs; want none", posts)
	}
	requireCapRefusal(t, provision("linux-3", config.RemoteExecProviderSandboxdLinux), "concurrency")
	if err := provision("mac-1", config.RemoteExecProviderSandboxd); err != nil {
		t.Fatalf("Mac provision after the Linux gateway filled: %v", err)
	}
	requireCapRefusal(t, provision("mac-2", config.RemoteExecProviderSandboxd), "concurrency")
	if _, posts := linux.counts(); posts != 2 {
		t.Fatalf("Linux gateway saw %d creates; want 2 (none for refused or Mac jobs)", posts)
	}
	if _, posts := mac.counts(); posts != 1 {
		t.Fatalf("Mac gateway saw %d creates; want 1", posts)
	}
	for provider, want := range map[string]int{config.RemoteExecProviderSandboxdLinux: 2, config.RemoteExecProviderSandboxd: 1} {
		rows, err := store.ListRecoverableExecBackendAttempts(context.Background(), provider)
		if err != nil || len(rows) != want {
			t.Fatalf("%s ledger rows = %d, %v; want %d", provider, len(rows), err, want)
		}
	}
}

// installSandboxdPairGateway points the model gateway at upstream and resets
// the registry for this test's home.
func installSandboxdPairGateway(t *testing.T, pair sandboxdPairHome, upstream string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(pair.paths.Home, runtimeAuthFileName), []byte(runtime.AnthropicAPIKeyEnv+"="+remoteBrokerTestKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalRegistry := credgw.DefaultRegistry
	originalUpstream := modelGatewayUpstreamURL
	originalLoopback := remoteModelGatewayAllowLoopbackHTTP
	credgw.DefaultRegistry = credgw.NewRegistry()
	modelGatewayUpstreamURL = upstream
	remoteModelGatewayAllowLoopbackHTTP = true
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = credgw.DefaultRegistry.CloseHome(ctx, pair.paths.Home)
		credgw.DefaultRegistry = originalRegistry
		modelGatewayUpstreamURL = originalUpstream
		remoteModelGatewayAllowLoopbackHTTP = originalLoopback
	})
}

// provisionSandboxdPairOmp provisions an omp job for provider through the
// daemon's provisioning path against a recording backend.
func provisionSandboxdPairOmp(t *testing.T, pair sandboxdPairHome, id, provider string) (*credentialTestBackend, *credgw.Lease) {
	t.Helper()
	inner := &credentialTestBackend{instance: &execbackend.Instance{ID: "sandbox-" + id, JobID: id, Workspace: "/home/user/workspace"}}
	lifecycle := &credentialRevokingExecutionBackend{inner: inner, home: pair.paths.Home}
	var seen config.RemoteExecConfig
	worker := jobWorker{
		Store: openExecBackendLedgerTestStore(t), ConfigHome: pair.home, ConfigHomeExplicit: true,
		ExecutionBackendFactory: func(_ execbackend.Backend, cfg config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
			seen = cfg
			return lifecycle, nil
		},
	}
	job, backend, cfg := sandboxdPairJob(t, worker, id, provider)
	_, instance, lease, _, err := worker.provisionExecutionBackend(context.Background(), backend, cfg, runtime.OmpRuntime, job, time.Minute, t.TempDir())
	if err != nil {
		t.Fatalf("provision %s omp job: %v", provider, err)
	}
	if instance != inner.instance || lease == nil || seen.Provider != provider {
		t.Fatalf("%s omp provision instance=%+v lease=%v provider=%q", provider, instance, lease, seen.Provider)
	}
	return inner, lease
}

// A sandboxd-linux omp review uploads the configured linux/amd64 omp into its
// amd64 guest, while the Mac provider on the same home keeps uploading its
// linux/arm64 omp.
func TestSandboxdLinuxOmpReviewUploadsAMD64Binary(t *testing.T) {
	pair := writeSandboxdPairHome(t, "https://sandboxd.invalid:8443", "http://127.0.0.1:43190", 1, 2)
	installSandboxdPairGateway(t, pair, "http://127.0.0.1:1")
	for _, test := range []struct {
		provider string
		want     []byte
		wrong    []byte
	}{
		{config.RemoteExecProviderSandboxdLinux, pair.linuxOmp, pair.macOmp},
		{config.RemoteExecProviderSandboxd, pair.macOmp, pair.linuxOmp},
	} {
		inner, _ := provisionSandboxdPairOmp(t, pair, "omp-"+test.provider, test.provider)
		uploaded := inner.runtimeContents[execbackend.RuntimeOmpExecutablePath]
		if !bytes.Equal(uploaded, test.want) || inner.runtimeFiles[execbackend.RuntimeOmpExecutablePath] != 0o700 {
			t.Fatalf("%s uploaded omp %q (mode %v); want the %q file", test.provider, uploaded, inner.runtimeFiles[execbackend.RuntimeOmpExecutablePath], test.want[64:])
		}
	}

	// An ARM64 binary under omp_linux_amd64_file is refused when the provider
	// is requested, before anything is enqueued.
	config := strings.Replace(string(mustReadFile(t, pair.paths.ConfigFile)), "omp-linux-amd64\"", "omp-linux-arm64\"", 1)
	if err := os.WriteFile(pair.paths.ConfigFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateRequestExecProvider(pair.home, "sandboxd-linux"); err == nil || !strings.Contains(err.Error(), "omp_linux_amd64_file") || !strings.Contains(err.Error(), "AMD64") {
		t.Fatalf("sandboxd-linux request with an ARM64 omp = %v; want a refusal naming omp_linux_amd64_file", err)
	}
	if check, _ := remoteExecDoctorCheck(pair.paths); check.OK || !strings.Contains(check.Detail, "omp_linux_amd64_file") {
		t.Fatalf("doctor with an ARM64 omp for amd64 guests = %+v; want a failing check", check)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// A sandboxd-linux guest reaches the host credential gateway at the slirp
// host alias 10.0.2.2, which forwards to the host loopback. Its lease names
// that origin, the one gateway listener's certificate carries the IP SAN, and
// a guest dialling https://10.0.2.2:<port> with its lease material completes
// mTLS and reaches the model upstream. An address the config never advertised
// is not covered.
func TestSandboxdLinuxGatewayCertificateCoversGuestAddress(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "brokered-success")
	}))
	defer upstream.Close()
	pair := writeSandboxdPairHome(t, "https://sandboxd.invalid:8443", "http://127.0.0.1:43190", 1, 2)
	installSandboxdPairGateway(t, pair, upstream.URL)

	_, lease := provisionSandboxdPairOmp(t, pair, "gateway-linux", config.RemoteExecProviderSandboxdLinux)
	material := lease.RemoteMaterial()
	if !strings.HasPrefix(material.URL, pair.linuxGateway+"/") {
		t.Fatalf("sandboxd-linux lease URL = %q; want the guest-visible gateway %q", material.URL, pair.linuxGateway)
	}

	// Verify the served certificate the way a guest's TLS stack does: the
	// guest dials an IP, so it sends no SNI and checks the IP SAN.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(material.CACertificate) {
		t.Fatal("append gateway CA")
	}
	clientCertificate, err := tls.X509KeyPair(material.ClientCertificate, material.ClientPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	handshake := func(serverName string) error {
		conn, err := tls.Dial("tcp", pair.listenAddress, &tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: serverName, Certificates: []tls.Certificate{clientCertificate},
		})
		if err != nil {
			return err
		}
		return conn.Close()
	}
	if err := handshake(sandboxdLinuxGuestGatewayHost); err != nil {
		t.Fatalf("gateway certificate does not cover %s: %v", sandboxdLinuxGuestGatewayHost, err)
	}
	var hostnameErr x509.HostnameError
	if err := handshake("10.0.2.3"); !errors.As(err, &hostnameErr) {
		t.Fatalf("handshake for an unadvertised address = %v; want a hostname mismatch", err)
	}

	// The full guest call: slirp maps 10.0.2.2 to the host's loopback.
	client := credentialMaterialHTTPClient(t, material)
	transport := client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host != sandboxdLinuxGuestGatewayHost {
			return nil, fmt.Errorf("guest dialled %q; only the gateway alias is reachable", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	request, err := http.NewRequest(http.MethodPost, material.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+material.Placeholder)
	request.Header.Set(credgw.CapabilityHeader, material.Capability)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("guest call to %s: %v", material.URL, err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || string(body) != "brokered-success" || upstreamCalls.Load() != 1 {
		t.Fatalf("guest call status=%d body=%q upstream=%d", response.StatusCode, body, upstreamCalls.Load())
	}
}
