package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// clusterEdgeCap bounds the number of fact->cluster-hub edges the brain graph
// emits. There is one cluster edge per clustered fact, so the count grows with the
// fact pool; it is capped to keep the payload (and the client force-graph) bounded
// in a large unattended deployment. The truncation is deterministic: facts are
// walked in their stable sorted order and cluster edges stop being appended once
// the cap is reached.
const clusterEdgeCap = 2000

// unclusteredHubID is the stable hub node id for facts in the reserved
// 'unclustered' bucket (cluster 0). Namespaced under "cluster:" so the UI groups
// them alongside real cluster hubs without colliding with a fact or agent id.
const unclusteredHubID = "cluster:unclustered"

// witnessKey identifies a fact's observation pool for the witness tally: the
// owning agent ref, the repo (as stored, "" == general/NULL), and the memory key.
type witnessKey struct {
	owner string
	repo  string
	key   string
}

// Knowledge returns the memory brain graph behind the Learning page's Knowledge
// view: the memory-enrolled agents, their confirmed facts (each carrying the #763
// detail fields — owning cluster, source-job/source-file provenance and the vault
// [[wikilink]] cross-references), the emergent clusters those facts belong to, and
// the owner/cluster/repo/supersede edges between them. It is a read-only pass over
// the enrolled config set (config.LoadAgentTypes) plus the confirmed_memories /
// memory_observations / memory_clusters tables. Clusters are additive: an
// un-recomputed store returns an empty Clusters slice and empty per-fact Cluster
// fields, and the client falls back to its pre-cluster scope/category view.
//
// Deliberate count divergence: a KnowledgeAgent's Facts count is the INJECTABLE set
// (CountConfirmedMemoriesForOwner, which excludes superseded rows), but the Facts
// slice INCLUDES superseded rows flagged Superseded==true so the graph can draw the
// supersede "ghosts". The per-agent count and the on-graph fact-node count for that
// agent therefore differ by its superseded rows — this is intentional.
func (d *webDataSource) Knowledge(ctx context.Context) (dashboard.Knowledge, error) {
	out, _, err := d.knowledgeWithShared(ctx)
	return out, err
}

