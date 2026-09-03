package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestKimiConfigIsNarrowedThroughTheStagingPath is the round-2 P1: kimi's
// config.toml was staged VERBATIM and REQUIRED into the seat's writable root
// while the same commit narrowed codex's. Measured on a live host, that file
// carries api_key under [services.*] and a key under [services.*.oauth].
func TestKimiConfigIsNarrowedThroughTheStagingPath(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".kimi-code")
	if err := os.MkdirAll(filepath.Join(source, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	const config = `default_model = "kimi-code/k3"

[services.moonshot_search]
base_url = "https://search.test"
api_key = "sk-SEARCH-SECRET"

[services.moonshot_search.oauth]
storage = "keyring"
key = "sk-OAUTH-SECRET"

[providers."managed:kimi-code"]
type = "kimi"
api_key = "sk-SELECTED-PROVIDER"

[providers."third-party-gateway"]
type = "openai"
base_url = "https://gateway.test/v1"
api_key = "sk-OTHER-PROVIDER-SECRET"
[providers."third-party-gateway".env]
GATEWAY_TOKEN = "sk-OTHER-ENV-SECRET"
`
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "credentials", "kimi-code.json"), []byte(`{"access_token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := runtime.Agent{Runtime: runtime.KimiRuntime, RuntimeConfigDir: source}
	stateDir, _, dropped, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(stateDir, "config.toml"))
	if err != nil {
		t.Fatalf("read staged config: %v", err)
	}
	out := string(staged)

	for _, secret := range []string{"sk-SEARCH-SECRET", "sk-OAUTH-SECRET", "sk-OTHER-PROVIDER-SECRET", "sk-OTHER-ENV-SECRET", "services"} {
		if strings.Contains(out, secret) {
			t.Errorf("the seat staged %q, which it must never read:\n%s", secret, out)
		}
	}
	// The seat legitimately needs the ONE provider default_model resolves to:
	// kimi refuses to start when both api_key and the env sub-table are absent.
	for _, keep := range []string{`default_model = "kimi-code/k3"`, `[providers."managed:kimi-code"]`, "sk-SELECTED-PROVIDER", "https://gateway.test/v1"} {
		if !strings.Contains(out, keep) {
			t.Errorf("narrowing dropped %q, which the seat needs:\n%s", keep, out)
		}
	}
	if len(dropped) == 0 {
		t.Error("narrowing withheld secrets but reported nothing, so a broken seat cannot say why")
	}
}

// TestKimiSelectedProviderWithholdsWhenItCannotProveWhichIsLive pins the
// fail-closed direction of the resolution rule.
func TestKimiSelectedProviderWithholdsWhenItCannotProveWhichIsLive(t *testing.T) {
	cases := map[string]struct{ config, want string }{
		"prefix match on the provider suffix": {"default_model = \"kimi-code/k3\"\n[providers.\"managed:kimi-code\"]\n", "managed:kimi-code"},
		"exact provider name":                 {"default_model = \"mine\"\n[providers.mine]\n", "mine"},
		"no default_model":                    {"[providers.mine]\n", ""},
		"no provider matches":                 {"default_model = \"absent/k3\"\n[providers.mine]\n", ""},
		"ambiguous match":                     {"default_model = \"dup/k3\"\n[providers.\"a:dup\"]\n[providers.\"b:dup\"]\n", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := kimiSelectedProvider([]byte(tc.config)); got != tc.want {
				t.Fatalf("kimiSelectedProvider = %q, want %q", got, tc.want)
			}
		})
	}

	// With no provider resolved, EVERY provider credential is withheld.
	result, err := narrowKimiConfigDetailed([]byte("default_model = \"absent/k3\"\n[providers.mine]\napi_key = \"sk-UNPROVEN\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.data), "sk-UNPROVEN") {
		t.Errorf("an unattributable provider credential was staged anyway:\n%s", result.data)
	}
	if len(result.dropped) == 0 {
		t.Error("withholding every provider credential must be reported, or the startup failure is unexplainable")
	}
}

// TestCodexKeepsTheSelectedProvidersKey is the round-2 P2 succeed-path
// regression: keeping model_providers structure while stripping api_key left
// the seat with no way to authenticate, because env_key survives narrowing but
// readOnlyRuntimeBaseEnv's allowlist never passes that variable into the seat.
func TestCodexKeepsTheSelectedProvidersKey(t *testing.T) {
	const config = `model = "custom"
model_provider = "mine"

[model_providers.mine]
base_url = "https://mine.test/v1"
wire_api = "chat"
env_key = "MY_API_KEY"
api_key = "sk-SELECTED"

[model_providers.other]
base_url = "https://other.test/v1"
api_key = "sk-UNUSED"
http_headers = { Authorization = "Bearer OTHER" }
`
	result, err := narrowCodexConfigDetailed([]byte(config))
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	out := string(result.data)
	if !strings.Contains(out, "sk-SELECTED") {
		t.Errorf("the SELECTED provider's key was stripped, so the seat cannot authenticate at all:\n%s", out)
	}
	for _, secret := range []string{"sk-UNUSED", "Bearer OTHER"} {
		if strings.Contains(out, secret) {
			t.Errorf("an unselected provider's credential %q reached the seat:\n%s", secret, out)
		}
	}
	// Structure the model needs must survive for both providers.
	for _, keep := range []string{"https://mine.test/v1", "https://other.test/v1", "wire_api"} {
		if !strings.Contains(out, keep) {
			t.Errorf("narrowing dropped %q:\n%s", keep, out)
		}
	}

	// With no model_provider selected, codex uses its built-in provider and
	// auth.json, so no inline provider key is needed.
	unselected, err := narrowCodexConfigDetailed([]byte("model = \"gpt-5\"\n[model_providers.mine]\napi_key = \"sk-NOBODY-SELECTED\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unselected.data), "sk-NOBODY-SELECTED") {
		t.Errorf("an unselected provider's key was staged with no model_provider set:\n%s", unselected.data)
	}
}

// TestNarrowingHandlesMultilineStrings is the round-2 P2 that failed in BOTH
// directions: a line inside """ or ”' beginning with "[" parsed as a section
// header, which re-attributed a following api_key (LEAK) and deleted lines out
// of a valid file leaving an unterminated string (CORRUPTION).
func TestNarrowingHandlesMultilineStrings(t *testing.T) {
	t.Run("leak: fake header inside a multi-line string", func(t *testing.T) {
		const config = `[model_providers.p]
base_url = "u"
notes = """
[example section]
"""
api_key = "SEKRIT"
`
		result, err := narrowCodexConfigDetailed([]byte(config))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		if strings.Contains(string(result.data), "SEKRIT") {
			t.Errorf("a fake header inside a multi-line string re-attributed api_key:\n%s", result.data)
		}
	})

	t.Run("leak: mcp env table after a fake header", func(t *testing.T) {
		const config = `[mcp_servers.gh]
notes = """
[model_providers.p]
"""
env = { TOKEN = "SEKRIT" }
`
		result, err := narrowCodexConfigDetailed([]byte(config))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		if strings.Contains(string(result.data), "SEKRIT") {
			t.Errorf("an mcp_servers env table survived behind a fake header:\n%s", result.data)
		}
	})

	t.Run("corruption: a valid file keeps its keys and closes its strings", func(t *testing.T) {
		const config = `model = "m"
notes = """
[mcp_servers.x]
"""
approval_policy = "never"
`
		result, err := narrowCodexConfigDetailed([]byte(config))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		out := string(result.data)
		if !strings.Contains(out, `approval_policy = "never"`) {
			t.Errorf("a key after a multi-line string was silently deleted:\n%s", out)
		}
		if count := strings.Count(out, `"""`); count%2 != 0 {
			t.Errorf("narrowing left an unterminated multi-line string, so the runtime reports a TOML error about a file gitmoot wrote:\n%s", out)
		}
		if out != strings.TrimPrefix(config, "") && strings.Contains(out, "mcp_servers.x") {
			t.Errorf("the section name inside the string was treated as a real section:\n%s", out)
		}
	})

	t.Run("literal delimiter and single-line form", func(t *testing.T) {
		result, err := narrowCodexConfigDetailed([]byte("model = \"m\"\nnotes = '''\n[mcp_servers.x]\n'''\napproval_policy = \"never\"\ninline = \"\"\"one line\"\"\"\n"))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		for _, keep := range []string{"approval_policy", "inline"} {
			if !strings.Contains(string(result.data), keep) {
				t.Errorf("narrowing dropped %q around a literal multi-line string:\n%s", keep, result.data)
			}
		}
	})

	t.Run("an unclosed string fails closed rather than corrupting", func(t *testing.T) {
		if _, err := narrowCodexConfigDetailed([]byte("model = \"m\"\nnotes = \"\"\"\nstill open\n")); err == nil {
			t.Fatal("an unterminated multi-line string was staged")
		}
	})
}

// TestNarrowingHandlesBOMAndEscapedQuotes covers the two round-2 P3 parser
// gaps: a UTF-8 BOM defeated header attribution for the WHOLE file, and a
// backslash-escaped quote in a table key failed the seat closed on valid TOML.
func TestNarrowingHandlesBOMAndEscapedQuotes(t *testing.T) {
	t.Run("BOM does not defeat attribution", func(t *testing.T) {
		for _, config := range []string{
			"\ufeff[mcp_servers.gh.env]\nTOKEN = \"SEKRIT\"\n",
			"\ufeff[model_providers.p]\napi_key = \"SEKRIT\"\n",
		} {
			result, err := narrowCodexConfigDetailed([]byte(config))
			if err != nil {
				t.Fatalf("narrow: %v", err)
			}
			if strings.Contains(string(result.data), "SEKRIT") {
				t.Errorf("a leading BOM hid the section header and leaked the secret:\n%s", result.data)
			}
		}
	})

	t.Run("escaped quote in a table key is valid TOML and must not fail closed", func(t *testing.T) {
		path, ok := parseTOMLKeyPath(`"a\"b".c`)
		if !ok {
			t.Fatal(`parseTOMLKeyPath rejected valid TOML ["a\"b".c], failing a whole seat closed`)
		}
		if len(path) != 2 || path[0] != `a"b` || path[1] != "c" {
			t.Fatalf("parsed path = %q, want [a\"b c]", path)
		}
		if _, err := narrowCodexConfigDetailed([]byte("[mcp_servers.\"a\\\"b\".env]\nTOKEN = \"SEKRIT\"\n")); err != nil {
			t.Fatalf("a file TOML accepts was refused: %v", err)
		}
	})

	t.Run("an unreadable header still fails closed", func(t *testing.T) {
		if _, err := narrowCodexConfigDetailed([]byte("[mcp_servers.gh\nTOKEN = \"SEKRIT\"\n")); err == nil {
			t.Fatal("a header that never closes was accepted")
		}
	})
}
