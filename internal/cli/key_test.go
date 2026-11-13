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
	"github.com/gitmoot/gitmoot/internal/keychain"
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

func TestKeyCLIRegistryAndSecretSafety(t *testing.T) {
	home := t.TempDir()
	path := writeDefaultKeychain(t, home, "LEGACY="+keychainSentinel+"\nPROXY=proxy-"+keychainSentinel+"\n")

	if code, out, errOut := runKeyTestCommand(t, "key", "path", "--home", home, "--json"); code != 0 || !strings.Contains(out, path) || !strings.Contains(out, `"status": "ready"`) {
		t.Fatalf("key path code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) {
		t.Fatalf("key path leaked value: %q %q", out, errOut)
	}

	if code, out, errOut := runKeyTestCommand(t, "key", "add", "PROXY", "--mode", db.KeychainModeProxied, "--home", home, "--json"); code != 0 {
		t.Fatalf("key add PROXY code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) || strings.Contains(out, "grants") {
		t.Fatalf("key add output leaked value or grants: %q %q", out, errOut)
	}
	// The injected mode is retired (#2542): adding one is refused and the error
	// names the only remaining mode.
	if code, out, errOut := runKeyTestCommand(t, "key", "add", "LEGACY", "--mode", "injected", "--home", home); code == 0 || !strings.Contains(errOut, "proxied") {
		t.Fatalf("injected add code=%d out=%q err=%q", code, out, errOut)
	}
	// A row written before #2542 may still carry mode "injected"; it must keep
	// listing, showing and removing.
	if err := withStore(home, func(store *db.Store) error {
		return store.ExecForTest(context.Background(), `INSERT INTO keychain_keys(name, mode) VALUES ('LEGACY', 'injected')`)
	}); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"grant", "revoke"} {
		if code, out, errOut := runKeyTestCommand(t, "key", removed, "PROXY", "--agent", "seat", "--home", home); code == 0 {
			t.Fatalf("key %s still accepted: out=%q err=%q", removed, out, errOut)
		}
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "configure", "PROXY", "--upstream", "https://api.example.test/v1/", "--auth", "header:X-Api-Token", "--home", home, "--json"); code != 0 || !strings.Contains(out, `"proxy_upstream": "https://api.example.test/v1"`) || !strings.Contains(out, `"proxy_auth_kind": "header"`) || !strings.Contains(out, `"proxy_header": "X-Api-Token"`) {
		t.Fatalf("key configure code=%d out=%q err=%q", code, out, errOut)
	} else if strings.Contains(out+errOut, keychainSentinel) {
		t.Fatalf("key configure leaked value: %q %q", out, errOut)
	}

	for _, args := range [][]string{
		{"key", "list", "--home", home, "--json"},
		{"key", "show", "LEGACY", "--home", home, "--json"},
		{"key", "show", "PROXY", "--home", home, "--json"},
		{"key", "list", "--home", home},
		{"key", "show", "LEGACY", "--home", home},
		{"key", "show", "PROXY", "--home", home},
	} {
		code, out, errOut := runKeyTestCommand(t, args...)
		wantName := "LEGACY"
		if slices.Contains(args, "PROXY") {
			wantName = "PROXY"
		}
		if code != 0 || !strings.Contains(out, wantName) {
			t.Fatalf("%v code=%d out=%q err=%q", args, code, out, errOut)
		}
		if wantName == "LEGACY" && !strings.Contains(out, "injected") {
			t.Fatalf("%v omitted the legacy mode: %q", args, out)
		}
		if wantName == "PROXY" && (!strings.Contains(out, "https://api.example.test/v1") || !strings.Contains(out, "X-Api-Token")) {
			t.Fatalf("%v omitted proxy configuration: %q", args, out)
		}
		if strings.Contains(out, "grant") {
			t.Fatalf("%v still renders grants: %q", args, out)
		}
		if strings.Contains(out+errOut, keychainSentinel) {
			t.Fatalf("%v leaked value: %q %q", args, out, errOut)
		}
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "rm", "LEGACY", "--force", "--home", home); code == 0 || !strings.Contains(errOut, "flag provided but not defined") {
		t.Fatalf("rm --force still accepted: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "rm", "LEGACY", "--home", home, "--json"); code != 0 || !strings.Contains(out, `"removed": true`) || !strings.Contains(out, `"file_entry_remains": true`) || strings.Contains(out, "grants_removed") {
		t.Fatalf("rm code=%d out=%q err=%q", code, out, errOut)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("key rm modified the keychain file")
	}

	code, out, errOut := runKeyTestCommand(t, "key", "add", "LEGACY", "--value", keychainSentinel, "--mode", "proxied", "--home", home)
	if code == 0 || !strings.Contains(errOut, "flag provided but not defined") {
		t.Fatalf("value flag unexpectedly accepted: code=%d out=%q err=%q", code, out, errOut)
	}
	usage := printKeyUsageString()
	if strings.Contains(usage, "value") || strings.Contains(usage, "grant") || strings.Contains(usage, "injected") {
		t.Fatalf("key command usage exposes a removed option: %s", usage)
	}
}

func TestKeyConfigureRejectsUnsafeProxyMetadata(t *testing.T) {
	t.Parallel()

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
	if err := withStore(home, func(store *db.Store) error {
		return store.ExecForTest(context.Background(), `INSERT INTO keychain_keys(name, mode) VALUES ('INJECTED', 'injected')`)
	}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runKeyTestCommand(t, "key", "configure", "INJECTED", "--upstream", "https://api.example.test", "--auth", "bearer", "--home", home); code == 0 || !strings.Contains(errOut, "requires proxied mode") {
		t.Fatalf("configure injected code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestKeychainFileValidation(t *testing.T) {
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
			home, _, store := daemonE2EHome(t)
			tt.setup(t, home, store)
			code, out, errOut := runKeyTestCommand(t, "key", "add", "TOKEN", "--mode", "proxied", "--home", home)
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
	home, _, store := daemonE2EHome(t)
	writeDefaultKeychain(t, home, "TOKEN="+keychainSentinel+"\n")
	original := keychain.CurrentUID
	keychain.CurrentUID = func() uint32 { return original() + 1 }
	t.Cleanup(func() { keychain.CurrentUID = original })
	_, _, err := keychain.LoadValidated(context.Background(), store, home)
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
