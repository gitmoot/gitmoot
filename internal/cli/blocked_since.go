package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	blockedRoleWakeInterval    = time.Minute
	blockedRoleSnapshotTimeout = 5 * time.Second
	directiveTTLSweepLimit     = 200
	// A blocked task gets a finite alert ladder independent of disposal. The
	// third nudge also emits the one terminal escalation; the episode then stays
	// queryable but silent until evidence disposal transitions the task itself.
	blockedTaskMaxNudges = 3
	// blockedEpisodeStaleGap bounds how long a role episode survives without being
	// re-observed blocked. Within it, a transient unknown/absent snapshot blip is
	// tolerated (the accrued blocked duration is preserved); beyond it the subject is
	// treated as no longer blocked (unblocked undetected, or the role gone for good)
	// and the episode is reaped — so a permanently-unknown role neither leaks a row
	// nor reuses a stale blocked_since on a later re-block.
	blockedEpisodeStaleGap = 5 * blockedRoleWakeInterval
)

type blockedRoleAvailability interface {
	Available(context.Context) bool
}

type blockedRoleWakeDependencies struct {
	availability blockedRoleAvailability
	provider     func([]config.OrgRole) org.Provider
	eventSink    func(context.Context, *db.Store, string) (events.Sink, error)
	directives   directiveTTLDependencies
	awaitedFacts awaitedFactTTLDependencies
}

type awaitedFactTTLDependencies struct {
	list   func(context.Context, *db.Store, time.Time) ([]db.AwaitedFact, error)
	expire func(context.Context, *db.Store, int64, string, time.Time) (bool, error)
}

func (d awaitedFactTTLDependencies) listDue(ctx context.Context, store *db.Store, now time.Time) ([]db.AwaitedFact, error) {
	if d.list != nil {
		return d.list(ctx, store, now)
	}
	return store.ListDueAwaitedFacts(ctx, now)
}

func (d awaitedFactTTLDependencies) markExpired(ctx context.Context, store *db.Store, id int64, target string, now time.Time) (bool, error) {
	if d.expire != nil {
		return d.expire(ctx, store, id, target, now)
	}
	return store.ExpireAwaitedFact(ctx, id, target, now)
}

type directiveTTLDependencies struct {
	list func(context.Context, *db.Store, int) ([]db.OrgDirectiveObligation, error)
	mark func(context.Context, *db.Store, int64, int, string, time.Time) (int, bool, error)
	// #1352: the completion phase advances its OWN counter, and the ladder ends
	// in a stamped terminal state rather than in silence.
	markDone  func(context.Context, *db.Store, int64, int, string, time.Time) (int, bool, error)
	exhaust   func(context.Context, *db.Store, int64, time.Time) (bool, error)
	countOpen func(context.Context, *db.Store) (int, error)
}

func defaultBlockedRoleWakeDependencies() blockedRoleWakeDependencies {
	return blockedRoleWakeDependencies{
		availability: cockpit.New(cockpit.Options{HerdrBin: "herdr"}, nil),
		provider:     cockpit.NewHerdrOrgProvider,
		eventSink:    enabledBlockedSinceEventSink,
	}
}

func (d directiveTTLDependencies) listOpen(ctx context.Context, store *db.Store, limit int) ([]db.OrgDirectiveObligation, error) {
	if d.list != nil {
		return d.list(ctx, store, limit)
	}
	return store.ListOpenOrgDirectiveObligations(ctx, limit)
}

func (d directiveTTLDependencies) markNudged(ctx context.Context, store *db.Store, item db.OrgDirectiveObligation, now time.Time) (int, bool, error) {
	if d.mark != nil {
		return d.mark(ctx, store, item.ID, item.NudgeCount, item.LastNudgedAt, now)
	}
	return store.MarkOrgDirectiveNudged(ctx, item.ID, item.NudgeCount, item.LastNudgedAt, now)
}

// markDoneNudged advances the COMPLETION-phase counter (#1352). Kept separate
// from markNudged because the phases cap independently.
func (d directiveTTLDependencies) markDoneNudged(ctx context.Context, store *db.Store, item db.OrgDirectiveObligation, now time.Time) (int, bool, error) {
	if d.markDone != nil {
		return d.markDone(ctx, store, item.ID, item.DoneNudgeCount, item.LastNudgedAt, now)
	}
	return store.MarkOrgDirectiveDoneNudged(ctx, item.ID, item.DoneNudgeCount, item.LastNudgedAt, now)
}

func (d directiveTTLDependencies) markExhausted(ctx context.Context, store *db.Store, item db.OrgDirectiveObligation, now time.Time) (bool, error) {
	if d.exhaust != nil {
		return d.exhaust(ctx, store, item.ID, now)
	}
	return store.MarkOrgDirectiveExhausted(ctx, item.ID, now)
}

