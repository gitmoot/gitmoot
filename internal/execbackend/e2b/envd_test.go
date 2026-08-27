package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testEnvdToken = "envd-GITMOOT-IMPL-sandbox-secret"

func TestEnvdStartStreamsAndBoundsOutput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertEnvdRequest(t, r, "/process.Process/Start", connectProtocolMediaType)
		if got := r.Header.Get("connect-protocol-version"); got != "1" {
			t.Errorf("connect-protocol-version = %q, want 1", got)
		}
		flag, body, err := readConnectFrame(r.Body)
		if err != nil || flag != 0 {
			t.Errorf("request frame = flag %d, %v", flag, err)
			return
		}
		var request startPayload
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Process.Command != "sh" || strings.Join(request.Process.Args, " ") != "-c echo test" || request.Process.Dir != "/workspace" || request.Process.Env["MODE"] != "test" {
			t.Errorf("start request = %+v", request)
		}

		w.Header().Set("Content-Type", connectProtocolMediaType)
		writeConnectTestFrame(t, w, 0, `{"event":{"start":{"pid":42}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"data":{"stdout":"cHJlZml4LQ=="}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"data":{"stdout":"dGFpbA=="}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"data":{"stderr":"d2FybmluZw=="}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"end":{"exitCode":0,"exited":true,"status":"exit status 0","error":""}}}`)
		writeConnectTestFrame(t, w, connectEndStreamFlag, `{}`)
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})

	var output bytes.Buffer
	var pid atomic.Int32
	stream, err := envd.Start(context.Background(), StartRequest{
		Name:           "sh",
		Args:           []string{"-c", "echo test"},
		Dir:            "/workspace",
		Env:            []string{"MODE=test"},
		Output:         &output,
		MaxOutputBytes: 4,
		OnStart:        func(value int) { pid.Store(int32(value)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if pid.Load() != 42 {
		t.Fatalf("start pid = %d, want 42", pid.Load())
	}
	if got := output.String(); got != "prefix-tailwarning" {
		t.Fatalf("streamed output = %q", got)
	}
	if result.Command != "sh" || strings.Join(result.Args, " ") != "-c echo test" || result.Stdout != "tail" || result.Stderr != "ning" {
		t.Fatalf("result = %+v", result)
	}
}

func TestEnvdStartNonzeroExitUsesMeasuredEndEvent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", connectProtocolMediaType)
		writeConnectTestFrame(t, w, 0, `{"event":{"start":{"pid":2000}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"end":{"exitCode":3, "exited":true, "status":"exit status 3", "error":"exit status 3"}}}`)
		writeConnectTestFrame(t, w, connectEndStreamFlag, `{}`)
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})

	stream, err := envd.Start(context.Background(), StartRequest{Name: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 || exitErr.Status != "exit status 3" {
		t.Fatalf("Wait = %+v, %v; want exit code 3", result, err)
	}
}

func TestEnvdUploadUsesProcessUserAndCredential(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertEnvdRequest(t, r, "/files", "application/octet-stream")
		if got := r.URL.Query().Get("path"); got != "/home/user/input.txt" {
			t.Errorf("upload path = %q", got)
		}
		if got := r.URL.Query().Get("username"); got != envdDefaultUser {
			t.Errorf("upload username = %q, want %q", got, envdDefaultUser)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != "payload" {
			t.Errorf("upload body = %q, %v", data, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"input.txt","path":"/home/user/input.txt","type":"file"}]`))
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})

	if err := envd.Upload(context.Background(), "/home/user/input.txt", strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
}

func TestEnvdRejectsMissingCredentialBeforeRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	envd, err := NewEnvd(Sandbox{ID: "sbx-1"}, EnvdCredential{}, EnvdOptions{
		HTTPClient:       server.Client(),
		EndpointResolver: func(string, int) string { return server.URL },
	})
	if err == nil || envd != nil || !strings.Contains(err.Error(), "credential is required") {
		t.Fatalf("NewEnvd = %+v, %v; want credential error", envd, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
}

func TestEnvdEndpointUsesSandboxIDAndDomainWithoutClientID(t *testing.T) {
	t.Parallel()

	var host string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		host = request.URL.Host
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`[]`)),
			Request:    request,
		}, nil
	})}
	envd, err := NewEnvd(Sandbox{ID: "sbx-1", ClientID: "deprecated-client", Domain: "sandbox.example"}, EnvdCredential{token: testEnvdToken}, EnvdOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if err := envd.Upload(context.Background(), "/home/user/input", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if host != "49983-sbx-1.sandbox.example" {
		t.Fatalf("envd host = %q", host)
	}
}

func TestEnvdStartTreats415AsCodecFailureAndRedacts(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		_, _ = w.Write([]byte("wrong codec " + testEnvdToken))
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})

	_, err := envd.Start(context.Background(), StartRequest{Name: "true"})
	if err == nil || !strings.Contains(err.Error(), "reached the route") || !strings.Contains(err.Error(), "rejected the Connect codec") {
		t.Fatalf("Start error = %v", err)
	}
	if strings.Contains(err.Error(), testEnvdToken) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("Start error leaked credential: %q", err)
	}
}

func TestEnvdCallsAreTimeoutBounded(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer server.Close()
	envd, err := NewEnvd(Sandbox{ID: "sbx-timeout"}, EnvdCredential{token: testEnvdToken}, EnvdOptions{
		HTTPClient:       server.Client(),
		RequestTimeout:   20 * time.Millisecond,
		EndpointResolver: func(string, int) string { return server.URL },
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "start handshake", call: func() error {
			_, err := envd.Start(context.Background(), StartRequest{Name: "true"})
			return err
		}},
		{name: "upload", call: func() error {
			return envd.Upload(context.Background(), "/home/user/input", strings.NewReader("x"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now()
			if err := test.call(); err == nil {
				t.Fatal("call succeeded")
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("call took %s", elapsed)
			}
		})
	}
}

func newTestEnvd(t *testing.T, server *httptest.Server, credential EnvdCredential) *Envd {
	t.Helper()
	envd, err := NewEnvd(Sandbox{ID: "sbx-test"}, credential, EnvdOptions{
		HTTPClient:       server.Client(),
		RequestTimeout:   time.Second,
		EndpointResolver: func(string, int) string { return server.URL },
	})
	if err != nil {
		t.Fatalf("NewEnvd: %v", err)
	}
	return envd
}

func assertEnvdRequest(t *testing.T, request *http.Request, path, contentType string) {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.EscapedPath() != path {
		t.Errorf("request = %s %s", request.Method, request.URL.EscapedPath())
	}
	if got := request.Header.Get("X-Access-Token"); got != testEnvdToken {
		t.Errorf("X-Access-Token = %q", got)
	}
	if got := request.Header.Get("Content-Type"); got != contentType {
		t.Errorf("Content-Type = %q, want %q", got, contentType)
	}
}

func writeConnectTestFrame(t *testing.T, writer io.Writer, flag byte, body string) {
	t.Helper()
	if _, err := writer.Write(connectFrame(flag, []byte(body))); err != nil {
		t.Errorf("write Connect frame: %v", err)
	}
}
