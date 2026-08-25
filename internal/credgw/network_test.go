package credgw

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type controlledClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *controlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controlledClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

type networkProxyFixture struct {
	t             *testing.T
	clock         *controlledClock
	gateway       *Gateway
	network       *NetworkProxy
	serverCA      *x509.Certificate
	clientCA      *x509.Certificate
	clientCAKey   crypto.Signer
	upstream      *httptest.Server
	upstreamCalls atomic.Int32
	resolverCalls atomic.Int32

	upstreamMu            sync.Mutex
	upstreamAuthorization string
	upstreamCapability    string
}

func newNetworkProxyFixture(t *testing.T) *networkProxyFixture {
	return newNetworkProxyFixtureForAddress(t, "127.0.0.1:0", "127.0.0.1")
}

func newNetworkProxyFixtureForAddress(t *testing.T, address, advertisedAuthority string) *networkProxyFixture {
	t.Helper()
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	clock := &controlledClock{now: now}
	serverCA, serverCAKey := newTestCertificateAuthority(t, now, "server-ca")
	clientCA, clientCAKey := newTestCertificateAuthority(t, now, "client-ca")
	certificateHost := advertisedAuthority
	if host, _, err := net.SplitHostPort(advertisedAuthority); err == nil {
		certificateHost = host
	}
	serverCertificate := newTestServerCertificate(t, now, serverCA, serverCAKey, certificateHost)
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	gateway.now = clock.Now
	network, err := gateway.StartNetworkProxy(NetworkProxyConfig{
		Address: address, AdvertisedAuthority: advertisedAuthority, ServerCertificate: serverCertificate,
		ClientCA: clientCA, ClientCAKey: clientCAKey,
	})
	if err != nil {
		gateway.Close(context.Background())
		t.Fatal(err)
	}
	fixture := &networkProxyFixture{
		t: t, clock: clock, gateway: gateway, network: network,
		serverCA: serverCA, clientCA: clientCA, clientCAKey: clientCAKey,
	}
	fixture.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.upstreamCalls.Add(1)
		fixture.upstreamMu.Lock()
		fixture.upstreamAuthorization = r.Header.Get("Authorization")
		fixture.upstreamCapability = r.Header.Get(CapabilityHeader)
		fixture.upstreamMu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "proxied")
	}))
	t.Cleanup(func() {
		fixture.upstream.Close()
		_ = network.Close(context.Background())
		_ = gateway.Close(context.Background())
	})
	return fixture
}

func (f *networkProxyFixture) register(jobID string, deadline time.Time) *NetworkLease {
	f.t.Helper()
	policy := ProxyPolicy{Upstream: f.upstream.URL, AuthKind: ProxyAuthBearer, AllowLoopbackHTTP: true}
	lease, err := f.network.RegisterProxy(jobID, deadline, policy, func(context.Context) (ResolvedCredential, error) {
		f.resolverCalls.Add(1)
		return ResolvedCredential{Value: testRealCredential, Upstream: policy.Upstream, AuthKind: policy.AuthKind}, nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return lease
}

func (f *networkProxyFixture) client(certificate tls.Certificate) *http.Client {
	return f.clientForAddress(certificate, "", "")
}

func (f *networkProxyFixture) clientForAddress(certificate tls.Certificate, serverName, dialAddress string) *http.Client {
	f.t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(f.serverCA)
	config := &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS13, Time: f.clock.Now}
	if len(certificate.Certificate) != 0 {
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &certificate, nil
		}
	}
	transport := &http.Transport{
		TLSClientConfig: config, DisableKeepAlives: true,
	}
	if dialAddress != "" {
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, dialAddress)
		}
	}
	return &http.Client{Transport: transport}
}

func (f *networkProxyFixture) request(client *http.Client, lease *NetworkLease, capability string) (*http.Response, error) {
	f.t.Helper()
	request, err := http.NewRequest(http.MethodPost, lease.URL()+"/v1/messages", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+lease.Placeholder())
	if capability != "" {
		request.Header.Set(CapabilityHeader, capability)
	}
	return client.Do(request)
}