// sweepWindow sizes the window from the LIVE POPULATION rather than a fixed
// oldest-N (#1352). This is the remedy the blocked-task evaluator already uses;
// a fixed window let immortal rows own it and starve newer directives forever.
func (d directiveTTLDependencies) sweepWindow(ctx context.Context, store *db.Store) int {
	count, err := func() (int, error) {
		if d.countOpen != nil {
			return d.countOpen(ctx, store)
		}
		return store.CountOpenOrgDirectiveObligations(ctx)
	}()
	if err != nil || count <= 0 {
		// Fail toward the previous behaviour rather than toward sweeping nothing:
		// a count error must not silently stop the checker.
		return directiveTTLSweepLimit
	}
	return count
}

// enabledBlockedSinceEventSink preserves the blocked-since path's stricter
// off-by-default contract: a configured webhook alone is not enough; at least
// one enabled organization event rule must exist before either evaluator does
// any episode work or emits a synthesized event.
func enabledBlockedSinceEventSink(ctx context.Context, store *db.Store, home string) (events.Sink, error) {
	if store == nil {
		return nil, nil
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		return nil, err
	}
	if !hasEnabledEventRule(rules) {
		return nil, nil
	}
	return daemonEventSink(store, home), nil
}

// sweepBlockedTaskWakeEvents is the per-repo tick entrypoint. Every failure is
// returned only for logging by the caller; it must never fail the daemon tick.
func sweepBlockedTaskWakeEvents(ctx context.Context, store *db.Store, home, repo string, stdout io.Writer, now time.Time) error {
	wakeAfter := resolveBlockedRoleWakeAfter(home)
	if wakeAfter <= 0 {
		return nil
	}
	sink, err := enabledBlockedSinceEventSink(ctx, store, home)
	if err != nil || sink == nil {
		return err
	}
	return evaluateBlockedTaskEpisodes(ctx, store, sink, repo, wakeAfter, stdout, now)
}

func evaluateBlockedTaskEpisodes(ctx context.Context, store *db.Store, sink events.Sink, repo string, wakeAfter time.Duration, stdout io.Writer, now time.Time) error {
	if store == nil || sink == nil || wakeAfter <= 0 {
		return nil
	}
	now = now.UTC()
	blockedTasks, err := store.ListTasksByRepoState(ctx, repo, string(workflow.TaskBlocked))
	if err != nil {
		return err
	}
	blockedSubjects := make(map[string]struct{}, len(blockedTasks))
	for _, task := range blockedTasks {
		blockedSubjects[taskEpisodeSubject(repo, task.ID)] = struct{}{}
	}
	var candidates []db.StaleTaskCandidate
	if len(blockedTasks) > 0 {
		// Bound the stale projection to the current blocked population, rather
		// than a fixed oldest-N window that could starve later task ids forever.
		candidates, err = store.ListStaleTaskCandidates(ctx, repo, []string{string(workflow.TaskBlocked)}, now.Add(-wakeAfter), len(blockedTasks))
		if err != nil {
			return err
		}
	}

	staleSubjects := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		subject := taskEpisodeSubject(repo, candidate.ID)
		// Keep the current-state set as a defensive guard around the two-query
		// snapshot. Do not open or emit an episode absent from that set.
		if _, blocked := blockedSubjects[subject]; !blocked {
			continue
		}
		blockedSince := parseTranscriptStoreTime(candidate.UpdatedAt)
		if blockedSince.IsZero() {
			writeLine(stdout, "blocked_since task %s skipped: unparseable updated_at %q", candidate.ID, candidate.UpdatedAt)
			continue
		}
		if err := store.UpsertBlockedEpisode(ctx, subject, blockedSince, now); err != nil {
			writeLine(stdout, "blocked_since task %s episode upsert failed: %v", candidate.ID, err)
			continue
		}
		staleSubjects[subject] = candidate.ID
	}

	// The episode table is tiny (off-by-default, few blocked subjects), so a full
	// list + byte-exact Go prefix filter is negligible and — unlike a SQL LIKE —
	// cannot case-fold a sibling repo whose name differs only in ASCII case.
	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		return err
	}
	taskPrefix := "task:" + strings.TrimSpace(repo) + ":"
	var digestItems []blockedTaskDigestItem
	var exhaustedItems []blockedTaskDigestItem
	for _, episode := range episodes {
		if !strings.HasPrefix(episode.Subject, taskPrefix) {
			continue
		}
		// Tasks have an authoritative blocked set (gitmoot's own state), so an
		// episode whose task is no longer blocked is cleared immediately — no
		// staleness heuristic needed (unlike the ambiguous Herdr role source).
		if _, blocked := blockedSubjects[episode.Subject]; !blocked {
			if err := store.ClearBlockedEpisode(ctx, episode.Subject); err != nil {
				writeLine(stdout, "blocked_since task episode clear failed for %s: %v", episode.Subject, err)
			}
			continue
		}
		taskID, stale := staleSubjects[episode.Subject]
		if !stale {
			continue
		}
		if strings.TrimSpace(episode.TaskExhaustedAt) != "" {
			continue
		}
		blockedSince, blockedFor, due, err := blockedEpisodeDue(episode, wakeAfter, now)
		if err != nil {
			writeLine(stdout, "blocked_since task %s emit failed: %v", taskID, err)
			continue
		}
		if !due {
			continue
		}
		newCount, exhausted, err := store.MarkBlockedTaskEpisodeEmitted(ctx, episode.Subject,
			episode.TaskEmitCount, blockedTaskMaxNudges, now)
		if err != nil {
			writeLine(stdout, "blocked_since task %s emit failed: %v", taskID, err)
			continue
		}
		if newCount == episode.TaskEmitCount {
			continue
		}
		item := blockedTaskDigestItem{
			taskID:       taskID,
			blockedSince: blockedSince,
			blockedFor:   blockedFor,
		}
		digestItems = append(digestItems, item)
		if exhausted {
			exhaustedItems = append(exhaustedItems, item)
		}
	}
	if len(digestItems) > 0 {
		sortBlockedTaskDigestItems(digestItems)
		events.EmitEvent(ctx, sink, buildBlockedTaskDigestEvent(repo, digestItems, now))
	}
	for _, item := range exhaustedItems {
		events.EmitEvent(ctx, sink, buildBlockedTaskAlertExhaustedEvent(repo, item, now))
	}
	return nil
}

