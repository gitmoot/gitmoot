package cli

import (
	"context"
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
)

// drainReplyWakeOutbox runs on the existing daemon tick. It claims durable
// batches before emitting, then the existing eventRuleSink delivery switch
// records delivered/stalled/failed outside every note transaction.
func drainReplyWakeOutbox(ctx context.Context, store *db.Store, sink events.Sink, now time.Time) error {
	if store == nil || sink == nil {
		return nil
	}
	entries, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil || len(entries) == 0 {
		return err
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		return err
	}

	groups := make(map[string][]db.WakeOutboxEntry)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.CoalesceKey, db.WakeOutboxReplyCoalescePrefix) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(entry.TargetRole)) + "\x00" + entry.CoalesceKey
		groups[key] = append(groups[key], entry)
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
			if !hasMatchingReplyRule(rules, event) {
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
				events.EmitEvent(ctx, sink, event)
			}
			start = end
		}
	}
	return nil
}

func replyWakeEvent(batch []db.WakeOutboxEntry, now time.Time) events.Event {
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
