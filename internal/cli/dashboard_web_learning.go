package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

// This file implements the two Learning-page DataSource methods — Skills (the
// SkillOpt evolution overview) and Knowledge (the memory brain graph) — over the
// same read-only store paths the rest of dashboard_web.go uses (withStore /
// withStoreAndPaths, parseJobTimeMillis, loadAgentTypesFailOpen). Both are
// deterministic: the Learning UI polls them with a change-signature skip, so the
// sort orders below must be stable across calls.

// Skills returns the SkillOpt evolution overview behind the Learning page's Skills
// view: one SkillTemplate per registered agent template, each carrying its full
// version history (the sparkline), the version it currently resolves to, any
// in-flight canary, and its pending candidates. It is a single read-only pass over
// ListAgentTemplates plus, per template, ListAgentTemplateVersions; each version's
// score is read best-effort from its candidate review. It is fail-open per
// template: a broken review or a version-list error degrades that one template
// (empty scores / history) rather than failing the endpoint.
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

		// Deterministic order: pending-first, then most-recently-promoted
		// (LastPromotedAt desc), TemplateID tie-break — mirrors the fake feed's sort.
		sort.SliceStable(out.Templates, func(i, j int) bool {
			pi, pj := len(out.Templates[i].Pending) > 0, len(out.Templates[j].Pending) > 0
			if pi != pj {
				return pi
			}
			if out.Templates[i].LastPromotedAt != out.Templates[j].LastPromotedAt {
				return out.Templates[i].LastPromotedAt > out.Templates[j].LastPromotedAt
			}
			return out.Templates[i].TemplateID < out.Templates[j].TemplateID
		})

		for i := range out.Templates {
			if out.Templates[i].CanarySample > 0 {
				out.ActiveCanaries++
			}
			out.PendingTotal += len(out.Templates[i].Pending)
		}
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
// a template "runs". Version scores are read best-effort from each version's
// candidate review; a broken/absent review simply leaves that version unscored.
func buildSkillTemplate(ctx context.Context, store *db.Store, tmpl db.AgentTemplate, agents []string) dashboard.SkillTemplate {
	st := dashboard.SkillTemplate{
		TemplateID:     tmpl.ID,
		Name:           strings.TrimSpace(tmpl.Name),
		Agents:         agents,
		Versions:       []dashboard.SkillVersion{},
		CurrentVersion: tmpl.VersionNumber,
		CurrentState:   strings.TrimSpace(tmpl.VersionState),
		Pending:        []dashboard.SkillCandidate{},
	}

	versions, err := store.ListAgentTemplateVersions(ctx, tmpl.ID)
	if err != nil {
		// Fail-open: a version-list error leaves this one template with no history
		// (its current version/state still resolved above) rather than failing the
		// whole endpoint.
		return st
	}

	for _, v := range versions {
		score, hasScore, rawScore := reviewScore(ctx, store, v.ID)
		state := strings.TrimSpace(v.State)

		st.Versions = append(st.Versions, dashboard.SkillVersion{
			Number:     v.VersionNumber,
			State:      state,
			Score:      score,
			HasScore:   hasScore,
			CreatedAt:  parseJobTimeMillis(v.CreatedAt),
			PromotedAt: parseJobTimeMillis(v.PromotedAt),
		})

		// LastPromotedAt is the most-recent promotion across the whole history (it
		// drives the template sort); a never-promoted version contributes 0.
		if pa := parseJobTimeMillis(v.PromotedAt); pa > st.LastPromotedAt {
			st.LastPromotedAt = pa
		}

		// Canary fields come from the single canary-state version (#484), when one is
		// in flight. At most one canary exists per template, so the last-writer here
		// is deterministic (there is only one).
		if state == "canary" && v.CanarySample > 0 {
			st.CanarySample = v.CanarySample
			st.CanaryStartedAt = parseJobTimeMillis(v.CanaryStartedAt)
		}

		// Pending candidates carry the review's raw score string passed through
		// verbatim (a decimal here, since the store's score column is REAL).
		if state == "pending" {
			st.Pending = append(st.Pending, dashboard.SkillCandidate{
				VersionID: v.ID,
				Number:    v.VersionNumber,
				Score:     rawScore,
				CreatedAt: parseJobTimeMillis(v.CreatedAt),
			})
		}
	}

	// Versions ascending by Number (the sparkline order); ListAgentTemplateVersions
	// already orders by version, but sort defensively so the contract holds even if
	// the store's order ever changes.
	sort.SliceStable(st.Versions, func(i, j int) bool {
		return st.Versions[i].Number < st.Versions[j].Number
	})
	sort.SliceStable(st.Pending, func(i, j int) bool {
		return st.Pending[i].Number < st.Pending[j].Number
	})
	return st
}

