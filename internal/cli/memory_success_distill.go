package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

const (
	skillOptPromotionProvenancePrefix = "skillopt-promotion:"
	skillOptPromotionFallbackOwner    = "lead"
)

// stageSkillOptPromotionObservation stages the #781 SkillOpt-promotion success
// observation. It is config-gated, best-effort, pending-only, low-trust, and never
// writes confirmed memory directly.
func stageSkillOptPromotionObservation(ctx context.Context, store *db.Store, paths config.Paths, promoted db.AgentTemplateVersion) {
	if store == nil || strings.TrimSpace(promoted.ID) == "" {
		return
	}
	settings, err := config.LoadMemorySettings(paths)
	if err != nil || settings.Disabled || !settings.DistillSuccesses {
		return
	}
	ownerRef, repo := resolveSkillOptPromotionOwnerAndRepo(ctx, store, paths, promoted)
	owner := db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: ownerRef}
	scope := memory.ScopeRepo
	if strings.TrimSpace(repo) == "" {
		scope = memory.ScopeGeneral
	}
	content := skillOptPromotionObservationContent(ctx, store, promoted)
	if ok, _ := memory.PreFilter(content, scope); !ok {
		return
	}
	key := skillOptPromotionObservationKey(promoted)
	if n, err := store.CountMemoryObservationsForKey(ctx, owner, repo, key); err != nil || n > 0 {
		return
	}
	seen, err := store.ObservationDedupKeys(ctx, owner.Ref)
	if err != nil {
		return
	}
	dkey := db.MemoryDedupKey(scope, repo, memory.ContentHash(content))
	if _, dup := seen[dkey]; dup {
		return
	}
	_, _ = store.InsertMemoryObservation(ctx, db.MemoryObservation{
		Owner:      owner,
		Repo:       repo,
		Scope:      scope,
		Key:        key,
		Content:    content,
		Provenance: skillOptPromotionProvenancePrefix + promoted.ID,
		TrustMark:  memory.TrustLow,
	})
}

func stageSkillOptPromotionObservationForHome(ctx context.Context, store *db.Store, home string, promoted db.AgentTemplateVersion) {
	paths, err := memoryPathsForHome(home)
	if err != nil {
		return
	}
	stageSkillOptPromotionObservation(ctx, store, paths, promoted)
}

func resolveSkillOptPromotionOwnerAndRepo(ctx context.Context, store *db.Store, paths config.Paths, promoted db.AgentTemplateVersion) (string, string) {
	templateID := strings.TrimSpace(promoted.TemplateID)
	owner := skillOptPromotionFallbackOwner
	repo := skillOptPromotionRepo(promoted.SourceRepo)
	if agents, err := store.ListAgents(ctx); err == nil {
		for _, agent := range agents {
			if !skillOptTemplateRefMatches(agent.TemplateID, templateID) {
				continue
			}
			owner = strings.TrimSpace(agent.Name)
			if repo == "" {
				repo = skillOptPromotionRepo(agent.RepoScope)
			}
			return ownerOrFallback(owner), repo
		}
	}
	if types, err := config.LoadAgentTypes(paths); err == nil {
		names := make([]string, 0, len(types))
		for name := range types {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if skillOptTemplateRefMatches(types[name].Template, templateID) {
				return ownerOrFallback(name), repo
			}
		}
	}
	return owner, repo
}

func skillOptTemplateRefMatches(ref, templateID string) bool {
	id, _ := db.SplitAgentTemplateReference(ref)
	return strings.TrimSpace(id) != "" && strings.TrimSpace(id) == strings.TrimSpace(templateID)
}

func ownerOrFallback(owner string) string {
	if strings.TrimSpace(owner) == "" {
		return skillOptPromotionFallbackOwner
	}
	return strings.TrimSpace(owner)
}

func skillOptPromotionRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || !strings.Contains(repo, "/") || strings.ContainsAny(repo, " \t\n\r") {
		return ""
	}
	return repo
}

func skillOptPromotionObservationKey(promoted db.AgentTemplateVersion) string {
	hash := strings.TrimSpace(promoted.ContentHash)
	if hash == "" {
		sum := sha256.Sum256([]byte(promoted.Content + promoted.ID))
		hash = hex.EncodeToString(sum[:])
	}
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return fmt.Sprintf("skillopt:%s-promoted:%s", strings.TrimSpace(promoted.ID), hash)
}

func skillOptPromotionObservationContent(ctx context.Context, store *db.Store, promoted db.AgentTemplateVersion) string {
	base := "unknown base"
	var review db.AgentTemplateCandidateReview
	if r, err := store.GetAgentTemplateCandidateReview(ctx, promoted.ID); err == nil {
		review = r
		if strings.TrimSpace(r.BaseVersionID) != "" {
			base = strings.TrimSpace(r.BaseVersionID)
		}
	}
	parts := []string{
		fmt.Sprintf("SkillOpt promoted %s over %s for template %s.", promoted.ID, base, promoted.TemplateID),
	}
	evidence := skillOptPromotionEvidence(ctx, store, promoted.ID, review)
	if len(evidence) > 0 {
		parts = append(parts, "Evidence: "+strings.Join(evidence, "; ")+".")
	}
	if weaknesses := skillOptPromotionWeaknesses(review); weaknesses != "" {
		parts = append(parts, "Trained weaknesses: "+weaknesses+".")
	}
	return strings.Join(parts, " ")
}

func skillOptPromotionEvidence(ctx context.Context, store *db.Store, versionID string, review db.AgentTemplateCandidateReview) []string {
	var evidence []string
	if review.Score != nil {
		evidence = append(evidence, fmt.Sprintf("review score %.4g", *review.Score))
	}
	if runs, err := store.ListSkillOptGateRuns(ctx, versionID); err == nil && len(runs) > 0 {
		run := runs[0]
		for _, candidate := range runs {
			if candidate.Accepted {
				run = candidate
				break
			}
		}
		verdict := "rejected"
		if run.Accepted {
			verdict = "accepted"
		}
		evidence = append(evidence, fmt.Sprintf("replay gate %s with candidate mean %.4g over champion mean %.4g", verdict, run.CandidateMean, run.ChampionMean))
	}
	return evidence
}

func skillOptPromotionWeaknesses(review db.AgentTemplateCandidateReview) string {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range []string{review.SummaryMetadataJSON, review.EvalReportJSON} {
		var decoded any
		if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &decoded) != nil {
			continue
		}
		collectSkillOptWeaknesses(decoded, "", seen, &out)
		if len(out) >= 3 {
			break
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return strings.Join(out, "; ")
}

func collectSkillOptWeaknesses(v any, key string, seen map[string]struct{}, out *[]string) {
	if len(*out) >= 3 {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childKey := k
			if skillOptWeaknessKey(key) {
				childKey = key
			}
			collectSkillOptWeaknesses(x[k], childKey, seen, out)
			if len(*out) >= 3 {
				return
			}
		}
	case []any:
		for _, item := range x {
			collectSkillOptWeaknesses(item, key, seen, out)
			if len(*out) >= 3 {
				return
			}
		}
	case string:
		if !skillOptWeaknessKey(key) {
			return
		}
		s := strings.Join(strings.Fields(x), " ")
		if len(s) > 120 {
			s = strings.TrimSpace(s[:120])
		}
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		*out = append(*out, s)
	}
}

func skillOptWeaknessKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(k, "weak") ||
		strings.Contains(k, "required_improvements") ||
		strings.Contains(k, "rejected_traits") ||
		strings.Contains(k, "optimizer_hint")
}
