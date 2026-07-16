package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func TestCuratedBaseAllowlistPinned(t *testing.T) {
	want := []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP", "TZ", "LANG", "LANGUAGE", "TERM", "COLORTERM", "NO_COLOR",
		"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "GOTOOLCHAIN", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GITMOOT_HOME",
	}
	if !reflect.DeepEqual(curatedBaseEnvNames, want) {
		t.Fatalf("base allowlist = %v, want %v", curatedBaseEnvNames, want)
	}
}

func envNames(entries []string) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestCuratedRuntimeBaseEnvPinnedPolicy(t *testing.T) {
	environ := []string{
		"PATH=/bin", "HOME=/home/test", "LC_ALL=C", "CLAUDE_CODE_OAUTH_TOKEN=claude",
		"ANTHROPIC_API_KEY=anthropic", "ANTHROPIC_AUTH_TOKEN=anthropic-auth", "CLAUDE_CONFIG_DIR=/claude", "CODEX_HOME=/codex",
		"GH_TOKEN=gh", "GITHUB_TOKEN=github", "GH_ENTERPRISE_TOKEN=gh-enterprise", "GITHUB_ENTERPRISE_TOKEN=github-enterprise", "GH_HOST=example", "SSH_AUTH_SOCK=/ssh",
		"GOCACHE=/go-cache", "NPM_TOKEN=npm", "UNLISTED=no",
	}
	tests := []struct {
		runtime string
		want    []string
	}{
		{runtime: runtime.CodexRuntime, want: []string{"CODEX_HOME", "GH_CONFIG_DIR", "GH_PROMPT_DISABLED", "GOCACHE", "HOME", "LC_ALL", "NPM_TOKEN", "PATH"}},
		{runtime: runtime.ClaudeRuntime, want: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "GH_CONFIG_DIR", "GH_PROMPT_DISABLED", "GOCACHE", "HOME", "LC_ALL", "NPM_TOKEN", "PATH"}},
		{runtime: runtime.KimiRuntime, want: []string{"GH_CONFIG_DIR", "GH_PROMPT_DISABLED", "GOCACHE", "HOME", "LC_ALL", "NPM_TOKEN", "PATH"}},
		{runtime: runtime.KimiCLIRuntime, want: []string{"GH_CONFIG_DIR", "GH_PROMPT_DISABLED", "GOCACHE", "HOME", "LC_ALL", "NPM_TOKEN", "PATH"}},
		{runtime: runtime.ShellRuntime, want: []string{"GH_CONFIG_DIR", "GH_PROMPT_DISABLED", "GOCACHE", "HOME", "LC_ALL", "NPM_TOKEN", "PATH"}},
	}
	cfg := config.CredentialsConfig{EnvCuration: true, EnvPassthrough: []string{"GOCACHE", "NPM_*"}, GitHub: config.CredentialsGitHubDeny}
	for _, test := range tests {
		t.Run(test.runtime, func(t *testing.T) {
			got := envNames(curatedRuntimeBaseEnv(cfg, test.runtime, environ, "/tmp/gh"))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("names = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCuratedRuntimeBaseEnvGitHubInherit(t *testing.T) {
	cfg := config.CredentialsConfig{EnvCuration: true, GitHub: config.CredentialsGitHubInherit}
	want := []string{"PATH=/bin", "GH_TOKEN=one", "GITHUB_TOKEN=two", "GH_HOST=three"}
	for _, runtimeName := range []string{runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime, runtime.KimiCLIRuntime, runtime.ShellRuntime} {
		t.Run(runtimeName, func(t *testing.T) {
			got := curatedRuntimeBaseEnv(cfg, runtimeName, []string{"PATH=/bin", "GH_TOKEN=one", "GITHUB_TOKEN=two", "GH_HOST=three"}, "")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("env = %v, want %v", got, want)
			}
		})
	}
}

func TestRuntimeJobRunnerOffIsIdenticalAndCreatesNoScratch(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	before, err := filepath.Glob(filepath.Join(os.TempDir(), "gitmoot-gh-config-*"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := runtimeJobRunner(home, runtime.ShellRuntime, nil)
	if err != nil {
		t.Fatalf("runtimeJobRunner: %v", err)
	}
	if runner != nil {
		t.Fatalf("runner = %T, want nil", runner)
	}
	adapter, err := buildRuntimeAdapter(home, runtime.Agent{Runtime: runtime.ShellRuntime}, "", nil)
	if err != nil {
		t.Fatalf("buildRuntimeAdapter: %v", err)
	}
	if got := adapter.(runtime.ShellAdapter).Runner; got != nil {
		t.Fatalf("off adapter runner = %T, want nil so cmd.Env remains nil", got)
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "gitmoot-gh-config-*"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("scratch dirs changed with curation off: before=%v after=%v", before, after)
	}
}

func TestRuntimeJobRunnerComposesWrappersAboveCuratedBase(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	content := config.DefaultConfig(paths) + "\n[credentials]\nenv_curation = true\ngithub = \"deny\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	tests := []struct {
		name  string
		outer subprocess.Runner
		check func(t *testing.T, got subprocess.Runner)
	}{
		{name: "tee", outer: subprocess.TeeRunner{}, check: func(t *testing.T, got subprocess.Runner) {
			tee := got.(subprocess.TeeRunner)
			if _, ok := tee.Inner.(subprocess.CuratedGroupRunner); !ok {
				t.Fatalf("tee inner = %T", tee.Inner)
			}
		}},
		{name: "env", outer: subprocess.EnvInjectingRunner{Env: []string{"RELAY=yes"}}, check: func(t *testing.T, got subprocess.Runner) {
			env := got.(subprocess.EnvInjectingRunner)
			if _, ok := env.Inner.(subprocess.CuratedGroupRunner); !ok {
				t.Fatalf("env inner = %T", env.Inner)
			}
		}},
		{name: "wrapping", outer: subprocess.WrappingRunner{}, check: func(t *testing.T, got subprocess.Runner) {
			wrap := got.(subprocess.WrappingRunner)
			if _, ok := wrap.Inner.(subprocess.CuratedGroupRunner); !ok {
				t.Fatalf("wrapper inner = %T", wrap.Inner)
			}
		}},
		{name: "nested wrapping env tee", outer: subprocess.WrappingRunner{Inner: subprocess.EnvInjectingRunner{Env: []string{"RELAY=yes"}, Inner: subprocess.TeeRunner{}}}, check: func(t *testing.T, got subprocess.Runner) {
			wrap := got.(subprocess.WrappingRunner)
			env := wrap.Inner.(subprocess.EnvInjectingRunner)
			tee := env.Inner.(subprocess.TeeRunner)
			if _, ok := tee.Inner.(subprocess.CuratedGroupRunner); !ok {
				t.Fatalf("nested tee inner = %T", tee.Inner)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := runtimeJobRunner(home, runtime.ShellRuntime, test.outer)
			if err != nil {
				t.Fatalf("runtimeJobRunner: %v", err)
			}
			test.check(t, got)
			removeCuratedRunnerScratch(got)
		})
	}
}

func TestRuntimeJobRunnerClaudeAuthInjectionCurationOnAndOff(t *testing.T) {
	for _, curation := range []bool{false, true} {
		t.Run(fmt.Sprintf("curation-%t", curation), func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatal(err)
			}
			if curation {
				body := config.DefaultConfig(paths) + "\n[credentials]\nenv_curation = true\n"
				if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), map[string]string{
				runtime.ClaudeOAuthTokenEnv: testOAuthToken,
			}); err != nil {
				t.Fatal(err)
			}
			t.Setenv(runtime.ClaudeOAuthTokenEnv, "sk-ant-oat01-ambient-token-abcdefghijklmnopqrstuvwxyz")
			t.Setenv(runtime.AnthropicAPIKeyEnv, testAPIKey)
			t.Setenv(runtime.AnthropicAuthTokenEnv, testAuthToken)

			runner, err := runtimeJobRunner(home, runtime.ClaudeRuntime, nil)
			if err != nil {
				t.Fatal(err)
			}
			base, ok := runner.(subprocess.CuratedGroupRunner)
			if !ok {
				t.Fatalf("runner = %T", runner)
			}
			got := effectiveEnv(base.BaseEnv)
			if got[runtime.ClaudeOAuthTokenEnv] != testOAuthToken {
				t.Fatalf("OAuth = %q", got[runtime.ClaudeOAuthTokenEnv])
			}
			if got[runtime.AnthropicAPIKeyEnv] != "" || got[runtime.AnthropicAuthTokenEnv] != "" {
				t.Fatalf("blank-out failed: api=%q auth=%q", got[runtime.AnthropicAPIKeyEnv], got[runtime.AnthropicAuthTokenEnv])
			}
		})
	}
}

func TestRuntimeJobRunnerClaudeAuthNoInjectionStatesAndFailures(t *testing.T) {
	clearAmbient := func(t *testing.T) {
		t.Helper()
		for _, name := range runtimeAuthEnvVars {
			t.Setenv(name, "")
		}
	}
	t.Run("explicit empty", func(t *testing.T) {
		clearAmbient(t)
		home := t.TempDir()
		paths := config.PathsForHome(home)
		if err := config.Initialize(paths); err != nil {
			t.Fatal(err)
		}
		if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), nil); err != nil {
			t.Fatal(err)
		}
		runner, err := runtimeJobRunner(home, runtime.ClaudeRuntime, nil)
		if err != nil || runner != nil {
			t.Fatalf("runner=%T err=%v", runner, err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		clearAmbient(t)
		home := t.TempDir()
		if err := config.Initialize(config.PathsForHome(home)); err != nil {
			t.Fatal(err)
		}
		runner, err := runtimeJobRunner(home, runtime.ClaudeRuntime, nil)
		if err != nil || runner != nil {
			t.Fatalf("runner=%T err=%v", runner, err)
		}
	})
	for _, test := range []struct {
		name string
		body string
		mode os.FileMode
	}{
		{name: "malformed", body: "BAD=value\n", mode: 0o600},
		{name: "permissions", body: runtime.ClaudeOAuthTokenEnv + "=" + testOAuthToken + "\n", mode: 0o644},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearAmbient(t)
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatal(err)
			}
			path := runtimeAuthFilePath(paths.Home)
			if err := os.WriteFile(path, []byte(test.body), test.mode); err != nil {
				t.Fatal(err)
			}
			_, err := runtimeJobRunner(home, runtime.ClaudeRuntime, nil)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("error = %v", err)
			}
			if other, err := runtimeJobRunner(home, runtime.ShellRuntime, nil); err != nil || other != nil {
				t.Fatalf("non-Claude runner=%T err=%v", other, err)
			}
		})
	}
}