func TestNetworkProxyRefusesToStartWithoutTLSMaterial(t *testing.T) {
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	serverCA, serverCAKey := newTestCertificateAuthority(t, now, "server-ca")
	serverCertificate := newTestServerCertificate(t, now, serverCA, serverCAKey, "127.0.0.1")
	clientCA, clientCAKey := newTestCertificateAuthority(t, now, "client-ca")
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	_, err = gateway.StartNetworkProxy(NetworkProxyConfig{
		Address: "127.0.0.1:0", AdvertisedAuthority: "127.0.0.1",
		ClientCA: clientCA, ClientCAKey: clientCAKey,
	})
	if err == nil || !strings.Contains(err.Error(), "server TLS certificate is required") {
		t.Fatalf("StartNetworkProxy error = %v", err)
	}
	_, err = gateway.StartNetworkProxy(NetworkProxyConfig{
		Address: "127.0.0.1:0", ServerCertificate: serverCertificate,
		ClientCA: clientCA, ClientCAKey: clientCAKey,
	})
	if err == nil || !strings.Contains(err.Error(), "advertised authority is required") {
		t.Fatalf("StartNetworkProxy without advertised authority error = %v", err)
	}
}

func TestNetworkProxyWildcardBindUsesAdvertisedAuthority(t *testing.T) {
	fixture := newNetworkProxyFixtureForAddress(t, "0.0.0.0:0", "gateway.internal")
	_, port, err := net.SplitHostPort(fixture.network.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dialAddress := net.JoinHostPort("127.0.0.1", port)
	lease := fixture.register("wildcard-job", fixture.clock.Now().Add(30*time.Minute))
	client := fixture.clientForAddress(lease.ClientCertificate(), "gateway.internal", dialAddress)

	response, err := fixture.request(client, lease, lease.Capability())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("wildcard-bind proxy status = %d, want 200", response.StatusCode)
	}
	if got := fixture.resolverCalls.Load(); got != 1 {
		t.Fatalf("wildcard-bind resolver calls = %d, want 1", got)
	}
	if err := lease.Renew(context.Background(), client); err != nil {
		t.Fatalf("wildcard-bind renewal: %v", err)
	}

	request, err := http.NewRequest(http.MethodPost, lease.URL()+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = net.JoinHostPort("wrong.internal", port)
	request.Header.Set("Authorization", "Bearer "+lease.Placeholder())
	request.Header.Set(CapabilityHeader, lease.Capability())
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched advertised authority status = %d, want 400", response.StatusCode)
	}
	if got := fixture.resolverCalls.Load(); got != 1 {
		t.Fatalf("resolver calls after mismatched authority = %d, want 1", got)
	}
}

