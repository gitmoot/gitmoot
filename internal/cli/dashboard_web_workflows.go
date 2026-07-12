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
	// A recently touched workflow remains active long enough for coordinator
	// handoffs and short pauses between dispatches to read as one live campaign.
	dashboardWorkflowActiveWindow = 30 * time.Minute
	// A failed/blocked quiet workflow needs attention for one day; after that it
	// ages into settled history instead of pinning the operator's stalled list.
	dashboardWorkflowStalledHorizon = 24 * time.Hour
)

type dashboardWorkflowActivity struct {
	Queued, Running, Failed, Blocked int
	LastActivity                     time.Time
	// LastFailure/LastNote drive the acknowledgment rule: a failure is only an
	// alarm while no journal note has been written AFTER it ("a failure alone is
	// not an alarm — the silence after it is").
	LastFailure time.Time
	LastNote    time.Time
}

// deriveDashboardWorkflowState is the single lifecycle definition shared by
// workflow index and detail responses.
func deriveDashboardWorkflowState(now time.Time, activity dashboardWorkflowActivity) (state string, stalledForS int64) {
	age := now.Sub(activity.LastActivity)
	if activity.Queued > 0 || activity.Running > 0 || (!activity.LastActivity.IsZero() && age <= dashboardWorkflowActiveWindow) {
		return "active", 0
	}
	failureUnacknowledged := !activity.LastFailure.IsZero() &&
		(activity.LastNote.IsZero() || activity.LastNote.Before(activity.LastFailure))
	if (activity.Failed > 0 || activity.Blocked > 0) && failureUnacknowledged &&
		age > dashboardWorkflowActiveWindow && age < dashboardWorkflowStalledHorizon {
		return "stalled", max(0, int64(age/time.Second))
	}
	return "settled", 0
}

// Workflows returns one deterministic index row for every explicit workflow
// label known through indexed jobs or journal notes. Auto-synthesized pipeline
// and adhoc groups are intentionally absent in Wave 2: the current run-root
// index does not carry enough naming metadata to synthesize them without a
// global payload scan.
func (d *webDataSource) Workflows(ctx context.Context) ([]dashboard.WorkflowIndexEntry, error) {
	out := []dashboard.WorkflowIndexEntry{}
	now := time.Now().UTC()
	err := withStore(d.home, func(store *db.Store) error {
		summaries, err := store.ListWorkflowSummaries(ctx)
		if err != nil {
			return err
		}
		metaByWorkflow, err := store.ListWorkflowMeta(ctx)
		if err != nil {
			return err
		}
		reposByWorkflow, err := store.ListWorkflowRepos(ctx)
		if err != nil {
			return err
		}

		out = make([]dashboard.WorkflowIndexEntry, 0, len(summaries))
		for _, summary := range summaries {
			lastAt := parseJobTimeMillis(summary.LastAt)
			state, stalledFor := deriveDashboardWorkflowState(now, dashboardWorkflowActivity{
				Queued: summary.Queued, Running: summary.Running, Failed: summary.Failed,
				Blocked: summary.Blocked, LastActivity: workflowMillisTime(lastAt),
				LastFailure: workflowMillisTime(parseJobTimeMillis(summary.LastFailureAt)),
				LastNote:    workflowMillisTime(parseJobTimeMillis(summary.LastNoteAt)),
			})
			meta := metaByWorkflow[summary.WorkflowID]
			author := strings.TrimSpace(meta.Author)
			if author == "" {
				author = strings.TrimSpace(summary.LastAuthor)
			}
			namespace, campaign := splitDashboardWorkflowLabel(summary.WorkflowID)
			out = append(out, dashboard.WorkflowIndexEntry{
				Label: summary.WorkflowID, Namespace: namespace, Campaign: campaign,
				Auto: false,
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
				Repos:    append([]string(nil), reposByWorkflow[summary.WorkflowID]...),
			})
		}
		sort.SliceStable(out, func(i, j int) bool { return dashboardWorkflowIndexLess(out[i], out[j]) })
		return nil
	})
	return out, err
}

func dashboardWorkflowIndexLess(a, b dashboard.WorkflowIndexEntry) bool {
	rank := func(state string) int {
		switch state {
		case "stalled":
			return 0
		case "active":
			return 1
		case "settled":
			return 2
		default:
			return 3
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