// reviewScore reads a template version's candidate-review score best-effort. The
// store column (agent_template_candidate_reviews.score) is a nullable REAL, so the
// review struct carries it as a *float64: a present score is parsed into the float
// (for SkillVersion.Score/HasScore) and its compact decimal string is passed
// through on SkillCandidate.Score. A missing review (sql.ErrNoRows) or ANY lookup
// error is swallowed — a broken review never errors the Skills endpoint (fail-open).
func reviewScore(ctx context.Context, store *db.Store, versionID string) (score float64, hasScore bool, raw string) {
	review, err := store.GetAgentTemplateCandidateReview(ctx, versionID)
	if err != nil || review.Score == nil {
		return 0, false, ""
	}
	return *review.Score, true, strconv.FormatFloat(*review.Score, 'f', -1, 64)
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

// categoryEdgeCap bounds the number of fact->category-hub edges the brain graph
// emits. There is one category edge per categorizable fact, so the count grows
// with the fact pool; it is capped to keep the payload (and the client
// force-graph) bounded in a large unattended deployment. The truncation is
// deterministic: facts are walked in their stable sorted order and category edges
// stop being appended once the cap is reached.
const categoryEdgeCap = 2000

// witnessKey identifies a fact's observation pool for the witness tally: the
// owning agent ref, the repo (as stored, "" == general/NULL), and the memory key.
type witnessKey struct {
	owner string
	repo  string
	key   string
}

// Knowledge returns the memory brain graph behind the Learning page's Knowledge
// view: the memory-enrolled agents, their confirmed facts, and the owner/category/
// supersede edges between them. It is a read-only pass over the enrolled config set
// (config.LoadAgentTypes) plus the confirmed_memories / memory_observations tables.
//
// Deliberate count divergence: a KnowledgeAgent's Facts count is the INJECTABLE set
// (CountConfirmedMemoriesForOwner, which excludes superseded rows), but the Facts
// slice INCLUDES superseded rows flagged Superseded==true so the graph can draw the
// supersede "ghosts". The per-agent count and the on-graph fact-node count for that
// agent therefore differ by its superseded rows — this is intentional.
func (d *webDataSource) Knowledge(ctx context.Context) (dashboard.Knowledge, error) {
	out := dashboard.Knowledge{
		Agents: []dashboard.KnowledgeAgent{},
		Facts:  []dashboard.KnowledgeFact{},
		Edges:  []dashboard.KnowledgeEdge{},
	}
	err := withStoreAndPaths(d.home, func(paths config.Paths, store *db.Store) error {
		// Confirmed facts (INCLUDING superseded ghosts) owned by any agent.
		rows, err := store.ListConfirmedMemoriesByOwnerKind(ctx, memory.OwnerKindAgent)
		if err != nil {
			return err
		}

		// Per-(owner,repo,key) witness tallies from the append-only observations, in
		// one grouped pass (no per-fact N+1). Fail-open: a query error leaves every
		// witness count at 0 rather than failing the endpoint.
		witnessByKey := map[witnessKey]int{}
		if ws, werr := store.CountObservationWitnessesByKey(ctx, memory.OwnerKindAgent); werr == nil {
			for _, w := range ws {
				witnessByKey[witnessKey{w.OwnerRef, w.Repo, w.Key}] = w.Count
			}
		}

		// Facts. rowid -> stable fact id, retained for supersede-edge resolution.
		idByRow := make(map[int64]string, len(rows))
		out.Facts = make([]dashboard.KnowledgeFact, 0, len(rows))
		for _, r := range rows {
			id := fmt.Sprintf("fact:%d", r.ID)
			idByRow[r.ID] = id
			out.Facts = append(out.Facts, dashboard.KnowledgeFact{
				ID:         id,
				Content:    r.Content,
				Repo:       strings.TrimSpace(r.Repo),
				Key:        strings.TrimSpace(r.Key),
				Owner:      strings.TrimSpace(r.Owner.Ref),
				Witnesses:  witnessByKey[witnessKey{r.Owner.Ref, r.Repo, r.Key}],
				FirstSeen:  parseJobTimeMillis(r.FirstConfirmedAt),
				LastSeen:   parseJobTimeMillis(r.UpdatedAt),
				Superseded: r.SupersededBy != 0,
			})
		}
		// Newest-first by FirstSeen, ID tie-break — mirrors the fake feed's stable order.
		sort.SliceStable(out.Facts, func(i, j int) bool {
			if out.Facts[i].FirstSeen != out.Facts[j].FirstSeen {
				return out.Facts[i].FirstSeen > out.Facts[j].FirstSeen
			}
			return out.Facts[i].ID < out.Facts[j].ID
		})

		out.Agents = knowledgeAgents(ctx, store, paths, out.Facts)
		out.Edges = knowledgeEdges(rows, out.Facts, idByRow)
		return nil
	})
	if err != nil {
		return dashboard.Knowledge{}, err
	}
	return out, nil
}

// knowledgeAgents returns the brain-graph's agent hubs: the memory-enrolled agents
// (config.LoadAgentTypes entry.Memory, fail-open to none) UNIONED with any agent
// that owns a confirmed fact, so every owner edge resolves to a listed node.
// Enrolled reflects the config flag; Facts/Observations reuse the #670 count
// helpers (Facts is the injectable count — superseded rows excluded — so it can be
// smaller than the agent's on-graph fact-node count). Sorted by name.
func knowledgeAgents(ctx context.Context, store *db.Store, paths config.Paths, facts []dashboard.KnowledgeFact) []dashboard.KnowledgeAgent {
	enrolled := map[string]bool{}
	for name, at := range loadAgentTypesFailOpen(paths) {
		if at.Memory {
			enrolled[name] = true
		}
	}
	names := map[string]bool{}
	for name := range enrolled {
		names[name] = true
	}
	for _, f := range facts {
		if f.Owner != "" {
			names[f.Owner] = true
		}
	}

	out := make([]dashboard.KnowledgeAgent, 0, len(names))
	for name := range names {
		ka := dashboard.KnowledgeAgent{Name: name, Enrolled: enrolled[name]}
		if n, cerr := store.CountConfirmedMemoriesForOwner(ctx, memory.OwnerKindAgent, name); cerr == nil {
			ka.Facts = n
		}
		if n, cerr := store.CountMemoryObservationsForOwner(ctx, memory.OwnerKindAgent, name); cerr == nil {
			ka.Observations = n
		}
		out = append(out, ka)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// knowledgeEdges builds the brain graph's edges from the facts (owner + category)
// and the raw confirmed rows (supersede). The final set is sorted by (kind,
// source, target) exactly like the fake feed, so a signature-skip poll is stable.
func knowledgeEdges(rows []db.ConfirmedMemory, facts []dashboard.KnowledgeFact, idByRow map[int64]string) []dashboard.KnowledgeEdge {
	edges := make([]dashboard.KnowledgeEdge, 0, len(facts)*2+len(rows))

	// owner: every fact -> its owning agent hub.
	for _, f := range facts {
		if f.Owner == "" {
			continue
		}
		edges = append(edges, dashboard.KnowledgeEdge{Source: f.ID, Target: f.Owner, Kind: "owner"})
	}

	// category: every categorizable fact -> its category hub, derived from the fact
	// key's LEADING colon-delimited dimension (the mechanical family the memory
	// writers key on, e.g. `outcome`/`fix-rounds`); facts sharing that dimension
	// share the hub. NOTE this derives the hub from the KEY per the design, which
	// differs from the fake feed's use of the fact's repo/scope as the hub — the
	// real key format carries the category dimension, the fake keys do not. Capped
	// at categoryEdgeCap with a deterministic truncation: facts are walked in their
	// already-sorted order and category edges stop once the cap is reached.
	categoryEdges := 0
	for _, f := range facts {
		if categoryEdges >= categoryEdgeCap {
			break
		}
		cat := knowledgeCategory(f.Key)
		if cat == "" {
			continue
		}
		edges = append(edges, dashboard.KnowledgeEdge{Source: f.ID, Target: cat, Kind: "category"})
		categoryEdges++
	}

	// supersede: the newer fact -> the older fact it replaced. confirmed_memories
	// links them via the OLDER row's superseded_by = <newer row id> (a real, scanned
	// column — verified linkable). NOTE: no production write path currently sets
	// superseded_by (UpsertConfirmedMemory updates the keyed row in place), so this
	// edge set is typically empty until a supersede write-path lands — but the
	// linkage exists, so the graph renders the ghost edges the moment it does.
	for _, r := range rows {
		if r.SupersededBy == 0 {
			continue
		}
		older, olderOK := idByRow[r.ID]
		newer, newerOK := idByRow[r.SupersededBy]
		if !olderOK || !newerOK {
			continue
		}
		edges = append(edges, dashboard.KnowledgeEdge{Source: newer, Target: older, Kind: "supersede"})
	}

	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		if edges[i].Source != edges[j].Source {
			return edges[i].Source < edges[j].Source
		}
		return edges[i].Target < edges[j].Target
	})
	return edges
}

// knowledgeCategory derives a fact's category-hub id from its memory key. The
// confirmed-memory writers key facts by a colon-delimited, closed-category form
// (`fix-rounds:<decision>`, `outcome:<action>:<decision>`), so the LEADING
// dimension is the category family. The hub id is namespaced `cat:<family>` so it
// never collides with a fact id ("fact:<n>") or an agent name. An empty/keyless
// fact yields "" (no category edge).
func knowledgeCategory(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	family := key
	if i := strings.Index(key, ":"); i >= 0 {
		family = strings.TrimSpace(key[:i])
	}
	if family == "" {
		return ""
	}
	return "cat:" + family
}