func TestNetworkProxyHTTPSDefaultPortAuthorityForms(t *testing.T) {
	fixture := newNetworkProxyFixtureForAddress(t, "127.0.0.1:0", "gateway.internal:443")
	_, port, err := net.SplitHostPort(fixture.network.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	lease := fixture.register("default-port-job", fixture.clock.Now().Add(30*time.Minute))
	client := fixture.clientForAddress(lease.ClientCertificate(), "gateway.internal", net.JoinHostPort("127.0.0.1", port))

	request := func(host string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, lease.URL()+"/v1/messages", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+lease.Placeholder())
		req.Header.Set(CapabilityHeader, lease.Capability())
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response
	}

	if response := request("gateway.internal"); response.StatusCode != http.StatusOK {
		t.Fatalf("portless default authority status = %d, want 200", response.StatusCode)
	}
	if err := lease.Renew(context.Background(), client); err != nil {
		t.Fatalf("portless default authority renewal: %v", err)
	}
	if response := request("gateway.internal:443"); response.StatusCode != http.StatusOK {
		t.Fatalf("explicit default authority status = %d, want 200", response.StatusCode)
	}
	renewRequest, err := http.NewRequest(http.MethodPost, fixture.network.URL()+networkRenewPrefix+strings.TrimPrefix(lease.lease.route, "/_gitmoot/proxy/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	renewRequest.Host = "gateway.internal:443"
	renewRequest.Header.Set(CapabilityHeader, lease.Capability())
	renewResponse, err := client.Do(renewRequest)
	if err != nil {
		t.Fatal(err)
	}
	renewResponse.Body.Close()
	if renewResponse.StatusCode != http.StatusOK {
		t.Fatalf("explicit default authority renewal status = %d, want 200", renewResponse.StatusCode)
	}
	if got := fixture.resolverCalls.Load(); got != 2 {
		t.Fatalf("default authority resolver calls = %d, want 2", got)
	}
	if got := fixture.upstreamCalls.Load(); got != 2 {
		t.Fatalf("default authority upstream calls = %d, want 2", got)
	}
	fixture.upstreamMu.Lock()
	defer fixture.upstreamMu.Unlock()
	if fixture.upstreamAuthorization != "Bearer "+testRealCredential || fixture.upstreamCapability != "" {
		t.Fatalf("upstream authorization=%q capability=%q", fixture.upstreamAuthorization, fixture.upstreamCapability)
	}
}

func TestNetworkProxyHTTPSNonDefaultPortRequiresExplicitPort(t *testing.T) {
	fixture := newNetworkProxyFixtureForAddress(t, "127.0.0.1:0", "gateway.internal:8443")
	_, port, err := net.SplitHostPort(fixture.network.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	lease := fixture.register("non-default-port-job", fixture.clock.Now().Add(30*time.Minute))
	client := fixture.clientForAddress(lease.ClientCertificate(), "gateway.internal", net.JoinHostPort("127.0.0.1", port))

	request := func(host string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, lease.URL()+"/v1/messages", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+lease.Placeholder())
		req.Header.Set(CapabilityHeader, lease.Capability())
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response
	}

	response := request("gateway.internal")
	if got := fixture.resolverCalls.Load(); got != 0 {
		t.Fatalf("resolver calls for omitted non-default port = %d, want 0", got)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("omitted non-default port status = %d, want 400", response.StatusCode)
	}
	if response = request("gateway.internal:8443"); response.StatusCode != http.StatusOK {
		t.Fatalf("explicit non-default authority status = %d, want 200", response.StatusCode)
	}
}

func TestNetworkProxyHTTPSAuthorityRejectsMismatches(t *testing.T) {
	tests := []struct {
		name         string
		host         string
		absoluteForm bool
	}{
		{name: "different host", host: "other.internal"},
		{name: "different non-default port", host: "gateway.internal:8443"},
		{name: "userinfo", host: "user@gateway.internal"},
		{name: "path", host: "gateway.internal/path"},
		{name: "query", host: "gateway.internal?query"},
		{name: "fragment", host: "gateway.internal#fragment"},
		{name: "absolute-form request URI", host: "gateway.internal", absoluteForm: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNetworkProxyFixtureForAddress(t, "127.0.0.1:0", "gateway.internal:443")
			lease := fixture.register("mismatch-job", fixture.clock.Now().Add(30*time.Minute))
			request := httptest.NewRequest(http.MethodPost, lease.lease.route+"/v1/messages", nil)
			request.Host = test.host
			request.Header.Set("Authorization", "Bearer "+lease.Placeholder())
			request.Header.Set(CapabilityHeader, lease.Capability())
			request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{lease.ClientCertificate().Leaf}}
			if test.absoluteForm {
				request.URL.Scheme = "https"
				request.URL.Host = "gateway.internal"
				request.RequestURI = request.URL.String()
			}
			recorder := httptest.NewRecorder()
			fixture.network.ServeHTTP(recorder, request)
			if got := fixture.resolverCalls.Load(); got != 0 {
				t.Fatalf("resolver calls = %d, want 0", got)
			}
			if got := fixture.upstreamCalls.Load(); got != 0 {
				t.Fatalf("upstream calls = %d, want 0", got)
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
		})
	}
}

func TestNetworkProxyTLSHandshakeRejectsMissingWrongAndExpiredClientCertificates(t *testing.T) {
	fixture := newNetworkProxyFixture(t)
	lease := fixture.register("tls-job", fixture.clock.Now().Add(10*time.Minute))
	wrongCA, wrongSigner := newTestCertificateAuthority(t, fixture.clock.Now(), "wrong-client-ca")
	wrongCertificate, _, err := issueNetworkClientCertificate(wrongCA, wrongSigner, "wrong-ca-job", fixture.clock.Now(), fixture.clock.Now().Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		certificate tls.Certificate
	}{
		{name: "missing client certificate"},
		{name: "wrong CA client certificate", certificate: wrongCertificate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := fixture.request(fixture.client(test.certificate), lease, lease.Capability())
			if err == nil {
				response.Body.Close()
				t.Fatalf("request reached HTTP with status %d; want TLS handshake failure", response.StatusCode)
			}
			if got := fixture.resolverCalls.Load(); got != 0 {
				t.Fatalf("credential resolver calls = %d, want 0", got)
			}
		})
	}

	fixture.clock.Advance(11 * time.Minute)
	response, err := fixture.request(fixture.client(lease.ClientCertificate()), lease, lease.Capability())
	if err == nil {
		response.Body.Close()
		t.Fatalf("expired certificate reached HTTP with status %d; want TLS handshake failure", response.StatusCode)
	}
	if got := fixture.resolverCalls.Load(); got != 0 {
		t.Fatalf("credential resolver calls after expired certificate = %d, want 0", got)
	}
}

