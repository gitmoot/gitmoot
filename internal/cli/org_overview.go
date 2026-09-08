package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

type orgLiveSource func(ctx context.Context, cfg config.OrgConfig) (states map[string]org.RoleLiveState, observedAt time.Time, providerVersion string, err error)

const storeOrgLivePresenceMaxAge = 5 * time.Minute

// A single miss older than this stops being flagged as current. The stored
// counter remains untouched for the next real delivery attempt.
const missedWakeStaleAfter = 24 * time.Hour

type orgSharedState struct {
	Config          config.OrgConfig
	Presence        map[string]db.OrgRolePresence
	Store           *db.Store
	MaxMissedWakes  int
	MissedWakes     map[string]int
	Warnings        []string
	jobCounts       map[string]map[string]int
	jobCountsErr    error
	jobCountsLoaded bool
	blockedEpisodes []db.BlockedEpisode
	blockedLoaded   bool
	livePresence    map[string]db.RoleLivePresence
	liveLoaded      bool
	unavailable     map[string]db.OrgRoleUnavailable
}

type orgLiveSourceError struct {
	err error
}

func (e *orgLiveSourceError) Error() string { return e.err.Error() }
func (e *orgLiveSourceError) Unwrap() error { return e.err }

// loadOrgSharedState loads the configuration and store-backed inputs shared by
// CLI and dashboard org projections. The caller owns the already-open store.
func loadOrgSharedState(ctx context.Context, paths config.Paths, store *db.Store, now time.Time) (orgSharedState, error) {
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
		unavailable: map[string]db.OrgRoleUnavailable{},
	}
	for _, row := range rows {
		state.Presence[row.Role] = row
	}
	unavailable, err := store.ListActiveOrgRolesUnavailable(ctx, now)
	if err != nil {
		return orgSharedState{}, fmt.Errorf("load org role unavailability: %w", err)
	}
	for _, row := range unavailable {
		state.unavailable[row.Role] = row
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
		if missedWakeIsStale(row.UpdatedAt, now) {
			continue
		}
		state.MissedWakes[row.Role] = row.Consecutive
	}
	return state, nil
}

