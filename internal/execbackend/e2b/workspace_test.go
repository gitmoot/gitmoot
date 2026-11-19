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
	// A per-ID 404 stays inconclusive, the shared client's fail-closed rule:
	// the workspace ledger settles such a sandbox by its funded deadline.
	if _, _, state, err := client.Connect(context.Background(), "sbx-gone"); err == nil || state != Unknown {
		t.Fatalf("Connect gone = %v %v", state, err)
	}
	if _, _, state, err := client.Connect(context.Background(), "sbx-insecure"); err == nil || state != Unknown {
		t.Fatalf("Connect without a token = %v %v", state, err)
	}
}

func TestEnvdSendInputUsesConnectUnaryJSON(t *testing.T) {
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
}
