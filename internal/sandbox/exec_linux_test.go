//go:build linux

package sandbox

import (
	"os"
	"slices"
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

// TestReadableRootsGrantOnlyRequestedAndFixedSystemRoots pins the invariant
// ruling 122157 actually established: NOTHING is promoted into the read set at
// runtime. Every root is either one the caller requested (a daemon-owned staged
// tree) or a member of the fixed, root-owned system list.
//
// AN EARLIER VERSION OF THIS TEST ASSERTED THE WRONG THING. It required
// /usr/local to be absent, on the reasoning that operators install there. CI
// refuted it: GitHub runners install Go at /usr/local/go, so a seat hit
// "go: Permission denied", exit 126 - reintroducing the #1918 symptom this work
// removes. The ruling deleted the PROMOTION of a root chosen at runtime from
// PATH, not the fixed system set, and all three measured exposures were profile
// or install roots. Encoding a claim the ruling never made cost a real
// capability, so the assertion now names the boundary instead of a prefix.
func TestReadableRootsGrantOnlyRequestedAndFixedSystemRoots(t *testing.T) {
	requested := t.TempDir()
	roots, err := readableRoots([]string{requested})
	if err != nil {
		t.Fatal(err)
	}
	fixed := map[string]bool{
		"/bin": true, "/sbin": true, "/lib": true, "/lib64": true, "/dev": true,
		"/usr/bin": true, "/usr/sbin": true, "/usr/lib": true, "/usr/lib64": true,
		"/usr/libexec": true, "/usr/share": true,
		"/usr/local/bin": true, "/usr/local/sbin": true, "/usr/local/lib": true,
		"/usr/local/lib64": true, "/usr/local/share": true,
		"/etc/ssl/certs": true, "/etc/pki": true, "/proc": true,
	}
	for _, root := range roots {
		if root == requested || fixed[root] {
			continue
		}
		t.Errorf("readableRoots granted %q, which is neither the requested staged root nor a fixed system root: a runtime-chosen root has been promoted back into the read set", root)
	}
	// The requested root must survive, or a passing test could mean the read set
	// dropped the staged tree the seat actually needs.
	if !slices.Contains(roots, requested) {
		t.Fatalf("readableRoots = %v, missing the requested root %q", roots, requested)
	}
	// A HOME-shaped root must never appear unrequested: that is the profile
	// exposure class (kimi's ~/.kimi-code, claude's ~/.local/share/claude).
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, root := range roots {
			if root == home || strings.HasPrefix(root, home+string(os.PathSeparator)) {
				t.Errorf("readableRoots granted %q inside the operator home %q", root, home)
			}
		}
	}
}
