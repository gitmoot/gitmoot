package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// TestPersistRuntimeAuthEnv_WritesTokenFile0600 is the SECURITY-CRITICAL
// assertion for #578: a token present in the environment is persisted to a file
// created 0600 (owner read/write ONLY) that contains the token value.
func TestPersistRuntimeAuthEnv_WritesTokenFile0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon-runtime.env")
	const token = "sk-ant-oat01-secret-abc123"

	wrote, err := PersistRuntimeAuthEnv(path, lookupFrom(map[string]string{ClaudeOAuthTokenEnv: token}))
	if err != nil {
		t.Fatalf("PersistRuntimeAuthEnv: %v", err)
	}
	if !wrote {
		t.Fatalf("expected a file to be written when a token is present")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 0600 (owner read/write only)", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), ClaudeOAuthTokenEnv+"="+token) {
		t.Fatalf("persisted file should contain the token line; got %q", string(data))
	}
}

// TestPersistRuntimeAuthEnv_NoTokenWritesNothing asserts a plain start on a box
// with no runtime auth in the env leaves no file (never create an empty file).
func TestPersistRuntimeAuthEnv_NoTokenWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon-runtime.env")

	wrote, err := PersistRuntimeAuthEnv(path, lookupFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("PersistRuntimeAuthEnv: %v", err)
	}
	if wrote {
		t.Fatalf("expected no write when no token is present")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file to exist; stat err = %v", err)
	}
}

// TestPersistRuntimeAuthEnv_EmptyEnvNeverErasesGoodToken guarantees the #578
// non-negotiable: an unset/empty env MUST NOT overwrite a good persisted token.
func TestPersistRuntimeAuthEnv_EmptyEnvNeverErasesGoodToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon-runtime.env")
	const token = "sk-ant-oat01-keep-me"

	if _, err := PersistRuntimeAuthEnv(path, lookupFrom(map[string]string{ClaudeOAuthTokenEnv: token})); err != nil {
		t.Fatalf("seed persist: %v", err)
	}
	// A subsequent start whose shell lacks the token must retain the good one.
	if _, err := PersistRuntimeAuthEnv(path, lookupFrom(map[string]string{ClaudeOAuthTokenEnv: "   "})); err != nil {
		t.Fatalf("second persist: %v", err)
	}
	loaded, err := LoadPersistedRuntimeAuthEnv(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded[ClaudeOAuthTokenEnv] != token {
		t.Fatalf("persisted token = %q, want %q (empty env must not erase it)", loaded[ClaudeOAuthTokenEnv], token)
	}
}

// TestPersistRuntimeAuthEnv_LiveEnvWins asserts a non-empty live env value takes
// precedence over the previously persisted value.
func TestPersistRuntimeAuthEnv_LiveEnvWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon-runtime.env")

	if _, err := PersistRuntimeAuthEnv(path, lookupFrom(map[string]string{ClaudeOAuthTokenEnv: "old"})); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := PersistRuntimeAuthEnv(path, lookupFrom(map[string]string{ClaudeOAuthTokenEnv: "new"})); err != nil {
		t.Fatalf("update: %v", err)
	}
	loaded, err := LoadPersistedRuntimeAuthEnv(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded[ClaudeOAuthTokenEnv] != "new" {
		t.Fatalf("token = %q, want live-env value %q", loaded[ClaudeOAuthTokenEnv], "new")
	}
}

// TestRuntimeAuthChildEnv_InjectsWhenEnvLacksToken proves the recovery path:
// with the token ABSENT from the live env but present in the file, the computed
// child env carries it; with the token present in the env, nothing is injected
// (live env wins, preserving pre-#578 inheritance).
func TestRuntimeAuthChildEnv_InjectsWhenEnvLacksToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon-runtime.env")
	const token = "sk-ant-oat01-recovered"

	if _, err := PersistRuntimeAuthEnv(path, lookupFrom(map[string]string{ClaudeOAuthTokenEnv: token})); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Env lacks the token -> inject from file.
	inject, err := RuntimeAuthChildEnv(path, lookupFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("child env: %v", err)
	}
	if want := ClaudeOAuthTokenEnv + "=" + token; !containsEntry(inject, want) {
		t.Fatalf("child env %v should contain %q", inject, want)
	}

	// Env already has a token -> never override.
	inject, err = RuntimeAuthChildEnv(path, lookupFrom(map[string]string{ClaudeOAuthTokenEnv: "live"}))
	if err != nil {
		t.Fatalf("child env (env present): %v", err)
	}
	if len(inject) != 0 {
		t.Fatalf("expected no injection when the live env already has a token; got %v", inject)
	}
}

// TestLoadPersistedRuntimeAuthEnv_MissingFile returns an empty map, nil error.
func TestLoadPersistedRuntimeAuthEnv_MissingFile(t *testing.T) {
	loaded, err := LoadPersistedRuntimeAuthEnv(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected empty map, got %v", loaded)
	}
}

func containsEntry(entries []string, want string) bool {
	for _, e := range entries {
		if e == want {
			return true
		}
	}
	return false
}
