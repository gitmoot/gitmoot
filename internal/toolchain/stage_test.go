package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// goInstall builds a minimal but REAL installation: the signature is a content
// proof, so a fixture that only names paths would assert nothing about it.
func goInstall(t *testing.T, root string, members ...string) string {
	t.Helper()
	if len(members) == 0 {
		members = []string{"bin", "pkg", "src", "lib"}
	}
	for _, member := range members {
		if err := os.MkdirAll(filepath.Join(root, member), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, member, "marker"), []byte(member), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\necho go\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.4\ntime 2026-05-29T15:26:39Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.env"), []byte("GOTOOLCHAIN=local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestIdentifyKeysOnContentNotVersionAlone(t *testing.T) {
	base := t.TempDir()
	first := goInstall(t, filepath.Join(base, "a"))
	second := goInstall(t, filepath.Join(base, "b"))

	one, err := Identify(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Identify(second)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("identical installations produced different identities %v and %v", one, two)
	}
	if one.Version != "go1.26.4" {
		t.Fatalf("Version = %q, want go1.26.4", one.Version)
	}

	// A PATCHED REBUILD AT THE SAME VERSION: the version string is unchanged and
	// the identity must still differ, or a stale copy would be served.
	if err := os.WriteFile(filepath.Join(second, "bin", "go"), []byte("#!/bin/sh\necho patched\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	patched, err := Identify(second)
	if err != nil {
		t.Fatal(err)
	}
	if patched.Version != one.Version {
		t.Fatalf("precondition failed: versions differ (%q vs %q), so this would not test the fingerprint", patched.Version, one.Version)
	}
	if patched == one {
		t.Fatal("a patched rebuild at the same VERSION produced the same identity; a path-or-version key would serve a stale compiler")
	}
}

func TestIdentifyRefusesNonInstallations(t *testing.T) {
	base := t.TempDir()

	noBin := filepath.Join(base, "no-bin")
	if err := os.MkdirAll(noBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noBin, "VERSION"), []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	noVersion := goInstall(t, filepath.Join(base, "no-version"))
	if err := os.Remove(filepath.Join(noVersion, "VERSION")); err != nil {
		t.Fatal(err)
	}

	wrongVersion := goInstall(t, filepath.Join(base, "wrong-version"))
	if err := os.WriteFile(filepath.Join(wrongVersion, "VERSION"), []byte("1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	notExecutable := goInstall(t, filepath.Join(base, "not-exec"))
	if err := os.Chmod(filepath.Join(notExecutable, "bin", "go"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A SYMLINKED VERSION: without O_NOFOLLOW the check reads an unrelated file
	// and a non-installation passes the signature.
	symlinkedVersion := goInstall(t, filepath.Join(base, "symlinked-version"))
	elsewhere := filepath.Join(base, "elsewhere-version")
	if err := os.WriteFile(elsewhere, []byte("go1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(symlinkedVersion, "VERSION")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(symlinkedVersion, "VERSION")); err != nil {
		t.Fatal(err)
	}

	symlinkedGo := goInstall(t, filepath.Join(base, "symlinked-go"))
	realGo := filepath.Join(base, "elsewhere-go")
	if err := os.WriteFile(realGo, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(symlinkedGo, "bin", "go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realGo, filepath.Join(symlinkedGo, "bin", "go")); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		source string
		want   error
	}{
		{name: "no bin/go", source: noBin, want: ErrNotPinned},
		{name: "no VERSION", source: noVersion, want: ErrNotPinned},
		{name: "VERSION does not name Go", source: wrongVersion, want: ErrNotPinned},
		{name: "bin/go not executable", source: notExecutable, want: ErrNotPinned},
		{name: "bin/go is a symlink", source: symlinkedGo, want: ErrSymlink},
		{name: "VERSION is a symlink to a valid version string", source: symlinkedVersion, want: ErrNotPinned},
		{name: "relative path", source: "relative/path", want: ErrNotPinned},
		{name: "empty path", source: "", want: ErrNotPinned},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Identify(test.source); !errors.Is(err, test.want) {
				t.Fatalf("Identify(%q) error = %v, want %v", test.source, err, test.want)
			}
		})
	}
}

// TestReadVersionRefusesAFifoWithoutBlocking pins the availability half: opening
// a FIFO with no writer before proving its type would BLOCK staging, which is a
// defect in the launch path rather than a leak.
func TestReadVersionRefusesAFifoWithoutBlocking(t *testing.T) {
	root := goInstall(t, filepath.Join(t.TempDir(), "fifo"))
	if err := os.Remove(filepath.Join(root, "VERSION")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "VERSION"), 0o644); err != nil {
		t.Skipf("cannot create a FIFO fixture: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Identify(root)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotPinned) {
			t.Fatalf("Identify with a FIFO VERSION = %v, want ErrNotPinned", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Identify BLOCKED on a FIFO with no writer; staging would hang")
	}
}

// TestStageRefusesASymlinkAnywhereInTheTree is the back door this shape must
// keep shut: a copier that follows a symlink copies something outside the tree
// it validated, and the outside-root problem returns wearing a copy's clothes.
func TestStageRefusesASymlinkAnywhereInTheTree(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "id_ed25519"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}

	// CONTROL: the same tree without the symlink stages successfully, so a
	// refusal below is about the symlink rather than about the fixture.
	clean := goInstall(t, filepath.Join(base, "clean"))
	if _, err := Stage(home, clean); err != nil {
		t.Fatalf("control failed: a clean tree must stage: %v", err)
	}

	for _, test := range []struct{ name, member string }{
		{name: "symlinked optional member", member: "src"},
		{name: "symlinked required member", member: "bin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := goInstall(t, filepath.Join(base, "src-"+test.member))
			if err := os.RemoveAll(filepath.Join(source, test.member)); err != nil {
				t.Fatal(err)
			}
			if test.member == "bin" {
				// keep the signature satisfiable through the link so the refusal
				// is about the symlink rather than a missing compiler
				if err := os.MkdirAll(filepath.Join(outside, "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(outside, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "bin"), filepath.Join(source, "bin")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(outside, filepath.Join(source, test.member)); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Stage(home, source)
			if err == nil {
				t.Fatal("a symlinked member staged successfully; the outside-root problem returns through it")
			}
			if !errors.Is(err, ErrSymlink) && !errors.Is(err, ErrNotPinned) {
				t.Fatalf("Stage error = %v, want a symlink or not-pinned refusal", err)
			}
			// and nothing may be published from a refused stage
			entries, _ := os.ReadDir(Root(home))
			for _, entry := range entries {
				if strings.Contains(entry.Name(), "staging") {
					t.Fatalf("a refused stage left a temporary tree behind: %s", entry.Name())
				}
			}
		})
	}
}

// TestStageSkipsAbsentOptionalMembersButRefusesOperationalFailure is the
// discriminating pair: absence and operational failure are different facts and
// only the first is benign.
func TestStageSkipsAbsentOptionalMembersButRefusesOperationalFailure(t *testing.T) {
	base := t.TempDir()

	minimal := goInstall(t, filepath.Join(base, "minimal"), "bin")
	staged, err := Stage(filepath.Join(base, "home-minimal"), minimal)
	if err != nil {
		t.Fatalf("a minimal layout must stage, absent optional members skip: %v", err)
	}
	for _, absent := range []string{"pkg", "src", "lib", "api", "misc", "test", "doc"} {
		if _, err := os.Stat(filepath.Join(staged, absent)); err == nil {
			t.Fatalf("absent member %q was materialised", absent)
		}
	}
	for _, present := range []string{"bin/go", "VERSION", "go.env"} {
		if _, err := os.Stat(filepath.Join(staged, present)); err != nil {
			t.Fatalf("required content %q missing from the staged copy: %v", present, err)
		}
	}

	// OPERATIONAL FAILURE on the same member that was benign when absent.
	broken := goInstall(t, filepath.Join(base, "broken"), "bin")
	if err := os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(broken, "doc")); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(filepath.Join(base, "home-broken"), broken); err == nil {
		t.Fatal("a symlinked optional member was skipped; only ABSENCE may skip")
	}
}

// TestStagePublishesAtomicallyAndLeavesNoPartialTree pins the publish protocol.
func TestStagePublishesAtomicallyAndLeavesNoPartialTree(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	source := goInstall(t, filepath.Join(base, "src"))

	staged, err := Stage(home, source)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := Identify(source)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(staged) != identity.String() {
		t.Fatalf("published at %q, want the identity directory %q", filepath.Base(staged), identity.String())
	}
	entries, err := os.ReadDir(Root(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != identity.String() {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("staged root holds %v, want exactly the published identity; a temporary tree must not survive", names)
	}

	// idempotent: a second stage returns the same path and publishes nothing new
	again, err := Stage(home, source)
	if err != nil || again != staged {
		t.Fatalf("second Stage = %q err=%v, want the same published path", again, err)
	}
}

// TestStageIsSingleFlightPerIdentity pins that concurrent jobs on one version do
// not race to publish the same tree.
func TestStageIsSingleFlightPerIdentity(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	source := goInstall(t, filepath.Join(base, "src"))

	const callers = 8
	var wait sync.WaitGroup
	results := make([]string, callers)
	failures := make([]error, callers)
	wait.Add(callers)
	for i := 0; i < callers; i++ {
		go func(index int) {
			defer wait.Done()
			results[index], failures[index] = Stage(home, source)
		}(i)
	}
	wait.Wait()
	for i, err := range failures {
		if err != nil {
			t.Fatalf("caller %d failed: %v", i, err)
		}
		if results[i] != results[0] {
			t.Fatalf("caller %d published %q, caller 0 published %q", i, results[i], results[0])
		}
	}
	entries, err := os.ReadDir(Root(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("staged root holds %d entries after concurrent stages, want 1", len(entries))
	}
}

// TestStagePreservesExecBitsAndStripsSetuid pins the permission contract.
func TestStagePreservesExecBitsAndStripsSetuid(t *testing.T) {
	base := t.TempDir()
	source := goInstall(t, filepath.Join(base, "src"))
	// os.ModeSetuid, NOT octal 0o4000: a FileMode's setuid is bit 1<<23, so
	// os.Chmod(f, 0o4755) sets 0755 and silently drops the setuid request. This
	// arm asserted nothing until the mutant that should have died survived.
	goBinary := filepath.Join(source, "bin", "go")
	if err := os.Chmod(goBinary, 0o755|os.ModeSetuid); err != nil {
		t.Skipf("cannot set setuid on the fixture: %v", err)
	}
	if before, err := os.Stat(goBinary); err != nil || before.Mode()&os.ModeSetuid == 0 {
		t.Skipf("fixture precondition: setuid did not stick on this filesystem (mode %v)", before.Mode())
	}
	staged, err := Stage(filepath.Join(base, "home"), source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(staged, "bin", "go"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("staged bin/go mode %v retains setuid", info.Mode())
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("staged bin/go mode %v lost its executable bit", info.Mode())
	}
}

// TestCollectKeepsCurrentPinsAndRemovesTheRest pins retention by CURRENT PIN
// rather than by age or count, and that leftovers from interrupted stages go.
func TestCollectKeepsCurrentPinsAndRemovesTheRest(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	current := goInstall(t, filepath.Join(base, "current"))
	stale := goInstall(t, filepath.Join(base, "stale"))
	if err := os.WriteFile(filepath.Join(stale, "bin", "go"), []byte("#!/bin/sh\necho other\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	currentPath, err := Stage(home, current)
	if err != nil {
		t.Fatal(err)
	}
	stalePath, err := Stage(home, stale)
	if err != nil {
		t.Fatal(err)
	}
	if currentPath == stalePath {
		t.Fatal("precondition failed: the two fixtures share an identity")
	}
	leftover := filepath.Join(Root(home), ".staging-interrupted")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}

	keep, err := Identify(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := Collect(home, []Identity{keep}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("the current pin was collected: %v", err)
	}
	if _, err := os.Stat(stalePath); err == nil {
		t.Fatal("an unreferenced copy survived collection")
	}
	if _, err := os.Stat(leftover); err == nil {
		t.Fatal("an interrupted stage's temporary tree survived collection")
	}
}

// TestMinFreeBytesIsAStatedFloor pins that the refusal threshold is a NUMBER in
// the code rather than whatever the disk happens to be at that minute.
func TestMinFreeBytesIsAStatedFloor(t *testing.T) {
	if MinFreeBytes < 1<<30 {
		t.Fatalf("MinFreeBytes = %d, want an explicit floor of at least 1 GiB", MinFreeBytes)
	}
	// and the refusal names the shortfall, so the failure is actionable
	err := checkFreeSpace(t.TempDir())
	if err != nil && !strings.Contains(err.Error(), "floor is") {
		t.Fatalf("checkFreeSpace error %q does not name the floor", err)
	}
}
