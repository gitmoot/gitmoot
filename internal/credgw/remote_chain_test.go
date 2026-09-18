package credgw

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const remoteChainUpstreamBody = "upstream-reached"

// remoteChainGateway starts a gateway with an mTLS listener in front of a stub
// upstream and returns the material a sandbox would receive.
func remoteChainGateway(t *testing.T) (*Gateway, RemoteMaterial, func()) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, remoteChainUpstreamBody)
	}))

	var logs logSink
	gateway, err := Start(logs.Logf)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if err := gateway.EnableRemote(RemoteListenerOptions{ListenAddress: "127.0.0.1:0"}); err != nil {
		t.Fatalf("EnableRemote returned error: %v", err)
	}
	lease, err := gateway.RegisterProxy("chain-job", ProxyPolicy{
		Upstream: upstream.URL, AuthKind: ProxyAuthResolved, AllowLoopbackHTTP: true,
		SandboxID: "sandbox-chain", Runtime: "shell", ExpiresAt: time.Now().Add(time.Minute),
		AllowedHosts: []string{"127.0.0.1"},
	}, func(context.Context) (ResolvedCredential, error) {
		return ResolvedCredential{Value: "real-credential-value", Upstream: upstream.URL, AuthKind: ProxyAuthBearer}, nil
	})
	if err != nil {
		t.Fatalf("RegisterProxy returned error: %v", err)
	}
	return gateway, lease.RemoteMaterial(), func() {
		_ = gateway.Close(context.Background())
		upstream.Close()
	}
}

// remoteChainClient builds a client that presents either a bare leaf or the
// leaf followed by the CA — the shape curl actually sends in a sandbox when the
// config supplies both `cert` and `cacert`.
func remoteChainClient(t *testing.T, material RemoteMaterial, withChain bool) *http.Client {
	t.Helper()
	certificate, err := tls.X509KeyPair(material.ClientCertificate, material.ClientPrivateKey)
	if err != nil {
		t.Fatalf("X509KeyPair returned error: %v", err)
	}
	if withChain {
		caCertificate, err := tls.X509KeyPair(material.CACertificate, material.ClientPrivateKey)
		if err == nil && len(caCertificate.Certificate) > 0 {
			certificate.Certificate = append(certificate.Certificate, caCertificate.Certificate[0])
		} else {
			// The CA PEM has no matching key, so parse the DER directly.
			block := material.CACertificate
			parsed, parseErr := parseFirstCertificateDER(block)
			if parseErr != nil {
				t.Fatalf("parse CA certificate: %v", parseErr)
			}
			certificate.Certificate = append(certificate.Certificate, parsed)
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(material.CACertificate) {
		t.Fatal("AppendCertsFromPEM(material CA) returned false")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate},
	}}}
}

func remoteChainCall(t *testing.T, material RemoteMaterial, client *http.Client) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, material.URL+"/v1/messages", strings.NewReader("request"))
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	request.Header.Set(CapabilityHeader, material.Capability)
	request.Header.Set("Authorization", "Bearer "+material.Placeholder)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("client.Do returned error: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	return response.StatusCode, string(body)
}

// TestRemoteGatewayAcceptsAClientChain is the regression for the defect that
// stopped every real sandbox authenticating.
//
// The handler required EXACTLY ONE peer certificate, which no client could
// satisfy through the material this gateway installs: curl given both `cert`
// and `cacert` presents the leaf AND the CA. The existing suite never caught it
// because remoteMaterialClient only ever builds a single-leaf tls.Certificate.
//
// MUTATION: restore `len(r.TLS.PeerCertificates) != 1` and this goes red with a
// 401.
func TestRemoteGatewayAcceptsAClientChain(t *testing.T) {
	_, material, cleanup := remoteChainGateway(t)
	defer cleanup()

	status, body := remoteChainCall(t, material, remoteChainClient(t, material, true))
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200: a client presenting leaf+CA must authenticate", status, strings.TrimSpace(body))
	}
	if body != remoteChainUpstreamBody {
		t.Fatalf("body = %q, want %q", body, remoteChainUpstreamBody)
	}
}

// TestRemoteGatewaySingleLeafStillWorks pins that admitting a chain did not
// break the bare-leaf client the suite already relied on.
func TestRemoteGatewaySingleLeafStillWorks(t *testing.T) {
	_, material, cleanup := remoteChainGateway(t)
	defer cleanup()

	if status, body := remoteChainCall(t, material, remoteChainClient(t, material, false)); status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", status, strings.TrimSpace(body))
	}
}

// TestRemoteRequestRefusalReasonNamesEachPrecondition pins that every refusal
// says WHICH check refused. The branch previously returned 401 and logged
// nothing, which is exactly why the chain defect survived until instrumented.
func TestRemoteRequestRefusalReasonNamesEachPrecondition(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		routed  bool
		request *http.Request
		want    string
	}{
		{"unrouted", false, &http.Request{}, "not a proxy route"},
		{"plaintext", true, &http.Request{}, "not TLS"},
		{"no certificate", true, &http.Request{TLS: &tls.ConnectionState{}}, "no certificate"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if reason := remoteRequestRefusalReason(testCase.routed, testCase.request); !strings.Contains(reason, testCase.want) {
				t.Fatalf("reason = %q, want it to mention %q", reason, testCase.want)
			}
		})
	}

	withChain := &http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}, {}}}}
	if reason := remoteRequestRefusalReason(true, withChain); reason != "" {
		t.Fatalf("reason = %q, want none: a presented chain is legitimate", reason)
	}
}

// TestUpstreamFailureLogRedactsTheCredential pins the hardening round 1 asked
// for. "Cannot leak today" was a property of the call sites, not of the
// function; this makes it a property of the function.
func TestUpstreamFailureLogRedactsTheCredential(t *testing.T) {
	const secret = "sk-ant-a-very-secret-value-0123456789"
	var logged []string
	gateway := &Gateway{logf: func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}}

	gateway.logUpstreamFailure(http.MethodPost, "api.example.com", "job-1",
		errors.New(`Post "https://api.example.com/v1?token=`+secret+`": connection reset`), secret)

	if len(logged) != 1 {
		t.Fatalf("log lines = %d, want 1", len(logged))
	}
	if strings.Contains(logged[0], secret) {
		t.Fatal("log line leaked the credential")
	}
	if !strings.Contains(logged[0], "connection reset") {
		t.Fatalf("log line = %q, want the transport cause preserved", logged[0])
	}
}

func parseFirstCertificateDER(pemBytes []byte) ([]byte, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE block in PEM")
	}
	return block.Bytes, nil
}
