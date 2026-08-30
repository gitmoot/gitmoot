package credgw

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
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
		t.Fatalf("redacted body lost control or exposed credential, length=%d", len(body))
	}
	if got := response.Header.Get("X-Reflected-Credential"); strings.Contains(got, testRealCredential) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("redacted response header length=%d", len(got))
	}
	logged := logs.waitFor(t, "job_id=remote-job")
	if strings.Contains(logged, testRealCredential) || strings.Contains(logged, material.Capability) || strings.Contains(logged, material.Placeholder) {
		t.Fatalf("gateway log contains protected material, length=%d", len(logged))
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

func TestRemoteProxyDecodesGzipBeforeCredentialRedaction(t *testing.T) {
	const visibleSentinel = "visible-gzip-positive-control"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("host transport Accept-Encoding = %q, want gzip", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		_, _ = io.WriteString(compressed, visibleSentinel+":"+testRealCredential)
		_ = compressed.Close()
	}))
	defer upstream.Close()

	gateway, material := newRemoteProxyTestLease(t, upstream.URL, "gzip-response")
	defer gateway.Close(context.Background())
	request, _ := http.NewRequest(http.MethodGet, material.URL, nil)
	request.Header.Set(CapabilityHeader, material.Capability)
	request.Header.Set("Authorization", "Bearer "+material.Placeholder)
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := remoteMaterialClient(t, material).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" {
		t.Fatalf("gateway response status=%d encoding=%q", response.StatusCode, response.Header.Get("Content-Encoding"))
	}
	if !strings.Contains(string(body), visibleSentinel) || strings.Contains(string(body), testRealCredential) || !strings.Contains(string(body), "[REDACTED]") {
		t.Fatalf("decoded response redaction failed, body length %d", len(body))
	}
}

func TestRemoteProxyRejectsUnsupportedContentEncoding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, "encoded-positive-control:"+testRealCredential)
	}))
	defer upstream.Close()

	gateway, material := newRemoteProxyTestLease(t, upstream.URL, "unsupported-response")
	defer gateway.Close(context.Background())
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
	if response.StatusCode != http.StatusBadGateway || strings.Contains(string(body), testRealCredential) || strings.Contains(string(body), "encoded-positive-control") {
		t.Fatalf("unsupported encoding status=%d body length=%d", response.StatusCode, len(body))
	}
}

func TestRemoteProxyRejectsUnexpectedHTTP2BeforeForwarding(t *testing.T) {
	const visibleSentinel = "visible-http2-positive-control"
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, visibleSentinel)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	upstreamClient := upstream.Client()
	controlResponse, err := upstreamClient.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	controlBody, err := io.ReadAll(controlResponse.Body)
	controlResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if controlResponse.ProtoMajor != 2 || string(controlBody) != visibleSentinel {
		t.Fatal("positive control did not produce the expected HTTP/2 response")
	}

	gateway, material := newRemoteProxyTestLease(t, upstream.URL, "http2-response")
	defer gateway.Close(context.Background())
	gateway.client = upstreamClient
	response := requestRemoteProxy(t, material)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadGateway || strings.Contains(string(body), visibleSentinel) {
		t.Fatalf("HTTP/2 response status=%d body length=%d", response.StatusCode, len(body))
	}
}

func TestRemoteProxyStreamsSSEBeforeUpstreamEOF(t *testing.T) {
	const (
		firstEvent  = "event: ready\ndata: first\n\n"
		secondEvent = "event: done\ndata: second\n\n"
	)
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, firstEvent)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, secondEvent)
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	gateway, material := newRemoteProxyTestLease(t, upstream.URL, "streaming-response")
	defer gateway.Close(context.Background())
	defer finish()
	request, _ := http.NewRequest(http.MethodGet, material.URL, nil)
	request.Header.Set(CapabilityHeader, material.Capability)
	request.Header.Set("Authorization", "Bearer "+material.Placeholder)
	client := remoteMaterialClient(t, material)
	responseResult := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	go func() {
		response, err := client.Do(request)
		responseResult <- struct {
			response *http.Response
			err      error
		}{response: response, err: err}
	}()

	var response *http.Response
	select {
	case result := <-responseResult:
		if result.err != nil {
			t.Fatal(result.err)
		}
		response = result.response
	case <-time.After(5 * time.Second):
		finish()
		t.Fatal("remote proxy did not release SSE headers before upstream EOF")
	}
	defer response.Body.Close()
	first := make([]byte, len(firstEvent))
	firstResult := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(response.Body, first)
		firstResult <- err
	}()
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		finish()
		t.Fatal("remote proxy did not flush the first SSE event")
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || string(first) != firstEvent {
		t.Fatalf("first SSE event status=%d content_type=%q bytes=%d", response.StatusCode, response.Header.Get("Content-Type"), len(first))
	}
	finish()
	rest, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != secondEvent {
		t.Fatalf("second SSE event bytes=%d", len(rest))
	}
}

