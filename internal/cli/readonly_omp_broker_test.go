package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

func TestReadOnlyOmpBrokerProxyFiltersProviderAndProtectsUpstreamToken(t *testing.T) {
	const upstreamToken = "upstream-vault-token"
	var selectedRefreshes atomic.Int32
	var foreignRefreshes atomic.Int32
	var usageReports atomic.Int32
	var credentialWrites atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+upstreamToken {
			writeOmpBrokerJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad upstream token"})
			return
		}
		switch request.URL.Path {
		case "/v1/snapshot":
			writeOmpBrokerJSON(w, http.StatusOK, map[string]any{
				"generation": 4, "generatedAt": 1, "serverNowMs": 1,
				"refresher": map[string]any{"enabled": true, "intervalMs": 1, "skewMs": 1, "nextSweepInMs": 1},
				"credentials": []any{
					map[string]any{"id": 11, "provider": "kimi-code", "credential": map[string]any{"type": "api_key", "key": "selected-access"}, "identityKey": "kimi"},
					map[string]any{"id": 22, "provider": "anthropic", "credential": map[string]any{"type": "oauth", "access": "foreign-access"}, "identityKey": "claude"},
				},
			})
		case "/v1/usage":
			writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"generatedAt": 1, "reports": []any{
				map[string]any{"provider": "kimi-code", "limits": []any{}},
				map[string]any{"provider": "anthropic", "limits": []any{}, "metadata": map[string]any{"account": "foreign"}},
			}})
		case "/v1/credential/11/refresh":
			selectedRefreshes.Add(1)
			writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"entry": map[string]any{"id": 11, "provider": "kimi-code", "credential": map[string]any{"type": "oauth", "access": "selected-refreshed"}}})
		case "/v1/credential/22/refresh":
			foreignRefreshes.Add(1)
			writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"entry": map[string]any{"id": 22, "provider": "anthropic", "credential": map[string]any{"type": "oauth", "access": "foreign-refreshed"}}})
		case "/v1/credential/11/disable":
			credentialWrites.Add(1)
			writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"ok": true})
		case "/v1/usage/observed":
			usageReports.Add(1)
			writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"ok": true})
		default:
			writeOmpBrokerJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		}
	}))
	defer upstream.Close()

	proxy, err := startReadOnlyOmpBrokerProxy(readOnlyOmpBrokerConfig{url: upstream.URL, token: upstreamToken}, "kimi-code/k3")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()
	proxyURL := "http://" + proxy.listener.Addr().String()
	proxyToken := proxy.downstreamKey
	if proxyToken == "" || proxyToken == upstreamToken {
		t.Fatalf("downstream token was empty or reused the upstream vault token")
	}

	request := func(method, path, token, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, proxyURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(data)
	}

	if status, _ := request(http.MethodGet, "/v1/snapshot", "wrong", ""); status != http.StatusUnauthorized {
		t.Fatalf("wrong downstream token status = %d, want 401", status)
	}
	status, snapshot := request(http.MethodGet, "/v1/snapshot", proxyToken, "")
	if status != http.StatusOK || !strings.Contains(snapshot, "selected-access") || strings.Contains(snapshot, "anthropic") || strings.Contains(snapshot, "foreign-access") {
		t.Fatalf("filtered snapshot status=%d body=%s", status, snapshot)
	}
	status, usage := request(http.MethodGet, "/v1/usage", proxyToken, "")
	if status != http.StatusOK || !strings.Contains(usage, "kimi-code") || strings.Contains(usage, "anthropic") || strings.Contains(usage, "foreign") {
		t.Fatalf("filtered usage status=%d body=%s", status, usage)
	}
	if status, _ := request(http.MethodGet, "/v1/snapshot/stream", proxyToken, ""); status != http.StatusNotFound {
		t.Fatalf("snapshot stream status = %d, want OMP fallback sentinel 404", status)
	}
	if status, _ := request(http.MethodPost, "/v1/credential/22/refresh", proxyToken, ""); status != http.StatusForbidden || foreignRefreshes.Load() != 0 {
		t.Fatalf("foreign refresh status=%d upstreamCalls=%d", status, foreignRefreshes.Load())
	}
	status, refreshed := request(http.MethodPost, "/v1/credential/11/refresh", proxyToken, "")
	if status != http.StatusOK || selectedRefreshes.Load() != 1 || !strings.Contains(refreshed, "selected-refreshed") {
		t.Fatalf("selected refresh status=%d calls=%d body=%s", status, selectedRefreshes.Load(), refreshed)
	}
	foreignUsage := `{"installId":"seat","entries":[{"provider":"anthropic","model":"claude"}]}`
	if status, _ := request(http.MethodPost, "/v1/usage/observed", proxyToken, foreignUsage); status != http.StatusForbidden || usageReports.Load() != 0 {
		t.Fatalf("foreign usage status=%d upstreamCalls=%d", status, usageReports.Load())
	}
	selectedUsage := `{"installId":"seat","entries":[{"provider":"kimi-code","model":"k3"}]}`
	if status, _ := request(http.MethodPost, "/v1/usage/observed", proxyToken, selectedUsage); status != http.StatusOK || usageReports.Load() != 0 {
		t.Fatalf("selected usage status=%d upstreamCalls=%d, want local acknowledgement", status, usageReports.Load())
	}
	if status, _ := request(http.MethodPost, "/v1/credential/11/disable", proxyToken, `{"cause":"seat"}`); status != http.StatusOK || credentialWrites.Load() != 0 {
		t.Fatalf("selected disable status=%d upstreamCalls=%d, want local acknowledgement", status, credentialWrites.Load())
	}
}

