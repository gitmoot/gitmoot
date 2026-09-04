//go:build linux

package sandbox

import "testing"

func TestOptionalSystemToolchainRoot(t *testing.T) {
	tests := []struct {
		name       string
		executable string
		want       string
	}{
		{name: "hosted Go", executable: "/opt/hostedtoolcache/go/1.26.0/x64/bin/go", want: "/opt/hostedtoolcache/go/1.26.0/x64"},
		{name: "local Go", executable: "/usr/local/go/bin/go", want: "/usr/local/go"},
		{name: "Nix Go", executable: "/nix/store/hash-go/bin/go", want: "/nix/store/hash-go"},
		{name: "root tool", executable: "/root/.local/bin/go"},
		{name: "home tool", executable: "/home/user/bin/go"},
		{name: "temporary tool", executable: "/tmp/toolchain/bin/go"},
		{name: "not a bin directory", executable: "/opt/toolchain/go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := optionalSystemToolchainRoot(test.executable); got != test.want {
				t.Fatalf("optionalSystemToolchainRoot(%q) = %q, want %q", test.executable, got, test.want)
			}
		})
	}
}

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
	roots, err := readableRoots([]string{t.TempDir()}, "/bin/sh")
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
