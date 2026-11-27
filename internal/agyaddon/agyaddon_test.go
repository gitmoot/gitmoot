package agyaddon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallAndInspectPlugin(t *testing.T) {
	tmp := t.TempDir()
	pluginDir := filepath.Join(tmp, "plugins", PluginName)
	registryDir := filepath.Join(tmp, ".gitmoot", "run", "agy")
	gitmootBin := "/usr/local/bin/gitmoot"

	insp, err := Inspect(pluginDir, registryDir, gitmootBin)
	if err != nil {
		t.Fatal(err)
	}
	if insp.Status != StatusMissing {
		t.Fatalf("before install: status=%q, want missing", insp.Status)
	}

	gotDir, changed, err := Install(pluginDir, registryDir, gitmootBin)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if gotDir != pluginDir || !changed {
		t.Fatalf("Install = (%q, %v), want (%q, true)", gotDir, changed, pluginDir)
	}

	// Verify plugin.json and hooks.json parse cleanly and reference agy-hook.
	hooksRaw, err := os.ReadFile(filepath.Join(pluginDir, hooksFile))
	if err != nil {
		t.Fatal(err)
	}
	var hooks map[string]any
	if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
		t.Fatalf("hooks.json invalid JSON: %v", err)
	}
	if !strings.Contains(string(hooksRaw), "agy-hook") || !strings.Contains(string(hooksRaw), registryDir) {
		t.Fatalf("hooks.json missing agy-hook or registryDir:\n%s", string(hooksRaw))
	}

	// Second install is an idempotent no-op.
	_, changed, err = Install(pluginDir, registryDir, gitmootBin)
	if err != nil || changed {
		t.Fatalf("second Install = (changed=%v, err=%v), want (false, nil)", changed, err)
	}

	insp, err = Inspect(pluginDir, registryDir, gitmootBin)
	if err != nil || insp.Status != StatusInstalled || insp.Version != Version {
		t.Fatalf("Inspect after install = %+v, err=%v", insp, err)
	}

	// Different registry directory reports outdated.
	otherRegistry := filepath.Join(tmp, "other", ".gitmoot", "run", "agy")
	insp, err = Inspect(pluginDir, otherRegistry, gitmootBin)
	if err != nil || insp.Status != StatusOutdated || insp.RegistryDir != registryDir {
		t.Fatalf("Inspect for other registry = %+v, err=%v", insp, err)
	}

	// Hand edit to hooks.json reports outdated.
	if err := os.WriteFile(filepath.Join(pluginDir, hooksFile), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	insp, err = Inspect(pluginDir, registryDir, gitmootBin)
	if err != nil || insp.Status != StatusOutdated {
		t.Fatalf("Inspect after edit = %+v, err=%v", insp, err)
	}
}

func TestRegisterFromEnvValidationAndFastPath(t *testing.T) {
	registryDir := filepath.Join(t.TempDir(), "run", "agy")
	envMap := map[string]string{
		EnvLSAddress:      "127.0.0.1:43883",
		EnvCSRFToken:      "00000000-1111-2222-3333-444444444444",
		EnvConversationID: "conv-1",
		EnvHerdrPaneID:    "w1:p1",
	}
	getenv := func(k string) string { return envMap[k] }

	metaCalls := 0
	portCalls := 0
	opts := RegisterOptions{
		RegistryDir:  registryDir,
		Getenv:       getenv,
		FindAncestor: func() (int, uint64, error) { return 4242, 999001, nil },
		VerifyPort: func(pid int, addr string) error {
			portCalls++
			if pid != 4242 || addr != envMap[EnvLSAddress] {
				return errors.New("wrong pid/addr")
			}
			return nil
		},
		VerifyMetadata: func(_ context.Context, addr, token, conv string) (string, int, error) {
			metaCalls++
			return InteractiveMode, 0, nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 27, 8, 0, 0, 0, time.UTC) },
	}

	wrote, err := RegisterFromEnv(context.Background(), opts)
	if err != nil || !wrote {
		t.Fatalf("first RegisterFromEnv = (%v, %v), want (true, nil)", wrote, err)
	}
	if metaCalls != 1 || portCalls != 1 {
		t.Fatalf("expected 1 verify call each, got meta=%d port=%d", metaCalls, portCalls)
	}

	// Directory is 0700 and file is 0600.
	dirInfo, err := os.Lstat(registryDir)
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("registry dir mode = %v, err=%v; want 0700", dirInfo.Mode().Perm(), err)
	}
	reg, fileInfo, err := ReadRegistration(RegistrationPath(registryDir, 4242))
	if err != nil {
		t.Fatalf("ReadRegistration: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("registration file mode = %04o, want 0600", fileInfo.Mode().Perm())
	}
	if reg.PID != 4242 || reg.StartTimeTicks != 999001 || reg.ConversationID != "conv-1" || reg.LSAddress != envMap[EnvLSAddress] {
		t.Fatalf("unexpected registration: %+v", reg)
	}

	// Second call with identical env hits the fast path without calling port/metadata verifiers.
	wrote, err = RegisterFromEnv(context.Background(), opts)
	if err != nil || wrote {
		t.Fatalf("second RegisterFromEnv = (%v, %v), want (false, nil)", wrote, err)
	}
	if metaCalls != 1 || portCalls != 1 {
		t.Fatalf("fast path should not re-verify: meta=%d port=%d", metaCalls, portCalls)
	}

	// Subagent (nestingDepth > 0) is refused and does not overwrite.
	subOpts := opts
	subOpts.Getenv = func(k string) string {
		if k == EnvConversationID {
			return "sub-conv"
		}
		return envMap[k]
	}
	subOpts.VerifyMetadata = func(_ context.Context, _, _, _ string) (string, int, error) {
		return InteractiveMode, 1, nil
	}
	if wrote, err := RegisterFromEnv(context.Background(), subOpts); err == nil || wrote {
		t.Fatalf("subagent should be rejected, got (%v, %v)", wrote, err)
	}

	// Non-interactive print mode is refused.
	printOpts := subOpts
	printOpts.VerifyMetadata = func(_ context.Context, _, _, _ string) (string, int, error) {
		return "INTERACTION_MODE_NON_INTERACTIVE", 0, nil
	}
	if wrote, err := RegisterFromEnv(context.Background(), printOpts); err == nil || wrote {
		t.Fatalf("non-interactive session should be rejected, got (%v, %v)", wrote, err)
	}

	// Non-loopback lsAddress is refused.
	remoteOpts := opts
	remoteOpts.Getenv = func(k string) string {
		if k == EnvLSAddress {
			return "192.168.1.50:43883"
		}
		return envMap[k]
	}
	if wrote, err := RegisterFromEnv(context.Background(), remoteOpts); err == nil || wrote {
		t.Fatalf("non-loopback lsAddress should be rejected, got (%v, %v)", wrote, err)
	}
}

