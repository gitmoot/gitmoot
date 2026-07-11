package activepieces

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestClientWritesPieceConnectionAndFlowRequests(t *testing.T) {
	type recordedRequest struct {
		Method string
		Path   string
		Body   map[string]any
	}
	var mu sync.Mutex
	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		mu.Lock()
		requests = append(requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/flows" {
			_, _ = w.Write([]byte(`{"id":"flow-1"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.InstallPiece(ctx, "test-token", "@gitmoot/piece-gitmoot", "0.1.2"); err != nil {
		t.Fatalf("InstallPiece: %v", err)
	}
	if err := client.UpsertBridgeConnection(ctx, "test-token", "project-1", "gitmoot-bridge", "http://host.docker.internal:8791", "bridge-secret", false); err != nil {
		t.Fatalf("UpsertBridgeConnection: %v", err)
	}
	flowID, err := client.CreateFlow(ctx, "test-token", "project-1", "Flow Name")
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	if flowID != "flow-1" {
		t.Fatalf("flowID = %q, want flow-1", flowID)
	}
	flow := json.RawMessage(`{"displayName":"Flow Name","schemaVersion":"20","trigger":{}}`)
	if err := client.ImportFlow(ctx, "test-token", flowID, flow); err != nil {
		t.Fatalf("ImportFlow: %v", err)
	}

	wants := []struct {
		method string
		path   string
		keys   []string
	}{
		{http.MethodPost, "/api/v1/pieces", []string{"pieceName", "pieceVersion", "scope", "packageType"}},
		{http.MethodPost, "/api/v1/app-connections", []string{"externalId", "projectId", "pieceName", "type", "value"}},
		{http.MethodPost, "/api/v1/flows", []string{"displayName", "projectId"}},
		{http.MethodPost, "/api/v1/flows/flow-1", []string{"type", "request"}},
	}
	if len(requests) != len(wants) {
		t.Fatalf("request count = %d, want %d: %+v", len(requests), len(wants), requests)
	}
	for i, want := range wants {
		got := requests[i]
		if got.Method != want.method || got.Path != want.path {
			t.Fatalf("request %d = %s %s, want %s %s", i, got.Method, got.Path, want.method, want.path)
		}
		for _, key := range want.keys {
			if _, ok := got.Body[key]; !ok {
				t.Errorf("request %d body missing %q: %+v", i, key, got.Body)
			}
		}
	}
	value, _ := requests[1].Body["value"].(map[string]any)
	props, _ := value["props"].(map[string]any)
	if props["bridge_url"] != "http://host.docker.internal:8791" || props["bridge_token"] != "bridge-secret" {
		t.Fatalf("connection props = %+v", props)
	}
	if requests[3].Body["type"] != "IMPORT_FLOW" {
		t.Fatalf("import type = %v", requests[3].Body["type"])
	}
	if _, ok := requests[3].Body["request"].(map[string]any); !ok {
		t.Fatalf("import request is not an object: %#v", requests[3].Body["request"])
	}
}

func TestInstallPieceRejectsEmptyVersion(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.InstallPiece(context.Background(), "token", "@gitmoot/piece-gitmoot", ""); err == nil {
		t.Fatal("expected an error when the piece version is empty")
	}
	if called {
		t.Fatal("Activepieces should not be called with an empty piece version")
	}
}

func TestInstallPieceSendsVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body["pieceVersion"] != "0.1.2" {
			t.Errorf("pieceVersion = %v, want 0.1.2", body["pieceVersion"])
			http.Error(w, "wrong version", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.InstallPiece(context.Background(), "token", "@gitmoot/piece-gitmoot", "0.1.2"); err != nil {
		t.Fatal(err)
	}
}

func TestClientErrorsIncludeResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad piece payload", http.StatusBadRequest)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.InstallPiece(context.Background(), "token", "piece", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "bad piece payload") {
		t.Fatalf("error = %v, want response body", err)
	}
}
