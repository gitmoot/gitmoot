package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func withAuthSecretReader(t *testing.T, values ...string) {
	t.Helper()
	previous := authReadSecret
	next := 0
	authReadSecret = func(io.Writer) (string, error) {
		if next >= len(values) {
			return "", fmt.Errorf("no test secret remaining")
		}
		value := values[next]
		next++
		return value, nil
	}
	t.Cleanup(func() { authReadSecret = previous })
}

func TestAuthSetUsesStdinAtomicWriteAndPermissions(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	path := runtimeAuthFilePath(paths.Home)
	if err := writeRuntimeAuthFile(path, map[string]string{runtime.ClaudeOAuthTokenEnv: testAuthToken}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	withAuthSecretReader(t, testOAuthToken)
	var stdout, stderr bytes.Buffer
	if code := runAuthSet([]string{"claude", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("runtime auth write reused the old inode; want atomic temp+rename replacement")
	}
	if after.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %04o", after.Mode().Perm())
	}
	state, err := loadRuntimeAuthFile(paths.Home)
	if err != nil || state.Values[runtime.ClaudeOAuthTokenEnv] != testOAuthToken {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if strings.Contains(stdout.String()+stderr.String(), testOAuthToken) {
		t.Fatal("auth set output leaked the token")
	}
}

func TestAuthSetReadsTokenFromStdin(t *testing.T) {
	home := t.TempDir()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(testOAuthToken + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	previousStdin := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = previousStdin
		_ = reader.Close()
	})
	var stdout, stderr bytes.Buffer
	if code := runAuthSet([]string{"claude", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	state, err := loadRuntimeAuthFile(config.PathsForHome(home).Home)
	if err != nil || state.Values[runtime.ClaudeOAuthTokenEnv] != testOAuthToken {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestAuthSetValidationWarningFromEnvAndUnset(t *testing.T) {
	t.Run("warn non oat", func(t *testing.T) {
		home := t.TempDir()
		withAuthSecretReader(t, "valid-but-not-oat-token-abcdefghijklmnopqrstuvwxyz")
		var stdout, stderr bytes.Buffer
		if code := runAuthSet([]string{"claude", "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "WARNING") || !strings.Contains(stderr.String(), "long-lived") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
	t.Run("reject whitespace", func(t *testing.T) {
		home := t.TempDir()
		withAuthSecretReader(t, "sk-ant-oat01-token with-space-abcdefghijklmnopqrstuvwxyz")
		var stdout, stderr bytes.Buffer
		if code := runAuthSet([]string{"claude", "--home", home}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "whitespace") {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	})
	t.Run("from env", func(t *testing.T) {
		home := t.TempDir()
		oldLookup := runtimeAuthEnvLookup
		runtimeAuthEnvLookup = func(name string) (string, bool) {
			if name == runtime.AnthropicAPIKeyEnv {
				return testAPIKey, true
			}
			return "", false
		}
		t.Cleanup(func() { runtimeAuthEnvLookup = oldLookup })
		var stdout, stderr bytes.Buffer
		if code := runAuthSet([]string{"claude", "--home", home, "--from-env"}, &stdout, &stderr); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		state, err := loadRuntimeAuthFile(config.PathsForHome(home).Home)
		if err != nil || state.Values[runtime.AnthropicAPIKeyEnv] != testAPIKey {
			t.Fatalf("state=%+v err=%v", state, err)
		}
	})
	t.Run("unset explicit empty", func(t *testing.T) {
		home := t.TempDir()
		path := runtimeAuthFilePath(config.PathsForHome(home).Home)
		if err := writeRuntimeAuthFile(path, map[string]string{runtime.ClaudeOAuthTokenEnv: testOAuthToken}); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := runAuthUnset([]string{"claude", "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		state, err := loadRuntimeAuthFile(config.PathsForHome(home).Home)
		if err != nil || !state.Exists || len(state.Values) != 0 {
			t.Fatalf("state=%+v err=%v", state, err)
		}
	})
}

func TestAuthStatusIsMaskedAndRejectsUnsafePermissions(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), map[string]string{runtime.ClaudeOAuthTokenEnv: testOAuthToken}); err != nil {
		t.Fatal(err)
	}
	oldLookup := runtimeAuthEnvLookup
	runtimeAuthEnvLookup = func(name string) (string, bool) {
		if name == runtime.AnthropicAPIKeyEnv {
			return testAPIKey, true
		}
		return "", false
	}
	t.Cleanup(func() { runtimeAuthEnvLookup = oldLookup })
	var stdout, stderr bytes.Buffer
	if code := runAuthStatus([]string{"--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"permissions 0600", "winner: runtime-auth.env", maskedAuthFingerprint(testOAuthToken), maskedAuthFingerprint(testAPIKey)} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testOAuthToken) || strings.Contains(out, testAPIKey) {
		t.Fatalf("status leaked a token:\n%s", out)
	}
	if err := os.Chmod(runtimeAuthFilePath(paths.Home), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAuthStatus([]string{"--home", home}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "refusing") || !strings.Contains(stderr.String(), "0644") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestAuthStatusReportsLegacyBootstrapWinner(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath := legacyRuntimeAuthFilePath(paths.Home)
	if err := os.WriteFile(legacyPath, []byte(runtime.ClaudeOAuthTokenEnv+"="+testOAuthToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runAuthStatus([]string{"--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "winner: "+legacyRuntimeAuthFileName) || !strings.Contains(out, maskedAuthFingerprint(testOAuthToken)) {
		t.Fatalf("status did not report masked legacy winner:\n%s", out)
	}
	if strings.Contains(out, testOAuthToken) {
		t.Fatalf("status leaked legacy token:\n%s", out)
	}
}

func TestClaudeRuntimeAuthRotatesWithoutRestartE2E(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	shim := filepath.Join(bin, "claude")
	script := `#!/bin/sh
fingerprint=$(printf '%s' "$CLAUDE_CODE_OAUTH_TOKEN" | sha256sum | cut -d' ' -f1)
printf '{"type":"result","is_error":false,"result":"%s"}\n' "$fingerprint"
`
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range runtimeAuthEnvVars {
		t.Setenv(name, "")
	}

	tokenA := "sk-ant-oat01-token-a-abcdefghijklmnopqrstuvwxyz"
	tokenB := "sk-ant-oat01-token-b-abcdefghijklmnopqrstuvwxyz"
	tokenC := "sk-ant-oat01-token-c-abcdefghijklmnopqrstuvwxyz"
	withAuthSecretReader(t, tokenA, tokenB)
	set := func() {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := runAuthSet([]string{"claude", "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("auth set code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
	deliver := func() string {
		t.Helper()
		adapter, err := buildRuntimeAdapter(home, runtime.Agent{Runtime: runtime.ClaudeRuntime}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		agent := runtime.Agent{
			Name: "claude-test", Role: "implementer", Runtime: runtime.ClaudeRuntime,
			RuntimeRef: runtime.FreshRefForJob("auth-e2e"), RepoScope: "owner/repo",
		}
		result, err := adapter.Deliver(context.Background(), agent, runtime.Job{ID: "auth-e2e", Prompt: "report fingerprint"})
		if err != nil {
			t.Fatal(err)
		}
		return result.Summary
	}
	set()
	if got := deliver(); got != shaFingerprint(tokenA) {
		t.Fatalf("first delivery fingerprint=%q want=%q", got, shaFingerprint(tokenA))
	}
	set()
	if got := deliver(); got != shaFingerprint(tokenB) {
		t.Fatalf("rotated delivery fingerprint=%q want=%q", got, shaFingerprint(tokenB))
	}

	t.Setenv(runtime.ClaudeOAuthTokenEnv, tokenC)
	oldLogf := runtimeAuthLogf
	var conflictLog strings.Builder
	runtimeAuthLogf = func(format string, args ...any) { fmt.Fprintf(&conflictLog, format, args...) }
	t.Cleanup(func() { runtimeAuthLogf = oldLogf })
	if got := deliver(); got != shaFingerprint(tokenB) {
		t.Fatalf("file did not beat ambient: got=%q want=%q", got, shaFingerprint(tokenB))
	}
	if !strings.Contains(conflictLog.String(), "WARNING") || strings.Contains(conflictLog.String(), tokenB) || strings.Contains(conflictLog.String(), tokenC) {
		t.Fatalf("conflict warning=%q", conflictLog.String())
	}
	var probeOut, probeErr bytes.Buffer
	if code := runAuthProbe([]string{"claude", "--home", home}, &probeOut, &probeErr); code != 0 {
		t.Fatalf("auth probe code=%d stdout=%q stderr=%q", code, probeOut.String(), probeErr.String())
	}
	if !strings.Contains(probeOut.String(), "source: "+runtimeAuthFileName) || !strings.Contains(probeOut.String(), "Claude auth: valid") {
		t.Fatalf("auth probe output=%q", probeOut.String())
	}

	var stdout, stderr bytes.Buffer
	if code := runAuthUnset([]string{"claude", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("auth unset code=%d stderr=%q", code, stderr.String())
	}
	if got := deliver(); got != shaFingerprint(tokenC) {
		t.Fatalf("explicit-empty fallback fingerprint=%q want ambient %q", got, shaFingerprint(tokenC))
	}
}

func shaFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
