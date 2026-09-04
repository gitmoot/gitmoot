//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// plantGoroot builds a COMPLETE, convincing GOROOT: executable bin/go,
// src/runtime, and a parseable VERSION. Everything a shape check can see.
func plantGoroot(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestToolchainGrantRefusesPlantedAndUserControlledRoots pins the three holes
// that review raised against earlier forms of this fix. Each fixture is a
// COMPLETE toolchain by shape, so only the trust boundary can refuse it.
func TestToolchainGrantRefusesPlantedAndUserControlledRoots(t *testing.T) {
	base := t.TempDir()
	fakeHome := filepath.Join(base, "home")
	fakeRootHome := filepath.Join(base, "root")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// Point the home and anchor lists at fixtures: the real /home and /opt
	// cannot hold a test tree, and an untestable rule is how #1839 survived.
	old, oldPkg := goToolchainHomeRoots, toolchainPackageRoots
	goToolchainHomeRoots = []string{fakeRootHome, fakeHome}
	toolchainPackageRoots = []string{filepath.Join(base, "opt")}
	oldOwn := daemonBuildGoroot
	daemonBuildGoroot = func() string { return filepath.Join(base, "pinned", "go1.26.4") }
	t.Cleanup(func() {
		goToolchainHomeRoots, toolchainPackageRoots, daemonBuildGoroot = old, oldPkg, oldOwn
	})

	refused := map[string]string{
		// A go-NAMED home directory: the hole the basename rule opened.
		"go_named_home":     plantGoroot(t, filepath.Join(fakeHome, "gordon")),
		"gopath_under_root": plantGoroot(t, filepath.Join(fakeRootHome, "gopath")),
		"bare_root_home":    plantGoroot(t, fakeRootHome),
		// A complete tree PLANTED somewhere writable and not a home: the hole
		// that shape-plus-home-refusal alone would have left open.
		"planted_in_tmp":     plantGoroot(t, filepath.Join(base, "tmp", "go1.26.4")),
		"planted_unanchored": plantGoroot(t, filepath.Join(base, "srv", "go")),
	}
	for name, root := range refused {
		t.Run("refused/"+name, func(t *testing.T) {
			if got := GoToolchainRoot(filepath.Join(root, "bin", "go")); got != "" {
				t.Fatalf("granted %q for a root that is user-controlled or unanchored; a complete shape must not be sufficient", got)
			}
		})
	}

	granted := map[string]string{
		// The live #1839 case: an operator-pinned install outside every
		// package root, anchored by being the daemon's own build toolchain.
		"pinned_operator_install": plantGoroot(t, filepath.Join(base, "pinned", "go1.26.4")),
		// A version-suffixed package install, as Debian and hostedtoolcache ship.
		"package_root_install": plantGoroot(t, filepath.Join(base, "opt", "hostedtoolcache", "go", "1.26.0", "x64")),
	}
	for name, root := range granted {
		t.Run("granted/"+name, func(t *testing.T) {
			if got := GoToolchainRoot(filepath.Join(root, "bin", "go")); got != root {
				t.Fatalf("GoToolchainRoot = %q, want %q: a legitimately anchored toolchain must still be granted", got, root)
			}
		})
	}
}

// TestToolchainGrantNeedsARealToolchainShape pins the shape half, including the
// executable bit - a readable file named go is not a toolchain, and the
// executable bit is the whole point of the grant.
func TestToolchainGrantNeedsARealToolchainShape(t *testing.T) {
	base := t.TempDir()
	oldPkg := toolchainPackageRoots
	toolchainPackageRoots = []string{base}
	t.Cleanup(func() { toolchainPackageRoots = oldPkg })

	t.Run("non_executable_go", func(t *testing.T) {
		root := plantGoroot(t, filepath.Join(base, "noexec"))
		if err := os.Chmod(filepath.Join(root, "bin", "go"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := GoToolchainRoot(filepath.Join(root, "bin", "go")); got != "" {
			t.Fatalf("granted %q for a NON-EXECUTABLE bin/go", got)
		}
	})
	t.Run("missing_version", func(t *testing.T) {
		root := plantGoroot(t, filepath.Join(base, "noversion"))
		if err := os.Remove(filepath.Join(root, "VERSION")); err != nil {
			t.Fatal(err)
		}
		if got := GoToolchainRoot(filepath.Join(root, "bin", "go")); got != "" {
			t.Fatalf("granted %q with no VERSION: this is the shape a GOPATH has", got)
		}
	})
	t.Run("version_is_not_a_go_release", func(t *testing.T) {
		root := plantGoroot(t, filepath.Join(base, "badversion"))
		if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("gopher\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := GoToolchainRoot(filepath.Join(root, "bin", "go")); got != "" {
			t.Fatalf("granted %q for a VERSION that does not name a release", got)
		}
	})
	t.Run("real_shape_is_granted", func(t *testing.T) {
		root := plantGoroot(t, filepath.Join(base, "go1.26.4"))
		if got := GoToolchainRoot(filepath.Join(root, "bin", "go")); got != root {
			t.Fatalf("GoToolchainRoot = %q, want %q", got, root)
		}
	})
}

// TestLiveToolchainOnThisHostIsGranted is the anti-inert check: the fix exists
// for the toolchain this box actually pins, so it is asserted against the real
// installation rather than only against fixtures.
func TestLiveToolchainOnThisHostIsGranted(t *testing.T) {
	live := filepath.Join(daemonBuildGoroot(), "bin", "go")
	if _, err := os.Stat(live); err != nil {
		t.Skipf("no build toolchain on disk at %s", live)
	}
	if got := GoToolchainRoot(live); got != filepath.Clean(daemonBuildGoroot()) {
		t.Fatalf("GoToolchainRoot(%q) = %q, want the daemon's own build GOROOT %q: the #1839 defect is back",
			live, got, daemonBuildGoroot())
	}
}
