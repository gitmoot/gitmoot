package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

const seatCredentialDoctorCheckName = "read-only seat credential"

// seatCredentialSource is the config dir a read-only seat will stage from, plus
// where that answer came from.
type seatCredentialSource struct {
	dir    string
	origin string
}

// resolveSeatCredentialSource answers the question the outage turned on: which
// credential do the DAEMON's seats stage?
//
// Reading the invoking shell's CLAUDE_CONFIG_DIR was the wrong instrument. The
// documented failure was "the daemon runs with CLAUDE_CONFIG_DIR=/root/.claude-13
// while a shell has none", so a shell-scoped read reports on a credential no
// review job uses, which is the same false-green shape this check exists to
// close (#1810 review, round 2). Prefer the live daemon's own environment, fall
// back to this process, then to the host default, and always SAY which one was
// used so the reader can tell an authoritative answer from a guess.
func resolveSeatCredentialSource(paths config.Paths) seatCredentialSource {
	if dir, ok := daemonEnvValue(paths, "CLAUDE_CONFIG_DIR"); ok && strings.TrimSpace(dir) != "" {
		return seatCredentialSource{dir: strings.TrimSpace(dir), origin: "daemon CLAUDE_CONFIG_DIR"}
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return seatCredentialSource{dir: dir, origin: "this shell's CLAUDE_CONFIG_DIR; the daemon's was unreadable"}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return seatCredentialSource{}
	}
	return seatCredentialSource{dir: filepath.Join(home, ".claude"), origin: "host default"}
}

// daemonEnvValue reads one variable from the RUNNING daemon's environment. It is
// best-effort by design: no daemon, no pidfile, or a kernel that denies
// /proc/<pid>/environ all return false rather than an error, and the caller then
// says which weaker source it fell back to.
func daemonEnvValue(paths config.Paths, name string) (string, bool) {
	if strings.TrimSpace(paths.Home) == "" {
		return "", false
	}
	pid, _, err := currentDaemonPID(daemonProcessState(paths))
	if err != nil || pid <= 0 {
		return "", false
	}
	raw, err := daemonEnvironReader(pid)
	if err != nil {
		return "", false
	}
	return lookupEnvironValue(raw, name)
}

// daemonEnvironReader is the /proc seam: a test supplies an environ blob instead
// of re-executing itself with a chosen environment.
var daemonEnvironReader = func(pid int) ([]byte, error) {
	return os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
}

// lookupEnvironValue reads one NUL-separated NAME=VALUE entry, the /proc
// environ format. A value may legitimately contain '=' and must survive intact.
func lookupEnvironValue(raw []byte, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if text := string(entry); strings.HasPrefix(text, prefix) {
			return strings.TrimPrefix(text, prefix), true
		}
	}
	return "", false
}

// seatCredentialDoctorCheck reports the credential a READ-ONLY SEAT stages,
// which is a different credential from the ambient one every other check
// measures.
//
// Measured 2026-09-03: `gitmoot doctor` and `gitmoot auth probe claude` were
// both green for hours while every claude review and ask job failed, because
// they probe the ambient token and a seat authenticates with a staged snapshot.
// An UNUSABLE staged credential is Required, so `gitmoot doctor` EXITS NON-ZERO
// on it: emitting it as a warning left automation gating on the exit code seeing
// exactly the green it saw during the outage (#1810 review, round 2).
func seatCredentialDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	source := resolveSeatCredentialSource(paths)
	if source.dir == "" {
		return doctor.Check{}, false
	}
	state, ok := inspectReadOnlySeatCredential(runtime.ClaudeRuntime, source.dir)
	if !ok {
		return doctor.Check{
			Name:     seatCredentialDoctorCheckName,
			OK:       true,
			Required: false,
			Detail:   fmt.Sprintf("claude seat stages from %s (%s); no readable expiry, so nothing is asserted", source.dir, source.origin),
		}, true
	}
	if !state.Expired(time.Now().UTC()) {
		return doctor.Check{
			Name:     seatCredentialDoctorCheckName,
			OK:       true,
			Required: false,
			Detail: fmt.Sprintf("claude seat credential declares expiry %s, not yet reached (%s); this reads a field and does not prove it authenticates",
				state.ExpiresAt.Format(time.RFC3339), source.origin),
		}, true
	}
	expiry := "no expiry recorded (a failed refresh writes this)"
	if !state.ExpiresAt.IsZero() {
		expiry = state.ExpiresAt.Format(time.RFC3339)
	}
	remedy := "re-login that account"
	if state.Refreshable {
		remedy = "the runtime must refresh it, and a seat discards the rotated token with the job"
	}
	return doctor.Check{
		Name:     seatCredentialDoctorCheckName,
		OK:       false,
		Required: true,
		Detail:   fmt.Sprintf("claude seat credential is EXPIRED (%s, %s); %s", expiry, source.origin, remedy),
	}, true
}
