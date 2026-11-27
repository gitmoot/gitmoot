package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/agyaddon"
)

func agyDoctorChecks(t *testing.T, home string) (int, map[string]pluginCheck, string) {
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
	checks := make(map[string]pluginCheck, len(output.Runtimes[0].Checks))
	for _, check := range output.Runtimes[0].Checks {
		checks[check.Name] = check
	}
	return code, checks, stderr.String()
}

func TestPluginInstallPathAndDoctorAgy(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "gemini-config")
	t.Setenv(agyaddon.ConfigDirEnv, configDir)
	defer stubPluginLookPath(map[string]string{"agy": "/opt/bin/agy"})()

	origPlatform := agyPlatformCheck
	agyPlatformCheck = func() error { return nil }
	t.Cleanup(func() { agyPlatformCheck = origPlatform })

	code, checks, _ := agyDoctorChecks(t, home)
	if code != 1 || checks["addon"].Status != "fail" || !strings.Contains(checks["addon"].Detail, "missing") {
		t.Fatalf("doctor before install: exit %d, addon %+v", code, checks["addon"])
	}

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

	stdout.Reset()
	if code := Run([]string{"plugin", "path", "agy", "--home", home}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != wantDir {
		t.Fatalf("plugin path agy = %q (exit %d), want %s", stdout.String(), code, wantDir)
	}

	stdout.Reset()
	if code := Run([]string{"plugin", "install", "agy", "--home", home}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "already up to date") {
		t.Fatalf("second install exit %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	code, checks, _ = agyDoctorChecks(t, home)
	if code != 0 || checks["addon"].Status != "ok" || !strings.Contains(checks["addon"].Detail, agyaddon.Version) || checks["platform"].Status != "ok" {
		t.Fatalf("doctor after install: exit %d, checks %+v", code, checks)
	}

	// Unsupported platform must fail doctor even when files and agy binary exist.
	agyPlatformCheck = func() error { return errors.New("agy process inspection and push delivery require Linux") }
	code, checks, _ = agyDoctorChecks(t, home)
	if code != 1 || checks["platform"].Status != "fail" {
		t.Fatalf("doctor on unsupported platform: exit %d, platform %+v", code, checks["platform"])
	}
	agyPlatformCheck = func() error { return nil }

	otherHome := t.TempDir()
	code, checks, _ = agyDoctorChecks(t, otherHome)
	if code != 1 || checks["addon"].Status != "fail" || !strings.Contains(checks["addon"].Detail, "outdated") {
		t.Fatalf("doctor for other home: exit %d, addon %+v", code, checks["addon"])
	}
}

func TestPluginAgyRelativeHomeWorksEndToEnd(t *testing.T) {
	base := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	configDir := filepath.Join(base, "gemini-config")
	t.Setenv(agyaddon.ConfigDirEnv, configDir)
	defer stubPluginLookPath(map[string]string{"agy": "/opt/bin/agy"})()

	origPlatform := agyPlatformCheck
	origFind := agyFindAncestor
	origPort := agyVerifyPort
	origMeta := agyVerifyMeta
	t.Cleanup(func() {
		agyPlatformCheck = origPlatform
		agyFindAncestor = origFind
		agyVerifyPort = origPort
		agyVerifyMeta = origMeta
	})
	agyPlatformCheck = func() error { return nil }
	agyFindAncestor = func() (int, uint64, error) { return 6161, 444555, nil }
	agyVerifyPort = func(int, string) (string, error) { return "127.0.0.1:42222", nil }
	agyVerifyMeta = func(context.Context, string, string, string) (string, int, error) {
		return agyaddon.InteractiveMode, 0, nil
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"plugin", "install", "agy", "--home", "./profile"}, &stdout, &stderr); code != 0 {
		t.Fatalf("install with relative --home failed (exit %d): %s", code, stderr.String())
	}
	code, checks, _ := agyDoctorChecks(t, "./profile")
	if code != 0 || checks["addon"].Status != "ok" {
		t.Fatalf("doctor with relative --home failed: exit %d, addon %+v", code, checks["addon"])
	}

	t.Setenv(agyaddon.EnvLSAddress, "localhost:42222")
	t.Setenv(agyaddon.EnvCSRFToken, "rel-tok")
	t.Setenv(agyaddon.EnvConversationID, "conv-rel")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"plugin", "agy-register", "--home", "./profile"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agy-register with relative --home failed (exit %d): %s", code, stderr.String())
	}
	absReg := filepath.Join(base, "profile", ".gitmoot", "run", "agy", "6161.json")
	if _, err := os.Stat(absReg); err != nil {
		t.Fatalf("expected registration at %s: %v", absReg, err)
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
	agyVerifyPort = func(int, string) (string, error) { return "127.0.0.1:41234", nil }
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

	// Hook updates conversation pin for top-level conversation and outputs `{}` with exit 0.
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
