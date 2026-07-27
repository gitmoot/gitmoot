package workflow

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRuntimeProcessLivenessUsesRecordedStartTime(t *testing.T) {
	procRoot := t.TempDir()
	const pid = 1234
	processDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.Mkdir(processDir, 0o700); err != nil {
		t.Fatalf("mkdir process dir: %v", err)
	}
	// Fields after comm begin at field 3. Nineteen values place "777" at
	// field 22 (starttime).
	stat := "1234 (runtime worker) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 777 20\n"
	if err := os.WriteFile(filepath.Join(processDir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	identity, err := runtimeProcessIdentity(pid, procRoot)
	if err != nil {
		t.Fatalf("runtimeProcessIdentity: %v", err)
	}
	if identity != "777" {
		t.Fatalf("identity = %q, want 777", identity)
	}
	if live, known := runtimeProcessLiveness(pid, identity, procRoot); !live || !known {
		t.Fatalf("matching identity liveness = (%v, %v), want (true, true)", live, known)
	}
	if live, known := runtimeProcessLiveness(pid, "776", procRoot); live || !known {
		t.Fatalf("recycled PID liveness = (%v, %v), want (false, true)", live, known)
	}
	if live, known := runtimeProcessLiveness(9999, "1", procRoot); live || !known {
		t.Fatalf("missing PID liveness = (%v, %v), want (false, true)", live, known)
	}
	if live, known := runtimeProcessLiveness(pid, "", procRoot); live || known {
		t.Fatalf("missing identity liveness = (%v, %v), want unknown", live, known)
	}
	if live, known := runtimeProcessLiveness(9999, "", procRoot); live || known {
		t.Fatalf("missing PID with no recorded identity liveness = (%v, %v), want unknown", live, known)
	}
	if live, known := runtimeProcessLiveness(pid, identity, filepath.Join(procRoot, "no-proc")); live || known {
		t.Fatalf("unavailable process table liveness = (%v, %v), want unknown", live, known)
	}
}