func (d *webDataSource) knowledgeWithShared(ctx context.Context) (dashboard.Knowledge, map[string]bool, error) {
	out := dashboard.Knowledge{
		Agents:   []dashboard.KnowledgeAgent{},
		Facts:    []dashboard.KnowledgeFact{},
		Clusters: []dashboard.KnowledgeCluster{},
		Edges:    []dashboard.KnowledgeEdge{},
	}
	sharedByFact := map[string]bool{}
	err := withStoreAndPaths(d.home, func(paths config.Paths, store *db.Store) error {
		// Confirmed facts (INCLUDING superseded ghosts) owned by any agent, plus
		// shared facts attributed to their preserved author.
		rows, err := store.ListConfirmedMemoriesForKnowledge(ctx)
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

		// rowid -> stable fact id, built up front so the per-fact [[wikilink]] set can
		// filter to the emitted fact pool (no dangling cross-references) and the
		// supersede edges can resolve.
		idByRow := make(map[int64]string, len(rows))
		for _, r := range rows {
			idByRow[r.ID] = fmt.Sprintf("fact:%d", r.ID)
		}

		// Persisted emergent-cluster membership (rowid -> cluster id) and the cluster
		// rows themselves (label/medoid), retiring the old key-prefix category hack
		// (#763). Both are fail-open: a query error (or an un-recomputed store) leaves
		// the maps empty, so per-fact Cluster fields and the Clusters slice stay empty
		// and the client falls back to its pre-cluster scope/category view.
		membByRow := map[int64]int64{}
		if members, merr := store.ListMemoryClusterMembers(ctx); merr == nil {
			for _, m := range members {
				membByRow[m.MemoryID] = m.ClusterID
			}
		}
		clusterByID := map[int64]db.MemoryCluster{}
		if clusters, cerr := store.ListMemoryClusters(ctx); cerr == nil {
			for _, c := range clusters {
				clusterByID[c.ClusterID] = c
			}
		}

		// Facts, with the Knowledge graph v2 detail fields (#763): the owning cluster
		// hub, provenance (source job xor source file), and the vault [[wikilink]]
		// cross-references. All are additive — an un-recomputed store leaves Cluster
		// empty and file-less provenance leaves SourceFile empty.
		out.Facts = make([]dashboard.KnowledgeFact, 0, len(rows))
		for _, r := range rows {
			owner := knowledgeFactOwner(r)
			fact := dashboard.KnowledgeFact{
				ID:         idByRow[r.ID],
				Content:    r.Content,
				Repo:       strings.TrimSpace(r.Repo),
				Key:        strings.TrimSpace(r.Key),
				Owner:      owner,
				Witnesses:  witnessByKey[witnessKey{owner, r.Repo, r.Key}],
				FirstSeen:  parseJobTimeMillis(r.FirstConfirmedAt),
				LastSeen:   parseJobTimeMillis(r.UpdatedAt),
				Superseded: r.SupersededBy != 0,
				Links:      knowledgeFactLinks(ctx, store, r, idByRow),
			}
			if r.Owner.Kind == memory.OwnerKindShared && r.Owner.Ref == memory.SharedOwnerRef {
				sharedByFact[fact.ID] = true
			}
			// Cluster: only when the membership resolves to a known cluster row, so a
			// fact's Cluster never dangles past the emitted Clusters slice.
			if cid, ok := membByRow[r.ID]; ok {
				if _, known := clusterByID[cid]; known {
					fact.Cluster = clusterHubID(cid)
				}
			}
			// Provenance: the job id wins; a file-shaped provenance backs SourceFile
			// only when there is no job, so a fact never carries both (the client's
			// "one of source job / source file, never both" contract).
			if job := strings.TrimSpace(r.SourceJob); job != "" {
				fact.SourceJob = job
			} else if file := factSourceFile(r.Provenance); file != "" {
				fact.SourceFile = file
			}
			out.Facts = append(out.Facts, fact)
		}
		// Newest-first by FirstSeen, ID tie-break — mirrors the fake feed's stable order.
		sort.SliceStable(out.Facts, func(i, j int) bool {
			if out.Facts[i].FirstSeen != out.Facts[j].FirstSeen {
				return out.Facts[i].FirstSeen > out.Facts[j].FirstSeen
			}
			return out.Facts[i].ID < out.Facts[j].ID
		})

		out.Clusters = knowledgeClusters(clusterByID, out.Facts, idByRow)
		out.Agents = knowledgeAgents(ctx, store, paths, out.Facts)
		out.Edges = knowledgeEdges(rows, out.Facts, idByRow, membByRow)
		return nil
	})
	if err != nil {
		return dashboard.Knowledge{}, nil, err
	}
	return out, sharedByFact, nil
}

func knowledgeFactOwner(r db.ConfirmedMemory) string {
	if author := strings.TrimSpace(r.AuthorRef); author != "" {
		return author
	}
	return strings.TrimSpace(r.Owner.Ref)
}

type dashboardKnowledgeResponse struct {
	Agents   []dashboard.KnowledgeAgent   `json:"agents"`
	Facts    []dashboardKnowledgeFact     `json:"facts"`
	Clusters []dashboard.KnowledgeCluster `json:"clusters"`
	Edges    []dashboard.KnowledgeEdge    `json:"edges"`
}

type dashboardKnowledgeFact struct {
	dashboard.KnowledgeFact
	Shared bool `json:"shared,omitempty"`
}

