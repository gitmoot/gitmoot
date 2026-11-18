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

// gatewayRefusal is the body E2B's gateway sent on 2026-11-18 (#2680), when the
// account's spending limit had been reached.
const gatewayRefusal = "upstream connect error or disconnect/reset before headers. reset reason: remote connection failure"

// #2680: a caller retries a provider 5xx by its status, so every provider
// answer that is not the expected success carries a typed HTTPStatusError
// naming the status and the call, from the control plane and from envd alike.
func TestProviderStatusAnswersCarryTypedStatus(t *testing.T) {
	t.Parallel()
	status := http.StatusServiceUnavailable
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(gatewayRefusal))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, 10*time.Second, nil)
	_, _, createErr := client.Create(context.Background(), "template", time.Minute, CreateOptions{})
	_, deleteErr := client.Delete(context.Background(), "sbx-test")
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})
	_, startErr := envd.Start(context.Background(), StartRequest{Name: "true"})
	uploadErr := envd.Upload(context.Background(), "/home/user/file", strings.NewReader("data"))

	for _, testCase := range []struct {
		name      string
		err       error
		operation string
	}{
		{"create", createErr, OperationCreate},
		{"delete", deleteErr, OperationDelete},
		{"envd start", startErr, OperationEnvdStart},
		{"envd upload", uploadErr, OperationEnvdUpload},
	} {
		var statusErr *HTTPStatusError
		if !errors.As(testCase.err, &statusErr) {
			t.Fatalf("%s error %v carries no HTTPStatusError", testCase.name, testCase.err)
		}
		if statusErr.StatusCode != status || statusErr.Operation != testCase.operation {
			t.Fatalf("%s status error = %d/%s, want %d/%s", testCase.name, statusErr.StatusCode, statusErr.Operation, status, testCase.operation)
		}
		if !strings.Contains(testCase.err.Error(), gatewayRefusal) {
			t.Fatalf("%s error %q lost the provider's text", testCase.name, testCase.err)
		}
		// A 5xx stays ambiguous for the ledger: it never proves non-allocation.
		var refused *RequestRefusedError
		if errors.As(testCase.err, &refused) {
			t.Fatalf("%s 503 classified as a refusal", testCase.name)
		}
		if statusErr.Envd() != strings.HasPrefix(testCase.operation, "envd_") {
			t.Fatalf("%s Envd() = %v", testCase.name, statusErr.Envd())
		}
	}
}

// A create refused with a 4xx keeps its RequestRefusedError, the ledger's
// release signal, and still carries the status underneath.
func TestCreateRefusalAlsoCarriesTypedStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"code":402,"message":"payment required"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, 10*time.Second, nil)
	_, _, err := client.Create(context.Background(), "template", time.Minute, CreateOptions{})
	var refused *RequestRefusedError
	if !errors.As(err, &refused) || refused.Operation != OperationCreate || refused.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("create 402 error = %v, want a create RequestRefusedError", err)
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("create 402 error = %v carries no HTTPStatusError", err)
	}
}
