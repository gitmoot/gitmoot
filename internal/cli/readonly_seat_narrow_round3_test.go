package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// stageKimiConfig runs one kimi config through the REAL staging path and
// returns the staged bytes plus what narrowing reported withholding. The round-3
// P1 was invisible to helper-level tests: the leak only shows when the file
// reaches cacheRoot, which is the seat's one writable root.
func stageKimiConfig(t *testing.T, config string) (string, []string) {
	t.Helper()
	home := t.TempDir()
	source := filepath.Join(home, ".kimi-code")
	if err := os.MkdirAll(filepath.Join(source, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "credentials", "kimi-code.json"), []byte(`{"access_token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()
	agent := runtime.Agent{Runtime: runtime.KimiRuntime, RuntimeConfigDir: source}
	stateDir, _, dropped, err := prepareReadOnlyRuntimeState(agent, cacheRoot, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !strings.HasPrefix(stateDir, cacheRoot) {
		t.Fatalf("stateDir %q is not under the seat's writable root %q", stateDir, cacheRoot)
	}
	staged, err := os.ReadFile(filepath.Join(stateDir, "config.toml"))
	if err != nil {
		t.Fatalf("read staged config: %v", err)
	}
	return string(staged), dropped
}

// TestEscapedQuoteInAValueCannotStageTheRestOfTheFile is the round-3 P1.
//
// bracketBalance read an ESCAPED quote as a real terminator, so the "[" after
// it looked unclosed. That opened a continuation run which emitted every
// following line verbatim - and because the run had no end-of-file check, the
// whole tail of the file was staged unclassified with dropped empty.
func TestEscapedQuoteInAValueCannotStageTheRestOfTheFile(t *testing.T) {
	const leaking = `default_model = "kimi-code/k3"
instructions = "use \"[\" to open"

[services.moonshot_search]
api_key = "sk-SECRET-SERVICE"

[services.moonshot_search.oauth]
key = "sk-SECRET-OAUTH"

[providers."managed:kimi-code"]
type = "kimi"

[providers."third-party-gateway"]
api_key = "sk-SECRET-OTHER"
`
	staged, dropped := stageKimiConfig(t, leaking)
	for _, secret := range []string{"sk-SECRET-SERVICE", "sk-SECRET-OAUTH", "sk-SECRET-OTHER"} {
		if strings.Contains(staged, secret) {
			t.Errorf("an escaped quote opened a phantom run and staged %q into the seat:\n%s", secret, staged)
		}
	}
	if len(dropped) == 0 {
		t.Error("nothing was reported withheld, so the narrowing event would be silent about a leak")
	}
	// The value itself is valid TOML and must survive untouched.
	if !strings.Contains(staged, `instructions = "use \"[\" to open"`) {
		t.Errorf("the valid escaped-quote value was mangled:\n%s", staged)
	}

	// POSITIVE CONTROL: the byte-identical file without the escape must narrow
	// the same way. If this arm ever leaks, the assertions above prove nothing.
	control, controlDropped := stageKimiConfig(t, strings.Replace(leaking, `instructions = "use \"[\" to open"`, `instructions = "use hi"`, 1))
	for _, secret := range []string{"sk-SECRET-SERVICE", "sk-SECRET-OAUTH", "sk-SECRET-OTHER"} {
		if strings.Contains(control, secret) {
			t.Fatalf("INSTRUMENT FAILURE: the control arm leaks %q, so this test cannot detect the defect", secret)
		}
	}
	if len(controlDropped) == 0 {
		t.Fatal("INSTRUMENT FAILURE: the control arm reported nothing withheld")
	}
}

// TestUnbalancedBracketsFailClosedAtEOF pins the missing end-of-file check: an
// array or inline table that never closes leaves the tail of the file
// unclassified, and staging an unclassified tail is the leak itself.
func TestUnbalancedBracketsFailClosedAtEOF(t *testing.T) {
	for name, config := range map[string]string{
		"unclosed array":        "model = \"m\"\nlist = [\n  \"a\",\n",
		"unclosed inline table": "model = \"m\"\ntable = {\n  a = 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := narrowCodexConfigDetailed([]byte(config)); err == nil {
				t.Fatal("an unbalanced run reached the end of the file and was staged anyway")
			}
		})
	}
	// A balanced multi-line array is still fine.
	result, err := narrowCodexConfigDetailed([]byte("model = \"m\"\nlist = [\n  \"a\",\n]\napproval_policy = \"never\"\n"))
	if err != nil {
		t.Fatalf("a balanced multi-line array was refused: %v", err)
	}
	if !strings.Contains(string(result.data), "approval_policy") {
		t.Errorf("a balanced array swallowed the key after it:\n%s", result.data)
	}
}

// TestTripleDelimitersInCommentsAndStrings is the round-3 P2 that failed in
// both directions: strings.Count over the raw value treated a `"""` inside a
// COMMENT or inside a single-line string as opening a multi-line string.
func TestTripleDelimitersInCommentsAndStrings(t *testing.T) {
	t.Run("valid configs are no longer refused", func(t *testing.T) {
		for name, config := range map[string]string{
			"triple in a comment":        "model = \"m\"\nnote = 1 # see \"\"\" docs\napproval_policy = \"never\"\n",
			"triple in a literal string": "model = \"m\"\ntpl = 'wrap in \"\"\" please'\napproval_policy = \"never\"\n",
		} {
			t.Run(name, func(t *testing.T) {
				result, err := narrowCodexConfigDetailed([]byte(config))
				if err != nil {
					t.Fatalf("a valid config was refused, hard-failing the seat: %v", err)
				}
				if !strings.Contains(string(result.data), "approval_policy") {
					t.Errorf("the phantom string swallowed a later key:\n%s", result.data)
				}
			})
		}
	})

	t.Run("two stray delimiters no longer leak the span between them", func(t *testing.T) {
		const config = `model = "m"
note = 1 # see """ docs

[services.search]
api_key = "sk-SECRET-A"

other = 2 # and """ again

[services.other]
api_key = "sk-SECRET-B"
`
		result, err := narrowKimiConfigDetailed([]byte(config))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		for _, secret := range []string{"sk-SECRET-A", "sk-SECRET-B"} {
			if strings.Contains(string(result.data), secret) {
				t.Errorf("stray triple delimiters left %q unclassified and staged:\n%s", secret, result.data)
			}
		}
	})
}

// TestSelectorsShareTheScannersState is the round-3 P2 that reintroduced round
// 2's own defect in the two scanners round 2 did not touch: a header-looking
// line inside a multi-line string stopped top-level key reading, so
// default_model / model_provider were never found and every provider
// credential was withheld - breaking the succeed path.
func TestSelectorsShareTheScannersState(t *testing.T) {
	t.Run("kimi selection survives a documented example section", func(t *testing.T) {
		const config = `instructions = """
configure a provider like [providers.example]
"""
default_model = "kimi-code/k3"

[providers."managed:kimi-code"]
type = "kimi"
api_key = "sk-SELECTED"
`
		staged, _ := stageKimiConfig(t, config)
		if !strings.Contains(staged, "sk-SELECTED") {
			t.Errorf("the selected provider's key was withheld because selection lost its state, so the seat cannot authenticate:\n%s", staged)
		}
	})

	t.Run("codex selection survives the same shape", func(t *testing.T) {
		const config = `instructions = """
see [model_providers.example]
"""
model_provider = "mine"

[model_providers.mine]
base_url = "https://mine.test/v1"
api_key = "sk-SELECTED"

[model_providers.other]
api_key = "sk-OTHER"
`
		result, err := narrowCodexConfigDetailed([]byte(config))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		if !strings.Contains(string(result.data), "sk-SELECTED") {
			t.Errorf("selection lost its state and withheld the selected provider's key:\n%s", result.data)
		}
		if strings.Contains(string(result.data), "sk-OTHER") {
			t.Errorf("an unselected provider's key survived:\n%s", result.data)
		}
	})

	t.Run("a section name inside a string is not a real section", func(t *testing.T) {
		lines, err := scanTOMLLines([]byte("doc = \"\"\"\n[providers.phantom]\n\"\"\"\n[providers.real]\n"))
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		names := tomlSectionNames(lines, "providers")
		if len(names) != 1 || names[0] != "real" {
			t.Fatalf("section names = %q, want only [real]: a name inside a multi-line string is not a section", names)
		}
	})
}

// TestCodexEnvKeyOnlyProviderFailsWithANamedCause is the round-3 P3: a selected
// provider that authenticates only through env_key leaves the seat with no
// credential, because the sandbox environment allowlist does not pass an
// arbitrary variable through. Staging used to succeed silently.
func TestCodexEnvKeyOnlyProviderFailsWithANamedCause(t *testing.T) {
	_, err := narrowCodexConfigDetailed([]byte("model_provider = \"mine\"\n[model_providers.mine]\nbase_url = \"https://mine.test/v1\"\nenv_key = \"MY_API_KEY\"\n"))
	if err == nil {
		t.Fatal("an env_key-only selected provider staged cleanly; the seat would fail later and opaquely")
	}
	for _, want := range []string{"env_key", "mine"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}

	// Both working shapes must still stage: an inline key, and env_key
	// ALONGSIDE an inline key.
	for name, config := range map[string]string{
		"inline api_key":        "model_provider = \"mine\"\n[model_providers.mine]\napi_key = \"sk-OK\"\n",
		"env_key plus api_key":  "model_provider = \"mine\"\n[model_providers.mine]\nenv_key = \"MY_API_KEY\"\napi_key = \"sk-OK\"\n",
		"no provider selected":  "model = \"gpt-5\"\n[model_providers.mine]\nenv_key = \"MY_API_KEY\"\n",
		"selected but no block": "model_provider = \"absent\"\nmodel = \"gpt-5\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := narrowCodexConfigDetailed([]byte(config)); err != nil {
				t.Fatalf("a workable configuration was refused: %v", err)
			}
		})
	}
}

// TestNarrowingEventReachesTheJobLog is the round-3 P3: the
// read_only_seat_config_narrowed event had no test at all, and it is the ONLY
// diagnostic a seat has for a credential the narrower withheld. Driven through
// the real worker, not the helper.
func TestNarrowingEventReachesTheJobLog(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	hostKimi := filepath.Join(home, "host-kimi")
	if err := os.MkdirAll(filepath.Join(hostKimi, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostKimi, "config.toml"), []byte("default_model = \"kimi-code/k3\"\n\n[services.moonshot_search]\napi_key = \"sk-SECRET\"\n\n[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostKimi, "credentials", "kimi-code.json"), []byte(`{"access_token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")
	headSHA := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	if err := store.UpsertRepoForce(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout, PrimaryCheckoutPath: checkout}); err != nil {
		t.Fatalf("UpsertRepoForce: %v", err)
	}
	registered := db.Agent{Name: "seat-kimi", Role: "reviewer", Runtime: runtime.KimiRuntime, RuntimeRef: "019fa4c8-69c1-7bc2-8628-00ade8fa43e1", RepoScope: "owner/repo"}

	job := db.Job{
		ID:    "local-review-seat-event",
		Agent: "seat-kimi",
		Type:  "review",
		State: string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", Branch: "main", PullRequest: 3, HeadSHA: headSHA,
			ReadOnlySeat: true, RuntimeConfigDir: hostKimi,
		}),
	}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	var output bytes.Buffer
	worker := jobWorker{
		Store: store, ConfigHome: home, ConfigHomeExplicit: true, Stdout: &output,
		AgentLookup:    func(context.Context, string) (db.Agent, error) { return registered, nil },
		AdapterFactory: func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return &cliWorkerFakeAdapter{}, nil },
	}
	_ = worker.run(ctx, job)

	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var narrowed string
	for _, event := range events {
		if event.Kind == "read_only_seat_config_narrowed" {
			narrowed = event.Message
		}
	}
	if narrowed == "" {
		t.Fatalf("no read_only_seat_config_narrowed event: the seat's only diagnostic for a withheld credential is silent; events: %+v", events)
	}
	if !strings.Contains(narrowed, "services") {
		t.Errorf("event message %q does not name what was withheld", narrowed)
	}
}