// startBlockedRoleWakeLoop owns the single host-global Herdr blocked-role lane.
// It is independent of repo ticks because a Herdr snapshot is host-global.
func startBlockedRoleWakeLoop(ctx context.Context, store *db.Store, home string, stdout io.Writer) {
	deps := defaultBlockedRoleWakeDependencies()
	go func() {
		ticker := time.NewTicker(blockedRoleWakeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				runBlockedRoleWakeOnce(ctx, store, home, stdout, now.UTC(), deps)
			}
		}
	}()
}

// runBlockedRoleWakeOnce performs one best-effort, dependency-injectable Herdr
// evaluation. It swallows and logs every failure so the background lane can
// never affect daemon supervision.
func runBlockedRoleWakeOnce(ctx context.Context, store *db.Store, home string, stdout io.Writer, now time.Time, deps blockedRoleWakeDependencies) {
	if store == nil {
		return
	}
	// #1136 shares this host-global one-minute cadence but is independent of
	// Herdr/config availability: a provider-declared quota reset must clear even
	// on a daemon with no live org presence provider.
	if _, err := store.ClearExpiredOrgRolesUnavailable(ctx, now.UTC()); err != nil {
		writeLine(stdout, "org role quota-unavailable sweep failed: %v", err)
	}
	configFile := resolveConfigFile(home)
	if configFile == "" {
		return
	}
	orgConfig, err := config.LoadOrg(config.Paths{ConfigFile: configFile})
	if err != nil {
		writeLine(stdout, "blocked_since role org config load failed: %v", err)
		return
	}
	roles := loadOrgRoster(ctx, store, orgConfig).Members()
	if len(roles) == 0 {
		// No org roles configured: there is nothing to snapshot or persist, so
		// skip silently (matches the pre-#1118 behavior for an unconfigured
		// daemon). Checking this FIRST means a herdr-less/no-org deployment — the
		// common OSS/automated-user case — never probes herdr and never logs.
		return
	}
	if err := evaluateAwaitedFactTTLs(ctx, store, orgConfig, stdout, now.UTC(), deps.awaitedFacts); err != nil {
		writeLine(stdout, "awaited fact TTL evaluation failed: %v", err)
	}
	// Directive supervision shares this existing one-minute lane but does not
	// depend on a Herdr snapshot. Resolve the event-rule guard first: with zero
	// enabled rules, sink is nil and no directive query is issued (config-inert).
	var sink events.Sink
	if deps.eventSink != nil {
		sink, err = deps.eventSink(ctx, store, home)
		if err != nil {
			writeLine(stdout, "org directive TTL event sink unavailable: %v", err)
		} else if sink != nil {
			if err := evaluateOrgDirectiveTTLs(ctx, store, sink, orgConfig, stdout, now.UTC(), deps.directives); err != nil {
				writeLine(stdout, "org directive TTL evaluation failed: %v", err)
			}
		}
	}
	// Herdr reachability gates the snapshot that BOTH org live-presence and the
	// blocked-role wake need. This return was previously silent, which made a
	// broken /org page impossible to diagnose from daemon.log for a deployment
	// that DOES configure org roles (#1118). Scoped to the roles-configured case
	// above so it cannot spam an unconfigured daemon's log every tick.
	if deps.availability == nil || !deps.availability.Available(ctx) {
		writeLine(stdout, "blocked_since role loop: herdr not available; org presence + wake skipped this tick")
		return
	}
	if deps.provider == nil {
		writeLine(stdout, "blocked_since role loop: nil org presence provider factory; skipped")
		return
	}
	provider := deps.provider(roles)
	if provider == nil {
		writeLine(stdout, "blocked_since role loop: nil org presence provider; skipped")
		return
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, blockedRoleSnapshotTimeout)
	snapshot, err := provider.Snapshot(snapshotCtx)
	cancel()
	if err != nil {
		writeLine(stdout, "blocked_since role snapshot failed: %v", err)
		return
	}
	if snapshot.ObservedAt.IsZero() {
		writeLine(stdout, "blocked_since role snapshot skipped: observed_at is zero")
		return
	}

	// Org live-presence persists on EVERY tick herdr is reachable, INDEPENDENT of
	// whether blocked-role waking is enabled — the /org page must populate even
	// when blocked_role_wake_after is 0 (its default). Coupling these was #1118.
	persistOrgRoleLivePresence(ctx, store, snapshot, stdout)

	// Blocked-role WAKE evaluation is the opt-in half: only when a wake threshold
	// is configured and the blocked-role event rule yields a sink.
	wakeAfter := resolveBlockedRoleWakeAfter(home)
	if wakeAfter <= 0 || sink == nil {
		return
	}
	if err := evaluateBlockedRoleEpisodes(ctx, store, sink, snapshot, wakeAfter, stdout, now.UTC()); err != nil {
		writeLine(stdout, "blocked_since role evaluation failed: %v", err)
	}
	if err := evaluateInputPendingRoleEpisodes(ctx, store, sink, snapshot, wakeAfter, stdout, now.UTC()); err != nil {
		writeLine(stdout, "input_pending role evaluation failed: %v", err)
	}
}

