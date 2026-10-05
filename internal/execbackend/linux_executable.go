package execbackend

import (
	"debug/elf"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Guest architectures a host-side Linux executable can be uploaded for.
const (
	GuestArchARM64 = "arm64"
	GuestArchAMD64 = "amd64"
)

type linuxGuestArch struct {
	label        string
	machine      elf.Machine
	interpreters []string
}

var linuxGuestArches = map[string]linuxGuestArch{
	GuestArchARM64: {label: "ARM64", machine: elf.EM_AARCH64,
		interpreters: []string{"/lib/ld-linux-aarch64.so.1", "/lib/ld-musl-aarch64.so.1", "/lib64/ld-linux-aarch64.so.1"}},
	GuestArchAMD64: {label: "AMD64", machine: elf.EM_X86_64,
		interpreters: []string{"/lib64/ld-linux-x86-64.so.2", "/lib/ld-musl-x86_64.so.1", "/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2"}},
}

// OpenLinuxExecutable opens path and verifies it is an executable regular
// Linux ELF file for the guest architecture arch ("arm64" or "amd64") with a
// standard dynamic loader, if any. The returned file is positioned at byte
// zero, so the caller uploads exactly the bytes that were verified.
func OpenLinuxExecutable(path, arch string) (*os.File, error) {
	want, ok := linuxGuestArches[arch]
	if !ok {
		return nil, fmt.Errorf("unsupported guest architecture %q: want %q or %q", arch, GuestArchARM64, GuestArchAMD64)
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("must name an absolute Linux %s executable, got %q", want.label, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	verified := false
	defer func() {
		if !verified {
			file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("%q must be an executable regular file", path)
	}
	executable, err := elf.NewFile(file)
	if err != nil {
		return nil, fmt.Errorf("%q is not an ELF executable: %w", path, err)
	}
	if executable.Class != elf.ELFCLASS64 || executable.Machine != want.machine ||
		(executable.Type != elf.ET_EXEC && executable.Type != elf.ET_DYN) ||
		(executable.OSABI != elf.ELFOSABI_NONE && executable.OSABI != elf.ELFOSABI_LINUX) {
		return nil, fmt.Errorf("%q must be a Linux %s ELF executable (found %s %s)", path, want.label, executable.Class, executable.Machine)
	}
	for _, program := range executable.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		interpreter, err := io.ReadAll(program.Open())
		if err != nil {
			return nil, fmt.Errorf("read %s ELF interpreter: %w", want.label, err)
		}
		name := strings.TrimRight(string(interpreter), "\x00")
		if !slices.Contains(want.interpreters, name) {
			return nil, fmt.Errorf("%q uses unsupported Linux %s interpreter %q", path, want.label, name)
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind verified executable: %w", err)
	}
	verified = true
	return file, nil
}
