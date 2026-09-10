package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInstallation builds a minimal Go installation: a bin/go and a VERSION
// file, which is all selection reads. Staging reads far more, and that
// difference is deliberate - selection must be able to rank a tree it has not
// yet proven stageable, or a host with one bad tree could never pick a good
// one.
func writeInstallation(t *testing.T, root, version string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	return root
}

// TestSelectInstallationPrefersTheVersionTheModuleNeeds is #2143's real shape,
// and it is the arm that would have caught the production failure: a distro
// launcher FIRST among the candidates, a newer toolchain also installed, and a
// module that needs the newer one.
//
// Before this selection existed, staging took the first `go` on PATH, so this
// host staged 1.22.2 and the seat could not build a go.mod saying 1.26. A test
// written on a host whose first PATH `go` already satisfies go.mod passes
// without exercising anything, which is the population failure this repository
// has hit repeatedly, so the ordering here is load-bearing rather than
// incidental.
func TestSelectInstallationPrefersTheVersionTheModuleNeeds(t *testing.T) {
	launcher := GoInstallation{Root: "/usr/lib/go-1.22", Version: "go1.22.2"}
	pinned := GoInstallation{Root: "/root/.local/toolchains/go1.26.4", Version: "go1.26.4"}

	selected, err := SelectInstallation([]GoInstallation{launcher, pinned}, "1.26")
	if err != nil {
		t.Fatalf("SelectInstallation: %v", err)
	}
	if selected.Root != pinned.Root {
		t.Errorf("selected %q (%s), want the toolchain that satisfies the module, %q",
			selected.Root, selected.Version, pinned.Root)
	}
}

// TestSelectInstallationTakesTheLowestSatisfyingRelease pins the rule that
// keeps a review from drifting: a module asking for 1.26 gets 1.26 even when
// something newer is installed, matching Go's own behaviour.
func TestSelectInstallationTakesTheLowestSatisfyingRelease(t *testing.T) {
	candidates := []GoInstallation{
		{Root: "/newest", Version: "go1.27.0"},
		{Root: "/asked", Version: "go1.26.0"},
		{Root: "/older", Version: "go1.22.2"},
	}
	selected, err := SelectInstallation(candidates, "go1.26")
	if err != nil {
		t.Fatalf("SelectInstallation: %v", err)
	}
	if selected.Root != "/asked" {
		t.Errorf("selected %q, want /asked: a review must not silently move to a newer toolchain", selected.Root)
	}

	// 1.26 and 1.26.0 must compare equal, or an exact match reads as too old.
	selected, err = SelectInstallation([]GoInstallation{{Root: "/exact", Version: "go1.26"}}, "1.26.0")
	if err != nil {
		t.Fatalf("SelectInstallation(exact): %v", err)
	}
	if selected.Root != "/exact" {
		t.Errorf("selected %q, want /exact", selected.Root)
	}
}

// TestSelectInstallationFailsLoudlyWhenNothingSatisfies protects the property
// the preflight depends on. A seat that cannot run the gate must refuse
// visibly; the failure that must never return is a silent downgrade to a
// toolchain too old to build the repository, which surfaces later as
// "go.mod requires go >= 1.26" and reads as a repository problem.
func TestSelectInstallationFailsLoudlyWhenNothingSatisfies(t *testing.T) {
	_, err := SelectInstallation([]GoInstallation{{Root: "/usr/lib/go-1.22", Version: "go1.22.2"}}, "1.26")
	if err == nil {
		t.Fatal("SelectInstallation accepted a toolchain older than the module requires")
	}
	if !errors.Is(err, ErrNoSatisfyingToolchain) {
		t.Errorf("error = %v, want ErrNoSatisfyingToolchain", err)
	}
	if errors.Is(err, ErrNotPinned) {
		t.Errorf("error = %v, must NOT read as an unpinned installation: every candidate here is a valid Go tree, just too old", err)
	}
	for _, want := range []string{"go1.26", "/usr/lib/go-1.22", "go1.22.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q so the operator can see what was rejected", err, want)
		}
	}

	_, err = SelectInstallation(nil, "1.26")
	if err == nil || !strings.Contains(err.Error(), "no Go installation was found") {
		t.Errorf("error = %v, want the empty-host case named distinctly", err)
	}
}

