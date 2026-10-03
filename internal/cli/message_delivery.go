package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/org"
)

// Check before claiming an outbox row. Busy/offline mail remains pending, not
// an attempted delivery, so a later safe boundary can drain it without replay.
func messageRecipientReadiness(home string) func(context.Context, string) (string, error) {
	return func(ctx context.Context, roleName string) (string, error) {
		cfg, err := config.LoadOrg(config.Paths{ConfigFile: resolveConfigFile(home)})
		if err != nil {
			return "", err
		}
		role, exists := cfg.Role(roleName)
		if !exists {
			return "recipient role is not registered; no destination guessed", nil
		}
		if role.Pane == "" {
			return "recipient has no current seat; message remains in inbox", nil
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		snapshot, err := orgProviderSnapshot(probe, cfg)
		if err != nil {
			return "recipient safety state unavailable; notification deferred", nil
		}
		binding, ok := snapshot.PaneBindings[role.Name]
		if !ok || binding.PaneID == "" || binding.Ambiguous {
			return "recipient is offline or its binding is unresolved", nil
		}
		state, ok := snapshot.States[role.Name]
		if !ok {
			return "recipient safety state unknown; notification deferred", nil
		}
		switch state.State {
		case org.StateIdle, org.StateDone:
			return "", nil
		default:
			return fmt.Sprintf("recipient is %s; waiting for a safe notification boundary", state.State), nil
		}
	}
}