func (d *webDataSource) handleLearningKnowledge(w http.ResponseWriter, r *http.Request) {
	k, sharedByFact, err := d.knowledgeWithShared(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := dashboardKnowledgeResponse{
		Agents:   k.Agents,
		Facts:    make([]dashboardKnowledgeFact, 0, len(k.Facts)),
		Clusters: k.Clusters,
		Edges:    k.Edges,
	}
	for _, f := range k.Facts {
		resp.Facts = append(resp.Facts, dashboardKnowledgeFact{
			KnowledgeFact: f,
			Shared:        sharedByFact[f.ID],
		})
	}
	buf, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	buf = append(buf, '\n')
	_, _ = w.Write(buf)
}

// factSourceFileMarkers are the Provenance prefixes the file-ingest write paths
// stamp a source file behind: `memory ingest` writes "ingest:<relpath>" and the
// vault import writes "vault-import:<file>" (internal/cli/memory_ingest.go,
// memory_vault_import.go). A confirmed fact carries its source observation's
// provenance verbatim, so these mirror those write sites. Job- or confirm-shaped
// provenance ("distill:<job>", "confirm", …) carries no file.
var factSourceFileMarkers = []string{"ingest:", "vault-import:"}

// factSourceFile extracts a file-shaped provenance (the file a fact was ingested
// from) from a confirmed row's Provenance column, or "" when the provenance is not
// file-shaped. It backs KnowledgeFact.SourceFile; the caller only consults it when
// SourceJob is empty so a fact never carries both.
func factSourceFile(provenance string) string {
	provenance = strings.TrimSpace(provenance)
	for _, marker := range factSourceFileMarkers {
		if strings.HasPrefix(provenance, marker) {
			if f := strings.TrimSpace(strings.TrimPrefix(provenance, marker)); f != "" {
				return f
			}
		}
	}
	return ""
}

// knowledgeFactLinks returns the vault [[wikilink]] fact ids for one confirmed
// row, reusing the SAME deterministic top-K co-occurrence derivation the vault
// export renders (vaultLinksFor, capped at vaultLinkK=5). Targets are mapped to
// their stable "fact:<id>" ids and filtered to the emitted fact set (idByRow) so
// the detail panel never renders a dangling cross-reference; the result is sorted
// by target id for a stable, signature-skippable payload. Fail-open: a link-query
// error (or an empty match) yields no links rather than failing the endpoint.
func knowledgeFactLinks(ctx context.Context, store *db.Store, src db.ConfirmedMemory, idByRow map[int64]string) []string {
	links, err := vaultLinksFor(ctx, store, src)
	if err != nil || len(links) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(links))
	for _, l := range links {
		if _, ok := idByRow[l.TargetID]; ok {
			ids = append(ids, l.TargetID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fmt.Sprintf("fact:%d", id))
	}
	return out
}

// knowledgeClusters builds the emergent-cluster hubs behind the Knowledge view's
// repo->cluster->fact hierarchy (#763): one dashboard.KnowledgeCluster per
// persisted community that has at least one EMITTED member fact. Label is the
// display label — an owner `memory cluster rename` override wins, resolved by the
// store's DisplayLabel, so the client renders it verbatim. Count is the
// emitted-member tally (so it matches the members the client can list), Repo is the
// dominant repo scope among those members ("" = general/mixed) for the nesting, and
// Medoid is the anchor fact but only when it is itself an emitted member of this
// hub. Sorted by hub id for a stable, signature-skippable payload. Fail-open
// upstream: an empty cluster set yields an empty slice and the client falls back to
// its pre-cluster scope/category view.
func knowledgeClusters(clusterByID map[int64]db.MemoryCluster, facts []dashboard.KnowledgeFact, idByRow map[int64]string) []dashboard.KnowledgeCluster {
	// Roll up the emitted facts by their cluster hub: member count, per-repo tally,
	// and the fact->hub index used to confirm a medoid is a member of its own hub.
	countByHub := map[string]int{}
	reposByHub := map[string]map[string]int{}
	hubOfFact := map[string]string{}
	for _, f := range facts {
		if f.Cluster == "" {
			continue
		}
		countByHub[f.Cluster]++
		if reposByHub[f.Cluster] == nil {
			reposByHub[f.Cluster] = map[string]int{}
		}
		reposByHub[f.Cluster][f.Repo]++
		hubOfFact[f.ID] = f.Cluster
	}

	out := make([]dashboard.KnowledgeCluster, 0, len(clusterByID))
	for cid, c := range clusterByID {
		hub := clusterHubID(cid)
		count := countByHub[hub]
		if count == 0 {
			continue // no emitted member facts -> not a hub the client can render
		}
		kc := dashboard.KnowledgeCluster{
			ID:    hub,
			Label: c.DisplayLabel(),
			Count: count,
			Repo:  dominantRepo(reposByHub[hub]),
		}
		if c.MedoidID != 0 {
			if mid := fmt.Sprintf("fact:%d", c.MedoidID); hubOfFact[mid] == hub {
				kc.Medoid = mid
			}
		}
		out = append(out, kc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// dominantRepo returns the most common repo scope among a cluster's member facts
// for the repo->cluster nesting: the modal repo, ties broken by the
// lexically-smallest repo for determinism. An empty result means general/mixed
// scope (the modal member is general-scoped or a general-vs-repo tie).
func dominantRepo(repos map[string]int) string {
	best, bestN := "", -1
	for repo, n := range repos {
		if n > bestN || (n == bestN && repo < best) {
			best, bestN = repo, n
		}
	}
	return best
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

// knowledgeEdges builds the brain graph's edges: owner (fact->agent), cluster
// (fact->emergent-cluster hub), repo (cluster->repo hub, the top tier of the
// repo->cluster->fact hierarchy), and supersede (newer->older). The final set is
// sorted by (kind, source, target) exactly like the fake feed, so a
// signature-skip poll is stable.
//
// membByRow is the persisted emergent-cluster membership (confirmed_memories.id
// -> cluster id) from `memory clusters recompute`. It REPLACES the retired
// key-prefix category hack: hubs are now the deterministic communities over the
// fact-similarity graph (#763), not the leading colon-delimited key dimension.
func knowledgeEdges(rows []db.ConfirmedMemory, facts []dashboard.KnowledgeFact, idByRow map[int64]string, membByRow map[int64]int64) []dashboard.KnowledgeEdge {
	edges := make([]dashboard.KnowledgeEdge, 0, len(facts)*3+len(rows))

	// owner: every fact -> its owning agent hub.
	for _, f := range facts {
		if f.Owner == "" {
			continue
		}
		edges = append(edges, dashboard.KnowledgeEdge{Source: f.ID, Target: f.Owner, Kind: "owner"})
	}

	// cluster + repo tier: every clustered fact -> its emergent-cluster hub, and
	// each (cluster, repo) pair among the members -> the repo hub, forming the
	// repo->cluster->fact hierarchy. Membership is looked up by the fact's stored
	// rowid (idByRow maps rowid->"fact:<id>"). rowidByFactID inverts idByRow so we
	// can resolve a KnowledgeFact back to its rowid for the membership lookup.
	// Capped at clusterEdgeCap with a deterministic truncation (facts walked in
	// their already-sorted order). repoByCluster dedupes the cluster->repo edges.
	rowidByFactID := make(map[string]int64, len(idByRow))
	for rowid, fid := range idByRow {
		rowidByFactID[fid] = rowid
	}
	repoByCluster := map[string]struct{}{}
	clusterEdges := 0
	for _, f := range facts {
		if clusterEdges >= clusterEdgeCap {
			break
		}
		rowid, ok := rowidByFactID[f.ID]
		if !ok {
			continue
		}
		cid, ok := membByRow[rowid]
		if !ok {
			continue // fact not yet clustered (no recompute run, or attach pending)
		}
		hub := clusterHubID(cid)
		edges = append(edges, dashboard.KnowledgeEdge{Source: f.ID, Target: hub, Kind: "cluster"})
		clusterEdges++
		// repo tier: link this cluster to the repo the fact belongs to (general
		// scope -> the "general" repo hub) once per (cluster, repo).
		repoHub := knowledgeRepoHub(f.Repo)
		key := hub + "\x00" + repoHub
		if _, seen := repoByCluster[key]; !seen {
			repoByCluster[key] = struct{}{}
			edges = append(edges, dashboard.KnowledgeEdge{Source: hub, Target: repoHub, Kind: "repo"})
		}
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

// clusterHubID is the stable hub node id for an emergent cluster. It is
// namespaced "cluster:<id>" so it never collides with a fact id ("fact:<n>") or an
// agent name. The reserved unclustered bucket (id 0) maps to the fixed
// unclusteredHubID so those facts group together.
func clusterHubID(clusterID int64) string {
	if clusterID == memory.UnclusteredID {
		return unclusteredHubID
	}
	return "cluster:" + strconv.FormatInt(clusterID, 10)
}

// knowledgeRepoHub is the top-tier repo hub node id. A general-scope fact (empty
// repo) maps to the fixed "repo:general" hub. Namespaced "repo:" so it never
// collides with a fact, agent, or cluster id.
func knowledgeRepoHub(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "repo:general"
	}
	return "repo:" + repo
}