// TestGatewayModeWithholdsEveryRuntimesCredential is the round-3 P3 asymmetry:
// gatewayMode cleared claude's credential only, so a gateway seat still staged
// codex's auth.json and kimi's token into its writable root.
func TestGatewayModeWithholdsEveryRuntimesCredential(t *testing.T) {
	home := t.TempDir()

	codexSource := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexSource, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-SECRET","auth_mode":"apikey"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexSource, "config.toml"), []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	kimiSource := filepath.Join(home, ".kimi-code")
	if err := os.MkdirAll(filepath.Join(kimiSource, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiSource, "config.toml"), []byte("default_model = \"kimi-code/k3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiSource, "credentials", "kimi-code.json"), []byte(`{"access_token":"sk-SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ runtimeName, source, credential string }{
		"codex": {runtime.CodexRuntime, codexSource, "auth.json"},
		"kimi":  {runtime.KimiRuntime, kimiSource, filepath.Join("credentials", "kimi-code.json")},
	} {
		t.Run(name, func(t *testing.T) {
			agent := runtime.Agent{Runtime: tc.runtimeName, RuntimeConfigDir: tc.source}
			stateDir, _, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), true)
			if err != nil {
				t.Fatalf("gateway staging: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(stateDir, tc.credential)); !os.IsNotExist(statErr) {
				t.Errorf("gateway mode staged %s's credential the gateway supplies: %v", name, statErr)
			}
			// Model settings are still needed and must still be staged.
			if _, statErr := os.Stat(filepath.Join(stateDir, "config.toml")); statErr != nil {
				t.Errorf("gateway mode dropped %s's model settings too: %v", name, statErr)
			}
		})
	}

	// Non-gateway staging must be unchanged.
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeConfigDir: codexSource}
	stateDir, _, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err != nil {
		t.Fatalf("non-gateway staging: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "auth.json")); statErr != nil {
		t.Errorf("a non-gateway codex seat lost the credential it needs: %v", statErr)
	}
}

