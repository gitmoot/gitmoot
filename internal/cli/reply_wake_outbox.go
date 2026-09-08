package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	// replyWakeCoalescingWindow is the DEFAULT hold, overridable by
	// [org].wake_coalesce_hold. It absorbs a burst of separate short-lived
	// `org escalate` processes and, since the owner decision of 2026-09-07,
	// deliberately trades up to five minutes of wake latency for a measured
	// 32.1% fewer interrupts on this fleet's own arrival history.
	//
	// IT IS A HOLD ON THE OLDEST PENDING ROW, NOT A LIMIT ON BATCH MEMBERSHIP
	// (#1978). It used to be both: a row created more than one window after
	// the batch anchor started a SECOND rolling window and therefore a second
	// wake, even though both rows were pending at the same drain and shared one
	// coalesce_key. Measured on this box's live store, 8,464 attempted rows were
	// delivered as 8,091 wakes, so 92.8% of delivery attempts carried exactly
	// one row while same-key arrivals were 5 seconds to 5 minutes apart.
	replyWakeCoalescingWindow  = config.DefaultWakeCoalesceHold
	replyWakeMaxCoalescedItems = 10
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
	// held counts deliverable rows still inside their coalescing hold. They are
	// SEPARATE from pending because they are waiting BY DESIGN, and with a
	// 300s default hold nearly every tick observes one (#1978/#1979). Counting
	// them as an outstanding obligation turned "drain unhealthy" from a real
	// diagnostic into a line the daemon prints most minutes.
	held  int
	inert int
	// routeRemoved means a durable tombstone proves that a matching rule was
	// deleted after the pending row existed.
	routeRemoved  int
	agedAttempted int
}

func (h replyWakeOutboxHealth) String() string {
	return fmt.Sprintf(
		"pending=%d held=%d inert=%d route_removed=%d aged_attempted=%d",
		h.pending,
		h.held,
		h.inert,
		h.routeRemoved,
		h.agedAttempted,
	)
}

// wakeOutboxStore is the narrow store dependency the reply-wake drain uses. It
// exists for the same reason tickCandidateStore does: production always threads
// the real *db.Store, and a counting fake can then pin how many times the drain
// projects the outbox — the property #1758 changed and would otherwise be
// unmeasurable from outside.
type wakeOutboxStore interface {
	ListWakeOutboxObligations(ctx context.Context, attemptedBefore time.Time) (db.WakeOutboxObligationProjection, error)
	ExpireAgedWakeOutbox(ctx context.Context, attemptedBefore time.Time, now time.Time) ([]db.WakeOutboxEntry, error)
	ClaimWakeOutbox(ctx context.Context, surviving int64, coalesced []int64, now time.Time) (bool, error)
	ListDeletedEventRulesForRoutes(ctx context.Context, routes []db.EventRuleRoute) ([]db.DeletedEventRule, error)
	// AddJobEventIfAbsent records the unroutable-wake condition at most once per
	// row, keyed on (job_id, kind) by the store (owner decision 2026-09-08).
	AddJobEventIfAbsent(ctx context.Context, event db.JobEvent) error
}

