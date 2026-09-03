package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

const seatCredentialDoctorCheckName = "read-only seat credential"

// seatCredentialDoctorCheck reports the credential a READ-ONLY SEAT stages,
// which is a different credential from the ambient one every existing check
// measures.
//
// Measured 2026-09-03: `gitmoot doctor` and `gitmoot auth probe claude` were
// both green for hours while every claude review and ask job failed, because
// they probe the ambient token and the seat authenticates with a staged snapshot
// of the configured config dir. The #1810 review called out that fixing only
// `auth probe` left this half of the false green in place. A check that cannot
// see the credential under test is worse than no check, because it is read as
// evidence.
func seatCredentialDoctorCheck() (doctor.Check, bool) {
	sourceDir := selectedRuntimeConfigDir(runtime.ClaudeRuntime)
	origin := "CLAUDE_CONFIG_DIR"
	if sourceDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return doctor.Check{}, false
		}
		sourceDir = filepath.Join(home, ".claude")
		origin = "host default"
	}
	state, ok := inspectReadOnlySeatCredential(runtime.ClaudeRuntime, sourceDir)
	if !ok {
		return doctor.Check{
			Name:     seatCredentialDoctorCheckName,
			OK:       true,
			Required: false,
			Detail:   fmt.Sprintf("claude seat stages from %s (%s); no readable expiry, so nothing is asserted", sourceDir, origin),
		}, true
	}
	if !state.Expired(time.Now().UTC()) {
		return doctor.Check{
			Name:     seatCredentialDoctorCheckName,
			OK:       true,
			Required: false,
			Detail: fmt.Sprintf("claude seat credential declares expiry %s, not yet reached (%s); this reads a field and does not prove it authenticates",
				state.ExpiresAt.Format(time.RFC3339), origin),
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
		Required: false,
		Detail:   fmt.Sprintf("claude seat credential is EXPIRED (%s, %s); %s", expiry, origin, remedy),
	}, true
}
