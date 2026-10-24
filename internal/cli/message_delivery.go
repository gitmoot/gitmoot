package cli

import (
	"context"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/org"
)

type messageRecipientStatus struct {
	state  org.LifecycleState
	reason string
}

// Check before claiming. A recipient's activity controls batching, while
// runtime admission still protects drafts, dialogs and operator input.
func messageRecipientReadiness(home string) func(context.Context, string) (messageRecipientStatus, error) {
	return func(ctx context.Context, roleName string) (messageRecipientStatus, error) {
		cfg, err := config.LoadOrg(config.Paths{ConfigFile: resolveConfigFile(home)})
		if err != nil {
			return messageRecipientStatus{}, err
		}
		role, exists := cfg.Role(roleName)
		if !exists {
			return messageRecipientStatus{reason: "recipient role is not registered; no destination guessed"}, nil
		}
		if role.Pane == "" {
			return messageRecipientStatus{reason: "recipient has no current seat; message remains in inbox"}, nil
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		snapshot, err := orgProviderSnapshot(probe, cfg)
		if err != nil {
			return messageRecipientStatus{reason: "recipient state unavailable; notification deferred"}, nil
		}
		binding, ok := snapshot.PaneBindings[role.Name]
		if !ok || binding.PaneID == "" || binding.Ambiguous {
			return messageRecipientStatus{reason: "recipient is offline or its binding is unresolved"}, nil
		}
		status := messageRecipientStatus{state: snapshot.States[role.Name].State}
		switch status.state {
		case org.StateIdle, org.StateWorking:
			// Claude/Codex collect mail from turn hooks, not this transport.
			status.reason = cockpit.NotificationCapability(snapshot.Sessions[role.Name].Agent)
		default:
			status.reason = "recipient activity is not safely deliverable; notification deferred"
		}
		return status, nil
	}
}
