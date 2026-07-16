package credgw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// envCapture is an EnvRunner that records the env it was handed and runs nothing.
type envCapture struct{ env []string }

func (c *envCapture) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{}, nil
}
func (c *envCapture) LookPath(string) (string, error) { return "", nil }
func (c *envCapture) RunEnv(_ context.Context, _ string, env []string, _ string, _ ...string) (subprocess.Result, error) {
	c.env = append([]string{}, env...)
	return subprocess.Result{}, nil
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- { // last wins, matching real env semantics
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix), true
		}
	}
	return "", false
}

func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close(context.Background()) })
	t.Cleanup(func() { t.Setenv("_", upstream.URL) }) // keep upstream referenced
	return gateway
}

// The runner must inject CLAUDE_CONFIG_DIR when it has one, so the child cannot
// fall back to its cached credential store (#936). Without it, the placeholder
// injection is defeated by ~/.claude/.credentials.json.
func TestRunnerInjectsChildConfigDir(t *testing.T) {
	gateway := newTestGateway(t)
	capture := &envCapture{}
	runner := &Runner{
		Inner:          capture,
		Gateway:        gateway,
		Credential:     Credential{Kind: CredentialBearer, Value: "real-token"},
		Policy:         Policy{Upstream: DefaultAnthropicUpstream, AllowedHosts: []string{"api.anthropic.com"}},
		ChildConfigDir: "/gitmoot/home/runtime/claude-gateway-config",
	}
	if _, err := runner.Run(context.Background(), "", "claude", "-p", "hi"); err != nil {
		t.Fatal(err)
	}

	dir, ok := envValue(capture.env, "CLAUDE_CONFIG_DIR")
	if !ok || dir != "/gitmoot/home/runtime/claude-gateway-config" {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q (present=%t), want the child config dir", dir, ok)
	}
	// The placeholder — never the real token — must be the injected credential.
	token, _ := envValue(capture.env, "CLAUDE_CODE_OAUTH_TOKEN")
	if token == "" || token == "real-token" || !strings.HasPrefix(token, "gitmoot-kc-") {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want a placeholder", token)
	}
	if base, ok := envValue(capture.env, "ANTHROPIC_BASE_URL"); !ok || !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", base)
	}
}

// An empty ChildConfigDir must leave CLAUDE_CONFIG_DIR untouched (pre-#936
// behavior), so the caller's own value — if any — is not overwritten.
func TestRunnerWithoutChildConfigDirLeavesItUntouched(t *testing.T) {
	gateway := newTestGateway(t)
	capture := &envCapture{}
	runner := &Runner{
		Inner:      capture,
		Gateway:    gateway,
		Credential: Credential{Kind: CredentialBearer, Value: "real-token"},
		Policy:     Policy{Upstream: DefaultAnthropicUpstream, AllowedHosts: []string{"api.anthropic.com"}},
	}
	if _, err := runner.RunEnv(context.Background(), "", []string{"CLAUDE_CONFIG_DIR=/caller/dir"}, "claude", "-p", "hi"); err != nil {
		t.Fatal(err)
	}
	if dir, _ := envValue(capture.env, "CLAUDE_CONFIG_DIR"); dir != "/caller/dir" {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want the caller's value preserved", dir)
	}
}

func TestRunnerGatewayAuthOverridesAgentEnv(t *testing.T) {
	gateway := newTestGateway(t)
	capture := &envCapture{}
	runner := &Runner{
		Inner:      capture,
		Gateway:    gateway,
		Credential: Credential{Kind: CredentialBearer, Value: "real-token"},
		Policy:     Policy{Upstream: DefaultAnthropicUpstream, AllowedHosts: []string{"api.anthropic.com"}},
	}
	if _, err := runner.RunEnv(context.Background(), "", []string{
		"CLAUDE_CODE_OAUTH_TOKEN=stage-supplied-value",
		"ANTHROPIC_BASE_URL=https://stage.invalid",
	}, "claude", "-p", "hi"); err != nil {
		t.Fatal(err)
	}
	token, _ := envValue(capture.env, "CLAUDE_CODE_OAUTH_TOKEN")
	if !strings.HasPrefix(token, "gitmoot-kc-") || token == "stage-supplied-value" {
		t.Fatalf("effective Claude token = %q, want model-gateway placeholder", token)
	}
	base, _ := envValue(capture.env, "ANTHROPIC_BASE_URL")
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("effective Anthropic base URL = %q, want model gateway", base)
	}
	if got := strings.Join(capture.env, "\n"); !strings.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN=stage-supplied-value") {
		t.Fatalf("incoming AgentEnv entry was not composed beneath gateway env: %q", got)
	}
}
