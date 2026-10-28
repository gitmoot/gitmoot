package toolchain

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRuntimeTool(t *testing.T, body string) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nprintf '"+body+"\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return source
}

// publishedRoot returns the <name>-<fingerprint> directory holding a staged launcher.
func publishedRoot(t *testing.T, launcher string) string {
	t.Helper()
	return filepath.Dir(filepath.Dir(launcher))
}

func ageRuntimeStamp(t *testing.T, home, published string, at time.Time) {
	t.Helper()
	stamp := filepath.Join(RuntimeRoot(home), runtimeLastUsedDir, filepath.Base(published))
	if err := os.Chtimes(stamp, at, at); err != nil {
		t.Fatalf("age stamp: %v", err)
	}
}

func emptyProc(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// fakeProcess makes procRoot/<pid> look like a live process whose executable
// is exe; cwd is "/" and it maps nothing.
func fakeProcess(t *testing.T, procRoot, pid, exe string) {
	t.Helper()
	dir := filepath.Join(procRoot, pid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(dir, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("tool\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "maps"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte("PATH=/usr/bin:/bin\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func names(copies []RuntimeCopy) string {
	var out []string
	for _, c := range copies {
		out = append(out, c.Name)
	}
	return strings.Join(out, ",")
}

// #2382: only a copy that is idle past MinIdle AND unreferenced is removed. A
// recently staged copy, a copy a live process executes, and an
// unavailable-runtime shim all survive, and a dry run removes nothing.
func TestCollectRuntimesRemovesOnlyIdleUnreferencedCopies(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)

	idleLauncher, _, err := StageRuntime(home, "idle", writeRuntimeTool(t, "idle"))
	if err != nil {
		t.Fatal(err)
	}
	recentLauncher, _, err := StageRuntime(home, "recent", writeRuntimeTool(t, "recent"))
	if err != nil {
		t.Fatal(err)
	}
	busyLauncher, _, err := StageRuntime(home, "busy", writeRuntimeTool(t, "busy"))
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := StageUnavailableRuntime(home, "gone")
	if err != nil {
		t.Fatal(err)
	}
	idle, recent, busy := publishedRoot(t, idleLauncher), publishedRoot(t, recentLauncher), publishedRoot(t, busyLauncher)
	ageRuntimeStamp(t, home, idle, old)
	ageRuntimeStamp(t, home, busy, old)
	unavailableRoot := publishedRoot(t, unavailable)
	if err := os.Chtimes(unavailableRoot, old, old); err != nil {
		t.Fatal(err)
	}
	proc := emptyProc(t)
	fakeProcess(t, proc, "4242", busyLauncher)

	options := RuntimeCollectOptions{MinIdle: 7 * 24 * time.Hour, Now: now, ProcRoot: proc, DryRun: true}
	dry, err := CollectRuntimes(home, options)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := names(dry.Removed); got != filepath.Base(idle) {
		t.Fatalf("dry run would remove %q, want only %q", got, filepath.Base(idle))
	}
	if _, err := os.Stat(idle); err != nil {
		t.Fatalf("dry run removed the idle copy: %v", err)
	}

	options.DryRun = false
	report, err := CollectRuntimes(home, options)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := names(report.Removed); got != filepath.Base(idle) {
		t.Fatalf("removed %q, want only %q", got, filepath.Base(idle))
	}
	if report.RemovedBytes <= 0 {
		t.Fatalf("removed bytes = %d, want the idle copy's size", report.RemovedBytes)
	}
	if _, err := os.Stat(idle); !os.IsNotExist(err) {
		t.Fatalf("idle copy still present: %v", err)
	}
	for _, kept := range []string{recent, busy, unavailableRoot} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(kept), err)
		}
	}
	for _, launcher := range []string{recentLauncher, busyLauncher} {
		if out, err := exec.Command(launcher).CombinedOutput(); err != nil {
			t.Fatalf("%s no longer runs: %v %s", launcher, err, out)
		}
	}
	entries, err := os.ReadDir(RuntimeRoot(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), runtimeTrashPrefix) {
			t.Fatalf("retired tree %s was left behind", entry.Name())
		}
	}
}

