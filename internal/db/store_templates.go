package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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
	out, err := queryList(ctx, s.db, `SELECT t.id, t.name, t.description, t.source_repo, t.source_ref, t.source_path, t.resolved_commit, t.content, t.metadata_json,
			COALESCE(v.id, ''), COALESCE(v.version, 0), COALESCE(v.state, ''), COALESCE(NULLIF(v.content_hash, ''), ''), t.created_at, t.updated_at
		FROM agent_templates t
		LEFT JOIN agent_template_versions v ON v.id = t.current_version_id
		ORDER BY t.id`, nil,
		// scanAgentTemplateWithVersion takes the DEFINED agentTemplateScanner
		// interface, which is never identical to queryList's unnamed one, so this
		// one call site needs an explicit adapter rather than the bare function.
		func(row rowScanner) (AgentTemplate, error) { return scanAgentTemplateWithVersion(row) })
	if err != nil {
		return nil, err
	}
	// Promised a non-nil empty slice before #1759 and still does.
	return emptyIfNil(out), nil
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
	row := s.db.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at
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
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
		FROM agent_template_versions
		WHERE template_id = ? AND (id = ? OR CAST(version AS TEXT) = ?)`, templateID, templateID+"@v"+versionRef, versionRef)
	return scanAgentTemplateVersion(row)
}

func (s *Store) GetAgentTemplateVersionByID(ctx context.Context, versionID string) (AgentTemplateVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
		FROM agent_template_versions WHERE id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateVersion(row)
}

func (s *Store) ListAgentTemplateVersions(ctx context.Context, templateID string) ([]AgentTemplateVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
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

// RevertAgentTemplateVersion makes a previously superseded version current
// again (a rollback): it supersedes the live current version, promotes the target
// back to `current`, and recomputes the template's current/latest pointers. Only a
// `superseded` target is accepted, so a revert can never resurrect a retired row.
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
	if err := scanner.Scan(&version.ID, &version.TemplateID, &version.VersionNumber, &version.State, &version.Name, &version.Description, &version.SourceRepo, &version.SourceRef, &version.SourcePath, &version.ResolvedCommit, &version.ContentHash, &version.Content, &version.MetadataJSON, &version.CreatedAt, &version.UpdatedAt, &version.PromotedAt); err != nil {
		return AgentTemplateVersion{}, err
	}
	if version.ContentHash == "" {
		version.ContentHash = templateContentHash(version.Content)
	}
	return version, nil
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
	row := tx.QueryRowContext(ctx, `SELECT v.id, v.template_id, v.version, v.state, v.name, v.description, v.source_repo, v.source_ref, v.source_path, v.resolved_commit, v.content_hash, v.content, v.metadata_json, v.created_at, v.updated_at, v.promoted_at
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
	row := tx.QueryRowContext(ctx, `SELECT id, template_id, version, state, name, description, source_repo, source_ref, source_path, resolved_commit, content_hash, content, metadata_json, created_at, updated_at, promoted_at
		FROM agent_template_versions WHERE id = ?`, strings.TrimSpace(versionID))
	return scanAgentTemplateVersion(row)
}

func latestSelectableVersionID(ctx context.Context, tx *sql.Tx, templateID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM agent_template_versions
		WHERE template_id = ? AND state = 'current'
		ORDER BY version DESC LIMIT 1`, templateID).Scan(&id)
	return id, err
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
