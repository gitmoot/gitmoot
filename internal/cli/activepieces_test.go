package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestResolveBridgeBind(t *testing.T) {
	cases := []struct {
		goos        string
		wantAddr    string
		wantRemote  bool
		wantURLHost string
	}{
		{goos: "linux", wantAddr: "172.17.0.1:8791", wantRemote: true, wantURLHost: "host.docker.internal:8791"},
		{goos: "darwin", wantAddr: "127.0.0.1:8791", wantRemote: false, wantURLHost: "host.docker.internal:8791"},
		{goos: "windows", wantAddr: "127.0.0.1:8791", wantRemote: false, wantURLHost: "host.docker.internal:8791"},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			addr, allowRemote, reachableURL := resolveBridgeBind(tc.goos)
			if addr != tc.wantAddr || allowRemote != tc.wantRemote || !strings.Contains(reachableURL, tc.wantURLHost) {
				t.Fatalf("resolveBridgeBind(%q) = %q, %v, %q", tc.goos, addr, allowRemote, reachableURL)
			}
			if tc.goos == "linux" && bridgeHostIsLoopback(strings.Split(addr, ":")[0]) {
				t.Fatalf("linux address %q is loopback", addr)
			}
		})
	}
}

func TestIsExactPieceVersion(t *testing.T) {
	exact := []string{"0.1.2", "1.0.0", "12.34.56"}
	for _, v := range exact {
		if !isExactPieceVersion(v) {
			t.Errorf("isExactPieceVersion(%q) = false, want true", v)
		}
	}
	ranged := []string{"", "~0.1.2", "^0.1.0", "0.1", "latest", "0.1.2-beta", "v0.1.2"}
	for _, v := range ranged {
		if isExactPieceVersion(v) {
			t.Errorf("isExactPieceVersion(%q) = true, want false", v)
		}
	}
}

func TestActivepiecesTemplatesList(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runActivepieces([]string{"templates", "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"webhook-run-pipeline",
		"Receive a webhook and enqueue a named Gitmoot pipeline.",
		"gmail-imap-ask-agent",
		"send an SMTP acknowledgement.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("templates list output missing %q:\n%s", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
