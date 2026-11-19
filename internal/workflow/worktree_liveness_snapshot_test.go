package workflow

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func fakeProcCWD(t *testing.T, procRoot string, pid int, cwd string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(cwd, filepath.Join(dir, "cwd")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
}

// TestSnapshotWorktreeLivenessMatching pins the snapshot probe to
// WorktreeLiveness's matching semantics, and proves it reads /proc once.
func TestSnapshotWorktreeLivenessMatching(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	exact := filepath.Join(root, "exact")
	nested := filepath.Join(root, "nested")
	deleted := filepath.Join(root, "deleted")
	sibling := filepath.Join(root, "wt")
	self := filepath.Join(root, "self")
	fakeProcCWD(t, procRoot, 999999901, exact)
	fakeProcCWD(t, procRoot, 999999902, filepath.Join(nested, "sub", "dir"))
	fakeProcCWD(t, procRoot, 999999903, deleted+" (deleted)")
	fakeProcCWD(t, procRoot, 999999904, sibling+"2")
	fakeProcCWD(t, procRoot, os.Getpid(), self)
	if err := os.MkdirAll(filepath.Join(procRoot, "999999905"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err) // no cwd link: skipped, best effort
	}

	probe := snapshotWorktreeLiveness(procRoot)
	// The snapshot owns its data: /proc is not read again per probe.
	if err := os.RemoveAll(procRoot); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	cases := []struct {
		name       string
		path       string
		live, know bool
	}{
		{"exact path", exact, true, true},
		{"exact path with whitespace", "  " + exact + "\n", true, true},
		{"process under a subpath", nested, true, true},
		{"deleted cwd suffix", deleted, true, true},
		{"sibling prefix does not match", sibling, false, true},
		{"own pid ignored", self, false, true},
		{"unrelated path", filepath.Join(root, "none"), false, true},
		{"empty path", "  ", false, true},
	}
	for _, tc := range cases {
		live, known := probe(tc.path)
		if live != tc.live || known != tc.know {
			t.Errorf("%s: probe(%q) = (%v, %v), want (%v, %v)", tc.name, tc.path, live, known, tc.live, tc.know)
		}
	}

	unreadable := snapshotWorktreeLiveness(filepath.Join(root, "missing-proc"))
	if live, known := unreadable(exact); live || known {
		t.Fatalf("unreadable proc snapshot = (%v, %v), want (false, false)", live, known)
	}
}

// TestSnapshotAgreesWithBestEffortScan keeps the two entry points on one
// matching rule.
func TestSnapshotAgreesWithBestEffortScan(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	wt := filepath.Join(root, "wt")
	fakeProcCWD(t, procRoot, 999999911, filepath.Join(wt, "a")+" (deleted)")
	fakeProcCWD(t, procRoot, 999999912, wt+"2")
	fakeProcCWD(t, procRoot, os.Getpid(), filepath.Join(root, "self"))
	probe := snapshotWorktreeLiveness(procRoot)
	for _, path := range []string{wt, wt + "2", filepath.Join(root, "self"), filepath.Join(wt, "a"), root} {
		sl, sk := probe(path)
		bl, bk := bestEffortWorktreeLiveness(path, procRoot)
		if sl != bl || sk != bk {
			t.Errorf("path %q: snapshot (%v, %v) != scan (%v, %v)", path, sl, sk, bl, bk)
		}
	}
}
