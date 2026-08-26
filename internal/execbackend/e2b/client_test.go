package e2b

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	testAPIKey             = "e2b-GITMOOT-COC-IMPL-secret-value"
	measuredAbsent404Body  = `{"code":404,"message":"Sandbox \"isandboxdoesnotexist000\" doesn't exist or you don't have access to it"}`
	measuredRouting404Body = `{"code":404,"message":"validation error: no matching operation was found"}`
)

func TestClientControlPlaneOperations(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("X-API-Key"); got != testAPIKey {
			t.Errorf("X-API-Key = %q, want caller key", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		switch r.Method + " " + r.URL.EscapedPath() {
		case "POST /sandboxes":
			if got := readBody(t, r); got != `{"templateID":"template-a","timeout":600,"metadata":{"job":"42"},"envVars":{"MODE":"test"}}` {
				t.Errorf("create body = %s", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandboxID":"sbx-1","templateID":"template-a","clientID":"client","envdVersion":"1.0"}`))
		case "GET /v2/sandboxes":
			if got := r.URL.Query().Get("limit"); got != "100" {
				t.Errorf("list limit = %q, want 100", got)
			}
			_, _ = w.Write([]byte(`[{"sandboxID":"sbx-1","templateID":"template-a","clientID":"client","envdVersion":"1.0","state":"running"}]`))
		case "GET /sandboxes/sbx-1":
			_, _ = w.Write([]byte(`{"sandboxID":"sbx-1","templateID":"template-a","clientID":"client","envdVersion":"1.0","startedAt":"2026-08-26T10:00:00Z","endAt":"2026-08-26T11:00:00Z","state":"running"}`))
		case "DELETE /sandboxes/sbx-1":
			w.WriteHeader(http.StatusNoContent)
		case "POST /sandboxes/sbx-1/timeout":
			if got := readBody(t, r); got != `{"timeout":1200}` {
				t.Errorf("timeout body = %s", got)
			}
			w.WriteHeader(http.StatusNoContent)
		case "GET /sandboxes/sbx-1/metrics":
			_, _ = w.Write([]byte(`[{"timestampUnix":1787738400,"cpuCount":2,"cpuUsedPct":12.5,"memUsed":10,"memTotal":20,"memCache":3,"diskUsed":30,"diskTotal":40}]`))
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, time.Second, server.Client())
	ctx := context.Background()

	created, err := client.Create(ctx, "template-a", 10*time.Minute, CreateOptions{
		Metadata: map[string]string{"job": "42"},
		Env:      map[string]string{"MODE": "test"},
	})
	if err != nil || created.ID != "sbx-1" {
		t.Fatalf("Create = %+v, %v", created, err)
	}
	listed, err := client.List(ctx)
	if err != nil || len(listed) != 1 || listed[0].State != "running" {
		t.Fatalf("List = %+v, %v", listed, err)
	}
	observed, err := client.Get(ctx, "sbx-1")
	if err != nil || observed.State != Present || observed.Sandbox == nil || observed.Sandbox.ID != "sbx-1" {
		t.Fatalf("Get = %+v, %v", observed, err)
	}
	if state, err := client.SetTimeout(ctx, "sbx-1", 20*time.Minute); err != nil || state != Present {
		t.Fatalf("SetTimeout = %s, %v", state, err)
	}
	metrics, err := client.Metrics(ctx, "sbx-1")
	if err != nil || metrics.State != Present || len(metrics.Metrics) != 1 || metrics.Metrics[0].CPUUsedPct != 12.5 {
		t.Fatalf("Metrics = %+v, %v", metrics, err)
	}
	if state, err := client.Delete(ctx, "sbx-1"); err != nil || state != Gone {
		t.Fatalf("Delete = %s, %v", state, err)
	}
	if got := calls.Load(); got != 6 {
		t.Fatalf("calls = %d, want 6", got)
	}
}

func TestClientListFollowsPagination(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v2/sandboxes" {
			http.Error(w, "wrong list route", http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		switch token := r.URL.Query().Get("nextToken"); token {
		case "":
			w.Header().Set("X-Next-Token", "page-2")
			_, _ = w.Write([]byte(`[{"sandboxID":"sbx-1"}]`))
		case "page-2":
			_, _ = w.Write([]byte(`[{"sandboxID":"sbx-2"}]`))
		default:
			http.Error(w, "unexpected token", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second, server.Client())

	sandboxes, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxes) != 2 || sandboxes[0].ID != "sbx-1" || sandboxes[1].ID != "sbx-2" {
		t.Fatalf("List = %+v, want both pages in order", sandboxes)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("list calls = %d, want 2", got)
	}
}

func TestClientRequiresCreationTTL(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second, server.Client())

	for _, ttl := range []time.Duration{0, -time.Second, 1500 * time.Millisecond} {
		_, err := client.Create(context.Background(), "template-a", ttl, CreateOptions{})
		if err == nil {
			t.Errorf("Create TTL %s succeeded", ttl)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
}

func TestClientRejectsShortAPIKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"", "short", "       "} {
		if _, err := NewClient(key, Options{}); err == nil {
			t.Errorf("NewClient(%q) succeeded", key)
		}
	}
}

func TestClientFailureModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "4xx", status: http.StatusBadRequest, body: `{"message":"bad request"}`},
		{name: "5xx", status: http.StatusServiceUnavailable, body: `{"message":"unavailable"}`},
		{name: "malformed body", status: http.StatusOK, body: `{"sandboxID":`},
		{name: "missing required ID", status: http.StatusOK, body: `{}`},
		{name: "multiple JSON values", status: http.StatusOK, body: `{"sandboxID":"a"} {"sandboxID":"b"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, time.Second, server.Client())

			observed, err := client.Get(context.Background(), "sbx-1")
			if err == nil || observed.State != Unknown {
				t.Fatalf("Get = %+v, %v; want unknown error", observed, err)
			}
		})
	}
}

func TestClientCollectionNotFoundIsAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second, server.Client())

	listed, err := client.List(context.Background())
	if err == nil || listed != nil {
		t.Fatalf("List = %+v, %v; collection 404 must not look like an empty successful list", listed, err)
	}
}

func TestClientThreeStateObservation(t *testing.T) {
	t.Parallel()

	const sandboxID = "isandboxdoesnotexist000"
	tests := []struct {
		name          string
		status        int
		body          string
		listStatus    int
		listBody      string
		wantListCalls int32
		wantState     State
		wantErr       bool
	}{
		{name: "present", status: http.StatusOK, body: `{"sandboxID":"isandboxdoesnotexist000"}`, wantState: Present},
		{name: "inventory confirms absent", status: http.StatusNotFound, body: measuredAbsent404Body, listStatus: http.StatusOK, listBody: `[]`, wantListCalls: 1, wantState: Gone},
		{name: "inventory confirms routing failure is inconclusive", status: http.StatusNotFound, body: measuredRouting404Body, listStatus: http.StatusOK, listBody: `[{"sandboxID":"isandboxdoesnotexist000"}]`, wantListCalls: 1, wantState: Unknown, wantErr: true},
		{name: "gone response still requires inventory", status: http.StatusGone, body: `{"code":410,"message":"sandbox gone"}`, listStatus: http.StatusOK, listBody: `[]`, wantListCalls: 1, wantState: Gone},
		{name: "unauthorized", status: http.StatusUnauthorized, wantState: Unknown, wantErr: true},
		{name: "provider unavailable", status: http.StatusServiceUnavailable, wantState: Unknown, wantErr: true},
		{name: "inconclusive malformed read", status: http.StatusOK, body: `{`, wantState: Unknown, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var listCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() == "/v2/sandboxes" {
					listCalls.Add(1)
					if test.listStatus == 0 {
						http.Error(w, "unexpected inventory request", http.StatusInternalServerError)
						return
					}
					w.WriteHeader(test.listStatus)
					_, _ = w.Write([]byte(test.listBody))
					return
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, time.Second, server.Client())

			observed, err := client.Get(context.Background(), sandboxID)
			if observed.State != test.wantState || (err != nil) != test.wantErr {
				t.Fatalf("Get state/error = %s/%v, want %s/error=%v", observed.State, err, test.wantState, test.wantErr)
			}
			if got := listCalls.Load(); got != test.wantListCalls {
				t.Fatalf("inventory calls = %d, want %d", got, test.wantListCalls)
			}
		})
	}

	t.Run("unreachable is unknown", func(t *testing.T) {
		client := newTestClient(t, "http://e2b.invalid", time.Second, &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("network unreachable")
			}),
		})
		observed, err := client.Get(context.Background(), sandboxID)
		if err == nil || observed.State != Unknown {
			t.Fatalf("Get = %+v, %v; unreachable must remain unknown", observed, err)
		}
	})
}

func TestClientIDOperationsPreserveState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		path      string
		status    int
		body      string
		operation func(context.Context, *Client) (State, error)
		wantState State
		wantErr   bool
	}{
		{
			name:      "delete success confirms gone",
			method:    http.MethodDelete,
			path:      "/sandboxes/sbx-1",
			status:    http.StatusNoContent,
			operation: func(ctx context.Context, client *Client) (State, error) { return client.Delete(ctx, "sbx-1") },
			wantState: Gone,
		},
		{
			name:      "delete absent response is inconclusive",
			method:    http.MethodDelete,
			path:      "/sandboxes/sbx-1",
			status:    http.StatusNotFound,
			body:      measuredAbsent404Body,
			operation: func(ctx context.Context, client *Client) (State, error) { return client.Delete(ctx, "sbx-1") },
			wantState: Unknown,
			wantErr:   true,
		},
		{
			name:      "delete routing response is inconclusive",
			method:    http.MethodDelete,
			path:      "/sandboxes/sbx-1",
			status:    http.StatusNotFound,
			body:      measuredRouting404Body,
			operation: func(ctx context.Context, client *Client) (State, error) { return client.Delete(ctx, "sbx-1") },
			wantState: Unknown,
			wantErr:   true,
		},
		{
			name:      "delete failure is unknown",
			method:    http.MethodDelete,
			path:      "/sandboxes/sbx-1",
			status:    http.StatusInternalServerError,
			operation: func(ctx context.Context, client *Client) (State, error) { return client.Delete(ctx, "sbx-1") },
			wantState: Unknown,
			wantErr:   true,
		},
		{
			name:   "timeout success confirms present",
			method: http.MethodPost,
			path:   "/sandboxes/sbx-1/timeout",
			status: http.StatusNoContent,
			operation: func(ctx context.Context, client *Client) (State, error) {
				return client.SetTimeout(ctx, "sbx-1", time.Minute)
			},
			wantState: Present,
		},
		{
			name:   "timeout not found is inconclusive",
			method: http.MethodPost,
			path:   "/sandboxes/sbx-1/timeout",
			status: http.StatusNotFound,
			body:   measuredAbsent404Body,
			operation: func(ctx context.Context, client *Client) (State, error) {
				return client.SetTimeout(ctx, "sbx-1", time.Minute)
			},
			wantState: Unknown,
			wantErr:   true,
		},
		{
			name:   "metrics failure is unknown",
			method: http.MethodGet,
			path:   "/sandboxes/sbx-1/metrics",
			status: http.StatusBadGateway,
			operation: func(ctx context.Context, client *Client) (State, error) {
				observed, err := client.Metrics(ctx, "sbx-1")
				return observed.State, err
			},
			wantState: Unknown,
			wantErr:   true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != test.method || r.URL.EscapedPath() != test.path {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"route not found"}`))
					return
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, time.Second, server.Client())

			state, err := test.operation(context.Background(), client)
			if state != test.wantState || (err != nil) != test.wantErr {
				t.Fatalf("state/error = %s/%v, want %s/error=%v", state, err, test.wantState, test.wantErr)
			}
		})
	}
}

func TestClientRejectsOversizedSuccessBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sandboxID":"sbx-1"}` + strings.Repeat(" ", maxProviderResponseBodyBytes)))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second, server.Client())

	observed, err := client.Get(context.Background(), "sbx-1")
	if err == nil || observed.State != Unknown || !strings.Contains(err.Error(), "oversized response") {
		t.Fatalf("Get = %+v, %v; want unknown oversized-response error", observed, err)
	}
}

func TestClientTimeoutAndCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	t.Run("bounded timeout", func(t *testing.T) {
		client := newTestClient(t, server.URL, 20*time.Millisecond, server.Client())
		started := time.Now()
		observed, err := client.Get(context.Background(), "sbx-1")
		if err == nil || observed.State != Unknown || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Get = %+v, %v; want unknown deadline error", observed, err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("bounded call took %s", elapsed)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		client := newTestClient(t, server.URL, time.Second, server.Client())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		observed, err := client.Get(ctx, "sbx-1")
		if err == nil || observed.State != Unknown || !errors.Is(err, context.Canceled) {
			t.Fatalf("Get = %+v, %v; want unknown cancellation error", observed, err)
		}
	})
}

func TestClientRedactsAPIKeyFromErrorsAndLogs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"message":"provider echoed %s"}`, testAPIKey)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second, server.Client())

	_, err := client.Get(context.Background(), "sbx-1")
	if err == nil {
		t.Fatal("Get succeeded")
	}
	var logs bytes.Buffer
	log.New(&logs, "control plane: ", 0).Printf("%v", err)
	for name, rendered := range map[string]string{"error": err.Error(), "log": logs.String()} {
		if strings.Contains(rendered, testAPIKey) {
			t.Fatalf("%s leaked API key: %q", name, rendered)
		}
		if !strings.Contains(rendered, "[REDACTED]") {
			t.Fatalf("%s = %q, want redaction marker", name, rendered)
		}
	}
}

func TestClientRedactsAPIKeyAtErrorBodyBoundary(t *testing.T) {
	t.Parallel()

	// The old pre-redaction limit retained only this seven-byte key prefix.
	padding := strings.Repeat("x", workflow.MaxStderrTailBytes-6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, padding+testAPIKey+"-tail")
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second, server.Client())

	_, err := client.Get(context.Background(), "sbx-1")
	if err == nil {
		t.Fatal("Get succeeded")
	}
	rendered := err.Error()
	if strings.Contains(rendered, testAPIKey) || strings.Contains(rendered, testAPIKey[:7]) {
		t.Fatalf("boundary error leaked API key material: %q", rendered)
	}
	if !strings.Contains(rendered, "[REDACTED]") {
		t.Fatalf("boundary error = %q, want redaction marker", rendered)
	}
}

func TestClientErrorChainDoesNotExposeAPIKey(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, "http://e2b.invalid", time.Second, &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			inner := fmt.Errorf("inner %s: %w", testAPIKey, context.Canceled)
			return nil, fmt.Errorf("outer %s: %w", testAPIKey, inner)
		}),
	})

	_, err := client.Get(context.Background(), "sbx-1")
	if err == nil {
		t.Fatal("Get succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(%v, context.Canceled) = false", err)
	}
	for level, current := 0, err; current != nil; level, current = level+1, errors.Unwrap(current) {
		if strings.Contains(current.Error(), testAPIKey) {
			t.Fatalf("unwrap level %d leaked API key: %q", level, current.Error())
		}
	}
}

func TestClientRefusesRedirectWithoutForwardingAPIKey(t *testing.T) {
	t.Parallel()

	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		if got := r.Header.Get("X-API-Key"); got != "" {
			t.Errorf("redirect forwarded API key %q", got)
		}
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := newTestClient(t, source.URL, time.Second, source.Client())

	observed, err := client.Get(context.Background(), "sbx-1")
	if err == nil || observed.State != Unknown {
		t.Fatalf("Get = %+v, %v; redirect must fail inconclusively", observed, err)
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("redirect destination calls = %d, want 0", got)
	}
}

func newTestClient(t *testing.T, baseURL string, timeout time.Duration, httpClient *http.Client) *Client {
	t.Helper()
	client, err := NewClient(testAPIKey, Options{
		BaseURL:        baseURL,
		HTTPClient:     httpClient,
		RequestTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	var body bytes.Buffer
	if _, err := body.ReadFrom(r.Body); err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return body.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
