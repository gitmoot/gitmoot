//go:build linux

package sandbox

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #2360: the isolated boundary pass showed that a descriptor opened by an
// authority process and inherited by the confined runtime stayed writable,
// because the kernel does not revalidate an open file against the new
// profile. These tests exercise the REAL launcher (`gitmoot sandbox-exec`)
// in both managed modes with an accidentally open authority descriptor.

var sealModes = []struct{ name, flag string }{
	{"read-only seat", "--read-only-workdir"},
	{"no-execution seat", "--no-execution"},
}

func (h noExecHarness) modeCommand(flag string, args ...string) *exec.Cmd {
	cmd := h.command(h.probe, args...)
	for i, arg := range cmd.Args {
		if arg == "--no-execution" {
			cmd.Args[i] = flag
			break
		}
	}
	return cmd
}

// protectedFile is outside every grant the launch receives; the test opens it
// read-write, as an authority process would hold it.
func protectedFile(t *testing.T) (string, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "protected-bytes")
	if err := os.WriteFile(path, []byte("ORIGINAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return path, f
}

func assertUnchanged(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ORIGINAL\n" {
		t.Fatalf("protected file changed through an inherited descriptor: %q", data)
	}
}

func TestInheritedAuthorityDescriptorIsWritableWithoutTheLauncher(t *testing.T) {
	// Negative control: the same probe, started directly, CAN write through
	// the inherited descriptor. Without this, a denial below could mean the
	// probe is broken rather than that the launcher sealed the descriptor.
	h := newNoExecHarness(t)
	path, f := protectedFile(t)
	cmd := exec.Command(h.probe, "fdwrite", "4")
	cmd.ExtraFiles = []*os.File{f, f}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "OK fdwrite fd=4") {
		t.Fatalf("control run: %v %q", err, out)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "WRITTEN-THROUGH-INHERITED-FD") {
		t.Fatalf("control did not write through the inherited descriptor: %q", data)
	}
}

func TestManagedLaunchDoesNotPassAnInheritedAuthorityDescriptorE2E(t *testing.T) {
	h := newNoExecHarness(t)
	for _, mode := range sealModes {
		// fd 3 alone is not enough: the no-execution supervisor dup2s its handoff
		// socket onto the child's fd 3 and closes it before exec, so EBADF there
		// would not prove the seal ran. fd 4 survives into the child unless the
		// seal marks it close-on-exec.
		for _, fd := range []string{"3", "4"} {
			t.Run(mode.name+"/fd"+fd, func(t *testing.T) {
				path, f := protectedFile(t)
				cmd := h.modeCommand(mode.flag, "fdwrite", fd)
				cmd.ExtraFiles = []*os.File{f, f} // fds 3 and 4 in the launcher, as an accidental leak
				out, _ := cmd.CombinedOutput()
				got := strings.TrimSpace(string(out))
				if !strings.Contains(got, "DENIED fdwrite errno=9") { // EBADF: the runtime has no such fd
					t.Fatalf("runtime reached the inherited descriptor: %q", got)
				}
				assertUnchanged(t, path)
			})
		}
	}
}

func TestManagedLaunchRefusesARegularFileAsStdioE2E(t *testing.T) {
	h := newNoExecHarness(t)
	for _, mode := range sealModes {
		t.Run(mode.name, func(t *testing.T) {
			path, f := protectedFile(t)
			cmd := h.modeCommand(mode.flag, "fdwrite", "1")
			cmd.Stdout = f // stdout handed over as a descriptor to protected bytes
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err == nil {
				t.Fatalf("launch with a regular file as stdout succeeded; stderr %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), "fd 1 is a regular file") {
				t.Fatalf("stderr = %q, want the fd 1 refusal", stderr.String())
			}
			assertUnchanged(t, path)
		})
	}
}

func TestManagedLaunchKeepsOrdinaryPipedIOE2E(t *testing.T) {
	h := newNoExecHarness(t)
	for _, mode := range sealModes {
		t.Run(mode.name, func(t *testing.T) {
			cmd := h.modeCommand(mode.flag, "stdio")
			cmd.Stdin = strings.NewReader("hello-from-stdin")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("run: %v; stdout %q stderr %q", err, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "OK stdio hello-from-stdin") || !strings.Contains(stderr.String(), "stderr-ok") {
				t.Fatalf("ordinary I/O broken: stdout %q stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}