// TestSelectionIgnoresSameNamedKeysInsideSections closes a gap a surviving
// mutant exposed: dropping the top-level requirement let a key named
// default_model or model_provider INSIDE a section be read as the seat's
// provider selection, which would keep a credential nothing selected.
func TestSelectionIgnoresSameNamedKeysInsideSections(t *testing.T) {
	t.Run("kimi", func(t *testing.T) {
		// No TOP-LEVEL default_model, so nothing is selected and every
		// provider credential must be withheld.
		const config = `[providers.mine]
default_model = "mine/k3"
api_key = "sk-NOT-SELECTED"
`
		result, err := narrowKimiConfigDetailed([]byte(config))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		if strings.Contains(string(result.data), "sk-NOT-SELECTED") {
			t.Errorf("a default_model inside a section was read as the selection, keeping a credential nothing selected:\n%s", result.data)
		}
		lines, err := scanTOMLLines([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		if got := kimiSelectedProvider(lines); got != "" {
			t.Errorf("kimiSelectedProvider = %q, want empty: the key is not top level", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		const config = `[model_providers.mine]
model_provider = "mine"
api_key = "sk-NOT-SELECTED"
`
		result, err := narrowCodexConfigDetailed([]byte(config))
		if err != nil {
			t.Fatalf("narrow: %v", err)
		}
		if strings.Contains(string(result.data), "sk-NOT-SELECTED") {
			t.Errorf("a model_provider inside a section was read as the selection:\n%s", result.data)
		}
	})

	// The top-level form still selects, so the guard is not simply refusing.
	lines, err := scanTOMLLines([]byte("default_model = \"mine/k3\"\n[providers.mine]\napi_key = \"sk-SELECTED\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := kimiSelectedProvider(lines); got != "mine" {
		t.Errorf("kimiSelectedProvider = %q, want mine for a top-level key", got)
	}
}
