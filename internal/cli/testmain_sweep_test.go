package cli

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestSweepLeakedTestTempRoots drives the #2644 sweep decision against a
// throwaway parent: a root goes only when its marker proves the owner gone
// (dead or recycled pid in this pid namespace). Live, unknown and
// other-namespace owners, roots without a usable marker whatever their age,
// and anything not shaped like gt<8 hex> stay.
func TestSweepLeakedTestTempRoots(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	now := time.Now()
	self := testTempRootOwner{PID: 100, StartTime: "1", PIDNamespace: "pid:[1]"}
	// pid -> current starttime; absent means no such pid, "unknown" means
	// /proc could not tell.
	processes := map[int]string{100: "1", 202: "999", 203: "3", 204: "4", 207: "unknown"}
	sweep := testTempRootSweep{
		self: self,
		liveness: func(pid int, startTime string) (bool, bool) {
			current, ok := processes[pid]
			if !ok {
				return false, true
			}
			if current == "unknown" {
				return false, false
			}
			return current == startTime, true
		},
	}
	owner := func(pid int, start string) testTempRootOwner {
		return testTempRootOwner{PID: pid, StartTime: start, PIDNamespace: self.PIDNamespace}
	}
	makeRoot := func(root string, marker *testTempRootOwner, age time.Duration) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "seat", "readonly"), 0o700); err != nil {
			t.Fatal(err)
		}
		// A read-only subtree, as the go command leaves module dirs.
		if err := os.WriteFile(filepath.Join(root, "seat", "readonly", "f"), []byte("x"), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, "seat", "readonly"), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = removeTestTempRoot(root) })
		if marker != nil {
			if err := writeTestTempRootOwner(root, *marker); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(root, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return root
	}
	ptr := func(o testTempRootOwner) *testTempRootOwner { return &o }
	otherNamespace := owner(299, "9")
	otherNamespace.PIDNamespace = "pid:[2]"
	// A dead pid in another namespace: its owner looks gone from here, but
	// that namespace's pids cannot be probed from this one.
	otherNamespaceDeadPID := owner(201, "1")
	otherNamespaceDeadPID.PIDNamespace = "pid:[2]"
	week := 7 * 24 * time.Hour

	removed := map[string]string{
		"dead pid":                makeRoot(filepath.Join(parent, "gt00000201"), ptr(owner(201, "1")), 0),
		"recycled pid":            makeRoot(filepath.Join(parent, "gt00000202"), ptr(owner(202, "2")), 0),
		"dead pid, old (ages ok)": makeRoot(filepath.Join(parent, "gt00000206"), ptr(owner(206, "6")), week),
	}
	kept := map[string]string{
		"live owner, old":             makeRoot(filepath.Join(parent, "gt00000204"), ptr(owner(204, "4")), week),
		"liveness unknown":            makeRoot(filepath.Join(parent, "gt00000207"), ptr(owner(207, "7")), week),
		"other pid namespace":         makeRoot(filepath.Join(parent, "gt00000299"), &otherNamespace, week),
		"other namespace, dead pid":   makeRoot(filepath.Join(parent, "gt00000298"), &otherNamespaceDeadPID, week),
		"marker-less, a week old":     makeRoot(filepath.Join(parent, "gt0000000a"), nil, week),
		"marker-less, fresh":          makeRoot(filepath.Join(parent, "gt0000000b"), nil, 0),
		"seven hex":                   makeRoot(filepath.Join(parent, "gt0000020"), ptr(owner(201, "1")), week),
		"nine hex":                    makeRoot(filepath.Join(parent, "gt000002010"), ptr(owner(201, "1")), week),
		"upper-case hex":              makeRoot(filepath.Join(parent, "gt0000020A"), ptr(owner(201, "1")), week),
		"other prefix":                makeRoot(filepath.Join(parent, "gx00000201"), ptr(owner(201, "1")), week),
		"symlink target (dead owner)": makeRoot(filepath.Join(outside, "gt00000208"), ptr(owner(201, "1")), week),
	}
	link := filepath.Join(parent, "gt00000209")
	if err := os.Symlink(kept["symlink target (dead owner)"], link); err != nil {
		t.Fatal(err)
	}
	kept["symlink"] = link
	file := filepath.Join(parent, "gt0000020f")
	if err := os.WriteFile(file, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, now.Add(-week), now.Add(-week)); err != nil {
		t.Fatal(err)
	}
	kept["regular file"] = file

	if err := sweepLeakedTestTempRoots([]string{parent, filepath.Join(parent, "missing")}, sweep); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for name, root := range removed {
		if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %s survived the sweep: %v", name, root, err)
		}
	}
	for name, root := range kept {
		if _, err := os.Lstat(root); err != nil {
			t.Errorf("%s: %s was removed: %v", name, root, err)
		}
	}
}

// TestSweepLeakedTestTempRootsProbesRealProcesses runs the production
// liveness probe: a root marked by this process stays, one marked by a child
// that has exited goes (whether its pid is free or already recycled).
func TestSweepLeakedTestTempRootsProbesRealProcesses(t *testing.T) {
	self, ok := currentTestTempRootOwner()
	if !ok {
		t.Skip("needs /proc process identities and a pid namespace")
	}
	child := exec.Command("true")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	dead := self
	dead.PID = child.Process.Pid
	dead.StartTime = workflow.RuntimeProcessIdentity(dead.PID)
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if dead.StartTime == "" {
		t.Fatal("no starttime for the running child")
	}
	parent := t.TempDir()
	live := filepath.Join(parent, "gt0000a11e")
	gone := filepath.Join(parent, "gt0000dead")
	for root, owner := range map[string]testTempRootOwner{live: self, gone: dead} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeTestTempRootOwner(root, owner); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := readTestTempRootOwner(live); err != nil || got != self {
		t.Fatalf("marker round trip = %+v, %v; want %+v", got, err, self)
	}
	if err := sweepLeakedTestTempRoots([]string{parent}, newTestTempRootSweep(self)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("root owned by this live process was removed: %v", err)
	}
	if _, err := os.Stat(gone); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("root owned by exited child %d survived: %v", dead.PID, err)
	}
}
