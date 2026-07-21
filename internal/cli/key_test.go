package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

const keychainSentinel = "keychain-sentinel-value-874"

func writeDefaultKeychain(t *testing.T, home, body string) string {
	t.Helper()
	path := filepath.Join(home, ".config", "gitmoot", "keychain.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runKeyTestCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestKeyCLIRegistryGrantsAndSecretSafety(t *testing.T) {
	home := t.TempDir()
	path := writeDefaultKeychain(t, home, "SHARED="+keychainSentinel+"\nPROXY=proxy-"+keychainSentinel+"\n")

	if code, out, errOut := runKeyTestCommand(t, "key", "path", "--home", home, "--json"); code != 0 || !strings.Contains(out, path) || !strings.Contains(out, `"status": "ready"`) {
		t.Fatalf("key path code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) {
		t.Fatalf("key path leaked value: %q %q", out, errOut)
	}

	for _, tc := range []struct {
		name string
		mode string
	}{
		{"SHARED", db.KeychainModeInjected},
		{"PROXY", db.KeychainModeProxied},
	} {
		code, out, errOut := runKeyTestCommand(t, "key", "add", tc.name, "--mode", tc.mode, "--home", home, "--json")
		if code != 0 {
			t.Fatalf("key add %s code=%d out=%q err=%q", tc.name, code, out, errOut)
		}
		if strings.Contains(out+errOut, keychainSentinel) {
			t.Fatalf("key add leaked value: %q %q", out, errOut)
		}
	}
	if err := withStore(home, func(store *db.Store) error {
		ctx := context.Background()
		if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "pipe", SpecYAML: "name: pipe\nstages: [{id: run, cmd: echo}]\n"}); err != nil {
			return err
		}
		return store.UpsertAgent(ctx, db.Agent{Name: "seat", Runtime: "codex"})
	}); err != nil {
		t.Fatal(err)
	}

	if code, out, errOut := runKeyTestCommand(t, "key", "grant", "PROXY", "--pipeline", "pipe", "--home", home); code == 0 || !strings.Contains(errOut, "proxied") {
		t.Fatalf("proxied grant code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) {
		t.Fatalf("proxied refusal leaked value: %q %q", out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "grant", "PROXY", "--agent", "seat", "--home", home); code == 0 || !strings.Contains(errOut, "not configured") {
		t.Fatalf("unconfigured agent grant code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "configure", "PROXY", "--upstream", "https://api.example.test/v1/", "--auth", "header:X-Api-Token", "--home", home, "--json"); code != 0 || !strings.Contains(out, `"proxy_upstream": "https://api.example.test/v1"`) || !strings.Contains(out, `"proxy_auth_kind": "header"`) || !strings.Contains(out, `"proxy_header": "X-Api-Token"`) {
		t.Fatalf("key configure code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) {
		t.Fatalf("key configure leaked value: %q %q", out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "grant", "PROXY", "--pipeline", "pipe", "--home", home); code != 0 {
		t.Fatalf("configured proxied grant code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "grant", "SHARED", "--agent", "seat", "--home", home); code == 0 || !strings.Contains(errOut, "proxied-only") {
		t.Fatalf("injected agent grant code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "grant", "PROXY", "--agent", "seat", "--home", home, "--json"); code != 0 || !strings.Contains(out, `"agent": "seat"`) {
		t.Fatalf("agent grant code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) {
		t.Fatalf("agent grant leaked value: %q %q", out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "grant", "PROXY", "--pipeline", "pipe", "--agent", "seat", "--home", home); code == 0 || !strings.Contains(errOut, "exactly one") {
		t.Fatalf("mutually exclusive consumer flags code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "grant", "SHARED", "--pipeline", "pipe", "--home", home, "--json"); code != 0 {
		t.Fatalf("grant code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) {
		t.Fatalf("grant leaked value: %q %q", out, errOut)
	}

	for _, args := range [][]string{
		{"key", "list", "--home", home, "--json"},
		{"key", "show", "SHARED", "--home", home, "--json"},
		{"key", "show", "PROXY", "--home", home, "--json"},
		{"key", "list", "--home", home},
		{"key", "show", "SHARED", "--home", home},
		{"key", "show", "PROXY", "--home", home},
	} {
		code, out, errOut := runKeyTestCommand(t, args...)
		wantGrantTarget := args[1] == "show" || slices.Contains(args, "--json")
		wantName := "SHARED"
		if slices.Contains(args, "PROXY") {
			wantName = "PROXY"
		}
		if code != 0 || !strings.Contains(out, wantName) || (wantGrantTarget && !strings.Contains(out, "pipe")) {
			t.Fatalf("%v code=%d out=%q err=%q", args, code, out, errOut)
		}
		if wantName == "PROXY" && (!strings.Contains(out, "https://api.example.test/v1") || !strings.Contains(out, "X-Api-Token")) {
			t.Fatalf("%v omitted proxy configuration: %q", args, out)
		}
		if wantName == "PROXY" && wantGrantTarget && !strings.Contains(out, "seat") {
			t.Fatalf("%v omitted agent grant: %q", args, out)
		}
		if strings.Contains(out+errOut, keychainSentinel) {
			t.Fatalf("%v leaked value: %q %q", args, out, errOut)
		}
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "revoke", "PROXY", "--agent", "seat", "--home", home, "--json"); code != 0 || !strings.Contains(out, `"agent": "seat"`) || !strings.Contains(out, `"changed": true`) {
		t.Fatalf("agent revoke code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) {
		t.Fatalf("agent revoke leaked value: %q %q", out, errOut)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "rm", "SHARED", "--force", "--home", home, "--json"); code != 0 || !strings.Contains(out, `"file_entry_remains": true`) {
		t.Fatalf("rm force code=%d out=%q err=%q", code, out, errOut)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("key rm --force modified the keychain file")
	}

	code, out, errOut := runKeyTestCommand(t, "key", "add", "SHARED", "--value", keychainSentinel, "--mode", "injected", "--home", home)
	if code == 0 || !strings.Contains(errOut, "flag provided but not defined") {
		t.Fatalf("value flag unexpectedly accepted: code=%d out=%q err=%q", code, out, errOut)
	}
	if strings.Contains(printKeyUsageString(), "value") {
		t.Fatal("key command usage exposes a value-input option")
	}
}

func TestKeyConfigureRejectsUnsafeProxyMetadata(t *testing.T) {
	tests := []struct {
		name     string
		upstream string
		auth     string
		want     string
	}{
		{name: "http non-loopback", upstream: "http://api.example.test/v1", auth: "bearer", want: "HTTPS is required"},
		{name: "userinfo", upstream: "https://user:pass@api.example.test/v1", auth: "bearer", want: "userinfo is not allowed"},
		{name: "query", upstream: "https://api.example.test/v1?token=x", auth: "bearer", want: "query is not allowed"},
		{name: "fragment", upstream: "https://api.example.test/v1#part", auth: "bearer", want: "fragment is not allowed"},
		{name: "relative", upstream: "api.example.test/v1", auth: "bearer", want: "absolute URL with a host"},
		{name: "dot path", upstream: "https://api.example.test/v1/../admin", auth: "bearer", want: "escaping segment"},
		{name: "encoded dot path", upstream: "https://api.example.test/v1/%2e%2e/admin", auth: "bearer", want: "escaping segment"},
		{name: "bad header token", upstream: "https://api.example.test/v1", auth: "header:Bad Header", want: "HTTP token"},
		{name: "hop by hop", upstream: "https://api.example.test/v1", auth: "header:Connection", want: "not allowed"},
		{name: "host", upstream: "https://api.example.test/v1", auth: "header:Host", want: "not allowed"},
		{name: "cookie", upstream: "https://api.example.test/v1", auth: "header:Cookie", want: "not allowed"},
		{name: "proxy auth", upstream: "https://api.example.test/v1", auth: "header:Proxy-Authorization", want: "not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			writeDefaultKeychain(t, home, "PROXY=proxy-"+keychainSentinel+"\n")
			if code, out, errOut := runKeyTestCommand(t, "key", "add", "PROXY", "--mode", "proxied", "--home", home); code != 0 {
				t.Fatalf("key add code=%d out=%q err=%q", code, out, errOut)
			}
			code, out, errOut := runKeyTestCommand(t, "key", "configure", "PROXY", "--upstream", tt.upstream, "--auth", tt.auth, "--home", home)
			if code == 0 || !strings.Contains(errOut, tt.want) {
				t.Fatalf("configure code=%d out=%q err=%q want=%q", code, out, errOut, tt.want)
			}
			if strings.Contains(out+errOut, keychainSentinel) {
				t.Fatalf("configure error leaked value: out=%q err=%q", out, errOut)
			}
		})
	}

	home := t.TempDir()
	writeDefaultKeychain(t, home, "INJECTED="+keychainSentinel+"\n")
	if code, out, errOut := runKeyTestCommand(t, "key", "add", "INJECTED", "--mode", "injected", "--home", home); code != 0 {
		t.Fatalf("key add injected code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "configure", "INJECTED", "--upstream", "https://api.example.test", "--auth", "bearer", "--home", home); code == 0 || !strings.Contains(errOut, "requires proxied mode") {
		t.Fatalf("configure injected code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestKeychainFileValidationMatchesPipelineEnvFile(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, home string, store *db.Store)
		want  string
	}{
		{
			name:  "missing",
			setup: func(_ *testing.T, _ string, _ *db.Store) {},
			want:  "does not exist",
		},
		{
			name: "wrong mode",
			setup: func(t *testing.T, home string, _ *db.Store) {
				path := writeDefaultKeychain(t, home, "TOKEN="+keychainSentinel+"\n")
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "want 0600",
		},
		{
			name: "reserved key",
			setup: func(t *testing.T, home string, _ *db.Store) {
				writeDefaultKeychain(t, home, "GITMOOT_INTERNAL="+keychainSentinel+"\nTOKEN=x\n")
			},
			want: "reserved GITMOOT_*",
		},
		{
			name: "inside Gitmoot home",
			setup: func(t *testing.T, home string, _ *db.Store) {
				path := filepath.Join(home, ".gitmoot", "keychain.env")
				if err := os.WriteFile(path, []byte("TOKEN="+keychainSentinel+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				writeKeychainOverride(t, home, path)
			},
			want: "inside Gitmoot home",
		},
		{
			name: "inside managed checkout",
			setup: func(t *testing.T, home string, store *db.Store) {
				checkout := t.TempDir()
				if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout}); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(checkout, "keychain.env")
				if err := os.WriteFile(path, []byte("TOKEN="+keychainSentinel+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				writeKeychainOverride(t, home, path)
			},
			want: "inside managed checkout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, _, store := heartbeatLoopE2EHome(t)
			tt.setup(t, home, store)
			code, out, errOut := runKeyTestCommand(t, "key", "add", "TOKEN", "--mode", "injected", "--home", home)
			if code == 0 || !strings.Contains(errOut, tt.want) {
				t.Fatalf("code=%d out=%q err=%q want=%q", code, out, errOut, tt.want)
			}
			if strings.Contains(out+errOut, keychainSentinel) {
				t.Fatalf("validation leaked value: out=%q err=%q", out, errOut)
			}
		})
	}
}

func TestKeychainFileWrongOwner(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	writeDefaultKeychain(t, home, "TOKEN="+keychainSentinel+"\n")
	original := pipeline.PipelineEnvCurrentUID
	pipeline.PipelineEnvCurrentUID = func() uint32 { return original() + 1 }
	t.Cleanup(func() { pipeline.PipelineEnvCurrentUID = original })
	_, _, err := loadValidatedKeychainFile(context.Background(), store, home)
	if err == nil || !strings.Contains(err.Error(), "owned by uid") || strings.Contains(err.Error(), keychainSentinel) {
		t.Fatalf("wrong-owner error=%v", err)
	}
}

func writeKeychainOverride(t *testing.T, home, path string) {
	t.Helper()
	configPath := filepath.Join(home, ".gitmoot", "config.toml")
	if err := os.WriteFile(configPath, []byte("[credentials]\nkeychain_path = \""+path+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func printKeyUsageString() string {
	var out bytes.Buffer
	printKeyUsage(&out)
	return out.String()
}
