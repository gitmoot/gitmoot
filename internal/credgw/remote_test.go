package credgw

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteProxyAuthenticatesBeforeCredentialLoadAndRevokes(t *testing.T) {
	const visibleSentinel = "visible-positive-control"
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if got := r.Header.Get("X-Api-Key"); got != testRealCredential {
			t.Fatalf("upstream api key length = %d, want %d", len(got), len(testRealCredential))
		}
		w.Header().Set("X-Reflected-Credential", "prefix-"+testRealCredential+"-suffix")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, visibleSentinel+":"+testRealCredential[:19])
		_, _ = io.WriteString(w, testRealCredential[19:])
	}))
	defer upstream.Close()

	var logs logSink
	gateway, err := Start(logs.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	if err := gateway.EnableRemote(RemoteListenerOptions{ListenAddress: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}

	policy := ProxyPolicy{
		Upstream: upstream.URL, AuthKind: ProxyAuthResolved, AllowLoopbackHTTP: true,
		SandboxID: "sandbox-one", Runtime: "shell", ExpiresAt: time.Now().Add(time.Minute),
		AllowedHosts: []string{"127.0.0.1"},
	}
	var resolverCalls atomic.Int32
	resolver := func(context.Context) (ResolvedCredential, error) {
		resolverCalls.Add(1)
		return ResolvedCredential{Value: testRealCredential, Upstream: upstream.URL, AuthKind: ProxyAuthHeader, Header: "X-Api-Key"}, nil
	}
	lease, err := gateway.RegisterProxy("remote-job", policy, resolver)
	if err != nil {
		t.Fatal(err)
	}
	material := lease.RemoteMaterial()
	client := remoteMaterialClient(t, material)

	call := func(material RemoteMaterial, client *http.Client, capability, placeholder string) (*http.Response, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, material.URL+"/v1/messages", strings.NewReader("request"))
		if err != nil {
			t.Fatal(err)
		}
		if capability != "" {
			request.Header.Set(CapabilityHeader, capability)
		}
		request.Header.Set("Authorization", "Bearer "+placeholder)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return response, string(body)
	}

	response, _ := call(material, client, "", material.Placeholder)
	if response.StatusCode != http.StatusUnauthorized || resolverCalls.Load() != 0 || upstreamCalls.Load() != 0 {
		t.Fatalf("missing capability status=%d resolver=%d upstream=%d", response.StatusCode, resolverCalls.Load(), upstreamCalls.Load())
	}

	otherPolicy := policy
	otherPolicy.SandboxID = "sandbox-two"
	otherLease, err := gateway.RegisterProxy("other-job", otherPolicy, resolver)
	if err != nil {
		t.Fatal(err)
	}
	otherMaterial := otherLease.RemoteMaterial()
	response, _ = call(material, client, otherMaterial.Capability, material.Placeholder)
	if response.StatusCode != http.StatusUnauthorized || resolverCalls.Load() != 0 || upstreamCalls.Load() != 0 {
		t.Fatalf("mismatched capability status=%d resolver=%d upstream=%d", response.StatusCode, resolverCalls.Load(), upstreamCalls.Load())
	}

	response, _ = call(material, remoteMaterialClient(t, otherMaterial), material.Capability, material.Placeholder)
	if response.StatusCode != http.StatusUnauthorized || resolverCalls.Load() != 0 || upstreamCalls.Load() != 0 {
		t.Fatalf("mismatched certificate status=%d resolver=%d upstream=%d", response.StatusCode, resolverCalls.Load(), upstreamCalls.Load())
	}

	response, body := call(material, client, material.Capability, material.Placeholder)
	if response.StatusCode != http.StatusTeapot || resolverCalls.Load() != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("success status=%d resolver=%d upstream=%d", response.StatusCode, resolverCalls.Load(), upstreamCalls.Load())
	}
	if !strings.Contains(body, visibleSentinel) || strings.Contains(body, testRealCredential) || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("redacted body lost control or exposed credential: %q", body)
	}
	if got := response.Header.Get("X-Reflected-Credential"); strings.Contains(got, testRealCredential) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("redacted response header = %q", got)
	}
	logged := logs.waitFor(t, "job_id=remote-job")
	if strings.Contains(logged, testRealCredential) || strings.Contains(logged, material.Capability) || strings.Contains(logged, material.Placeholder) {
		t.Fatalf("gateway log contains protected material: %q", logged)
	}

	lease.Revoke()
	response, _ = call(material, client, material.Capability, material.Placeholder)
	if response.StatusCode != http.StatusUnauthorized || resolverCalls.Load() != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("revoked status=%d resolver=%d upstream=%d", response.StatusCode, resolverCalls.Load(), upstreamCalls.Load())
	}
}

