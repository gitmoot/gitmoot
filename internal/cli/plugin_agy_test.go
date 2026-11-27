package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/agyaddon"
)

func agyDoctor(t *testing.T, home string) (int, pluginCheck, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"plugin", "doctor", "agy", "--home", home, "--json"}, &stdout, &stderr)
	var output pluginDoctorOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("doctor JSON did not parse: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if len(output.Runtimes) != 1 || output.Runtimes[0].Runtime != "agy" {
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

func TestPluginInstallPathAndDoctorAgy(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "gemini-config")
	t.Setenv(agyaddon.ConfigDirEnv, configDir)
	defer stubPluginLookPath(map[string]string{"agy": "/opt/bin/agy"})()

	// Before install, doctor fails with missing.
	code, check, _ := agyDoctor(t, home)
	if code != 1 || check.Status != "fail" || !strings.Contains(check.Detail, "missing") {
		t.Fatalf("doctor before install: exit %d, addon %+v", code, check)
	}

	// Install plugin cleanly.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"plugin", "install", "agy", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("install exit %d: %s", code, stderr.String())
	}
	wantDir := filepath.Join(configDir, "plugins", agyaddon.PluginName)
	for _, name := range []string{"plugin.json", "hooks.json"} {
		if _, err := os.Stat(filepath.Join(wantDir, name)); err != nil {
			t.Fatalf("missing installed file %s: %v", name, err)
		}
	}

	// Global hooks.json must never be created or modified.
	if _, err := os.Stat(filepath.Join(configDir, "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("install must not touch global hooks.json: %v", err)
	}

	// Path prints plugin directory.
	stdout.Reset()
	if code := Run([]string{"plugin", "path", "agy", "--home", home}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != wantDir {
		t.Fatalf("plugin path agy = %q (exit %d), want %s", stdout.String(), code, wantDir)
	}

	// Second install is idempotent.
	stdout.Reset()
	if code := Run([]string{"plugin", "install", "agy", "--home", home}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "already up to date") {
		t.Fatalf("second install exit %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	// Doctor passes for installed home, fails as outdated for another home.
	code, check, _ = agyDoctor(t, home)
	if code != 0 || check.Status != "ok" || !strings.Contains(check.Detail, agyaddon.Version) {
		t.Fatalf("doctor after install: exit %d, addon %+v", code, check)
	}
	otherHome := t.TempDir()
	code, check, _ = agyDoctor(t, otherHome)
	if code != 1 || check.Status != "fail" || !strings.Contains(check.Detail, "outdated") {
		t.Fatalf("doctor for other home: exit %d, addon %+v", code, check)
	}
}

func TestPluginAgyRegisterAndHookCommands(t *testing.T) {
	home := t.TempDir()
	registryDir := filepath.Join(home, ".gitmoot", "run", "agy")

	origFind := agyFindAncestor
	origPort := agyVerifyPort
	origMeta := agyVerifyMeta
	origInput := agyHookInput
	t.Cleanup(func() {
		agyFindAncestor = origFind
		agyVerifyPort = origPort
		agyVerifyMeta = origMeta
		agyHookInput = origInput
	})

	agyFindAncestor = func() (int, uint64, error) { return 5150, 777123, nil }
	agyVerifyPort = func(int, string) error { return nil }
	agyVerifyMeta = func(context.Context, string, string, string) (string, int, error) {
		return agyaddon.InteractiveMode, 0, nil
	}

	t.Setenv(agyaddon.EnvLSAddress, "127.0.0.1:41234")
	t.Setenv(agyaddon.EnvCSRFToken, "test-token-value")
	t.Setenv(agyaddon.EnvConversationID, "conv-initial")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"plugin", "agy-register", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("agy-register exit %d: %s", code, stderr.String())
	}
	reg, _, err := agyaddon.ReadRegistration(agyaddon.RegistrationPath(registryDir, 5150))
	if err != nil {
		t.Fatalf("registration not written: %v", err)
	}
	if reg.ConversationID != "conv-initial" || reg.PID != 5150 {
		t.Fatalf("unexpected registration: %+v", reg)
	}

	// Hook updates conversation pin and outputs `{}` with exit 0.
	agyHookInput = strings.NewReader(`{"conversationId":"conv-switched","hookEventName":"SessionStart"}`)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"plugin", "agy-hook", "--registry-dir", registryDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("agy-hook exit %d: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "{}" {
		t.Fatalf("agy-hook stdout = %q, want {}", stdout.String())
	}
	pin, _, err := agyaddon.ReadConvPin(agyaddon.ConvPinPath(registryDir, 5150))
	if err != nil {
		t.Fatalf("conv pin not written: %v", err)
	}
	if pin.ConversationID != "conv-switched" || pin.Event != "SessionStart" {
		t.Fatalf("unexpected conv pin: %+v", pin)
	}
}