func TestReadOnlyOmpBrokerPayloadFilteringFailsClosed(t *testing.T) {
	if _, err := filterOmpBrokerPayload([]byte(`{"generation":1}`), "kimi-code", "credentials"); err == nil {
		t.Fatal("snapshot without credentials array was accepted")
	}
	if _, err := filterOmpBrokerPayload([]byte(`{"entry":{"id":22,"provider":"anthropic"}}`), "kimi-code", "entry"); err == nil {
		t.Fatal("foreign refresh result was accepted")
	}
	filtered, err := filterOmpBrokerPayload([]byte(`{"credentials":[{"id":11,"provider":"kimi-code"},{"id":22,"provider":"anthropic"}]}`), "kimi-code", "credentials")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(filtered), "anthropic") || !strings.Contains(string(filtered), "kimi-code") {
		t.Fatalf("filtered payload escaped provider scope: %s", filtered)
	}
}

func TestReadOnlyOmpBrokerRequiresQualifiedAndStableEffectiveProvider(t *testing.T) {
	for _, model := range []string{"", "k3", "/k3", "kimi-code/"} {
		if _, err := runtime.OmpModelProvider(model); err == nil {
			t.Fatalf("model %q was accepted without a stable provider scope", model)
		}
	}
	if provider, err := runtime.OmpModelProvider("kimi-code/k3"); err != nil || provider != "kimi-code" {
		t.Fatalf("qualified model provider=%q err=%v", provider, err)
	}

	session := &readOnlyOmpBrokerSession{
		config:   readOnlyOmpBrokerConfig{url: "http://127.0.0.1:1", token: "upstream"},
		stateDir: t.TempDir(),
	}
	defer session.close()
	adapter := readOnlyRuntimeAdapter{Adapter: &cliWorkerFakeAdapter{}, ompSession: session}
	_, err := adapter.Deliver(
		context.Background(),
		runtime.Agent{Model: "kimi-code/k3"},
		runtime.Job{Model: "anthropic/claude-opus-5"},
	)
	if err != nil || session.broker == nil || session.broker.provider != "anthropic" {
		t.Fatalf("job override broker=%v err=%v, want anthropic scope", session.broker, err)
	}
	if _, err := adapter.Deliver(context.Background(), runtime.Agent{Model: "kimi-code/k3"}, runtime.Job{}); err == nil || !strings.Contains(err.Error(), "outside the broker scope") {
		t.Fatalf("cross-provider repair turn error = %v", err)
	}
	_, err = adapter.Deliver(
		context.Background(),
		runtime.Agent{Model: "anthropic/claude-opus-5"},
		runtime.Job{Plan: true, PlanInto: "kimi-code/k3"},
	)
	if err == nil || !strings.Contains(err.Error(), "plan execution provider") {
		t.Fatalf("cross-provider plan override error = %v", err)
	}
}

func TestReadOnlyOmpBrokerConfigRejectsUnsafeURL(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "broker-token")
	if err := os.WriteFile(tokenFile, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, brokerURL := range []string{
		"file:///tmp/broker",
		"http://user:secret@127.0.0.1:8765",
		"http://127.0.0.1:8765/path",
		"http://127.0.0.1:8765?token=leak",
		"http://example.com",
	} {
		_, err := readOnlySeatOmpBrokerConfig([]string{
			"OMP_AUTH_BROKER_URL=" + brokerURL,
			"OMP_AUTH_BROKER_TOKEN_FILE=" + tokenFile,
		})
		if err == nil {
			t.Fatalf("unsafe broker URL %q was accepted", brokerURL)
		}
	}

	config, err := readOnlySeatOmpBrokerConfig([]string{
		"OMP_AUTH_BROKER_URL=http://127.0.0.1:8765/",
		"OMP_AUTH_BROKER_TOKEN_FILE=" + tokenFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.url != "http://127.0.0.1:8765" || config.token != "token" {
		t.Fatalf("normalized broker config = url:%q token-loaded:%v", config.url, config.token != "")
	}
	if _, err := readOnlySeatOmpBrokerConfig([]string{
		"OMP_AUTH_BROKER_URL=http://127.0.0.1:8765",
		"OMP_AUTH_BROKER_TOKEN=token",
		"OMP_AUTH_BROKER_TOKEN_FILE=" + tokenFile,
	}); err == nil || !strings.Contains(err.Error(), "/proc") {
		t.Fatalf("ambient upstream broker token was not refused: %v", err)
	}
	if err := os.Chmod(tokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnlySeatOmpBrokerConfig([]string{
		"OMP_AUTH_BROKER_URL=http://127.0.0.1:8765",
		"OMP_AUTH_BROKER_TOKEN_FILE=" + tokenFile,
	}); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("insecure token file was accepted: %v", err)
	}
}
