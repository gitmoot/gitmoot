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

func firstInstallation(candidates []GoInstallation, required string) (GoInstallation, error) {
	ranked, err := SelectInstallations(candidates, required)
	if err != nil {
		return GoInstallation{}, err
	}
	return ranked[0], nil
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

	selected, err := firstInstallation([]GoInstallation{launcher, pinned}, "1.26")
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
	selected, err := firstInstallation(candidates, "go1.26")
	if err != nil {
		t.Fatalf("SelectInstallation: %v", err)
	}
	if selected.Root != "/asked" {
		t.Errorf("selected %q, want /asked: a review must not silently move to a newer toolchain", selected.Root)
	}

	// Go distinguishes the language version from the first release: go1.26 is
	// older than go1.26.0 and must not satisfy that exact release requirement.
	_, err = firstInstallation([]GoInstallation{{Root: "/language", Version: "go1.26"}}, "1.26.0")
	if !errors.Is(err, ErrNoSatisfyingToolchain) {
		t.Fatalf("SelectInstallation(language version) error = %v, want ErrNoSatisfyingToolchain", err)
	}
}

func TestSelectInstallationUsesGoVersionOrdering(t *testing.T) {
	candidates := []GoInstallation{
		{Root: "/release", Version: "go1.26.0"},
		{Root: "/rc2", Version: "go1.26rc2"},
		{Root: "/rc1", Version: "go1.26rc1"},
	}
	selected, err := firstInstallation(candidates, "1.26rc1")
	if err != nil {
		t.Fatalf("SelectInstallation: %v", err)
	}
	if selected.Root != "/rc1" {
		t.Errorf("selected %q, want /rc1", selected.Root)
	}
	if _, err := firstInstallation(candidates[1:], "1.26.0"); !errors.Is(err, ErrNoSatisfyingToolchain) {
		t.Errorf("release requirement error = %v, want ErrNoSatisfyingToolchain", err)
	}

	huge := "1." + strings.Repeat("9", 100)
	selected, err = firstInstallation([]GoInstallation{{Root: "/huge", Version: "go" + huge}}, huge)
	if err != nil {
		t.Fatalf("SelectInstallation(large component): %v", err)
	}
	if selected.Root != "/huge" {
		t.Errorf("selected %q, want /huge", selected.Root)
	}
}

