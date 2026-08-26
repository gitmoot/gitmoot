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
)

const testAPIKey = "e2b-GITMOOT-COC-IMPL-secret-value"

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
		case "POST /v2/sandboxes":
			if got := readBody(t, r); got != `{"templateID":"template-a","timeout":600,"metadata":{"job":"42"},"envVars":{"MODE":"test"}}` {
				t.Errorf("create body = %s", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandboxID":"sbx-1","templateID":"template-a","clientID":"client","envdVersion":"1.0"}`))
		case "GET /v2/sandboxes":
			_, _ = w.Write([]byte(`[{"sandboxID":"sbx-1","templateID":"template-a","clientID":"client","envdVersion":"1.0","state":"running"}]`))
		case "GET /v2/sandboxes/sbx-1":
			_, _ = w.Write([]byte(`{"sandboxID":"sbx-1","templateID":"template-a","clientID":"client","envdVersion":"1.0","startedAt":"2026-08-26T10:00:00Z","endAt":"2026-08-26T11:00:00Z","state":"running"}`))
		case "DELETE /v2/sandboxes/sbx-1":
			w.WriteHeader(http.StatusNoContent)
		case "POST /v2/sandboxes/sbx-1/timeout":
			if got := readBody(t, r); got != `{"timeout":1200}` {
				t.Errorf("timeout body = %s", got)
			}
			w.WriteHeader(http.StatusNoContent)
		case "GET /v2/sandboxes/sbx-1/metrics":
			_, _ = w.Write([]byte(`[{"timestampUnix":1787738400,"cpuCount":2,"cpuUsedPct":12.5,"memUsed":10,"memTotal":20,"memCache":3,"diskUsed":30,"diskTotal":40}]`))
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/v2", time.Second, server.Client())
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

func TestClientRequiresCreationTTL(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v2", time.Second, server.Client())

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
			client := newTestClient(t, server.URL+"/v2", time.Second, server.Client())

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
	client := newTestClient(t, server.URL+"/v2", time.Second, server.Client())

	listed, err := client.List(context.Background())
	if err == nil || listed != nil {
		t.Fatalf("List = %+v, %v; collection 404 must not look like an empty successful list", listed, err)
	}
}

func TestClientThreeStateObservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		body      string
		wantState State
		wantErr   bool
	}{
		{name: "present", status: http.StatusOK, body: `{"sandboxID":"sbx-1"}`, wantState: Present},
		{name: "not found", status: http.StatusNotFound, wantState: Gone},
		{name: "provider confirms gone", status: http.StatusGone, wantState: Gone},
		{name: "unauthorized", status: http.StatusUnauthorized, wantState: Unknown, wantErr: true},
		{name: "provider unavailable", status: http.StatusServiceUnavailable, wantState: Unknown, wantErr: true},
		{name: "inconclusive malformed read", status: http.StatusOK, body: `{`, wantState: Unknown, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL+"/v2", time.Second, server.Client())

			observed, err := client.Get(context.Background(), "sbx-1")
			if observed.State != test.wantState || (err != nil) != test.wantErr {
				t.Fatalf("Get state/error = %s/%v, want %s/error=%v", observed.State, err, test.wantState, test.wantErr)
			}
		})
	}

	t.Run("unreachable is unknown", func(t *testing.T) {
		client := newTestClient(t, "http://e2b.invalid/v2", time.Second, &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("network unreachable")
			}),
		})
		observed, err := client.Get(context.Background(), "sbx-1")
		if err == nil || observed.State != Unknown {
			t.Fatalf("Get = %+v, %v; unreachable must remain unknown", observed, err)
		}
	})
}

func TestClientIDOperationsPreserveState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		body      string
		operation func(context.Context, *Client) (State, error)
		wantState State
		wantErr   bool
	}{
		{
			name:      "delete success confirms gone",
			status:    http.StatusNoContent,
			operation: func(ctx context.Context, client *Client) (State, error) { return client.Delete(ctx, "sbx-1") },
			wantState: Gone,
		},
		{
			name:      "delete not found confirms gone",
			status:    http.StatusNotFound,
			operation: func(ctx context.Context, client *Client) (State, error) { return client.Delete(ctx, "sbx-1") },
			wantState: Gone,
		},
		{
			name:      "delete failure is unknown",
			status:    http.StatusInternalServerError,
			operation: func(ctx context.Context, client *Client) (State, error) { return client.Delete(ctx, "sbx-1") },
			wantState: Unknown,
			wantErr:   true,
		},
		{
			name:   "timeout success confirms present",
			status: http.StatusNoContent,
			operation: func(ctx context.Context, client *Client) (State, error) {
				return client.SetTimeout(ctx, "sbx-1", time.Minute)
			},
			wantState: Present,
		},
		{
			name:   "timeout not found confirms gone",
			status: http.StatusNotFound,
			operation: func(ctx context.Context, client *Client) (State, error) {
				return client.SetTimeout(ctx, "sbx-1", time.Minute)
			},
			wantState: Gone,
		},
		{
			name:   "metrics failure is unknown",
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL+"/v2", time.Second, server.Client())

			state, err := test.operation(context.Background(), client)
			if state != test.wantState || (err != nil) != test.wantErr {
				t.Fatalf("state/error = %s/%v, want %s/error=%v", state, err, test.wantState, test.wantErr)
			}
		})
	}
}

func TestClientTimeoutAndCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	t.Run("bounded timeout", func(t *testing.T) {
		client := newTestClient(t, server.URL+"/v2", 20*time.Millisecond, server.Client())
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
		client := newTestClient(t, server.URL+"/v2", time.Second, server.Client())
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
	client := newTestClient(t, server.URL+"/v2", time.Second, server.Client())

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
	client := newTestClient(t, source.URL+"/v2", time.Second, source.Client())

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
