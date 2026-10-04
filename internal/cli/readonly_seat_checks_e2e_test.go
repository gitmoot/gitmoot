package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestReadOnlySeatRunsSocketTestsAndOfflineModuleBuildsE2E is the #2314
// acceptance test. A real read-only seat, launched by `job run` under real
// Landlock, must be able to run the project checks reviewers reported blocked,
// and still must not be able to write anything outside its own scratch space:
//
//   - a Go test that binds a Unix socket under its t.TempDir passes. The seat's
//     TMPDIR used to be <cache root>/tmp, so the socket path overran the
//     108-byte limit before the test's own suffix fit;
//   - an offline `go build` of a module with a dependency succeeds. The seat's
//     module cache used to be a new empty directory per checkout path;
//   - writes to the checkout, the Gitmoot home, the temp parent, /tmp, an
//     unrelated directory and another seat's gmr dir are denied;
//   - the seat's private temp dir and its registry marker are gone after the
//     job.
//
// WHY `shell`: no LLM, no runtime auth, no network. The seat reports what it
// observed in its result summary, so a failure names the check that broke.
func TestReadOnlySeatRunsSocketTestsAndOfflineModuleBuildsE2E(t *testing.T) {
	skipUnlessSeatTempParentWritable(t)
	home := t.TempDir()
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent-herdr.sock"))
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go on PATH: %v", err)
	}
	// Keep the host's large runtime installations out of staging; the seat only
	// needs Go and the base system.
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(goBinary), "/usr/bin", "/bin"}, string(os.PathListSeparator)))
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")

	// The daemon's module proxy: a file:// proxy serving one dependency. The
	// daemon prefetches through it; the seat itself never sees it.
	proxy := t.TempDir()
	writeSeatModuleProxy(t, proxy, "example.com/dep", "v1.0.0", map[string]string{
		"go.mod": "module example.com/dep\n\ngo 1.22\n",
		"dep.go": "package dep\n\nfunc Name() string { return \"dep\" }\n",
	})
	t.Setenv("GOPROXY", "file://"+proxy)

	store := openCLIJobStore(t, home)
	defer store.Close()

	checkout := t.TempDir()
	writeSeatFixtureFile(t, checkout, "go.mod", "module example.com/review\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n")
	writeSeatFixtureFile(t, checkout, "main.go", "package main\n\nimport \"example.com/dep\"\n\nfunc main() { println(dep.Name()) }\n")
	// t.TempDir appends the test name, a random number and /001, so the socket
	// path is long even under a short TMPDIR. The fixture asserts that, so it
	// cannot pass by binding a short path.
	writeSeatFixtureFile(t, checkout, filepath.Join("sock", "sock_test.go"), `package sock

import (
	"net"
	"path/filepath"
	"testing"
)

func TestReviewerBindsAUnixSocketInItsTempDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sock")
	if len(path) < 64 {
		t.Fatalf("socket path %q is too short to exercise the limit", path)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
}
`)
	writeSeatModuleSums(t, checkout, proxy)
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "add", "-A")
	runGit(t, checkout, "commit", "-m", "init")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// Places the seat must not write. otherSeat stands in for a concurrent
	// review's private temp dir: same parent, same naming, same owner.
	other := t.TempDir()
	otherSeat := filepath.Join(seatTempParent(), "gmr-"+randomSeatHex(t))
	if err := os.Mkdir(otherSeat, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(otherSeat) })
	tmpProbe := "/tmp/gitmoot-seat-write-probe-" + randomSeatHex(t)
	t.Cleanup(func() { _ = os.Remove(tmpProbe) })

	// The seat runs its checks the way the seat docs tell a reviewer to:
	// CGO_ENABLED=0, because cgo reads /usr/include, which no seat is granted.
	script := strings.NewReplacer(
		"@CHECKOUT@", checkout,
		"@HOME@", home,
		"@OTHER@", other,
		"@OTHER_SEAT@", otherSeat,
		"@TMP_PROBE@", tmpProbe,
	).Replace(`clean() { tr '\n\t' '  ' | tr -d '\000-\037"\\' | cut -c1-600; }
probe() { if ( : > "$1" ) 2>/dev/null; then rm -f "$1"; echo WRITABLE; else echo denied; fi; }
cd '@CHECKOUT@' || exit 1
mode=$(stat -c %a "$TMPDIR" 2>&1 | clean)
sock_log=$(CGO_ENABLED=0 go test -count=1 ./sock 2>&1); sock=$?
sock_log=$(printf '%s' "$sock_log" | clean)
build_log=$(CGO_ENABLED=0 go build -o "$TMPDIR/review-app" . 2>&1); build=$?
build_log=$(printf '%s' "$build_log" | clean)
w_checkout=$(probe '@CHECKOUT@/seat-write-probe')
w_home=$(probe '@HOME@/seat-write-probe')
w_parent=$(probe "$(dirname "$TMPDIR")/seat-write-probe")
w_tmp=$(probe '@TMP_PROBE@')
w_other=$(probe '@OTHER@/seat-write-probe')
w_other_seat=$(probe '@OTHER_SEAT@/seat-write-probe')
w_own=$(probe "$TMPDIR/seat-write-probe")
printf '{"gitmoot_result":{"decision":"approved","summary":"tmpdir=%s mode=%s sock=%s build=%s checkout=%s home=%s parent=%s tmp=%s other=%s other_seat=%s own=%s || sock_log=%s || build_log=%s","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}\n' "$TMPDIR" "$mode" "$sock" "$build" "$w_checkout" "$w_home" "$w_parent" "$w_tmp" "$w_other" "$w_other_seat" "$w_own" "$sock_log" "$build_log"
`)
	seedDaemonWorkerAgent(t, store, "seat", "shell", script, []string{"ask"}, "owner/repo")
	seedCLIJob(t, store, db.Job{
		ID:    "job-seat-checks",
		Agent: "seat",
		Type:  "ask",
		State: string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo:         "owner/repo",
			Branch:       "main",
			WorktreePath: checkout,
			ReadOnlySeat: true,
		}),
	}, "queued")

	summary, stdout, stderr := runSeatJobSummary(t, store, home, "job-seat-checks")
	facts, _, _ := strings.Cut(summary, " || ")
	got := map[string]string{}
	for _, field := range strings.Fields(facts) {
		key, value, _ := strings.Cut(field, "=")
		got[key] = value
	}
	if len(got) == 0 {
		t.Fatalf("the seat reported nothing (summary %q)\nstdout=%s\nstderr=%s", summary, stdout, stderr)
	}

	seatTemp := got["tmpdir"]
	if !regexp.MustCompile(`^gmr-[0-9a-f]{8}$`).MatchString(filepath.Base(seatTemp)) || !filepath.IsAbs(seatTemp) {
		t.Errorf("seat TMPDIR = %q, want a private <temp parent>/gmr-<8 hex> directory", seatTemp)
	}
	if got["mode"] != "700" {
		t.Errorf("seat TMPDIR mode = %q, want 700 so no other process can read the review's scratch files", got["mode"])
	}
	if got["sock"] != "0" {
		t.Errorf("a Go test binding a Unix socket under the seat's TMPDIR failed (exit %q): %s", got["sock"], summary)
	}
	if got["build"] != "0" {
		t.Errorf("an offline `go build` of a module with a dependency failed in the seat (exit %q): %s", got["build"], summary)
	}
	if got["own"] != "WRITABLE" {
		t.Errorf("the seat could not write its own TMPDIR (%q), so the denial probes below prove nothing: %s", got["own"], summary)
	}
	for _, place := range []string{"checkout", "home", "parent", "tmp", "other", "other_seat"} {
		if got[place] != "denied" {
			t.Errorf("seat write to %s = %q, want denied: %s", place, got[place], summary)
		}
	}

	// The seat's temp dir and its marker go away with the job.
	if seatTemp != "" {
		if _, err := os.Lstat(seatTemp); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("seat temp dir %q still exists after the job (err %v)", seatTemp, err)
			_ = os.RemoveAll(seatTemp)
		}
		marker := filepath.Join(config.PathsForHome(home).Home, "cache", "seat-tmp", filepath.Base(seatTemp))
		if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("seat temp marker %q still exists after the job (err %v)", marker, err)
		}
	}
}

