package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeServer struct {
	mu      sync.Mutex
	calls   int
	headers []http.Header
	paths   []string
	bodies  []string
	server  *httptest.Server
}

func newFakeServer(t *testing.T, reply func(n int) (int, http.Header, string)) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		f.mu.Lock()
		f.calls++
		n := f.calls
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.Path)
		f.bodies = append(f.bodies, string(raw))
		f.mu.Unlock()
		status, header, body := reply(n)
		for key, values := range header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func testClient(endpoint string) *Client {
	return &Client{
		Endpoint:   endpoint,
		APIKey:     "test-key",
		Timeout:    5 * time.Second,
		MaxRetries: 2,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	}
}

const okAnswer = `{"model":"typesafe/jev-1.13","answers":{"risk":{"type":"noul","noul":0.12}}}`

func TestClientSendsBearerToSystemOneAndDecodesAnswers(t *testing.T) {
	server := newFakeServer(t, func(int) (int, http.Header, string) { return http.StatusOK, nil, okAnswer })
	client := testClient(server.server.URL + "/api/v1/systemone")
	exchange, err := client.Evaluate(context.Background(), Request{
		Model:     DefaultModel,
		State:     map[string]any{"diff": "a < b"},
		Questions: map[string]Question{"risk": {Type: "noul", Instructions: "x"}},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := server.headers[0].Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("Authorization = %q", got)
	}
	if server.paths[0] != "/api/v1/systemone" {
		t.Fatalf("path = %q", server.paths[0])
	}
	if !strings.Contains(server.bodies[0], `"a < b"`) {
		t.Fatalf("diff text was HTML-escaped: %s", server.bodies[0])
	}
	answer := exchange.Response.Answers["risk"]
	if answer.Noul == nil || *answer.Noul != 0.12 {
		t.Fatalf("noul = %v", answer.Noul)
	}
}

func TestClientRetriesServerErrorsThenSucceeds(t *testing.T) {
	server := newFakeServer(t, func(n int) (int, http.Header, string) {
		if n == 1 {
			return http.StatusBadGateway, nil, "upstream down"
		}
		return http.StatusOK, nil, okAnswer
	})
	exchange, err := testClient(server.server.URL).Evaluate(context.Background(), Request{Model: DefaultModel})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if exchange.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", exchange.Attempts)
	}
}

func TestClientDoesNotRetryAuthFailureAndRedactsKey(t *testing.T) {
	server := newFakeServer(t, func(int) (int, http.Header, string) {
		return http.StatusUnauthorized, nil, "bad key test-key"
	})
	exchange, err := testClient(server.server.URL).Evaluate(context.Background(), Request{Model: DefaultModel})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || apiErr.Retryable {
		t.Fatalf("err = %v", err)
	}
	if exchange.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", exchange.Attempts)
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatalf("key leaked into error: %v", err)
	}
}

// A transient provider 403 ("RBAC: access denied" for a valid key) gets one
// retry; a lasting 403 fails after exactly two attempts, never more.
func TestClientRetriesOneForbiddenOnly(t *testing.T) {
	transient := newFakeServer(t, func(n int) (int, http.Header, string) {
		if n == 1 {
			return http.StatusForbidden, nil, `{"error":{"message":"HTTP 403: RBAC: access denied","code":403}}`
		}
		return http.StatusOK, nil, okAnswer
	})
	var waits []time.Duration
	client := testClient(transient.server.URL)
	client.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	exchange, err := client.Evaluate(context.Background(), Request{Model: DefaultModel})
	if err != nil || exchange.Attempts != 2 {
		t.Fatalf("transient 403: attempts = %d, err = %v; want success on attempt 2", exchange.Attempts, err)
	}
	if len(waits) != 1 || waits[0] != forbiddenRetryWait {
		t.Fatalf("waits = %v, want [%v]", waits, forbiddenRetryWait)
	}

	lasting := newFakeServer(t, func(int) (int, http.Header, string) {
		return http.StatusForbidden, nil, "denied"
	})
	exchange, err = testClient(lasting.server.URL).Evaluate(context.Background(), Request{Model: DefaultModel})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("lasting 403: err = %v", err)
	}
	if exchange.Attempts != 2 {
		t.Fatalf("lasting 403: attempts = %d, want 2 (MaxRetries is %d)", exchange.Attempts, testClient("").MaxRetries)
	}
}

func TestClientCapsRetryAfterAtMaxBackoff(t *testing.T) {
	server := newFakeServer(t, func(n int) (int, http.Header, string) {
		if n == 1 {
			return http.StatusTooManyRequests, http.Header{"Retry-After": {"3600"}}, "slow down"
		}
		return http.StatusOK, nil, okAnswer
	})
	client := testClient(server.server.URL)
	client.MaxBackoff = 2 * time.Second
	var waited []time.Duration
	client.Sleep = func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	}
	if _, err := client.Evaluate(context.Background(), Request{Model: DefaultModel}); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(waited) != 1 || waited[0] != 2*time.Second {
		t.Fatalf("waited = %v, want [2s]", waited)
	}
}
