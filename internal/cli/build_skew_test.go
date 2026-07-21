package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/buildinfo"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/update"
)

// stageLiveDaemon makes the home look like it hosts a running daemon whose
// recorded build is version/commit. Liveness is verified against the real process
// table (processLooksLikeDaemon matches argv), so the test process impersonates
// the daemon: its own pid and argv are what we record. That also pins
// meta.Executable to the test binary, which is why the on-disk build probe goes
// through the execBinaryBuildFn seam rather than exec'ing it.
func stageLiveDaemon(t *testing.T, home, version, commit string) config.Paths {
	t.Helper()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	state := daemonProcessState(paths)
	meta := daemonMeta{
		PID:        os.Getpid(),
		Args:       os.Args[1:],
		Executable: os.Args[0],
		LogFile:    state.LogFile,
		Version:    version,
		Commit:     commit,
	}
	if err := writeDaemonState(state, meta); err != nil {
		t.Fatal(err)
	}
	pid, _, err := currentDaemonPID(state)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("staged daemon not seen as live: pid=%d err=%v", pid, err)
	}
	return paths
}

// stubOnDiskBuild makes the binary at the daemon's path report this build.
func stubOnDiskBuild(t *testing.T, version, commit string) {
	t.Helper()
	previous := execBinaryBuildFn
	execBinaryBuildFn = func(context.Context, string) (string, string) { return version, commit }
	t.Cleanup(func() { execBinaryBuildFn = previous })
}

// stubUpdateCheck keeps Health off the network (the package seam; see
// dashboard_web_test.go).
func stubUpdateCheck(t *testing.T, latest string) {
	t.Helper()
	previous := updateCheckFn
	updateCheckFn = func(_ context.Context, current buildinfo.Info, _ string) (update.CheckResult, error) {
		return update.CheckResult{
			CurrentVersion: current.Version,
			LatestVersion:  latest,
			NoRelease:      latest == "",
		}, nil
	}
	t.Cleanup(func() { updateCheckFn = previous })
}

func TestDaemonStateRoundTripsTheRunningBuild(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	state := daemonProcessState(paths)
	if err := writeDaemonState(state, daemonMetaWithCurrentBuild(daemonMeta{PID: os.Getpid(), LogFile: state.LogFile})); err != nil {
		t.Fatal(err)
	}
	meta, err := readDaemonMeta(state)
	if err != nil {
		t.Fatal(err)
	}
	current := buildinfo.Current()
	if meta.Version != current.Version || meta.Commit != current.Commit {
		t.Fatalf("daemon.json build = %q/%q, want this process's %q/%q", meta.Version, meta.Commit, current.Version, current.Commit)
	}
}

// A daemon started by an older gitmoot recorded no build. "Unknown" must never
// be reported as skew, or every upgrade false-alarms until the daemon happens to
// be restarted.
func TestLegacyDaemonWithoutRecordedBuildIsUnknownNotSkew(t *testing.T) {
	home := t.TempDir()
	paths := stageLiveDaemon(t, home, "", "")
	stubOnDiskBuild(t, "v0.9.1", "aaaaaaaa")

	meta, err := readDaemonMeta(daemonProcessState(paths))
	if err != nil {
		t.Fatal(err)
	}
	if daemonBuildLabel(meta) != "unknown" {
		t.Fatalf("legacy build label = %q, want unknown", daemonBuildLabel(meta))
	}
	if check := daemonBuildCheck(paths); !check.OK {
		t.Fatalf("legacy daemon reported as skewed: %q", check.Detail)
	}
}

// daemonBuildStatus must never resolve daemon.pid/daemon.json RELATIVE to the
// cwd: currentDaemonPID deletes a pidfile it cannot parse, and a zero Paths (a
// missing HOME, which the call sites tolerate) would point it at a stranger's
// file in the working directory.
func TestDaemonBuildStatusRefusesZeroPaths(t *testing.T) {
	status := daemonBuildStatus(config.Paths{})
	if status.DaemonRunning {
		t.Fatal("zero Paths reported a running daemon")
	}
	if check := daemonBuildCheck(config.Paths{}); !check.OK {
		t.Fatalf("zero Paths reported skew: %q", check.Detail)
	}
}

