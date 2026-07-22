package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) UpsertAgentTemplate(ctx context.Context, template AgentTemplate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	contentHash := templateContentHash(template.Content)
	current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, template.ID)
	if err != nil {
		return err
	}
	versionID := current.ID
	versionNumber := current.VersionNumber
	if !hasCurrent || current.ContentHash != contentHash {
		versionNumber, err = nextAgentTemplateVersionNumber(ctx, tx, template.ID)
		if err != nil {
			return err
		}
		versionID = agentTemplateVersionID(template.ID, versionNumber)
		if hasCurrent {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'superseded', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = 'current'`, current.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_template_versions(id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, promoted_at, updated_at)
			VALUES (?, ?, ?, 'current', ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			versionID, template.ID, versionNumber, template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, contentHash, template.Content, template.MetadataJSON); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions
			SET state = 'current',
				name = ?,
				description = ?,
				source_repo = ?,
				source_ref = ?,
				source_path = ?,
				resolved_commit = ?,
				content_hash = ?,
				content = ?,
				metadata_json = ?,
				updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`,
			template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, contentHash, template.Content, template.MetadataJSON, current.ID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_templates(id, name, description, source_repo, source_ref, source_path, resolved_commit, content, metadata_json, current_version_id, latest_version_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			source_repo = excluded.source_repo,
			source_ref = excluded.source_ref,
			source_path = excluded.source_path,
			resolved_commit = excluded.resolved_commit,
			content = excluded.content,
			metadata_json = excluded.metadata_json,
			current_version_id = excluded.current_version_id,
			latest_version_id = CASE
				WHEN agent_templates.current_version_id = excluded.current_version_id AND agent_templates.latest_version_id <> '' THEN agent_templates.latest_version_id
				ELSE excluded.latest_version_id
			END,
			updated_at = CURRENT_TIMESTAMP`,
		template.ID, template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, template.Content, template.MetadataJSON, versionID, versionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetAgentTemplate(ctx context.Context, id string) (AgentTemplate, error) {
	row := s.db.QueryRowContext(ctx, `SELECT t.id, t.name, t.description, t.source_repo, t.source_ref, t.source_path, t.resolved_commit, t.content, t.metadata_json,
			COALESCE(v.id, ''), COALESCE(v.version, 0), COALESCE(v.state, ''), COALESCE(NULLIF(v.content_hash, ''), ''), t.created_at, t.updated_at
		FROM agent_templates t
		LEFT JOIN agent_template_versions v ON v.id = t.current_version_id
		WHERE t.id = ?`, id)
	return scanAgentTemplateWithVersion(row)
}

func (s *Store) ListAgentTemplates(ctx context.Context) ([]AgentTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.name, t.description, t.source_repo, t.source_ref, t.source_path, t.resolved_commit, t.content, t.metadata_json,
			COALESCE(v.id, ''), COALESCE(v.version, 0), COALESCE(v.state, ''), COALESCE(NULLIF(v.content_hash, ''), ''), t.created_at, t.updated_at
		FROM agent_templates t
		LEFT JOIN agent_template_versions v ON v.id = t.current_version_id
		ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	templates := []AgentTemplate{}
	for rows.Next() {
		template, err := scanAgentTemplateWithVersion(rows)
		if err != nil {
			return nil, err
		}
		templates = append(templates, template)
	}
	return templates, rows.Err()
}

func (s *Store) GetAgentTemplateReference(ctx context.Context, ref string) (AgentTemplate, error) {
	templateID, versionRef := SplitAgentTemplateReference(ref)
	if versionRef == "" || versionRef == "current" {
		return s.GetAgentTemplate(ctx, templateID)
	}
	if versionRef == "latest" {
		return s.GetLatestAgentTemplateVersion(ctx, templateID)
	}
	version, err := s.GetAgentTemplateVersion(ctx, templateID, versionRef)
	if err != nil {
		return AgentTemplate{}, err
	}
	return agentTemplateFromVersion(version), nil
}

func (s *Store) GetLatestAgentTemplateVersion(ctx context.Context, templateID string) (AgentTemplate, error) {
	row := s.db.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at, v.canary_sample, v.canary_started_at
		FROM agent_templates t
		JOIN agent_template_versions v ON v.id = t.latest_version_id
		WHERE t.id = ?`, strings.TrimSpace(templateID))
	version, err := scanAgentTemplateVersion(row)
	if err != nil {
		return AgentTemplate{}, err
	}
	return agentTemplateFromVersion(version), nil
}

func (s *Store) GetAgentTemplateVersion(ctx context.Context, templateID string, versionRef string) (AgentTemplateVersion, error) {
	templateID = strings.TrimSpace(templateID)
	versionRef = strings.TrimSpace(versionRef)
	if strings.HasPrefix(versionRef, "v") && len(versionRef) > 1 {
		versionRef = versionRef[1:]
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions
		WHERE template_id = ? AND (id = ? OR CAST(version AS TEXT) = ?)`, templateID, templateID+"@v"+versionRef, versionRef)
	return scanAgentTemplateVersion(row)
}

func (s *Store) GetAgentTemplateVersionByID(ctx context.Context, versionID string) (AgentTemplateVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions WHERE id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateVersion(row)
}

func (s *Store) ListAgentTemplateVersions(ctx context.Context, templateID string) ([]AgentTemplateVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions WHERE template_id = ? ORDER BY version`, templateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []AgentTemplateVersion{}
	for rows.Next() {
		version, err := scanAgentTemplateVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

func (s *Store) ListPendingAgentTemplateVersions(ctx context.Context, templateID string) ([]AgentTemplateVersion, error) {
	templateID = strings.TrimSpace(templateID)
	query := `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions WHERE state = 'pending'`
	args := []any{}
	if templateID != "" {
		query += ` AND template_id = ?`
		args = append(args, templateID)
	}
	query += ` ORDER BY template_id, version`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []AgentTemplateVersion{}
	for rows.Next() {
		version, err := scanAgentTemplateVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

func (s *Store) AddPendingAgentTemplateVersion(ctx context.Context, template AgentTemplate) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	_, versionNumber, err := addPendingAgentTemplateVersionTx(ctx, tx, template)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersion(ctx, template.ID, fmt.Sprintf("v%d", versionNumber))
}

func (s *Store) AddPendingAgentTemplateCandidate(ctx context.Context, template AgentTemplate, review AgentTemplateCandidateReview, artifacts []EvalArtifact) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	versionID, versionNumber, err := addPendingAgentTemplateVersionTx(ctx, tx, template)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	for _, artifact := range artifacts {
		if err := insertEvalArtifactTx(ctx, tx, artifact); err != nil {
			return AgentTemplateVersion{}, err
		}
	}
	review.VersionID = versionID
	if strings.TrimSpace(review.TemplateID) == "" {
		review.TemplateID = template.ID
	}
	if err := insertAgentTemplateCandidateReviewTx(ctx, tx, review); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersion(ctx, template.ID, fmt.Sprintf("v%d", versionNumber))
}

func addPendingAgentTemplateVersionTx(ctx context.Context, tx *sql.Tx, template AgentTemplate) (string, int, error) {
	versionNumber, err := nextAgentTemplateVersionNumber(ctx, tx, template.ID)
	if err != nil {
		return "", 0, err
	}
	versionID := agentTemplateVersionID(template.ID, versionNumber)
	contentHash := templateContentHash(template.Content)
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_template_versions(id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, updated_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		versionID, template.ID, versionNumber, template.Name, template.Description, template.SourceRepo, template.SourceRef, template.SourcePath, template.ResolvedCommit, contentHash, template.Content, template.MetadataJSON); err != nil {
		return "", 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET latest_version_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, versionID, template.ID)
	if err != nil {
		return "", 0, err
	}
	if err := requireAffected(result, "agent template", template.ID); err != nil {
		return "", 0, err
	}
	return versionID, versionNumber, nil
}

func (s *Store) UpsertAgentTemplateCandidateReview(ctx context.Context, review AgentTemplateCandidateReview) error {
	return upsertAgentTemplateCandidateReview(ctx, s.db, review)
}

func upsertAgentTemplateCandidateReview(ctx context.Context, execer sqlExecer, review AgentTemplateCandidateReview) error {
	if strings.TrimSpace(review.VersionID) == "" {
		return errors.New("candidate review version id is required")
	}
	if strings.TrimSpace(review.TemplateID) == "" {
		return errors.New("candidate review template id is required")
	}
	if strings.TrimSpace(review.State) == "" {
		review.State = "pending"
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO agent_template_candidate_reviews(
			version_id, template_id, base_version_id, diff_artifact_id, score, preference_summary,
			eval_report_json, summary_metadata_json, state, decision_reason, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(version_id) DO UPDATE SET
			template_id = excluded.template_id,
			base_version_id = excluded.base_version_id,
			diff_artifact_id = excluded.diff_artifact_id,
			score = excluded.score,
			preference_summary = excluded.preference_summary,
			eval_report_json = excluded.eval_report_json,
			summary_metadata_json = excluded.summary_metadata_json,
			state = excluded.state,
			decision_reason = excluded.decision_reason,
			updated_at = CURRENT_TIMESTAMP`,
		strings.TrimSpace(review.VersionID),
		strings.TrimSpace(review.TemplateID),
		strings.TrimSpace(review.BaseVersionID),
		strings.TrimSpace(review.DiffArtifactID),
		review.Score,
		strings.TrimSpace(review.PreferenceSummary),
		strings.TrimSpace(review.EvalReportJSON),
		strings.TrimSpace(review.SummaryMetadataJSON),
		strings.TrimSpace(review.State),
		strings.TrimSpace(review.DecisionReason))
	return err
}

func insertAgentTemplateCandidateReviewTx(ctx context.Context, tx *sql.Tx, review AgentTemplateCandidateReview) error {
	return upsertAgentTemplateCandidateReview(ctx, tx, review)
}

func (s *Store) GetAgentTemplateCandidateReview(ctx context.Context, versionID string) (AgentTemplateCandidateReview, error) {
	row := s.db.QueryRowContext(ctx, `SELECT version_id, template_id, base_version_id, diff_artifact_id, score, preference_summary,
			eval_report_json, summary_metadata_json, state, decision_reason, created_at, updated_at, decided_at
		FROM agent_template_candidate_reviews WHERE version_id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateCandidateReview(row)
}

func (s *Store) PromoteAgentTemplateVersion(ctx context.Context, versionID string) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, versionID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	// A `pending` candidate promotes directly (#471); a `canary` candidate (#484)
	// GRADUATES through the SAME state-machine writes (supersede champion, become
	// current, clear the canary fraction/window). Any other state is a hard error.
	if target.State != "pending" && target.State != "canary" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not pending or canary", target.ID, target.State)
	}
	current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if hasCurrent {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'superseded', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, current.ID); err != nil {
			return AgentTemplateVersion{}, err
		}
	}
	// Clear canary_sample/canary_started_at on the target so a graduated canary
	// carries no stale window state; pending targets already have the 0/'' defaults,
	// so this is a no-op for the #471 path.
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'current', promoted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP, canary_sample = 0, canary_started_at = '' WHERE id = ?`, target.ID); err != nil {
		return AgentTemplateVersion{}, err
	}
	latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET
			name = ?, description = ?, source_repo = ?, source_ref = ?, source_path = ?, resolved_commit = ?,
			content = ?, metadata_json = ?, current_version_id = ?, latest_version_id = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		target.Name, target.Description, target.SourceRepo, target.SourceRef, target.SourcePath, target.ResolvedCommit,
		target.Content, target.MetadataJSON, target.ID, latestID, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := upsertAgentTemplateCandidateReviewDecisionTx(ctx, tx, target, "promoted", ""); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

// UpdateAgentTemplateMetadata replaces the stored metadata_json for an installed
// template (the agent_templates row) and, when present, its current version row,
// so both the template-level read path and version-referenced exports observe the
// change. Content, name, description, and version identity are untouched: this is
// a focused metadata write used by `skillopt judge promote` to fold an accepted
// judge prompt into the template's Evaluation map without minting a new version.
func (s *Store) UpdateAgentTemplateMetadata(ctx context.Context, templateID string, metadataJSON string) (AgentTemplate, error) {
	templateID = strings.TrimSpace(templateID)
	metadataJSON = strings.TrimSpace(metadataJSON)
	if templateID == "" {
		return AgentTemplate{}, errors.New("template id is required")
	}
	if metadataJSON == "" {
		return AgentTemplate{}, errors.New("metadata_json is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplate{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET metadata_json = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, metadataJSON, templateID)
	if err != nil {
		return AgentTemplate{}, err
	}
	if err := requireAffected(result, "agent template", templateID); err != nil {
		return AgentTemplate{}, err
	}
	if current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, templateID); err != nil {
		return AgentTemplate{}, err
	} else if hasCurrent {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET metadata_json = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, metadataJSON, current.ID); err != nil {
			return AgentTemplate{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplate{}, err
	}
	return s.GetAgentTemplate(ctx, templateID)
}

// RejectAgentTemplateVersion retires a pending (#471) or canary (#484) candidate.
// The returned changed bool reports whether THIS call performed the rejection
// transition: it is false in the idempotent already-rejected branch and true when
// the row actually moved to `rejected`. Callers that emit a one-shot side effect on
// rejection (the #484 candidate.rolled_back event) MUST gate it on changed so a
// concurrent / post-crash re-run does not double-fire.
func (s *Store) RejectAgentTemplateVersion(ctx context.Context, versionID string, reason string) (AgentTemplateVersion, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, versionID)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	// Idempotent: an already-rejected target is a no-op success (changed=false) so
	// the #484 canary auto-rollback (which rejects the regressing canary) can be
	// re-run safely after a crash — or raced by a concurrent harvest — without
	// erroring AND without re-firing the rolled_back event.
	if target.State == "rejected" {
		if err := tx.Commit(); err != nil {
			return AgentTemplateVersion{}, false, err
		}
		return target, false, nil
	}
	// A `pending` candidate rejects directly (#471); a `canary` candidate (#484) is
	// retired by the auto-rollback. A canary never holds current_version_id (the
	// champion stays current throughout the canary), so rejecting it leaves the
	// champion live — no current pointer changes. The canary fraction/window are
	// cleared so no stale routing state remains.
	if target.State != "pending" && target.State != "canary" {
		return AgentTemplateVersion{}, false, fmt.Errorf("agent template version %s is %s, not pending or canary", target.ID, target.State)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'rejected', updated_at = CURRENT_TIMESTAMP, canary_sample = 0, canary_started_at = '' WHERE id = ?`, target.ID); err != nil {
		return AgentTemplateVersion{}, false, err
	}
	latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET latest_version_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, latestID, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
		return AgentTemplateVersion{}, false, err
	}
	if err := upsertAgentTemplateCandidateReviewDecisionTx(ctx, tx, target, "rejected", reason); err != nil {
		return AgentTemplateVersion{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, false, err
	}
	version, err := s.GetAgentTemplateVersionByID(ctx, target.ID)
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	return version, true, nil
}

// RevertAgentTemplateVersion makes a previously superseded version current
// again (a rollback). It mirrors PromoteAgentTemplateVersion's pointer/state
// writes but accepts a superseded target instead of a pending one, and records
// no candidate-review decision (reverts are not candidate decisions).
func (s *Store) RevertAgentTemplateVersion(ctx context.Context, templateID string, versionID string) (AgentTemplateVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, versionID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if target.TemplateID != strings.TrimSpace(templateID) {
		return AgentTemplateVersion{}, fmt.Errorf("version %s belongs to template %s, not %s", target.ID, target.TemplateID, templateID)
	}
	if target.State != "superseded" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not superseded; only a previously current version can be reverted to", target.ID, target.State)
	}
	current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if hasCurrent {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'superseded', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, current.ID); err != nil {
			return AgentTemplateVersion{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'current', promoted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, target.ID); err != nil {
		return AgentTemplateVersion{}, err
	}
	latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET
			name = ?, description = ?, source_repo = ?, source_ref = ?, source_path = ?, resolved_commit = ?,
			content = ?, metadata_json = ?, current_version_id = ?, latest_version_id = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		target.Name, target.Description, target.SourceRepo, target.SourceRef, target.SourcePath, target.ResolvedCommit,
		target.Content, target.MetadataJSON, target.ID, latestID, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

// CanaryPromoteAgentTemplateVersion transitions a PENDING candidate to the new
// `canary` state (#484) WITHOUT touching the template's current_version_id: the
// prior champion stays the live current version, so every non-sampled resolution
// is byte-identical and the routing seam (templateSnapshot) opts only a sampled
// fraction onto the canary. It records the active canary's sample fraction and
// window-start for the daemon regression comparator, and recomputes
// latest_version_id (which excludes the `canary` state) so the canary never leaks
// into latest_version_id. It enforces at most ONE active canary per template and
// requires an existing current champion (a canary only makes sense behind a live
// champion). It mirrors PromoteAgentTemplateVersion's state-machine writes but
// leaves the champion current — the defining safety property of the canary.
func (s *Store) CanaryPromoteAgentTemplateVersion(ctx context.Context, versionID string, sample float64) (AgentTemplateVersion, error) {
	if sample <= 0 || sample > 1 {
		return AgentTemplateVersion{}, fmt.Errorf("canary sample %v must be in (0,1]", sample)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, versionID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if target.State != "pending" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not pending; only a pending candidate can become a canary", target.ID, target.State)
	}
	// A canary sits BEHIND a live champion; refuse if there is no current version so
	// non-sampled traffic always has a champion to resolve to.
	_, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if !hasCurrent {
		return AgentTemplateVersion{}, fmt.Errorf("template %s has no current champion; refusing to start a canary without one", target.TemplateID)
	}
	// At most one active canary per template: a second concurrent canary would make
	// routing ambiguous and the regression window unattributable.
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_template_versions WHERE template_id = ? AND state = 'canary'`, target.TemplateID).Scan(&existing); err != nil {
		return AgentTemplateVersion{}, err
	}
	if existing > 0 {
		return AgentTemplateVersion{}, fmt.Errorf("template %s already has an active canary; resolve it before starting another", target.TemplateID)
	}
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'canary', canary_sample = ?, canary_started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, sample, startedAt, target.ID); err != nil {
		return AgentTemplateVersion{}, err
	}
	// Recompute latest_version_id: latestSelectableVersionID only considers
	// current/pending, so the now-canary version is excluded and latest falls back
	// to the champion (or a higher pending) — the canary never becomes latest.
	latestID, err := latestSelectableVersionID(ctx, tx, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_templates SET latest_version_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, latestID, target.TemplateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := requireAffected(result, "agent template", target.TemplateID); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

// GetActiveCanaryVersion returns the template's single active `canary`-state
// version (#484), or ok=false when none is canarying. It is the indexed lookup
// (idx_atv_canary) the routing seam and the daemon regression comparator use to
// discover the active canary. An unresolvable/empty template yields ok=false, not
// an error.
func (s *Store) GetActiveCanaryVersion(ctx context.Context, templateID string) (AgentTemplateVersion, bool, error) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return AgentTemplateVersion{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions
		WHERE template_id = ? AND state = 'canary'
		ORDER BY version DESC LIMIT 1`, templateID)
	version, err := scanAgentTemplateVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTemplateVersion{}, false, nil
	}
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	return version, true, nil
}

func insertEvalArtifactTx(ctx context.Context, tx *sql.Tx, artifact EvalArtifact) error {
	artifact, err := normalizeEvalArtifact(artifact)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO eval_artifacts(id, hash, media_type, size_bytes, driver, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		artifact.ID, artifact.Hash, artifact.MediaType, artifact.SizeBytes, artifact.Driver)
	return err
}

// GetCurrentAgentTemplateVersion returns the current champion version for a template
// and whether one exists (#627). It is the public, tx-free counterpart of
// getCurrentAgentTemplateVersion so the replay gate can resolve the champion the
// candidate is compared against without opening a write transaction.
func (s *Store) GetCurrentAgentTemplateVersion(ctx context.Context, templateID string) (AgentTemplateVersion, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at, v.canary_sample, v.canary_started_at
		FROM agent_templates t
		JOIN agent_template_versions v ON v.id = t.current_version_id
		WHERE t.id = ?`, strings.TrimSpace(templateID))
	version, err := scanAgentTemplateVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTemplateVersion{}, false, nil
	}
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	return version, true, nil
}

type agentTemplateScanner interface {
	Scan(dest ...any) error
}

func scanAgentTemplateWithVersion(scanner agentTemplateScanner) (AgentTemplate, error) {
	var template AgentTemplate
	if err := scanner.Scan(&template.ID, &template.Name, &template.Description, &template.SourceRepo, &template.SourceRef, &template.SourcePath, &template.ResolvedCommit, &template.Content, &template.MetadataJSON, &template.VersionID, &template.VersionNumber, &template.VersionState, &template.ContentHash, &template.CreatedAt, &template.UpdatedAt); err != nil {
		return AgentTemplate{}, err
	}
	if template.ContentHash == "" {
		template.ContentHash = templateContentHash(template.Content)
	}
	if template.VersionState == "" {
		template.VersionState = "current"
	}
	return template, nil
}

func scanAgentTemplateVersion(scanner agentTemplateScanner) (AgentTemplateVersion, error) {
	var version AgentTemplateVersion
	if err := scanner.Scan(&version.ID, &version.TemplateID, &version.VersionNumber, &version.State, &version.Name, &version.Description, &version.SourceRepo, &version.SourceRef, &version.SourcePath, &version.ResolvedCommit, &version.ContentHash, &version.Content, &version.MetadataJSON, &version.CreatedAt, &version.UpdatedAt, &version.PromotedAt, &version.CanarySample, &version.CanaryStartedAt); err != nil {
		return AgentTemplateVersion{}, err
	}
	if version.ContentHash == "" {
		version.ContentHash = templateContentHash(version.Content)
	}
	return version, nil
}

func scanAgentTemplateCandidateReview(scanner agentTemplateScanner) (AgentTemplateCandidateReview, error) {
	var review AgentTemplateCandidateReview
	var score sql.NullFloat64
	if err := scanner.Scan(&review.VersionID, &review.TemplateID, &review.BaseVersionID, &review.DiffArtifactID, &score, &review.PreferenceSummary, &review.EvalReportJSON, &review.SummaryMetadataJSON, &review.State, &review.DecisionReason, &review.CreatedAt, &review.UpdatedAt, &review.DecidedAt); err != nil {
		return AgentTemplateCandidateReview{}, err
	}
	if score.Valid {
		review.Score = &score.Float64
	}
	return review, nil
}

func agentTemplateFromVersion(version AgentTemplateVersion) AgentTemplate {
	return AgentTemplate{
		ID:             version.TemplateID,
		Name:           version.Name,
		Description:    version.Description,
		SourceRepo:     version.SourceRepo,
		SourceRef:      version.SourceRef,
		SourcePath:     version.SourcePath,
		ResolvedCommit: version.ResolvedCommit,
		Content:        version.Content,
		MetadataJSON:   version.MetadataJSON,
		VersionID:      version.ID,
		VersionNumber:  version.VersionNumber,
		VersionState:   version.State,
		ContentHash:    version.ContentHash,
		CreatedAt:      version.CreatedAt,
		UpdatedAt:      version.UpdatedAt,
	}
}

func SplitAgentTemplateReference(ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}
	if index := strings.LastIndex(ref, "@"); index > 0 {
		return strings.TrimSpace(ref[:index]), strings.TrimSpace(ref[index+1:])
	}
	return ref, ""
}

func getCurrentAgentTemplateVersion(ctx context.Context, tx *sql.Tx, templateID string) (AgentTemplateVersion, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at, v.canary_sample, v.canary_started_at
		FROM agent_templates t
		JOIN agent_template_versions v ON v.id = t.current_version_id
		WHERE t.id = ?`, templateID)
	version, err := scanAgentTemplateVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTemplateVersion{}, false, nil
	}
	if err != nil {
		return AgentTemplateVersion{}, false, err
	}
	return version, true, nil
}

