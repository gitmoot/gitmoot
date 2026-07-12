package cli

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

const (
	dashboardTaskMergedWindow = 7 * 24 * time.Hour
	// Open work older than this is stale backlog, not a board card: the live
	// store carries months of dormant planned/blocked tasks that would drown
	// the Tasks board and the needs-you strip.
	dashboardTaskActiveWindow = 30 * 24 * time.Hour
	dashboardTodayWindow      = 24 * time.Hour
	// Bounded blocks: the strip and the needs-you list must stay glanceable on
	// a store with years of history.
	dashboardFleetCap        = 16
	dashboardNeedsYouKindCap = 8
)

// capDashboardNeedsYou bounds each needs-you kind after sorting so one noisy
// kind can never drown the others.
func capDashboardNeedsYou(out *dashboard.Overview) {
	perKind := map[string]int{}
	kept := out.NeedsYou[:0]
	for _, item := range out.NeedsYou {
		if perKind[item.Kind] >= dashboardNeedsYouKindCap {
			continue
		}
		perKind[item.Kind]++
		kept = append(kept, item)
	}
	out.NeedsYou = kept
}

const ()

// Tasks projects Gitmoot's richer internal task lifecycle onto the five
// dashboard columns. CI remains empty because no current CI conclusion is
// persisted locally; dashboard requests never call GitHub.
func (d *webDataSource) Tasks(ctx context.Context) ([]dashboard.TaskSummary, error) {
	now := time.Now().UTC()
	out := []dashboard.TaskSummary{}
	err := withStore(d.home, func(store *db.Store) error {
		var err error
		out, err = dashboardTasks(ctx, store, now)
		return err
	})
	return out, err
}