func evaluateAwaitedFactTTLs(ctx context.Context, store *db.Store, orgConfig config.OrgConfig, stdout io.Writer, now time.Time, deps awaitedFactTTLDependencies) error {
	if store == nil {
		return nil
	}
	items, err := deps.listDue(ctx, store, now.UTC())
	if err != nil {
		return err
	}
	for _, item := range items {
		role, ok := orgConfig.Role(item.WaiterRole)
		target := item.WaiterRole
		if ok {
			target = strings.TrimSpace(role.Parent)
		} else {
			// Seat removal cannot remove the wait's termination bound. Preserve the
			// exact removed-role address so delivery failure remains separately
			// observable while the subscription itself becomes terminal/queryable.
			writeLine(stdout, "awaited fact %d waiter role %q is no longer configured; expiring with wake still addressed to that role", item.ID, item.WaiterRole)
		}
		if target == "" {
			// A root wait still terminates visibly. With no parent available, wake
			// the root itself rather than turning expiry into silence.
			target = item.WaiterRole
		}
		expired, err := deps.markExpired(ctx, store, item.ID, target, now.UTC())
		if err != nil {
			writeLine(stdout, "awaited fact %d expiry failed: %v", item.ID, err)
			continue
		}
		if expired {
			writeLine(stdout, "awaited fact %d expired; escalation addressed to %s", item.ID, target)
		}
	}
	return nil
}