// #2382: an unstamped copy published before stamps existed is judged by its
// directory time, and a process scan that cannot complete keeps everything.
func TestCollectRuntimesJudgesLegacyCopiesByDirectoryTimeAndKeepsAllWhenScanFails(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	launcher, _, err := StageRuntime(home, "legacy", writeRuntimeTool(t, "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	published := publishedRoot(t, launcher)
	if err := os.Remove(filepath.Join(RuntimeRoot(home), runtimeLastUsedDir, filepath.Base(published))); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(published, old, old); err != nil {
		t.Fatal(err)
	}

	// A live process whose cmdline cannot be read: the scan is incomplete.
	proc := emptyProc(t)
	fakeProcess(t, proc, "77", "/usr/bin/true")
	cmdline := filepath.Join(proc, "77", "cmdline")
	if err := os.Remove(cmdline); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cmdline, 0o700); err != nil {
		t.Fatal(err)
	}
	options := RuntimeCollectOptions{MinIdle: 7 * 24 * time.Hour, Now: now, ProcRoot: proc}
	report, err := CollectRuntimes(home, options)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if report.Incomplete == "" || len(report.Removed) != 0 {
		t.Fatalf("incomplete scan: report = %+v, want nothing removed and Incomplete set", report)
	}
	if _, err := os.Stat(published); err != nil {
		t.Fatalf("legacy copy removed despite an incomplete scan: %v", err)
	}

	options.ProcRoot = emptyProc(t)
	report, err = CollectRuntimes(home, options)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(report.Removed) != 1 || report.Removed[0].Stamped {
		t.Fatalf("report = %+v, want the unstamped legacy copy removed", report)
	}
}

// #2382: a copy collected while current is republished by the next staging
// pass, and staging refreshes the stamp so a copy in use is never idle.
func TestStageRuntimeRefreshesStampAndRepublishesACollectedCopy(t *testing.T) {
	home := t.TempDir()
	source := writeRuntimeTool(t, "again")
	launcher, _, err := StageRuntime(home, "again", source)
	if err != nil {
		t.Fatal(err)
	}
	published := publishedRoot(t, launcher)
	old := time.Now().Add(-10 * 24 * time.Hour)
	ageRuntimeStamp(t, home, published, old)

	// Reusing the existing copy refreshes the stamp, so it is not idle.
	if _, _, err := StageRuntime(home, "again", source); err != nil {
		t.Fatal(err)
	}
	options := RuntimeCollectOptions{MinIdle: 7 * 24 * time.Hour, ProcRoot: emptyProc(t)}
	report, err := CollectRuntimes(home, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Removed) != 0 {
		t.Fatalf("a copy just reused by staging was removed: %+v", report.Removed)
	}

	ageRuntimeStamp(t, home, published, old)
	if report, err = CollectRuntimes(home, options); err != nil || len(report.Removed) != 1 {
		t.Fatalf("collect idle copy: report=%+v err=%v", report, err)
	}
	again, _, err := StageRuntime(home, "again", source)
	if err != nil {
		t.Fatalf("restage after collection: %v", err)
	}
	if again != launcher {
		t.Fatalf("restaged launcher %q, want the same content-addressed path %q", again, launcher)
	}
	if out, err := exec.Command(again).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "again" {
		t.Fatalf("republished copy does not run: err=%v out=%q", err, out)
	}
}

// #2382: collection waits for a staging pass holding the shared root lock, so
// it cannot retire a copy that staging is handing out.
func TestCollectRuntimesWaitsForStagingLock(t *testing.T) {
	home := t.TempDir()
	launcher, _, err := StageRuntime(home, "locked", writeRuntimeTool(t, "locked"))
	if err != nil {
		t.Fatal(err)
	}
	published := publishedRoot(t, launcher)
	ageRuntimeStamp(t, home, published, time.Now().Add(-10*24*time.Hour))

	unlock, err := lockRuntimeRoot(RuntimeRoot(home), false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan RuntimeCollectReport, 1)
	proc := emptyProc(t)
	go func() {
		report, _ := CollectRuntimes(home, RuntimeCollectOptions{MinIdle: 7 * 24 * time.Hour, ProcRoot: proc})
		done <- report
	}()
	select {
	case report := <-done:
		unlock()
		t.Fatalf("collection finished while staging held the shared lock: %+v", report)
	case <-time.After(300 * time.Millisecond):
	}
	// The staging pass refreshes the stamp before releasing the lock.
	if err := markRuntimeUsed(RuntimeRoot(home), filepath.Base(published), time.Now()); err != nil {
		t.Fatal(err)
	}
	unlock()
	select {
	case report := <-done:
		if len(report.Removed) != 0 {
			t.Fatalf("collection retired a copy staged while it waited: %+v", report.Removed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("collection never finished after the staging lock was released")
	}
	if _, err := os.Stat(published); err != nil {
		t.Fatalf("copy staged during collection is gone: %v", err)
	}
}

// #2404 (Jarvis 366005): with every copy recently staged there is nothing idle,
// and a dry run must still leave a leftover .trash- entry from an earlier
// interrupted pass on disk. Only a real pass removes it.
func TestCollectRuntimesDryRunLeavesTrashWhenNothingIsIdle(t *testing.T) {
	home := t.TempDir()
	if _, _, err := StageRuntime(home, "recent", writeRuntimeTool(t, "recent")); err != nil {
		t.Fatal(err)
	}
	trash := filepath.Join(RuntimeRoot(home), runtimeTrashPrefix+"old-copy-abc")
	if err := os.MkdirAll(filepath.Join(trash, ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := RuntimeCollectOptions{MinIdle: 7 * 24 * time.Hour, ProcRoot: emptyProc(t), DryRun: true}
	report, err := CollectRuntimes(home, options)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(report.Removed) != 0 {
		t.Fatalf("dry run reported removals with nothing idle: %s", names(report.Removed))
	}
	if _, err := os.Stat(trash); err != nil {
		t.Fatalf("dry run deleted the leftover trash entry: %v", err)
	}
	options.DryRun = false
	if _, err := CollectRuntimes(home, options); err != nil {
		t.Fatalf("real pass: %v", err)
	}
	if _, err := os.Stat(trash); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("real pass left the trash entry: %v", err)
	}
}

// #2404: a long-lived shell whose ONLY reference to a copy is a PATH entry keeps
// that copy; the same process without the entry does not.
func TestCollectRuntimesKeepsACopyThatOnlyAShellPATHReferences(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	launcher, _, err := StageRuntime(home, "pathonly", writeRuntimeTool(t, "pathonly"))
	if err != nil {
		t.Fatal(err)
	}
	published := publishedRoot(t, launcher)
	ageRuntimeStamp(t, home, published, now.Add(-10*24*time.Hour))
	options := RuntimeCollectOptions{MinIdle: 7 * 24 * time.Hour, Now: now, DryRun: true}

	options.ProcRoot = emptyProc(t)
	fakeProcess(t, options.ProcRoot, "77", "/bin/sh")
	control, err := CollectRuntimes(home, options)
	if err != nil || names(control.Removed) != filepath.Base(published) {
		t.Fatalf("control (no PATH entry) = %+v %v, want the idle copy removable", control, err)
	}

	options.ProcRoot = emptyProc(t)
	fakeProcess(t, options.ProcRoot, "77", "/bin/sh")
	environ := "HOME=/root\x00PATH=/usr/bin:" + filepath.Dir(launcher) + "/:/bin\x00"
	if err := os.WriteFile(filepath.Join(options.ProcRoot, "77", "environ"), []byte(environ), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := CollectRuntimes(home, options)
	if err != nil || len(report.Removed) != 0 {
		t.Fatalf("PATH-referenced copy = %+v %v, want kept", report, err)
	}
}

// #2404: an unreadable environ (as for another user's process when not root) is an
// incomplete scan, so every copy is kept.
func TestCollectRuntimesKeepsAllWhenEnvironIsUnreadable(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	launcher, _, err := StageRuntime(home, "noenv", writeRuntimeTool(t, "noenv"))
	if err != nil {
		t.Fatal(err)
	}
	ageRuntimeStamp(t, home, publishedRoot(t, launcher), now.Add(-10*24*time.Hour))
	proc := emptyProc(t)
	fakeProcess(t, proc, "78", "/bin/sh")
	environ := filepath.Join(proc, "78", "environ")
	if err := os.Remove(environ); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(environ, 0o700); err != nil { // reading a directory fails with EISDIR
		t.Fatal(err)
	}
	report, err := CollectRuntimes(home, RuntimeCollectOptions{MinIdle: 7 * 24 * time.Hour, Now: now, ProcRoot: proc})
	if err != nil || report.Incomplete == "" || len(report.Removed) != 0 {
		t.Fatalf("report = %+v %v, want Incomplete and nothing removed", report, err)
	}
}

// #2404: a copy that fails validation is not re-stamped, so it still ages out.
func TestStageRuntimeDoesNotRefreshTheStampOfACorruptCopy(t *testing.T) {
	home := t.TempDir()
	source := writeRuntimeTool(t, "corrupt")
	launcher, _, err := StageRuntime(home, "corrupt", source)
	if err != nil {
		t.Fatal(err)
	}
	published := publishedRoot(t, launcher)
	aged := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	ageRuntimeStamp(t, home, published, aged)
	if err := os.Remove(launcher); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StageRuntime(home, "corrupt", source); !errors.Is(err, ErrRuntimeNotStageable) {
		t.Fatalf("restage of a corrupt copy = %v, want ErrRuntimeNotStageable", err)
	}
	info, err := os.Stat(filepath.Join(RuntimeRoot(home), runtimeLastUsedDir, filepath.Base(published)))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(aged) {
		t.Fatalf("corrupt copy was re-stamped: %v, want %v", info.ModTime(), aged)
	}
}

// #2404: the collector skips any ".staging-" directory as an in-flight publish, so
// a runtime with such a name could never age out; staging refuses it.
func TestStageRuntimeRefusesNamesTheCollectorWouldSkip(t *testing.T) {
	home := t.TempDir()
	source := writeRuntimeTool(t, "staged")
	for _, name := range []string{"tool.staging-1", ".hidden"} {
		if _, _, err := StageRuntime(home, name, source); !errors.Is(err, ErrRuntimeNotStageable) {
			t.Fatalf("StageRuntime(%q) = %v, want ErrRuntimeNotStageable", name, err)
		}
		if _, err := StageUnavailableRuntime(home, name); !errors.Is(err, ErrRuntimeNotStageable) {
			t.Fatalf("StageUnavailableRuntime(%q) = %v, want ErrRuntimeNotStageable", name, err)
		}
	}
	if _, _, err := StageRuntime(home, "tool.v2", source); err != nil {
		t.Fatalf("an ordinary dotted name was refused: %v", err)
	}
}