func TestRemoteProxyExpiryAndAllowlistFailClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	if err := gateway.EnableRemote(RemoteListenerOptions{ListenAddress: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	policy := ProxyPolicy{
		Upstream: upstream.URL, AuthKind: ProxyAuthResolved, AllowLoopbackHTTP: true,
		SandboxID: "sandbox-expiry", Runtime: "shell", ExpiresAt: time.Now().Add(time.Minute),
		AllowedHosts: []string{"example.invalid"},
	}
	if _, err := gateway.RegisterProxy("deny-upstream", policy, func(context.Context) (ResolvedCredential, error) {
		return ResolvedCredential{}, nil
	}); err == nil || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("deny-by-default error = %v", err)
	}

	policy.AllowedHosts = []string{"127.0.0.1"}
	var resolverCalls atomic.Int32
	lease, err := gateway.RegisterProxy("expires", policy, func(context.Context) (ResolvedCredential, error) {
		resolverCalls.Add(1)
		return ResolvedCredential{Value: testRealCredential, Upstream: upstream.URL, AuthKind: ProxyAuthBearer}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	material := lease.RemoteMaterial()
	gateway.mu.Lock()
	entry := gateway.proxyEntries[lease.route]
	entry.capability.expiresAt = time.Now().Add(-time.Second)
	gateway.proxyEntries[lease.route] = entry
	gateway.mu.Unlock()
	request, _ := http.NewRequest(http.MethodGet, material.URL, nil)
	request.Header.Set(CapabilityHeader, material.Capability)
	request.Header.Set("Authorization", "Bearer "+material.Placeholder)
	response, err := remoteMaterialClient(t, material).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || resolverCalls.Load() != 0 {
		t.Fatalf("expired status=%d resolver=%d", response.StatusCode, resolverCalls.Load())
	}
}

func TestRemoteProxyRedactsTransformedCredentialRepresentations(t *testing.T) {
	const (
		credential      = "Sk-Ant+/=Credential?MixedCase"
		visibleSentinel = "visible-positive-control"
	)
	for _, test := range []struct {
		name      string
		transform func(string) string
	}{
		{name: "base64", transform: func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }},
		{name: "URL encoded", transform: url.QueryEscape},
		{name: "case varied", transform: alternateCredentialCase},
	} {
		t.Run(test.name, func(t *testing.T) {
			transformed := test.transform(credential)
			planted := visibleSentinel + ":" + transformed
			if !strings.Contains(planted, visibleSentinel) || !strings.Contains(planted, transformed) {
				t.Fatal("positive control cannot detect planted transformed credential")
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Reflected-Credential", planted)
				_, _ = io.WriteString(w, visibleSentinel+":"+transformed[:len(transformed)/2])
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, transformed[len(transformed)/2:])
			}))
			defer upstream.Close()

			gateway, err := Start(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close(context.Background())
			if err := gateway.EnableRemote(RemoteListenerOptions{ListenAddress: "127.0.0.1:0"}); err != nil {
				t.Fatal(err)
			}
			lease, err := gateway.RegisterProxy("transformed-response", ProxyPolicy{
				Upstream: upstream.URL, AuthKind: ProxyAuthResolved, AllowLoopbackHTTP: true,
				SandboxID: "sandbox-transform", Runtime: "shell", ExpiresAt: time.Now().Add(time.Minute),
				AllowedHosts: []string{"127.0.0.1"},
			}, func(context.Context) (ResolvedCredential, error) {
				return ResolvedCredential{Value: credential, Upstream: upstream.URL, AuthKind: ProxyAuthBearer}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			material := lease.RemoteMaterial()
			request, _ := http.NewRequest(http.MethodGet, material.URL, nil)
			request.Header.Set(CapabilityHeader, material.Capability)
			request.Header.Set("Authorization", "Bearer "+material.Placeholder)
			response, err := remoteMaterialClient(t, material).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			header := response.Header.Get("X-Reflected-Credential")
			bodyText := string(body)
			if !strings.Contains(bodyText, visibleSentinel) || !strings.Contains(header, visibleSentinel) {
				t.Fatalf("positive control missing from body/header lengths %d/%d", len(bodyText), len(header))
			}
			if strings.Contains(bodyText, transformed) || strings.Contains(header, transformed) || !strings.Contains(bodyText, "[REDACTED]") || !strings.Contains(header, "[REDACTED]") {
				t.Fatalf("transformed credential redaction failed for body/header lengths %d/%d", len(bodyText), len(header))
			}
		})
	}
}

func alternateCredentialCase(value string) string {
	result := []byte(value)
	upper := false
	for index, character := range result {
		switch {
		case character >= 'a' && character <= 'z':
			if upper {
				result[index] = character - ('a' - 'A')
			}
			upper = !upper
		case character >= 'A' && character <= 'Z':
			if !upper {
				result[index] = character + ('a' - 'A')
			}
			upper = !upper
		}
	}
	return string(result)
}

func remoteMaterialClient(t *testing.T, material RemoteMaterial) *http.Client {
	t.Helper()
	certificate, err := tls.X509KeyPair(material.ClientCertificate, material.ClientPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(material.CACertificate) {
		t.Fatal("append remote gateway CA")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate},
	}}}
}
