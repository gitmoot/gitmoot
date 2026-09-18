package e2b

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRefusalClassificationOverRealResponses pins (method, path, status) ->
// (Operation, refused?) against a REAL HTTP response rather than a fabricated
// error.
//
// It exists because the previous tests used a fake inner backend, so the
// classification the ledger depends on was never exercised end to end — and the
// gap hid two defects: a 404 case that was dead code behind an earlier return,
// and a refusal wrapper that could not tell a refused create from a refused
// cleanup delete.
//
// The distinction is load-bearing for money. A refused CREATE releases the cost
// reservation; anything else keeps the fail-safe hold, because the sandbox may
// exist and still be billing.
func TestRefusalClassificationOverRealResponses(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		status        int
		wantRefused   bool
		wantOperation string
		why           string
	}{
		{"create rejected by validation", http.StatusBadRequest, true, OperationCreate,
			"a 400 on the collection is proof nothing was allocated"},
		{"create unauthorized", http.StatusUnauthorized, true, OperationCreate,
			"credentials refused before any instance exists"},
		{"create forbidden", http.StatusForbidden, true, OperationCreate,
			"authorization refused before any instance exists"},
		{"create payment required", http.StatusPaymentRequired, true, OperationCreate,
			"billing refusal happens before allocation"},
		{"create unprocessable", http.StatusUnprocessableEntity, true, OperationCreate,
			"the request was rejected on its own terms"},
		{"create not found", http.StatusNotFound, true, OperationCreate,
			"a 404 on the COLLECTION cannot coexist with an allocated instance"},
		{"create rate limited", http.StatusTooManyRequests, false, "",
			"a 429 can race a completed allocation, so it stays ambiguous"},
		{"create server error", http.StatusInternalServerError, false, "",
			"a 5xx can follow a completed allocation, so it stays ambiguous"},
		{"create bad gateway", http.StatusBadGateway, false, "",
			"same ambiguity as any other 5xx"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(`{"code":` + http.StatusText(testCase.status) + `}`))
			}))
			defer server.Close()

			client := newTestClient(t, server.URL, 10*time.Second, nil)
			_, _, err := client.Create(context.Background(), "some-template", time.Minute, CreateOptions{})
			if err == nil {
				t.Fatal("Create succeeded despite the provider returning an error status")
			}

			var refused *RequestRefusedError
			gotRefused := errors.As(err, &refused)
			if gotRefused != testCase.wantRefused {
				t.Fatalf("refused = %v, want %v (%s); error was %v", gotRefused, testCase.wantRefused, testCase.why, err)
			}
			if gotRefused && refused.Operation != testCase.wantOperation {
				t.Fatalf("Operation = %q, want %q: the ledger releases only on a refused create", refused.Operation, testCase.wantOperation)
			}
		})
	}
}

// TestPerIDNotFoundStaysInconclusive pins the other half of the 404 split, and
// it is the half that must NOT be "improved" into a refusal.
//
// On a per-instance path, "never existed" and "already gone" are
// indistinguishable. Treating either as proof of non-allocation would release a
// reservation for a sandbox that may still be running — the double-run hazard,
// through the mechanism added to stop a leak.
func TestPerIDNotFoundStaysInconclusive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404}`))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, 10*time.Second, nil)
	_, err := client.Delete(context.Background(), "sandbox-that-may-exist")
	if err == nil {
		t.Fatal("Delete succeeded despite a 404")
	}
	var refused *RequestRefusedError
	if errors.As(err, &refused) {
		t.Fatalf("a per-id 404 was classified as a refusal (%s): it is ambiguous and must keep the fail-safe hold", refused.Operation)
	}
	if !strings.Contains(err.Error(), "inconclusive") {
		t.Fatalf("error = %v, want it marked inconclusive", err)
	}
}

// TestOperationForRequestCoversEveryCaller pins the mapping the ledger gate
// depends on. A mis-mapped operation silently turns the money gate into either a
// leak or a double-run.
func TestOperationForRequestCoversEveryCaller(t *testing.T) {
	for _, testCase := range []struct {
		method, path, want string
	}{
		{http.MethodPost, "/sandboxes", OperationCreate},
		{http.MethodPost, "/sandboxes/", OperationCreate},
		{http.MethodGet, "/sandboxes/abc", OperationGet},
		{http.MethodDelete, "/sandboxes/abc", OperationDelete},
		{http.MethodGet, "/sandboxes/abc/metrics", OperationMetrics},
		{http.MethodPost, "/sandboxes/abc/timeout", OperationSetTimeout},
	} {
		if got := operationForRequest(testCase.method, testCase.path); got != testCase.want {
			t.Errorf("operationForRequest(%s %s) = %q, want %q", testCase.method, testCase.path, got, testCase.want)
		}
	}
}
