//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptionalSystemToolchainRoot(t *testing.T) {
	tests := []struct {
		name       string
		executable string
		want       string
	}{
		{name: "hosted Go", executable: "/opt/hostedtoolcache/go/1.26.0/x64/bin/go", want: "/opt/hostedtoolcache/go/1.26.0/x64"},
		{name: "local Go", executable: "/usr/local/go/bin/go", want: "/usr/local/go"},
		{name: "Nix Go", executable: "/nix/store/hash-go/bin/go", want: "/nix/store/hash-go"},
		{name: "root tool", executable: "/root/.local/bin/go"},
		{name: "home tool", executable: "/home/user/bin/go"},
		{name: "temporary tool", executable: "/tmp/toolchain/bin/go"},
		{name: "not a bin directory", executable: "/opt/toolchain/go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := optionalSystemToolchainRoot(test.executable); got != test.want {
				t.Fatalf("optionalSystemToolchainRoot(%q) = %q, want %q", test.executable, got, test.want)
			}
		})
	}
}

// TestOptionalSystemToolchainRootGrantsThePinnedOperatorToolchain is #1878's
// regression. A read-only review seat could not exec the toolchain this
// repository pins, so four `go` arms returned exit 126 and the verdict carried
// evidence=static_only: `go` was readable and resolvable but not executable,
// because the Landlock domain never received a grant for it.
//
// The path is built from the operator's real home rather than hardcoded, so
// this asserts the RULE on whatever account runs it instead of one box's
// layout.
func TestOptionalSystemToolchainRootGrantsThePinnedOperatorToolchain(t *testing.T) {
	home := operatorHomeDir()
	if home == "" || home == "/" {
		t.Skip("operator home is unavailable or /, so the depth rule has nothing to measure against")
	}
	root := filepath.Join(home, ".local", "toolchains", "go1.26.4")
	if got := optionalSystemToolchainRoot(filepath.Join(root, "bin", "go")); got != root {
		t.Fatalf("optionalSystemToolchainRoot = %q, want %q; a read-only seat cannot exec the pinned toolchain", got, root)
	}
}

// TestToolchainRootBelowHome pins the depth boundary in both directions,
// because a rule that only ever admits is not a rule. Depth one is the
// credential-directory depth (~/.gitmoot, ~/.codex, ~/.claude) and MUST be
// refused; depth two is the shallowest grantable root.
func TestToolchainRootBelowHome(t *testing.T) {
	tests := []struct {
		name string
		root string
		home string
		want bool
	}{
		{name: "home itself", root: "/root", home: "/root"},
		{name: "credential depth one", root: "/root/.gitmoot", home: "/root"},
		{name: "runtime credential depth one", root: "/root/.codex", home: "/root"},
		{name: "bin depth one", root: "/root/bin", home: "/root"},
		{name: "depth two", root: "/root/.local/toolchains", home: "/root", want: true},
		{name: "depth three", root: "/root/.local/toolchains/go1.26.4", home: "/root", want: true},
		{name: "outside home", root: "/tmp/toolchain", home: "/root"},
		{name: "parent of home", root: "/", home: "/root"},
		{name: "sibling prefix collision", root: "/rootless/a/b", home: "/root"},
		{name: "empty home", root: "/root/a/b", home: ""},
		{name: "empty root", root: "", home: "/root"},
		{name: "relative root", root: "a/b", home: "/root"},
		{name: "unclean path", root: "/root/.local/../.local/toolchains/go", home: "/root", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := toolchainRootBelowHome(test.root, test.home); got != test.want {
				t.Fatalf("toolchainRootBelowHome(%q, %q) = %v, want %v", test.root, test.home, got, test.want)
			}
		})
	}
}

// TestOperatorHomeDirIgnoresARewrittenHOME defends the property that makes the
// grant work AT ALL inside the process that needs it.
//
// MEASURED: a read-only seat rewrites HOME to its own throwaway cache root and
// sandbox-exec inherits that env. With HOME pointed at a scratch directory,
// os.UserHomeDir() returns the scratch path while user.Current().HomeDir still
// returns the operator's home. Written against os.UserHomeDir(), the depth rule
// would never match the operator's toolchain and this fix would be INERT in
// exactly the process it exists to serve - a green unit test and an unchanged
// exit 126 in production.
func TestOperatorHomeDirIgnoresARewrittenHOME(t *testing.T) {
	real := operatorHomeDir()
	if real == "" {
		t.Skip("passwd lookup is unavailable, so there is nothing to compare against")
	}
	seatHome := t.TempDir()
	t.Setenv("HOME", seatHome)
	if envHome, err := os.UserHomeDir(); err != nil || envHome != seatHome {
		t.Fatalf("os.UserHomeDir() = %q err=%v, want the rewritten %q; the premise of this test no longer holds", envHome, err, seatHome)
	}
	if got := operatorHomeDir(); got != real {
		t.Fatalf("operatorHomeDir() = %q under a rewritten HOME, want the operator's %q; the toolchain grant would be inert in a seat", got, real)
	}
}

// TestReadableRootsWithholdsOperatorCredentialDirectories is the containment
// half of #1878's fix. The depth rule bounds which root may be granted, never
// what that root contains, so this pins that no granted root is the operator's
// home, a withheld runtime credential directory, or an ancestor of home -
// asserted through readableRoots, the function execSandbox actually calls.
func TestReadableRootsWithholdsOperatorCredentialDirectories(t *testing.T) {
	home := operatorHomeDir()
	if home == "" || home == "/" {
		t.Skip("operator home is unavailable or /, so there is no credential boundary to assert")
	}
	roots, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
	if err != nil {
		t.Fatalf("readableRoots returned error: %v", err)
	}
	if len(roots) == 0 {
		t.Fatal("readableRoots returned no roots, so this assertion would pass vacuously")
	}
	withheld := []string{home}
	for _, dir := range []string{".gitmoot", ".codex", ".claude", ".config", ".ssh"} {
		withheld = append(withheld, filepath.Join(home, dir))
	}
	for _, root := range roots {
		clean := filepath.Clean(root)
		for _, deny := range withheld {
			if clean == deny {
				t.Fatalf("readableRoots granted %q, which a read-only seat must never read; roots=%v", clean, roots)
			}
		}
		if rel, relErr := filepath.Rel(clean, home); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("readableRoots granted %q, an ancestor of the operator home %q; roots=%v", clean, home, roots)
		}
	}
}

func TestRuntimeHostReadFilesIncludesOpenSSLConfig(t *testing.T) {
	for _, path := range runtimeHostReadFiles {
		if path == "/etc/ssl/openssl.cnf" {
			return
		}
	}
	t.Fatal("runtime host file grants omit /etc/ssl/openssl.cnf; Node-based review runtimes cannot initialize TLS")
}

// TestReadableRootsGrantsProcfs pins the runtime BOOTSTRAP grant that strict
// read-path mode dropped. Without it the Bun-based Claude/Kimi binaries abort
// and codex's bwrap cannot read /proc/sys/kernel/overflowuid, so every read-only
// review dies before doing any work. Asserted through readableRoots (the
// function execSandbox actually calls) rather than by inspecting a literal list.
func TestReadableRootsGrantsProcfs(t *testing.T) {
	roots, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
	if err != nil {
		t.Fatalf("readableRoots returned error: %v", err)
	}
	for _, root := range roots {
		if root == "/proc" {
			return
		}
	}
	t.Fatalf("readableRoots = %v, want /proc among them; review runtimes cannot bootstrap without procfs", roots)
}
