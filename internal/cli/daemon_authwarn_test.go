package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestDaemonRestartEnvCaveatMentionsPerDeliveryAuth(t *testing.T) {
	if !strings.Contains(daemonRestartEnvCaveat, runtimeAuthFileName) {
		t.Fatalf("caveat should name the authoritative auth file: %q", daemonRestartEnvCaveat)
	}
	if !strings.Contains(daemonRestartEnvCaveat, "does not require a restart") {
		t.Fatalf("caveat should explain per-delivery rotation: %q", daemonRestartEnvCaveat)
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
