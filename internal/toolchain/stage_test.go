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

// goInstall builds a minimal but REAL installation. The signature is a content
// proof, so a fixture that only named paths would assert nothing about it.
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
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
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

// withCompiler adds an executable under pkg/tool, which is where the ACTUAL
// compiler lives. The identity must cover it.
func withCompiler(t *testing.T, root, bytes string) string {
	t.Helper()
	dir := filepath.Join(root, "pkg", "tool", "linux_amd64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compile"), []byte(bytes), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func escapesRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, filepath.Clean(candidate))
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestVersionTraversalCannotEscapeTheDaemonRoot is the P1 regression.
//
// REPRODUCED BEFORE IT WAS FIXED: at head 65bdda18 a VERSION of "go/../../escaped"
// was embedded verbatim in the published name and Stage published OUTSIDE the
// daemon-owned root. The pair here is deliberate: a positive control proves a
// legitimate VERSION still stages, so a passing test cannot mean "everything is
// refused".
func TestVersionTraversalCannotEscapeTheDaemonRoot(t *testing.T) {
	for _, test := range []struct {
		name     string
		version  string
		accepted bool
	}{
		{name: "CONTROL a real version stages", version: "go1.26.4\n", accepted: true},
		{name: "CONTROL a release candidate stages", version: "go1.27rc1\n", accepted: true},
		{name: "parent traversal", version: "go/../../escaped\n"},
		{name: "separator only", version: "go/escaped\n"},
		{name: "absolute path", version: "go//etc/passwd\n"},
		{name: "dot dot suffix", version: "go..\n"},
		{name: "NUL byte", version: "go1.26\x004\n"},
		{name: "does not name go", version: "1.26.4\n"},
		{name: "over length", version: "go" + strings.Repeat("9", maxVersionLength) + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "home")
			source := goInstall(t, filepath.Join(base, "src"))
			if err := os.WriteFile(filepath.Join(source, "VERSION"), []byte(test.version), 0o644); err != nil {
				t.Fatal(err)
			}
			staged, err := Stage(home, source)
			if test.accepted {
				if err != nil {
					t.Fatalf("Stage(%q) refused a legitimate version: %v", test.version, err)
				}
				if escapesRoot(Root(home), staged) {
					t.Fatalf("even the control published outside the root at %q", staged)
				}
				return
			}
			if err == nil {
				t.Fatalf("Stage(%q) succeeded at %q; an untrusted VERSION must not name the publish path", test.version, staged)
			}
			if escapesRoot(Root(home), staged) && staged != "" {
				t.Fatalf("published OUTSIDE the daemon root at %q", staged)
			}
			// and nothing may exist outside the root either way
			if _, statErr := os.Stat(filepath.Join(base, "home", "escaped")); statErr == nil {
				t.Fatal("an entry was created outside the staging root")
			}
		})
	}
}

