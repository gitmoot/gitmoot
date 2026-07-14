package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/credgw"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// gatewayLogSink collects gateway/auth logs. The gateway logs from its request
// goroutines, so reads from the test goroutine must be synchronized.
type gatewayLogSink struct {
	mu    sync.Mutex
	lines strings.Builder
}

func (s *gatewayLogSink) Logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(&s.lines, format, args...)
}

func (s *gatewayLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines.String()
}

// waitFor blocks until the logs contain substr and returns everything logged so
// far, so assertions never race the goroutine that writes the entry.
func (s *gatewayLogSink) waitFor(t *testing.T, substr string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		logged := s.String()
		if strings.Contains(logged, substr) {
			return logged
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs never contained %q: %q", substr, logged)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRuntimeJobRunnerModelGatewayInjectsPlaceholderWithCurationOnAndOff(t *testing.T) {
	for _, curation := range []bool{false, true} {
		t.Run(fmt.Sprintf("curation-%t", curation), func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatal(err)
			}
			body := config.DefaultConfig(paths) + fmt.Sprintf(`
[credentials]
env_curation = %t
model_gateway = true
model_gateway_allow_hosts = ["api.anthropic.com"]
`, curation)
			if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), map[string]string{
				runtime.ClaudeOAuthTokenEnv: testOAuthToken,
			}); err != nil {
				t.Fatal(err)
			}
			t.Setenv(runtime.ClaudeOAuthTokenEnv, "ambient-oauth-must-not-reach-child")
			t.Setenv(runtime.AnthropicAPIKeyEnv, "ambient-api-key-must-not-reach-child")
			t.Setenv(runtime.AnthropicAuthTokenEnv, "ambient-auth-token-must-not-reach-child")

			registry := credgw.NewRegistry()
			previousRegistry := modelGatewayRegistry
			previousGatewayLogf := modelGatewayLogf
			previousAuthLogf := runtimeAuthLogf
			modelGatewayRegistry = registry
			modelGatewayLogf = func(string, ...any) {}
			runtimeAuthLogf = func(string, ...any) {}
			t.Cleanup(func() {
				closeModelGatewayHome(paths.Home)
				modelGatewayRegistry = previousRegistry
				modelGatewayLogf = previousGatewayLogf
				runtimeAuthLogf = previousAuthLogf
			})

			runner, _, source, err := runtimeJobRunnerWithAuth(home, runtime.ClaudeRuntime, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := runner.(*credgw.Runner); !ok {
				t.Fatalf("runner = %T", runner)
			}
			if source != runtimeAuthFileName+" via model gateway" {
				t.Fatalf("auth source = %q", source)
			}
			result, err := runner.Run(context.Background(), "", "sh", "-c", `printf '%s\n%s\n%s\n%s' "$CLAUDE_CODE_OAUTH_TOKEN" "$ANTHROPIC_API_KEY" "$ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_BASE_URL"`)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(result.Stdout, "\n")
			if len(lines) != 4 || !strings.HasPrefix(lines[0], "gitmoot-kc-runtime-call-") || lines[1] != "" || lines[2] != "" || !strings.HasPrefix(lines[3], "http://127.0.0.1:") {
				t.Fatalf("gateway child env = %q", result.Stdout)
			}
			for _, secret := range []string{testOAuthToken, "ambient-oauth-must-not-reach-child", "ambient-api-key-must-not-reach-child", "ambient-auth-token-must-not-reach-child"} {
				if strings.Contains(result.Stdout, secret) {
					t.Fatalf("child env contains credential %q", secret)
				}
			}
			request, _ := http.NewRequest(http.MethodPost, lines[3]+"/v1/messages", nil)
			request.Header.Set("Authorization", "Bearer "+lines[0])
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked placeholder status = %d", response.StatusCode)
			}
		})
	}
}