func evaluateOrgDirectiveTTLs(ctx context.Context, store *db.Store, sink events.Sink, orgConfig config.OrgConfig, stdout io.Writer, now time.Time, deps directiveTTLDependencies) error {
	if store == nil || sink == nil {
		return nil
	}
	now = now.UTC()
	items, err := deps.listOpen(ctx, store, deps.sweepWindow(ctx, store))
	if err != nil {
		return err
	}
	for _, item := range items {
		from, to, _, _, ok := workflow.ParseOrgDirectiveNote(item.Body)
		if !ok {
			writeLine(stdout, "org directive %d TTL skipped: malformed directive marker", item.ID)
			continue
		}
		anchor, ttl, phase, unacked, due := directiveTTLDue(item, orgConfig, now)
		if !due {
			continue
		}
		// #1352: each phase advances its OWN counter, so each caps independently.
		var newCount int
		var claimed bool
		if unacked {
			newCount, claimed, err = deps.markNudged(ctx, store, item, now)
		} else {
			newCount, claimed, err = deps.markDoneNudged(ctx, store, item, now)
		}
		if err != nil {
			writeLine(stdout, "org directive %d nudge mark failed: %v", item.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		events.EmitEvent(ctx, sink, buildDirectiveNudgeEvent(item, to, phase, anchor, ttl, newCount, now))
		// The ladder ends the same way in BOTH phases: one terminal escalation at
		// the cap, then a stamped exhausted state. The acked phase previously had
		// neither, so an acknowledged directive with a done TTL re-nudged forever.
		if newCount >= orgConfig.DirectiveMaxNudges() {
			events.EmitEvent(ctx, sink, buildDirectiveEscalationEvent(item, orgConfig, from, to, phase, newCount, now))
			// #1352 B2: the stamp is COMPLETION-ONLY. Stamping it for the
			// acknowledgment phase terminated BOTH phases, so a directive that
			// exhausted its ack ladder and was then acknowledged LATE could never
			// start its completion ladder at all. The ack phase's terminal condition
			// is its own counter at the cap — pre-existing, queryable, and it does
			// not block the phase that follows it.
			if !unacked {
				if stamped, err := deps.markExhausted(ctx, store, item, now); err != nil {
					writeLine(stdout, "org directive %d exhausted mark failed: %v", item.ID, err)
				} else if stamped {
					// #1352 B1: the COLUMN alone was invisible to operators — Comms
					// builds threads from NOTES, so a column-only terminal state showed
					// no thread at all. The marker is what makes exhaustion discoverable;
					// the column stays for the evaluator's own reads.
					if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
						WorkflowID: item.WorkflowID,
						Author:     to,
						Body:       workflow.FormatOrgDirectiveExhaustedNote(item.ID, to),
						Repo:       item.Repo,
					}); err != nil {
						writeLine(stdout, "org directive %d exhausted marker note failed: %v", item.ID, err)
					}
					writeLine(stdout, "org directive %d %s ladder exhausted after %d nudges; obligation remains open and queryable", item.ID, phase, newCount)
				}
			}
		}
	}
	return nil
}

func directiveTTLDue(item db.OrgDirectiveObligation, orgConfig config.OrgConfig, now time.Time) (anchor time.Time, ttl time.Duration, phase string, unacked, due bool) {
	// #1352 B2: the stamp is COMPLETION-phase terminal only. An unacknowledged
	// directive is never gated by it, so a late ack starts a fresh completion
	// ladder instead of inheriting the previous phase's terminal state.
	if strings.TrimSpace(item.AckedAt) != "" && strings.TrimSpace(item.ExhaustedAt) != "" {
		return time.Time{}, 0, "", false, false
	}
	if strings.TrimSpace(item.AckedAt) == "" {
		if item.NudgeCount >= orgConfig.DirectiveMaxNudges() {
			return time.Time{}, 0, "", true, false
		}
		anchor = parseTranscriptStoreTime(item.CreatedAt)
		ttl = orgConfig.DirectiveAckTTL()
		phase = "acknowledgment"
		unacked = true
	} else {
		// The completion phase caps on its OWN counter. This branch previously had
		// NO max check at all, which is defect 1: an acknowledged directive with a
		// done TTL re-nudged forever, and those immortal rows are also what starved
		// the sweep window.
		if item.DoneNudgeCount >= orgConfig.DirectiveMaxNudges() {
			return time.Time{}, 0, "", false, false
		}
		anchor = parseTranscriptStoreTime(item.AckedAt)
		ttl = orgConfig.DirectiveDoneTTL()
		if item.DoneTTLOverrideSeconds >= 0 {
			ttl = time.Duration(item.DoneTTLOverrideSeconds) * time.Second
		}
		phase = "completion"
	}
	if anchor.IsZero() || ttl <= 0 || now.Sub(anchor) <= ttl {
		return anchor, ttl, phase, unacked, false
	}
	if lastText := strings.TrimSpace(item.LastNudgedAt); lastText != "" {
		last := parseTranscriptStoreTime(lastText)
		if last.IsZero() || now.Sub(last) <= ttl {
			return anchor, ttl, phase, unacked, false
		}
	}
	return anchor, ttl, phase, unacked, true
}

func buildDirectiveNudgeEvent(item db.OrgDirectiveObligation, target, phase string, anchor time.Time, ttl time.Duration, nudgeCount int, now time.Time) events.Event {
	id := fmt.Sprint(item.ID)
	detail := fmt.Sprintf("directive %s to %s awaits %s after %s; nudge %d", id, target, phase, now.Sub(anchor).Round(time.Second), nudgeCount)
	ev := events.NewEvent(events.EventOrgDirective, id, db.WakeOutboxSourceWorkflowNote+":"+id, item.Repo, "overdue", detail, now, workflow.RedactCommentText)
	ev.Cause = "directive_" + phase + "_overdue"
	ev.WakeTargetRole = target
	return ev
}

