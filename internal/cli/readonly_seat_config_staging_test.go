package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestSeatStagesCodexConfigWithoutThirdPartySecrets pins the narrowing THROUGH
// THE STAGING PATH, not through narrowCodexConfig directly.
//
// The narrowing is only worth anything if prepareReadOnlyRuntimeState actually
// applies it: a policy that declares config.toml as an optional input and
// forgets its narrower would leave the helper perfectly correct and the seat
// still leaking. This test reads the file out of the seat's state dir.
func TestSeatStagesCodexConfigWithoutThirdPartySecrets(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, ".codex")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	const cfg = `model = "gpt-5"
approval_policy = "never"

[mcp_servers.github]
command = "npx"
[mcp_servers.github.env]
GITHUB_TOKEN = "ghp_SUPERSECRET_TOKEN"

[model_providers.mycorp]
base_url = "https://api.mycorp.test/v1"
wire_api = "chat"
api_key = "sk-LEAKED-PROVIDER-KEY"
http_headers = { Authorization = "Bearer LEAKED-HEADER" }
`
	if err := os.WriteFile(filepath.Join(src, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeConfigDir: src}
	stateDir, _, _, err := prepareReadOnlyRuntimeState(agent, cacheRoot, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(stateDir, "config.toml"))
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	for _, secret := range []string{"ghp_SUPERSECRET_TOKEN", "sk-LEAKED-PROVIDER-KEY", "LEAKED-HEADER"} {
		if strings.Contains(string(staged), secret) {
			t.Errorf("the seat staged a third-party secret: %q is readable inside the sandbox", secret)
		}
	}
	if !strings.Contains(string(staged), `model = "gpt-5"`) {
		t.Errorf("narrowing dropped the model setting, which is what the file is staged for:\n%s", staged)
	}
	if !strings.Contains(string(staged), "api.mycorp.test") {
		t.Errorf("narrowing dropped the provider base_url, so a custom model would not resolve:\n%s", staged)
	}
}
