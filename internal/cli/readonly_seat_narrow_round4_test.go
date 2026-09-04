package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestTrailingCommentsCannotDefeatClassification is the round-4 P1 and P2 in one
// place, because they were one defect: nothing stripped comments, so a ']' in a
// header's trailing comment was swallowed into the section NAME (secrets staged
// verbatim, dropped empty) and a comment on model_provider / default_model was
// swallowed into the selected provider name (the seat's own credential
// withheld, and for kimi a hard fail of a required file).
func TestTrailingCommentsCannotDefeatClassification(t *testing.T) {
	t.Run("header comment containing a bracket still drops the section", func(t *testing.T) {
		cases := map[string]string{
			"kimi services":        "default_model = \"kimi-code/k3\"\n[services] # see [docs]\napi_key = \"sk-SECRET-DIRECT\"\n",
			"kimi quoted services": "default_model = \"kimi-code/k3\"\n[\"services\"] # see [docs]\napi_key = \"sk-SECRET-QUOTED\"\n",
			"kimi dotted":          "default_model = \"kimi-code/k3\"\n[services.search] # see [docs]\napi_key = \"sk-SECRET-DOTTED\"\n",
		}
		for name, config := range cases {
			t.Run(name, func(t *testing.T) {
				result, err := narrowKimiConfigDetailed([]byte(config))
				if err != nil {
					t.Fatalf("narrow: %v", err)
				}
				if strings.Contains(string(result.data), "sk-SECRET") {
					t.Errorf("a trailing comment defeated classification and staged the secret:\n%s", result.data)
				}
				if len(result.dropped) == 0 {
					t.Error("nothing reported withheld, so the narrowing event would be silent about a leak")
				}
			})
		}

		codex, err := narrowCodexConfigDetailed([]byte("[mcp_servers] # see [docs]\nenv = { TOKEN = \"sk-SECRET-MCP\" }\n"))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		if strings.Contains(string(codex.data), "sk-SECRET-MCP") {
			t.Errorf("a commented mcp_servers header staged its token:\n%s", codex.data)
		}
	})

	t.Run("selector comment does not withhold the selected credential", func(t *testing.T) {
		for name, config := range map[string]string{
			"space before hash": "default_model = \"kimi-code/k3\" # pinned\n[providers.\"managed:kimi-code\"]\napi_key = \"sk-KEEPME\"\n",
			"no space":          "default_model = \"kimi-code/k3\"# pinned\n[providers.\"managed:kimi-code\"]\napi_key = \"sk-KEEPME\"\n",
			"tab before hash":   "default_model = \"kimi-code/k3\"\t# pinned\n[providers.\"managed:kimi-code\"]\napi_key = \"sk-KEEPME\"\n",
		} {
			t.Run(name, func(t *testing.T) {
				result, err := narrowKimiConfigDetailed([]byte(config))
				if err != nil {
					t.Fatalf("narrow: %v", err)
				}
				if !strings.Contains(string(result.data), "sk-KEEPME") {
					t.Errorf("a trailing comment withheld the credential the seat requires:\n%s", result.data)
				}
			})
		}

		codex, err := narrowCodexConfigDetailed([]byte("model_provider = \"mine\" # chosen by ops\n[model_providers.mine]\napi_key = \"sk-KEEPME\"\n[model_providers.other]\napi_key = \"sk-DROPME\"\n"))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		if !strings.Contains(string(codex.data), "sk-KEEPME") {
			t.Errorf("a commented model_provider withheld the selected credential:\n%s", codex.data)
		}
		if strings.Contains(string(codex.data), "sk-DROPME") {
			t.Errorf("an unselected provider's credential survived:\n%s", codex.data)
		}
	})

	// The comment text itself must survive into the staged file: this narrows
	// credentials, it does not rewrite the operator's file.
	t.Run("comments are preserved in the staged output", func(t *testing.T) {
		result, err := narrowCodexConfigDetailed([]byte("model = \"gpt-5\" # keep me\n"))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		if !strings.Contains(string(result.data), "# keep me") {
			t.Errorf("narrowing stripped the operator's comment:\n%s", result.data)
		}
	})
}

// TestSelectorWitnessFromTheMultilineBlock is the round-4 P2/F3: a mutant that
// put tomlTopLevelScalar back on a naive raw scan SURVIVED the round-3 tests.
// This is the reviewer's witness input, asserted on the selector's OUTPUT
// rather than only on the staged bytes - the round-3 test could pass with the
// selector broken because both arms withheld nothing observable.
func TestSelectorWitnessFromTheMultilineBlock(t *testing.T) {
	const config = `instructions = """
[providers.example]
"""
default_model = "kimi-code/k3"

[providers."managed:kimi-code"]
api_key = "sk-SELECTED"
`
	lines, err := scanTOMLLines([]byte(config))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := tomlTopLevelScalar(lines, "default_model"); got != "kimi-code/k3" {
		t.Fatalf("tomlTopLevelScalar = %q, want kimi-code/k3: a section-looking line inside a multi-line string ended the top-level region", got)
	}
	if got := kimiSelectedProvider(lines); got != "managed:kimi-code" {
		t.Fatalf("kimiSelectedProvider = %q, want managed:kimi-code", got)
	}

	result, err := narrowKimiConfigDetailed([]byte(config))
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if !strings.Contains(string(result.data), "sk-SELECTED") {
		t.Errorf("the selected provider's credential was withheld:\n%s", result.data)
	}
	if len(result.dropped) != 0 {
		t.Errorf("nothing should have been withheld here, got %v", result.dropped)
	}
}