// TestSelectInstallationWithNoRequirementTakesTheNewest covers the arm with no
// module context. Taking the lowest there would stage the oldest Go on the
// host, which is the original defect in different clothes.
func TestSelectInstallationWithNoRequirementTakesTheNewest(t *testing.T) {
	candidates := []GoInstallation{
		{Root: "/old", Version: "go1.22.2"},
		{Root: "/new", Version: "go1.26.4"},
	}
	selected, err := SelectInstallation(candidates, "")
	if err != nil {
		t.Fatalf("SelectInstallation: %v", err)
	}
	if selected.Root != "/new" {
		t.Errorf("selected %q, want /new", selected.Root)
	}
}

// TestSelectInstallationSkipsUnreadableCandidates keeps one bad tree from
// making the host unusable, while still naming it when nothing works.
func TestSelectInstallationSkipsUnreadableCandidates(t *testing.T) {
	candidates := []GoInstallation{
		{Root: "/broken", Version: ""},
		{Root: "/good", Version: "go1.26.4"},
	}
	selected, err := SelectInstallation(candidates, "1.26")
	if err != nil {
		t.Fatalf("SelectInstallation: %v", err)
	}
	if selected.Root != "/good" {
		t.Errorf("selected %q, want /good", selected.Root)
	}

	_, err = SelectInstallation([]GoInstallation{{Root: "/broken", Version: ""}}, "1.26")
	if err == nil {
		t.Fatal("a single unreadable candidate was accepted")
	}
	if !strings.Contains(err.Error(), "unreadable version") {
		t.Errorf("error = %q, want the unreadable candidate described rather than omitted", err)
	}
}

// TestInstallationVersionReadsARealTree checks the reader against a tree on
// disk rather than a literal, because selection is only as good as the version
// it is handed.
func TestInstallationVersionReadsARealTree(t *testing.T) {
	root := writeInstallation(t, t.TempDir(), "go1.26.4")
	version, err := InstallationVersion(root)
	if err != nil {
		t.Fatalf("InstallationVersion: %v", err)
	}
	if version != "go1.26.4" {
		t.Errorf("version = %q, want go1.26.4", version)
	}

	if _, err := InstallationVersion(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("InstallationVersion accepted a root that does not exist")
	}
}

// TestInstallationRootRejectsAnExecutableOutsideABinDirectory pins the guard
// that stops a stray PATH entry from nominating the whole system tree: on the
// #2143 host, classifying /usr/bin/go without resolving it first yields "/usr".
func TestInstallationRootRejectsAnExecutableOutsideABinDirectory(t *testing.T) {
	if root, ok := InstallationRoot("/usr/lib/go-1.22/bin/go"); !ok || root != "/usr/lib/go-1.22" {
		t.Errorf("InstallationRoot = (%q, %v), want (/usr/lib/go-1.22, true)", root, ok)
	}
	if root, ok := InstallationRoot("/opt/weird/go"); ok {
		t.Errorf("InstallationRoot = (%q, true), want refusal: the parent is not bin/ or sbin/", root)
	}
}

// TestInstallationsOnPathConsidersEveryEntry is the other half of #2143's real
// shape: the distro launcher FIRST on PATH, the satisfying toolchain later.
// exec.LookPath returns only the first, which is exactly how a host with a
// usable 1.26 installed staged 1.22 instead.
func TestInstallationsOnPathConsidersEveryEntry(t *testing.T) {
	base := t.TempDir()
	launcher := writeInstallation(t, filepath.Join(base, "distro"), "go1.22.2")
	pinned := writeInstallation(t, filepath.Join(base, "pinned"), "go1.26.4")

	pathEnv := strings.Join([]string{
		filepath.Join(launcher, "bin"),
		"/nonexistent",
		filepath.Join(pinned, "bin"),
		filepath.Join(launcher, "bin"), // duplicate, must not appear twice
	}, string(os.PathListSeparator))

	found := InstallationsOnPath(pathEnv)
	if len(found) != 2 {
		t.Fatalf("found %d installations, want 2: %+v", len(found), found)
	}
	if found[0].Root != launcher || found[1].Root != pinned {
		t.Errorf("found %+v, want the launcher then the pinned root", found)
	}
	if found[1].Version != "go1.26.4" {
		t.Errorf("version = %q, want go1.26.4", found[1].Version)
	}

	// The end-to-end property: first on PATH does not win.
	selected, err := SelectInstallation(found, "1.26")
	if err != nil {
		t.Fatalf("SelectInstallation: %v", err)
	}
	if selected.Root != pinned {
		t.Errorf("selected %q, want %q: PATH order must not decide", selected.Root, pinned)
	}
}

