package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// openTerminalForTest returns a REAL terminal for tests that must pass the
// --watch terminal guard. /dev/ptmx is a pty master, so TCGETS succeeds on it
// and no pty helper dependency is needed.
//
// #1838: tests used to open /dev/null for this, which worked only because the
// guard was a character-device check. Anything relying on that would now skip
// silently rather than fail, which is why this helper exists in one place.
func openTerminalForTest(t *testing.T) *os.File {
	t.Helper()
	file, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx on this host, so no genuine terminal is available: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// THE #1838 FAILING CONTROL, driven through the production dashboard entry
// path: `gitmoot dashboard --watch >/dev/null` must refuse with exit 2 rather
// than entering the loop. Before the fix this passed the guard - /dev/null is a
// character device - and the watch loop ran until interrupted against a sink
// nobody could see, which is why the original report measured exit=124 from a
// timeout kill rather than a refusal.
func TestDashboardWatchRefusesDevNull(t *testing.T) {
	home := dashboardTestHome(t)
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	original := runDashboardWatchFn
	t.Cleanup(func() { runDashboardWatchFn = original })
	started := 0
	runDashboardWatchFn = func(io.Writer, string, bool, time.Duration) int {
		started++
		return 0
	}

	var stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--watch"}, devNull, &stderr)

	// The loop assertion is the one that matters: exit 2 with a started loop
	// would still hang in production.
	if started != 0 {
		t.Fatalf("the watch loop started against %s; it ran %d time(s)", os.DevNull, started)
	}
	if code != 2 {
		t.Fatalf("Run = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dashboard --watch requires a terminal") {
		t.Fatalf("stderr = %q, want the TERMINAL guard's own message", stderr.String())
	}
}

// THE POSITIVE CONTROL, and it is the half a tightened bound usually breaks: a
// genuine terminal must still reach the watch loop. Without this, a guard that
// refused every writer would pass the case above.
func TestDashboardWatchAcceptsARealTerminal(t *testing.T) {
	home := dashboardTestHome(t)
	tty := openTerminalForTest(t)

	original := runDashboardWatchFn
	t.Cleanup(func() { runDashboardWatchFn = original })
	started := 0
	var seenInterval time.Duration
	runDashboardWatchFn = func(_ io.Writer, _ string, _ bool, interval time.Duration) int {
		started++
		seenInterval = interval
		return 0
	}

	var stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--watch", "--interval", "3s"}, tty, &stderr)

	if started != 1 {
		t.Fatalf("the watch loop ran %d time(s) on a genuine terminal, want 1; stderr=%q", started, stderr.String())
	}
	if code != 0 {
		t.Fatalf("Run = %d, want 0; stderr=%q", code, stderr.String())
	}
	if seenInterval != 3*time.Second {
		t.Fatalf("interval = %s, want 3s: the guard must not disturb the flags it gates", seenInterval)
	}
}

// A pipe is the ordinary non-terminal case and must keep being refused - the
// fix narrows what counts as a terminal and must not widen it anywhere.
func TestDashboardWatchRefusesAPipe(t *testing.T) {
	home := dashboardTestHome(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})
	// Drain so a full pipe buffer can never be mistaken for the hang under test.
	go func() { _, _ = io.Copy(io.Discard, reader) }()

	original := runDashboardWatchFn
	t.Cleanup(func() { runDashboardWatchFn = original })
	started := 0
	runDashboardWatchFn = func(io.Writer, string, bool, time.Duration) int {
		started++
		return 0
	}

	var stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--watch"}, writer, &stderr)

	if started != 0 {
		t.Fatalf("the watch loop started against a pipe; it ran %d time(s)", started)
	}
	if code != 2 {
		t.Fatalf("Run = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dashboard --watch requires a terminal") {
		t.Fatalf("stderr = %q, want the TERMINAL guard's own message", stderr.String())
	}
}