// TestPublishedSymlinkIsRefusedAndReplaced is the second P1 regression.
//
// REPRODUCED AT 65bdda18 on a clean worktree: stage() used os.Stat, so a symlink
// planted at the published name was accepted as a valid staged copy and the
// external target was readable through the staged path. PROVENANCE, settled at
// that head rather than assumed: internal/toolchain is ABSENT at origin/main and
// at the merge base and was added by this PR's own commit, so the defect is this
// PR's rather than pre-existing.
func TestPublishedSymlinkIsRefusedAndReplaced(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	source := goInstall(t, filepath.Join(base, "src"))
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "SECRET"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := Identify(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(Root(home), 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(Root(home), identity.String())
	if err := os.Symlink(outside, planted); err != nil {
		t.Fatal(err)
	}

	staged, err := Stage(home, source)
	if err != nil {
		t.Fatalf("Stage must replace the planted link rather than fail: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(staged, "SECRET")); statErr == nil {
		t.Fatal("the planted symlink was followed; the external target is readable through the staged path")
	}
	info, err := os.Lstat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the staged path is still a symlink")
	}
	if !info.IsDir() {
		t.Fatalf("staged path is not a directory, mode %v", info.Mode())
	}
	if _, statErr := os.Stat(filepath.Join(staged, "bin", "go")); statErr != nil {
		t.Fatalf("the replacement copy is not a real installation: %v", statErr)
	}
	// the external directory itself must be untouched
	if _, statErr := os.Stat(filepath.Join(outside, "SECRET")); statErr != nil {
		t.Fatalf("replacing the link damaged the external target: %v", statErr)
	}
}

// TestIdentityCoversEveryCopiedExecutable is the P2 regression.
//
// REPRODUCED: the identity hashed VERSION plus bin/go, so two sources agreeing on
// those but differing in pkg/tool/linux_amd64/compile - the ACTUAL compiler -
// produced the same identity, and the second source was served the first's
// compiler from cache.
func TestIdentityCoversEveryCopiedExecutable(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	clean := withCompiler(t, goInstall(t, filepath.Join(base, "clean")), "CLEAN-COMPILER")
	poisoned := withCompiler(t, goInstall(t, filepath.Join(base, "poisoned")), "POISONED-COMPILER")

	one, err := Identify(clean)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Identify(poisoned)
	if err != nil {
		t.Fatal(err)
	}
	if one.Version != two.Version {
		t.Fatalf("precondition failed: versions differ (%q vs %q), so this would not test the fingerprint", one.Version, two.Version)
	}
	if one == two {
		t.Fatal("two installations differing only in pkg/tool share an identity; the key omits the real compiler")
	}

	first, err := Stage(home, clean)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Stage(home, poisoned)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both sources published at the same path %q", first)
	}
	for path, want := range map[string]string{first: "CLEAN-COMPILER", second: "POISONED-COMPILER"} {
		served, readErr := os.ReadFile(filepath.Join(path, "pkg", "tool", "linux_amd64", "compile"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(served) != want {
			t.Fatalf("staged copy %q serves compiler %q, want %q", filepath.Base(path), served, want)
		}
	}

	// CONTROL: identical sources DO share, so the discrimination above is about
	// content rather than about never reusing anything.
	twin := withCompiler(t, goInstall(t, filepath.Join(base, "twin")), "CLEAN-COMPILER")
	again, err := Stage(home, twin)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("an identical source published separately at %q; reuse is broken", again)
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

	notExecutable := goInstall(t, filepath.Join(base, "not-exec"))
	if err := os.Chmod(filepath.Join(notExecutable, "bin", "go"), 0o644); err != nil {
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

	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "no bin/go", source: noBin},
		{name: "no VERSION", source: noVersion},
		{name: "bin/go not executable", source: notExecutable},
		{name: "bin/go is a symlink out of the tree", source: symlinkedGo},
		{name: "VERSION is a symlink out of the tree", source: symlinkedVersion},
		{name: "relative path", source: "relative/path"},
		{name: "empty path", source: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Identify(test.source); err == nil {
				t.Fatalf("Identify(%q) succeeded; it is not a pinned installation", test.source)
			}
		})
	}

	// CONTROL: a real installation IS identified, so the refusals above are about
	// the fixtures rather than about refusing everything.
	if _, err := Identify(goInstall(t, filepath.Join(base, "good"))); err != nil {
		t.Fatalf("control failed: a real installation must identify: %v", err)
	}
}

// TestReadVersionRefusesAFifoWithoutBlocking pins the availability half: opening a
// FIFO with no writer before proving its type would BLOCK seat launch, which is a
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
		if err == nil {
			t.Fatal("a FIFO VERSION was accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Identify BLOCKED on a FIFO with no writer; seat launch would hang")
	}
}

