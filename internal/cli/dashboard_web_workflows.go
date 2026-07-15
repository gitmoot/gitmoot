package cli

import (
	"context"
	"sort"
	"strings"
	"time"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/db"
)

const (
	// A recently touched workflow remains recent long enough for coordinator
	// handoffs and short pauses between dispatches to read as one campaign.
	dashboardWorkflowActiveWindow = 30 * time.Minute
	// A failed/blocked quiet workflow needs attention for one day; after that it
	// ages into settled history instead of pinning the operator's stalled list.
	dashboardWorkflowStalledHorizon = 24 * time.Hour
)

type dashboardWorkflowActivity struct {
	Queued, Running, Failed, Blocked int
	LastActivity                     time.Time
	// LastFailure/LastHumanNote drive the acknowledgment rule: a failure is only
	// an alarm while no human journal note has been written AFTER it. Daemon PR
	// receipts remain ordinary activity but cannot acknowledge an alarm.
	LastFailure   time.Time
	LastHumanNote time.Time
}

// deriveDashboardWorkflowState is the single lifecycle definition shared by
// workflow index and detail responses.
func deriveDashboardWorkflowState(now time.Time, activity dashboardWorkflowActivity) (state string, stalledForS int64) {
	age := now.Sub(activity.LastActivity)
	if activity.Queued > 0 || activity.Running > 0 {
		return "active", 0
	}
	failureUnacknowledged := !activity.LastFailure.IsZero() &&
		(activity.LastHumanNote.IsZero() || activity.LastHumanNote.Before(activity.LastFailure))
	if (activity.Failed > 0 || activity.Blocked > 0) && failureUnacknowledged &&
		age > dashboardWorkflowActiveWindow && age < dashboardWorkflowStalledHorizon {
		return "stalled", max(0, int64(age/time.Second))
	}
	if !activity.LastActivity.IsZero() && age <= dashboardWorkflowActiveWindow {
		return "recent", 0
	}
	return "settled", 0
}

// Workflows returns one deterministic index row for every explicit workflow
// label. Unlabeled jobs are omitted from the workflow index.
// workflowAPIEntries returns the widened (description/status) index entries.
// It is the source for both the cached /api/workflows shadow (workflowsJSON)
// and any caller needing the server-owned extension fields.
func (d *webDataSource) workflowAPIEntries(ctx context.Context) ([]dashboardWorkflowAPIEntry, error) {
	var out []dashboardWorkflowAPIEntry
	err := withStore(d.home, func(store *db.Store) error {
		entries, err := dashboardWorkflowEntries(ctx, store, time.Now().UTC())
		if err != nil {
			return err
		}
		out = entries
		return nil
	})
	return out, err
}

func (d *webDataSource) Workflows(ctx context.Context) ([]dashboard.WorkflowIndexEntry, error) {
	out := []dashboard.WorkflowIndexEntry{}
	now := time.Now().UTC()
	err := withStore(d.home, func(store *db.Store) error {
		entries, err := dashboardWorkflowEntries(ctx, store, now)
		if err != nil {
			return err
		}
		out = make([]dashboard.WorkflowIndexEntry, 0, len(entries))
		for _, entry := range entries {
			out = append(out, entry.WorkflowIndexEntry)
		}
		return err
	})
	return out, err
}

// dashboardWorkflowAPIEntry is the server-owned extension of the pinned
// dashboard module's index contract. Keeping the widening here avoids changing
// or vendoring the separate frontend repository while exposing the new wire
// fields immediately.
type dashboardWorkflowAPIEntry struct {
	dashboard.WorkflowIndexEntry
	Description string `json:"description"`
	Status      string `json:"status"`
}

func dashboardWorkflowEntries(ctx context.Context, store *db.Store, now time.Time) ([]dashboardWorkflowAPIEntry, error) {
	summaries, err := store.ListWorkflowSummaries(ctx)
	if err != nil {
		return nil, err
	}
	metaByWorkflow, err := store.ListWorkflowMeta(ctx)
	if err != nil {
		return nil, err
	}
	reposByWorkflow, err := store.ListWorkflowRepos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]dashboardWorkflowAPIEntry, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, dashboardWorkflowEntry(now, summary, metaByWorkflow[summary.WorkflowID], reposByWorkflow[summary.WorkflowID]))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return dashboardWorkflowIndexLess(out[i].WorkflowIndexEntry, out[j].WorkflowIndexEntry)
	})
	return out, nil
}

func dashboardWorkflowEntry(now time.Time, summary db.WorkflowSummary, meta db.WorkflowMeta, repos []string) dashboardWorkflowAPIEntry {
	lastAt := parseJobTimeMillis(summary.LastAt)
	state, stalledFor := deriveDashboardWorkflowState(now, dashboardWorkflowActivity{
		Queued: summary.Queued, Running: summary.Running, Failed: summary.Failed,
		Blocked: summary.Blocked, LastActivity: workflowMillisTime(lastAt),
		LastFailure:   workflowMillisTime(parseJobTimeMillis(summary.LastFailureAt)),
		LastHumanNote: workflowMillisTime(parseJobTimeMillis(summary.LastHumanNoteAt)),
	})
	author := strings.TrimSpace(meta.Author)
	if author == "" {
		author = strings.TrimSpace(summary.LastHumanAuthor)
	}
	namespace, campaign := splitDashboardWorkflowLabel(summary.WorkflowID)
	description := workflowDisplayDescription(summary.WorkflowID, meta.Description)
	return dashboardWorkflowAPIEntry{
		Description: description,
		Status:      meta.Status,
		WorkflowIndexEntry: dashboard.WorkflowIndexEntry{
			Label: summary.WorkflowID, Namespace: namespace, Campaign: campaign, Auto: false,
			Summary: firstNonEmpty(meta.Summary, description),
			Coordinator: dashboard.WorkflowCoordinator{
				Author: author, Pane: strings.TrimSpace(meta.Pane), SessionID: strings.TrimSpace(meta.SessionID),
			},
			State: state, StalledForS: stalledFor,
			Counts: dashboard.WorkflowCounts{
				Jobs: summary.JobCount, Running: summary.Running, Queued: summary.Queued,
				Succeeded: summary.Succeeded, Failed: summary.Failed, Blocked: summary.Blocked,
				Notes: summary.NoteCount,
			},
			TokensIn: summary.InputTokens, TokensOut: summary.OutputTokens,
			FirstAt: parseJobTimeMillis(summary.FirstAt), LastAt: lastAt,
			LastNote: dashboardWorkflowOneLine(summary.LastNote),
			Repos:    append([]string(nil), repos...),
		},
	}
}

func dashboardWorkflowIndexLess(a, b dashboard.WorkflowIndexEntry) bool {
	rank := func(state string) int {
		switch state {
		case "stalled":
			return 0
		case "active":
			return 1
		case "recent":
			return 2
		case "settled":
			return 3
		default:
			return 4
		}
	}
	if ar, br := rank(a.State), rank(b.State); ar != br {
		return ar < br
	}
	if a.State == "stalled" && a.StalledForS != b.StalledForS {
		return a.StalledForS > b.StalledForS
	}
	if a.LastAt != b.LastAt {
		return a.LastAt > b.LastAt
	}
	return a.Label < b.Label
}

func splitDashboardWorkflowLabel(label string) (namespace, campaign string) {
	if left, right, ok := strings.Cut(label, "/"); ok {
		return left, right
	}
	return "", label
}

func dashboardWorkflowOneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func workflowMillisTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}
