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
	blockedEpisodeTimeLayout   = "2006-01-02T15:04:05.000000000Z"
)

type blockedRoleAvailability interface {
	Available(context.Context) bool
}

type blockedRoleWakeDependencies struct {
	availability blockedRoleAvailability
	provider     func([]string) org.Provider
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
		blockedSince, err := parseTaskUpdatedAt(candidate.UpdatedAt)
		if err != nil {
			writeLine(stdout, "blocked_since task %s skipped: %v", candidate.ID, err)
			continue
		}
		if err := store.UpsertBlockedEpisode(ctx, subject, blockedSince); err != nil {
			writeLine(stdout, "blocked_since task %s episode upsert failed: %v", candidate.ID, err)
			continue
		}
		staleSubjects[subject] = candidate.ID
	}

	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		return err
	}
	prefix := "task:" + strings.TrimSpace(repo) + ":"
	for _, episode := range episodes {
		if !strings.HasPrefix(episode.Subject, prefix) {
			continue
		}
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
	wakeAfter := resolveBlockedRoleWakeAfter(home)
	if wakeAfter <= 0 || store == nil || deps.eventSink == nil {
		return
	}
	sink, err := deps.eventSink(ctx, store, home)
	if err != nil {
		writeLine(stdout, "blocked_since role event sink unavailable: %v", err)
		return
	}
	if sink == nil || deps.availability == nil || !deps.availability.Available(ctx) {
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
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
	}
	if deps.provider == nil {
		return
	}
	provider := deps.provider(names)
	if provider == nil {
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
	if err := evaluateBlockedRoleEpisodes(ctx, store, sink, snapshot, wakeAfter, stdout, now.UTC()); err != nil {
		writeLine(stdout, "blocked_since role evaluation failed: %v", err)
	}
}

func evaluateBlockedRoleEpisodes(ctx context.Context, store *db.Store, sink events.Sink, snapshot org.Snapshot, wakeAfter time.Duration, stdout io.Writer, now time.Time) error {
	if store == nil || sink == nil || wakeAfter <= 0 {
		return nil
	}
	blockedSubjects := map[string]string{}
	readySubjects := map[string]struct{}{}
	for role, live := range snapshot.States {
		if live.State != org.StateBlocked {
			continue
		}
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		subject := "role:" + role
		blockedSubjects[subject] = role
		if err := store.UpsertBlockedEpisode(ctx, subject, snapshot.ObservedAt); err != nil {
			writeLine(stdout, "blocked_since role %s episode upsert failed: %v", role, err)
			continue
		}
		readySubjects[subject] = struct{}{}
	}

	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		return err
	}
	for _, episode := range episodes {
		if !strings.HasPrefix(episode.Subject, "role:") {
			continue
		}
		role, blocked := blockedSubjects[episode.Subject]
		if !blocked {
			if err := store.ClearBlockedEpisode(ctx, episode.Subject); err != nil {
				writeLine(stdout, "blocked_since role episode clear failed for %s: %v", episode.Subject, err)
			}
			continue
		}
		if _, ready := readySubjects[episode.Subject]; !ready {
			continue
		}
		subjectID := "org-blocked:" + role
		if err := emitBlockedSinceEpisode(ctx, store, sink, episode, subjectID, subjectID, "", "role "+role, wakeAfter, now); err != nil {
			writeLine(stdout, "blocked_since role %s emit failed: %v", role, err)
		}
	}
	return nil
}

func emitBlockedSinceEpisode(ctx context.Context, store *db.Store, sink events.Sink, episode db.BlockedEpisode, subjectID, rootID, repo, detailSubject string, wakeAfter time.Duration, now time.Time) error {
	if strings.TrimSpace(episode.EmittedAt) != "" {
		return nil
	}
	blockedSince, err := time.Parse(blockedEpisodeTimeLayout, episode.BlockedSince)
	if err != nil {
		return fmt.Errorf("parse blocked_since %q: %w", episode.BlockedSince, err)
	}
	blockedFor := now.UTC().Sub(blockedSince)
	if blockedFor <= wakeAfter {
		return nil
	}
	if blockedFor < 0 {
		blockedFor = 0
	}
	blockedFor = blockedFor.Round(time.Second)
	detail := fmt.Sprintf("%s blocked %s", detailSubject, blockedFor)
	ev := events.NewEvent(events.EventJobBlocked, subjectID, rootID, repo, string(workflow.TaskBlocked), detail, now, workflow.RedactCommentText)
	ev.Cause = "blocked_since"
	events.EmitEvent(ctx, sink, ev)
	return store.MarkBlockedEpisodeEmitted(ctx, episode.Subject)
}

func taskEpisodeSubject(repo, taskID string) string {
	return "task:" + strings.TrimSpace(repo) + ":" + strings.TrimSpace(taskID)
}

func parseTaskUpdatedAt(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, blockedEpisodeTimeLayout} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse task updated_at %q", raw)
}