func TestRuntimeJobRunnerClaudeAuthRemainsBelowWrapperEnvironment(t *testing.T) {
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
	runner, err := runtimeJobRunner(home, runtime.ClaudeRuntime, subprocess.EnvInjectingRunner{Env: []string{
		runtime.ClaudeConfigDirEnv + "=/wrapper-config",
		"GITMOOT_CHAT_RELAY=/relay.sock",
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), "", "sh", "-c", `printf '%s|%s|%s|%s' "$CLAUDE_CODE_OAUTH_TOKEN" "$ANTHROPIC_API_KEY" "$CLAUDE_CONFIG_DIR" "$GITMOOT_CHAT_RELAY"`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != testOAuthToken+"||/wrapper-config|/relay.sock" {
		t.Fatalf("wrapper ordering output = %q", result.Stdout)
	}
}

func effectiveEnv(entries []string) map[string]string {
	values := map[string]string{}
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}

func removeCuratedRunnerScratch(runner subprocess.Runner) {
	switch runner := runner.(type) {
	case subprocess.CuratedGroupRunner:
		for _, path := range runner.ScratchDirs {
			_ = os.RemoveAll(path)
		}
	case subprocess.TeeRunner:
		if inner, ok := runner.Inner.(subprocess.Runner); ok {
			removeCuratedRunnerScratch(inner)
		}
	case subprocess.EnvInjectingRunner:
		removeCuratedRunnerScratch(runner.Inner)
	case subprocess.WrappingRunner:
		removeCuratedRunnerScratch(runner.Inner)
	}
}

func TestCuratedRunnerCompositionPreservesInjectedEnvironment(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+"\n[credentials]\nenv_curation = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	runner, err := runtimeJobRunner(home, runtime.ShellRuntime, subprocess.TeeRunner{Out: &streamed})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), "", "sh", "-c", `printf '%s' "$PATH"`)
	if err != nil || result.Stdout == "" || streamed.Len() == 0 {
		t.Fatalf("curated+tee result=%+v stream=%q err=%v", result, streamed.String(), err)
	}

	runner, err = runtimeJobRunner(home, runtime.ShellRuntime, subprocess.EnvInjectingRunner{Env: []string{"GITMOOT_CHAT_RELAY=/relay", "GITMOOT_CHAT_RELAY_AUTH=token"}})
	if err != nil {
		t.Fatal(err)
	}
	adapter := runtime.ShellAdapter{Runner: runner}
	job := runtime.Job{
		Prompt: "prompt",
		ShellEnv: []string{
			"GITMOOT_PIPELINE_NAME=pipe",
			"GITMOOT_PIPELINE_RUN_ID=run",
			"GITMOOT_PIPELINE_STAGE_ID=stage",
		},
		ShellUpstreamContext: `{"schema_version":1,"complete":true}`,
	}
	agent := runtime.Agent{Name: "shell", Role: "runner", Runtime: runtime.ShellRuntime, RepoScope: "owner/repo", RuntimeRef: `printf '%s|%s|%s|%s|%s' "$GITMOOT_CHAT_RELAY" "$GITMOOT_CHAT_RELAY_AUTH" "$GITMOOT_PIPELINE_NAME" "$GITMOOT_PIPELINE_STAGE_ID" "$(test -r "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE" && echo context)"`}
	got, err := adapter.Deliver(context.Background(), agent, job)
	if err != nil {
		t.Fatal(err)
	}
	if got.Raw != "/relay|token|pipe|stage|context" {
		t.Fatalf("injected environment = %q", got.Raw)
	}
}

func TestRuntimeCredentialCurationBothDispatchFactories(t *testing.T) {
	t.Setenv("GH_TOKEN", "seed-gh")
	t.Setenv("GITHUB_TOKEN", "seed-github")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "seed-claude")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/seed-ssh")
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	write := func(enabled bool) {
		t.Helper()
		body := "\n[credentials]\nenv_curation = false\n"
		if enabled {
			body = "\n[credentials]\nenv_curation = true\ngithub = \"deny\"\n"
		}
		if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	type deliverer interface {
		Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error)
	}
	builders := []struct {
		name  string
		build func() (deliverer, error)
	}{
		{name: "foreground", build: func() (deliverer, error) {
			return runtimeAdapterFor(home, runtime.ShellRuntime, "")
		}},
		{name: "daemon-worker", build: func() (deliverer, error) {
			return buildRuntimeAdapter(home, runtime.Agent{Runtime: runtime.ShellRuntime}, "", nil)
		}},
	}
	script := `env | sort; if test -n "$GH_CONFIG_DIR"; then printf 'GH_EMPTY='; test -z "$(find "$GH_CONFIG_DIR" -mindepth 1 -print -quit)" && echo yes || echo no; printf 'GH_MODE='; stat -c %a "$GH_CONFIG_DIR"; fi`
	agent := runtime.Agent{Name: "shell", Role: "runner", Runtime: runtime.ShellRuntime, RepoScope: "owner/repo", RuntimeRef: script}
	for _, builder := range builders {
		t.Run(builder.name, func(t *testing.T) {
			write(true)
			adapter, err := builder.build()
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "print env"})
			if err != nil {
				t.Fatal(err)
			}
			for _, absent := range []string{"GH_TOKEN=seed-gh", "GITHUB_TOKEN=seed-github", "CLAUDE_CODE_OAUTH_TOKEN=seed-claude", "SSH_AUTH_SOCK=/tmp/seed-ssh"} {
				if strings.Contains(result.Raw, absent) {
					t.Fatalf("curated output leaked %s:\n%s", absent, result.Raw)
				}
			}
			for _, present := range []string{"PATH=", "HOME=", "GH_CONFIG_DIR=", "GH_PROMPT_DISABLED=1", "GH_EMPTY=yes", "GH_MODE=700"} {
				if !strings.Contains(result.Raw, present) {
					t.Fatalf("curated output missing %s:\n%s", present, result.Raw)
				}
			}
			var scratch string
			for _, line := range strings.Split(result.Raw, "\n") {
				if strings.HasPrefix(line, "GH_CONFIG_DIR=") {
					scratch = strings.TrimPrefix(line, "GH_CONFIG_DIR=")
				}
			}
			if scratch == "" {
				t.Fatal("missing scratch path")
			}
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Fatalf("GitHub scratch remains after delivery: %v", err)
			}

			write(false)
			adapter, err = builder.build()
			if err != nil {
				t.Fatal(err)
			}
			result, err = adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "print env"})
			if err != nil {
				t.Fatal(err)
			}
			for _, inherited := range []string{"GH_TOKEN=seed-gh", "GITHUB_TOKEN=seed-github", "CLAUDE_CODE_OAUTH_TOKEN=seed-claude", "SSH_AUTH_SOCK=/tmp/seed-ssh"} {
				if !strings.Contains(result.Raw, inherited) {
					t.Fatalf("off output missing inherited %s:\n%s", inherited, result.Raw)
				}
			}
		})
	}
}