func TestCredentialRedactorBoundsCarryAndRedactsSplitSecret(t *testing.T) {
	patterns := credentialRedactionPatterns(testRealCredential)
	maxPatternLength := 0
	var longestPattern []byte
	for _, pattern := range patterns {
		if len(pattern.value) > maxPatternLength {
			maxPatternLength = len(pattern.value)
			longestPattern = pattern.value
		}
	}
	var destination countingWriter
	redactor := &credentialRedactingWriter{destination: &destination, patterns: patterns}
	chunk := bytes.Repeat([]byte("ordinary-response-byte"), 2048)
	for range 256 {
		if _, err := redactor.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if len(redactor.pending) >= maxPatternLength {
			t.Fatalf("redactor retained %d bytes, bound is %d", len(redactor.pending), maxPatternLength-1)
		}
	}
	beforeBound := destination.written
	if _, err := redactor.Write(append([]byte("visible:"), longestPattern[:maxPatternLength-1]...)); err != nil {
		t.Fatal(err)
	}
	if destination.written <= beforeBound || len(redactor.pending) != maxPatternLength-1 {
		t.Fatalf("redactor progress=%v retained=%d bound=%d", destination.written > beforeBound, len(redactor.pending), maxPatternLength-1)
	}
	var splitOutput bytes.Buffer
	splitRedactor := &credentialRedactingWriter{destination: &splitOutput, patterns: patterns}
	if _, err := splitRedactor.Write([]byte("visible:" + testRealCredential[:19])); err != nil {
		t.Fatal(err)
	}
	if _, err := splitRedactor.Write([]byte(testRealCredential[19:] + ":done")); err != nil {
		t.Fatal(err)
	}
	if err := splitRedactor.flush(); err != nil {
		t.Fatal(err)
	}
	if output := splitOutput.String(); !strings.Contains(output, "visible:") || !strings.Contains(output, "[REDACTED]") || strings.Contains(output, testRealCredential) {
		t.Fatalf("split redaction output length=%d", len(output))
	}
}

func TestRemoteProxyDropsCredentialBearingResponseHeaderNames(t *testing.T) {
	credentialHeader := "X-" + testRealCredential
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(credentialHeader, "harmless")
		w.Header().Set("X-Visible-Control", "preserved")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	gateway, material := newRemoteProxyTestLease(t, upstream.URL, "header-name-response")
	defer gateway.Close(context.Background())
	response := requestRemoteProxy(t, material)
	response.Body.Close()
	for name := range response.Header {
		if strings.Contains(strings.ToLower(name), strings.ToLower(testRealCredential)) {
			t.Fatal("credential-bearing response header name was forwarded")
		}
	}
	if response.StatusCode != http.StatusNoContent || response.Header.Get("X-Visible-Control") != "preserved" {
		t.Fatalf("header-name response status=%d control=%q", response.StatusCode, response.Header.Get("X-Visible-Control"))
	}
}