func missedWakeIsStale(updatedAt string, now time.Time) bool {
	updated, err := time.Parse(db.BlockedEpisodeTimeLayout, updatedAt)
	if err != nil {
		return false
	}
	return now.Sub(updated) > missedWakeStaleAfter
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
		// Blocked stays sourced from org_blocked_episodes so the node dot cannot
		// contradict the blocked_since badge, Health.Blocked, or the feed: an
		// episode persists through a transient absent/unknown snapshot, while the
		// live-presence row for that role is reaped or ages out of the freshness
		// window. Working/idle come from the persisted Herdr snapshot.
		episodes, err := shared.loadBlockedEpisodes(ctx)
		if err != nil {
			return nil, time.Time{}, "", err
		}
		blocked := make(map[string]bool, len(episodes))
		for _, episode := range episodes {
			if strings.HasPrefix(episode.Subject, "role:") {
				blocked[strings.TrimPrefix(episode.Subject, "role:")] = true
			}
		}
		now := time.Now().UTC()
		var latest time.Time
		members := loadOrgRoster(ctx, shared.Store, cfg).Members()
		states := make(map[string]org.RoleLiveState, len(members))
		for _, role := range members {
			state := org.StateUnknown
			if blocked[role.Name] {
				state = org.StateBlocked
			} else if row, ok := presence[role.Name]; ok {
				if observedAt, parsed := parseOrgPresenceTime(row.ObservedAt); parsed && !observedAt.After(now) && now.Sub(observedAt) <= storeOrgLivePresenceMaxAge {
					switch org.LifecycleState(row.State) {
					case org.StateWorking, org.StateIdle:
						state = org.LifecycleState(row.State)
						if observedAt.After(latest) {
							latest = observedAt
						}
					}
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
		return shared.jobCounts, shared.jobCountsErr
	}
	shared.jobCountsLoaded = true
	counts, err := shared.Store.CountCurrentJobsByOrgRole(ctx)
	if err != nil {
		shared.jobCountsErr = err
		shared.Warnings = append(shared.Warnings, fmt.Sprintf("active-jobs counts unavailable: %v", err))
		return nil, err
	}
	shared.jobCounts = counts
	return counts, nil
}

func buildOrgStatusRows(ctx context.Context, shared *orgSharedState, src orgLiveSource, command string, includeActiveJobs bool) ([]orgStatusOutput, error) {
	states, observedAt, providerVersion, err := src(ctx, shared.Config)
	if err != nil {
		return nil, &orgLiveSourceError{err: err}
	}
	roles := loadOrgRoster(ctx, shared.Store, shared.Config).Members()
	rows := make([]orgStatusOutput, 0, len(roles))
	observedNow := time.Now().UTC()
	for _, role := range roles {
		live, ok := states[role.Name]
		if !ok {
			live = org.RoleLiveState{State: org.StateUnknown, Detail: "provider snapshot omitted this role"}
		}
		unavailableReason := ""
		unavailableUntil := ""
		if incident, unavailable := shared.unavailable[role.Name]; unavailable {
			unavailableReason = incident.Reason
			unavailableUntil = formatOrgRoleUnavailableUntil(incident.Until)
			live = org.RoleLiveState{
				State:  org.StateUnavailable,
				Detail: fmt.Sprintf("⚠ UNAVAILABLE reason=%s until=%s", incident.Reason, unavailableUntil),
			}
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
		activeJobs := 0
		if command == "status" && includeActiveJobs {
			jobCounts, err := shared.loadJobCounts(ctx)
			if err == nil {
				activeJobs = jobCounts[role.Name]["queued"] + jobCounts[role.Name]["running"]
				if recycleAfter := shared.Config.RecycleAfterFor(role.Name); recycleAfter > 0 {
					recycleStatus = orgRecycleStatus(seen.LastSeenAt, observedNow, live.State, activeJobs, recycleAfter)
					recycleAfterText = formatOrgRecycleAfter(recycleAfter)
				}
			}
		}
		rows = append(rows, orgStatusOutput{
			Role: role.Name, Parent: role.Parent, Pane: role.Pane, Depth: len(shared.Config.Path(role.Name)) - 1,
			Scope: role.Scope, MergeRule: role.MergeRule, Model: role.Model, ActiveJobs: activeJobs, LastSeenAt: seen.LastSeenAt, LastSeenAge: orgPresenceAge(seen.LastSeenAt, observedNow), LastCommand: seen.LastCommand,
			// #1702: live.Activity was already on this line and unread. The turn
			// counter distinguishes a seat INSIDE a long turn from one that stopped
			// after a short one; LastSeenAge cannot, because a note age conflates the
			// two - measured on this host, two seats read 27h and 45h by age while
			// their last turn had completed 7 and 4 minutes earlier.
			//
			// LastTurnAge is the half that turns the number into a verdict: for a
			// WORKING seat it is how long the current turn has been running. It needs
			// no stored prior, because the provider reports the completion time
			// itself (org.RoleActivity.CompletedAt, required by the cockpit parser).
			// Movement BETWEEN gitmoot observations is the separate reading that does
			// need storage, and it is deliberately not built here.
			LastTurn:      orgLastTurn(live),
			LastTurnAge:   orgTurnAge(live, observedNow),
			ProviderState: live.State, ProviderDetail: live.Detail, ObservedAt: observedAt, ProviderVersion: providerVersion,
			RecycleStatus: recycleStatus, RecycleAfter: recycleAfterText,
			MissedWakes: consecutive, Flagged: flagged, FlagReason: flagReason,
			UnavailableReason: unavailableReason, UnavailableUntil: unavailableUntil,
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

func formatOrgRoleUnavailableUntil(value string) string {
	until, err := time.Parse(db.BlockedEpisodeTimeLayout, strings.TrimSpace(value))
	if err != nil {
		return strings.TrimSpace(value)
	}
	return until.UTC().Format(time.RFC3339)
}

// orgLastTurn extracts the provider's last completed turn, preserving the
// difference between "not reported" and any numeric value (#1702).
//
// org.RoleActivity's own comment is the contract this honours: "A nil
// *RoleActivity means the provider did not report turn activity; callers must
// not treat that as a zero-valued or stale turn." So a nil activity yields a
// nil turn and the surfaces render it as absent, never as 0 - a rendered 0
// would invent a stalled seat out of a silent provider.
func orgLastTurn(live org.RoleLiveState) *int64 {
	if live.Activity == nil {
		return nil
	}
	turn := live.Activity.Turn
	return &turn
}

// orgTurnText renders a turn for the human surfaces. Absent is a dash, which is
// the same rendering every other unreported column uses, so a reader does not
// have to learn a second convention for missing data.
func orgTurnText(turn *int64) string {
	if turn == nil {
		return "-"
	}
	return strconv.FormatInt(*turn, 10)
}

// orgTurnAge reports how long ago the provider's last turn COMPLETED, which for
// a working seat is how long its current turn has been running (#1702).
//
// This is the half that makes the turn number actionable. Measured on this host,
// with the number and the age side by side:
//
//	deimos            working  turn=117  turn_age=7m   seen=27h32m
//	among-friends-omp done     turn=54   turn_age=4m   seen=45h36m
//	numbra            working  turn=14   turn_age=18h  seen=21h41m
//
// The first two look long dead by note age and are minutes old. numbra is the
// real suspect and NEITHER a low turn number nor a 21h age singles it out: a
// seat reporting WORKING whose last turn completed 18 hours ago. The pair
// produces that reading; neither field alone does.
//
// No stored prior is needed: org.RoleActivity carries CompletedAt and the
// cockpit parser REQUIRES it (a missing completion time yields a nil activity,
// herdr_org.go:180-183), so this is another already-present fact. Detecting
// movement BETWEEN gitmoot observations is the separate reading that needs
// storage, and it stays out of scope.
func orgTurnAge(live org.RoleLiveState, now time.Time) string {
	if live.Activity == nil || live.Activity.CompletedAt.IsZero() {
		return ""
	}
	age := now.Sub(live.Activity.CompletedAt.UTC())
	if age < 0 {
		age = 0
	}
	return age.Round(time.Second).String()
}
