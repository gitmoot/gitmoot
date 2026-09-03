package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
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

			check, ok := seatCredentialDoctorCheck(config.Paths{})
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
	if err := writeClaudeCredentialBody(dir, `{"claudeAiOauth":{"accessToken":"host"}}`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	check, ok := seatCredentialDoctorCheck(config.Paths{})
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

// An UNUSABLE staged credential must fail doctor's exit code, not warn. Emitting
// it as Required:false left automation gating on the exit code seeing exactly the
// green it saw during the outage (#1810 review, round 2).
func TestSeatCredentialDoctorCheckIsRequiredWhenUnusable(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, 0, "")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	check, ok := seatCredentialDoctorCheck(config.Paths{})
	if !ok {
		t.Fatal("check must be emitted")
	}
	if check.OK || !check.Required {
		t.Fatalf("expired credential: OK=%v Required=%v, want a required failure", check.OK, check.Required)
	}

	// A live credential must NOT be required, so a healthy box cannot be failed
	// by a field read that proves nothing.
	live := t.TempDir()
	writeClaudeCredential(t, live, time.Now().UTC().Add(8*time.Hour).UnixMilli(), "r")
	t.Setenv("CLAUDE_CONFIG_DIR", live)
	if check, ok := seatCredentialDoctorCheck(config.Paths{}); !ok || !check.OK || check.Required {
		t.Fatalf("live credential: ok=%v OK=%v Required=%v", ok, check.OK, check.Required)
	}
}

// The check must say WHERE its answer came from, so a reader can tell the
// daemon's own environment from a shell-scoped guess.
func TestSeatCredentialDoctorCheckNamesItsSource(t *testing.T) {
	dir := t.TempDir()
	writeClaudeCredential(t, dir, 0, "")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	check, ok := seatCredentialDoctorCheck(config.Paths{})
	if !ok {
		t.Fatal("check must be emitted")
	}
	if !strings.Contains(check.Detail, "this shell") {
		t.Fatalf("detail must name the weaker source it fell back to: %q", check.Detail)
	}
}

// The check must be REGISTERED in `gitmoot doctor`, and an unusable staged
// credential must make the command exit non-zero. Nothing asserted either, so a
// mutant deleting the registration kept the suite green (#1810 review F8).
func TestDoctorRegistersTheSeatCredentialCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	writeClaudeCredential(t, dir, 0, "")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	var out bytes.Buffer
	code := runDoctor([]string{"--json", "--home", home}, &out, &out)
	var checks []struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Required bool   `json:"required"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatalf("doctor --json: %v (%s)", err, out.String())
	}
	var seat *struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Required bool   `json:"required"`
		Detail   string `json:"detail"`
	}
	for i := range checks {
		if checks[i].Name == seatCredentialDoctorCheckName {
			seat = &checks[i]
		}
	}
	if seat == nil {
		t.Fatalf("doctor did not emit the %q check; checks=%+v", seatCredentialDoctorCheckName, checks)
	}
	if seat.Status != "fail" || !seat.Required {
		t.Fatalf("unusable staged credential reported as %q required=%v: %s", seat.Status, seat.Required, seat.Detail)
	}
	if code == 0 {
		t.Fatal("doctor exited 0 with an unusable read-only seat credential")
	}
}

// The DAEMON's CLAUDE_CONFIG_DIR decides which credential seats stage. Reading
// the invoking shell's value reported on a credential no job uses, which is the
// same false green relocated (#1810 review F4).
func TestSeatCredentialDoctorCheckPrefersTheDaemonEnvironment(t *testing.T) {
	home := t.TempDir()
	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatal(err)
	}
	state := daemonProcessState(paths)
	if err := writeDaemonState(state, daemonMetaWithCurrentBuild(daemonMeta{
		PID: os.Getpid(), Args: os.Args[1:], Executable: os.Args[0], LogFile: state.LogFile,
	})); err != nil {
		t.Fatal(err)
	}

	daemonDir := t.TempDir()
	writeClaudeCredential(t, daemonDir, 0, "")
	shellDir := t.TempDir()
	writeClaudeCredential(t, shellDir, time.Now().UTC().Add(8*time.Hour).UnixMilli(), "r")
	t.Setenv("CLAUDE_CONFIG_DIR", shellDir)

	original := daemonEnvironReader
	daemonEnvironReader = func(pid int) ([]byte, error) {
		if pid != os.Getpid() {
			t.Fatalf("read environ of pid %d, want the recorded daemon %d", pid, os.Getpid())
		}
		return []byte("PATH=/usr/bin\x00CLAUDE_CONFIG_DIR=" + daemonDir + "\x00HOME=/root\x00"), nil
	}
	t.Cleanup(func() { daemonEnvironReader = original })

	check, ok := seatCredentialDoctorCheck(paths)
	if !ok {
		t.Fatal("check must be emitted")
	}
	if check.OK || !check.Required {
		t.Fatalf("the daemon's dead credential was not reported: OK=%v required=%v detail=%q", check.OK, check.Required, check.Detail)
	}
	if !strings.Contains(check.Detail, "daemon CLAUDE_CONFIG_DIR") {
		t.Fatalf("detail must name the daemon as its source: %q", check.Detail)
	}

	// An unreadable daemon environment falls back to this shell AND says so.
	daemonEnvironReader = func(int) ([]byte, error) { return nil, os.ErrPermission }
	fallback, ok := seatCredentialDoctorCheck(paths)
	if !ok || !fallback.OK || !strings.Contains(fallback.Detail, "this shell") {
		t.Fatalf("fallback check ok=%v OK=%v detail=%q", ok, fallback.OK, fallback.Detail)
	}
}

// A /proc environ entry is NUL-separated and its value may contain '='.
func TestLookupEnvironValueSplitsOnNULAndKeepsEquals(t *testing.T) {
	raw := []byte("A=1\x00CLAUDE_CONFIG_DIR=/profiles/a=b\x00Z=9")
	got, ok := lookupEnvironValue(raw, "CLAUDE_CONFIG_DIR")
	if !ok || got != "/profiles/a=b" {
		t.Fatalf("value = %q ok=%v, want the full value after the first '='", got, ok)
	}
	if _, ok := lookupEnvironValue(raw, "MISSING"); ok {
		t.Fatal("an absent name must not report a value")
	}
	// A newline-separated blob is NOT the environ format: matching it would make
	// the reader accept a file that is not /proc/<pid>/environ.
	if _, ok := lookupEnvironValue([]byte("A=1\nCLAUDE_CONFIG_DIR=/x\n"), "CLAUDE_CONFIG_DIR"); ok {
		t.Fatal("newline-separated text must not parse as environ")
	}
}
