package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func agentPresetDelivery(t *testing.T, home, name string) string {
	t.Helper()
	var mode string
	if err := withStore(home, func(store *db.Store) error {
		agent, err := store.GetAgent(context.Background(), name)
		if err != nil {
			return err
		}
		mode = agent.PresetDelivery
		return nil
	}); err != nil {
		t.Fatalf("read agent %q: %v", name, err)
	}
	return mode
}

// TestAgentSubscribePresetDeliveryFlag covers the #33 config surface end to end:
// subscribe --preset-delivery persists the mode, an omitted flag defaults to full,
// agent update flips it in place, and an invalid value is rejected.
func TestAgentSubscribePresetDeliveryFlag(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--capability", "review",
		"--preset-delivery", "referenced",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit = %d, stderr=%s", code, stderr.String())
	}
	if got := agentPresetDelivery(t, home, "audit"); got != db.PresetDeliveryReferenced {
		t.Fatalf("subscribed preset delivery = %q, want %q", got, db.PresetDeliveryReferenced)
	}

	// Default (no flag) is full.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "planner",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440002",
		"--role", "coordinator",
		"--repo", "jerryfane/gitmoot",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe planner exit = %d, stderr=%s", code, stderr.String())
	}
	if got := agentPresetDelivery(t, home, "planner"); got != db.PresetDeliveryFull {
		t.Fatalf("default preset delivery = %q, want %q", got, db.PresetDeliveryFull)
	}

	// agent update flips the mode in place.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "update", "audit", "--home", home, "--preset-delivery", "auto"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("update exit = %d, stderr=%s", code, stderr.String())
	}
	if got := agentPresetDelivery(t, home, "audit"); got != db.PresetDeliveryAuto {
		t.Fatalf("updated preset delivery = %q, want %q", got, db.PresetDeliveryAuto)
	}

	// Invalid mode is rejected on both subscribe and update.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"agent", "update", "audit", "--home", home, "--preset-delivery", "loose"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("update invalid mode exit = %d, want 2 (stderr=%s)", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "bad",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440003",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--capability", "review",
		"--preset-delivery", "loose",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("subscribe invalid mode exit = %d, want 2 (stderr=%s)", code, stderr.String())
	}
}

// TestAgentReSubscribePreservesPresetDelivery guards the review finding: re-running
// `agent subscribe` on an existing agent WITHOUT --preset-delivery must not silently
// reset the sticky mode back to full. A brand-new subscribe without the flag still
// defaults to full.
func TestAgentReSubscribePreservesPresetDelivery(t *testing.T) {
	home := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440001",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--capability", "review",
		"--preset-delivery", "auto",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe exit = %d, stderr=%s", code, stderr.String())
	}
	if got := agentPresetDelivery(t, home, "audit"); got != db.PresetDeliveryAuto {
		t.Fatalf("subscribed preset delivery = %q, want %q", got, db.PresetDeliveryAuto)
	}

	// Re-subscribe to refresh the session with NO --preset-delivery flag: the
	// previously-chosen auto mode must survive.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440099",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("re-subscribe exit = %d, stderr=%s", code, stderr.String())
	}
	if got := agentPresetDelivery(t, home, "audit"); got != db.PresetDeliveryAuto {
		t.Fatalf("re-subscribe preset delivery = %q, want %q (should be preserved)", got, db.PresetDeliveryAuto)
	}

	// Explicitly passing --preset-delivery on re-subscribe still overrides.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "audit",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440099",
		"--role", "reviewer",
		"--repo", "jerryfane/gitmoot",
		"--capability", "review",
		"--preset-delivery", "full",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("re-subscribe override exit = %d, stderr=%s", code, stderr.String())
	}
	if got := agentPresetDelivery(t, home, "audit"); got != db.PresetDeliveryFull {
		t.Fatalf("re-subscribe override preset delivery = %q, want %q", got, db.PresetDeliveryFull)
	}

	// A brand-new agent without the flag still defaults to full.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"agent", "subscribe", "planner",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440002",
		"--role", "coordinator",
		"--repo", "jerryfane/gitmoot",
		"--capability", "ask",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("subscribe planner exit = %d, stderr=%s", code, stderr.String())
	}
	if got := agentPresetDelivery(t, home, "planner"); got != db.PresetDeliveryFull {
		t.Fatalf("new agent default preset delivery = %q, want %q", got, db.PresetDeliveryFull)
	}
}
