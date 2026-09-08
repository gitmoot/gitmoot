package cli

import (
	"bytes"
	"strings"
	"testing"
)

// #1820. Before this command the only way to warm-reload was
// `systemctl --user kill -s HUP gitmoot-daemon`, whose `--kill-whom` defaults to
// `all`, so the signal reached every dispatched runtime binary and killed it
// with exit 129 while the daemon itself survived. These tests pin the two
// properties that make the command a fix rather than a rename.

// THE LOAD-BEARING ONE: the signal must reach exactly the recorded daemon pid.
// A negative pid signals the whole PROCESS GROUP, which is a different shape of
// the same defect - every job subprocess would receive it.
func TestDaemonReloadSignalsOnlyTheRecordedDaemonPID(t *testing.T) {
	home := t.TempDir()
	paths := stageLiveDaemon(t, home, "test-version", "test-commit")
	want, _, err := currentDaemonPID(daemonProcessState(paths))
	if err != nil {
		t.Fatalf("currentDaemonPID: %v", err)
	}

	var got []int
	restore := daemonReloadSignal
	daemonReloadSignal = func(pid int) error {
		got = append(got, pid)
		return nil
	}
	defer func() { daemonReloadSignal = restore }()

	var stdout, stderr bytes.Buffer
	if code := runDaemon([]string{"reload", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon reload exit=%d stderr=%q", code, stderr.String())
	}
	if len(got) != 1 {
		t.Fatalf("signal delivered %d times, want exactly 1: %v", len(got), got)
	}
	if got[0] != want {
		t.Fatalf("signalled pid %d, want the recorded daemon pid %d", got[0], want)
	}
	if got[0] <= 0 {
		t.Fatalf("signalled pid %d: a non-positive pid targets a process GROUP, which is the #1820 defect", got[0])
	}
	if !strings.Contains(stdout.String(), "daemon reload signalled pid") {
		t.Fatalf("stdout did not confirm the reload: %q", stdout.String())
	}
}

// A reload that reloaded nothing must not report success. `stop` is satisfied by
// an absent daemon; `reload` is not - an operator (or a deploy script) running
// it wants a live process to re-read its config, and exit 0 would let a config
// change look applied when no process saw it.
func TestDaemonReloadWithNoDaemonFailsInsteadOfReportingSuccess(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	restore := daemonReloadSignal
	daemonReloadSignal = func(pid int) error {
		t.Fatalf("signalled pid %d with no daemon recorded", pid)
		return nil
	}
	defer func() { daemonReloadSignal = restore }()

	code := runDaemon([]string{"reload", "--home", home}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("daemon reload exit=0 with no daemon running; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "daemon not running") {
		t.Fatalf("stderr did not name the cause: %q", stderr.String())
	}
}

// The usage text has to steer an operator away from the cgroup-wide command,
// because that is the only path that existed before and the one muscle memory
// reaches for. Asserting the WARNING is present, not merely the verb.
func TestDaemonUsageDocumentsReloadAndWarnsAgainstSystemctlKill(t *testing.T) {
	var stdout bytes.Buffer
	if code := runDaemon([]string{"--help"}, &stdout, &stdout); code != 0 {
		t.Fatalf("daemon --help exit=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "gitmoot daemon reload") {
		t.Fatalf("usage does not list reload: %q", out)
	}
	for _, want := range []string{"systemctl kill", "--kill-whom", "129"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage omits %q, so nothing warns an operator off the cgroup-wide path: %q", want, out)
		}
	}
}
