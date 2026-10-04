package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/ompaddon"
)

// shortOMPHome keeps <home>/.gitmoot/run/omp under the Unix socket path
// limit the installer enforces; t.TempDir paths embed the test name.
func shortOMPHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gmo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func ompDoctor(t *testing.T, home string) (int, pluginCheck, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "doctor", "omp", "--home", home, "--json"}, &stdout, &stderr)
	var output pluginDoctorOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("doctor JSON did not parse: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if len(output.Runtimes) != 1 || output.Runtimes[0].Runtime != "omp" {
		t.Fatalf("doctor runtimes = %+v", output.Runtimes)
	}
	for _, check := range output.Runtimes[0].Checks {
		if check.Name == "addon" {
			return code, check, stderr.String()
		}
	}
	t.Fatalf("doctor output has no addon check: %s", stdout.String())
	return 0, pluginCheck{}, ""
}

// The installed add-on must serve the Gitmoot home it was installed for: the
// daemon of --home H reads H/.gitmoot/run/omp, so a file baked for another
// home would register sessions where no daemon looks.
func TestPluginInstallOMPUsesAgentDirAndBakesHomeRegistry(t *testing.T) {
	home := shortOMPHome(t)
	agentDir := filepath.Join(t.TempDir(), "omp-agent")
	t.Setenv(ompaddon.AgentDirEnv, agentDir)
	defer stubPluginLookPath(map[string]string{"omp": "/opt/bin/omp"})()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"plugin", "install", "omp", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("install exit %d: %s", code, stderr.String())
	}
	installed := filepath.Join(agentDir, "extensions", ompaddon.FileName)
	inspection, err := ompaddon.Inspect(filepath.Dir(installed), filepath.Join(home, ".gitmoot", "run", "omp"))
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != ompaddon.StatusInstalled {
		t.Fatalf("add-on at %s for --home registry: %+v", installed, inspection)
	}

	stdout.Reset()
	if code := Run([]string{"plugin", "install", "omp", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("second install exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already up to date") {
		t.Fatalf("second install should be a no-op:\n%s", stdout.String())
	}

	code, check, _ := ompDoctor(t, home)
	if code != 0 || check.Status != "ok" || !strings.Contains(check.Detail, ompaddon.Version) {
		t.Fatalf("doctor after install: exit %d, addon %+v", code, check)
	}

	// The same add-on file inspected for another Gitmoot home is outdated.
	other := shortOMPHome(t)
	code, check, stderrText := ompDoctor(t, other)
	if code != 1 || check.Status != "fail" || !strings.Contains(check.Detail, "outdated") ||
		!strings.Contains(check.Detail, filepath.Join(home, ".gitmoot", "run", "omp")) {
		t.Fatalf("doctor for another home: exit %d, addon %+v, stderr %s", code, check, stderrText)
	}
}

func TestPluginInstallOMPDefaultsToHomeAgentDir(t *testing.T) {
	home := shortOMPHome(t)
	t.Setenv(ompaddon.AgentDirEnv, "")
	defer stubPluginLookPath(map[string]string{"omp": "/opt/bin/omp"})()

	code, check, _ := ompDoctor(t, home)
	if code != 1 || check.Status != "fail" || !strings.Contains(check.Detail, "missing") {
		t.Fatalf("doctor before install: exit %d, addon %+v", code, check)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"plugin", "install", "omp", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("install exit %d: %s", code, stderr.String())
	}
	want := filepath.Join(home, ".omp", "agent", "extensions", ompaddon.FileName)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("add-on not installed at %s: %v", want, err)
	}
	stdout.Reset()
	if code := Run([]string{"plugin", "path", "omp", "--home", home}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("plugin path omp = %q (exit %d), want %s", stdout.String(), code, want)
	}
}
