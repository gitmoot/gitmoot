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

	// The reviewer's exact scenario in its own terms: a home carrying real
	// credential filenames, shaped as a complete GOROOT. Refusal must not
	// depend on what the user is CALLED - the old rule DENIED /home/mallory
	// and GRANTED /home/gordon, which was the finding.
	gordon := plantGoroot(t, filepath.Join(fakeHome, "gordon"))
	for rel, body := range map[string]string{
		".ssh/id_ed25519":  "PRIVATE KEY",
		".aws/credentials": "[default]",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(gordon, rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gordon, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"gordon", "mallory", "alice"} {
		root := plantGoroot(t, filepath.Join(fakeHome, name))
		if got := GoToolchainRoot(filepath.Join(root, "bin", "go")); got != "" {
			t.Fatalf("granted %q for home %q: refusal must not depend on the victim's NAME", got, name)
		}
	}

	// A home that sits INSIDE an anchor is the case only the home refusal can
	// answer: the anchor check passes, the shape is complete, and the name is
	// irrelevant. Without this the home rule is untested - a mutant deleting
	// it SURVIVED, because every other fixture here is already unanchored, so
	// the anchor was doing all the work. Homes under a package root are real
	// (an operator packaging homes beneath /usr/local, /opt/home on some
	// layouts), and this is what stops the guard from being unprovable.
	anchoredHome := filepath.Join(base, "opt", "home")
	if err := os.MkdirAll(anchoredHome, 0o755); err != nil {
		t.Fatal(err)
	}
	goToolchainHomeRoots = append(goToolchainHomeRoots, anchoredHome)
	insideAnchor := plantGoroot(t, filepath.Join(anchoredHome, "dave"))
	if got := GoToolchainRoot(filepath.Join(insideAnchor, "bin", "go")); got != "" {
		t.Fatalf("granted %q: a home INSIDE an anchor passes the anchor check, so only the home refusal can stop it", got)
	}

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

// TestAnchorIsNotEnvironmentControlled pins the P2 residual found by experiment
// on the round-2 head: runtime.GOROOT() returns the GOROOT ENVIRONMENT VARIABLE
// when set, so the same compiled binary could be made to report an
// attacker-named anchor. The arms mirror that measurement.
func TestAnchorIsNotEnvironmentControlled(t *testing.T) {
	t.Setenv("GOROOT", "")
	baseline := daemonBuildGoroot()
	if baseline == "" {
		t.Skip("no build toolchain to anchor against")
	}

	t.Run("empty_is_treated_as_absent", func(t *testing.T) {
		t.Setenv("GOROOT", "")
		if got := daemonBuildGoroot(); got != baseline {
			t.Fatalf("daemonBuildGoroot() = %q with an EMPTY GOROOT, want the build value %q", got, baseline)
		}
	})

	t.Run("hostile_value_fails_closed", func(t *testing.T) {
		hostile := filepath.Join(t.TempDir(), "evil-goroot")
		if err := os.MkdirAll(hostile, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOROOT", hostile)
		if got := daemonBuildGoroot(); got != "" {
			t.Fatalf("daemonBuildGoroot() = %q from the ENVIRONMENT; the anchor must be withheld, never attacker-supplied", got)
		}
	})

	t.Run("hostile_value_cannot_anchor_a_planted_tree", func(t *testing.T) {
		hostile := filepath.Join(t.TempDir(), "evil-goroot")
		planted := plantGoroot(t, hostile)
		t.Setenv("GOROOT", hostile)
		oldPkg := toolchainPackageRoots
		toolchainPackageRoots = []string{filepath.Join(t.TempDir(), "unrelated-opt")}
		t.Cleanup(func() { toolchainPackageRoots = oldPkg })
		if got := GoToolchainRoot(filepath.Join(planted, "bin", "go")); got != "" {
			t.Fatalf("granted %q: an environment-named GOROOT anchored a planted tree", got)
		}
	})
}
