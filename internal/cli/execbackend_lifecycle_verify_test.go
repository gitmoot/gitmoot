package cli

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifiedLinuxARM64OmpRejectsWrongArchitectureAndPreservesUploadBytes(t *testing.T) {
	// Minimal ELF64 little-endian executable header. The verifier does not run it.
	arm64 := make([]byte, 64)
	copy(arm64, []byte("\x7fELF"))
	arm64[4], arm64[5], arm64[6], arm64[7] = 2, 1, 1, 3
	binary.LittleEndian.PutUint16(arm64[16:], 2)   // ET_EXEC
	binary.LittleEndian.PutUint16(arm64[18:], 183) // EM_AARCH64
	binary.LittleEndian.PutUint32(arm64[20:], 1)
	binary.LittleEndian.PutUint16(arm64[52:], 64)

	path := filepath.Join(t.TempDir(), "omp-arm64")
	if err := os.WriteFile(path, arm64, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := verifiedLinuxARM64Omp(path)
	if err != nil {
		t.Fatalf("valid ARM64 ELF refused: %v", err)
	}
	defer file.Close()
	upload, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(upload, arm64) {
		t.Fatalf("verified upload did not start at byte zero: %v, %x", err, upload)
	}

	binary.LittleEndian.PutUint16(arm64[18:], 62) // EM_X86_64
	if err := os.WriteFile(path, arm64, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedLinuxARM64Omp(path); err == nil || !strings.Contains(err.Error(), "ARM64") {
		t.Fatalf("x86_64 ELF accepted: %v", err)
	}

	const forbidden = "/bin/sh\x00"
	arm64 = append(arm64, make([]byte, 56+len(forbidden))...)
	binary.LittleEndian.PutUint16(arm64[18:], 183) // EM_AARCH64
	binary.LittleEndian.PutUint64(arm64[32:], 64)  // program header offset
	binary.LittleEndian.PutUint16(arm64[54:], 56)  // program header size
	binary.LittleEndian.PutUint16(arm64[56:], 1)   // one program header
	binary.LittleEndian.PutUint32(arm64[64:], 3)   // PT_INTERP
	binary.LittleEndian.PutUint64(arm64[72:], 120) // interpreter offset
	binary.LittleEndian.PutUint64(arm64[96:], uint64(len(forbidden)))
	copy(arm64[120:], forbidden)
	if err := os.WriteFile(path, arm64, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedLinuxARM64Omp(path); err == nil || !strings.Contains(err.Error(), "unsupported Linux ARM64 interpreter") {
		t.Fatalf("unsafe ARM64 interpreter accepted: %v", err)
	}
}
