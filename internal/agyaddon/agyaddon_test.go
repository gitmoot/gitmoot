package agyaddon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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

	_, changed, err = Install(pluginDir, registryDir, gitmootBin)
	if err != nil || changed {
		t.Fatalf("second Install = (changed=%v, err=%v), want (false, nil)", changed, err)
	}

	insp, err = Inspect(pluginDir, registryDir, gitmootBin)
	if err != nil || insp.Status != StatusInstalled || insp.Version != Version {
		t.Fatalf("Inspect after install = %+v, err=%v", insp, err)
	}

	otherRegistry := filepath.Join(tmp, "other", ".gitmoot", "run", "agy")
	insp, err = Inspect(pluginDir, otherRegistry, gitmootBin)
	if err != nil || insp.Status != StatusOutdated || insp.RegistryDir != registryDir {
		t.Fatalf("Inspect for other registry = %+v, err=%v", insp, err)
	}

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
		EnvLSAddress:      "localhost:43883",
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
		VerifyPort: func(pid int, addr string) (string, error) {
			portCalls++
			if pid != 4242 || addr != envMap[EnvLSAddress] {
				return "", errors.New("wrong pid/addr")
			}
			// Resolves localhost to the concrete verified numeric loopback address.
			return "127.0.0.1:43883", nil
		},
		VerifyMetadata: func(_ context.Context, addr, token, conv string) (string, int, error) {
			metaCalls++
			if addr != "127.0.0.1:43883" {
				t.Errorf("VerifyMetadata got %q, want concrete 127.0.0.1:43883", addr)
			}
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
	if reg.PID != 4242 || reg.StartTimeTicks != 999001 || reg.ConversationID != "conv-1" || reg.LSAddress != "127.0.0.1:43883" {
		t.Fatalf("unexpected registration: %+v", reg)
	}

	// Second call hits fast path without re-verifying port/metadata.
	wrote, err = RegisterFromEnv(context.Background(), opts)
	if err != nil || wrote {
		t.Fatalf("second RegisterFromEnv = (%v, %v), want (false, nil)", wrote, err)
	}
	if metaCalls != 1 || portCalls != 1 {
		t.Fatalf("fast path should not re-verify: meta=%d port=%d", metaCalls, portCalls)
	}

	// Subagent (nestingDepth > 0) is refused.
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

func TestRecordHookConversationRejectsUnregisteredAndSubagents(t *testing.T) {
	registryDir := filepath.Join(t.TempDir(), "run", "agy")
	now := func() time.Time { return time.Date(2026, 11, 27, 8, 5, 0, 0, time.UTC) }

	// Unregistered PID is ignored cleanly.
	wrote, err := RecordHookConversation(context.Background(), HookConversationOptions{
		RegistryDir:    registryDir,
		PID:            4242,
		StartTimeTicks: 999001,
		ConversationID: "conv-new",
		Event:          "SessionStart",
		VerifyPort:     func(int, string) (string, error) { return "127.0.0.1:43883", nil },
		VerifyMetadata: func(context.Context, string, string, string) (string, int, error) { return InteractiveMode, 0, nil },
		Now:            now,
	})
	if err != nil || wrote {
		t.Fatalf("unregistered hook = (%v, %v), want (false, nil)", wrote, err)
	}

	// Register PID 4242.
	_, err = RegisterFromEnv(context.Background(), RegisterOptions{
		RegistryDir: registryDir,
		Getenv: func(k string) string {
			return map[string]string{
				EnvLSAddress:      "127.0.0.1:43883",
				EnvCSRFToken:      "tok",
				EnvConversationID: "conv-1",
			}[k]
		},
		FindAncestor:   func() (int, uint64, error) { return 4242, 999001, nil },
		VerifyPort:     func(int, string) (string, error) { return "127.0.0.1:43883", nil },
		VerifyMetadata: func(context.Context, string, string, string) (string, int, error) { return InteractiveMode, 0, nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Subagent hook under the same PID (nestingDepth == 1) must NOT write a pin.
	wrote, err = RecordHookConversation(context.Background(), HookConversationOptions{
		RegistryDir:    registryDir,
		PID:            4242,
		StartTimeTicks: 999001,
		ConversationID: "conv-subagent",
		Event:          "SessionStart",
		VerifyPort:     func(int, string) (string, error) { return "127.0.0.1:43883", nil },
		VerifyMetadata: func(context.Context, string, string, string) (string, int, error) { return InteractiveMode, 1, nil },
		Now:            now,
	})
	if err != nil || wrote {
		t.Fatalf("subagent hook = (%v, %v), want (false, nil)", wrote, err)
	}
	if _, err := os.Stat(ConvPinPath(registryDir, 4242)); !os.IsNotExist(err) {
		t.Fatalf("subagent hook created conv pin: %v", err)
	}

	// Mismatched startTime is ignored.
	wrote, err = RecordHookConversation(context.Background(), HookConversationOptions{
		RegistryDir:    registryDir,
		PID:            4242,
		StartTimeTicks: 111111,
		ConversationID: "conv-wrong",
		Event:          "SessionStart",
		VerifyPort:     func(int, string) (string, error) { return "127.0.0.1:43883", nil },
		VerifyMetadata: func(context.Context, string, string, string) (string, int, error) { return InteractiveMode, 0, nil },
		Now:            now,
	})
	if err != nil || wrote {
		t.Fatalf("mismatched startTime = (%v, %v), want (false, nil)", wrote, err)
	}

	// Interactive top-level conversation (nestingDepth == 0) writes 0600 <pid>.conv.json.
	wrote, err = RecordHookConversation(context.Background(), HookConversationOptions{
		RegistryDir:    registryDir,
		PID:            4242,
		StartTimeTicks: 999001,
		ConversationID: "conv-2",
		Event:          "PreInvocation",
		VerifyPort:     func(int, string) (string, error) { return "127.0.0.1:43883", nil },
		VerifyMetadata: func(context.Context, string, string, string) (string, int, error) { return InteractiveMode, 0, nil },
		Now:            now,
	})
	if err != nil || !wrote {
		t.Fatalf("matching top-level hook = (%v, %v), want (true, nil)", wrote, err)
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
				return 99, nil
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

func TestVerifyPIDOwnsLoopbackPortConcreteAddressBinding(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs port ownership verification is Linux-only")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	// Exact IPv4 loopback succeeds and returns concrete 127.0.0.1:<port>.
	got, err := VerifyPIDOwnsLoopbackPort(os.Getpid(), addr)
	if err != nil || got != addr {
		t.Fatalf("VerifyPIDOwnsLoopbackPort(self, %s) = (%q, %v), want (%q, nil)", addr, got, err, addr)
	}

	// Querying localhost:<port> resolves to the concrete 127.0.0.1:<port> owned by this PID.
	got, err = VerifyPIDOwnsLoopbackPort(os.Getpid(), net.JoinHostPort("localhost", portStr))
	if err != nil || got != addr {
		t.Fatalf("VerifyPIDOwnsLoopbackPort(self, localhost:%s) = (%q, %v), want (%q, nil)", portStr, got, err, addr)
	}

	// Querying [::1]:<port> MUST fail because this PID only listens on IPv4 127.0.0.1!
	if gotV6, err := VerifyPIDOwnsLoopbackPort(os.Getpid(), net.JoinHostPort("::1", portStr)); err == nil {
		t.Fatalf("expected [::1]:%s to be rejected for IPv4-only listener, got %q", portStr, gotV6)
	}

	// PID 1 does not own our test server's ephemeral port.
	if _, err := VerifyPIDOwnsLoopbackPort(1, addr); err == nil {
		t.Fatalf("expected PID 1 not to own %s", addr)
	}
	// Non-loopback address is rejected before touching procfs.
	if _, err := VerifyPIDOwnsLoopbackPort(os.Getpid(), "10.0.0.1:443"); err == nil {
		t.Fatal("expected non-loopback address to be rejected")
	}
}