// drainReplyWakeOutboxWithHealth is a store-global daemon operation. It
// deliberately reads durable work before resolving delivery: unreadable outbox
// state and an empty outbox therefore cannot collapse into the same result.
//
// `hold` is how long a pending group waits after its OLDEST row before the
// whole due group is delivered as one wake. A non-positive value falls back to
// the default rather than delivering with no hold at all, so a misread config
// cannot turn every burst back into one wake per note.
func drainReplyWakeOutboxWithHealth(ctx context.Context, store wakeOutboxStore, now time.Time, hold time.Duration, resolve replyWakeDeliveryResolver) (replyWakeOutboxHealth, error) {
	if store == nil {
		return replyWakeOutboxHealth{}, errors.New("wake outbox store is required")
	}
	if hold <= 0 {
		hold = replyWakeCoalescingWindow
	}
	attemptedBefore := now.UTC().Add(-replyWakeAttemptedUnknownAfter)
	obligations, err := store.ListWakeOutboxObligations(ctx, attemptedBefore)
	if err != nil || obligations.Len() == 0 {
		return replyWakeOutboxHealth{}, err
	}
	// Tracks whether this drain changed any outbox row. Only a successful claim
	// does: ExpireAgedWakeOutbox returns above whenever it expired anything, and
	// every other branch is a read. When nothing was claimed the projection read
	// above still describes the store exactly, so the closing health pass
	// can reuse it instead of issuing the same query a second time (#1758).
	mutated := false

	agedAttempted := len(obligations.AgedAttempted)
	if agedAttempted > 0 {
		expired, err := store.ExpireAgedWakeOutbox(ctx, attemptedBefore, now)
		if err != nil {
			return replyWakeOutboxHealth{}, fmt.Errorf("expire aged attempted wake outbox: %w", err)
		}
		// A PROVEN DELIVERY IS NOT AN UNKNOWN ONE (#1958). The sweep now resolves
		// an aged row against destination evidence and returns the state it
		// wrote, so only the rows it genuinely could not explain belong in this
		// diagnostic. Counting every expired row here reported a false unknown
		// delivery for each proven one AND drove both supervisors to log the
		// drain unhealthy - the store-side fix stopped at the store, and this is
		// the caller that still spoke for it.
		if len(expired) > 0 {
			mutated = true
		}
		unknown := 0
		for _, entry := range expired {
			if entry.State == db.WakeOutboxStateDeliveryUnknown {
				unknown++
			}
		}
		if unknown > 0 {
			return replyWakeOutboxHealth{}, fmt.Errorf(
				"wake outbox delivery unknown: expired %d aged attempted rows without retry",
				unknown,
			)
		}
	}

	groups := make(map[string][]db.WakeOutboxObligation)
	for _, entry := range obligations.Pending {
		key := strings.ToLower(strings.TrimSpace(entry.TargetRole)) + "\x00" + entry.CoalesceKey
		if entry.SourceKind == db.WakeOutboxSourceWorkflowNote &&
			strings.HasPrefix(strings.ToLower(entry.CoalesceKey), db.WakeOutboxDirectiveCoalescePrefix) {
			// A directive prompt names exactly one obligation and command. Never
			// let role-level coalescing mark another directive delivered unseen.
			key += "\x00" + entry.SourceID + "\x00" + entry.DirectivePhase
		}
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
			deadline := startedAt.Add(hold)
			if now.UTC().Before(deadline) {
				// Later rows for the same group cannot be due before its oldest
				// row, so leave the whole tail pending for a future tick.
				break
			}
			// EVERY DUE PENDING ROW FOR THIS KEY JOINS THE BATCH, capped only by
			// replyWakeMaxCoalescedItems (#1978). The anchor's hold above has
			// already elapsed, and a newer row's own hold cannot matter: it is
			// carried by a wake this drain is emitting anyway, so including it
			// only removes a later interrupt. The batch event still names every
			// member's retrieval command, so collapsing loses no note.
			end := min(start+replyWakeMaxCoalescedItems, len(items))

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
			// The oldest row SURVIVES as the delivered obligation: it is the one
			// wakeOutboxEvent identifies as `oldest id`, and ids[0] is that row.
			// The WHOLE batch travels to the outcome (#1982): a delivered wake
			// supersedes the rest into the survivor, while a retryable failure
			// returns them all to pending so the next attempt re-coalesces every
			// note instead of losing the ones a survivor carried.
			claimed, err := store.ClaimWakeOutbox(ctx, ids[0], ids[1:], now)
			if err != nil {
				return replyWakeOutboxHealth{}, err
			}
			if claimed {
				mutated = true
				event.WakeOutboxIDs = ids
				if err := emitReplyWakeOutboxEvent(ctx, delivery.sink, event, matchingRules); err != nil {
					return replyWakeOutboxHealth{}, fmt.Errorf("emit claimed %s wake: %w", event.WakeKind, err)
				}
			}
			start = end
		}
	}
	if mutated {
		return wakeOutboxObligationHealth(ctx, store, attemptedBefore, now, hold, resolve)
	}
	// The #1200/#1201 contract is untouched by the reuse: an unreadable outbox
	// already returned its error above, so only a SUCCESSFUL read can reach
	// here, and an empty one returned early. Reuse therefore never turns "could
	// not read" into "nothing to do".
	return classifyWakeOutboxObligations(ctx, store, obligations, attemptedBefore, now, hold, resolve)
}

func wakeOutboxObligationHealth(
	ctx context.Context,
	store wakeOutboxStore,
	attemptedBefore time.Time,
	now time.Time,
	hold time.Duration,
	resolve replyWakeDeliveryResolver,
) (replyWakeOutboxHealth, error) {
	obligations, err := store.ListWakeOutboxObligations(ctx, attemptedBefore)
	if err != nil {
		return replyWakeOutboxHealth{}, fmt.Errorf("read wake outbox obligations: %w", err)
	}
	return classifyWakeOutboxObligations(ctx, store, obligations, attemptedBefore, now, hold, resolve)
}

