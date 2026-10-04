package cli

import (
	"context"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
)

// Check before claiming an outbox row. Offline, unbound and unsupported
// recipients keep their mail pending, not attempted, so nothing is replayed.
// A working OMP recipient is not held back: its inbox add-on admits the notice
// at the next step boundary or defers it itself.
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
			return "recipient state unavailable; notification deferred", nil
		}
		binding, ok := snapshot.PaneBindings[role.Name]
		if !ok || binding.PaneID == "" || binding.Ambiguous {
			return "recipient is offline or its binding is unresolved", nil
		}
		// Claude Code and Codex collect mail from their own turn hooks; other
		// runtimes have no notification transport. Neither is claimed here.
		return cockpit.NotificationCapability(snapshot.Sessions[role.Name].Agent), nil
	}
}