func TestNetworkProxyRejectedAuthorizationNeverResolvesCredential(t *testing.T) {
	tests := []struct {
		name  string
		probe func(*networkProxyFixture, *NetworkLease) (*http.Response, error)
	}{
		{
			name: "missing capability",
			probe: func(f *networkProxyFixture, lease *NetworkLease) (*http.Response, error) {
				return f.request(f.client(lease.ClientCertificate()), lease, "")
			},
		},
		{
			name: "malformed capability",
			probe: func(f *networkProxyFixture, lease *NetworkLease) (*http.Response, error) {
				return f.request(f.client(lease.ClientCertificate()), lease, "not-a-capability")
			},
		},
		{
			name: "certificate bound to another job",
			probe: func(f *networkProxyFixture, lease *NetworkLease) (*http.Response, error) {
				other := f.register("other-cert-job", f.clock.Now().Add(30*time.Minute))
				if bytes.Equal(lease.ClientCertificate().Certificate[0], other.ClientCertificate().Certificate[0]) {
					f.t.Fatal("different jobs received the same client certificate")
				}
				return f.request(f.client(other.ClientCertificate()), lease, lease.Capability())
			},
		},
		{
			name: "expired capability",
			probe: func(f *networkProxyFixture, lease *NetworkLease) (*http.Response, error) {
				f.clock.Advance(proxyCapabilityTTL + time.Second)
				return f.request(f.client(lease.ClientCertificate()), lease, lease.Capability())
			},
		},
		{
			name: "capability from another route",
			probe: func(f *networkProxyFixture, lease *NetworkLease) (*http.Response, error) {
				other := f.register("other-capability-job", f.clock.Now().Add(30*time.Minute))
				return f.request(f.client(lease.ClientCertificate()), lease, other.Capability())
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNetworkProxyFixture(t)
			lease := fixture.register("target-job", fixture.clock.Now().Add(30*time.Minute))
			response, err := test.probe(fixture, lease)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
			}
			if got := fixture.resolverCalls.Load(); got != 0 {
				t.Fatalf("credential resolver calls = %d, want 0", got)
			}
			if got := fixture.upstreamCalls.Load(); got != 0 {
				t.Fatalf("upstream calls = %d, want 0", got)
			}
		})
	}
}

func TestNetworkProxyValidRequestSubstitutesCredential(t *testing.T) {
	fixture := newNetworkProxyFixture(t)
	lease := fixture.register("valid-job", fixture.clock.Now().Add(30*time.Minute))
	response, err := fixture.request(fixture.client(lease.ClientCertificate()), lease, lease.Capability())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), testRealCredential) || strings.Contains(string(body), lease.Capability()) {
		t.Fatalf("client response exposed credential material: %q", body)
	}
	if got := fixture.resolverCalls.Load(); got != 1 {
		t.Fatalf("credential resolver calls = %d, want 1", got)
	}
	if got := fixture.upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	fixture.upstreamMu.Lock()
	defer fixture.upstreamMu.Unlock()
	if fixture.upstreamAuthorization != "Bearer "+testRealCredential || fixture.upstreamCapability != "" {
		t.Fatalf("upstream authorization=%q capability=%q", fixture.upstreamAuthorization, fixture.upstreamCapability)
	}
}

func TestNetworkProxyCapabilityRenewalOverlapAndAbsoluteDeadline(t *testing.T) {
	fixture := newNetworkProxyFixture(t)
	deadline := fixture.clock.Now().Add(15 * time.Minute)
	lease := fixture.register("renew-job", deadline)
	stableURL := lease.URL()
	if lease.ClientCertificate().Leaf == nil || lease.ClientCertificate().Leaf.NotAfter.After(deadline) {
		t.Fatalf("client certificate deadline = %v, want no later than %v", lease.ClientCertificate().Leaf, deadline)
	}
	fixture.gateway.mu.RLock()
	initialEntry := fixture.gateway.proxyEntries[lease.lease.route]
	fixture.gateway.mu.RUnlock()
	if initialEntry.capability.expiresAt.After(deadline) {
		t.Fatalf("initial capability deadline = %v, want no later than %v", initialEntry.capability.expiresAt, deadline)
	}
	client := fixture.client(lease.ClientCertificate())
	oldCapability := lease.Capability()
	fixture.clock.Advance(4 * time.Minute)
	if err := lease.Renew(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	newCapability := lease.Capability()
	if newCapability == oldCapability {
		t.Fatal("renewal did not rotate the capability")
	}
	if lease.URL() != stableURL {
		t.Fatalf("renewal changed route URL from %q to %q", stableURL, lease.URL())
	}

	response, err := fixture.request(fixture.client(lease.ClientCertificate()), lease, oldCapability)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("previous capability during overlap status = %d, want 200", response.StatusCode)
	}
	fixture.clock.Advance(proxyCapabilityOverlap + time.Second)
	response, err = fixture.request(fixture.client(lease.ClientCertificate()), lease, oldCapability)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("previous capability after overlap status = %d, want 401", response.StatusCode)
	}
	response, err = fixture.request(fixture.client(lease.ClientCertificate()), lease, newCapability)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("renewed capability status = %d, want 200", response.StatusCode)
	}

	fixture.clock.Advance(deadline.Sub(fixture.clock.Now()) + time.Second)
	fixture.gateway.mu.Lock()
	registered := fixture.gateway.proxyEntries[lease.lease.route]
	registered.capability.expiresAt = fixture.clock.Now().Add(time.Hour)
	fixture.gateway.proxyEntries[lease.lease.route] = registered
	fixture.gateway.mu.Unlock()
	certificateHash := sha256.Sum256(lease.ClientCertificate().Certificate[0])
	if _, err := fixture.gateway.renewProxyCapability(lease.lease.route, newCapability, certificateHash); err == nil {
		t.Fatal("post-deadline renewal succeeded")
	}
	response, err = fixture.request(fixture.client(lease.ClientCertificate()), lease, newCapability)
	if err == nil {
		response.Body.Close()
		t.Fatalf("post-deadline certificate reached HTTP with status %d", response.StatusCode)
	}
}

