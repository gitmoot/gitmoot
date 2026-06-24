package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/pluginpack"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestRunPluginPathPrintsPackagePath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"plugin", "path", "codex", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin path exit code = %d, stderr=%s", code, stderr.String())
	}
	want := filepath.Join(home, ".gitmoot", "plugins", "build", "codex", "gitmoot") + "\n"
	if stdout.String() != want {
		t.Fatalf("plugin path stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunPluginCodexLaunchPrintsAddDirCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	repo := t.TempDir()

	code := Run([]string{"plugin", "codex-launch", "--home", home, "--repo", repo}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin codex-launch exit code = %d, stderr=%s", code, stderr.String())
	}
	want := formatCodexLaunchCommand("codex-face", repo, filepath.Join(home, ".gitmoot"), codexLaunchShell()) + "\n"
	if stdout.String() != want {
		t.Fatalf("plugin codex-launch stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunPluginCodexLaunchConfigSnippet(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"plugin", "codex-launch", "--home", home, "--config-snippet"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin codex-launch --config-snippet exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		`sandbox_mode = "workspace-write"`,
		`[sandbox_workspace_write]`,
		`writable_roots = [` + strconv.Quote(filepath.Join(home, ".gitmoot")) + `]`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("config snippet missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestFormatCodexLaunchCommandQuotesShellArguments(t *testing.T) {
	posix := formatCodexLaunchCommand("codex-face", "/tmp/repo with spaces", "/tmp/git'moot", "posix")
	if !strings.Contains(posix, "'/tmp/repo with spaces'") || !strings.Contains(posix, "'/tmp/git'\\''moot'") {
		t.Fatalf("posix command did not quote safely: %s", posix)
	}
	powershell := formatCodexLaunchCommand("codex-face", `C:\Repo With Spaces`, `C:\Users\O'Brien\.gitmoot`, "powershell")
	if !strings.Contains(powershell, `'C:\Repo With Spaces'`) || !strings.Contains(powershell, `'C:\Users\O''Brien\.gitmoot'`) {
		t.Fatalf("powershell command did not quote safely: %s", powershell)
	}
}

func TestRunPluginBuildPrintsPackagePath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"plugin", "build", "claude", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin build exit code = %d, stderr=%s", code, stderr.String())
	}
	packagePath := strings.TrimSpace(stdout.String())
	if packagePath != filepath.Join(home, ".gitmoot", "plugins", "build", "claude", "gitmoot") {
		t.Fatalf("unexpected package path %q", packagePath)
	}
	for _, path := range []string{
		filepath.Join(packagePath, ".claude-plugin", "plugin.json"),
		filepath.Join(packagePath, "skills", "gitmoot", "SKILL.md"),
		filepath.Join(packagePath, "hooks", "hooks.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected generated file %s: %v", path, err)
		}
	}
}

func TestRunPluginBuildExplicitOutDoesNotNeedHome(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := filepath.Join(t.TempDir(), "plugin")

	code := Run([]string{"plugin", "build", "codex", "--out", out}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin build --out exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != out {
		t.Fatalf("plugin build --out stdout = %q, want %q", stdout.String(), out)
	}
	if _, err := os.Stat(filepath.Join(out, ".codex-plugin", "plugin.json")); err != nil {
		t.Fatalf("codex manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "hooks", "hooks.json")); err != nil {
		t.Fatalf("hook manifest missing: %v", err)
	}
}

func TestRunPluginBuildRejectsBadRuntime(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run([]string{"plugin", "build", "nope"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("bad runtime exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown plugin runtime "nope"`) {
		t.Fatalf("stderr missing bad runtime:\n%s", stderr.String())
	}
}

func TestRunPluginBuildHelpBeforeRuntime(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run([]string{"plugin", "build", "--help"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin build --help exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-force") || !strings.Contains(stderr.String(), "-home") {
		t.Fatalf("plugin build --help missing flag help:\n%s", stderr.String())
	}
}

func TestRunPluginPathHelpBeforeRuntime(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run([]string{"plugin", "path", "--help"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin path --help exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-home") {
		t.Fatalf("plugin path --help missing flag help:\n%s", stderr.String())
	}
}

func TestResolveGitmootBinaryPrefersExecutable(t *testing.T) {
	restoreExecutable := stubPluginExecutable(func() (string, error) {
		return "/tmp/current/gitmoot", nil
	})
	defer restoreExecutable()
	restorePath := stubPluginLookPath(map[string]string{"gitmoot": "/tmp/path/gitmoot"})
	defer restorePath()

	if got := resolveGitmootBinary(); got != "/tmp/current/gitmoot" {
		t.Fatalf("resolveGitmootBinary() = %q, want current executable", got)
	}
}

func TestResolveGitmootBinaryFallsBackToLookPath(t *testing.T) {
	restoreExecutable := stubPluginExecutable(func() (string, error) {
		return "", errors.New("not available")
	})
	defer restoreExecutable()
	restorePath := stubPluginLookPath(map[string]string{"gitmoot": "/tmp/path/gitmoot"})
	defer restorePath()

	if got := resolveGitmootBinary(); got != "/tmp/path/gitmoot" {
		t.Fatalf("resolveGitmootBinary() = %q, want PATH binary", got)
	}
}

func TestRunPluginHookContextSuccessAndOutputShape(t *testing.T) {
	cwd := t.TempDir()
	input, err := json.Marshal(map[string]string{
		"cwd":             cwd,
		"hook_event_name": "SessionStart",
	})
	if err != nil {
		t.Fatalf("marshal hook input: %v", err)
	}
	restore := replacePluginHookInput(bytes.NewReader(input))
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "hook-context"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin hook-context exit code = %d, stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("plugin hook-context stderr = %q, want empty", stderr.String())
	}
	var output struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("hook-context output did not parse: %v\n%s", err, stdout.String())
	}
	if output.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hook event = %q, want SessionStart", output.HookSpecificOutput.HookEventName)
	}
	contextText := output.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		"- cwd: \"" + cwd + "\"",
		"- repo: not detected",
		"`gitmoot dashboard`",
		"answer directly",
		"live monitoring follow-up",
	} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("additional context missing %q:\n%s", want, contextText)
		}
	}
}

func TestRunPluginHookContextMalformedInputStillSucceeds(t *testing.T) {
	restore := replacePluginHookInput(strings.NewReader("{not json"))
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "hook-context"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin hook-context exit code = %d, stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("plugin hook-context stderr = %q, want empty", stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("hook-context output did not parse: %v\n%s", err, stdout.String())
	}
	if _, ok := output["hookSpecificOutput"]; !ok {
		t.Fatalf("hookSpecificOutput missing from malformed-input fallback: %s", stdout.String())
	}
}

func TestRunPluginUsageDoesNotListHookContext(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run([]string{"plugin", "--help"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin --help exit code = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "hook-context") {
		t.Fatalf("plugin help should not list hidden hook-context:\n%s", stdout.String())
	}
}

func TestRunPluginDoctorJSON(t *testing.T) {
	home := t.TempDir()
	build := pluginpack.BuildOptions{
		Provider: pluginpack.ProviderCodex,
		Home:     filepath.Join(home, ".gitmoot"),
	}
	if _, err := pluginpack.Build(build); err != nil {
		t.Fatalf("build package: %v", err)
	}
	restore := stubPluginLookPath(map[string]string{"codex": "/tmp/bin/codex"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "doctor", "codex", "--home", home, "--json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin doctor exit code = %d, stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	var output pluginDoctorOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("doctor JSON did not parse: %v\n%s", err, stdout.String())
	}
	if output.Home != filepath.Join(home, ".gitmoot") {
		t.Fatalf("doctor home = %q", output.Home)
	}
	if len(output.Runtimes) != 1 || output.Runtimes[0].Runtime != "codex" {
		t.Fatalf("unexpected runtimes: %+v", output.Runtimes)
	}
	if !output.Runtimes[0].Healthy {
		t.Fatalf("codex runtime should be healthy: %+v", output.Runtimes[0])
	}
	assertDoctorCheck(t, output.Runtimes[0].Checks, "manifest", "ok")
	assertDoctorCheck(t, output.Runtimes[0].Checks, "hook-manifest", "ok")
	assertDoctorCheck(t, output.Runtimes[0].Checks, "copied-skill", "ok")
	assertDoctorCheck(t, output.Runtimes[0].Checks, "marketplace-path", "ok")
	assertDoctorCheck(t, output.Runtimes[0].Checks, "runtime-cli", "ok")
	assertDoctorCheck(t, output.Runtimes[0].Checks, "validation-command", "warn")
}

func TestRunPluginDoctorFailsMissingHookManifest(t *testing.T) {
	home := t.TempDir()
	if _, err := pluginpack.Build(pluginpack.BuildOptions{
		Provider: pluginpack.ProviderCodex,
		Home:     filepath.Join(home, ".gitmoot"),
	}); err != nil {
		t.Fatalf("build package: %v", err)
	}
	packagePath := filepath.Join(home, ".gitmoot", "plugins", "build", "codex", "gitmoot")
	if err := os.Remove(pluginpack.HooksPath(packagePath)); err != nil {
		t.Fatalf("remove hooks manifest: %v", err)
	}
	restore := stubPluginLookPath(map[string]string{"codex": "/tmp/bin/codex"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "doctor", "codex", "--home", home, "--json"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("plugin doctor exit code = %d, want 1; stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "codex runtime is unhealthy") {
		t.Fatalf("stderr missing unhealthy runtime:\n%s", stderr.String())
	}
	var output pluginDoctorOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("doctor JSON did not parse: %v\n%s", err, stdout.String())
	}
	if output.Runtimes[0].Healthy {
		t.Fatalf("codex runtime should be unhealthy: %+v", output.Runtimes[0])
	}
	check := findDoctorCheck(t, output.Runtimes[0].Checks, "hook-manifest")
	if check.Status != "fail" {
		t.Fatalf("hook-manifest status = %q, want fail; checks=%+v", check.Status, output.Runtimes[0].Checks)
	}
	if !strings.Contains(check.Detail, filepath.Join("hooks", "hooks.json")) {
		t.Fatalf("hook-manifest detail = %q, want hooks path", check.Detail)
	}
}

func TestRunPluginDoctorClaudeReportsMaskedAuthEnv(t *testing.T) {
	withClaudeAuthEnv(t, map[string]string{runtime.ClaudeOAuthTokenEnv: "secret-token"})
	home := t.TempDir()
	if _, err := pluginpack.Build(pluginpack.BuildOptions{
		Provider: pluginpack.ProviderClaude,
		Home:     filepath.Join(home, ".gitmoot"),
	}); err != nil {
		t.Fatalf("build claude package: %v", err)
	}
	restore := stubPluginLookPath(map[string]string{"claude": "/tmp/bin/claude"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "doctor", "claude", "--home", home, "--json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin doctor exit code = %d, stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	var output pluginDoctorOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("doctor JSON did not parse: %v\n%s", err, stdout.String())
	}
	if len(output.Runtimes) != 1 || output.Runtimes[0].Runtime != "claude" {
		t.Fatalf("unexpected runtimes: %+v", output.Runtimes)
	}
	check := findDoctorCheck(t, output.Runtimes[0].Checks, "runtime-auth-env")
	if check.Status != "ok" {
		t.Fatalf("runtime-auth-env status = %q, want ok; checks=%+v", check.Status, output.Runtimes[0].Checks)
	}
	if !strings.Contains(check.Detail, runtime.ClaudeOAuthTokenEnv+"=set") || strings.Contains(check.Detail, "secret-token") {
		t.Fatalf("runtime-auth-env detail = %q", check.Detail)
	}
}

func TestRunPluginDoctorClaudeWarnsWhenAuthEnvMissing(t *testing.T) {
	withClaudeAuthEnv(t, nil)
	home := t.TempDir()
	if _, err := pluginpack.Build(pluginpack.BuildOptions{
		Provider: pluginpack.ProviderClaude,
		Home:     filepath.Join(home, ".gitmoot"),
	}); err != nil {
		t.Fatalf("build claude package: %v", err)
	}
	restore := stubPluginLookPath(map[string]string{"claude": "/tmp/bin/claude"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "doctor", "claude", "--home", home, "--json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin doctor exit code = %d, stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	var output pluginDoctorOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("doctor JSON did not parse: %v\n%s", err, stdout.String())
	}
	check := findDoctorCheck(t, output.Runtimes[0].Checks, "runtime-auth-env")
	if check.Status != "warn" {
		t.Fatalf("runtime-auth-env status = %q, want warn; checks=%+v", check.Status, output.Runtimes[0].Checks)
	}
	if !strings.Contains(check.Detail, "claude setup-token") {
		t.Fatalf("runtime-auth-env detail = %q, want setup-token guidance", check.Detail)
	}
}

func TestRunPluginDoctorClaudeLiveRunsSmoke(t *testing.T) {
	withClaudeAuthEnv(t, map[string]string{runtime.ClaudeOAuthTokenEnv: "secret-token"})
	home := t.TempDir()
	if _, err := pluginpack.Build(pluginpack.BuildOptions{
		Provider: pluginpack.ProviderClaude,
		Home:     filepath.Join(home, ".gitmoot"),
	}); err != nil {
		t.Fatalf("build claude package: %v", err)
	}
	restorePath := stubPluginLookPath(map[string]string{"claude": "/tmp/bin/claude"})
	defer restorePath()
	runner := &agentStartRunner{results: []subprocess.Result{{Stdout: `{"result":"OK"}`}}}
	restoreRunner := replacePluginDoctorRunner(runner)
	defer restoreRunner()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "doctor", "claude", "--home", home, "--live", "--json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin doctor exit code = %d, stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	var output pluginDoctorOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("doctor JSON did not parse: %v\n%s", err, stdout.String())
	}
	check := findDoctorCheck(t, output.Runtimes[0].Checks, "runtime-live")
	if check.Status != "ok" {
		t.Fatalf("runtime-live status = %q, want ok; checks=%+v", check.Status, output.Runtimes[0].Checks)
	}
	runner.want(t, 0, "", "claude", "-p", "--output-format", "json", "--", runtime.ClaudeLiveCheckPrompt)
}

func TestRunPluginDoctorClaudeLiveClassifiesAuthFailure(t *testing.T) {
	withClaudeAuthEnv(t, nil)
	home := t.TempDir()
	if _, err := pluginpack.Build(pluginpack.BuildOptions{
		Provider: pluginpack.ProviderClaude,
		Home:     filepath.Join(home, ".gitmoot"),
	}); err != nil {
		t.Fatalf("build claude package: %v", err)
	}
	restorePath := stubPluginLookPath(map[string]string{"claude": "/tmp/bin/claude"})
	defer restorePath()
	runner := &agentStartRunner{
		results: []subprocess.Result{{Stderr: "401 Invalid authentication credentials"}},
		errs:    []error{errors.New("exit 1")},
	}
	restoreRunner := replacePluginDoctorRunner(runner)
	defer restoreRunner()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "doctor", "claude", "--home", home, "--live", "--json"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("plugin doctor exit code = %d, want 1; stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	var output pluginDoctorOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("doctor JSON did not parse: %v\n%s", err, stdout.String())
	}
	check := findDoctorCheck(t, output.Runtimes[0].Checks, "runtime-live")
	if check.Status != "fail" {
		t.Fatalf("runtime-live status = %q, want fail; checks=%+v", check.Status, output.Runtimes[0].Checks)
	}
	if !strings.Contains(check.Detail, runtime.ClaudeSessionAuthFailedMessage) {
		t.Fatalf("runtime-live detail = %q, want session-failure guidance", check.Detail)
	}
}

func TestRunPluginDoctorFailsExplicitUnhealthyRuntime(t *testing.T) {
	restore := stubPluginLookPath(map[string]string{})
	defer restore()
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"plugin", "doctor", "claude", "--home", home}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("plugin doctor exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "claude runtime is unhealthy") {
		t.Fatalf("stderr missing unhealthy runtime:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "manifest") || !strings.Contains(stdout.String(), "fail") {
		t.Fatalf("stdout missing failed checks:\n%s", stdout.String())
	}
}

func TestRunPluginDoctorAllRuntimesAllowsMissingRuntimeWarnings(t *testing.T) {
	home := t.TempDir()
	if _, err := pluginpack.Build(pluginpack.BuildOptions{
		Provider: pluginpack.ProviderCodex,
		Home:     filepath.Join(home, ".gitmoot"),
	}); err != nil {
		t.Fatalf("build codex package: %v", err)
	}
	restore := stubPluginLookPath(map[string]string{"codex": "/tmp/bin/codex"})
	defer restore()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"plugin", "doctor", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("plugin doctor exit code = %d, stderr=%s\nstdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "codex: ok") {
		t.Fatalf("stdout missing healthy codex:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "claude: fail") {
		t.Fatalf("stdout missing unhealthy claude report:\n%s", stdout.String())
	}
}

func stubPluginLookPath(paths map[string]string) func() {
	original := pluginLookPath
	pluginLookPath = func(file string) (string, error) {
		if path, ok := paths[file]; ok {
			return path, nil
		}
		return "", errors.New("not found")
	}
	return func() {
		pluginLookPath = original
	}
}

func stubPluginExecutable(fn func() (string, error)) func() {
	original := pluginExecutable
	pluginExecutable = fn
	return func() {
		pluginExecutable = original
	}
}

func replacePluginHookInput(r io.Reader) func() {
	original := pluginHookInput
	pluginHookInput = r
	return func() {
		pluginHookInput = original
	}
}

func replacePluginDoctorRunner(runner subprocess.Runner) func() {
	original := pluginDoctorRunner
	pluginDoctorRunner = runner
	return func() {
		pluginDoctorRunner = original
	}
}

func assertDoctorCheck(t *testing.T, checks []pluginCheck, name string, status string) {
	t.Helper()
	check := findDoctorCheck(t, checks, name)
	if check.Status != status {
		t.Fatalf("%s status = %q, want %q; checks=%+v", name, check.Status, status, checks)
	}
}

func findDoctorCheck(t *testing.T, checks []pluginCheck, name string) pluginCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("missing check %q in %+v", name, checks)
	return pluginCheck{}
}