func dashboardTasks(ctx context.Context, store *db.Store, now time.Time) ([]dashboard.TaskSummary, error) {
	rows, err := store.ListDashboardTasks(ctx, dashboardSQLiteTime(now.Add(-dashboardTaskMergedWindow)))
	if err != nil {
		return nil, err
	}
	out := make([]dashboard.TaskSummary, 0, len(rows))
	activeCutoff := now.Add(-dashboardTaskActiveWindow)
	for _, row := range rows {
		state, blockedReason := dashboardTaskState(row.State)
		updatedAt := parseJobTimeMillis(row.UpdatedAt)
		if state != "merged" && workflowMillisTime(updatedAt).Before(activeCutoff) {
			continue
		}
		item := dashboard.TaskSummary{
			ID: row.ID, Title: row.Title, Repo: row.Repo, State: state, Agent: row.Agent,
			BlockedReason: blockedReason, UpdatedAt: updatedAt,
			AgeS: dashboardAgeSeconds(now, workflowMillisTime(updatedAt)),
		}
		if state == "pr_open" || state == "merged" {
			item.PRNumber = row.PRNumber
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ir, jr := dashboardTaskStateRank(out[i].State), dashboardTaskStateRank(out[j].State); ir != jr {
			return ir < jr
		}
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func dashboardTaskState(state string) (string, string) {
	switch strings.TrimSpace(state) {
	case "planned":
		return "planned", ""
	case "implementing":
		return "implementing", ""
	case "pr_open", "reviewing", "changes_requested", "ready_to_merge":
		return "pr_open", ""
	case "blocked":
		return "blocked", "Task is blocked"
	case "awaiting_human":
		return "blocked", "Awaiting human input"
	case "merged":
		return "merged", ""
	default:
		return "planned", ""
	}
}

func dashboardTaskStateRank(state string) int {
	switch state {
	case "planned":
		return 0
	case "implementing":
		return 1
	case "pr_open":
		return 2
	case "blocked":
		return 3
	case "merged":
		return 4
	default:
		return 5
	}
}

// Overview assembles the operator landing page from local summary queries only.
func (d *webDataSource) Overview(ctx context.Context) (dashboard.Overview, error) {
	now := time.Now().UTC()
	out := emptyDashboardOverview()
	err := withStore(d.home, func(store *db.Store) error {
		var err error
		out, err = dashboardOverview(ctx, store, now)
		return err
	})
	return out, err
}

func emptyDashboardOverview() dashboard.Overview {
	return dashboard.Overview{
		NeedsYou:  []dashboard.OverviewNeedsYou{},
		Activity:  dashboard.OverviewActivity{Workflows: []dashboard.OverviewWorkflowActivity{}},
		Today:     dashboard.OverviewToday{Notable: []dashboard.OverviewNotable{}},
		Scheduled: []dashboard.OverviewScheduled{},
		Fleet:     []dashboard.OverviewFleet{},
	}
}

func dashboardOverview(ctx context.Context, store *db.Store, now time.Time) (dashboard.Overview, error) {
	out := emptyDashboardOverview()
	cutoff := dashboardSQLiteTime(now.Add(-dashboardTodayWindow))

	tasks, err := dashboardTasks(ctx, store, now)
	if err != nil {
		return out, err
	}
	for _, task := range tasks {
		if task.State != "pr_open" {
			continue
		}
		ref := ""
		link := ""
		if task.PRNumber > 0 {
			ref = fmt.Sprintf("#%d", task.PRNumber)
			if strings.TrimSpace(task.Repo) != "" {
				link = fmt.Sprintf("https://github.com/%s/pull/%d", task.Repo, task.PRNumber)
			}
		}
		out.NeedsYou = append(out.NeedsYou, dashboard.OverviewNeedsYou{
			Kind: "pr_awaiting_merge", Repo: task.Repo, Ref: ref, Title: task.Title,
			AgeS: task.AgeS, CI: task.CI, Link: link,
		})
	}

	blocked, err := store.ListDashboardBlockedJobs(ctx)
	if err != nil {
		return out, err
	}
	for _, job := range blocked {
		payload, _ := workflow.ParseJobPayload(job.Payload)
		title := dashboardJobPayloadTitle(payload, job.ID)
		if reason := dashboardShortLine(job.Reason, 160); reason != "" {
			title = reason
		}
		out.NeedsYou = append(out.NeedsYou, dashboard.OverviewNeedsYou{
			Kind: "blocked_job", Repo: job.Repo, Ref: job.Agent, Title: title,
			AgeS: dashboardAgeSeconds(now, workflowMillisTime(parseJobTimeMillis(job.UpdatedAt))),
			Link: "/attention",
		})
	}

	workflows, err := dashboardWorkflowEntries(ctx, store, now)
	if err != nil {
		return out, err
	}
	for _, item := range workflows {
		if item.State != "stalled" {
			continue
		}
		failures := item.Counts.Failed + item.Counts.Blocked
		title := fmt.Sprintf("%d run%s failed · coordinator silent", failures, dashboardPlural(failures))
		out.NeedsYou = append(out.NeedsYou, dashboard.OverviewNeedsYou{
			Kind: "stalled_workflow", Ref: item.Label, Title: title, AgeS: item.StalledForS,
			Link: "/workflows/" + url.PathEscape(item.Label), Label: item.Label,
			Pane: item.Coordinator.Pane, SessionID: item.Coordinator.SessionID, LastNote: item.LastNote,
		})
	}

	active, err := store.ListDashboardActiveJobs(ctx)
	if err != nil {
		return out, err
	}
	out.Activity = dashboardActivity(now, active)

	terminal, err := store.ListDashboardTerminalBuckets(ctx, cutoff, dashboardSQLiteTime(now))
	if err != nil {
		return out, err
	}
	notable, err := store.ListDashboardNotableJobs(ctx, cutoff, 5)
	if err != nil {
		return out, err
	}
	out.Today = dashboardToday(now, terminal, notable)

	pipelines, err := store.ListPipelines(ctx)
	if err != nil {
		return out, err
	}
	for _, pipeline := range pipelines {
		if !pipeline.Enabled || strings.TrimSpace(pipeline.Interval) == "" {
			continue
		}
		out.Scheduled = append(out.Scheduled, dashboard.OverviewScheduled{
			Name: pipeline.Name, Schedule: dashboardPipelineSchedule(pipeline.Interval, pipeline.Jitter),
			LastStatus: pipeline.LastStatus, NextInS: dashboardPipelineNextIn(now, pipeline),
		})
	}

	agents, err := store.ListAgents(ctx)
	if err != nil {
		return out, err
	}
	counts, err := store.ListDashboardFleetCounts(ctx, cutoff)
	if err != nil {
		return out, err
	}
	countByAgent := make(map[string]db.DashboardFleetCount, len(counts))
	for _, count := range counts {
		countByAgent[count.Agent] = count
	}
	for _, agent := range agents {
		count := countByAgent[agent.Name]
		// The strip is "fleet at a glance", not a registry dump: only agents
		// that ran something today (or are running now) earn a card.
		if count.JobsToday == 0 && count.Running == 0 {
			continue
		}
		out.Fleet = append(out.Fleet, dashboard.OverviewFleet{
			Agent: agent.Name, Runtime: agent.Runtime, Running: count.Running > 0, JobsToday: count.JobsToday,
		})
	}

	sortDashboardOverview(&out)
	if len(out.Fleet) > dashboardFleetCap {
		out.Fleet = out.Fleet[:dashboardFleetCap]
	}
	capDashboardNeedsYou(&out)
	return out, nil
}

func dashboardActivity(now time.Time, jobs []db.DashboardJobRow) dashboard.OverviewActivity {
	type group struct {
		running              int
		agents               map[string]struct{}
		oldestRunning, first time.Time
	}
	groups := map[string]*group{}
	queued := 0
	unattended := 0
	for _, job := range jobs {
		if job.State == "queued" {
			queued++
		}
		if strings.TrimSpace(job.WorkflowID) == "" {
			unattended++
			continue
		}
		g := groups[job.WorkflowID]
		if g == nil {
			g = &group{agents: map[string]struct{}{}}
			groups[job.WorkflowID] = g
		}
		if agent := strings.TrimSpace(job.Agent); agent != "" {
			g.agents[agent] = struct{}{}
		}
		created := workflowMillisTime(parseJobTimeMillis(job.CreatedAt))
		if g.first.IsZero() || (!created.IsZero() && created.Before(g.first)) {
			g.first = created
		}
		if job.State == "running" {
			g.running++
			if g.oldestRunning.IsZero() || (!created.IsZero() && created.Before(g.oldestRunning)) {
				g.oldestRunning = created
			}
		}
	}
	out := dashboard.OverviewActivity{Workflows: []dashboard.OverviewWorkflowActivity{}, Queued: queued}
	for label, g := range groups {
		agents := make([]string, 0, len(g.agents))
		for agent := range g.agents {
			agents = append(agents, agent)
		}
		sort.Strings(agents)
		started := g.oldestRunning
		if started.IsZero() {
			started = g.first
		}
		namespace, campaign := splitDashboardWorkflowLabel(label)
		out.Workflows = append(out.Workflows, dashboard.OverviewWorkflowActivity{
			Label: label, Namespace: namespace, Campaign: campaign, Running: g.running,
			Agents: agents, StartedAgoS: dashboardAgeSeconds(now, started),
		})
	}
	if unattended > 0 {
		out.UnattendedNote = fmt.Sprintf("%d active job%s without a workflow label", unattended, dashboardPlural(unattended))
	}
	return out
}

func dashboardToday(now time.Time, terminal []db.DashboardTerminalBucket, notable []db.DashboardJobRow) dashboard.OverviewToday {
	out := dashboard.OverviewToday{Notable: []dashboard.OverviewNotable{}}
	for _, bucket := range terminal {
		switch bucket.State {
		case "succeeded":
			out.Completed += bucket.Jobs
		case "failed":
			out.Failed += bucket.Jobs
		case "cancelled":
			out.Cancelled += bucket.Jobs
		}
		out.TokensIn += bucket.InputTokens
		out.TokensOut += bucket.OutputTokens
		index := 23 - bucket.AgeHours
		if index >= 0 && index < len(out.PerHour) {
			out.PerHour[index] += bucket.Jobs
		}
	}
	for _, job := range notable {
		payload, _ := workflow.ParseJobPayload(job.Payload)
		created := workflowMillisTime(parseJobTimeMillis(job.CreatedAt))
		updated := workflowMillisTime(parseJobTimeMillis(job.UpdatedAt))
		out.Notable = append(out.Notable, dashboard.OverviewNotable{
			Agent: job.Agent, Title: dashboardJobPayloadTitle(payload, job.ID), Outcome: job.State,
			ElapsedS: max(0, int64(updated.Sub(created)/time.Second)),
			AgeS:     dashboardAgeSeconds(now, updated),
		})
	}
	return out
}

func dashboardPipelineSchedule(interval, jitter string) string {
	out := "every " + strings.TrimSpace(interval)
	if jitter = strings.TrimSpace(jitter); jitter != "" {
		out += " +" + jitter
	}
	return out
}

func dashboardPipelineNextIn(now time.Time, pipeline db.Pipeline) int64 {
	next := pipeline.NextDueAt.UTC()
	if interval, err := time.ParseDuration(strings.TrimSpace(pipeline.Interval)); err == nil && !pipeline.LastRunAt.IsZero() {
		next = pipeline.LastRunAt.UTC().Add(interval)
	}
	if next.IsZero() {
		return 0
	}
	return max(0, int64(next.Sub(now)/time.Second))
}

func dashboardJobPayloadTitle(payload workflow.JobPayload, fallback string) string {
	if title := dashboardShortLine(payload.TaskTitle, 160); title != "" {
		return title
	}
	if title := firstInstructionLine(payload.Instructions); title != "" {
		return dashboardShortLine(title, 160)
	}
	return fallback
}

func dashboardShortLine(value string, limit int) string {
	value = dashboardWorkflowOneLine(value)
	if limit > 0 && len(value) > limit {
		value = value[:limit] + "…"
	}
	return value
}

func dashboardAgeSeconds(now, then time.Time) int64 {
	if then.IsZero() {
		return 0
	}
	return max(0, int64(now.Sub(then)/time.Second))
}

func dashboardSQLiteTime(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04:05")
}

func dashboardPlural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func sortDashboardOverview(out *dashboard.Overview) {
	sort.SliceStable(out.NeedsYou, func(i, j int) bool {
		if ir, jr := dashboardNeedRank(out.NeedsYou[i].Kind), dashboardNeedRank(out.NeedsYou[j].Kind); ir != jr {
			return ir < jr
		}
		if out.NeedsYou[i].AgeS != out.NeedsYou[j].AgeS {
			return out.NeedsYou[i].AgeS < out.NeedsYou[j].AgeS
		}
		return out.NeedsYou[i].Ref < out.NeedsYou[j].Ref
	})
	sort.SliceStable(out.Activity.Workflows, func(i, j int) bool {
		if out.Activity.Workflows[i].Running != out.Activity.Workflows[j].Running {
			return out.Activity.Workflows[i].Running > out.Activity.Workflows[j].Running
		}
		return out.Activity.Workflows[i].Label < out.Activity.Workflows[j].Label
	})
	sort.SliceStable(out.Today.Notable, func(i, j int) bool {
		if out.Today.Notable[i].AgeS != out.Today.Notable[j].AgeS {
			return out.Today.Notable[i].AgeS < out.Today.Notable[j].AgeS
		}
		return out.Today.Notable[i].Title < out.Today.Notable[j].Title
	})
	sort.SliceStable(out.Scheduled, func(i, j int) bool {
		if out.Scheduled[i].NextInS != out.Scheduled[j].NextInS {
			return out.Scheduled[i].NextInS < out.Scheduled[j].NextInS
		}
		return out.Scheduled[i].Name < out.Scheduled[j].Name
	})
	sort.SliceStable(out.Fleet, func(i, j int) bool {
		if out.Fleet[i].Running != out.Fleet[j].Running {
			return out.Fleet[i].Running
		}
		if out.Fleet[i].JobsToday != out.Fleet[j].JobsToday {
			return out.Fleet[i].JobsToday > out.Fleet[j].JobsToday
		}
		return out.Fleet[i].Agent < out.Fleet[j].Agent
	})
}

func dashboardNeedRank(kind string) int {
	switch kind {
	case "stalled_workflow":
		return 0
	case "pr_awaiting_merge":
		return 1
	case "blocked_job":
		return 2
	case "groom_proposal":
		return 3
	default:
		return 4
	}
}