func buildDirectiveEscalationEvent(item db.OrgDirectiveObligation, orgConfig config.OrgConfig, sender, target, phase string, nudgeCount int, now time.Time) events.Event {
	id := fmt.Sprint(item.ID)
	// #1352: both phases escalate now, so the detail must say WHICH obligation
	// went unmet — "unacknowledged" would be wrong for a completion escalation.
	unmet := "unacknowledged"
	if phase == "completion" {
		unmet = "incomplete"
	}
	detail := fmt.Sprintf("directive %s to %s remains %s after %d nudges; nudge ladder exhausted", id, target, unmet, nudgeCount)
	ev := events.NewEvent(events.EventJobNeedsAttention, "org-directive:"+id, db.WakeOutboxSourceWorkflowNote+":"+id, item.Repo, "overdue", detail, now, workflow.RedactCommentText)
	ev.Cause = "escalation"
	if role, ok := orgConfig.Role(sender); ok {
		ev.WakeTargetRole = role.Parent
	}
	return ev
}

func persistOrgRoleLivePresence(ctx context.Context, store *db.Store, snapshot org.Snapshot, stdout io.Writer) {
	roles := make([]string, 0, len(snapshot.States))
	persistedAll := true
	for role, live := range snapshot.States {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		if err := store.UpsertRoleLivePresence(ctx, role, string(live.State), snapshot.ObservedAt); err != nil {
			writeLine(stdout, "blocked_since role live presence persist failed for %s: %v", role, err)
			persistedAll = false
			continue
		}
		roles = append(roles, role)
	}
	// Do not prune a previously complete snapshot after a partial write failure.
	if persistedAll {
		if err := store.DeleteRoleLivePresenceExcept(ctx, roles); err != nil {
			writeLine(stdout, "blocked_since role live presence reap failed: %v", err)
		}
	}
}

func evaluateBlockedRoleEpisodes(ctx context.Context, store *db.Store, sink events.Sink, snapshot org.Snapshot, wakeAfter time.Duration, stdout io.Writer, now time.Time) error {
	if store == nil || sink == nil || wakeAfter <= 0 {
		return nil
	}
	blockedSubjects := map[string]string{}
	readySubjects := map[string]struct{}{}
	confirmedUnblocked := map[string]struct{}{}
	for role, live := range snapshot.States {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		subject := "role:" + role
		switch live.State {
		case org.StateBlocked:
			blockedSubjects[subject] = role
			if err := store.UpsertBlockedEpisode(ctx, subject, snapshot.ObservedAt, now); err != nil {
				writeLine(stdout, "blocked_since role %s episode upsert failed: %v", role, err)
				continue
			}
			readySubjects[subject] = struct{}{}
		case org.StateIdle, org.StateWorking, org.StateInputPending, org.StateDone:
			// A DEFINITIVE non-blocked observation is the only thing that closes an
			// episode. StateUnknown — or a role absent from the snapshot (pane recycle,
			// transient agent_status, a brief exact-label mismatch) — is ambiguous and
			// MUST NOT clear the episode, else a momentary blip would discard the
			// accrued blocked duration and reset the wake timer.
			confirmedUnblocked[subject] = struct{}{}
		}
	}

	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		return err
	}
	staleBefore := now.Add(-blockedEpisodeStaleGap)
	for _, episode := range episodes {
		if !strings.HasPrefix(episode.Subject, "role:") {
			continue
		}
		if role, blocked := blockedSubjects[episode.Subject]; blocked {
			if _, ready := readySubjects[episode.Subject]; !ready {
				continue
			}
			subjectID := "org-blocked:" + role
			if err := emitBlockedSinceEpisode(ctx, store, sink, episode, subjectID, subjectID, "", "role "+role, wakeAfter, now); err != nil {
				writeLine(stdout, "blocked_since role %s emit failed: %v", role, err)
			}
			continue
		}
		// Not observed blocked this tick. Clear on a DEFINITIVE non-blocked state, or
		// once the episode has gone STALE (not re-observed blocked within the gap) —
		// reaping a role gone permanently unknown/absent without letting a single
		// transient blip discard the accrued duration or leak the row forever.
		if _, confirmed := confirmedUnblocked[episode.Subject]; confirmed || blockedEpisodeStale(episode.UpdatedAt, staleBefore) {
			if err := store.ClearBlockedEpisode(ctx, episode.Subject); err != nil {
				writeLine(stdout, "blocked_since role episode clear failed for %s: %v", episode.Subject, err)
			}
		}
	}
	return nil
}

