package cli

import (
	"context"
	"fmt"
	"io"
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
}

func defaultBlockedRoleWakeDependencies() blockedRoleWakeDependencies {
	return blockedRoleWakeDependencies{
		availability: cockpit.New(cockpit.Options{HerdrBin: "herdr"}, nil),
		provider:     cockpit.NewHerdrOrgProvider,
		eventSink:    enabledBlockedSinceEventSink,
	}
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
		if err := emitBlockedSinceEpisode(ctx, store, sink, episode, taskID, taskID, repo, "task "+taskID, wakeAfter, now); err != nil {
			writeLine(stdout, "blocked_since task %s emit failed: %v", taskID, err)
		}
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
	configFile := resolveConfigFile(home)
	if configFile == "" {
		return
	}
	orgConfig, err := config.LoadOrg(config.Paths{ConfigFile: configFile})
	if err != nil {
		writeLine(stdout, "blocked_since role org config load failed: %v", err)
		return
	}
	roles := orgConfig.Roles()
	if len(roles) == 0 {
		// No org roles configured: there is nothing to snapshot or persist, so
		// skip silently (matches the pre-#1118 behavior for an unconfigured
		// daemon). Checking this FIRST means a herdr-less/no-org deployment — the
		// common OSS/automated-user case — never probes herdr and never logs.
		return
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
	if wakeAfter <= 0 || deps.eventSink == nil {
		return
	}
	sink, err := deps.eventSink(ctx, store, home)
	if err != nil {
		writeLine(stdout, "blocked_since role event sink unavailable: %v", err)
		return
	}
	if sink == nil {
		return
	}
	if err := evaluateBlockedRoleEpisodes(ctx, store, sink, snapshot, wakeAfter, stdout, now.UTC()); err != nil {
		writeLine(stdout, "blocked_since role evaluation failed: %v", err)
	}
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
		case org.StateIdle, org.StateWorking, org.StateDone:
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

// blockedEpisodeStale reports whether an episode's last blocked observation
// (updatedAt, fixed-width UTC) is at or before staleBefore. An unparseable stamp
// is treated as stale so a malformed row can never linger forever.
func blockedEpisodeStale(updatedAt string, staleBefore time.Time) bool {
	parsed, err := time.Parse(db.BlockedEpisodeTimeLayout, strings.TrimSpace(updatedAt))
	if err != nil {
		return true
	}
	return !parsed.After(staleBefore)
}

func emitBlockedSinceEpisode(ctx context.Context, store *db.Store, sink events.Sink, episode db.BlockedEpisode, subjectID, rootID, repo, detailSubject string, wakeAfter time.Duration, now time.Time) error {
	now = now.UTC()
	blockedSince, err := time.Parse(db.BlockedEpisodeTimeLayout, episode.BlockedSince)
	if err != nil {
		return fmt.Errorf("parse blocked_since %q: %w", episode.BlockedSince, err)
	}
	blockedFor := now.Sub(blockedSince)
	if blockedFor <= wakeAfter {
		return nil // not blocked long enough yet
	}
	// Re-nudge at most once per wakeAfter interval while the subject stays blocked
	// (an alert repeat_interval), rather than a single durable one-shot: a wake that
	// is dropped downstream (herdr briefly down, a transient sink, a mark-write
	// failure) self-heals on the next interval instead of being lost forever.
	if last := strings.TrimSpace(episode.EmittedAt); last != "" {
		if lastEmitted, err := time.Parse(db.BlockedEpisodeTimeLayout, last); err == nil && now.Sub(lastEmitted) <= wakeAfter {
			return nil // already nudged within the current interval
		}
	}
	// Mark BEFORE emit: a mark-write failure then means no emit this tick (retry
	// next tick, the gate is still open) — never a per-tick duplicate — and a
	// mark-success whose async wake is dropped re-nudges on the next interval.
	if err := store.MarkBlockedEpisodeEmitted(ctx, episode.Subject, now); err != nil {
		return fmt.Errorf("mark blocked episode emitted: %w", err)
	}
	// Carry the stable first_since (blocked_since) so a consumer distinguishes a
	// re-nudge (same job_id + same since) from a genuinely fresh episode after a
	// re-block (same job_id, new since) — a repeat must not read as a fresh alert.
	detail := fmt.Sprintf("%s blocked %s (since %s)", detailSubject, blockedFor.Round(time.Second), blockedSince.UTC().Format(time.RFC3339))
	ev := events.NewEvent(events.EventJobBlocked, subjectID, rootID, repo, string(workflow.TaskBlocked), detail, now, workflow.RedactCommentText)
	ev.Cause = "blocked_since"
	events.EmitEvent(ctx, sink, ev)
	return nil
}

func taskEpisodeSubject(repo, taskID string) string {
	return "task:" + strings.TrimSpace(repo) + ":" + strings.TrimSpace(taskID)
}
