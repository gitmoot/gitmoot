package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/pluginpack"
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
	assertDoctorCheck(t, output.Runtimes[0].Checks, "copied-skill", "ok")
	assertDoctorCheck(t, output.Runtimes[0].Checks, "marketplace-path", "ok")
	assertDoctorCheck(t, output.Runtimes[0].Checks, "runtime-cli", "ok")
	assertDoctorCheck(t, output.Runtimes[0].Checks, "validation-command", "warn")
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

func assertDoctorCheck(t *testing.T, checks []pluginCheck, name string, status string) {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			if check.Status != status {
				t.Fatalf("%s status = %q, want %q; checks=%+v", name, check.Status, status, checks)
			}
			return
		}
	}
	t.Fatalf("missing check %q in %+v", name, checks)
}
