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
	"sync"
	"testing"
	"time"
)

const (
	testRealCredential = "sk-ant-oat01-real-credential-abcdefghijklmnopqrstuvwxyz"
	testRequestBody    = "request-body-must-not-be-logged"
)

// logSink collects gateway logs. The gateway logs from its request goroutines,
// so reads from the test goroutine must be synchronized.
type logSink struct {
	mu    sync.Mutex
	lines strings.Builder
}

func (s *logSink) Logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(&s.lines, format, args...)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines.String()
}

// waitFor blocks until the logs contain substr and returns everything logged so
// far, so assertions never race the goroutine that writes the entry.
func (s *logSink) waitFor(t *testing.T, substr string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		logged := s.String()
		if strings.Contains(logged, substr) {
			return logged
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs never contained %q: %q", substr, logged)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

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

	var logs logSink
	gateway, err := Start(logs.Logf)
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

	// The gateway logs the request after the response is proxied back, so the
	// client returning does not mean the log line exists yet.
	entry := logs.waitFor(t, "job_id=job-123")
	if !strings.Contains(entry, "method=POST") || !strings.Contains(entry, "status=201") {
		t.Fatalf("safe request log = %q", entry)
	}
	for name, token := range map[string]string{
		"credential":  testRealCredential,
		"placeholder": placeholder,
		"header":      "Authorization",
		"body":        testRequestBody,
	} {
		if strings.Contains(entry, token) {
			t.Fatalf("logs contain %s: %q", name, entry)
		}
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

func TestGenericProxyLeaseBearerRotationRevocationAndPathPinning(t *testing.T) {
	const rotatedCredential = "rotated-real-credential-874"
	var mu sync.Mutex
	credential := testRealCredential
	granted := true
	upstreamCalls := 0
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		upstreamCalls++
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Api-Key")+" "+r.Host)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	var logs logSink
	gateway, err := Start(logs.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	policy := ProxyPolicy{Upstream: upstream.URL + "/api/v1/", AuthKind: ProxyAuthBearer, AllowLoopbackHTTP: true}
	resolver := func(context.Context) (ResolvedCredential, error) {
		mu.Lock()
		defer mu.Unlock()
		if !granted {
			return ResolvedCredential{}, fmt.Errorf("revoked")
		}
		return ResolvedCredential{Value: credential, Upstream: policy.Upstream, AuthKind: ProxyAuthBearer}, nil
	}
	lease, err := gateway.RegisterProxy("proxy-job", policy, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(lease.Placeholder(), "gitmoot-kc-proxy-job-") || !strings.HasPrefix(lease.URL(), gateway.URL()+"/_gitmoot/proxy/") {
		t.Fatalf("lease placeholder/url = %q %q", lease.Placeholder(), lease.URL())
	}

	call := func(wantStatus int) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, lease.URL()+"/messages?stream=true", strings.NewReader(testRequestBody))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+lease.Placeholder())
		request.Header.Set("X-Api-Key", lease.Placeholder())
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != wantStatus {
			t.Fatalf("status = %d, want %d", response.StatusCode, wantStatus)
		}
	}
	call(http.StatusNoContent)
	mu.Lock()
	credential = rotatedCredential
	mu.Unlock()
	call(http.StatusNoContent)

	mu.Lock()
	if upstreamCalls != 2 || len(seen) != 2 || !strings.Contains(seen[0], "POST /api/v1/messages?stream=true Bearer "+testRealCredential+"  ") || !strings.Contains(seen[1], "Bearer "+rotatedCredential) {
		t.Fatalf("upstream calls=%d seen=%q", upstreamCalls, seen)
	}
	mu.Unlock()

	refused := []struct {
		url  string
		host string
	}{
		{url: lease.URL() + "/safe", host: "other.example.test"},
		{url: lease.URL() + "/safe/%2e%2e/escape"},
	}
	for _, probe := range refused {
		request, _ := http.NewRequest(http.MethodGet, probe.url, nil)
		request.Header.Set("Authorization", "Bearer "+lease.Placeholder())
		if probe.host != "" {
			request.Host = probe.host
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("escaped/foreign request status = %d", response.StatusCode)
		}
	}
	mu.Lock()
	if upstreamCalls != 2 {
		t.Fatalf("refused requests reached upstream: calls=%d", upstreamCalls)
	}
	granted = false
	mu.Unlock()
	call(http.StatusUnauthorized)
	mu.Lock()
	if upstreamCalls != 2 {
		t.Fatalf("revoked request reached upstream: calls=%d", upstreamCalls)
	}
	mu.Unlock()

	logged := logs.waitFor(t, "job_id=proxy-job")
	for _, forbidden := range []string{testRealCredential, rotatedCredential, lease.Placeholder(), testRequestBody, "Authorization", "X-Api-Key"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("gateway log leaked %q: %q", forbidden, logged)
		}
	}
	lease.Revoke()
	call(http.StatusUnauthorized)
}

func TestGenericProxyLeaseCustomHeaderAndRedirectRefusal(t *testing.T) {
	const headerName = "X-Service-Token"
	var gotHeader, gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(headerName)
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	policy := ProxyPolicy{Upstream: upstream.URL + "/fixed", AuthKind: ProxyAuthHeader, Header: headerName, AllowLoopbackHTTP: true}
	lease, err := gateway.RegisterProxy("header-job", policy, func(context.Context) (ResolvedCredential, error) {
		return ResolvedCredential{Value: testRealCredential, Upstream: policy.Upstream, AuthKind: policy.AuthKind, Header: policy.Header}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, lease.URL(), nil)
	request.Header.Set(headerName, lease.Placeholder())
	request.Header.Set("Authorization", "Bearer should-be-stripped")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || gotHeader != testRealCredential || gotAuthorization != "" {
		t.Fatalf("status=%d header=%q authorization=%q", response.StatusCode, gotHeader, gotAuthorization)
	}

	redirectCalls := 0
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectCalls++ }))
	defer redirectTarget.Close()
	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()
	redirectPolicy := ProxyPolicy{Upstream: redirectSource.URL, AuthKind: ProxyAuthBearer, AllowLoopbackHTTP: true}
	redirectLease, err := gateway.RegisterProxy("redirect-job", redirectPolicy, func(context.Context) (ResolvedCredential, error) {
		return ResolvedCredential{Value: testRealCredential, Upstream: redirectPolicy.Upstream, AuthKind: ProxyAuthBearer}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	redirectRequest, _ := http.NewRequest(http.MethodGet, redirectLease.URL(), nil)
	redirectRequest.Header.Set("Authorization", "Bearer "+redirectLease.Placeholder())
	redirectResponse, err := http.DefaultClient.Do(redirectRequest)
	if err != nil {
		t.Fatal(err)
	}
	redirectResponse.Body.Close()
	if redirectResponse.StatusCode != http.StatusBadGateway || redirectCalls != 0 {
		t.Fatalf("redirect status=%d target_calls=%d", redirectResponse.StatusCode, redirectCalls)
	}
}

func TestGenericProxyConcurrentLeasesRemainIsolated(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	policy := ProxyPolicy{Upstream: upstream.URL, AuthKind: ProxyAuthBearer, AllowLoopbackHTTP: true}
	makeLease := func(jobID, value string) *Lease {
		lease, err := gateway.RegisterProxy(jobID, policy, func(context.Context) (ResolvedCredential, error) {
			return ResolvedCredential{Value: value, Upstream: policy.Upstream, AuthKind: ProxyAuthBearer}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	first := makeLease("concurrent-a", "real-a")
	second := makeLease("concurrent-b", "real-b")

	wrong, _ := http.NewRequest(http.MethodGet, first.URL(), nil)
	wrong.Header.Set("Authorization", "Bearer "+second.Placeholder())
	wrongResponse, err := http.DefaultClient.Do(wrong)
	if err != nil {
		t.Fatal(err)
	}
	wrongResponse.Body.Close()
	if wrongResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-lease placeholder status=%d", wrongResponse.StatusCode)
	}

	var wait sync.WaitGroup
	for _, lease := range []*Lease{first, second} {
		for i := 0; i < 10; i++ {
			wait.Add(1)
			go func(lease *Lease) {
				defer wait.Done()
				request, _ := http.NewRequest(http.MethodGet, lease.URL(), nil)
				request.Header.Set("Authorization", "Bearer "+lease.Placeholder())
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Errorf("request: %v", err)
					return
				}
				response.Body.Close()
				if response.StatusCode != http.StatusNoContent {
					t.Errorf("status=%d", response.StatusCode)
				}
			}(lease)
		}
	}
	wait.Wait()
	mu.Lock()
	defer mu.Unlock()
	if seen["Bearer real-a"] != 10 || seen["Bearer real-b"] != 10 || len(seen) != 2 {
		t.Fatalf("isolated credentials = %#v", seen)
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
