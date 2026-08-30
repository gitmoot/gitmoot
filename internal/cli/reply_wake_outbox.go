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

type replyWakeOutboxHealth struct {
	pending int
	inert   int
	// routeRemoved means a durable tombstone proves that a matching rule was
	// deleted after the pending row existed.
	routeRemoved  int
	agedAttempted int
}

func (h replyWakeOutboxHealth) String() string {
	return fmt.Sprintf(
		"pending=%d inert=%d route_removed=%d aged_attempted=%d",
		h.pending,
		h.inert,
		h.routeRemoved,
		h.agedAttempted,
	)
}

// drainReplyWakeOutbox is a store-global daemon operation. It deliberately
// reads durable work before resolving delivery: unreadable outbox state and an
// empty outbox therefore cannot collapse into the same result.
func drainReplyWakeOutbox(ctx context.Context, store *db.Store, now time.Time, resolve replyWakeDeliveryResolver) error {
	_, err := drainReplyWakeOutboxWithHealth(ctx, store, now, resolve)
	return err
}

func drainReplyWakeOutboxWithHealth(ctx context.Context, store *db.Store, now time.Time, resolve replyWakeDeliveryResolver) (replyWakeOutboxHealth, error) {
	if store == nil {
		return replyWakeOutboxHealth{}, errors.New("wake outbox store is required")
	}
	attemptedBefore := now.UTC().Add(-replyWakeAttemptedUnknownAfter)
	obligations, err := store.ListWakeOutboxObligations(ctx, attemptedBefore)
	if err != nil || obligations.Len() == 0 {
		return replyWakeOutboxHealth{}, err
	}

	agedAttempted := len(obligations.AgedAttempted)
	if agedAttempted > 0 {
		expired, err := store.ExpireAgedWakeOutbox(ctx, attemptedBefore, now)
		if err != nil {
			return replyWakeOutboxHealth{}, fmt.Errorf("expire aged attempted wake outbox: %w", err)
		}
		if len(expired) > 0 {
			return replyWakeOutboxHealth{}, fmt.Errorf(
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
		return replyWakeOutboxHealth{}, errors.New("wake outbox delivery resolver is required")
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
				return replyWakeOutboxHealth{}, fmt.Errorf("parse wake outbox created_at for row %d: %w", items[start].ID, err)
			}
			deadline := startedAt.Add(replyWakeCoalescingWindow)
			end := start + 1
			for end < len(items) {
				createdAt, err := time.Parse(time.RFC3339Nano, items[end].CreatedAt)
				if err != nil {
					return replyWakeOutboxHealth{}, fmt.Errorf("parse wake outbox created_at for row %d: %w", items[end].ID, err)
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
				return replyWakeOutboxHealth{}, err
			}
			// Authorization is deliberately batch-scoped and pre-claim. A rule
			// removed while an earlier synchronous batch is delivering cannot
			// authorize this later claim, and no post-claim rule read can strand
			// attempted rows on a routing-store failure.
			delivery, err := resolve(ctx)
			if err != nil {
				return replyWakeOutboxHealth{}, fmt.Errorf("resolve wake outbox delivery: %w", err)
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
				return replyWakeOutboxHealth{}, err
			}
			if claimed {
				event.WakeOutboxIDs = ids
				if err := emitReplyWakeOutboxEvent(ctx, delivery.sink, event, matchingRules); err != nil {
					return replyWakeOutboxHealth{}, fmt.Errorf("emit claimed %s wake: %w", event.WakeKind, err)
				}
			}
			start = end
		}
	}
	return wakeOutboxObligationHealth(ctx, store, attemptedBefore, resolve)
}

func wakeOutboxObligationHealth(
	ctx context.Context,
	store *db.Store,
	attemptedBefore time.Time,
	resolve replyWakeDeliveryResolver,
) (replyWakeOutboxHealth, error) {
	obligations, err := store.ListWakeOutboxObligations(ctx, attemptedBefore)
	if err != nil {
		return replyWakeOutboxHealth{}, fmt.Errorf("read wake outbox obligations: %w", err)
	}
	health := replyWakeOutboxHealth{agedAttempted: len(obligations.AgedAttempted)}
	if len(obligations.Pending) == 0 {
		if health.agedAttempted == 0 {
			return health, nil
		}
		return health, fmt.Errorf("wake outbox has outstanding obligations: %s", health)
	}
	if resolve == nil {
		return replyWakeOutboxHealth{}, errors.New("classify wake outbox obligations: delivery resolver is required")
	}
	delivery, err := resolve(ctx)
	if err != nil {
		return replyWakeOutboxHealth{}, fmt.Errorf("classify wake outbox obligations: %w", err)
	}
	type unmatchedWakeObligation struct {
		event     events.Event
		createdAt time.Time
	}
	unmatched := make([]unmatchedWakeObligation, 0, len(obligations.Pending))
	routes := make([]db.EventRuleRoute, 0, len(obligations.Pending))
	seenRoutes := make(map[string]struct{})
	for _, obligation := range obligations.Pending {
		event, err := wakeOutboxEvent([]db.WakeOutboxObligation{obligation}, attemptedBefore)
		if err != nil {
			return replyWakeOutboxHealth{}, fmt.Errorf("classify wake outbox row %d: %w", obligation.ID, err)
		}
		if len(matchingWakeRules(delivery.rules, event)) > 0 {
			health.pending++
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, obligation.CreatedAt)
		if err != nil {
			return replyWakeOutboxHealth{}, fmt.Errorf("parse wake outbox created_at for row %d: %w", obligation.ID, err)
		}
		unmatched = append(unmatched, unmatchedWakeObligation{event: event, createdAt: createdAt})
		key := wakeRuleRouteKey(event.WakeTargetRole, event.WakeKind)
		if _, ok := seenRoutes[key]; !ok {
			seenRoutes[key] = struct{}{}
			routes = append(routes, db.EventRuleRoute{OnKind: event.WakeKind, WakeRole: event.WakeTargetRole})
		}
	}
	deletedRules, err := store.ListDeletedEventRulesForRoutes(ctx, routes)
	if err != nil {
		return replyWakeOutboxHealth{}, fmt.Errorf("classify wake outbox obligations against deleted rules: %w", err)
	}
	deletedRulesAt, err := parseDeletedWakeRules(deletedRules)
	if err != nil {
		return replyWakeOutboxHealth{}, fmt.Errorf("classify wake outbox obligations against deleted rules: %w", err)
	}
	for _, obligation := range unmatched {
		deletions := deletedRulesAt[wakeRuleRouteKey(obligation.event.WakeTargetRole, obligation.event.WakeKind)]
		if matchesDeletedWakeRule(deletions, obligation.event, obligation.createdAt) {
			health.routeRemoved++
			continue
		}
		health.inert++
	}
	if health.pending == 0 && health.routeRemoved == 0 && health.agedAttempted == 0 {
		return health, nil
	}
	return health, fmt.Errorf("wake outbox has outstanding obligations: %s", health)
}

type deletedWakeRule struct {
	rule      db.EventRule
	deletedAt time.Time
}

func parseDeletedWakeRules(deletions []db.DeletedEventRule) (map[string][]deletedWakeRule, error) {
	parsed := make(map[string][]deletedWakeRule)
	for _, deletion := range deletions {
		deletedAt, err := time.Parse(time.RFC3339Nano, deletion.DeletedAt)
		if err != nil {
			return nil, fmt.Errorf("parse deletion time for event rule %q: %w", deletion.ID, err)
		}
		key := wakeRuleRouteKey(deletion.WakeRole, deletion.OnKind)
		parsed[key] = append(parsed[key], deletedWakeRule{rule: deletion.EventRule, deletedAt: deletedAt})
	}
	return parsed, nil
}

func wakeRuleRouteKey(wakeRole, onKind string) string {
	return strings.ToLower(strings.TrimSpace(wakeRole)) + "\x00" + strings.ToLower(strings.TrimSpace(onKind))
}

func matchesDeletedWakeRule(deletions []deletedWakeRule, event events.Event, rowCreatedAt time.Time) bool {
	for _, deletion := range deletions {
		if deletion.deletedAt.Before(rowCreatedAt) {
			continue
		}
		if len(matchingWakeRules([]db.EventRule{deletion.rule}, event)) > 0 {
			return true
		}
	}
	return false
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
			switch oldest.DirectivePhase {
			case db.WakeOutboxDirectivePhaseCompletion:
				event.Cause = directiveCompletionOverdueCause
			case db.WakeOutboxDirectivePhaseTerminal:
				event.Cause = directiveTerminalCause
			default:
				event.Cause = "addressed_directive"
			}
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
		factID := fmt.Sprintf("awaited-fact:%d", payload.ID)
		event = events.NewEvent(
			events.EventOrgFact,
			factID,
			factID,
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
