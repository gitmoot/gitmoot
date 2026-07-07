package db

import (
	"context"
	"sort"
	"strings"
	"time"
)

// RoutingTelemetry is one execution-grounded routing observation (#530): a single
// row recorded best-effort at a job's terminal transition so Gitmoot can later
// compare how a (action, runtime, model, template) combination actually performed
// on real jobs. It is ADVISORY only — nothing in v1 reads it back to change
// routing behavior; the CLI summary and the optional coordinator context block are
// the only consumers. Every field is best-effort: a runtime that does not report
// tokens contributes 0, a job with no phase leaves Phase empty, and so on.
type RoutingTelemetry struct {
	ID    int64
	JobID string
	Repo  string
	// Action is the job type (ask/review/implement/continuation/…).
	Action string
	Phase  string
	// Runtime and Model are the effective runtime + model the job actually ran on.
	Runtime string
	Model   string
	Agent   string
	// TemplateID and TemplateCommit identify the resolved agent template snapshot.
	TemplateID     string
	TemplateCommit string
	// JobState is the terminal job state: succeeded | failed | blocked.
	JobState string
	// Decision is the agent result decision (approved/implemented/changes_requested/
	// blocked/failed) when the job carried a parseable result; empty otherwise.
	Decision string
	// Approved mirrors the engine's approving-outcome test (approved|implemented|
	// succeeded) so the summary can report an approval rate without re-deriving it.
	Approved bool
	// TestsRun is len(result.tests_run) — a coarse "did the agent exercise tests"
	// signal. It is NOT a pass/fail count (the agent reports the tests it ran, not
	// their verdicts); the job state/decision carry the real outcome.
	TestsRun     int
	DurationMS   int64
	InputTokens  int
	OutputTokens int
	CreatedAt    string
}

// RoutingTelemetryFilter narrows a ListRoutingTelemetry read. A zero value returns
// every row. Since, when non-zero, is a lower bound on created_at (inclusive).
type RoutingTelemetryFilter struct {
	Repo   string
	Action string
	Since  time.Time
}