// TestDaemonStartupSweepsSeatTempDirsOfDeadOwners covers the crash path of
// #2314: a daemon that dies mid-review never runs its seats' cleanup, so the
// next daemon's startup removes every seat temp dir whose owner process is
// gone, and leaves a live owner's dir alone.
func TestDaemonStartupSweepsSeatTempDirsOfDeadOwners(t *testing.T) {
	skipUnlessSeatTempParentWritable(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := exited.Process.Pid

	registry := filepath.Join(paths.Home, "cache", "seat-tmp")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	seed := func(pid int) (string, string) {
		name := "gmr-" + randomSeatHex(t)
		dir := filepath.Join(seatTempParent(), name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		writeSeatFixtureFile(t, dir, "scratch", "left behind")
		marker := filepath.Join(registry, name)
		if err := os.WriteFile(marker, []byte(strconv.Itoa(pid)+"\n"+dir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir, marker
	}
	deadDir, deadMarker := seed(deadPID)
	liveDir, liveMarker := seed(os.Getpid())

	var out bytes.Buffer
	cleanup := daemonRunStartupReconcile(context.Background(), home, os.Args, &out)
	defer cleanup()

	for _, path := range []string{deadDir, deadMarker} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("startup left %q of a dead owner behind (err %v); output %q", path, err, out.String())
		}
	}
	for _, path := range []string{liveDir, liveMarker} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("startup removed %q of a live owner: %v", path, err)
		}
	}
}