func TestRuntimeCredentialCurationForegroundAndDaemonE2E(t *testing.T) {
	t.Setenv("GH_TOKEN", "seed-gh")
	t.Setenv("GITHUB_TOKEN", "seed-github")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "seed-claude")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/seed-ssh")
	t.Setenv("GH_CONFIG_DIR", "/tmp/ambient-gh-config")
	t.Setenv("GH_PROMPT_DISABLED", "ambient")

	for _, enabled := range []bool{true, false} {
		for _, background := range []bool{false, true} {
			name := fmt.Sprintf("curation-%t/background-%t", enabled, background)
			t.Run(name, func(t *testing.T) {
				home, store, checkout := runtimeOverrideE2EHome(t)
				paths := config.PathsForHome(home)
				body := "\n[credentials]\nenv_curation = false\n"
				if enabled {
					body = "\n[credentials]\nenv_curation = true\ngithub = \"deny\"\n"
				}
				if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
					t.Fatal(err)
				}
				evidence := filepath.Join(checkout, "credential-env.txt")
				script := fmt.Sprintf(`env | sort > %q
if test "$GH_CONFIG_DIR" != /tmp/ambient-gh-config && test -n "$GH_CONFIG_DIR"; then
  printf 'GH_EMPTY=' >> %q
  test -z "$(find "$GH_CONFIG_DIR" -mindepth 1 -print -quit)" && echo yes >> %q || echo no >> %q
  printf 'GH_MODE=' >> %q
  stat -c %%a "$GH_CONFIG_DIR" >> %q
fi
printf '%%s' '{"gitmoot_result":{"decision":"approved","summary":"credential e2e","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`,
					evidence, evidence, evidence, evidence, evidence, evidence)
				args := []string{
					"agent", "ask", "maintainer", "inspect environment",
					"--home", home,
					"--repo", "owner/repo",
					"--runtime", "shell",
					"--session", script,
					"--json",
				}
				if background {
					args = append(args, "--background")
				}
				var stdout, stderr strings.Builder
				if code := Run(args, &stdout, &stderr); code != 0 {
					t.Fatalf("agent ask exit=%d stderr=%s", code, stderr.String())
				}
				if background {
					worker := defaultJobWorker(store, io.Discard, home)
					if err := runEnabledRepoWorkerTicks(context.Background(), store, worker, 1, io.Discard, time.Now().UTC()); err != nil {
						t.Fatalf("worker tick: %v", err)
					}
				}
				data, err := os.ReadFile(evidence)
				if err != nil {
					t.Fatalf("read evidence: %v", err)
				}
				got := string(data)
				seeded := []string{"GH_TOKEN=seed-gh", "GITHUB_TOKEN=seed-github", "CLAUDE_CODE_OAUTH_TOKEN=seed-claude", "SSH_AUTH_SOCK=/tmp/seed-ssh"}
				if enabled {
					for _, secret := range seeded {
						if strings.Contains(got, secret) {
							t.Fatalf("curated E2E leaked %s:\n%s", secret, got)
						}
					}
					for _, want := range []string{"PATH=", "HOME=", "GH_CONFIG_DIR=", "GH_PROMPT_DISABLED=1", "GH_EMPTY=yes", "GH_MODE=700"} {
						if !strings.Contains(got, want) {
							t.Fatalf("curated E2E missing %s:\n%s", want, got)
						}
					}
					var scratch string
					for _, line := range strings.Split(got, "\n") {
						if strings.HasPrefix(line, "GH_CONFIG_DIR=") {
							scratch = strings.TrimPrefix(line, "GH_CONFIG_DIR=")
						}
					}
					if _, err := os.Stat(scratch); !os.IsNotExist(err) {
						t.Fatalf("scratch remains after E2E: %v", err)
					}
				} else {
					for _, inherited := range seeded {
						if !strings.Contains(got, inherited) {
							t.Fatalf("off E2E missing inherited %s:\n%s", inherited, got)
						}
					}
				}
			})
		}
	}
}
