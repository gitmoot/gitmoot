package cli

import (
	"strings"
	"testing"
)

func TestNarrowCodexConfigRemovesCredentialsAndKeepsSettings(t *testing.T) {
	const cfg = `model = "gpt-5"
approval_policy = "never"

[sandbox_workspace_write]
network_access = false

[mcp_servers.github]
command = "npx"
args = ["-y", "server", "--token=inline_secret"]
[mcp_servers.github.env]
GITHUB_TOKEN = "ghp_SECRET"

[model_providers.mycorp]
base_url = "https://api.mycorp.test/v1"
wire_api = "chat"
env_key = "MYCORP_API_KEY"
api_key = "sk-SECRET"
http_headers = { Authorization = "Bearer SECRET_HEADER" }
`
	got, err := narrowCodexConfig([]byte(cfg))
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	out := string(got)
	for _, secret := range []string{"ghp_SECRET", "sk-SECRET", "SECRET_HEADER", "inline_secret", "GITHUB_TOKEN", "mcp_servers"} {
		if strings.Contains(out, secret) {
			t.Errorf("narrowed config still carries %q:\n%s", secret, out)
		}
	}
	// The settings the file is staged for must survive, including the provider
	// definition that makes a custom model resolvable.
	for _, keep := range []string{`model = "gpt-5"`, "approval_policy", "sandbox_workspace_write", "network_access", "[model_providers.mycorp]", "api.mycorp.test", "wire_api", `env_key = "MYCORP_API_KEY"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("narrowed config dropped %q:\n%s", keep, out)
		}
	}
}

// TestNarrowCodexConfigClosesDottedAndQuotedBypasses pins the two shapes that
// route around a scanner keyed on section headers alone: a top-level dotted key
// that never opens a [mcp_servers] section, and a quoted server name whose dot
// is inside the quotes.
func TestNarrowCodexConfigClosesDottedAndQuotedBypasses(t *testing.T) {
	cases := map[string]string{
		"top-level dotted key":     "model = \"gpt-5\"\nmcp_servers.github.env.GITHUB_TOKEN = \"ghp_DOTTED\"\n",
		"quoted server name":       "model = \"gpt-5\"\n[mcp_servers.\"my server\".env]\nTOKEN = \"ghp_QUOTED\"\n",
		"dotted provider api_key":  "model = \"gpt-5\"\nmodel_providers.mycorp.api_key = \"sk-DOTTED\"\n",
		"multi-line inline table":  "model = \"gpt-5\"\n[mcp_servers.github]\nenv = {\n  GITHUB_TOKEN = \"ghp_MULTILINE\"\n}\n",
		"provider inline continue": "model = \"gpt-5\"\n[model_providers.p]\nhttp_headers = {\n  Authorization = \"Bearer SPANNED\"\n}\nbase_url = \"https://keep.test\"\n",
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := narrowCodexConfig([]byte(cfg))
			if err != nil {
				t.Fatalf("narrow: %v", err)
			}
			out := string(got)
			for _, secret := range []string{"ghp_DOTTED", "ghp_QUOTED", "sk-DOTTED", "ghp_MULTILINE", "SPANNED"} {
				if strings.Contains(out, secret) {
					t.Errorf("bypass: %q survived narrowing:\n%s", secret, out)
				}
			}
			if !strings.Contains(out, `model = "gpt-5"`) {
				t.Errorf("narrowing dropped the model setting:\n%s", out)
			}
		})
	}
	// A value dropped mid-table must not take the following key with it.
	got, err := narrowCodexConfig([]byte("[model_providers.p]\nhttp_headers = {\n  Authorization = \"Bearer SPANNED\"\n}\nbase_url = \"https://keep.test\"\n"))
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if !strings.Contains(string(got), "https://keep.test") {
		t.Errorf("a dropped inline table swallowed the key after it:\n%s", got)
	}
}

// TestNarrowCodexConfigFailsClosedOnUnreadableHeader pins the direction that
// matters: once a header cannot be attributed, staging must refuse rather than
// keep the rest of the file and hope it holds no credentials.
func TestNarrowCodexConfigFailsClosedOnUnreadableHeader(t *testing.T) {
	for name, cfg := range map[string]string{
		"header never closes": "[mcp_servers.github\nGITHUB_TOKEN = \"ghp_AFTER_MALFORMED\"\n",
		"empty header":        "[]\napi_key = \"sk-AFTER_EMPTY\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := narrowCodexConfig([]byte(cfg))
			if err == nil {
				t.Fatalf("expected a refusal, got a staged file:\n%s", got)
			}
			if !strings.Contains(err.Error(), "not readable") {
				t.Errorf("error does not say what is wrong with the file: %v", err)
			}
		})
	}
}