// The skew check must compare the daemon PROCESS against the binary at the
// daemon's own path — not against whatever binary is invoking `doctor`, which
// may not be the daemon's binary at all.
func TestBuildCheckComparesDaemonAgainstBinaryAtItsOwnPath(t *testing.T) {
	home := t.TempDir()
	paths := stageLiveDaemon(t, home, "dev-old", "0000000000")

	stubOnDiskBuild(t, "dev-new", "1111111111")
	check := daemonBuildCheck(paths)
	if check.OK {
		t.Fatal("a daemon running dev-old with dev-new on disk was reported as current")
	}
	for _, want := range []string{"dev-old", "dev-new", "restart the daemon"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("skew detail %q missing %q", check.Detail, want)
		}
	}

	// Same build on disk: no warning, regardless of what binary runs this test.
	stubOnDiskBuild(t, "dev-old", "0000000000")
	if check := daemonBuildCheck(paths); !check.OK {
		t.Fatalf("daemon running the on-disk build was reported as skewed: %q", check.Detail)
	}
}

// Health must report the build the daemon PROCESS runs (recorded), while the
// update badge stays relative to the binary ON DISK. Conflating them makes the
// badge sticky: after `gitmoot update`, it would keep claiming an update is
// available until someone restarted the daemon.
func TestHealthReportsRunningBuildWhileUpdateBadgeTracksTheBinaryOnDisk(t *testing.T) {
	home := t.TempDir()
	stageLiveDaemon(t, home, "v0.9.0", "old00000")
	stubOnDiskBuild(t, "v0.9.1", "new11111")
	stubUpdateCheck(t, "v0.9.1")

	ds := &webDataSource{home: home}
	health, err := ds.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !health.Daemon.Running || health.Daemon.Version != "v0.9.0" {
		t.Fatalf("daemon build = %q (running=%t), want the RECORDED v0.9.0", health.Daemon.Version, health.Daemon.Running)
	}
	if health.Update == nil {
		t.Fatal("no update info")
	}
	if health.Update.Current != "v0.9.1" {
		t.Fatalf("update badge current = %q, want the on-disk v0.9.1", health.Update.Current)
	}
	if health.Update.UpdateAvailable {
		t.Fatal("update badge is stuck on: the binary on disk is already the latest release")
	}
}

// /api/health reported only the daemon's build, so a dashboard process serving
// stale code looked perfectly current. It must also report its OWN build — and,
// shadowing the module's handler, keep every list non-nil as that contract
// promises.
func TestHealthEndpointAddsServingBuildAndKeepsListsNonNil(t *testing.T) {
	home := t.TempDir()
	stageLiveDaemon(t, home, "dev-stale-daemon", "stale000")
	stubOnDiskBuild(t, "dev-stale-daemon", "stale000")
	stubUpdateCheck(t, "")

	ds := &webDataSource{home: home}
	recorder := httptest.NewRecorder()
	ds.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	var payload struct {
		Daemon struct {
			Version       string `json:"version"`
			VersionSource string `json:"versionSource"`
		} `json:"daemon"`
		Server struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		} `json:"server"`
		Locks          *json.RawMessage `json:"locks"`
		ResourceLocks  *json.RawMessage `json:"resourceLocks"`
		Stuck          *json.RawMessage `json:"stuck"`
		RecentFailures *json.RawMessage `json:"recentFailures"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Daemon.Version != "dev-stale-daemon" {
		t.Fatalf("daemon build = %q", payload.Daemon.Version)
	}
	if payload.Daemon.VersionSource != "recorded" {
		t.Fatalf("daemon version source = %q, want recorded", payload.Daemon.VersionSource)
	}
	current := buildinfo.Current()
	if payload.Server.Version != current.Version || payload.Server.Commit != current.Commit {
		t.Fatalf("server build = %q/%q, want this process's own %q/%q", payload.Server.Version, payload.Server.Commit, current.Version, current.Commit)
	}
	if payload.Server.Version == payload.Daemon.Version {
		t.Fatal("server and daemon builds are indistinguishable; a stale dashboard would stay invisible")
	}
	// The module's handler coerces these to []; shadowing it must not regress the
	// documented shape to null.
	for name, raw := range map[string]*json.RawMessage{
		"locks":          payload.Locks,
		"resourceLocks":  payload.ResourceLocks,
		"stuck":          payload.Stuck,
		"recentFailures": payload.RecentFailures,
	} {
		if raw == nil || string(*raw) == "null" {
			t.Fatalf("%s serialized as null; the contract promises an array", name)
		}
	}
}

// A daemon started before build recording was added has no trustworthy running
// version. The binary now sitting at its path may already have been replaced,
// so substituting that on-disk version would turn unknown into a false fact.
func TestHealthEndpointDoesNotSubstituteOnDiskVersionForLegacyDaemon(t *testing.T) {
	home := t.TempDir()
	stageLiveDaemon(t, home, "", "")
	stubOnDiskBuild(t, "dev-fresh-on-disk", "freshdisk")
	stubUpdateCheck(t, "")

	ds := &webDataSource{home: home}
	recorder := httptest.NewRecorder()
	ds.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	var payload struct {
		Daemon struct {
			Version       string `json:"version"`
			VersionSource string `json:"versionSource"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Daemon.Version != "" {
		t.Fatalf("legacy daemon build = %q, want unknown (not the on-disk build)", payload.Daemon.Version)
	}
	if payload.Daemon.VersionSource != "unknown" {
		t.Fatalf("daemon version source = %q, want unknown", payload.Daemon.VersionSource)
	}
}