// TestQuotedKeyContainingEqualsIsNotDropped is the round-4 P3/F4: splitting on
// the first '=' with no lexical state dropped a VALID quoted key as an
// anonymous "unreadable key".
func TestQuotedKeyContainingEqualsIsNotDropped(t *testing.T) {
	const config = "model = \"gpt-5\"\n[mcp_servers.gh]\n\"a=b\" = \"value\"\n"
	result, err := narrowCodexConfigDetailed([]byte(config))
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	for _, label := range result.dropped {
		if strings.Contains(label, "unreadable key") {
			t.Errorf("a valid quoted key containing '=' was dropped as unreadable: %v", result.dropped)
		}
	}

	// And the same key under a KEPT section survives into the output.
	kept, err := narrowCodexConfigDetailed([]byte("[settings]\n\"a=b\" = \"value\"\n"))
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if !strings.Contains(string(kept.data), "\"a=b\"") {
		t.Errorf("a valid quoted key containing '=' was dropped from a kept section:\n%s", kept.data)
	}
}

// TestOptionalInputRefusalDoesNotKillTheSeat is the round-4 F8: optionalInputs
// was optional only for ABSENCE. Any narrowing refusal propagated and failed
// the whole job, so a codex config.toml the codex CLI itself accepts could kill
// a reviewer - while the policy's own comment reasons that a seat without the
// file falls back to the runtime default.
func TestOptionalInputRefusalDoesNotKillTheSeat(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	// Unbalanced '[' at EOF: one of the two refusals this work added.
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("model = \"gpt-5\"\nlist = [\n  \"a\",\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"OPENAI_API_KEY":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeConfigDir: source}
	stateDir, _, dropped, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err != nil {
		t.Fatalf("an unnarrowable OPTIONAL input killed the seat: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "config.toml")); !os.IsNotExist(statErr) {
		t.Errorf("a file that could not be narrowed was staged anyway: %v", statErr)
	}
	if len(dropped) == 0 {
		t.Fatal("the refusal was silent; an operator has no way to learn the config was not staged")
	}
	if !strings.Contains(strings.Join(dropped, " "), "config.toml") {
		t.Errorf("dropped = %v, want it to name config.toml", dropped)
	}

	// kimi's config.toml is REQUIRED, so there fail-closed is right and must
	// stay: the seat cannot run without it.
	kimi := filepath.Join(home, ".kimi-code")
	if err := os.MkdirAll(filepath.Join(kimi, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimi, "config.toml"), []byte("default_model = \"k/3\"\nlist = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimi, "credentials", "kimi-code.json"), []byte(`{"access_token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := prepareReadOnlyRuntimeState(runtime.Agent{Runtime: runtime.KimiRuntime, RuntimeConfigDir: kimi}, t.TempDir(), false); err == nil {
		t.Error("an unnarrowable REQUIRED input staged cleanly; kimi cannot run without it")
	}
}

// TestGatewayPolicyReportsWhatItEnforces is the round-4 F5: gateway mode was
// applied at the staging call site, so the policy readOnlySeatStatePolicyFor
// RETURNED still named codex's auth.json and kimi's token while the seat
// withheld them - a policy that disagrees with itself, and the next reader
// trusts the returned value.
func TestGatewayPolicyReportsWhatItEnforces(t *testing.T) {
	for _, runtimeName := range []string{runtime.ClaudeRuntime, runtime.CodexRuntime, runtime.KimiRuntime} {
		t.Run(runtimeName, func(t *testing.T) {
			gateway, needs, err := readOnlySeatStatePolicyFor(runtimeName, t.TempDir(), true)
			if err != nil || !needs {
				t.Fatalf("policy for %s: needs=%v err=%v", runtimeName, needs, err)
			}
			if gateway.credentialFile != "" {
				t.Errorf("gateway policy still names credentialFile %q, which the seat does not stage", gateway.credentialFile)
			}
			if gateway.credentialUsable != nil {
				t.Error("gateway policy still carries a usability check for a credential it does not stage")
			}

			// Non-gateway must be unchanged: the credential is needed there.
			direct, needs, err := readOnlySeatStatePolicyFor(runtimeName, t.TempDir(), false)
			if err != nil || !needs {
				t.Fatalf("non-gateway policy for %s: needs=%v err=%v", runtimeName, needs, err)
			}
			if direct.credentialFile == "" {
				t.Errorf("non-gateway policy for %s lost its credential file", runtimeName)
			}
		})
	}
}