// TestSeatTempSweepKeepsADirOutsideAnySeatTempParent bounds the sweep's
// recursive delete: a marker of a dead owner that names a gmr-<8 hex> directory
// deep in the filesystem, where seatTempParent never puts one under any
// TMPDIR, is refused and kept, and the directory survives.
func TestSeatTempSweepKeepsADirOutsideAnySeatTempParent(t *testing.T) {
	home := t.TempDir()
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	registry := seatTempRegistry(home)
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "gmr-" + randomSeatHex(t)
	dir := filepath.Join(t.TempDir(), "operator", "work", name)
	writeSeatFixtureFile(t, dir, "keep", "not a seat's")
	marker := filepath.Join(registry, name)
	if err := os.WriteFile(marker, []byte(strconv.Itoa(exited.Process.Pid)+"\n"+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := sweepStaleSeatTempDirs(home)
	if removed != 0 || err == nil || !strings.Contains(err.Error(), "not a seat temp dir") {
		t.Fatalf("sweep = removed %d, err %v; want the deep directory refused", removed, err)
	}
	for _, path := range []string{filepath.Join(dir, "keep"), marker} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("sweep removed %q: %v", path, err)
		}
	}
}

// TestSeatTempSweepNeverRemovesItsOwnTempDir covers a gitmoot run nested in a
// seat: its registry is writable, so a marker in it can name the outer seat's
// gmr dir, the TMPDIR the nested run stands in. The sweep must refuse it even
// when the recorded owner is dead.
func TestSeatTempSweepNeverRemovesItsOwnTempDir(t *testing.T) {
	skipUnlessSeatTempParentWritable(t)
	home := t.TempDir()
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	registry := seatTempRegistry(home)
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "gmr-" + randomSeatHex(t)
	outer := filepath.Join(seatTempParent(), name)
	if err := os.Mkdir(outer, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outer) })
	writeSeatFixtureFile(t, outer, "keep", "the outer seat's scratch")
	marker := filepath.Join(registry, name)
	if err := os.WriteFile(marker, []byte(strconv.Itoa(exited.Process.Pid)+"\n"+outer+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", outer)

	removed, err := sweepStaleSeatTempDirs(home)
	if removed != 0 || err == nil || !strings.Contains(err.Error(), "own temp dir") {
		t.Fatalf("sweep = removed %d, err %v; want its own TMPDIR refused", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(outer, "keep")); err != nil {
		t.Fatalf("sweep removed the outer seat's temp dir: %v", err)
	}
}

// TestReadOnlySeatFallsBackToCacheRootTempWithoutAShortWritableParent covers a
// gitmoot run inside a seat of a daemon from before #2314: TMPDIR is long and
// /tmp is denied, so no short seat temp dir can be made. The seat must still
// be built, with the pre-#2314 <cache root>/tmp as TMPDIR, no extra write
// grant, no registry marker, and that dir gone after the job's cleanup.
func TestReadOnlySeatFallsBackToCacheRootTempWithoutAShortWritableParent(t *testing.T) {
	// /sys refuses mkdir even to root (EPERM), the way Landlock refuses a seat.
	if err := os.Mkdir("/sys/gmr-probe", 0o700); !errors.Is(err, fs.ErrPermission) {
		_ = os.Remove("/sys/gmr-probe")
		t.Skipf("needs a short parent that refuses mkdir; /sys gave %v", err)
	}
	home := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "review-worktree")
	stateDir := filepath.Join(t.TempDir(), "claude-state")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSeatFixtureFile(t, stateDir, ".credentials.json", `{"claudeAiOauth":{"accessToken":"seat"}}`)
	t.Setenv("TMPDIR", "/sys")
	agent := runtime.Agent{
		Runtime:          runtime.ClaudeRuntime,
		AutonomyPolicy:   runtime.AutonomyPolicyReadOnly,
		ReadOnlySeat:     true,
		RuntimeConfigDir: stateDir,
	}
	wrapped, _, err := wrapReadOnlySandboxAdapter(home, agent, checkout, "", runtime.ClaudeAdapter{Runner: subprocess.GroupRunner{}})
	if err != nil {
		t.Fatalf("a seat without a short writable temp parent was refused: %v", err)
	}
	seat, ok := wrapped.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter = %T, want readOnlyRuntimeAdapter", wrapped)
	}
	runner, ok := seat.Adapter.(runtime.ClaudeAdapter).Runner.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("wrapped runner = %T, want subprocess.WrappingRunner", seat.Adapter.(runtime.ClaudeAdapter).Runner)
	}
	if !seat.cleanupTemp.inCacheRoot || seat.cleanupTemp.dir != filepath.Join(seat.cleanupRoot, "tmp") {
		t.Fatalf("seat temp = %+v, want the fallback <cache root>/tmp under %s", seat.cleanupTemp, seat.cleanupRoot)
	}
	assertSeatTempDirGrant(t, runner.WritablePaths, seat.cleanupTemp, runner.Env)
	registry := seatTempRegistry(config.PathsForHome(home).Home)
	if entries, err := os.ReadDir(registry); err != nil || len(entries) != 0 {
		t.Fatalf("seat temp registry %s = %v err=%v, want no marker for a cache-root temp dir", registry, entries, err)
	}
	if err := seat.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(seat.cleanupTemp.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cache-root seat temp %q survived cleanup: %v", seat.cleanupTemp.dir, err)
	}
}

