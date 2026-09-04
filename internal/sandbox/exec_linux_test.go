//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// makeGoroot builds a directory shaped like a real GOROOT: bin/go as a regular
// file plus src/runtime. The classifier stats the filesystem, so a table of
// bare strings cannot exercise it - that is why these are real trees.
func makeGoroot(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real GOROOT declares its release, and that file is the discriminator
	// that separates a toolchain from a GOPATH.
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "bin", "go")
}

// TestOptionalGoToolchainRootGrantsPinnedToolchains is #1839's guard: a
// toolchain outside the four previously allowlisted prefixes must be granted,
// because this repository pins one at /root/.local/toolchains/go1.26.4 and the
// old prefix list silently refused it - which is why every read-only review
// seat got EACCES on execve and no engine verdict could execute the gate.
func TestOptionalGoToolchainRootGrantsPinnedToolchains(t *testing.T) {
	base := t.TempDir()

	// The shape and placement of the pinned toolchain on this box, under a
	// parent that the old prefix list refused. It is grantable because it is
	// the toolchain the daemon itself was built against - the anchor that
	// replaced the basename rule, which would have trusted any go-named
	// directory including a user's home.
	pinned := filepath.Join(base, "root", ".local", "toolchains", "go1.26.4")
	oldOwn := daemonBuildGoroot
	daemonBuildGoroot = func() string { return pinned }
	t.Cleanup(func() { daemonBuildGoroot = oldOwn })
	if got := GoToolchainRoot(makeGoroot(t, pinned)); got != pinned {
		t.Fatalf("GoToolchainRoot(pinned) = %q, want %q: a pinned toolchain outside /opt|/usr/local|/nix/store|/snap must still be granted", got, pinned)
	}

	// A package-root install keeps working even though its leaf is not
	// go-named. The package-root list is pointed at the fixture tree, because
	// a fixture cannot live under a real /opt - and an arm no test can reach is
	// how #1839's defect survived review in the first place.
	previous := toolchainPackageRoots
	toolchainPackageRoots = []string{filepath.Join(base, "opt")}
	t.Cleanup(func() { toolchainPackageRoots = previous })
	hosted := filepath.Join(base, "opt", "hostedtoolcache", "go", "1.26.0", "x64")
	if got := GoToolchainRoot(makeGoroot(t, hosted)); got != hosted {
		t.Fatalf("GoToolchainRoot(hosted) = %q, want %q", got, hosted)
	}
}

// TestOptionalGoToolchainRootStillRefusesCredentialBearingParents keeps the
// original security intent, which the fix must not trade away: a
// user-controlled binary must not turn its credential-bearing parent into a
// readable subtree. Each of these is GOROOT-SHAPED on disk, so only the
// naming/placement rule can refuse them - the shape test alone would not.
func TestOptionalGoToolchainRootStillRefusesCredentialBearingParents(t *testing.T) {
	base := t.TempDir()
	for name, rel := range map[string][]string{
		"user local bin": {"root", ".local"},
		"home bin":       {"home", "user"},
		"temp toolchain": {"tmp", "toolchain"},
		"bare home":      {"home", "someone"},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(append([]string{base}, rel...)...)
			if got := GoToolchainRoot(makeGoroot(t, root)); got != "" {
				t.Fatalf("GoToolchainRoot(%q) = %q, want refusal: granting it would expose a credential-bearing parent", root, got)
			}
		})
	}
}

// TestOptionalGoToolchainRootRefusesThingsThatAreNotToolchains pins the shape
// half: a directory that merely holds a file named go is not a GOROOT, and a
// binary that is not in a bin/ directory is not a toolchain binary.
func TestOptionalGoToolchainRootRefusesThingsThatAreNotToolchains(t *testing.T) {
	base := t.TempDir()

	// go-named root, bin/go present, but no src/runtime: a plant, not a GOROOT.
	plant := filepath.Join(base, "opt", "golang-ish")
	if err := os.MkdirAll(filepath.Join(plant, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plant, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := GoToolchainRoot(filepath.Join(plant, "bin", "go")); got != "" {
		t.Fatalf("GoToolchainRoot(plant) = %q, want refusal: no src/runtime means it is not a toolchain", got)
	}

	// Not in a bin/ directory at all.
	loose := filepath.Join(base, "opt", "toolchain")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loose, "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := GoToolchainRoot(filepath.Join(loose, "go")); got != "" {
		t.Fatalf("GoToolchainRoot(loose) = %q, want refusal", got)
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
