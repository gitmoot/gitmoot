package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

const (
	// Configuration states, not delivery states: each has its own alert path,
	// so a sender is never told about them as a stuck delivery (#2574).
	messageRecipientUnregisteredReason = "recipient role is not registered; no destination guessed"
	messageRecipientNoSeatReason       = "recipient has no current seat; message remains in inbox"

	// messageDeferralNoticeAfter is how long a message's wake may stay
	// undelivered, measured from the wake row's creation, before its sender
	// gets one system notice saying so (#2574).
	messageDeferralNoticeAfter = 15 * time.Minute
	// messageDeferralNoticeMaxAge bounds the notice to rows younger than this.
	// The first daemon after an upgrade would otherwise announce weeks of old
	// backlog at once; a live daemon notices a row long before it is this old.
	messageDeferralNoticeMaxAge = 24 * time.Hour
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
			return messageRecipientStatus{reason: messageRecipientUnregisteredReason}, nil
		}
		if role.Pane == "" {
			return messageRecipientStatus{reason: messageRecipientNoSeatReason}, nil
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

func messageOrgLoader(home string) func() (config.OrgConfig, error) {
	return func() (config.OrgConfig, error) {
		return config.LoadOrg(config.Paths{ConfigFile: resolveConfigFile(home)})
	}
}

// noticeDeferredMessages tells each sender, once, that a message it sent is
// still undelivered (#2574), so nobody types into a recipient's pane by hand.
// It issues no read of its own: it filters the drain's pending projection in
// memory, reads the org registry only when some row is owed a notice, and
// writes once per owed row. reasons overrides a row's projected last_error
// with the one this drain pass just recorded. loadOrg is the registry reader
// of a delivery the pass already resolved; when the pass resolved none, the
// notice resolves one itself, and only if a notice is owed.
//
// BEST EFFORT, like recordUnroutableWake: a failure is logged and the notice
// stays owed, because it is stamped in the same transaction that sends it.
func noticeDeferredMessages(
	ctx context.Context,
	store wakeOutboxStore,
	pending []db.WakeOutboxObligation,
	reasons map[int64]string,
	now time.Time,
	loadOrg func() (config.OrgConfig, error),
	resolve replyWakeDeliveryResolver,
) {
	notifier, ok := store.(interface {
		SendMessageDeferralNotice(context.Context, int64, string, string, time.Time) (bool, error)
	})
	if !ok {
		return
	}
	type owedNotice struct {
		row    db.WakeOutboxObligation
		reason string
		wait   time.Duration
	}
	var owed []owedNotice
	for _, row := range pending {
		reason, recorded := reasons[row.ID]
		if !recorded {
			reason = row.LastError
		}
		if wait, ok := messageDeferralNoticeOwed(row, strings.TrimSpace(reason), now); ok {
			owed = append(owed, owedNotice{row: row, reason: strings.TrimSpace(reason), wait: wait})
		}
	}
	if len(owed) == 0 {
		return
	}
	if loadOrg == nil && resolve != nil {
		delivery, err := resolve(ctx)
		if err != nil {
			slog.Warn("message deferral notice: resolve delivery", "error", err)
			return
		}
		loadOrg = delivery.loadOrg
	}
	if loadOrg == nil {
		return
	}
	cfg, err := loadOrg()
	if err != nil {
		slog.Warn("message deferral notice: load org registry", "error", err)
		return
	}
	for _, notice := range owed {
		sender := strings.TrimSpace(notice.row.MessageSender)
		if _, registered := cfg.Role(sender); !registered {
			continue
		}
		body := messageDeferralNoticeBody(notice.row.MessageID, strings.TrimSpace(notice.row.TargetRole), notice.wait, notice.reason)
		if _, err := notifier.SendMessageDeferralNotice(ctx, notice.row.ID, sender, body, now); err != nil {
			slog.Warn("message deferral notice failed",
				"wake_outbox_row", notice.row.ID, "message", notice.row.MessageID, "sender", sender, "error", err)
		}
	}
}

// messageDeferralNoticeOwed reports whether a pending row's sender is owed a
// notice, and how long the row has waited. Only a role-to-other-role message
// qualifies: system notices and self-messages never do, nor does a directive
// whose delivery is already proven. Neither does a row held for a
// configuration state with its own alert path: an unregistered or seatless
// recipient, or a row the drain never evaluated for delivery (no reason
// recorded), which is an unroutable wake recorded by recordUnroutableWake.
func messageDeferralNoticeOwed(row db.WakeOutboxObligation, reason string, now time.Time) (time.Duration, bool) {
	sender := strings.TrimSpace(row.MessageSender)
	if row.MessageID <= 0 || row.DeferralNoticeAt != "" || sender == "" || sender == db.MessageSystemSender ||
		strings.EqualFold(sender, strings.TrimSpace(row.TargetRole)) {
		return 0, false
	}
	if row.DirectivePhase == db.WakeOutboxDirectivePhaseCompletion || row.DirectivePhase == db.WakeOutboxDirectivePhaseTerminal {
		return 0, false
	}
	if reason == "" || reason == messageRecipientUnregisteredReason || reason == messageRecipientNoSeatReason {
		return 0, false
	}
	createdAt, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
	if err != nil {
		return 0, false
	}
	wait := now.Sub(createdAt)
	return wait, wait > messageDeferralNoticeAfter && wait <= messageDeferralNoticeMaxAge
}

func messageDeferralNoticeBody(messageID int64, recipient string, wait time.Duration, reason string) string {
	waited := strings.TrimSuffix(wait.Truncate(time.Minute).String(), "0s")
	return fmt.Sprintf(
		"Your message %d to %s has waited %s undelivered. Current reason: %s. "+
			"It stays queued and will be delivered when %s can take it; there is no need to type it into %s's pane.",
		messageID, recipient, waited, reason, recipient, recipient)
}