// TestInstallationsOnPathKeepsAnUnreadableTreeVisible: a tree that exists but
// cannot be read must still be reportable, or the failure says nothing was
// found when something was.
func TestInstallationsOnPathKeepsAnUnreadableTreeVisible(t *testing.T) {
	base := t.TempDir()
	broken := filepath.Join(base, "broken")
	if err := os.MkdirAll(filepath.Join(broken, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "bin", "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write go: %v", err)
	}
	// No VERSION file at all.
	found := InstallationsOnPath(filepath.Join(broken, "bin"))
	if len(found) != 1 {
		t.Fatalf("found %d, want 1", len(found))
	}
	if found[0].Version != "" {
		t.Errorf("version = %q, want empty for an unreadable tree", found[0].Version)
	}
	if _, err := SelectInstallation(found, "1.26"); err == nil || !strings.Contains(err.Error(), broken) {
		t.Errorf("error = %v, want the unreadable tree named", err)
	}
}

// TestModuleGoDirectiveReadsTheRequirement covers the input that decides which
// toolchain is correct, including the two arms that must NOT produce a
// requirement: a non-module directory, and the toolchain line.
func TestModuleGoDirectiveReadsTheRequirement(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/x\n\ngo 1.26\n\ntoolchain go1.27.1\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if got := ModuleGoDirective(dir); got != "1.26" {
		t.Errorf("directive = %q, want 1.26", got)
	}

	// No go.mod is not an error: a seat may review a repository that is not a
	// Go module, and that must not refuse to stage a toolchain.
	if got := ModuleGoDirective(t.TempDir()); got != "" {
		t.Errorf("directive = %q, want empty for a non-module directory", got)
	}

	// The requirement drives selection: 1.26 must not be satisfied by 1.22.
	if _, err := SelectInstallation([]GoInstallation{{Root: "/old", Version: "go1.22.2"}}, ModuleGoDirective(dir)); err == nil {
		t.Error("a go 1.26 module accepted a go1.22.2 installation")
	}
}

// TestStageNamesTheInstallationItRefused covers #2143's diagnostics gap.
//
// The member walk reports the refused path RELATIVE to the installation, so
// the production failure read "optional member pkg: symlink refused:
// pkg/include" and never said which tree. Every Go installation on a host has
// a pkg/include, so that message fits all of them equally and identifies none;
// three agents spent an evening inspecting three different roots because of
// it. The refusal must name the root, or it cannot be acted on.
//
// The fixture is the shape that actually failed: golang-1.22-go ships
// pkg/include as a symlink into /usr/share, and the walk refuses a symlink at
// any component.
func TestStageNamesTheInstallationItRefused(t *testing.T) {
	source := writeInstallation(t, filepath.Join(t.TempDir(), "go-1.22"), "go1.22.2")
	if err := os.MkdirAll(filepath.Join(source, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	if err := os.Symlink("../../../share/go-1.22/pkg/include", filepath.Join(source, "pkg", "include")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := Stage(t.TempDir(), source)
	if err == nil {
		t.Fatal("Stage accepted an installation with a refused member")
	}
	if !strings.Contains(err.Error(), "pkg/include") {
		t.Errorf("error = %q, want the refused member named", err)
	}
	if !strings.Contains(err.Error(), source) {
		t.Errorf("error = %q, want the installation root %q named: without it the message fits every Go tree on the host",
			err, source)
	}

}
