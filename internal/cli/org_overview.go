package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

type orgLiveSource func(ctx context.Context, cfg config.OrgConfig) (states map[string]org.RoleLiveState, observedAt time.Time, providerVersion string, err error)

const storeOrgLivePresenceMaxAge = 5 * time.Minute

type orgSharedState struct {
	Config          config.OrgConfig
	Presence        map[string]db.OrgRolePresence
	Store           *db.Store
	MaxMissedWakes  int
	MissedWakes     map[string]int
	Warnings        []string
	jobCounts       map[string]map[string]int
	jobCountsLoaded bool
	blockedEpisodes []db.BlockedEpisode
	blockedLoaded   bool
	livePresence    map[string]db.RoleLivePresence
	liveLoaded      bool
}

type orgLiveSourceError struct {
	err error
}

func (e *orgLiveSourceError) Error() string { return e.err.Error() }
func (e *orgLiveSourceError) Unwrap() error { return e.err }

// loadOrgSharedState loads the configuration and store-backed inputs shared by
// CLI and dashboard org projections. The caller owns the already-open store.
func loadOrgSharedState(ctx context.Context, paths config.Paths, store *db.Store) (orgSharedState, error) {
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		return orgSharedState{}, fmt.Errorf("load org registry: %w", err)
	}
	if !cfg.Enabled() {
		return orgSharedState{}, errors.New("organization registry is disabled; run `gitmoot org init`")
	}
	rows, err := store.ListOrgRolePresence(ctx)
	if err != nil {
		return orgSharedState{}, err
	}
	state := orgSharedState{
		Config:      cfg,
		Presence:    make(map[string]db.OrgRolePresence, len(rows)),
		Store:       store,
		MissedWakes: map[string]int{},
	}
	for _, row := range rows {
		state.Presence[row.Role] = row
	}

	// Missed-wake flagging is a best-effort add-on. Keep its existing
	// degrade-not-fail behavior and leave rendering of warnings to the caller.
	policy, err := config.LoadOrchestratePolicy(paths)
	if err != nil {
		state.Warnings = append(state.Warnings, fmt.Sprintf("missed-wake flag disabled (orchestrate policy unreadable): %v", err))
		return state, nil
	}
	state.MaxMissedWakes = policy.MaxConsecutiveMissedWakes
	if state.MaxMissedWakes <= 0 {
		return state, nil
	}
	missed, err := store.ListRoleMissedWakes(ctx)
	if err != nil {
		state.Warnings = append(state.Warnings, fmt.Sprintf("missed-wake counts unavailable: %v", err))
		return state, nil
	}
	for _, row := range missed {
		state.MissedWakes[row.Role] = row.Consecutive
	}
	return state, nil
}

func herdrOrgLiveSource(ctx context.Context, cfg config.OrgConfig) (map[string]org.RoleLiveState, time.Time, string, error) {
	snapshot, err := orgProviderSnapshot(ctx, cfg)
	return snapshot.States, snapshot.ObservedAt, snapshot.ProviderVersion, err
}

// storeOrgLiveSource derives live state entirely from the latest persisted
// Herdr snapshot. It does not construct or contact a Herdr provider.
func storeOrgLiveSource(shared *orgSharedState) orgLiveSource {
	return func(ctx context.Context, cfg config.OrgConfig) (map[string]org.RoleLiveState, time.Time, string, error) {
		presence, err := shared.loadLivePresence(ctx)
		if err != nil {
			return nil, time.Time{}, "", err
		}
		now := time.Now().UTC()
		var latest time.Time
		states := make(map[string]org.RoleLiveState, len(cfg.Roles()))
		for _, role := range cfg.Roles() {
			state := org.StateUnknown
			row, ok := presence[role.Name]
			observedAt, parsed := parseOrgPresenceTime(row.ObservedAt)
			if ok && parsed && !observedAt.After(now) && now.Sub(observedAt) <= storeOrgLivePresenceMaxAge {
				switch org.LifecycleState(row.State) {
				case org.StateBlocked, org.StateWorking, org.StateIdle:
					state = org.LifecycleState(row.State)
				}
				if observedAt.After(latest) {
					latest = observedAt
				}
			}
			states[role.Name] = org.RoleLiveState{State: state}
		}
		return states, latest, "store", nil
	}
}

