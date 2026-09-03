package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `gitmoot auth probe claude` and `gitmoot doctor` were green for hours while
// every claude review job failed, because they probe the AMBIENT credential and
// a read-only seat authenticates with a staged snapshot of the configured
// config dir. A probe that cannot see the credential under test is a false
// green, so the probe now reports the staged file as well.
func TestSeatCredentialProbeReportsTheStagedCredential(t *testing.T) {
	for name, test := range map[string]struct {
		expiresAt    int64
		refreshToken string
		want         []string
	}{
		"unusable": {
			expiresAt: 0, refreshToken: "",
			want: []string{"UNUSABLE", "no refresh token", "re-logged in"},
		},
		"expired but refreshable": {
			expiresAt: time.Now().UTC().Add(-time.Hour).UnixMilli(), refreshToken: "r",
			want: []string{"EXPIRED", "refresh token present", "discarded with the job"},
		},
		"valid": {
			expiresAt: time.Now().UTC().Add(8 * time.Hour).UnixMilli(), refreshToken: "r",
			want: []string{"valid until"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeClaudeCredential(t, dir, test.expiresAt, test.refreshToken)
			t.Setenv("CLAUDE_CONFIG_DIR", dir)

			var stdout bytes.Buffer
			writeSeatCredentialProbe(&stdout, t.TempDir())
			out := stdout.String()
			if !strings.Contains(out, dir) {
				t.Fatalf("probe output must name the staged file; got %q", out)
			}
			for _, want := range test.want {
				if !strings.Contains(out, want) {
					t.Fatalf("probe output %q must contain %q", out, want)
				}
			}
		})
	}
}

// A credential file with no readable expiry must not be reported as a verdict
// either way: the probe says what it can see and nothing more.
func TestSeatCredentialProbeStatesWhenItCannotAssert(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"host"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	var stdout bytes.Buffer
	writeSeatCredentialProbe(&stdout, t.TempDir())
	out := stdout.String()
	if !strings.Contains(out, "no readable expiry") {
		t.Fatalf("probe output %q must say it cannot assert", out)
	}
	for _, forbidden := range []string{"UNUSABLE", "EXPIRED", "valid until"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("probe output %q must not claim %q without an expiry", out, forbidden)
		}
	}
}
