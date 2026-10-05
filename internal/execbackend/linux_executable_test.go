package execbackend

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalLinuxELF is an ELF64 little-endian executable header for machine,
// optionally with one PT_INTERP program header naming interpreter. The
// verifier does not run it.
func minimalLinuxELF(machine uint16, interpreter string) []byte {
	elf := make([]byte, 64)
	copy(elf, []byte("\x7fELF"))
	elf[4], elf[5], elf[6], elf[7] = 2, 1, 1, 3
	binary.LittleEndian.PutUint16(elf[16:], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(elf[18:], machine)
	binary.LittleEndian.PutUint32(elf[20:], 1)
	binary.LittleEndian.PutUint16(elf[52:], 64)
	if interpreter == "" {
		return elf
	}
	name := interpreter + "\x00"
	elf = append(elf, make([]byte, 56+len(name))...)
	binary.LittleEndian.PutUint64(elf[32:], 64)  // program header offset
	binary.LittleEndian.PutUint16(elf[54:], 56)  // program header size
	binary.LittleEndian.PutUint16(elf[56:], 1)   // one program header
	binary.LittleEndian.PutUint32(elf[64:], 3)   // PT_INTERP
	binary.LittleEndian.PutUint64(elf[72:], 120) // interpreter offset
	binary.LittleEndian.PutUint64(elf[96:], uint64(len(name)))
	copy(elf[120:], name)
	return elf
}

const (
	elfMachineAARCH64 = 183
	elfMachineX86_64  = 62
)

// Each guest architecture accepts only its own Linux ELF and loader, and the
// verified file is handed back at byte zero so the upload is the checked bytes.
func TestOpenLinuxExecutableMatchesGuestArchitecture(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		arch, label, interpreter string
		machine, wrongMachine    uint16
	}{
		{GuestArchARM64, "ARM64", "/lib/ld-linux-aarch64.so.1", elfMachineAARCH64, elfMachineX86_64},
		{GuestArchAMD64, "AMD64", "/lib64/ld-linux-x86-64.so.2", elfMachineX86_64, elfMachineAARCH64},
	} {
		path := filepath.Join(dir, "omp-"+tc.arch)
		want := minimalLinuxELF(tc.machine, tc.interpreter)
		if err := os.WriteFile(path, want, 0o700); err != nil {
			t.Fatal(err)
		}
		file, err := OpenLinuxExecutable(path, tc.arch)
		if err != nil {
			t.Fatalf("%s: valid ELF refused: %v", tc.arch, err)
		}
		upload, err := io.ReadAll(file)
		file.Close()
		if err != nil || !bytes.Equal(upload, want) {
			t.Fatalf("%s: verified upload did not start at byte zero: %v", tc.arch, err)
		}

		if err := os.WriteFile(path, minimalLinuxELF(tc.wrongMachine, ""), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenLinuxExecutable(path, tc.arch); err == nil || !strings.Contains(err.Error(), "must be a Linux "+tc.label+" ELF executable") {
			t.Fatalf("%s: wrong-architecture ELF accepted: %v", tc.arch, err)
		}

		if err := os.WriteFile(path, minimalLinuxELF(tc.machine, "/bin/sh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenLinuxExecutable(path, tc.arch); err == nil || !strings.Contains(err.Error(), "unsupported Linux "+tc.label+" interpreter") {
			t.Fatalf("%s: unsafe interpreter accepted: %v", tc.arch, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenLinuxExecutable(path, tc.arch); err == nil || !strings.Contains(err.Error(), "executable regular file") {
			t.Fatalf("%s: non-executable file accepted: %v", tc.arch, err)
		}
	}
	if _, err := OpenLinuxExecutable(filepath.Join(dir, "missing"), GuestArchAMD64); err == nil || !os.IsNotExist(err) {
		t.Fatalf("missing file = %v; want not-exist", err)
	}
	if _, err := OpenLinuxExecutable("omp", GuestArchAMD64); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path = %v; want a refusal", err)
	}
}
