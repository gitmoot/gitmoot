package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	// replyWakeCoalescingWindow is long enough to absorb a burst of separate
	// short-lived `org escalate` processes while keeping the daemon-tick wake
	// latency small. Rolling windows start at the oldest pending item.
	replyWakeCoalescingWindow = 5 * time.Second
	// A synchronous reply wake gets one 12s Herdr call plus bounded probes and
	// its terminal store write. Thirty seconds leaves margin for a live owner;
	// older attempted rows are crash residue whose delivery outcome is unknown.
	replyWakeAttemptedUnknownAfter = 30 * time.Second
)

type replyWakeDelivery struct {
	sink  events.Sink
	rules []db.EventRule
}

type replyWakeDeliveryResolver func(context.Context) (replyWakeDelivery, error)

// drainReplyWakeOutbox is a store-global daemon operation. It deliberately
// reads durable work before resolving delivery: unreadable outbox state and an
// empty outbox therefore cannot collapse into the same result.
func drainReplyWakeOutbox(ctx context.Context, store *db.Store, now time.Time, resolve replyWakeDeliveryResolver) error {
	if store == nil {
		return errors.New("wake outbox store is required")
	}
	attemptedBefore := now.UTC().Add(-replyWakeAttemptedUnknownAfter)
	obligations, err := store.ListWakeOutboxObligations(ctx, attemptedBefore)
	if err != nil || obligations.Len() == 0 {
		return err
	}

	agedAttempted := len(obligations.AgedAttempted)
	if agedAttempted > 0 {
		expired, err := store.ExpireAgedWakeOutbox(ctx, attemptedBefore, now)
		if err != nil {
			return fmt.Errorf("expire aged attempted wake outbox: %w", err)
		}
		if len(expired) > 0 {
			return fmt.Errorf(
				"wake outbox delivery unknown: expired %d aged attempted rows without retry",
				len(expired),
			)
		}
	}

	groups := make(map[string][]db.WakeOutboxObligation)
	for _, entry := range obligations.Pending {
		if !strings.HasPrefix(entry.CoalesceKey, db.WakeOutboxReplyCoalescePrefix) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(entry.TargetRole)) + "\x00" + entry.CoalesceKey
		groups[key] = append(groups[key], entry)
	}
	if resolve == nil {
		return errors.New("wake outbox delivery resolver is required")
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		items := groups[key]
		for start := 0; start < len(items); {
			startedAt, err := time.Parse(time.RFC3339Nano, items[start].CreatedAt)
			if err != nil {
				return fmt.Errorf("parse wake outbox created_at for row %d: %w", items[start].ID, err)
			}
			deadline := startedAt.Add(replyWakeCoalescingWindow)
			end := start + 1
			for end < len(items) {
				createdAt, err := time.Parse(time.RFC3339Nano, items[end].CreatedAt)
				if err != nil {
					return fmt.Errorf("parse wake outbox created_at for row %d: %w", items[end].ID, err)
				}
				if !createdAt.Before(deadline) {
					break
				}
				end++
			}
			if now.UTC().Before(deadline) {
				// Later rows for the same rolling group cannot be due before its
				// oldest row, so leave the whole tail pending for a future tick.
				break
			}

			batch := items[start:end]
			event := replyWakeEvent(batch, now)
			// Authorization is deliberately batch-scoped and pre-claim. A rule
			// removed while an earlier synchronous batch is delivering cannot
			// authorize this later claim, and no post-claim rule read can strand
			// attempted rows on a routing-store failure.
			delivery, err := resolve(ctx)
			if err != nil {
				return fmt.Errorf("resolve wake outbox delivery: %w", err)
			}
			if delivery.sink == nil {
				break
			}
			if !hasMatchingReplyRule(delivery.rules, event) {
				// Pending remains an explicit never-attempted state. A rule added
				// later can deliver the same batch; nothing is silently erased.
				break
			}
			ids := make([]int64, 0, len(batch))
			for _, entry := range batch {
				ids = append(ids, entry.ID)
			}
			claimed, err := store.ClaimWakeOutbox(ctx, ids, now)
			if err != nil {
				return err
			}
			if claimed {
				event.WakeOutboxIDs = ids
				if err := emitReplyWakeOutboxEvent(ctx, delivery.sink, event, delivery.rules); err != nil {
					return fmt.Errorf("emit claimed reply wake: %w", err)
				}
			}
			start = end
		}
	}
	return wakeOutboxObligationHealth(ctx, store, attemptedBefore)
}

func wakeOutboxObligationHealth(ctx context.Context, store *db.Store, attemptedBefore time.Time) error {
	obligations, err := store.ListWakeOutboxObligations(ctx, attemptedBefore)
	if err != nil {
		return fmt.Errorf("read wake outbox obligations: %w", err)
	}
	pending := len(obligations.Pending)
	agedAttempted := len(obligations.AgedAttempted)
	if pending == 0 && agedAttempted == 0 {
		return nil
	}
	return fmt.Errorf(
		"wake outbox has outstanding obligations: pending=%d aged_attempted=%d",
		pending,
		agedAttempted,
	)
}

type synchronousWakeOutboxSink interface {
	emitWakeOutbox(context.Context, events.Event, []db.EventRule) error
}

func emitReplyWakeOutboxEvent(ctx context.Context, sink events.Sink, event events.Event, rules []db.EventRule) error {
	if durable, ok := sink.(synchronousWakeOutboxSink); ok {
		return durable.emitWakeOutbox(ctx, event, rules)
	}
	return errors.New("wake outbox sink does not support synchronous delivery")
}

func replyWakeEvent(batch []db.WakeOutboxObligation, now time.Time) events.Event {
	oldest := batch[0]
	role := strings.ToLower(strings.TrimSpace(oldest.TargetRole))
	detail := fmt.Sprintf("%d new items, oldest id %s", len(batch), oldest.SourceID)
	event := events.NewEvent(
		events.EventOrgReply,
		"org-reply:"+role,
		oldest.SourceKind+":"+oldest.SourceID,
		"",
		db.WakeOutboxStateAttempted,
		detail,
		now,
		workflow.RedactCommentText,
	)
	event.Cause = "addressed_note"
	event.WakeTargetRole = role
	return event
}

// Deliberately scope-blind pending a later durable-outbox slice: among enabled,
// filter-matching reply rules, WakeRole == WakeTargetRole authorizes the claim.
// An observer-scoped rule can therefore claim for its own addressed role, but
// not for a different target.
func hasMatchingReplyRule(rules []db.EventRule, event events.Event) bool {
	for _, rule := range rules {
		if rule.Enabled &&
			strings.EqualFold(strings.TrimSpace(rule.OnKind), "reply") &&
			strings.EqualFold(strings.TrimSpace(rule.WakeRole), strings.TrimSpace(event.WakeTargetRole)) &&
			eventRuleMatches(rule.MatchFilter, event) {
			return true
		}
	}
	return false
}
