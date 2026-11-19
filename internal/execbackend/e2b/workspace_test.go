package e2b

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientConnectReattachesWithSandboxCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sandboxes/sbx-live":
			_, _ = w.Write([]byte(`{"sandboxID":"sbx-live","templateID":"t","envdAccessToken":"envd-token","domain":"example.test"}`))
		case "/sandboxes/sbx-insecure":
			_, _ = w.Write([]byte(`{"sandboxID":"sbx-insecure","templateID":"t"}`))
		default:
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second, server.Client())

	sandbox, credential, state, err := client.Connect(context.Background(), "sbx-live")
	if err != nil || state != Present || sandbox.ID != "sbx-live" || sandbox.Domain != "example.test" || credential.token != "envd-token" {
		t.Fatalf("Connect = %+v %v %v", sandbox, state, err)
	}
	if _, _, state, err := client.Connect(context.Background(), "sbx-gone"); err == nil || state != Gone {
		t.Fatalf("Connect gone = %v %v", state, err)
	}
	if _, _, state, err := client.Connect(context.Background(), "sbx-insecure"); err == nil || state != Unknown {
		t.Fatalf("Connect without a token = %v %v", state, err)
	}
}

func TestEnvdSendInputAndSignalUseConnectUnaryJSON(t *testing.T) {
	type call struct {
		path string
		body map[string]any
	}
	calls := make(chan call, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "token" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("connect-protocol-version") != "1" {
			t.Errorf("headers = %v", r.Header)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls <- call{path: r.URL.Path, body: body}
		if r.URL.Path == "/process.Process/SendSignal" {
			http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: "token"})
	if err := envd.SendInput(context.Background(), 42, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	got := <-calls
	process, _ := got.body["process"].(map[string]any)
	input, _ := got.body["input"].(map[string]any)
	if got.path != "/process.Process/SendInput" || process["pid"] != 42.0 || input["stdin"] != "aGk=" {
		t.Fatalf("SendInput = %+v", got)
	}
	err := envd.SendSignal(context.Background(), 42, SignalKill)
	got = <-calls
	if got.path != "/process.Process/SendSignal" || got.body["signal"] != "SIGNAL_SIGKILL" {
		t.Fatalf("SendSignal = %+v", got)
	}
	var status *HTTPStatusError
	if err == nil || !asHTTPStatus(err, &status) || status.StatusCode != http.StatusNotFound {
		t.Fatalf("SendSignal error = %v", err)
	}
}

func asHTTPStatus(err error, target **HTTPStatusError) bool {
	status, ok := err.(*HTTPStatusError)
	if ok {
		*target = status
	}
	return ok
}