func TestRemoteProxyPreservesOrdinaryResponse(t *testing.T) {
	const body = "ordinary response body"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("X-Visible-Control", "one")
		w.Header().Add("X-Visible-Control", "two")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	gateway, material := newRemoteProxyTestLease(t, upstream.URL, "ordinary-response")
	defer gateway.Close(context.Background())
	response := requestRemoteProxy(t, material)
	responseBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted || response.Header.Get("Content-Type") != "application/json" || !slices.Equal(response.Header.Values("X-Visible-Control"), []string{"one", "two"}) || string(responseBody) != body {
		t.Fatalf("ordinary response status=%d content_type=%q controls=%d body_bytes=%d", response.StatusCode, response.Header.Get("Content-Type"), len(response.Header.Values("X-Visible-Control")), len(responseBody))
	}
}

func TestRemoteProxyForwardsLargeUnencodedResponse(t *testing.T) {
	const responseSize = 2 << 20
	expected := bytes.Repeat([]byte("normal-proxy-response\n"), responseSize/len("normal-proxy-response\n")+1)
	expected = expected[:responseSize]
	expectedHash := sha256.Sum256(expected)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(expected)
	}))
	defer upstream.Close()

	gateway, material := newRemoteProxyTestLease(t, upstream.URL, "large-response")
	defer gateway.Close(context.Background())
	request, _ := http.NewRequest(http.MethodGet, material.URL, nil)
	request.Header.Set(CapabilityHeader, material.Capability)
	request.Header.Set("Authorization", "Bearer "+material.Placeholder)
	response, err := remoteMaterialClient(t, material).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || written != responseSize || !bytes.Equal(hash.Sum(nil), expectedHash[:]) {
		t.Fatalf("large response status=%d bytes=%d hash_match=%v", response.StatusCode, written, bytes.Equal(hash.Sum(nil), expectedHash[:]))
	}
}

func TestRegistryRemoteGatewayRejectsChangedCoordinates(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(RemoteListenerOptions) RemoteListenerOptions
	}{
		{name: "listen address", change: func(options RemoteListenerOptions) RemoteListenerOptions {
			options.ListenAddress = "127.0.0.1:1"
			return options
		}},
		{name: "advertise URL", change: func(options RemoteListenerOptions) RemoteListenerOptions {
			options.AdvertiseURL = "https://replacement.example:9443"
			return options
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := NewRegistry()
			home := t.TempDir()
			options := RemoteListenerOptions{ListenAddress: "127.0.0.1:0", AdvertiseURL: "https://initial.example:8443"}
			gateway, err := registry.RemoteGateway(home, nil, options)
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close(context.Background())
			reused, err := registry.RemoteGateway(home, nil, options)
			if err != nil || reused != gateway {
				t.Fatalf("identical options reuse gateway=%v err=%v", reused == gateway, err)
			}
			if stale, err := registry.RemoteGateway(home, nil, test.change(options)); err == nil || stale != nil || !strings.Contains(err.Error(), "configuration changed") {
				t.Fatalf("changed options gateway=%v error=%v", stale != nil, err)
			}
		})
	}
}

type countingWriter struct {
	written int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	w.written += int64(len(data))
	return len(data), nil
}

func requestRemoteProxy(t *testing.T, material RemoteMaterial) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, material.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(CapabilityHeader, material.Capability)
	request.Header.Set("Authorization", "Bearer "+material.Placeholder)
	response, err := remoteMaterialClient(t, material).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func newRemoteProxyTestLease(t *testing.T, upstreamURL, jobID string) (*Gateway, RemoteMaterial) {
	t.Helper()
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.EnableRemote(RemoteListenerOptions{ListenAddress: "127.0.0.1:0"}); err != nil {
		_ = gateway.Close(context.Background())
		t.Fatal(err)
	}
	lease, err := gateway.RegisterProxy(jobID, ProxyPolicy{
		Upstream: upstreamURL, AuthKind: ProxyAuthResolved, AllowLoopbackHTTP: true,
		SandboxID: "sandbox-" + jobID, Runtime: "shell", ExpiresAt: time.Now().Add(time.Minute),
		AllowedHosts: []string{"127.0.0.1"},
	}, func(context.Context) (ResolvedCredential, error) {
		return ResolvedCredential{Value: testRealCredential, Upstream: upstreamURL, AuthKind: ProxyAuthBearer}, nil
	})
	if err != nil {
		_ = gateway.Close(context.Background())
		t.Fatal(err)
	}
	return gateway, lease.RemoteMaterial()
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
