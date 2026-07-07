package cli

import (
	"context"
	"sort"
	"strings"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// This file implements the three #528 DataSource methods that surface binary-check
// output and human gates where a human manages work — Attention (the fleet-wide
// "Needs a human" roll-up), JobChecks (a job's failed deterministic result checks
// plus the policy mode, #711), and BinaryVerdicts (a SkillOpt eval run's per-question
// binary verdicts, #714) — over the same read-only store paths the rest of
// dashboard_web.go uses (withStore / withStoreAndPaths). All three are deterministic:
// the UI polls them with a change-signature skip, so ordering must be stable across
// calls (the store queries already sort; the candidate list is sorted here).

// Attention returns the "Needs a human" view: every item across the fleet parked on
// an explicit human decision — blocked job gates (#693), pending synth-review
// approvals (skillopt_synth_items status pending_human_approval), and agent-template
// candidates awaiting promotion (versions in the "pending" state, the same set the
// Skills view surfaces as SkillCandidate). It is a single read-only pass; each list is
// deterministically ordered (gates oldest-first by insertion id, synth items
// newest-first, candidates by templateId then version number) and always non-nil.
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

		// --- pending synth approvals ---
		items, err := store.ListSynthReviewItems(ctx, db.SynthItemStatusPending)
		if err != nil {
			return err
		}
		for _, it := range items {
			out.SynthItems = append(out.SynthItems, dashboard.AttentionSynthItem{
				ID:          it.ID,
				TemplateID:  strings.TrimSpace(it.TemplateID),
				Repo:        strings.TrimSpace(it.Repo),
				Question:    it.Question,
				Gap:         it.Gap,
				WeakAgent:   strings.TrimSpace(it.WeakAgent),
				StrongAgent: strings.TrimSpace(it.StrongAgent),
				JudgeAgent:  strings.TrimSpace(it.JudgeAgent),
				CreatedAt:   pipelineTimeMillis(it.CreatedAt),
			})
		}

		// --- candidates awaiting promotion (pending template versions) ---
		out.Candidates = pendingTemplateCandidates(ctx, store)

		return nil
	})
	if err != nil {
		return dashboard.Attention{}, err
	}
	out.Total = len(out.Gates) + len(out.SynthItems) + len(out.Candidates)
	return out, nil
}

// pendingTemplateCandidates lists every agent-template version in the "pending" state
// (a candidate awaiting human promotion) across all templates, sorted by templateId
// then version number. Its Score passes through the candidate review's stored form,
// exactly as the Skills view's SkillCandidate does (reuses reviewScore). Fail-open per
// template: a version-list error skips that one template rather than failing the view.
func pendingTemplateCandidates(ctx context.Context, store *db.Store) []dashboard.AttentionCandidate {
	out := []dashboard.AttentionCandidate{}
	templates, err := store.ListAgentTemplates(ctx)
	if err != nil {
		return out
	}
	for _, tmpl := range templates {
		versions, verr := store.ListAgentTemplateVersions(ctx, tmpl.ID)
		if verr != nil {
			continue
		}
		for _, v := range versions {
			if strings.TrimSpace(v.State) != "pending" {
				continue
			}
			_, _, rawScore := reviewScore(ctx, store, v.ID)
			out = append(out, dashboard.AttentionCandidate{
				TemplateID: tmpl.ID,
				Name:       strings.TrimSpace(tmpl.Name),
				VersionID:  v.ID,
				Number:     v.VersionNumber,
				Score:      rawScore,
				CreatedAt:  parseJobTimeMillis(v.CreatedAt),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TemplateID != out[j].TemplateID {
			return out[i].TemplateID < out[j].TemplateID
		}
		return out[i].Number < out[j].Number
	})
	return out
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

// BinaryVerdicts returns the per-run SkillOpt binary-check breakdown (#714) for a
// skillopt eval run id: the verdicts ordered by (dimension, questionId) — the same
// order ListBinaryVerdicts reads — plus pass/fail headline counts. Pass mirrors
// Verdict == "yes". An unknown run is not an error: it returns zero counts and an
// empty (never nil) list.
func (d *webDataSource) BinaryVerdicts(ctx context.Context, runID string) (dashboard.BinaryVerdicts, error) {
	out := dashboard.BinaryVerdicts{RunID: runID, Verdicts: []dashboard.BinaryVerdict{}}
	err := withStore(d.home, func(store *db.Store) error {
		rows, err := store.ListBinaryVerdicts(ctx, runID)
		if err != nil {
			return err
		}
		for _, v := range rows {
			pass := strings.EqualFold(strings.TrimSpace(v.Verdict), "yes")
			out.Verdicts = append(out.Verdicts, dashboard.BinaryVerdict{
				QuestionID:  v.QuestionID,
				Dimension:   v.Dimension,
				Verdict:     v.Verdict,
				Pass:        pass,
				Explanation: v.Explanation,
				Weight:      v.QuestionWeight,
			})
			if pass {
				out.Passed++
			} else {
				out.Failed++
			}
		}
		return nil
	})
	if err != nil {
		return dashboard.BinaryVerdicts{}, err
	}
	return out, nil
}
