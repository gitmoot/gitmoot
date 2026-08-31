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