// InsertRoutingTelemetry appends one advisory routing observation. It is called
// best-effort at the job terminal chokepoint; the caller swallows any error so a
// telemetry write can never fail a job.
func (s *Store) InsertRoutingTelemetry(ctx context.Context, t RoutingTelemetry) error {
	approved := 0
	if t.Approved {
		approved = 1
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO routing_telemetry(
	job_id, repo, action, phase, runtime, model, agent,
	template_id, template_commit, job_state, decision, approved,
	tests_run, duration_ms, input_tokens, output_tokens
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.JobID, t.Repo, t.Action, t.Phase, t.Runtime, t.Model, t.Agent,
		t.TemplateID, t.TemplateCommit, t.JobState, t.Decision, approved,
		t.TestsRun, t.DurationMS, t.InputTokens, t.OutputTokens)
	return err
}

// ListRoutingTelemetry returns the recorded observations matching filter, newest
// first. A missing routing_telemetry table (a store that predates the #530
// migration, e.g. a read-only open) is treated as "no observations" so a summary
// on an un-migrated DB reports empty rather than erroring.
func (s *Store) ListRoutingTelemetry(ctx context.Context, filter RoutingTelemetryFilter) ([]RoutingTelemetry, error) {
	query := `
SELECT id, job_id, repo, action, phase, runtime, model, agent,
	template_id, template_commit, job_state, decision, approved,
	tests_run, duration_ms, input_tokens, output_tokens, created_at
FROM routing_telemetry`
	var conds []string
	var args []any
	if repo := strings.TrimSpace(filter.Repo); repo != "" {
		conds = append(conds, "repo = ?")
		args = append(args, repo)
	}
	if action := strings.TrimSpace(filter.Action); action != "" {
		conds = append(conds, "action = ?")
		args = append(args, action)
	}
	if !filter.Since.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, filter.Since.UTC().Format("2006-01-02 15:04:05"))
	}
	if len(conds) > 0 {
		query += "\nWHERE " + strings.Join(conds, " AND ")
	}
	query += "\nORDER BY id DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		if isMissingRelationErr(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()

	var out []RoutingTelemetry
	for rows.Next() {
		var t RoutingTelemetry
		var approved int
		if err := rows.Scan(&t.ID, &t.JobID, &t.Repo, &t.Action, &t.Phase, &t.Runtime, &t.Model, &t.Agent,
			&t.TemplateID, &t.TemplateCommit, &t.JobState, &t.Decision, &approved,
			&t.TestsRun, &t.DurationMS, &t.InputTokens, &t.OutputTokens, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Approved = approved != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// isMissingRelationErr reports whether err is a SQLite "no such table"/"no such
// column" error, which for a read-only telemetry read means the #530 migration has
// not run on this DB yet (treat as empty, not fatal).
func isMissingRelationErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column")
}

// RoutingSummaryGroup is one aggregated (action, runtime, model, template) bucket
// of observed performance. It is a pure projection of a set of RoutingTelemetry
// rows — local observed performance, NOT a benchmark.
type RoutingSummaryGroup struct {
	Action           string  `json:"action"`
	Runtime          string  `json:"runtime"`
	Model            string  `json:"model"`
	TemplateID       string  `json:"template_id"`
	Count            int     `json:"count"`
	SuccessCount     int     `json:"success_count"`
	ApprovedCount    int     `json:"approved_count"`
	BlockedCount     int     `json:"blocked_count"`
	FailedCount      int     `json:"failed_count"`
	SuccessRate      float64 `json:"success_rate"`
	ApprovalRate     float64 `json:"approval_rate"`
	MedianDurationMS int64   `json:"median_duration_ms"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
}

// AggregateRoutingTelemetry groups rows by (action, runtime, model, template_id)
// and computes count, success/approval rates, blocked/failed counts, median
// duration, and summed tokens per group. It is a pure function (no I/O) so both
// the CLI summary and the coordinator context block share one definition. Groups
// are returned deterministically: most observations first, ties broken by the key
// fields, so seeded-row tests and prompt-injection tests are stable.
func AggregateRoutingTelemetry(rows []RoutingTelemetry) []RoutingSummaryGroup {
	type acc struct {
		group     RoutingSummaryGroup
		durations []int64
	}
	buckets := map[string]*acc{}
	var order []string
	for _, r := range rows {
		key := r.Action + "\x00" + r.Runtime + "\x00" + r.Model + "\x00" + r.TemplateID
		a := buckets[key]
		if a == nil {
			a = &acc{group: RoutingSummaryGroup{
				Action: r.Action, Runtime: r.Runtime, Model: r.Model, TemplateID: r.TemplateID,
			}}
			buckets[key] = a
			order = append(order, key)
		}
		a.group.Count++
		switch strings.ToLower(strings.TrimSpace(r.JobState)) {
		case "succeeded":
			a.group.SuccessCount++
		case "blocked":
			a.group.BlockedCount++
		case "failed":
			a.group.FailedCount++
		}
		if r.Approved {
			a.group.ApprovedCount++
		}
		a.group.InputTokens += r.InputTokens
		a.group.OutputTokens += r.OutputTokens
		a.durations = append(a.durations, r.DurationMS)
	}

	groups := make([]RoutingSummaryGroup, 0, len(order))
	for _, key := range order {
		a := buckets[key]
		if a.group.Count > 0 {
			a.group.SuccessRate = float64(a.group.SuccessCount) / float64(a.group.Count)
			a.group.ApprovalRate = float64(a.group.ApprovedCount) / float64(a.group.Count)
		}
		a.group.MedianDurationMS = medianInt64(a.durations)
		groups = append(groups, a.group)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		if groups[i].Action != groups[j].Action {
			return groups[i].Action < groups[j].Action
		}
		if groups[i].Runtime != groups[j].Runtime {
			return groups[i].Runtime < groups[j].Runtime
		}
		if groups[i].Model != groups[j].Model {
			return groups[i].Model < groups[j].Model
		}
		return groups[i].TemplateID < groups[j].TemplateID
	})
	return groups
}

func medianInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