func (shared *orgSharedState) loadLivePresence(ctx context.Context) (map[string]db.RoleLivePresence, error) {
	if shared.liveLoaded {
		return shared.livePresence, nil
	}
	rows, err := shared.Store.ListRoleLivePresence(ctx)
	if err != nil {
		return nil, err
	}
	shared.livePresence = make(map[string]db.RoleLivePresence, len(rows))
	for _, row := range rows {
		shared.livePresence[row.Role] = row
	}
	shared.liveLoaded = true
	return shared.livePresence, nil
}

func (shared *orgSharedState) loadBlockedEpisodes(ctx context.Context) ([]db.BlockedEpisode, error) {
	if shared.blockedLoaded {
		return shared.blockedEpisodes, nil
	}
	episodes, err := shared.Store.ListBlockedEpisodes(ctx)
	if err != nil {
		return nil, err
	}
	shared.blockedEpisodes = episodes
	shared.blockedLoaded = true
	return episodes, nil
}

// loadJobCounts caches the indexed queued/running counts for one overview
// projection. Presence and recycle enrichment share the same snapshot.
func (shared *orgSharedState) loadJobCounts(ctx context.Context) (map[string]map[string]int, error) {
	if shared.jobCountsLoaded {
		return shared.jobCounts, nil
	}
	counts, err := shared.Store.CountCurrentJobsByOrgRole(ctx)
	if err != nil {
		return nil, err
	}
	shared.jobCounts = counts
	shared.jobCountsLoaded = true
	return counts, nil
}

func buildOrgStatusRows(ctx context.Context, shared *orgSharedState, src orgLiveSource, command string) ([]orgStatusOutput, error) {
	states, observedAt, providerVersion, err := src(ctx, shared.Config)
	if err != nil {
		return nil, &orgLiveSourceError{err: err}
	}
	roles := shared.Config.Roles()
	rows := make([]orgStatusOutput, 0, len(roles))
	observedNow := time.Now().UTC()
	for _, role := range roles {
		live, ok := states[role.Name]
		if !ok {
			live = org.RoleLiveState{State: org.StateUnknown, Detail: "provider snapshot omitted this role"}
		}
		seen := shared.Presence[role.Name]
		consecutive := shared.MissedWakes[role.Name]
		flagged := shared.MaxMissedWakes > 0 && consecutive >= shared.MaxMissedWakes
		flagReason := ""
		if flagged {
			flagReason = fmt.Sprintf("%d consecutive missed wakes", consecutive)
		}
		recycleStatus := ""
		recycleAfterText := ""
		if command == "status" {
			recycleAfter := shared.Config.RecycleAfterFor(role.Name)
			if recycleAfter > 0 {
				jobCounts, err := shared.loadJobCounts(ctx)
				if err != nil {
					return nil, fmt.Errorf("count active jobs for role %q: %w", role.Name, err)
				}
				activeJobs := jobCounts[role.Name]["queued"] + jobCounts[role.Name]["running"]
				recycleStatus = orgRecycleStatus(seen.LastSeenAt, observedNow, live.State, activeJobs, recycleAfter)
				recycleAfterText = formatOrgRecycleAfter(recycleAfter)
			}
		}
		rows = append(rows, orgStatusOutput{
			Role: role.Name, Parent: role.Parent, Pane: role.Pane, Depth: len(shared.Config.Path(role.Name)) - 1,
			Scope: role.Scope, MergeRule: role.MergeRule, LastSeenAt: seen.LastSeenAt, LastSeenAge: orgPresenceAge(seen.LastSeenAt, observedNow), LastCommand: seen.LastCommand,
			ProviderState: live.State, ProviderDetail: live.Detail, ObservedAt: observedAt, ProviderVersion: providerVersion,
			RecycleStatus: recycleStatus, RecycleAfter: recycleAfterText,
			MissedWakes: consecutive, Flagged: flagged, FlagReason: flagReason,
		})
	}
	if command == "chart" {
		sort.Slice(rows, func(i, j int) bool {
			left := strings.Join(shared.Config.Path(rows[i].Role), "/")
			right := strings.Join(shared.Config.Path(rows[j].Role), "/")
			return left < right
		})
	}
	return rows, nil
}
