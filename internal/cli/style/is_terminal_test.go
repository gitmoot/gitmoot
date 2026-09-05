package style

import (
	"bytes"
	"os"
	"testing"
)

// #1838: IsTerminal used to be a character-device check, so /dev/null passed
// it and `dashboard --watch >/dev/null` looped forever against a sink nobody
// could see. These cases pin the discriminator itself: every path below is a
// CHARACTER DEVICE, so a char-device check cannot tell them apart and the old
// implementation returned true for all three.
func TestIsTerminalRejectsCharacterDevicesThatAreNotTerminals(t *testing.T) {
	for _, path := range []string{os.DevNull, "/dev/zero"} {
		t.Run(path, func(t *testing.T) {
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Skipf("cannot open %s on this host: %v", path, err)
			}
			t.Cleanup(func() { _ = file.Close() })

			info, err := file.Stat()
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			// State the premise as an assertion rather than a comment: if this
			// host does not report a character device here, the case is not
			// testing what it claims to.
			if info.Mode()&os.ModeCharDevice == 0 {
				t.Fatalf("%s is not a character device on this host, so it cannot discriminate the two checks", path)
			}
			if IsTerminal(file) {
				t.Fatalf("IsTerminal(%s) = true, want false: a character device is not a terminal", path)
			}
		})
	}
}

// The positive control. Without it the guard could reject every writer and
// still pass the cases above - every bound in this campaign had a version that
// rejected valid input. /dev/ptmx is a real terminal (TCGETS succeeds on the
// master), so this needs no pty helper and no skip on Linux CI.
func TestIsTerminalAcceptsARealTerminal(t *testing.T) {
	file, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx on this host: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	if !IsTerminal(file) {
		t.Fatal("IsTerminal(/dev/ptmx) = false, want true: the guard must still accept a genuine terminal")
	}
}

// A writer with no file descriptor at all - the ordinary pipe/buffer case that
// callers rely on to take the non-interactive branch.
func TestIsTerminalRejectsWritersWithoutADescriptor(t *testing.T) {
	if IsTerminal(&bytes.Buffer{}) {
		t.Fatal("IsTerminal(*bytes.Buffer) = true, want false")
	}
	// A Stat()-only writer is exactly what the old char-device implementation
	// accepted; it has no descriptor, so an ioctl cannot be performed on it.
	if IsTerminal(charDevice{}) {
		t.Fatal("IsTerminal(charDevice{}) = true, want false: Stat() alone cannot prove a terminal")
	}
}