// In the incident scenario, the serving dashboard and the freshly replaced
// on-disk daemon binary are the same build while the still-running legacy daemon
// is not. Unknown must not become a false server==daemon healthy match.
func TestHealthEndpointUnknownDaemonVersionCannotFalseMatchServer(t *testing.T) {
	home := t.TempDir()
	stageLiveDaemon(t, home, "", "")
	current := buildinfo.Current()
	stubOnDiskBuild(t, current.Version, current.Commit)
	stubUpdateCheck(t, "")

	ds := &webDataSource{home: home}
	recorder := httptest.NewRecorder()
	ds.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	var payload struct {
		Daemon struct {
			Version       string `json:"version"`
			VersionSource string `json:"versionSource"`
		} `json:"daemon"`
		Server struct {
			Version string `json:"version"`
		} `json:"server"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Server.Version == "" {
		t.Fatal("serving build version is empty; test cannot prove the false-match regression")
	}
	if payload.Daemon.Version != "" || payload.Daemon.VersionSource != "unknown" {
		t.Fatalf("legacy daemon = version %q source %q, want empty/unknown", payload.Daemon.Version, payload.Daemon.VersionSource)
	}
	if payload.Server.Version == payload.Daemon.Version {
		t.Fatalf("unknown daemon falsely matches serving build %q", payload.Server.Version)
	}
}

// E2E on an isolated home (NEVER /root/.gitmoot): a daemon running an older build
// than the binary at its path must be called out by BOTH `gitmoot daemon status`
// and `gitmoot doctor` — and, once they agree, by neither.
func TestBuildSkewSurfacedByDoctorAndDaemonStatusE2E(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // runDoctor/runDaemonStatus resolve config.DefaultPaths() from $HOME
	stageLiveDaemon(t, home, "dev-stale-000", "stale000sha")
	stubOnDiskBuild(t, "dev-fresh-999", "fresh999sha")

	var status bytes.Buffer
	if code := runDaemonStatus(nil, &status, &status); code != 0 {
		t.Fatalf("daemon status exit = %d: %s", code, status.String())
	}
	statusOut := status.String()
	if !strings.Contains(statusOut, "build: dev-stale-000") {
		t.Fatalf("daemon status did not print the running build:\n%s", statusOut)
	}
	for _, want := range []string{"WARNING", "dev-stale-000", "dev-fresh-999", "restart the daemon"} {
		if !strings.Contains(statusOut, want) {
			t.Fatalf("daemon status skew warning missing %q:\n%s", want, statusOut)
		}
	}

	var doctorOut bytes.Buffer
	runDoctor(nil, &doctorOut, &doctorOut)
	buildLine := doctorCheckLine(t, doctorOut.String(), "build")
	if !strings.Contains(buildLine, "dev-stale-000") || !strings.Contains(buildLine, "dev-fresh-999") {
		t.Fatalf("doctor build line did not name both builds: %q", buildLine)
	}

	// The operator restarts: the daemon now runs the binary on disk.
	stageLiveDaemon(t, home, "dev-fresh-999", "fresh999sha")

	var agreed bytes.Buffer
	if code := runDaemonStatus(nil, &agreed, &agreed); code != 0 {
		t.Fatalf("daemon status exit = %d: %s", code, agreed.String())
	}
	if strings.Contains(agreed.String(), "WARNING") {
		t.Fatalf("matching builds still warned:\n%s", agreed.String())
	}
	var healthy bytes.Buffer
	runDoctor(nil, &healthy, &healthy)
	if line := doctorCheckLine(t, healthy.String(), "build"); !strings.Contains(line, "ok") {
		t.Fatalf("matching builds did not report ok: %q", line)
	}
}

func doctorCheckLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), name+" ") {
			return line
		}
	}
	t.Fatalf("doctor printed no %q check:\n%s", name, out)
	return ""
}