func evaluateInputPendingRoleEpisodes(ctx context.Context, store *db.Store, sink events.Sink, snapshot org.Snapshot, wakeAfter time.Duration, stdout io.Writer, now time.Time) error {
	if store == nil || sink == nil || wakeAfter <= 0 {
		return nil
	}
	pendingSubjects := map[string]string{}
	readySubjects := map[string]struct{}{}
	confirmedNotPending := map[string]struct{}{}
	for role, live := range snapshot.States {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		subject := "input-pending:role:" + role
		switch live.State {
		case org.StateInputPending:
			pendingSubjects[subject] = role
			if err := store.UpsertBlockedEpisode(ctx, subject, snapshot.ObservedAt, now); err != nil {
				writeLine(stdout, "input_pending role %s episode upsert failed: %v", role, err)
				continue
			}
			readySubjects[subject] = struct{}{}
		case org.StateIdle, org.StateWorking, org.StateBlocked, org.StateDone:
			// Unknown or absent is ambiguous and preserves accrued duration through
			// a transient Herdr snapshot blip. Any concrete non-pending state closes
			// the episode.
			confirmedNotPending[subject] = struct{}{}
		}
	}

	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		return err
	}
	staleBefore := now.Add(-blockedEpisodeStaleGap)
	for _, episode := range episodes {
		if !strings.HasPrefix(episode.Subject, "input-pending:role:") {
			continue
		}
		if role, pending := pendingSubjects[episode.Subject]; pending {
			if _, ready := readySubjects[episode.Subject]; !ready {
				continue
			}
			if err := emitInputPendingEpisode(ctx, store, sink, episode, role, wakeAfter, now); err != nil {
				writeLine(stdout, "input_pending role %s emit failed: %v", role, err)
			}
			continue
		}
		if _, confirmed := confirmedNotPending[episode.Subject]; confirmed || blockedEpisodeStale(episode.UpdatedAt, staleBefore) {
			if err := store.ClearBlockedEpisode(ctx, episode.Subject); err != nil {
				writeLine(stdout, "input_pending role episode clear failed for %s: %v", episode.Subject, err)
			}
		}
	}
	return nil
}

func emitInputPendingEpisode(ctx context.Context, store *db.Store, sink events.Sink, episode db.BlockedEpisode, role string, wakeAfter time.Duration, now time.Time) error {
	now = now.UTC()
	pendingSince, pendingFor, due, err := blockedEpisodeDue(episode, wakeAfter, now)
	if err != nil {
		return err
	}
	if !due {
		return nil
	}
	if err := markBlockedEpisodeEmitted(ctx, store, episode, now); err != nil {
		return err
	}
	subjectID := "org-input-pending:" + role
	detail := fmt.Sprintf("role %s input pending %s (since %s)", role, pendingFor.Round(time.Second), pendingSince.UTC().Format(time.RFC3339))
	ev := events.NewEvent(events.EventOrgInputPending, subjectID, subjectID, "", string(org.StateInputPending), detail, now, workflow.RedactCommentText)
	ev.Cause = "input_pending_since"
	ev.WakeTargetRole = role
	events.EmitEvent(ctx, sink, ev)
	return nil
}

// blockedEpisodeStale reports whether an episode's last matching observation
// (updatedAt, fixed-width UTC) is at or before staleBefore. It is shared by the
// blocked and input-pending episode evaluators. An unparseable stamp is treated
// as stale so a malformed row can never linger forever.
func blockedEpisodeStale(updatedAt string, staleBefore time.Time) bool {
	parsed, err := time.Parse(db.BlockedEpisodeTimeLayout, strings.TrimSpace(updatedAt))
	if err != nil {
		return true
	}
	return !parsed.After(staleBefore)
}

func blockedEpisodeDue(episode db.BlockedEpisode, wakeAfter time.Duration, now time.Time) (time.Time, time.Duration, bool, error) {
	now = now.UTC()
	blockedSince, err := time.Parse(db.BlockedEpisodeTimeLayout, episode.BlockedSince)
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("parse blocked_since %q: %w", episode.BlockedSince, err)
	}
	blockedFor := now.Sub(blockedSince)
	if blockedFor <= wakeAfter {
		return time.Time{}, 0, false, nil // not blocked long enough yet
	}
	// Re-nudge at most once per wakeAfter interval while the subject stays blocked
	// (an alert repeat_interval), rather than a single durable one-shot: a wake that
	// is dropped downstream (herdr briefly down, a transient sink, a mark-write
	// failure) self-heals on the next interval instead of being lost forever.
	if last := strings.TrimSpace(episode.EmittedAt); last != "" {
		if lastEmitted, err := time.Parse(db.BlockedEpisodeTimeLayout, last); err == nil && now.Sub(lastEmitted) <= wakeAfter {
			return time.Time{}, 0, false, nil // already nudged within the current interval
		}
	}
	return blockedSince, blockedFor, true, nil
}

