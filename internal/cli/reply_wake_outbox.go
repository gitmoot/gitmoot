package cli

import (
	"context"
	"encoding/json"
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
			event, err := wakeOutboxEvent(batch, now)
			if err != nil {
				return err
			}
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
			matchingRules := matchingWakeRules(delivery.rules, event)
			if len(matchingRules) == 0 {
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
				if err := emitReplyWakeOutboxEvent(ctx, delivery.sink, event, matchingRules); err != nil {
					return fmt.Errorf("emit claimed %s wake: %w", event.WakeKind, err)
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
	pending := 0
	for _, obligation := range obligations.Pending {
		// Directives are config-inert by contract: without a matching rule their
		// durable row remains pending for later configuration, not as a permanent
		// daemon health error stream.
		if strings.HasPrefix(strings.ToLower(obligation.CoalesceKey), db.WakeOutboxDirectiveCoalescePrefix) {
			continue
		}
		pending++
	}
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

type wakeOutboxSourceKind struct {
	sourceKind string
	wakeKind   string
}

var wakeOutboxSourceKinds = []wakeOutboxSourceKind{
	{sourceKind: db.WakeOutboxSourceWorkflowNote, wakeKind: db.WakeOutboxKindReply},
	{sourceKind: db.WakeOutboxSourceChatMessage, wakeKind: db.WakeOutboxKindReply},
	{sourceKind: db.WakeOutboxSourceBlocked, wakeKind: db.WakeOutboxKindBlocked},
	{sourceKind: db.WakeOutboxSourceEscalation, wakeKind: db.WakeOutboxKindEscalation},
	{sourceKind: db.WakeOutboxSourceAwaitedFact, wakeKind: db.WakeOutboxKindFact},
}

func wakeOutboxKindForSource(sourceKind, coalesceKey string) (string, bool) {
	if sourceKind == db.WakeOutboxSourceWorkflowNote && strings.HasPrefix(strings.ToLower(coalesceKey), db.WakeOutboxDirectiveCoalescePrefix) {
		return db.WakeOutboxKindDirective, true
	}
	for _, definition := range wakeOutboxSourceKinds {
		if definition.sourceKind == sourceKind {
			return definition.wakeKind, true
		}
	}
	return "", false
}

func wakeOutboxDirectedKinds() []string {
	unique := map[string]struct{}{db.WakeOutboxKindDirective: {}}
	for _, definition := range wakeOutboxSourceKinds {
		unique[definition.wakeKind] = struct{}{}
	}
	kinds := make([]string, 0, len(unique))
	for kind := range unique {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func emitReplyWakeOutboxEvent(ctx context.Context, sink events.Sink, event events.Event, rules []db.EventRule) error {
	if durable, ok := sink.(synchronousWakeOutboxSink); ok {
		return durable.emitWakeOutbox(ctx, event, rules)
	}
	return errors.New("wake outbox sink does not support synchronous delivery")
}

func wakeOutboxEvent(batch []db.WakeOutboxObligation, now time.Time) (events.Event, error) {
	if len(batch) == 0 {
		return events.Event{}, errors.New("wake outbox batch is empty")
	}
	oldest := batch[0]
	role := strings.ToLower(strings.TrimSpace(oldest.TargetRole))
	wakeKind, ok := wakeOutboxKindForSource(oldest.SourceKind, oldest.CoalesceKey)
	if !ok {
		return events.Event{}, fmt.Errorf(
			"wake outbox row %d has unsupported source kind %q",
			oldest.ID, oldest.SourceKind,
		)
	}
	var event events.Event
	switch oldest.SourceKind {
	case db.WakeOutboxSourceWorkflowNote, db.WakeOutboxSourceChatMessage:
		if wakeKind == db.WakeOutboxKindDirective {
			detail := fmt.Sprintf("directive id %s for %s", oldest.SourceID, role)
			event = events.NewEvent(
				events.EventOrgDirective,
				"org-directive:"+role,
				oldest.SourceKind+":"+oldest.SourceID,
				"",
				db.WakeOutboxStateAttempted,
				detail,
				now,
				workflow.RedactCommentText,
			)
			event.Cause = "addressed_directive"
			break
		}
		detail := fmt.Sprintf("%d new items, oldest id %s", len(batch), oldest.SourceID)
		event = events.NewEvent(
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
	case db.WakeOutboxSourceBlocked:
		if err := json.Unmarshal([]byte(oldest.SourceID), &event); err != nil {
			return events.Event{}, fmt.Errorf("decode blocked wake outbox event for row %d: %w", oldest.ID, err)
		}
	case db.WakeOutboxSourceEscalation:
		if err := json.Unmarshal([]byte(oldest.SourceID), &event); err != nil {
			return events.Event{}, fmt.Errorf("decode escalation wake outbox event for row %d: %w", oldest.ID, err)
		}
	case db.WakeOutboxSourceAwaitedFact:
		var payload db.AwaitedFactWakePayload
		if err := json.Unmarshal([]byte(oldest.SourceID), &payload); err != nil {
			return events.Event{}, fmt.Errorf("decode awaited fact wake outbox event for row %d: %w", oldest.ID, err)
		}
		detail := fmt.Sprintf("awaited %s %s for %s is %s", payload.SubjectKind, payload.SubjectKey, payload.WaiterRole, payload.State)
		if len(batch) > 1 {
			detail = fmt.Sprintf("%d awaited facts ready; oldest: %s", len(batch), detail)
		}
		event = events.NewEvent(
			events.EventOrgFact,
			fmt.Sprintf("awaited-fact:%d", payload.ID),
			oldest.SourceKind+":"+oldest.SourceID,
			"",
			payload.State,
			detail,
			now,
			workflow.RedactCommentText,
		)
		event.Cause = "awaited_fact_" + payload.State
	default:
		return events.Event{}, fmt.Errorf(
			"wake outbox row %d has unsupported source kind %q",
			oldest.ID, oldest.SourceKind,
		)
	}
	event.WakeKind = wakeKind
	event.WakeTargetRole = role
	return event, nil
}

// Deliberately scope-blind pending a later durable-outbox slice: among enabled,
// filter-matching rules for the event's own kind, WakeRole == WakeTargetRole
// authorizes the claim. An observer-scoped rule can therefore claim for its own
// addressed role, but not for a different target.
func matchingWakeRules(rules []db.EventRule, event events.Event) []db.EventRule {
	for _, rule := range rules {
		if rule.Enabled &&
			strings.EqualFold(strings.TrimSpace(rule.OnKind), strings.TrimSpace(event.WakeKind)) &&
			strings.EqualFold(strings.TrimSpace(rule.WakeRole), strings.TrimSpace(event.WakeTargetRole)) &&
			eventRuleMatches(rule.MatchFilter, event) {
			return []db.EventRule{rule}
		}
	}
	return nil
}
