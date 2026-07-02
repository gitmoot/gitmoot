package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// withClaudeAuthLookup swaps the injectable auth-readiness seam for the duration
// of a test so the #559 warning can be exercised without real host credentials.
func withClaudeAuthLookup(t *testing.T, env map[string]string) {
	t.Helper()
	prev := claudeAuthEnvLookup
	claudeAuthEnvLookup = func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
	t.Cleanup(func() { claudeAuthEnvLookup = prev })
}

func TestWarnIfDaemonStartLosesClaudeAuth_WarnsWhenNotReady(t *testing.T) {
	withClaudeAuthLookup(t, map[string]string{}) // no Claude auth in the inherited env

	for _, restart := range []bool{false, true} {
		var buf bytes.Buffer
		warnIfDaemonStartLosesClaudeAuth(&buf, restart, restart)
		out := buf.String()
		if !strings.Contains(out, "WARNING") {
			t.Fatalf("restart=%v: expected a WARNING, got %q", restart, out)
		}
		if !strings.Contains(out, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("restart=%v: warning should name the token, got %q", restart, out)
		}
		if !strings.Contains(out, "ALL subscribed repos") {
			t.Fatalf("restart=%v: warning should note it affects all repos, got %q", restart, out)
		}
	}
}

func TestWarnIfDaemonStartLosesClaudeAuth_RestartWordingReflectsDrop(t *testing.T) {
	withClaudeAuthLookup(t, map[string]string{})

	var start, restart bytes.Buffer
	warnIfDaemonStartLosesClaudeAuth(&start, false, false)
	warnIfDaemonStartLosesClaudeAuth(&restart, true, true) // prior daemon confirmed authed

	if strings.Contains(start.String(), "DROP") {
		t.Fatalf("plain-start wording should not claim a DROP, got %q", start.String())
	}
	if !strings.Contains(restart.String(), "DROP") || !strings.Contains(restart.String(), "LOSE") {
		t.Fatalf("restart wording should reflect the auth being dropped/lost, got %q", restart.String())
	}
}

// TestWarnIfDaemonStartLosesClaudeAuth_RestartWithoutPriorAuthUsesNeutralWording
// guards the #559 review fix: a restart must NOT assert a DROP/LOSE when the prior
// daemon's Claude auth is unknown or was absent (e.g. a Codex/Kimi-only box, or an
// unreadable prior env). It still warns that the new daemon comes up without auth,
// but with the neutral plain-start wording.
func TestWarnIfDaemonStartLosesClaudeAuth_RestartWithoutPriorAuthUsesNeutralWording(t *testing.T) {
	withClaudeAuthLookup(t, map[string]string{}) // no Claude auth in the inherited env

	var buf bytes.Buffer
	warnIfDaemonStartLosesClaudeAuth(&buf, true, false) // restart, but prior auth unknown/absent
	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("expected a WARNING even without prior auth, got %q", out)
	}
	if strings.Contains(out, "DROP") || strings.Contains(out, "LOSE") ||
		strings.Contains(out, "the previous daemon had") {
		t.Fatalf("restart without confirmed prior auth must not claim a DROP/LOSE, got %q", out)
	}
	if !strings.Contains(out, "starting WITHOUT Claude auth") {
		t.Fatalf("expected neutral plain-start wording, got %q", out)
	}
}

func TestWarnIfDaemonStartLosesClaudeAuth_SilentWhenReady(t *testing.T) {
	// Any single Claude credential makes auth ready; the warning must stay silent.
	for _, key := range []string{
		runtime.ClaudeOAuthTokenEnv,
		runtime.AnthropicAPIKeyEnv,
		runtime.AnthropicAuthTokenEnv,
	} {
		withClaudeAuthLookup(t, map[string]string{key: "x"})
		for _, restart := range []bool{false, true} {
			var buf bytes.Buffer
			warnIfDaemonStartLosesClaudeAuth(&buf, restart, restart)
			if buf.Len() != 0 {
				t.Fatalf("key=%s restart=%v: expected silence when auth ready, got %q", key, restart, buf.String())
			}
		}
	}
}

func TestDaemonRestartEnvCaveat_MentionsEnvReinheritance(t *testing.T) {
	if !strings.Contains(daemonRestartEnvCaveat, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("footgun caveat should name the token: %q", daemonRestartEnvCaveat)
	}
	if !strings.Contains(daemonRestartEnvCaveat, "RE-INHERITS") {
		t.Fatalf("footgun caveat should note env re-inheritance: %q", daemonRestartEnvCaveat)
	}
	if !strings.Contains(daemonRestartEnvCaveat, "scheduler state") {
		t.Fatalf("footgun caveat should note scheduler-state reset: %q", daemonRestartEnvCaveat)
	}
}

func TestDaemonUsage_ClarifiesRepoScope(t *testing.T) {
	var buf bytes.Buffer
	printDaemonUsage(&buf)
	out := buf.String()
	if !strings.Contains(out, "SCOPES the daemon to a SINGLE repo") {
		t.Fatalf("usage should clarify --repo scopes the daemon to a single repo, got:\n%s", out)
	}
	if !strings.Contains(out, "Omit --repo to supervise ALL enabled") {
		t.Fatalf("usage should clarify omitting --repo supervises all enabled repos, got:\n%s", out)
	}
}
