package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const dashboardOrgNoteLimit = 200

// Org serves the deterministic store-backed organization snapshot. The cache
// uses a read-only cursor lookup and a short hard TTL because presence and
// episode tables are intentionally not dashboard cursor components.
func (d *webDataSource) Org(ctx context.Context) (dashboard.OrgView, error) {
	paths, err := pathsFromFlag(d.home)
	if err != nil {
		return dashboard.OrgView{}, err
	}
	var out dashboard.OrgView
	err = withReadOnlyStore(d.home, func(store *db.Store) error {
		cursor, err := dashboardOrgCursor(ctx, store)
		if err != nil {
			return err
		}
		body, _, err := d.cacheForDashboard().get(ctx, "org", cursor, dashboardOrgCachePolicy, func(cacheCtx context.Context) ([]byte, error) {
			view, err := buildDashboardOrg(cacheCtx, paths, store, d.home, time.Now().UTC())
			if err != nil {
				return nil, err
			}
			return marshalDashboardJSON(view)
		})
		if err != nil {
			return err
		}
		return json.Unmarshal(body, &out)
	})
	return out, err
}

// OrgRole serves one configured role's store-backed drill-down.
func (d *webDataSource) OrgRole(ctx context.Context, name string) (dashboard.OrgRoleView, error) {
	paths, err := pathsFromFlag(d.home)
	if err != nil {
		return dashboard.OrgRoleView{}, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	var out dashboard.OrgRoleView
	err = withReadOnlyStore(d.home, func(store *db.Store) error {
		cursor, err := dashboardOrgCursor(ctx, store)
		if err != nil {
			return err
		}
		body, _, err := d.cacheForDashboard().get(ctx, "org-role:"+name, cursor, dashboardOrgRoleCachePolicy, func(cacheCtx context.Context) ([]byte, error) {
			view, err := buildDashboardOrgRole(cacheCtx, paths, store, name, time.Now().UTC())
			if err != nil {
				return nil, err
			}
			return marshalDashboardJSON(view)
		})
		if err != nil {
			return err
		}
		return json.Unmarshal(body, &out)
	})
	return out, err
}

func dashboardOrgCursor(ctx context.Context, store *db.Store) (string, error) {
	jobEventID, workflowNoteID, taskEventID, memoryEventID, err := store.DashboardChangeCursor(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%d.%d.%d", jobEventID, workflowNoteID, taskEventID, memoryEventID), nil
}

type dashboardOrgInputs struct {
	shared           orgSharedState
	rows             []orgStatusOutput
	blocked          []db.BlockedEpisode
	recycleOverdue   []db.RecycleOverdueEpisode
	missedWakes      []db.RoleMissedWake
	escalationNotes  []db.WorkflowNote
	handoffNotes     []db.WorkflowNote
	detectionEnabled bool
	detectionHint    string
	dataAsOf         time.Time
}

func loadDashboardOrgInputs(ctx context.Context, paths config.Paths, store *db.Store, home string) (dashboardOrgInputs, error) {
	shared, err := loadOrgSharedState(ctx, paths, store)
	if err != nil {
		return dashboardOrgInputs{}, err
	}
	rows, err := buildOrgStatusRows(ctx, &shared, storeOrgLiveSource(&shared), "status")
	if err != nil {
		return dashboardOrgInputs{}, err
	}
	blocked, err := shared.loadBlockedEpisodes(ctx)
	if err != nil {
		return dashboardOrgInputs{}, err
	}
	recycleOverdue, err := store.ListRecycleOverdueEpisodes(ctx)
	if err != nil {
		return dashboardOrgInputs{}, err
	}
	missedWakes, err := store.ListRoleMissedWakes(ctx)
	if err != nil {
		return dashboardOrgInputs{}, err
	}
	escalationNotes, err := store.ListWorkflowNotesByBodyPrefix(ctx, workflow.OrgEscalatePrefix, dashboardOrgNoteLimit)
	if err != nil {
		return dashboardOrgInputs{}, err
	}
	handoffNotes, err := store.ListWorkflowNotesByBodyPrefix(ctx, workflow.OrgHandoffPrefix, dashboardOrgNoteLimit)
	if err != nil {
		return dashboardOrgInputs{}, err
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		return dashboardOrgInputs{}, err
	}

	wakeAfter := resolveBlockedRoleWakeAfter(home)
	enabledRules := hasEnabledEventRule(rules)
	enabled := wakeAfter > 0 && enabledRules
	hint := ""
	switch {
	case wakeAfter <= 0:
		hint = "blocked detection off - blocked_role_wake_after is disabled"
	case !enabledRules:
		hint = "blocked detection off - no enabled org event rules"
	}

	inputs := dashboardOrgInputs{
		shared: shared, rows: rows, blocked: blocked, recycleOverdue: recycleOverdue,
		missedWakes: missedWakes, escalationNotes: escalationNotes, handoffNotes: handoffNotes,
		detectionEnabled: enabled, detectionHint: hint,
	}
	for _, presence := range shared.Presence {
		inputs.observe(presence.LastSeenAt)
	}
	for _, episode := range blocked {
		inputs.observe(episode.UpdatedAt)
	}
	for _, episode := range recycleOverdue {
		inputs.observe(episode.UpdatedAt)
	}
	for _, missed := range missedWakes {
		inputs.observe(missed.UpdatedAt)
	}
	for _, note := range escalationNotes {
		inputs.observe(note.CreatedAt)
	}
	for _, note := range handoffNotes {
		inputs.observe(note.CreatedAt)
	}
	return inputs, nil
}

func (i *dashboardOrgInputs) observe(value string) {
	if observed, ok := parseOrgPresenceTime(value); ok && observed.After(i.dataAsOf) {
		i.dataAsOf = observed.UTC()
	}
}

func buildDashboardOrg(ctx context.Context, paths config.Paths, store *db.Store, home string, now time.Time) (dashboard.OrgView, error) {
	inputs, err := loadDashboardOrgInputs(ctx, paths, store, home)
	if err != nil {
		return dashboard.OrgView{}, err
	}
	out := dashboard.OrgView{
		DetectionEnabled: inputs.detectionEnabled,
		DetectionHint:    inputs.detectionHint,
		Roles:            []dashboard.OrgNode{},
		Escalations:      dashboardOrgEscalations(inputs.escalationNotes, ""),
		Feed:             dashboardOrgFeed(inputs.blocked, inputs.recycleOverdue, inputs.handoffNotes),
	}
	if !inputs.dataAsOf.IsZero() {
		out.DataAsOf = inputs.dataAsOf.Format(time.RFC3339)
	}

	blockedSince := dashboardOrgBlockedSince(inputs.blocked)
	for _, row := range inputs.rows {
		recycleAfter := inputs.shared.Config.RecycleAfterFor(row.Role)
		_, overdue := dashboardOrgRecycleTiming(row.LastSeenAt, now, recycleAfter)
		node := dashboard.OrgNode{
			Name: row.Role, Parent: row.Parent, Depth: row.Depth,
			Scope: append([]string{}, row.Scope...), MergeRule: row.MergeRule, Pane: row.Pane,
			PresenceState:  dashboardOrgPresenceState(row.ProviderState),
			PresenceDetail: row.ProviderDetail,
			Badges: dashboard.OrgBadges{
				BlockedSince: dashboardOrgTimestamp(blockedSince[row.Role]),
				Overdue:      overdue,
				MissedWakes:  inputs.shared.MissedWakes[row.Role],
			},
			LastSeenAt: dashboardOrgTimestamp(row.LastSeenAt),
		}
		out.Roles = append(out.Roles, node)
		switch node.PresenceState {
		case "working":
			out.Health.Working++
		case "blocked":
			out.Health.Blocked++
		}
		if node.Badges.Overdue != "" {
			out.Health.Overdue++
		}
	}
	sort.Slice(out.Roles, func(i, j int) bool {
		left := strings.Join(inputs.shared.Config.Path(out.Roles[i].Name), "/")
		right := strings.Join(inputs.shared.Config.Path(out.Roles[j].Name), "/")
		return left < right
	})
	out.Health.Roles = len(out.Roles)
	out.Health.OpenEscalations = len(out.Escalations)
	for _, missed := range inputs.missedWakes {
		if missed.Consecutive > 0 {
			out.Health.StalledWakes++
		}
	}
	return out, nil
}

func buildDashboardOrgRole(ctx context.Context, paths config.Paths, store *db.Store, name string, now time.Time) (dashboard.OrgRoleView, error) {
	shared, err := loadOrgSharedState(ctx, paths, store)
	if err != nil {
		return dashboard.OrgRoleView{}, err
	}
	role, ok := shared.Config.Role(name)
	if !ok {
		return dashboard.OrgRoleView{}, dashboard.ErrOrgRoleNotFound
	}
	rows, err := buildOrgStatusRows(ctx, &shared, storeOrgLiveSource(&shared), "status")
	if err != nil {
		return dashboard.OrgRoleView{}, err
	}
	var row orgStatusOutput
	for _, candidate := range rows {
		if candidate.Role == role.Name {
			row = candidate
			break
		}
	}
	blocked, err := shared.loadBlockedEpisodes(ctx)
	if err != nil {
		return dashboard.OrgRoleView{}, err
	}
	escalationNotes, err := store.ListWorkflowNotesByBodyPrefix(ctx, workflow.OrgEscalatePrefix, dashboardOrgNoteLimit)
	if err != nil {
		return dashboard.OrgRoleView{}, err
	}
	notes, err := store.ListWorkflowNotes(ctx, "org/"+role.Name, 0)
	if err != nil {
		// Activity-note count and the latest handoff are advisory. Presence and
		// identity remain useful if that narrow journal read degrades.
		notes = []db.WorkflowNote{}
	}
	jobsToday, err := store.CountJobsByOrgRoleSince(ctx, now.UTC().Truncate(24*time.Hour))
	if err != nil {
		return dashboard.OrgRoleView{}, err
	}

	path := shared.Config.Path(role.Name)
	remaining, overdue := dashboardOrgRecycleTiming(row.LastSeenAt, now, shared.Config.RecycleAfterFor(role.Name))
	out := dashboard.OrgRoleView{
		Identity: dashboard.OrgRoleIdentity{
			Name: role.Name, Parent: role.Parent, MergeRule: role.MergeRule, Pane: role.Pane,
			Scope: append([]string{}, role.Scope...), Depth: len(path) - 1, Path: append([]string{}, path...),
		},
		Presence: dashboard.OrgRolePresence{
			State: dashboardOrgPresenceState(row.ProviderState), BlockedSince: dashboardOrgTimestamp(dashboardOrgBlockedSince(blocked)[role.Name]),
			LastSeenAt: dashboardOrgTimestamp(row.LastSeenAt), MissedWakes: shared.MissedWakes[role.Name],
		},
		Recycle: dashboard.OrgRoleRecycle{
			RecycleAfter: formatOrgRecycleAfter(shared.Config.RecycleAfterFor(role.Name)),
			Remaining:    remaining, Overdue: overdue,
		},
		Activity: dashboard.OrgRoleActivity{
			JobsToday: copyOrgJobCounts(jobsToday[role.Name]),
			Notes:     len(notes),
		},
		Escalations: dashboardOrgEscalations(escalationNotes, role.Name),
	}
	for index := len(notes) - 1; index >= 0; index-- {
		noteRole, handoff, ok := workflow.ParseOrgHandoffNote(notes[index].Body)
		if ok && noteRole == role.Name {
			out.Recycle.LastHandoffAt = dashboardOrgTimestamp(notes[index].CreatedAt)
			out.Recycle.LastHandoffText = handoff
			break
		}
	}
	return out, nil
}

func dashboardOrgPresenceState(state org.LifecycleState) string {
	switch state {
	case org.StateBlocked:
		return "blocked"
	case org.StateWorking:
		return "working"
	case org.StateIdle:
		return "idle"
	default:
		return "never-seen"
	}
}

func dashboardOrgBlockedSince(episodes []db.BlockedEpisode) map[string]string {
	out := map[string]string{}
	for _, episode := range episodes {
		if !strings.HasPrefix(episode.Subject, "role:") {
			continue
		}
		role := strings.TrimPrefix(episode.Subject, "role:")
		if role != "" {
			out[role] = episode.BlockedSince
		}
	}
	return out
}

func dashboardOrgRecycleTiming(lastSeen string, now time.Time, recycleAfter time.Duration) (remaining, overdue string) {
	age, known, isOverdue := orgRecycleAge(lastSeen, now, recycleAfter)
	if !known {
		return "", ""
	}
	if isOverdue {
		return "", dashboardOrgDuration(age - recycleAfter)
	}
	return dashboardOrgDuration(recycleAfter - age), ""
}

func dashboardOrgDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	return value.Round(time.Second).String()
}

func dashboardOrgTimestamp(value string) string {
	if parsed, ok := parseOrgPresenceTime(value); ok {
		return parsed.UTC().Format(time.RFC3339)
	}
	return ""
}

type dashboardOrgEscalationRecord struct {
	view dashboard.OrgEscalation
	id   int64
}

func dashboardOrgEscalations(notes []db.WorkflowNote, role string) []dashboard.OrgEscalation {
	records := []dashboardOrgEscalationRecord{}
	for _, note := range notes {
		from, to, workflowID, question, ok := workflow.ParseOrgEscalateNote(note.Body)
		if !ok || (role != "" && from != role && to != role) {
			continue
		}
		records = append(records, dashboardOrgEscalationRecord{
			view: dashboard.OrgEscalation{From: from, To: to, Wf: workflowID, Question: question, At: dashboardOrgTimestamp(note.CreatedAt)},
			id:   note.ID,
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].view.At != records[j].view.At {
			return records[i].view.At < records[j].view.At
		}
		return records[i].id < records[j].id
	})
	out := make([]dashboard.OrgEscalation, 0, len(records))
	for _, record := range records {
		out = append(out, record.view)
	}
	return out
}

func dashboardOrgFeed(blocked []db.BlockedEpisode, recycle []db.RecycleOverdueEpisode, handoffs []db.WorkflowNote) []dashboard.OrgFeedRow {
	out := []dashboard.OrgFeedRow{}
	for _, episode := range blocked {
		if !strings.HasPrefix(episode.Subject, "role:") {
			continue
		}
		out = append(out, dashboard.OrgFeedRow{
			Kind: "blocked_since", Role: strings.TrimPrefix(episode.Subject, "role:"),
			At: dashboardOrgTimestamp(episode.EmittedAt), Since: dashboardOrgTimestamp(episode.BlockedSince),
		})
	}
	for _, episode := range recycle {
		out = append(out, dashboard.OrgFeedRow{
			Kind: "recycle_overdue", Role: episode.Subject,
			At: dashboardOrgTimestamp(episode.EmittedAt), Since: dashboardOrgTimestamp(episode.OverdueSince),
		})
	}
	for _, note := range handoffs {
		role, handoff, ok := workflow.ParseOrgHandoffNote(note.Body)
		if !ok {
			continue
		}
		out = append(out, dashboard.OrgFeedRow{
			Kind: "recycle", Role: role, At: dashboardOrgTimestamp(note.CreatedAt),
			Detail: "journaled handoff: " + handoff,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

func copyOrgJobCounts(counts map[string]int) map[string]int {
	out := map[string]int{}
	for state, count := range counts {
		out[state] = count
	}
	return out
}