// TestSelectInstallationFailsLoudlyWhenNothingSatisfies protects the property
// the preflight depends on. A seat that cannot run the gate must refuse
// visibly; the failure that must never return is a silent downgrade to a
// toolchain too old to build the repository, which surfaces later as
// "go.mod requires go >= 1.26" and reads as a repository problem.
func TestSelectInstallationFailsLoudlyWhenNothingSatisfies(t *testing.T) {
	_, err := firstInstallation([]GoInstallation{{Root: "/usr/lib/go-1.22", Version: "go1.22.2"}}, "1.26")
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

	_, err = firstInstallation(nil, "1.26")
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
	selected, err := firstInstallation(candidates, "")
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
	selected, err := firstInstallation(candidates, "1.26")
	if err != nil {
		t.Fatalf("SelectInstallation: %v", err)
	}
	if selected.Root != "/good" {
		t.Errorf("selected %q, want /good", selected.Root)
	}

	_, err = firstInstallation([]GoInstallation{{Root: "/broken", Version: ""}}, "1.26")
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

// TestInstallationRootAcceptsEveryInstallationPrefix carries ruling 122157's
// coverage, moved here with the function it tests (#2143 deleted the cli copy
// rather than leaving two implementations).
//
// /opt, /usr/local, /nix/store and /snap are all STAGED, so a test that
// refused them would protect the defect that ruling removed. The bin/ guard
// still matters for a different reason, kept as the control below: on the
// #2143 host, classifying /usr/bin/go without resolving the symlink first
// yields "/usr", and staging would copy the system tree.
func TestInstallationRootAcceptsEveryInstallationPrefix(t *testing.T) {
	for path, want := range map[string]string{
		"/opt/go/bin/go":                          "/opt/go",
		"/opt/nested/deeper/go/bin/go":            "/opt/nested/deeper/go",
		"/usr/local/go/bin/go":                    "/usr/local/go",
		"/usr/local/bin/go":                       "/usr/local",
		"/nix/store/abc-go-1.26.4/bin/go":         "/nix/store/abc-go-1.26.4",
		"/snap/go/current/bin/go":                 "/snap/go/current",
		"/root/.local/toolchains/go1.26.4/bin/go": "/root/.local/toolchains/go1.26.4",
		"/home/op/sdk/go1.26.4/bin/go":            "/home/op/sdk/go1.26.4",
		"/opt-not-a-prefix/go/bin/go":             "/opt-not-a-prefix/go",
		"/usr/localish/go/bin/go":                 "/usr/localish/go",
		"/usr/lib/go-1.22/bin/go":                 "/usr/lib/go-1.22",
	} {
		root, ok := InstallationRoot(path)
		if !ok || root != want {
			t.Errorf("InstallationRoot(%q) = %q,%v; want %q,true. Refusing a system prefix leaves that operator root for the recursive host grant that cutover deleted.", path, root, ok, want)
		}
	}

	// CONTROL: a path that is not an installation layout is still refused, so
	// accepting the prefixes above did not turn this into filepath.Dir twice.
	if _, ok := InstallationRoot("/somewhere/go"); ok {
		t.Fatal("a go executable not under bin/ or sbin/ was accepted as an installation")
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
	selected, err := firstInstallation(found, "1.26")
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
	if _, err := firstInstallation(found, "1.26"); err == nil || !strings.Contains(err.Error(), broken) {
		t.Errorf("error = %v, want the unreadable tree named", err)
	}
}

// TestWorkspaceGoRequirementReadsModule covers the module input that decides
// which toolchain is correct, including accepted whitespace and comments.
func TestWorkspaceGoRequirementReadsModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/x\r\n\r\ngo\t1.26 // minimum\r\n\r\ntoolchain go1.27.1\r\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	requirement, effective, err := WorkspaceGoRequirement(dir, "off")
	if err != nil {
		t.Fatal(err)
	}
	if requirement != "1.26" || effective != "off" {
		t.Errorf("requirement = (%q, %q), want (1.26, off)", requirement, effective)
	}
	if _, err := firstInstallation([]GoInstallation{{Root: "/old", Version: "go1.22.2"}}, requirement); err == nil {
		t.Error("a go 1.26 module accepted a go1.22.2 installation")
	}

	malformed := t.TempDir()
	if err := os.WriteFile(filepath.Join(malformed, "go.mod"), []byte("module x\ngo 1.26 extra\n"), 0o644); err != nil {
		t.Fatalf("write malformed go.mod: %v", err)
	}
	if _, _, err := WorkspaceGoRequirement(malformed, "off"); err == nil {
		t.Fatal("malformed module directive was accepted")
	}
}

func TestWorkspaceGoRequirementHonorsGOWORK(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "module")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootWork := filepath.Join(root, "go.work")
	if err := os.WriteFile(rootWork, []byte("go 1.26\n\nuse ./module\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, gowork := range []string{"", "auto"} {
		version, effective, err := WorkspaceGoRequirement(child, gowork)
		if err != nil {
			t.Fatal(err)
		}
		if version != "1.26" || effective != rootWork {
			t.Fatalf("GOWORK=%q requirement = (%q, %q), want (1.26, %q)", gowork, version, effective, rootWork)
		}
	}
	version, effective, err := WorkspaceGoRequirement(child, "off")
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.22" || effective != "off" {
		t.Fatalf("disabled requirement = (%q, %q), want (1.22, off)", version, effective)
	}

	explicit := filepath.Join(t.TempDir(), "alternate.work")
	if err := os.WriteFile(explicit, []byte("go 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	version, effective, err = WorkspaceGoRequirement(child, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.27" || effective != explicit {
		t.Fatalf("explicit requirement = (%q, %q), want (1.27, %q)", version, effective, explicit)
	}
	if err := os.WriteFile(filepath.Join(child, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	version, effective, err = WorkspaceGoRequirement(child, "off")
	if err != nil {
		t.Fatalf("directive-less valid module was refused: %v", err)
	}
	if version != "" || effective != "off" {
		t.Fatalf("directive-less module requirement = (%q, %q), want (empty, off)", version, effective)
	}
	if err := os.WriteFile(rootWork, []byte("use ./module\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	version, effective, err = WorkspaceGoRequirement(child, "auto")
	if err != nil {
		t.Fatalf("directive-less valid workspace was refused: %v", err)
	}
	if version != "" || effective != rootWork {
		t.Fatalf("directive-less requirement = (%q, %q), want (empty, %q)", version, effective, rootWork)
	}
	if err := os.Chmod(root, 0o111); err != nil {
		t.Fatal(err)
	}
	version, effective, err = WorkspaceGoRequirement(child, "auto")
	if restoreErr := os.Chmod(root, 0o755); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err != nil {
		t.Fatalf("workspace under search-only ancestor was refused: %v", err)
	}
	if version != "" || effective != rootWork {
		t.Fatalf("search-only ancestor requirement = (%q, %q), want (empty, %q)", version, effective, rootWork)
	}

	ignoredWork := filepath.Join(child, "go.work")
	if err := os.Mkdir(ignoredWork, 0o755); err != nil {
		t.Fatal(err)
	}
	version, effective, err = WorkspaceGoRequirement(child, "auto")
	if err != nil {
		t.Fatalf("non-file go.work was not ignored during automatic search: %v", err)
	}
	if version != "" || effective != rootWork {
		t.Fatalf("ignored non-file requirement = (%q, %q), want (empty, %q)", version, effective, rootWork)
	}
	if err := os.Remove(ignoredWork); err != nil {
		t.Fatal(err)
	}

	linkedRoot := t.TempDir()
	if err := os.Symlink(explicit, filepath.Join(linkedRoot, "go.work")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WorkspaceGoRequirement(linkedRoot, "auto"); err == nil {
		t.Fatal("an escaping go.work symlink was silently parsed under different staging and execution rules")
	}
}

func TestWorkspaceGoRequirementContainsAndBoundsModuleRead(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.mod")
	if err := os.WriteFile(outside, []byte("go 99.0\n"), 0o644); err != nil {
		t.Fatalf("write outside go.mod: %v", err)
	}
	linked := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(linked, "go.mod")); err != nil {
		t.Fatalf("symlink go.mod: %v", err)
	}

	nonRegular := t.TempDir()
	if err := os.Mkdir(filepath.Join(nonRegular, "go.mod"), 0o755); err != nil {
		t.Fatalf("mkdir go.mod: %v", err)
	}

	oversized := t.TempDir()
	contents := strings.Repeat("x", int(maxGoModBytes)) + "\ngo 99.0\n"
	if err := os.WriteFile(filepath.Join(oversized, "go.mod"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write oversized go.mod: %v", err)
	}

	for name, dir := range map[string]string{
		"escaping symlink": linked,
		"non-regular":      nonRegular,
		"oversized":        oversized,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := WorkspaceGoRequirement(dir, "off"); err == nil {
				t.Fatal("unsafe go.mod was accepted")
			}
		})
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
