//go:build linux

package sandbox

import (
	"strings"
	"testing"
)

func TestRuntimeHostReadFilesIncludesOpenSSLConfig(t *testing.T) {
	for _, path := range runtimeHostReadFiles {
		if path == "/etc/ssl/openssl.cnf" {
			return
		}
	}
	t.Fatal("runtime host file grants omit /etc/ssl/openssl.cnf; Node-based review runtimes cannot initialize TLS")
}

// TestReadableRootsGrantsProcfs pins the runtime BOOTSTRAP grant that strict
// read-path mode dropped. Without it the Bun-based Claude/Kimi binaries abort
// and codex's bwrap cannot read /proc/sys/kernel/overflowuid, so every read-only
// review dies before doing any work. Asserted through readableRoots (the
// function execSandbox actually calls) rather than by inspecting a literal list.
func TestReadableRootsGrantsProcfs(t *testing.T) {
	roots, err := readableRoots([]string{t.TempDir()})
	if err != nil {
		t.Fatalf("readableRoots returned error: %v", err)
	}
	for _, root := range roots {
		if root == "/proc" {
			return
		}
	}
	t.Fatalf("readableRoots = %v, want /proc among them; review runtimes cannot bootstrap without procfs", roots)
}

func TestReadableRootsExcludeOperatorManagedLocalPrefix(t *testing.T) {
	roots, err := readableRoots([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		if root == "/usr/local" || strings.HasPrefix(root, "/usr/local/") {
			t.Fatalf("readableRoots contains recursive operator-managed root %q; runtimes and Go must use daemon-owned staged copies", root)
		}
	}
}
