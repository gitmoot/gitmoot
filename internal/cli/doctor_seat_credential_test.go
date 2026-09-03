package cli

import (
	"strings"
	"testing"
	"time"
)

// doctor must report the credential a SEAT stages, not only the ambient one.
// Measured 2026-09-03: doctor and auth probe were both green for hours while
// every claude review and ask job failed, because they probe the ambient token
// and a seat authenticates with a staged snapshot of the configured config dir.
// The #1810 review flagged that fixing only auth probe left half the false
// green in place.
func TestSeatCredentialDoctorCheckReportsTheStagedCredential(t *testing.T) {
	for name, test := range map[string]struct {
		expiresAt    int64
		refreshToken string
		wantOK       bool
		wantPhrase   string
	}{
		"expired without refresh": {expiresAt: 0, refreshToken: "", wantOK: false, wantPhrase: "re-login that account"},
		"expired with refresh":    {expiresAt: time.Now().UTC().Add(-time.Hour).UnixMilli(), refreshToken: "r", wantOK: false, wantPhrase: "discards the rotated token"},
		"live":                    {expiresAt: time.Now().UTC().Add(8 * time.Hour).UnixMilli(), refreshToken: "r", wantOK: true, wantPhrase: "does not prove it authenticates"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeClaudeCredential(t, dir, test.expiresAt, test.refreshToken)
			t.Setenv("CLAUDE_CONFIG_DIR", dir)

			check, ok := seatCredentialDoctorCheck()
			if !ok {
				t.Fatal("check must be emitted")
			}
			if check.OK != test.wantOK {
				t.Fatalf("check.OK = %v, want %v (detail %q)", check.OK, test.wantOK, check.Detail)
			}
			if !strings.Contains(check.Detail, test.wantPhrase) {
				t.Fatalf("detail %q must contain %q", check.Detail, test.wantPhrase)
			}
		})
	}
}

// A file with no readable expiry must not produce a verdict either way.
func TestSeatCredentialDoctorCheckAssertsNothingWithoutAnExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileForTest(dir, `{"claudeAiOauth":{"accessToken":"host"}}`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	check, ok := seatCredentialDoctorCheck()
	if !ok || !check.OK {
		t.Fatalf("check ok=%v OK=%v detail=%q", ok, check.OK, check.Detail)
	}
	if !strings.Contains(check.Detail, "nothing is asserted") {
		t.Fatalf("detail %q must say it asserts nothing", check.Detail)
	}
	for _, forbidden := range []string{"EXPIRED", "declares expiry"} {
		if strings.Contains(check.Detail, forbidden) {
			t.Fatalf("detail %q must not claim %q", check.Detail, forbidden)
		}
	}
}
