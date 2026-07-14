package credgw

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testRealCredential = "sk-ant-oat01-real-credential-abcdefghijklmnopqrstuvwxyz"
	testRequestBody    = "request-body-must-not-be-logged"
)

func TestGatewayPlaceholderLifecycleAndCredentialCustody(t *testing.T) {
	var upstreamAuthorization string
	var upstreamAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuthorization = r.Header.Get("Authorization")
		upstreamAPIKey = r.Header.Get("X-Api-Key")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	var logs strings.Builder
	gateway, err := Start(func(format string, args ...any) { fmt.Fprintf(&logs, format, args...) })
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	if !strings.HasPrefix(gateway.URL(), "http://127.0.0.1:") {
		t.Fatalf("gateway URL = %q", gateway.URL())
	}

	placeholder, err := gateway.Register("job-123", Credential{Kind: CredentialBearer, Value: testRealCredential}, testPolicy(t, upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(placeholder, "gitmoot-kc-job-123-") || strings.Contains(placeholder, testRealCredential) {
		t.Fatalf("placeholder format = %q", placeholder)
	}

	request, err := http.NewRequest(http.MethodPost, gateway.URL()+"/v1/messages", strings.NewReader(testRequestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+placeholder)
	request.Header.Set("X-Api-Key", placeholder)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if upstreamAuthorization != "Bearer "+testRealCredential {
		t.Fatalf("upstream Authorization = %q", upstreamAuthorization)
	}
	if upstreamAPIKey != "" {
		t.Fatalf("placeholder reached upstream x-api-key = %q", upstreamAPIKey)
	}

	for name, token := range map[string]string{
		"credential":  testRealCredential,
		"placeholder": placeholder,
		"header":      "Authorization",
		"body":        testRequestBody,
	} {
		if strings.Contains(logs.String(), token) {
			t.Fatalf("logs contain %s: %q", name, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "method=POST") || !strings.Contains(logs.String(), "status=201") || !strings.Contains(logs.String(), "job_id=job-123") {
		t.Fatalf("safe request log = %q", logs.String())
	}

	gateway.Revoke(placeholder)
	assertGatewayStatus(t, gateway.URL(), placeholder, http.StatusUnauthorized)
	assertGatewayStatus(t, gateway.URL(), "gitmoot-kc-unknown", http.StatusUnauthorized)
}

func TestGatewayAttachesAPIKeyWithoutForwardingPlaceholder(t *testing.T) {
	var gotAuthorization, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	placeholder, err := gateway.Register("api-key-job", Credential{Kind: CredentialAPIKey, Value: testRealCredential}, testPolicy(t, upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, gateway.URL()+"/v1/messages", nil)
	request.Header.Set("Authorization", "Bearer "+placeholder)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if gotAuthorization != "" || gotAPIKey != testRealCredential {
		t.Fatalf("upstream auth = Authorization %q, X-Api-Key %q", gotAuthorization, gotAPIKey)
	}
}

func TestGatewayStreamsFlushedChunks(t *testing.T) {
	firstSent := make(chan struct{})
	releaseSecond := make(chan struct{})
	defer func() {
		select {
		case <-releaseSecond:
		default:
			close(releaseSecond)
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(firstSent)
		<-releaseSecond
		_, _ = io.WriteString(w, "data: second\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	placeholder, err := gateway.Register("stream-job", Credential{Kind: CredentialBearer, Value: testRealCredential}, testPolicy(t, upstream.URL))
	if err != nil {
		t.Fatal(err)
	}

	request, _ := http.NewRequest(http.MethodGet, gateway.URL()+"/v1/messages", nil)
	request.Header.Set("Authorization", "Bearer "+placeholder)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	<-firstSent
	line := make(chan string, 1)
	go func() {
		value, _ := bufio.NewReader(response.Body).ReadString('\n')
		line <- value
	}()
	select {
	case got := <-line:
		if got != "data: first\n" {
			t.Fatalf("first chunk = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first SSE chunk was buffered behind the second")
	}
	close(releaseSecond)
}

func TestGatewayRejectsUnallowlistedUpstream(t *testing.T) {
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	_, err = gateway.Register("blocked-job", Credential{Kind: CredentialBearer, Value: testRealCredential}, Policy{
		Upstream:     "https://api.anthropic.com",
		AllowedHosts: []string{"example.com"},
	})
	if err == nil || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("error = %v", err)
	}
}

func TestGatewayClosedFailsLeaseRegistration(t *testing.T) {
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = gateway.Register("closed-job", Credential{Kind: CredentialBearer, Value: testRealCredential}, Policy{
		Upstream:     DefaultAnthropicUpstream,
		AllowedHosts: []string{"api.anthropic.com"},
	})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("error = %v", err)
	}
}

func TestRegistryUsesOneGatewayPerHome(t *testing.T) {
	registry := NewRegistry()
	home := t.TempDir()
	first, err := registry.Gateway(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Gateway(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same home received different model gateways")
	}
	other, err := registry.Gateway(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first == other {
		t.Fatal("different homes shared a model gateway")
	}
	defer other.Close(context.Background())
	if err := registry.CloseHome(context.Background(), home); err != nil {
		t.Fatal(err)
	}
}

func assertGatewayStatus(t *testing.T, gatewayURL, placeholder string, want int) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, gatewayURL+"/v1/messages", bytes.NewReader(nil))
	request.Header.Set("Authorization", "Bearer "+placeholder)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("status = %d, want %d", response.StatusCode, want)
	}
}

func testPolicy(t *testing.T, upstream string) Policy {
	t.Helper()
	parsed, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	return Policy{Upstream: upstream, AllowedHosts: []string{parsed.Hostname()}}
}