// TestStageRefusesASymlinkAnywhereInTheTree is the back door the copy shape must
// keep shut: a copier that follows a link copies something outside the tree it
// validated, and the outside-root problem returns wearing a copy's clothes.
func TestStageRefusesASymlinkAnywhereInTheTree(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "id_ed25519"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}

	// CONTROL first: the same shape without the symlink stages successfully.
	if _, err := Stage(filepath.Join(base, "home-control"), goInstall(t, filepath.Join(base, "clean"))); err != nil {
		t.Fatalf("control failed: a clean tree must stage: %v", err)
	}

	for _, member := range []string{"src", "doc", "pkg"} {
		t.Run("symlinked "+member, func(t *testing.T) {
			source := goInstall(t, filepath.Join(base, "src-"+member))
			if err := os.RemoveAll(filepath.Join(source, member)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(source, member)); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(base, "home-"+member)
			if _, err := Stage(home, source); err == nil {
				t.Fatal("a symlinked member staged successfully; the outside-root problem returns through it")
			}
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
	if filepath.Dir(staged) != Root(home) {
		t.Fatalf("published at %q, want it under %q", staged, Root(home))
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
		t.Fatalf("staged root holds %v, want exactly the published identity", names)
	}

	again, err := Stage(home, source)
	if err != nil || again != staged {
		t.Fatalf("second Stage = %q err=%v, want the same published path", again, err)
	}
}

// TestStageRestagesACorruptedPublishedCopy pins the durability mechanism that
// REPLACED per-file fsync, which cost 136.18s against 1.07s on 15022 files.
func TestStageRestagesACorruptedPublishedCopy(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	source := goInstall(t, filepath.Join(base, "src"))

	staged, err := Stage(home, source)
	if err != nil {
		t.Fatal(err)
	}
	// simulate what a crash after rename leaves behind: the name is durable, the
	// data is not.
	if err := os.WriteFile(filepath.Join(staged, "bin", "go"), []byte(""), 0o755); err != nil {
		t.Fatal(err)
	}
	restaged, err := Stage(home, source)
	if err != nil {
		t.Fatalf("a corrupted copy must be restaged, not served: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(restaged, "bin", "go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Fatal("the truncated bin/go was served as if correct; verification did not run")
	}
}

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

// TestStagePreservesExecBitsAndStripsSetuidAndSetgid covers BOTH bits.
//
// A setgid-only mutant survived the reviewer's run because this test checked
// ModeSetuid alone. And the fixture itself was vacuous before that: os.Chmod(f,
// 0o4755) does not set setuid in Go, because a FileMode's setuid is bit 1<<23
// rather than octal 0o4000, so the assertion could not fail. Both are fixed, and
// the fixture now asserts its own precondition.
func TestStagePreservesExecBitsAndStripsSetuidAndSetgid(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
		bit  os.FileMode
	}{
		{name: "setuid", mode: 0o755 | os.ModeSetuid, bit: os.ModeSetuid},
		{name: "setgid", mode: 0o755 | os.ModeSetgid, bit: os.ModeSetgid},
		{name: "both", mode: 0o755 | os.ModeSetuid | os.ModeSetgid, bit: os.ModeSetuid | os.ModeSetgid},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			source := goInstall(t, filepath.Join(base, "src"))
			goBinary := filepath.Join(source, "bin", "go")
			if err := os.Chmod(goBinary, test.mode); err != nil {
				t.Skipf("cannot set %s on the fixture: %v", test.name, err)
			}
			before, err := os.Stat(goBinary)
			if err != nil || before.Mode()&test.bit == 0 {
				t.Skipf("fixture precondition: %s did not stick on this filesystem (mode %v)", test.name, before.Mode())
			}
			staged, err := Stage(filepath.Join(base, "home"), source)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(staged, "bin", "go"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&test.bit != 0 {
				t.Fatalf("staged bin/go mode %v retains %s", info.Mode(), test.name)
			}
			if info.Mode().Perm()&0o111 == 0 {
				t.Fatalf("staged bin/go mode %v lost its executable bit", info.Mode())
			}
		})
	}
}

func TestCollectKeepsCurrentPinsAndRemovesTheRest(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	current := withCompiler(t, goInstall(t, filepath.Join(base, "current")), "CURRENT")
	stale := withCompiler(t, goInstall(t, filepath.Join(base, "stale")), "STALE")

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

func TestMinFreeBytesIsAStatedFloor(t *testing.T) {
	if MinFreeBytes < 1<<30 {
		t.Fatalf("MinFreeBytes = %d, want an explicit floor of at least 1 GiB", MinFreeBytes)
	}

	// FORCING, NOT CONDITIONAL. The previous version asserted only inside
	// `if err != nil`, so on any host above the floor the refusal branch never
	// ran and the test would have passed even if the check always returned nil.
	// Driving the predicate directly reaches both arms on a host of any size.
	for _, test := range []struct {
		name    string
		free    uint64
		floor   uint64
		refused bool
	}{
		{name: "below the floor is refused", free: 1 << 20, floor: 4 << 30, refused: true},
		{name: "one byte below is refused", free: (4 << 30) - 1, floor: 4 << 30, refused: true},
		{name: "empty disk is refused", free: 0, floor: 4 << 30, refused: true},
		{name: "CONTROL exactly at the floor is allowed", free: 4 << 30, floor: 4 << 30},
		{name: "CONTROL far above the floor is allowed", free: 900 << 30, floor: 4 << 30},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := enoughFree(test.free, test.floor, "/staged")
			if test.refused {
				if err == nil {
					t.Fatalf("enoughFree(%d, %d) allowed a copy that cannot fit", test.free, test.floor)
				}
				if !errors.Is(err, ErrLowDisk) {
					t.Fatalf("error = %v, want ErrLowDisk", err)
				}
				// the refusal must name BOTH numbers, or an operator cannot act
				for _, want := range []string{"floor is", "/staged"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("enoughFree(%d, %d) refused a copy that fits: %v", test.free, test.floor, err)
			}
		})
	}

	// and the production wiring still reaches the predicate
	if err := checkFreeSpace(t.TempDir()); err != nil && !errors.Is(err, ErrLowDisk) {
		t.Fatalf("checkFreeSpace returned an unexpected error: %v", err)
	}
}

// TestInternalSymlinksAreRefusedNotFollowed covers the layer os.Root does NOT.
//
// os.Root refuses a symlink that ESCAPES the root, which is why every
// outside-aiming fixture in this file passes whether or not the inner guards are
// present. It deliberately FOLLOWS links that stay inside the root, so an
// in-root link needs a fixture that stays inside to be tested at all. Three
// mutants survived until these existed.
//
// AN EARLIER VERSION OF THIS COMMENT NAMED THE WRONG GUARDS. It said "the
// O_NOFOLLOW opens and the directory-entry type check are the only things
// refusing an internal link". Both of those are gone at this head: the
// directory-entry check was deleted as redundant, and the caller-supplied
// O_NOFOLLOW bits were deleted as INERT, because os.Root ORs that flag itself at
// every openat call site. THE LOAD-BEARING GUARD IS openVerified, which refuses a
// symlinked name by Lstat and then proves the opened object is the inspected one.
// Left stale, this comment contradicted the production rationale it was supposed
// to be testing.
func TestInternalSymlinksAreRefusedNotFollowed(t *testing.T) {
	t.Run("VERSION is an internal symlink", func(t *testing.T) {
		base := t.TempDir()
		source := goInstall(t, filepath.Join(base, "src"))
		// a decoy INSIDE the tree, so os.Root permits the traversal and only
		// openVerified's Lstat can refuse it
		if err := os.WriteFile(filepath.Join(source, "lib", "decoy"), []byte("go9.9.9\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(source, "VERSION")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("lib/decoy", filepath.Join(source, "VERSION")); err != nil {
			t.Fatal(err)
		}
		identity, err := Identify(source)
		if err == nil {
			t.Fatalf("an internal VERSION symlink was followed and yielded version %q", identity.Version)
		}
	})

	t.Run("a member is an internal symlink", func(t *testing.T) {
		base := t.TempDir()
		source := goInstall(t, filepath.Join(base, "src"))
		if err := os.RemoveAll(filepath.Join(source, "src")); err != nil {
			t.Fatal(err)
		}
		// points at a sibling INSIDE the tree
		if err := os.Symlink("lib", filepath.Join(source, "src")); err != nil {
			t.Fatal(err)
		}
		if _, err := Stage(filepath.Join(base, "home"), source); err == nil {
			t.Fatal("an internal member symlink was followed and copied rather than refused")
		}
	})

	t.Run("a nested entry is an internal symlink", func(t *testing.T) {
		base := t.TempDir()
		source := goInstall(t, filepath.Join(base, "src"))
		if err := os.Symlink("../lib/marker", filepath.Join(source, "pkg", "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := Stage(filepath.Join(base, "home"), source); err == nil {
			t.Fatal("a nested internal symlink was followed and copied rather than refused")
		}
	})

	t.Run("bin/go is an internal symlink", func(t *testing.T) {
		base := t.TempDir()
		source := goInstall(t, filepath.Join(base, "src"))
		// a real executable INSIDE the tree, so os.Root permits the traversal
		// and only the Lstat guard can refuse it
		if err := os.WriteFile(filepath.Join(source, "lib", "realgo"), []byte("#!/bin/sh\necho go\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(source, "bin", "go")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../lib/realgo", filepath.Join(source, "bin", "go")); err != nil {
			t.Fatal(err)
		}
		if _, err := Identify(source); err == nil {
			t.Fatal("an internal bin/go symlink was followed and accepted as the pinned compiler")
		}
	})

	t.Run("go.env is an internal symlink", func(t *testing.T) {
		base := t.TempDir()
		source := goInstall(t, filepath.Join(base, "src"))
		// go.env is the ONE root file nothing else validates before the copy:
		// VERSION is proven by readVersion during Identify and bin/go by the
		// fingerprint walk, so copyOneFile's own guard is the only thing between
		// a symlinked go.env and a followed link. A mutant removing that guard
		// survived every test until this fixture existed - it was untested, not
		// redundant, which is the opposite diagnosis from the deleted guards.
		if err := os.WriteFile(filepath.Join(source, "lib", "decoy-env"), []byte("GOTOOLCHAIN=auto\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(source, "go.env")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("lib/decoy-env", filepath.Join(source, "go.env")); err != nil {
			t.Fatal(err)
		}
		if _, err := Stage(filepath.Join(base, "home"), source); err == nil {
			t.Fatal("an internal go.env symlink was followed and copied rather than refused")
		}
	})

	t.Run("CONTROL the same trees without links stage", func(t *testing.T) {
		base := t.TempDir()
		if _, err := Stage(filepath.Join(base, "home"), goInstall(t, filepath.Join(base, "src"))); err != nil {
			t.Fatalf("control failed: %v", err)
		}
	})
}

// TestStageRefusesToPublishUnderAnIdentityItCannotProve covers the mid-copy
// source-change guard, which nothing else reaches.
//
// It calls the production stage() with an identity that does NOT describe the
// source, which is exactly the state a source mutated between identification and
// copying would produce. Without the post-copy re-proof a tree is published under
// a name that lies about its contents and handed to the CURRENT caller; reuse
// verification would only catch it on a later call, after this seat already had it.
func TestStageRefusesToPublishUnderAnIdentityItCannotProve(t *testing.T) {
	base := t.TempDir()
	root := Root(filepath.Join(base, "home"))
	source := goInstall(t, filepath.Join(base, "src"))

	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()

	honest, err := identifyRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL: the honest identity publishes.
	if _, err := stage(root, honest, sourceRoot); err != nil {
		t.Fatalf("control failed: an honest identity must publish: %v", err)
	}

	lying := Identity{Version: honest.Version, Fingerprint: "deadbeefdeadbeef"}
	published, err := stage(root, lying, sourceRoot)
	if err == nil {
		t.Fatalf("published at %q under an identity the copy cannot prove", published)
	}
	if _, statErr := os.Stat(filepath.Join(root, lying.String())); statErr == nil {
		t.Fatal("a tree was published under the unprovable identity")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "staging") {
			t.Fatalf("a refused publish left a temporary tree behind: %s", entry.Name())
		}
	}
}

// TestSameObjectRefusesASubstitutedObject drives the inode comparison DIRECTLY,
// both arms, because the end-to-end version can only be reached by winning a race.
//
// This is the pure-predicate half of the F1 fix. A racing test that fails to
// expose anything is weak evidence; a predicate driven to both outcomes is not.
func TestSameObjectRefusesASubstitutedObject(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	if err := os.WriteFile(first, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Lstat(second)
	if err != nil {
		t.Fatal(err)
	}

	// CONTROL: the same object compares equal, so a passing refusal below cannot
	// mean "this function refuses everything".
	if err := sameObject(firstInfo, firstInfo, "first"); err != nil {
		t.Fatalf("control failed: an unchanged object was refused: %v", err)
	}
	// and a re-stat of the same path is still the same object
	again, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := sameObject(firstInfo, again, "first"); err != nil {
		t.Fatalf("control failed: a re-stat of the same path was refused: %v", err)
	}

	// THE ARM THAT MATTERS: a different object at the same name is refused.
	err = sameObject(firstInfo, secondInfo, "victim")
	if err == nil {
		t.Fatal("a substituted object was accepted; the Lstat-to-open window is open again")
	}
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("error = %v, want it to wrap ErrSymlink", err)
	}
	if !strings.Contains(err.Error(), "changed between inspection and open") {
		t.Errorf("refusal %q does not say what happened", err)
	}
	if !strings.Contains(err.Error(), "victim") {
		t.Errorf("refusal %q does not name the file", err)
	}
}

// TestSubstitutedSourceCannotExposeAnUnselectedFile closes F1 deterministically.
//
// REPRODUCED BEFORE THE FIX: a clean stage never copies PRIVATE-NOTES, and a stage
// racing a go.env swap published its contents AS go.env, on attempt 284 of 3000.
// Post-copy identity verification missed it because go.env is non-executable and
// therefore outside the fingerprint, so the hole was the COMPOSITION of the
// exec-set digest scope with the Lstat-to-open gap.
//
// WHY THIS USES A HOOK RATHER THAN A RACE. The racing version was a coin-flip
// killer: measured, neutering sameObject's comparison died while REMOVING its call
// - the same mutation semantically - survived the identical run. Scheduling the
// substitution makes the guard's regression deterministic. The hook is nil in
// production with one caller.
func TestSubstitutedSourceCannotExposeAnUnselectedFile(t *testing.T) {
	base := t.TempDir()
	source := goInstall(t, filepath.Join(base, "src"), "bin")
	if err := os.WriteFile(filepath.Join(source, "PRIVATE-NOTES"), []byte("NEVER-SELECTED-FOR-COPY"), 0o600); err != nil {
		t.Fatal(err)
	}

	// CONTROL: the file is genuinely not selected, so an exposure would be a real
	// change rather than the copy doing its job.
	clean, err := Stage(filepath.Join(base, "home-clean"), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(clean, "PRIVATE-NOTES")); err == nil {
		t.Fatal("precondition failed: the file IS selected for copying")
	}

	goEnv := filepath.Join(source, "go.env")
	swapped := false
	openWindowHook = func(name string) {
		if name != "go.env" || swapped {
			return
		}
		swapped = true
		if err := os.Remove(goEnv); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink("PRIVATE-NOTES", goEnv); err != nil {
			t.Error(err)
		}
	}
	defer func() { openWindowHook = nil }()

	staged, err := Stage(filepath.Join(base, "home"), source)
	if !swapped {
		t.Fatal("the hook never fired, so this test proved nothing about the window")
	}
	if err == nil {
		body, readErr := os.ReadFile(filepath.Join(staged, "go.env"))
		if readErr == nil && string(body) == "NEVER-SELECTED-FOR-COPY" {
			t.Fatalf("an unselected file was published as go.env: %q", body)
		}
		t.Fatalf("a source substituted inside the open window was staged anyway, at %q", staged)
	}
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("error = %v, want it to wrap ErrSymlink", err)
	}
	if !strings.Contains(err.Error(), "changed between inspection and open") {
		t.Errorf("refusal %q does not name the substitution", err)
	}
}

// TestRacingSourceCannotExposeAnUnselectedFile corroborates the above through the
// production path with no hook. A negative race result is weak evidence, which is
// why the deterministic test above is the actual regression.
func TestRacingSourceCannotExposeAnUnselectedFile(t *testing.T) {
	base := t.TempDir()
	source := goInstall(t, filepath.Join(base, "src"), "bin")
	if err := os.WriteFile(filepath.Join(source, "PRIVATE-NOTES"), []byte("NEVER-SELECTED-FOR-COPY"), 0o600); err != nil {
		t.Fatal(err)
	}
	goEnv := filepath.Join(source, "go.env")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(goEnv)
			_ = os.Symlink("PRIVATE-NOTES", goEnv)
			_ = os.Remove(goEnv)
			_ = os.WriteFile(goEnv, []byte("GOTOOLCHAIN=local\n"), 0o644)
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	home := filepath.Join(base, "home")
	for attempt := 0; attempt < 400; attempt++ {
		_ = os.RemoveAll(Root(home))
		staged, err := Stage(home, source)
		if err != nil {
			continue
		}
		if body, readErr := os.ReadFile(filepath.Join(staged, "go.env")); readErr == nil && string(body) == "NEVER-SELECTED-FOR-COPY" {
			t.Fatalf("attempt %d published an unselected file's contents as go.env", attempt)
		}
	}
}

// TestIrregularMembersAreRefusedWithoutBlocking covers a defect this test found
// rather than the one it was written for.
//
// It was written to justify openVerified's pre-open symlink refusal, on the theory
// that without it a symlinked FIFO would be opened and block. The SYMLINKED arm was
// refused correctly, so that theory was right about the guard. The DIRECT arm hung
// for the full timeout: a FIFO sitting inside an ordinary member was opened with a
// blocking open, because O_NONBLOCK had only ever been passed at the VERSION call
// site and never on member opens. That is an availability defect - it hangs seat
// launch rather than refusing it - and it predates the inode check entirely.
//
// O_NONBLOCK now lives inside openVerified so it covers every open. Both arms are
// kept because they fail for different reasons and a single arm would have hidden
// one of them.
func TestIrregularMembersAreRefusedWithoutBlocking(t *testing.T) {
	for _, test := range []struct {
		name      string
		symlinked bool
	}{
		{name: "a FIFO directly inside a member", symlinked: false},
		{name: "a member symlinked to a FIFO", symlinked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			source := goInstall(t, filepath.Join(base, "src"))
			if test.symlinked {
				if err := syscall.Mkfifo(filepath.Join(base, "outside-pipe"), 0o644); err != nil {
					t.Skipf("cannot create a FIFO fixture: %v", err)
				}
				if err := os.RemoveAll(filepath.Join(source, "doc")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(base, "outside-pipe"), filepath.Join(source, "doc")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := syscall.Mkfifo(filepath.Join(source, "lib", "pipe"), 0o644); err != nil {
					t.Skipf("cannot create a FIFO fixture: %v", err)
				}
			}

			done := make(chan error, 1)
			go func() {
				_, err := Stage(filepath.Join(base, "home"), source)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("an irregular file was staged successfully")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("Stage BLOCKED on a FIFO; every open must be O_NONBLOCK")
			}
		})
	}

	// CONTROL: the same tree with no irregular file stages, so the refusals above
	// are about the FIFO rather than about the fixture.
	base := t.TempDir()
	if _, err := Stage(filepath.Join(base, "home"), goInstall(t, filepath.Join(base, "src"))); err != nil {
		t.Fatalf("control failed: a clean tree must stage: %v", err)
	}
}