func getAgentTemplateVersionByIDTx(ctx context.Context, tx *sql.Tx, versionID string) (AgentTemplateVersion, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at, canary_sample, canary_started_at
		FROM agent_template_versions WHERE id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateVersion(row)
}

func latestSelectableVersionID(ctx context.Context, tx *sql.Tx, templateID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM agent_template_versions
		WHERE template_id = ? AND state IN ('current', 'pending')
		ORDER BY version DESC LIMIT 1`, templateID).Scan(&id)
	return id, err
}

func upsertAgentTemplateCandidateReviewDecisionTx(ctx context.Context, tx *sql.Tx, version AgentTemplateVersion, state string, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_template_candidate_reviews(
			version_id, template_id, state, decision_reason, decided_at, updated_at
		)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(version_id) DO UPDATE SET
			state = excluded.state,
			decision_reason = excluded.decision_reason,
			decided_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP`,
		version.ID,
		version.TemplateID,
		strings.TrimSpace(state),
		strings.TrimSpace(reason))
	return err
}

func nextAgentTemplateVersionNumber(ctx context.Context, tx *sql.Tx, templateID string) (int, error) {
	var current sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM agent_template_versions WHERE template_id = ?`, templateID).Scan(&current); err != nil {
		return 0, err
	}
	if !current.Valid {
		return 1, nil
	}
	return int(current.Int64) + 1, nil
}

func agentTemplateVersionID(templateID string, version int) string {
	return fmt.Sprintf("%s@v%d", strings.TrimSpace(templateID), version)
}

func templateContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}
