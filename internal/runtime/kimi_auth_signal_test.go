package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// TestKimiDeliverNeverLabelsAFailureAsAuth pins #1857's fix: kimi no longer
// classifies auth from the child's TEXT, in either direction.
//
// Arm 1 is the measured defect. Kimi relays its TOOLS' output through the same
// stdout/stderr it reports its own failures on, so the pre-fix substring test
// (isKimiAuthFailure at d3fd2877:internal/runtime/kimi.go:300-306, matching
// "login"/"authenticate"/"unauthorized"/"authentication" anywhere in
// stdout+stderr) labelled a read-only sandbox denial as a kimi auth failure
// because the GitHub CLI printed "To log in, run: gh auth login" inside it. The
// operator was then told to re-login a session that was never the problem.
//
// Arm 2 is the DELIBERATE loss and the contract, not a regret: a genuine kimi
// authorization rejection is now ALSO returned unwrapped. After this change
// gitmoot does not claim to detect kimi auth from text at all - kimi's
// stream-json envelope carries no error or auth field and subprocess.Result
// carries no exit code, so there is nothing kimi's own machinery offers to
// classify from, and a test of another program's vocabulary is worse than no
// test. Both arms assert the SAME contract: whatever the child printed, the
// error is the child's own cause with no auth wrapper and no login remedy.
func TestKimiDeliverNeverLabelsAFailureAsAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		stderr string
	}{
		{
			name:   "another tool's login text is not kimi auth",
			stdout: "running gh pr view\nTo log in, run: gh auth login\n",
			stderr: "bwrap: Permission denied\nopen /repo/.git: Permission denied\n",
		},
		{
			name:   "a genuine kimi authorization rejection is also unwrapped",
			stdout: "",
			stderr: "kimi: request failed: 401 unauthorized: authentication required\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{
				results: []subprocess.Result{{Stdout: tc.stdout, Stderr: tc.stderr}},
				errs:    []error{errors.New("exit status 1")},
			}
			adapter := KimiAdapter{Runner: runner}
			agent := Agent{Name: "kimi", Role: "implementer", Runtime: KimiRuntime, RuntimeRef: FreshRefForJob("job-1")}
			_, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "work"})
			if err == nil {
				t.Fatal("Deliver returned nil error for a failed child")
			}
			got := err.Error()
			for _, forbidden := range []string{"Kimi Code authentication required", "restart the Gitmoot daemon so it inherits the session"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("error carries gitmoot's own auth label %q: %q", forbidden, got)
				}
			}
			if !strings.Contains(got, "exit status 1") {
				t.Fatalf("error dropped the child's own cause: %q", got)
			}
			// commandError prefers stderr, falling back to stdout, so the
			// surviving detail is the child's real first-hand text.
			detail := strings.TrimSpace(tc.stderr)
			if detail == "" {
				detail = strings.TrimSpace(tc.stdout)
			}
			if !strings.Contains(got, strings.SplitN(detail, "\n", 2)[0]) {
				t.Fatalf("error dropped the child's own output: %q, want it to carry %q", got, detail)
			}
		})
	}
}
