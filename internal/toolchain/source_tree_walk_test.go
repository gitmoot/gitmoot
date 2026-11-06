package toolchain

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The non-Linux open path must refuse what openat2 refuses on Linux: a symlink
// at any component, anything outside the boundary, and malformed paths, while
// opening ordinary members. Both implementations run against the same tree so
// they cannot drift apart.
func TestOpenBeneathByComponentsMatchesOpenat2Refusals(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustWrite := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(root, "bin", "tool"))
	mustWrite(filepath.Join(root, "lib", "deep", "file"))
	mustWrite(filepath.Join(outside, "secret"))
	for link, target := range map[string]string{
		"leaf-link":        "bin/tool",
		"dir-link":         "lib",
		"escape":           outside,
		"lib/deep/up-link": "../../bin/tool",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	boundary, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(boundary)

	cases := []struct {
		relative  string
		directory bool
		wantOpen  bool
		// walkOnly: openat2 accepts this non-canonical path because it stays
		// beneath the boundary; the walker refuses it, which is stricter and
		// harmless because callers only pass enumerated, clean paths.
		walkOnly bool
	}{
		{".", true, true, false},
		{"bin/tool", false, true, false},
		{"lib/deep/file", false, true, false},
		{"lib/deep", true, true, false},
		{"leaf-link", false, false, false},
		{"dir-link/deep/file", false, false, false},
		{"escape/secret", false, false, false},
		{"lib/deep/up-link", false, false, false},
		{"../" + filepath.Base(outside) + "/secret", false, false, false},
		{"/etc/passwd", false, false, false},
		{"", false, false, false},
		{"bin/tool/", false, false, false},
		{"lib/../bin/tool", false, false, true},
		{"lib//deep/file", false, false, true},
		{"./bin/tool", false, false, true},
	}
	implementations := map[string]func(int, string, int) (int, error){
		"components": openBeneathByComponents,
		"platform":   openBeneath,
	}
	for name, open := range implementations {
		for _, tc := range cases {
			if tc.walkOnly && name == "platform" {
				continue
			}
			flags := unix.O_RDONLY | unix.O_CLOEXEC
			if tc.directory {
				flags |= unix.O_DIRECTORY
			}
			fd, err := open(boundary, tc.relative, flags)
			if err == nil {
				_ = unix.Close(fd)
			}
			if got := err == nil; got != tc.wantOpen {
				t.Errorf("%s: open %q: opened=%v err=%v, want opened=%v", name, tc.relative, got, err, tc.wantOpen)
			}
		}
	}
}

// A directory renamed out of the boundary after the walk opened it must not
// let the walk return a file outside the boundary: openat2's RESOLVE_BENEATH
// refuses that on Linux, and the walker must too.
func TestOpenBeneathByComponentsRefusesDirectoryRelocatedMidWalk(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lib", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "deep", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	boundary, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(boundary)
	walkBarrier = func(opened string) {
		if opened == "lib" {
			if err := os.Rename(filepath.Join(root, "lib"), filepath.Join(outside, "lib")); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer func() { walkBarrier = nil }()
	fd, err := openBeneathByComponents(boundary, "lib/deep/file", unix.O_RDONLY)
	if err == nil {
		_ = unix.Close(fd)
		t.Fatal("opened lib/deep/file after lib was moved outside the boundary")
	}
}
