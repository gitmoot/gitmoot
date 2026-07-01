package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// withStubbedDaemonChild swaps the child-spawn seam so the real start/restart
// command body can be driven end-to-end without launching an actual daemon
// process. Mirrors the claudeAuthEnvLookup seam.
func withStubbedDaemonChild(t *testing.T) {
	t.Helper()
	prev := startDaemonChildFn
	startDaemonChildFn = func(home, poll string, workers int, watchSkillOptReviews, watchIssues bool, scheduler, repo, session string, state daemonState, workDir string) (daemonMeta, error) {
		return daemonMeta{PID: 424242, LogFile: filepath.Join(home, "daemon.log")}, nil
	}
	t.Cleanup(func() { startDaemonChildFn = prev })
}

// TestDaemonStartPathEmitsAuthWarning_E2E drives the REAL start/restart command
// body (runDaemonStartWithWorkDirRestart: arg parse -> paths -> pid check ->
// warn -> spawn) with the child spawn stubbed, proving the #559 auth-drop
// warning is actually WIRED into the (re)start path and reaches stderr with the
// correct wording per scenario — not just the isolated warn function.
//
// Mutation check (manual): deleting the warnIfDaemonStartLosesClaudeAuth call in
// runDaemonStartWithWorkDirRestart makes every wantWarn subtest fail (no WARNING
// on stderr), confirming this exercises the real wiring rather than the function
// in isolation.
func TestDaemonStartPathEmitsAuthWarning_E2E(t *testing.T) {
	withStubbedDaemonChild(t)

	cases := []struct {
		name         string
		env          map[string]string
		restart      bool
		priorHadAuth bool
		wantWarn     bool
		wantDrop     bool
	}{
		{name: "plain_start_no_auth_neutral", env: map[string]string{}, restart: false, priorHadAuth: false, wantWarn: true, wantDrop: false},
		{name: "restart_prior_had_auth_drop", env: map[string]string{}, restart: true, priorHadAuth: true, wantWarn: true, wantDrop: true},
		{name: "restart_no_prior_auth_neutral", env: map[string]string{}, restart: true, priorHadAuth: false, wantWarn: true, wantDrop: false},
		{name: "auth_ready_silent", env: map[string]string{runtime.ClaudeOAuthTokenEnv: "x"}, restart: true, priorHadAuth: true, wantWarn: false, wantDrop: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withClaudeAuthLookup(t, tc.env)
			home := t.TempDir()
			var stdout, stderr bytes.Buffer

			code := runDaemonStartWithWorkDirRestart([]string{"--home", home}, "", tc.restart, tc.priorHadAuth, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("start returned code %d; stderr=%q", code, stderr.String())
			}
			// The start path ran through to the (stubbed) spawn.
			if !strings.Contains(stdout.String(), "daemon started") {
				t.Fatalf("expected the start path to complete; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}

			out := stderr.String()
			if got := strings.Contains(out, "WARNING"); got != tc.wantWarn {
				t.Fatalf("WARNING present = %v, want %v; stderr=%q", got, tc.wantWarn, out)
			}
			if !tc.wantWarn {
				return
			}
			if got := strings.Contains(out, "DROP"); got != tc.wantDrop {
				t.Fatalf("DROP wording = %v, want %v; stderr=%q", got, tc.wantDrop, out)
			}
			// Every warning, drop or neutral, must name the token and note the
			// blast radius so the operator can act.
			if !strings.Contains(out, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(out, "ALL subscribed repos") {
				t.Fatalf("warning should name the token and note it affects all repos; stderr=%q", out)
			}
		})
	}
}
