package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testOAuthToken = "sk-ant-oat01-file-token-abcdefghijklmnopqrstuvwxyz"
	testAPIKey     = "sk-ant-api03-ambient-token-abcdefghijklmnopqrstuvwxyz"
	testAuthToken  = "sk-ant-auth-token-abcdefghijklmnopqrstuvwxyz"
)

func TestRuntimeAuthLoader(t *testing.T) {
	home := t.TempDir()
	if err := writeRuntimeAuthFile(runtimeAuthFilePath(home), map[string]string{
		claudeOAuthTokenEnv: testOAuthToken,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := loadRuntimeAuthFile(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Values[claudeOAuthTokenEnv]; got != testOAuthToken {
		t.Fatalf("file OAuth = %q", got)
	}
}

func TestRuntimeAuthLoaderEmptyMissingMalformedAndPermissions(t *testing.T) {
	t.Run("explicit empty", func(t *testing.T) {
		home := t.TempDir()
		if err := writeRuntimeAuthFile(runtimeAuthFilePath(home), nil); err != nil {
			t.Fatal(err)
		}
		state, err := loadRuntimeAuthFile(home)
		if err != nil {
			t.Fatal(err)
		}
		if !state.Exists || len(state.Values) != 0 {
			t.Fatalf("state = %+v", state)
		}
	})
	t.Run("missing", func(t *testing.T) {
		state, err := loadRuntimeAuthFile(t.TempDir())
		if err != nil || state.Exists {
			t.Fatalf("state=%+v err=%v", state, err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		home := t.TempDir()
		path := runtimeAuthFilePath(home)
		if err := os.WriteFile(path, []byte("NOT_MANAGED=secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadRuntimeAuthFile(home)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "line 1") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("permissions", func(t *testing.T) {
		home := t.TempDir()
		path := runtimeAuthFilePath(home)
		if err := os.WriteFile(path, []byte(claudeOAuthTokenEnv+"="+testOAuthToken+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := loadRuntimeAuthFile(home)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "0644") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRuntimeAuthBootstrapPrecedenceAndNoOverwrite(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == anthropicAPIKeyEnv {
			return testAPIKey, true
		}
		return "", false
	}
	t.Run("legacy first", func(t *testing.T) {
		home := t.TempDir()
		legacy := claudeOAuthTokenEnv + "=" + testOAuthToken + "\n"
		if err := os.WriteFile(legacyRuntimeAuthFilePath(home), []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		var logs []string
		seeded, err := bootstrapRuntimeAuth(home, lookup, func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		})
		if err != nil || !seeded {
			t.Fatalf("seeded=%v err=%v", seeded, err)
		}
		state, err := loadRuntimeAuthFile(home)
		if err != nil {
			t.Fatal(err)
		}
		if state.Values[claudeOAuthTokenEnv] != testOAuthToken || state.Values[anthropicAPIKeyEnv] != "" {
			t.Fatalf("legacy did not win: %+v", state.Values)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], legacyRuntimeAuthFileName) {
			t.Fatalf("logs = %v", logs)
		}
	})
	t.Run("ambient second", func(t *testing.T) {
		home := t.TempDir()
		seeded, err := bootstrapRuntimeAuth(home, lookup, nil)
		if err != nil || !seeded {
			t.Fatalf("seeded=%v err=%v", seeded, err)
		}
		state, err := loadRuntimeAuthFile(home)
		if err != nil || state.Values[anthropicAPIKeyEnv] != testAPIKey {
			t.Fatalf("state=%+v err=%v", state, err)
		}
	})
	t.Run("never overwrite", func(t *testing.T) {
		home := t.TempDir()
		if err := writeRuntimeAuthFile(runtimeAuthFilePath(home), map[string]string{anthropicAuthTokenEnv: testAuthToken}); err != nil {
			t.Fatal(err)
		}
		seeded, err := bootstrapRuntimeAuth(home, lookup, nil)
		if err != nil || seeded {
			t.Fatalf("seeded=%v err=%v", seeded, err)
		}
		state, _ := loadRuntimeAuthFile(home)
		if state.Values[anthropicAuthTokenEnv] != testAuthToken || len(state.Values) != 1 {
			t.Fatalf("existing file changed: %+v", state.Values)
		}
	})
}

func TestDaemonStartBootstrapsRuntimeAuth(t *testing.T) {
	home := t.TempDir()
	oldLookup := runtimeAuthEnvLookup
	runtimeAuthEnvLookup = func(name string) (string, bool) {
		if name == claudeOAuthTokenEnv {
			return testOAuthToken, true
		}
		return "", false
	}
	oldStart := startDaemonChildFn
	startDaemonChildFn = func(home, poll string, workers int, watchIssues bool, scheduler, repo, session string, state daemonState, workDir string) (daemonMeta, error) {
		return daemonMeta{PID: 987654, LogFile: state.LogFile}, nil
	}
	t.Cleanup(func() {
		runtimeAuthEnvLookup = oldLookup
		startDaemonChildFn = oldStart
	})
	var stdout, stderr bytes.Buffer
	if code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	state, err := loadRuntimeAuthFile(filepath.Join(home, ".gitmoot"))
	if err != nil || state.Values[claudeOAuthTokenEnv] != testOAuthToken {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}
