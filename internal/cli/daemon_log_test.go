package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
)

func TestDaemonStatusWarnsWhenRunningLogPredatesStart(t *testing.T) {
	home := t.TempDir()
	paths := stageLiveDaemon(t, home, "dev-log-test", "logtest0")
	stubOnDiskBuild(t, "dev-log-test", "logtest0")
	state := daemonProcessState(paths)

	started := time.Date(2026, 7, 27, 8, 30, 0, 0, time.UTC)
	lastWrite := started.Add(-6 * 24 * time.Hour)
	if err := os.WriteFile(state.LogFile, []byte("old daemon output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(state.LogFile, lastWrite, lastWrite); err != nil {
		t.Fatal(err)
	}
	setDaemonStartedAt(t, state, started.Format(time.RFC3339))

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon status exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"WARNING:",
		state.LogFile,
		"last write: " + lastWrite.Format(time.RFC3339),
		"daemon started: " + started.Format(time.RFC3339),
		"if running under systemd",
		"journalctl --user -u gitmoot-daemon -f",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("daemon status warning missing %q:\n%s", want, out)
		}
	}
}

func TestDaemonStatusFreshLogPreservesExistingOutput(t *testing.T) {
	home := t.TempDir()
	paths := stageLiveDaemon(t, home, "dev-log-test", "logtest0")
	stubOnDiskBuild(t, "dev-log-test", "logtest0")
	state := daemonProcessState(paths)

	started := time.Date(2026, 7, 27, 8, 30, 0, 0, time.UTC)
	lastWrite := started
	if err := os.WriteFile(state.LogFile, []byte("current daemon output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(state.LogFile, lastWrite, lastWrite); err != nil {
		t.Fatal(err)
	}
	setDaemonStartedAt(t, state, started.Format(time.RFC3339))

	var fresh bytes.Buffer
	if code := Run([]string{"daemon", "status", "--home", home}, &fresh, &fresh); code != 0 {
		t.Fatalf("fresh daemon status exit = %d: %s", code, fresh.String())
	}
	if strings.Contains(fresh.String(), "journalctl") || strings.Contains(fresh.String(), "advertised log") {
		t.Fatalf("fresh daemon log produced a staleness warning:\n%s", fresh.String())
	}

	// An older metadata record has no comparable StartedAt, which was the status
	// behavior before this check. A current log must produce that exact output.
	setDaemonStartedAt(t, state, "")
	var legacy bytes.Buffer
	if code := Run([]string{"daemon", "status", "--home", home}, &legacy, &legacy); code != 0 {
		t.Fatalf("legacy daemon status exit = %d: %s", code, legacy.String())
	}
	if fresh.String() != legacy.String() {
		t.Fatalf("fresh log changed existing status output:\nfresh:\n%s\nlegacy:\n%s", fresh.String(), legacy.String())
	}
}

func TestDaemonStatusStoppedKeepsPlainLogLine(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	state := daemonProcessState(paths)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"daemon", "status", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon status exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "daemon stopped\n") || !strings.Contains(out, "log: "+state.LogFile+"\n") {
		t.Fatalf("stopped status lost existing lines:\n%s", out)
	}
	if strings.Contains(out, "WARNING") || strings.Contains(out, "journalctl") {
		t.Fatalf("stopped daemon received a log staleness verdict:\n%s", out)
	}
}

func TestDoctorSurfacesRunningDaemonStaleLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths := stageLiveDaemon(t, home, "dev-log-test", "logtest0")
	stubOnDiskBuild(t, "dev-log-test", "logtest0")
	state := daemonProcessState(paths)
	setDaemonStartedAt(t, state, time.Date(2026, 7, 27, 8, 30, 0, 0, time.UTC).Format(time.RFC3339))

	var out bytes.Buffer
	runDoctor(nil, &out, &out)
	line := doctorCheckLine(t, out.String(), "daemon log")
	for _, want := range []string{"warn", "missing", "if running under systemd", "journalctl --user -u gitmoot-daemon -f"} {
		if !strings.Contains(line, want) {
			t.Fatalf("doctor daemon log line missing %q: %q", want, line)
		}
	}
}

func setDaemonStartedAt(t *testing.T, state daemonState, startedAt string) {
	t.Helper()
	meta, err := readDaemonMeta(state)
	if err != nil {
		t.Fatal(err)
	}
	meta.StartedAt = startedAt
	if err := writeDaemonState(state, meta); err != nil {
		t.Fatal(err)
	}
}
