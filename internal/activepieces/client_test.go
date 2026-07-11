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

func TestClientDeletesFlow(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.EscapedPath() != "/api/v1/flows/flow%2F1" {
			t.Errorf("path = %s, want /api/v1/flows/flow%%2F1", r.URL.EscapedPath())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if requests == 2 {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := client.DeleteFlow(context.Background(), "test-token", "flow/1"); err != nil {
			t.Fatalf("DeleteFlow call %d: %v", i+1, err)
		}
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

func TestClientFlowOperationsAndGet(t *testing.T) {
	type call struct {
		method string
		path   string
		body   map[string]any
	}
	var calls []call
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		calls = append(calls, call{r.Method, r.URL.EscapedPath(), body})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"flow/1","displayName":"gitmoot: mail","status":"ENABLED","version":{"metadata":{"gitmoot":{"binding_id":"bind-1"}}}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.PublishFlow(ctx, "token", "flow/1"); err != nil {
		t.Fatal(err)
	}
	if err := client.SetFlowStatus(ctx, "token", "flow/1", false); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateFlowMetadata(ctx, "token", "flow/1", map[string]any{"gitmoot": map[string]any{"binding_id": "bind-1"}}); err != nil {
		t.Fatal(err)
	}
	flow, err := client.GetFlow(ctx, "token", "flow/1")
	if err != nil {
		t.Fatal(err)
	}
	if flow.ID != "flow/1" || flow.DisplayName != "gitmoot: mail" || flow.Status != "ENABLED" || flow.Metadata["gitmoot"] == nil {
		t.Fatalf("flow = %+v", flow)
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %+v", calls)
	}
	wants := []struct{ method, op string }{{http.MethodPost, "LOCK_AND_PUBLISH"}, {http.MethodPost, "CHANGE_STATUS"}, {http.MethodPost, "UPDATE_METADATA"}, {http.MethodGet, ""}}
	for i, want := range wants {
		if calls[i].method != want.method || calls[i].path != "/api/v1/flows/flow%2F1" {
			t.Fatalf("call %d = %+v", i, calls[i])
		}
		if want.op != "" && calls[i].body["type"] != want.op {
			t.Fatalf("call %d type = %v, want %s", i, calls[i].body["type"], want.op)
		}
	}
	if request := calls[0].body["request"].(map[string]any); len(request) != 0 {
		t.Fatalf("publish request = %+v, want empty object", request)
	}
	if got := calls[1].body["request"].(map[string]any)["status"]; got != "DISABLED" {
		t.Fatalf("status = %v", got)
	}
	metadata := calls[2].body["request"].(map[string]any)["metadata"].(map[string]any)
	gitmoot := metadata["gitmoot"].(map[string]any)
	if gitmoot["binding_id"] != "bind-1" {
		t.Fatalf("metadata request = %+v", metadata)
	}
}

func TestUpsertPieceConnectionAndResolveVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["externalId"] != "gmail-imap" || body["pieceName"] != "@activepieces/piece-imap" {
				t.Errorf("connection body = %+v", body)
			}
			props := body["value"].(map[string]any)["props"].(map[string]any)
			if props["username"] != "user@example.com" {
				t.Errorf("props = %+v", props)
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			if r.URL.Path != "/api/v1/pieces/@activepieces/piece-imap" {
				t.Errorf("piece path = %s", r.URL.Path)
			}
			if r.URL.Query().Get("projectId") != "project-1" || r.URL.Query().Get("version") != "~0.4.4" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"version":"0.4.3"}`))
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, server.Client())
	if err := client.UpsertPieceConnection(context.Background(), "token", "project-1", "gmail-imap", "Gmail IMAP", "@activepieces/piece-imap", map[string]any{"username": "user@example.com"}, false); err != nil {
		t.Fatal(err)
	}
	version, err := client.ResolvePieceVersion(context.Background(), "token", "project-1", "@activepieces/piece-imap", "~0.4.4")
	if err != nil || version != "0.4.3" {
		t.Fatalf("ResolvePieceVersion = %q, %v", version, err)
	}
}

// Activepieces 0.82 rejects sign-up on an already-provisioned platform with
// 403 INVITATION_ONLY_SIGN_UP; SignUpOrIn must treat that as "account exists"
// and fall back to sign-in, or every post-setup session (connect, bind) fails.
func TestSignUpOrInFallsBackOnInvitationOnly(t *testing.T) {
	var signIns int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/authentication/sign-up":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"INVITATION_ONLY_SIGN_UP","params":{"message":"User is not invited to the platform"}}`))
		case "/api/v1/authentication/sign-in":
			signIns++
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "projectId": "proj"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	token, projectID, created, err := client.SignUpOrIn(context.Background(), "admin@gitmoot.local", "pw")
	if err != nil {
		t.Fatalf("SignUpOrIn: %v", err)
	}
	if token != "tok" || projectID != "proj" || created {
		t.Fatalf("got token=%q project=%q created=%v", token, projectID, created)
	}
	if signIns != 1 {
		t.Fatalf("sign-in calls = %d, want 1", signIns)
	}
}