// TestShortTestTempRootReportsUnusableParents covers the read-only seat case
// of TestMain: when no parent can hold a short test temp root (one refuses the
// mkdir, as a seat's denied /tmp does; one is too long, as a seat's own TMPDIR
// is), it returns an error naming each (which TestMain reports and runs on)
// and creates nothing.
func TestShortTestTempRootReportsUnusableParents(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("needs procfs: /proc is a short parent nobody can mkdir in")
	}
	long := t.TempDir()
	root, err := makeShortTestTempRoot([]string{"/proc", long})
	if err == nil {
		_ = removeTestTempRoot(root)
		t.Fatalf("makeShortTestTempRoot = %q, want an error", root)
	}
	if !strings.Contains(err.Error(), "mkdir /proc/gt") || !strings.Contains(err.Error(), "too long to parent seat temp dirs") {
		t.Fatalf("makeShortTestTempRoot error = %v, want both parents named", err)
	}
	if entries, err := os.ReadDir(long); err != nil || len(entries) != 0 {
		t.Fatalf("long parent %q = %v err=%v, want untouched", long, entries, err)
	}
}

// runSeatJobSummary runs a queued read-only seat job through `job run` and
// returns its result summary with the command's output. `job run` exits 0 for
// a failed or refused job too, so the job's own state is the gate: a seat that
// never ran has no result to read, and the test fails naming why rather than
// dereferencing a nil result and aborting the whole test binary.
func runSeatJobSummary(t *testing.T, store *db.Store, home, jobID string) (summary, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run([]string{"job", "run", jobID, "--home", home}, &out, &errOut); code != 0 {
		t.Fatalf("job run exit code = %d\nstdout=%s\nstderr=%s", code, out.String(), errOut.String())
	}
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("decode job payload: %v", err)
	}
	if !payload.ReadOnlySeat {
		t.Fatalf("the job did not run as a read-only seat, so this test measured the wrong environment: payload=%+v", payload)
	}
	if job.State != string(workflow.JobSucceeded) || payload.Result == nil {
		t.Fatalf("the seat job did not succeed: state %q, result %v\npayload=%+v\nstdout=%s\nstderr=%s", job.State, payload.Result, payload, out.String(), errOut.String())
	}
	return payload.Result.Summary, out.String(), errOut.String()
}

