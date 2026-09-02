package cli

import (
	"context"
	"strings"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// This file implements the three #528 DataSource methods that surface human gates
// where a human manages work — Attention (the fleet-wide "Needs a human" roll-up),
// JobChecks (a job's failed deterministic result checks plus the policy mode, #711),
// and BinaryVerdicts (#714) — over the same read-only store paths the rest of
// dashboard_web.go uses (withStore / withStoreAndPaths). All three are deterministic:
// the UI polls them with a change-signature skip, so ordering must be stable across
// calls (the store queries already sort).
//
// #1752 removed the SkillOpt optimization loop, so two of Attention's three lists and
// all of BinaryVerdicts lost their backing tables (skillopt_synth_items,
// agent_template_candidate_reviews and the `pending` version state,
// skillopt_binary_verdicts). The methods and their response fields remain — they are
// the pinned dashboard module's DataSource contract — and serialize as empty,
// never-nil lists.

// Attention returns the "Needs a human" view: every item across the fleet parked on
// an explicit human decision. Since #1752 that is blocked job gates (#693) only,
// ordered oldest-first by insertion id; SynthItems and Candidates are always empty.
func (d *webDataSource) Attention(ctx context.Context) (dashboard.Attention, error) {
	out := dashboard.Attention{
		Gates:      []dashboard.AttentionGate{},
		SynthItems: []dashboard.AttentionSynthItem{},
		Candidates: []dashboard.AttentionCandidate{},
	}
	err := withStore(d.home, func(store *db.Store) error {
		// --- blocked job gates (#693) ---
		gates, err := store.ListOpenJobGates(ctx)
		if err != nil {
			return err
		}
		// Enrich each gate with its job's title/agent/repo/PR/state via one ListJobs
		// pass.
		var jobByID map[string]db.Job
		if len(gates) > 0 {
			jobs, jerr := store.ListJobs(ctx)
			if jerr != nil {
				return jerr
			}
			jobByID = make(map[string]db.Job, len(jobs))
			for _, j := range jobs {
				jobByID[j.ID] = j
			}
		}
		for _, g := range gates {
			// Only surface a gate whose job is actually parked on a human decision
			// (#528 review): ListOpenJobGates returns every unsatisfied gate row, but
			// CancelJob (job_recovery.go) and the blocked-TTL sweep move a job out of
			// blocked WITHOUT clearing its gates, so an abandoned (cancelled) — or
			// retried, now queued/running — job would otherwise keep showing up as
			// "Needs a human" forever. A gate whose job row is missing is likewise not
			// actionable, so skip it too.
			job, ok := jobByID[g.JobID]
			if !ok || strings.TrimSpace(job.State) != string(workflow.JobBlocked) {
				continue
			}
			payload, _ := workflow.ParseJobPayload(job.Payload)
			out.Gates = append(out.Gates, dashboard.AttentionGate{
				JobID:     g.JobID,
				Need:      g.Need,
				CreatedAt: parseJobTimeMillis(g.CreatedAt),
				Title:     jobTitle(payload, job),
				Agent:     strings.TrimSpace(job.Agent),
				Repo:      strings.TrimSpace(payload.Repo),
				PR:        payload.PullRequest,
				State:     mapNodeState(job.State),
			})
		}

		// The synth-review queue and the pending-candidate queue were both owned by
		// the SkillOpt loop (#1752) and no longer exist. Nothing else parks work on a
		// human decision, so Gates is the whole view.

		return nil
	})
	if err != nil {
		return dashboard.Attention{}, err
	}
	out.Total = len(out.Gates)
	return out, nil
}

// JobChecks returns the job-detail failed-check section (#711): the deterministic
// result checks a job's result failed (question + explanation) in insertion order,
// plus the home-wide [workflow] result_checks policy mode in force ("off" | "warn" |
// "block"). An unknown job is not an error — it returns the resolved Mode with an
// empty Failed list. Mode resolution is fail-open to the documented default (warn).
func (d *webDataSource) JobChecks(ctx context.Context, jobID string) (dashboard.JobChecks, error) {
	out := dashboard.JobChecks{JobID: jobID, Failed: []dashboard.ResultCheck{}}
	err := withStoreAndPaths(d.home, func(paths config.Paths, store *db.Store) error {
		mode, merr := config.LoadResultChecksMode(paths)
		if merr != nil {
			// Fail-open: a malformed knob never fails the endpoint — report the default.
			mode = config.DefaultResultChecksMode
		}
		out.Mode = string(mode)

		rows, err := store.ListResultCheckFailures(ctx, jobID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			out.Failed = append(out.Failed, dashboard.ResultCheck{
				CheckID:     r.CheckID,
				Question:    r.Question,
				Explanation: r.Explanation,
			})
		}
		return nil
	})
	if err != nil {
		return dashboard.JobChecks{}, err
	}
	return out, nil
}

// BinaryVerdicts returns the per-run binary-check breakdown (#714). The
// skillopt_binary_verdicts table it read was dropped with the SkillOpt loop (#1752),
// so every run now resolves to zero counts and an empty (never nil) list. The method
// stays because it is part of the pinned dashboard module's DataSource interface.
func (d *webDataSource) BinaryVerdicts(ctx context.Context, runID string) (dashboard.BinaryVerdicts, error) {
	return dashboard.BinaryVerdicts{RunID: runID, Verdicts: []dashboard.BinaryVerdict{}}, nil
}