func TestRecordHookConversationRequiresMatchingRegistration(t *testing.T) {
	registryDir := filepath.Join(t.TempDir(), "run", "agy")
	now := time.Date(2026, 11, 27, 8, 5, 0, 0, time.UTC)

	// Unregistered PID is ignored cleanly without creating a file.
	wrote, err := RecordHookConversation(registryDir, 4242, 999001, "conv-new", "SessionStart", now)
	if err != nil || wrote {
		t.Fatalf("unregistered hook = (%v, %v), want (false, nil)", wrote, err)
	}

	// Register PID 4242, then record conversation switch.
	_, err = RegisterFromEnv(context.Background(), RegisterOptions{
		RegistryDir: registryDir,
		Getenv: func(k string) string {
			return map[string]string{
				EnvLSAddress:      "localhost:43883",
				EnvCSRFToken:      "tok",
				EnvConversationID: "conv-1",
			}[k]
		},
		FindAncestor:   func() (int, uint64, error) { return 4242, 999001, nil },
		VerifyPort:     func(int, string) error { return nil },
		VerifyMetadata: func(context.Context, string, string, string) (string, int, error) { return InteractiveMode, 0, nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mismatched startTime is ignored.
	wrote, err = RecordHookConversation(registryDir, 4242, 111111, "conv-wrong", "SessionStart", now)
	if err != nil || wrote {
		t.Fatalf("mismatched startTime = (%v, %v), want (false, nil)", wrote, err)
	}

	// Matching PID + startTime writes 0600 <pid>.conv.json.
	wrote, err = RecordHookConversation(registryDir, 4242, 999001, "conv-2", "PreInvocation", now)
	if err != nil || !wrote {
		t.Fatalf("matching hook = (%v, %v), want (true, nil)", wrote, err)
	}
	pin, info, err := ReadConvPin(ConvPinPath(registryDir, 4242))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || pin.ConversationID != "conv-2" || pin.Event != "PreInvocation" {
		t.Fatalf("unexpected pin: %+v mode=%04o", pin, info.Mode().Perm())
	}
}

func TestPruneDeadRegistrations(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agy")
	if err := EnsureRegistryDir(dir); err != nil {
		t.Fatal(err)
	}
	writeReg := func(pid int, start uint64) string {
		p := RegistrationPath(dir, pid)
		data, _ := json.Marshal(Registration{Version: RegistrationVersion, PID: pid, StartTimeTicks: start, LSAddress: "127.0.0.1:1234", CSRFToken: "t", ConversationID: "c"})
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	deadPath := writeReg(1001, 50)
	reusedPath := writeReg(1002, 50)
	livePath := writeReg(1003, 77)

	PruneDeadRegistrationsWith(
		dir,
		func(pid int) bool { return pid != 1001 },
		func(pid int) (uint64, error) {
			if pid == 1002 {
				return 99, nil // PID reused with newer start time
			}
			return 77, nil
		},
	)
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Fatalf("dead PID file was not pruned: %v", err)
	}
	if _, err := os.Stat(reusedPath); !os.IsNotExist(err) {
		t.Fatalf("reused PID file was not pruned: %v", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("live PID file was wrongly removed: %v", err)
	}
}

func TestVerifyPIDOwnsLoopbackPortRealListener(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs port ownership verification is Linux-only")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := VerifyPIDOwnsLoopbackPort(os.Getpid(), addr); err != nil {
		t.Fatalf("expected current process to own %s, got error: %v", addr, err)
	}
	// PID 1 does not own our test server's ephemeral port.
	if err := VerifyPIDOwnsLoopbackPort(1, addr); err == nil {
		t.Fatalf("expected PID 1 not to own %s", addr)
	}
	// Non-loopback address is rejected before touching procfs.
	if err := VerifyPIDOwnsLoopbackPort(os.Getpid(), "10.0.0.1:443"); err == nil {
		t.Fatal("expected non-loopback address to be rejected")
	}
}