// classifyWakeOutboxObligations grades an ALREADY-READ obligation projection.
// Splitting it out lets the drain reuse its own opening read when it claimed
// nothing; the read itself stays in wakeOutboxObligationHealth for callers that
// need a fresh projection.
func classifyWakeOutboxObligations(
	ctx context.Context,
	store wakeOutboxStore,
	obligations db.WakeOutboxObligationProjection,
	attemptedBefore time.Time,
	now time.Time,
	hold time.Duration,
	resolve replyWakeDeliveryResolver,
) (replyWakeOutboxHealth, error) {
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
		id         int64
		sourceKind string
		sourceID   string
		event      events.Event
		createdAt  time.Time
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
			createdAt, err := time.Parse(time.RFC3339Nano, obligation.CreatedAt)
			if err != nil {
				return replyWakeOutboxHealth{}, fmt.Errorf("parse wake outbox created_at for row %d: %w", obligation.ID, err)
			}
			// A deliverable row inside its hold is WAITING, not outstanding: the
			// next due tick delivers it, and with a 300s hold this is the normal
			// state of the outbox rather than a fault (#1978).
			if hold > 0 && now.UTC().Before(createdAt.Add(hold)) {
				health.held++
				continue
			}
			health.pending++
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, obligation.CreatedAt)
		if err != nil {
			return replyWakeOutboxHealth{}, fmt.Errorf("parse wake outbox created_at for row %d: %w", obligation.ID, err)
		}
		unmatched = append(unmatched, unmatchedWakeObligation{
			id: obligation.ID, sourceKind: obligation.SourceKind, sourceID: obligation.SourceID,
			event: event, createdAt: createdAt,
		})
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
		condition := db.WakeOutboxUnroutableNeverConfigured
		if matchesDeletedWakeRule(deletions, obligation.event, obligation.createdAt) {
			condition = db.WakeOutboxUnroutableRouteRemoved
			health.routeRemoved++
		} else {
			health.inert++
		}
		recordUnroutableWake(ctx, store, obligation.id, obligation.sourceKind, obligation.sourceID, obligation.event, condition)
	}
	if health.pending == 0 && health.routeRemoved == 0 && health.agedAttempted == 0 {
		return health, nil
	}
	return health, fmt.Errorf("wake outbox has outstanding obligations: %s", health)
}

// recordUnroutableWake makes a dead-ended wake visible in ONE query instead of
// silently queued (owner decision 2026-09-08, relayed by phobos).
//
// It records nothing new about the row's fate: an unroutable row stays pending
// and is deliberately NOT failed, because a wake nobody can receive is not a
// wake that is wrong, and a rule added later can still deliver it. What changes
// is that the condition is attributable: role, kind, source and WHICH condition
// it is, since a retired seat needs no remedy while a never-configured one
// needs a route.
//
// ONCE PER WAKE, NOT PER TICK. AddJobEventIfAbsent keys on (job_id, kind), and
// the drain re-classifies every tick, so an unconditional append would add a
// row per unroutable obligation per tick forever: the mechanism that grew
// job_events past a million rows.
//
// BEST EFFORT. An audit write that fails must not change the drain's verdict,
// so the failure is logged and the classification stands.
func recordUnroutableWake(
	ctx context.Context,
	store wakeOutboxStore,
	rowID int64,
	sourceKind, sourceID string,
	event events.Event,
	condition string,
) {
	if store == nil || rowID <= 0 {
		return
	}
	source := strings.TrimSpace(sourceKind)
	// A blocked, escalation or fact source id is a serialized event payload, so
	// only a workflow-note id is worth rendering into an operator-facing record.
	if source == db.WakeOutboxSourceWorkflowNote {
		source += ":" + strings.TrimSpace(sourceID)
	}
	message := fmt.Sprintf(
		"role=%s kind=%s source=%s condition=%s",
		strings.TrimSpace(event.WakeTargetRole),
		strings.TrimSpace(event.WakeKind),
		source,
		condition,
	)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), db.DurableWriteBudget)
	defer cancel()
	if err := store.AddJobEventIfAbsent(writeCtx, db.JobEvent{
		JobID:   fmt.Sprintf("wake-outbox:%d", rowID),
		Kind:    db.WakeOutboxUnroutableEventKind,
		Message: message,
	}); err != nil {
		slog.Warn("unroutable wake record failed",
			"wake_outbox_row", rowID, "role", event.WakeTargetRole, "kind", event.WakeKind, "error", err)
	}
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
	case db.WakeOutboxSourceWorkflowNote:
		if wakeKind == db.WakeOutboxKindDirective {
			// #1981: the DELIVERABLE travels with the wake. Naming the row and
			// leaving the seat to fetch it costs another turn, and the reader it
			// was pointed at truncated the body anyway. The prompt renderer
			// bounds this; nothing is cut here.
			//
			// The marker header (`[org:directive to=... from=... wf=...]`) is
			// machine addressing the prompt already states, so the seat gets the
			// directive TEXT. An unparseable body falls back to the raw note
			// rather than to silence.
			detail := strings.TrimSpace(oldest.DirectiveBody)
			if _, _, _, text, ok := workflow.ParseOrgDirectiveNote(detail); ok && strings.TrimSpace(text) != "" {
				detail = strings.TrimSpace(text)
			}
			if detail == "" {
				detail = fmt.Sprintf("directive id %s for %s", oldest.SourceID, role)
			}
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
		retrievalCommands := make([]string, 0, len(batch))
		for _, entry := range batch {
			if entry.SourceKind == db.WakeOutboxSourceWorkflowNote {
				retrievalCommands = append(retrievalCommands, "gitmoot workflow show-note "+entry.SourceID)
			}
		}
		if len(retrievalCommands) > 0 {
			detail += "; inspect with " + strings.Join(retrievalCommands, "; ")
		}
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
