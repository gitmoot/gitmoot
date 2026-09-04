package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
)

// keychain.env is a credential-bearing overlay, and its path is OPERATOR INPUT.
// A relative credentials.keychain_path resolves against whatever directory the
// process runs in - for a pipeline stage that is the checkout - so the file is
// read, and can then be staged, from inside the work tree. That is the class
// that put a live token in a public repo (#1810), reached through a different
// door than runtime-auth.env.
func TestResolveKeychainPathRefusesARelativeKeychain(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	write := func(t *testing.T, body string) {
		t.Helper()
		if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A relative CONFIGURED path is refused by the config loader itself. This is
	// pre-existing validation, asserted here so the boundary is recorded rather
	// than duplicated in the resolver as unreachable code.
	for _, relative := range []string{"keychain.env", "./keychain.env", "config/keychain.env"} {
		write(t, "\n[credentials]\nkeychain_path = \""+relative+"\"\n")
		got, err := ResolveKeychainPath(nil, home)
		if err == nil {
			t.Fatalf("keychain_path %q was accepted and resolved to %q", relative, got)
		}
		if !strings.Contains(err.Error(), "must be absolute") {
			t.Fatalf("error for %q = %v, want the loader's absolute-path rejection", relative, err)
		}
	}

	// The DEFAULT path is derived from paths.Home, which nothing else validates,
	// and that branch is this change's own guard. Reaching it needs a relative
	// home that actually EXISTS - otherwise the config load fails first and the
	// test would pass without the guard ever running, which is how the first
	// version of this assertion fooled me.
	t.Run("relative home resolves inside the working directory", func(t *testing.T) {
		t.Chdir(t.TempDir())
		relPaths := config.PathsForHome("home")
		if err := config.Initialize(relPaths); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(relPaths.ConfigFile, []byte(config.DefaultConfig(relPaths)), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveKeychainPath(nil, "home")
		if err == nil {
			t.Fatalf("a relative home resolved keychain.env to %q; a credential must never resolve inside the working directory", got)
		}
		if !strings.Contains(err.Error(), "relative") {
			t.Fatalf("relative-home error = %v, want it to name the relative path", err)
		}
	})

	// An absolute configured path still works, and so does the default.
	absolute := filepath.Join(t.TempDir(), "keychain.env")
	write(t, "\n[credentials]\nkeychain_path = \""+absolute+"\"\n")
	got, err := ResolveKeychainPath(nil, home)
	if err != nil || got != absolute {
		t.Fatalf("absolute keychain_path = %q err=%v, want %q", got, err, absolute)
	}
	write(t, "")
	got, err = ResolveKeychainPath(nil, home)
	if err != nil {
		t.Fatalf("default keychain resolution: %v", err)
	}
	if !filepath.IsAbs(got) || filepath.Base(got) != "keychain.env" {
		t.Fatalf("default keychain path = %q, want an absolute path ending in keychain.env", got)
	}
}