func markBlockedEpisodeEmitted(ctx context.Context, store *db.Store, episode db.BlockedEpisode, now time.Time) error {
	// Mark BEFORE emit: a mark-write failure then means no emit this tick (retry
	// next tick, the gate is still open) — never a per-tick duplicate — and a
	// mark-success whose async wake is dropped re-nudges on the next interval.
	if err := store.MarkBlockedEpisodeEmitted(ctx, episode.Subject, now); err != nil {
		return fmt.Errorf("mark blocked episode emitted: %w", err)
	}
	return nil
}

func buildBlockedSinceEvent(subjectID, rootID, repo, detailSubject string, blockedSince time.Time, blockedFor time.Duration, now time.Time) events.Event {
	// Carry the stable first_since (blocked_since) so a consumer distinguishes a
	// re-nudge (same job_id + same since) from a genuinely fresh episode after a
	// re-block (same job_id, new since) — a repeat must not read as a fresh alert.
	detail := fmt.Sprintf("%s blocked %s (since %s)", detailSubject, blockedFor.Round(time.Second), blockedSince.UTC().Format(time.RFC3339))
	ev := events.NewEvent(events.EventJobBlocked, subjectID, rootID, repo, string(workflow.TaskBlocked), detail, now, workflow.RedactCommentText)
	ev.Cause = "blocked_since"
	return ev
}

func emitBlockedSinceEpisode(ctx context.Context, store *db.Store, sink events.Sink, episode db.BlockedEpisode, subjectID, rootID, repo, detailSubject string, wakeAfter time.Duration, now time.Time) error {
	now = now.UTC()
	blockedSince, blockedFor, due, err := blockedEpisodeDue(episode, wakeAfter, now)
	if err != nil {
		return err
	}
	if !due {
		return nil
	}
	if err := markBlockedEpisodeEmitted(ctx, store, episode, now); err != nil {
		return err
	}
	ev := buildBlockedSinceEvent(subjectID, rootID, repo, detailSubject, blockedSince, blockedFor, now)
	events.EmitEvent(ctx, sink, ev)
	return nil
}

type blockedTaskDigestItem struct {
	taskID       string
	blockedSince time.Time
	blockedFor   time.Duration
}

func sortBlockedTaskDigestItems(items []blockedTaskDigestItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].blockedFor == items[j].blockedFor {
			return items[i].taskID < items[j].taskID
		}
		return items[i].blockedFor > items[j].blockedFor
	})
}

func buildBlockedTaskDigestEvent(repo string, items []blockedTaskDigestItem, now time.Time) events.Event {
	oldest := items[0]
	suffix := ""
	if len(items) > 1 {
		suffix = fmt.Sprintf(" (+%d more)", len(items)-1)
	}
	// Carry the anchor's "(since ...)" the same way buildBlockedSinceEvent does
	// for the role path: JobID/RootID here is the oldest item's taskID, so a
	// consumer keying off (job_id, since) can still tell a re-nudge of the SAME
	// anchor apart from a fresh digest whose oldest task changed identity
	// between ticks.
	detail := fmt.Sprintf("%d tasks blocked — oldest %s (since %s) — %s%s",
		len(items), oldest.blockedFor.Round(time.Second), oldest.blockedSince.UTC().Format(time.RFC3339), oldest.taskID, suffix)
	ev := events.NewEvent(events.EventJobBlocked, oldest.taskID, oldest.taskID, repo, string(workflow.TaskBlocked), detail, now, workflow.RedactCommentText)
	ev.Cause = "blocked_since"
	return ev
}

func buildBlockedTaskAlertExhaustedEvent(repo string, item blockedTaskDigestItem, now time.Time) events.Event {
	detail := fmt.Sprintf("task %s remains blocked after %d alerts over %s (since %s); alert ladder exhausted, task remains queryable pending evidence disposal",
		item.taskID, blockedTaskMaxNudges, item.blockedFor.Round(time.Second), item.blockedSince.UTC().Format(time.RFC3339))
	ev := events.NewEvent(events.EventJobNeedsAttention, item.taskID, item.taskID, repo,
		string(workflow.TaskBlocked), detail, now, workflow.RedactCommentText)
	ev.Cause = "escalation"
	return ev
}

func taskEpisodeSubject(repo, taskID string) string {
	return "task:" + strings.TrimSpace(repo) + ":" + strings.TrimSpace(taskID)
}