func TestNetworkProxyRevocation(t *testing.T) {
	fixture := newNetworkProxyFixture(t)
	lease := fixture.register("revoked-job", fixture.clock.Now().Add(30*time.Minute))
	client := fixture.client(lease.ClientCertificate())
	response, err := fixture.request(client, lease, lease.Capability())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initial status = %d, want 200", response.StatusCode)
	}
	resolvedBeforeRevoke := fixture.resolverCalls.Load()
	lease.Revoke()
	response, err = fixture.request(fixture.client(lease.ClientCertificate()), lease, lease.Capability())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked status = %d, want 401", response.StatusCode)
	}
	if got := fixture.resolverCalls.Load(); got != resolvedBeforeRevoke {
		t.Fatalf("resolver calls after revocation = %d, want %d", got, resolvedBeforeRevoke)
	}

	if got := fixture.upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls after revocation = %d, want 1", got)
	}
}

func TestNetworkProxyLegacyEntryCannotReachProxyRoute(t *testing.T) {
	fixture := newNetworkProxyFixture(t)
	lease := fixture.register("network-job", fixture.clock.Now().Add(30*time.Minute))
	legacyPlaceholder, err := fixture.gateway.Register("legacy-job", Credential{Kind: CredentialBearer, Value: testRealCredential}, testPolicy(t, fixture.upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, lease.URL()+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+legacyPlaceholder)
	request.Header.Set(CapabilityHeader, lease.Capability())
	response, err := fixture.client(lease.ClientCertificate()).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := fixture.resolverCalls.Load(); got != 0 {
		t.Fatalf("network resolver calls for legacy entry = %d, want 0", got)
	}
	if got := fixture.upstreamCalls.Load(); got != 0 {
		t.Fatalf("legacy entry reached upstream: calls = %d, want 0", got)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("legacy entry on proxy route status = %d, want 401", response.StatusCode)
	}
}

func TestNetworkProxyTerminalRouteDoesNotDispatchLegacyEntry(t *testing.T) {
	fixture := newNetworkProxyFixture(t)
	lease := fixture.register("network-job", fixture.clock.Now().Add(30*time.Minute))
	legacyPlaceholder, err := fixture.gateway.Register("legacy-job", Credential{Kind: CredentialBearer, Value: testRealCredential}, testPolicy(t, fixture.upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, fixture.network.URL()+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+legacyPlaceholder)
	response, err := fixture.client(lease.ClientCertificate()).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := fixture.upstreamCalls.Load(); got != 0 {
		t.Fatalf("terminal network route reached legacy upstream: calls = %d, want 0", got)
	}
	if got := fixture.resolverCalls.Load(); got != 0 {
		t.Fatalf("network resolver calls for terminal route = %d, want 0", got)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("terminal network route status = %d, want 404", response.StatusCode)
	}
}

func newTestCertificateAuthority(t *testing.T, now time.Time, name string) (*x509.Certificate, crypto.Signer) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func newTestServerCertificate(t *testing.T, now time.Time, ca *x509.Certificate, caKey crypto.Signer, advertisedHost string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "credential-gateway.test"},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	if net.ParseIP(advertisedHost) == nil {
		template.DNSNames = []string{advertisedHost}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key, Leaf: leaf}
}