func randomSeatHex(t *testing.T) string {
	t.Helper()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(suffix[:])
}

// skipUnlessSeatTempParentWritable skips a test about the SHORT seat temp dir
// itself when seatTempParent() refuses writes. That happens only inside a seat
// started by a daemon from before #2314: its TMPDIR is a long <cache root>/tmp
// and /tmp is denied, so nested seats fall back to <cache root>/tmp (they still
// run) and no gmr-<8 hex> dir can exist for these tests to examine.
func skipUnlessSeatTempParentWritable(t *testing.T) {
	t.Helper()
	parent := seatTempParent()
	probe, err := os.MkdirTemp(parent, "gitmoot-seat-probe-")
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
		t.Skipf("seat temp parent %s is not writable (a seat from a daemon predating #2314?), so seats fall back to the cache-root temp and there is no short seat temp dir to test: %v", parent, err)
	}
	if err != nil {
		t.Fatalf("probe seat temp parent %s: %v", parent, err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
}

func writeSeatFixtureFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSeatModuleProxy lays out one module version in GOPROXY file:// form.
func writeSeatModuleProxy(t *testing.T, proxy, module, version string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(proxy, module, "@v")
	writeSeatFixtureFile(t, dir, "list", version+"\n")
	writeSeatFixtureFile(t, dir, version+".info", `{"Version":"`+version+`","Time":"2026-01-01T00:00:00Z"}`)
	writeSeatFixtureFile(t, dir, version+".mod", files["go.mod"])
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range files {
		entry, err := writer.Create(module + "@" + version + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writeSeatFixtureFile(t, dir, version+".zip", archive.String())
}

// writeSeatModuleSums writes the checkout's go.sum with the host go command, in
// a throwaway module cache, so the fixture is a module a reviewer could build.
func writeSeatModuleSums(t *testing.T, checkout, proxy string) {
	t.Helper()
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(),
		"GOPROXY=file://"+proxy,
		"GOSUMDB=off",
		"GOFLAGS=-modcacherw",
		"GOMODCACHE="+t.TempDir(),
		"GOTOOLCHAIN=local",
		"GOWORK=off",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, output)
	}
}