func TestRuntimeJobRunnerModelGatewayDisabledKeepsDirectAuthInjection(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	for _, name := range runtimeAuthEnvVars {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), map[string]string{
		runtime.ClaudeOAuthTokenEnv: testOAuthToken,
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := runtimeJobRunner(home, runtime.ClaudeRuntime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, enabled := runner.(*credgw.Runner); enabled {
		t.Fatalf("disabled model gateway returned %T", runner)
	}
	result, err := runner.Run(context.Background(), "", "sh", "-c", `printf '%s|%s' "$CLAUDE_CODE_OAUTH_TOKEN" "$ANTHROPIC_BASE_URL"`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != testOAuthToken+"|" {
		t.Fatalf("disabled gateway child env = %q", result.Stdout)
	}
}

func TestRuntimeJobRunnerModelGatewayRequiresAuthoritativeCredential(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	body := config.DefaultConfig(paths) + `
[credentials]
model_gateway = true
`
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range runtimeAuthEnvVars {
		t.Setenv(name, "")
	}
	_, err := runtimeJobRunner(home, runtime.ClaudeRuntime, nil)
	if err == nil || !strings.Contains(err.Error(), "requires a populated "+runtimeAuthFileName) {
		t.Fatalf("error = %v", err)
	}
}

func TestClaudeModelGatewayCredentialCustodyE2E(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}

	realToken := "sk-ant-oat01-gateway-real-token-abcdefghijklmnopqrstuvwxyz"
	seenAuthorization := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthorization <- r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, shaFingerprint(realToken))
	}))
	defer upstream.Close()
	parsedUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	configBody := config.DefaultConfig(paths) + fmt.Sprintf(`
[credentials]
model_gateway = true
model_gateway_allow_hosts = [%q]
`, parsedUpstream.Hostname())
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), map[string]string{
		runtime.ClaudeOAuthTokenEnv: realToken,
	}); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	shim := filepath.Join(binDir, "claude")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run '^TestClaudeModelGatewayShimProcess$' -- \"$@\"\n", testBinary)
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	envDump := filepath.Join(t.TempDir(), "claude-env.txt")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITMOOT_CLAUDE_GATEWAY_HELPER", "1")
	t.Setenv("GITMOOT_CLAUDE_GATEWAY_ENV_DUMP", envDump)
	t.Setenv(runtime.ClaudeOAuthTokenEnv, "ambient-credential-must-not-win")
	t.Setenv(runtime.AnthropicAPIKeyEnv, "ambient-api-key-must-not-win")

	previousRegistry := modelGatewayRegistry
	previousUpstream := modelGatewayUpstreamURL
	previousLogf := modelGatewayLogf
	previousAuthLogf := runtimeAuthLogf
	modelGatewayRegistry = credgw.NewRegistry()
	modelGatewayUpstreamURL = upstream.URL
	var logs gatewayLogSink
	modelGatewayLogf = logs.Logf
	runtimeAuthLogf = logs.Logf
	t.Cleanup(func() {
		closeModelGatewayHome(paths.Home)
		modelGatewayRegistry = previousRegistry
		modelGatewayUpstreamURL = previousUpstream
		modelGatewayLogf = previousLogf
		runtimeAuthLogf = previousAuthLogf
	})

	adapter, err := buildRuntimeAdapter(home, runtime.Agent{Runtime: runtime.ClaudeRuntime}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{
		Name:       "gateway-e2e",
		Role:       "implementer",
		Runtime:    runtime.ClaudeRuntime,
		RuntimeRef: runtime.FreshRefForJob("gateway-e2e"),
		RepoScope:  "owner/repo",
	}
	result, err := adapter.Deliver(context.Background(), agent, runtime.Job{ID: "gateway-e2e-job", Prompt: "exercise model gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != shaFingerprint(realToken) {
		t.Fatalf("upstream result = %q", result.Summary)
	}
	select {
	case got := <-seenAuthorization:
		if got != "Bearer "+realToken {
			t.Fatalf("upstream Authorization = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive the gateway request")
	}
	dump, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	dumpText := string(dump)
	if !strings.Contains(dumpText, "CLAUDE_CODE_OAUTH_TOKEN=gitmoot-kc-gateway-e2e-job-") || !strings.Contains(dumpText, "ANTHROPIC_BASE_URL=http://127.0.0.1:") {
		t.Fatalf("child env dump = %q", dumpText)
	}
	// Wait for the gateway's own request log before asserting on log contents:
	// it is written by the request goroutine after the response is proxied back,
	// so reading too early would make these leak checks vacuous.
	logged := logs.waitFor(t, "job_id=gateway-e2e-job")
	for _, secret := range []string{realToken, "ambient-credential-must-not-win", "ambient-api-key-must-not-win"} {
		if strings.Contains(dumpText, secret) || strings.Contains(logged, secret) {
			t.Fatalf("credential leaked through child env/logs")
		}
	}
	placeholder := envDumpValue(dumpText, runtime.ClaudeOAuthTokenEnv)
	if placeholder == "" || strings.Contains(logged, placeholder) {
		t.Fatalf("placeholder missing or logged: logs=%q", logged)
	}
	request, _ := http.NewRequest(http.MethodPost, envDumpValue(dumpText, "ANTHROPIC_BASE_URL")+"/v1/messages", bytes.NewReader(nil))
	request.Header.Set("Authorization", "Bearer "+placeholder)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("settled job placeholder status = %d", response.StatusCode)
	}
}

func TestClaudeModelGatewayShimProcess(t *testing.T) {
	if os.Getenv("GITMOOT_CLAUDE_GATEWAY_HELPER") != "1" {
		return
	}
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	placeholder := os.Getenv(runtime.ClaudeOAuthTokenEnv)
	dump := fmt.Sprintf("%s=%s\n%s=%s\n%s=%s\nANTHROPIC_BASE_URL=%s\n",
		runtime.ClaudeOAuthTokenEnv, placeholder,
		runtime.AnthropicAPIKeyEnv, os.Getenv(runtime.AnthropicAPIKeyEnv),
		runtime.AnthropicAuthTokenEnv, os.Getenv(runtime.AnthropicAuthTokenEnv),
		baseURL,
	)
	if err := os.WriteFile(os.Getenv("GITMOOT_CLAUDE_GATEWAY_ENV_DUMP"), []byte(dump), 0o600); err != nil {
		os.Exit(2)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader("test"))
	if err != nil {
		os.Exit(2)
	}
	request.Header.Set("Authorization", "Bearer "+placeholder)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		os.Exit(2)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type":     "result",
		"is_error": false,
		"result":   string(body),
	})
	os.Exit(0)
}

func envDumpValue(dump, name string) string {
	for _, line := range strings.Split(dump, "\n") {
		if value, ok := strings.CutPrefix(line, name+"="); ok {
			return value
		}
	}
	return ""
}
