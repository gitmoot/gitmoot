package cli

import (
	"context"
	"sort"
	"strings"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/db"
)

// This file implements the Learning page's Skills DataSource method over the
// agent_templates / agent_template_versions / agents tables, which SURVIVE the
// #2201 scope narrowing: #2204 keeps the agent-template store and the read path
// that feeds reviewer prompts, so the template overview is still a real feature.
// It was carried over from the retired dashboard_web_learning.go, which also
// held Knowledge() (the memory brain graph); that half is gone with the memory
// tables and Knowledge() is an empty stub in dashboard_web.go.
//
// Skills reads through the same read-only store paths as the rest of
// dashboard_web.go (withStore, parseJobTimeMillis) and is deterministic: the
// Learning UI polls it with a change-signature skip, so the sort orders below
// must be stable across calls.

// Skills returns the agent-template overview behind the Learning page's Skills
// view: one SkillTemplate per registered agent template, each carrying its full
// version history (the sparkline) and the version it currently resolves to. It is
// a single read-only pass over ListAgentTemplates plus, per template,
// ListAgentTemplateVersions. It is fail-open per template: a version-list error
// degrades that one template (empty history) rather than failing the endpoint.
//
// #1752 removed the SkillOpt optimization loop, and with it the candidate/canary
// layer that used to populate per-version scores, in-flight canaries and pending
// candidates. Those DataSource fields are still part of the pinned dashboard
// module's contract, so they are serialized as their zero values: Pending is empty,
// ActiveCanaries and PendingTotal are 0. The version history itself is unaffected —
// it comes from the agent_templates/agent_template_versions store, which stays.
func (d *webDataSource) Skills(ctx context.Context) (dashboard.Skills, error) {
	out := dashboard.Skills{Templates: []dashboard.SkillTemplate{}}
	err := withStore(d.home, func(store *db.Store) error {
		templates, err := store.ListAgentTemplates(ctx)
		if err != nil {
			return err
		}
		agentsByTemplate := agentsByTemplateID(ctx, store)

		out.Templates = make([]dashboard.SkillTemplate, 0, len(templates))
		for _, tmpl := range templates {
			out.Templates = append(out.Templates, buildSkillTemplate(ctx, store, tmpl, agentsByTemplate[tmpl.ID]))
		}

		// Deterministic order: most-recently-promoted first (LastPromotedAt desc),
		// TemplateID tie-break. The pending-first tier is gone with the candidate
		// layer (#1752): Pending is always empty, so it can no longer discriminate.
		sort.SliceStable(out.Templates, func(i, j int) bool {
			if out.Templates[i].LastPromotedAt != out.Templates[j].LastPromotedAt {
				return out.Templates[i].LastPromotedAt > out.Templates[j].LastPromotedAt
			}
			return out.Templates[i].TemplateID < out.Templates[j].TemplateID
		})
		return nil
	})
	if err != nil {
		return dashboard.Skills{}, err
	}
	return out, nil
}

// buildSkillTemplate maps one store template plus its version history into a
// dashboard SkillTemplate. The current version/state come from ListAgentTemplates'
// LEFT JOIN on current_version_id (tmpl.VersionNumber/VersionState) — the SAME
// resolution Agent()'s Current marker uses, so the two views agree on which version
// a template "runs".
//
// The mapping itself lives in skillTemplateFromVersions so the version ordering is
// testable: this function's input always arrives pre-sorted from the store, which
// would otherwise mask the defensive sort entirely.
func buildSkillTemplate(ctx context.Context, store *db.Store, tmpl db.AgentTemplate, agents []string) dashboard.SkillTemplate {
	versions, err := store.ListAgentTemplateVersions(ctx, tmpl.ID)
	if err != nil {
		// Fail-open: a version-list error leaves this one template with no history
		// (its current version/state still resolved below) rather than failing the
		// whole endpoint.
		return skillTemplateFromVersions(tmpl, agents, nil)
	}
	return skillTemplateFromVersions(tmpl, agents, versions)
}

// skillTemplateFromVersions is the pure mapping from a template plus its version
// rows to the dashboard shape. It is separated from the store read so a test can
// hand it DESCENDING or shuffled versions: ListAgentTemplateVersions is
// ORDER BY version, so the defensive sort below is unobservable through the store
// path and a test that only went through it could not tell the sort from its
// absence.
func skillTemplateFromVersions(tmpl db.AgentTemplate, agents []string, versions []db.AgentTemplateVersion) dashboard.SkillTemplate {
	st := dashboard.SkillTemplate{
		TemplateID:     tmpl.ID,
		Name:           strings.TrimSpace(tmpl.Name),
		Agents:         agents,
		Versions:       []dashboard.SkillVersion{},
		CurrentVersion: tmpl.VersionNumber,
		CurrentState:   strings.TrimSpace(tmpl.VersionState),
		// Pending stays an initialized EMPTY slice even though #1752 removed the
		// candidate layer that filled it: it is still part of the pinned dashboard
		// module's response shape, and a nil slice marshals as JSON null, which
		// breaks a client that iterates it.
		Pending: []dashboard.SkillCandidate{},
	}

	for _, v := range versions {
		st.Versions = append(st.Versions, dashboard.SkillVersion{
			Number:     v.VersionNumber,
			State:      strings.TrimSpace(v.State),
			CreatedAt:  parseJobTimeMillis(v.CreatedAt),
			PromotedAt: parseJobTimeMillis(v.PromotedAt),
		})

		// LastPromotedAt is the most-recent promotion across the whole history (it
		// drives the template sort); a never-promoted version contributes 0.
		if pa := parseJobTimeMillis(v.PromotedAt); pa > st.LastPromotedAt {
			st.LastPromotedAt = pa
		}
	}

	// Versions ascending by Number (the sparkline order). The store already orders
	// by version, so this is defensive — it keeps the dashboard contract true if
	// either layer's ordering ever changes. TestSkillTemplateFromVersionsSortsAscending
	// feeds it descending input, so deleting this sort fails that test.
	sort.SliceStable(st.Versions, func(i, j int) bool {
		return st.Versions[i].Number < st.Versions[j].Number
	})
	return st
}

// agentsByTemplateID maps each base template id to the sorted names of the
// registered agents instantiated from it. An agent's TemplateID can carry an
// @version ref, so it is split down to the base id (SplitAgentTemplateReference)
// before grouping. Returns an empty map on a list error (fail-open — the Skills
// view just shows no agents-per-template).
func agentsByTemplateID(ctx context.Context, store *db.Store) map[string][]string {
	out := map[string][]string{}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return out
	}
	for _, a := range agents {
		tid, _ := db.SplitAgentTemplateReference(a.TemplateID)
		if tid = strings.TrimSpace(tid); tid == "" {
			continue
		}
		out[tid] = append(out[tid], a.Name)
	}
	for tid := range out {
		sort.Strings(out[tid])
	}
	return out
}
