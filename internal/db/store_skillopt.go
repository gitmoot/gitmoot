package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

func (s *Store) DecideSkillOptTrainCandidate(ctx context.Context, session SkillOptTrainSession, iteration SkillOptTrainIteration, candidateID string, decision string) (AgentTemplateVersion, error) {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	iteration, err = normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	defer tx.Rollback()
	target, err := getAgentTemplateVersionByIDTx(ctx, tx, candidateID)
	if err != nil {
		return AgentTemplateVersion{}, err
	}
	if target.State != "pending" {
		return AgentTemplateVersion{}, fmt.Errorf("agent template version %s is %s, not pending", target.ID, target.State)
	}
	// Snapshot the candidate's eval report inside the transaction so the
	// judge-outcome captured after commit reflects the report the decision was
	// made against. Best-effort: capture must never fail the decision.
	normalizedDecision := strings.TrimSpace(decision)
	var captureReason string
	if normalizedDecision == "rejected" {
		captureReason = iteration.DecisionReason
	}
	capturedEvalReport := candidateEvalReportJSONTx(ctx, tx, target.ID)
	switch normalizedDecision {
	case "promoted":
		current, hasCurrent, err := getCurrentAgentTemplateVersion(ctx, tx, target.TemplateID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		if hasCurrent {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'superseded', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, current.ID); err != nil {
				return AgentTemplateVersion{}, err
			}
		}
		stateResult, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'current', promoted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = 'pending'`, target.ID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		if err := requireAffected(stateResult, "pending agent template version", target.ID); err != nil {
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
	case "rejected":
		stateResult, err := tx.ExecContext(ctx, `UPDATE agent_template_versions SET state = 'rejected', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND state = 'pending'`, target.ID)
		if err != nil {
			return AgentTemplateVersion{}, err
		}
		if err := requireAffected(stateResult, "pending agent template version", target.ID); err != nil {
			return AgentTemplateVersion{}, err
		}
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
		if err := upsertAgentTemplateCandidateReviewDecisionTx(ctx, tx, target, "rejected", iteration.DecisionReason); err != nil {
			return AgentTemplateVersion{}, err
		}
	default:
		return AgentTemplateVersion{}, fmt.Errorf("candidate decision %q is not supported", decision)
	}
	if err := upsertSkillOptTrainSessionTx(ctx, tx, session); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := upsertSkillOptTrainIterationTx(ctx, tx, iteration); err != nil {
		return AgentTemplateVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentTemplateVersion{}, err
	}
	// Capture the judge-vs-human outcome only after the decision is durably
	// committed, and never let a capture failure surface to the caller — the
	// human's promote/reject must stand regardless. (#345)
	if err := captureSkillOptJudgeOutcome(ctx, s.db, target, capturedEvalReport, normalizedDecision, captureReason); err != nil {
		log.Printf("skillopt: capture judge outcome for candidate %s failed: %v", target.ID, err)
	}
	return s.GetAgentTemplateVersionByID(ctx, target.ID)
}

func candidateEvalReportJSONTx(ctx context.Context, tx *sql.Tx, versionID string) string {
	var report string
	if err := tx.QueryRowContext(ctx, `SELECT eval_report_json FROM agent_template_candidate_reviews WHERE version_id = ?`, strings.TrimSpace(versionID)).Scan(&report); err != nil {
		return ""
	}
	return report
}

func (s *Store) UpsertEvalArtifact(ctx context.Context, artifact EvalArtifact) error {
	artifact, err := normalizeEvalArtifact(artifact)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO eval_artifacts(id, hash, media_type, size_bytes, driver, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			hash = excluded.hash,
			media_type = excluded.media_type,
			size_bytes = excluded.size_bytes,
			driver = excluded.driver`,
		artifact.ID, artifact.Hash, artifact.MediaType, artifact.SizeBytes, artifact.Driver)
	return err
}

func upsertEvalArtifactTx(ctx context.Context, tx *sql.Tx, artifact EvalArtifact) error {
	artifact, err := normalizeEvalArtifact(artifact)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO eval_artifacts(id, hash, media_type, size_bytes, driver, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			hash = excluded.hash,
			media_type = excluded.media_type,
			size_bytes = excluded.size_bytes,
			driver = excluded.driver`,
		artifact.ID, artifact.Hash, artifact.MediaType, artifact.SizeBytes, artifact.Driver)
	return err
}

func normalizeEvalArtifact(artifact EvalArtifact) (EvalArtifact, error) {
	if strings.TrimSpace(artifact.ID) == "" {
		artifact.ID = artifact.Hash
	}
	if strings.TrimSpace(artifact.Hash) == "" {
		return EvalArtifact{}, errors.New("eval artifact hash is required")
	}
	if artifact.SizeBytes < 0 {
		return EvalArtifact{}, errors.New("eval artifact size cannot be negative")
	}
	artifact.ID = strings.TrimSpace(artifact.ID)
	artifact.Hash = strings.TrimSpace(artifact.Hash)
	artifact.MediaType = strings.TrimSpace(artifact.MediaType)
	artifact.Driver = strings.TrimSpace(artifact.Driver)
	return artifact, nil
}

func (s *Store) GetEvalArtifact(ctx context.Context, id string) (EvalArtifact, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, hash, media_type, size_bytes, driver, created_at
		FROM eval_artifacts WHERE id = ?`, id)
	return scanEvalArtifact(row)
}

func (s *Store) UpsertEvalRun(ctx context.Context, run EvalRun) error {
	run, err := normalizeEvalRun(run)
	if err != nil {
		return err
	}
	return upsertEvalRunExec(ctx, s.db, run)
}

func upsertEvalRunExec(ctx context.Context, exec sqlExecer, run EvalRun) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO eval_runs(id, template_id, template_version_id, target_repo, state, mode, exploration_level, options_count, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			template_id = excluded.template_id,
			template_version_id = excluded.template_version_id,
			target_repo = excluded.target_repo,
			state = excluded.state,
			mode = excluded.mode,
			exploration_level = excluded.exploration_level,
			options_count = excluded.options_count,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		run.ID, run.TemplateID, run.TemplateVersionID, run.TargetRepo, run.State, run.Mode, run.ExplorationLevel, run.OptionsCount, run.MetadataJSON)
	return err
}

func normalizeEvalRun(run EvalRun) (EvalRun, error) {
	run.ID = strings.TrimSpace(run.ID)
	if run.ID == "" {
		return EvalRun{}, errors.New("eval run id is required")
	}
	run.TemplateID = strings.TrimSpace(run.TemplateID)
	run.TemplateVersionID = strings.TrimSpace(run.TemplateVersionID)
	run.TargetRepo = strings.TrimSpace(run.TargetRepo)
	run.State = strings.TrimSpace(run.State)
	if run.State == "" {
		run.State = "draft"
	}
	run.Mode = strings.TrimSpace(strings.ToLower(run.Mode))
	if run.Mode == "" {
		run.Mode = EvalRunModeValidate
	}
	switch run.Mode {
	case EvalRunModeExplore, EvalRunModeRefine, EvalRunModeDistill, EvalRunModeValidate:
	default:
		return EvalRun{}, fmt.Errorf("eval run mode %q is not supported", run.Mode)
	}
	run.ExplorationLevel = strings.TrimSpace(strings.ToLower(run.ExplorationLevel))
	if run.ExplorationLevel == "" {
		switch run.Mode {
		case EvalRunModeExplore:
			run.ExplorationLevel = ExplorationLevelHigh
		case EvalRunModeRefine:
			run.ExplorationLevel = ExplorationLevelMedium
		default:
			run.ExplorationLevel = ExplorationLevelLow
		}
	}
	switch run.ExplorationLevel {
	case ExplorationLevelHigh, ExplorationLevelMedium, ExplorationLevelLow:
	default:
		return EvalRun{}, fmt.Errorf("eval run exploration level %q is not supported", run.ExplorationLevel)
	}
	if run.OptionsCount == 0 {
		if run.Mode == EvalRunModeExplore {
			run.OptionsCount = 5
		} else {
			run.OptionsCount = 2
		}
	}
	if run.OptionsCount < 2 {
		return EvalRun{}, errors.New("eval run options count must be at least 2")
	}
	run.MetadataJSON = strings.TrimSpace(run.MetadataJSON)
	return run, nil
}

func (s *Store) GetEvalRun(ctx context.Context, id string) (EvalRun, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, template_version_id, target_repo, state, mode, exploration_level, options_count, metadata_json, created_at, updated_at
		FROM eval_runs WHERE id = ?`, id)
	return scanEvalRun(row)
}

func (s *Store) UpsertSkillOptTrainSession(ctx context.Context, session SkillOptTrainSession) error {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return err
	}
	return upsertSkillOptTrainSessionExec(ctx, s.db, session)
}

func upsertSkillOptTrainSessionTx(ctx context.Context, tx *sql.Tx, session SkillOptTrainSession) error {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return err
	}
	return upsertSkillOptTrainSessionExec(ctx, tx, session)
}

func upsertSkillOptTrainSessionExec(ctx context.Context, exec sqlExecer, session SkillOptTrainSession) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO skillopt_train_sessions(
			id, template_id, template_version_id, target_repo, workspace_repo, preview_repo,
			request_summary, task_kind, state, metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			template_id = excluded.template_id,
			template_version_id = excluded.template_version_id,
			target_repo = excluded.target_repo,
			workspace_repo = excluded.workspace_repo,
			preview_repo = excluded.preview_repo,
			request_summary = excluded.request_summary,
			task_kind = excluded.task_kind,
			state = excluded.state,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		session.ID, session.TemplateID, session.TemplateVersionID, session.TargetRepo, session.WorkspaceRepo,
		session.PreviewRepo, session.RequestSummary, session.TaskKind, session.State, session.MetadataJSON)
	return err
}

func normalizeSkillOptTrainSession(session SkillOptTrainSession) (SkillOptTrainSession, error) {
	session.ID = strings.TrimSpace(session.ID)
	if session.ID == "" {
		return SkillOptTrainSession{}, errors.New("skillopt train session id is required")
	}
	session.TemplateID = strings.TrimSpace(session.TemplateID)
	if session.TemplateID == "" {
		return SkillOptTrainSession{}, errors.New("skillopt train session template id is required")
	}
	session.TemplateVersionID = strings.TrimSpace(session.TemplateVersionID)
	session.TargetRepo = strings.TrimSpace(session.TargetRepo)
	session.WorkspaceRepo = strings.TrimSpace(session.WorkspaceRepo)
	session.PreviewRepo = strings.TrimSpace(session.PreviewRepo)
	session.RequestSummary = strings.TrimSpace(session.RequestSummary)
	session.TaskKind = strings.TrimSpace(strings.ToLower(session.TaskKind))
	if session.TaskKind == "" {
		session.TaskKind = "custom"
	}
	session.State = strings.TrimSpace(session.State)
	if session.State == "" {
		session.State = "request_confirmed"
	}
	session.MetadataJSON = strings.TrimSpace(session.MetadataJSON)
	return session, nil
}

func (s *Store) GetSkillOptTrainSession(ctx context.Context, id string) (SkillOptTrainSession, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, template_id, template_version_id, target_repo, workspace_repo, preview_repo,
			request_summary, task_kind, state, metadata_json, created_at, updated_at
		FROM skillopt_train_sessions WHERE id = ?`, strings.TrimSpace(id))
	return scanSkillOptTrainSession(row)
}

// ListSkillOptTrainSessions returns all SkillOpt train sessions, most recently
// updated first.
func (s *Store) ListSkillOptTrainSessions(ctx context.Context) ([]SkillOptTrainSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_id, template_version_id, target_repo, workspace_repo, preview_repo,
			request_summary, task_kind, state, metadata_json, created_at, updated_at
		FROM skillopt_train_sessions ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []SkillOptTrainSession
	for rows.Next() {
		session, err := scanSkillOptTrainSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) UpsertSkillOptTrainIteration(ctx context.Context, iteration SkillOptTrainIteration) error {
	iteration, err := normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return err
	}
	return upsertSkillOptTrainIterationExec(ctx, s.db, iteration)
}

func upsertSkillOptTrainIterationTx(ctx context.Context, tx *sql.Tx, iteration SkillOptTrainIteration) error {
	iteration, err := normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return err
	}
	return upsertSkillOptTrainIterationExec(ctx, tx, iteration)
}

func upsertSkillOptTrainIterationExec(ctx context.Context, exec sqlExecer, iteration SkillOptTrainIteration) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO skillopt_train_iterations(
			id, session_id, eval_run_id, base_template_version_id, candidate_version_id,
			mode, exploration_level, state, issue_repo, issue_number, issue_url,
			pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			session_id = excluded.session_id,
			eval_run_id = excluded.eval_run_id,
			base_template_version_id = excluded.base_template_version_id,
			candidate_version_id = excluded.candidate_version_id,
			mode = excluded.mode,
			exploration_level = excluded.exploration_level,
			state = excluded.state,
			issue_repo = excluded.issue_repo,
			issue_number = excluded.issue_number,
			issue_url = excluded.issue_url,
			pull_request_repo = excluded.pull_request_repo,
			pull_request_number = excluded.pull_request_number,
			pull_request_url = excluded.pull_request_url,
			decision_reason = excluded.decision_reason,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		iteration.ID, iteration.SessionID, iteration.EvalRunID, iteration.BaseTemplateVersionID, iteration.CandidateVersionID,
		iteration.Mode, iteration.ExplorationLevel, iteration.State, iteration.IssueRepo, iteration.IssueNumber, iteration.IssueURL,
		iteration.PullRequestRepo, iteration.PullRequestNumber, iteration.PullRequestURL, iteration.DecisionReason, iteration.MetadataJSON)
	return err
}

func (s *Store) UpsertSkillOptTrainSessionAndIteration(ctx context.Context, session SkillOptTrainSession, iteration SkillOptTrainIteration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertSkillOptTrainSessionTx(ctx, tx, session); err != nil {
		return err
	}
	if err := upsertSkillOptTrainIterationTx(ctx, tx, iteration); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpsertSkillOptTrainSessionIterationAndReviewWatch(ctx context.Context, session SkillOptTrainSession, iteration SkillOptTrainIteration, watch SkillOptReviewWatch) error {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return err
	}
	iteration, err = normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return err
	}
	watch, err = normalizeSkillOptReviewWatch(watch)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertSkillOptTrainSessionExec(ctx, tx, session); err != nil {
		return err
	}
	if err := upsertSkillOptTrainIterationExec(ctx, tx, iteration); err != nil {
		return err
	}
	if err := upsertSkillOptReviewWatchExec(ctx, tx, watch); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpsertSkillOptTrainNextIteration(ctx context.Context, session SkillOptTrainSession, iteration SkillOptTrainIteration, run EvalRun, items []EvalReviewItem) error {
	session, err := normalizeSkillOptTrainSession(session)
	if err != nil {
		return err
	}
	iteration, err = normalizeSkillOptTrainIteration(iteration)
	if err != nil {
		return err
	}
	run, err = normalizeEvalRun(run)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertSkillOptTrainSessionExec(ctx, tx, session); err != nil {
		return err
	}
	if err := upsertSkillOptTrainIterationExec(ctx, tx, iteration); err != nil {
		return err
	}
	if err := upsertEvalRunExec(ctx, tx, run); err != nil {
		return err
	}
	for _, item := range items {
		if err := upsertEvalReviewItemExec(ctx, tx, item); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func normalizeSkillOptTrainIteration(iteration SkillOptTrainIteration) (SkillOptTrainIteration, error) {
	iteration.ID = strings.TrimSpace(iteration.ID)
	if iteration.ID == "" {
		return SkillOptTrainIteration{}, errors.New("skillopt train iteration id is required")
	}
	iteration.SessionID = strings.TrimSpace(iteration.SessionID)
	if iteration.SessionID == "" {
		return SkillOptTrainIteration{}, errors.New("skillopt train iteration session id is required")
	}
	iteration.EvalRunID = strings.TrimSpace(iteration.EvalRunID)
	iteration.BaseTemplateVersionID = strings.TrimSpace(iteration.BaseTemplateVersionID)
	iteration.CandidateVersionID = strings.TrimSpace(iteration.CandidateVersionID)
	iteration.Mode = strings.TrimSpace(strings.ToLower(iteration.Mode))
	if iteration.Mode == "" {
		iteration.Mode = EvalRunModeExplore
	}
	switch iteration.Mode {
	case EvalRunModeExplore, EvalRunModeRefine, EvalRunModeDistill, EvalRunModeValidate:
	default:
		return SkillOptTrainIteration{}, fmt.Errorf("skillopt train iteration mode %q is not supported", iteration.Mode)
	}
	iteration.ExplorationLevel = strings.TrimSpace(strings.ToLower(iteration.ExplorationLevel))
	if iteration.ExplorationLevel == "" {
		switch iteration.Mode {
		case EvalRunModeExplore:
			iteration.ExplorationLevel = ExplorationLevelHigh
		case EvalRunModeRefine:
			iteration.ExplorationLevel = ExplorationLevelMedium
		default:
			iteration.ExplorationLevel = ExplorationLevelLow
		}
	}
	switch iteration.ExplorationLevel {
	case ExplorationLevelHigh, ExplorationLevelMedium, ExplorationLevelLow:
	default:
		return SkillOptTrainIteration{}, fmt.Errorf("skillopt train iteration exploration level %q is not supported", iteration.ExplorationLevel)
	}
	iteration.State = strings.TrimSpace(iteration.State)
	if iteration.State == "" {
		iteration.State = "request_confirmed"
	}
	iteration.IssueRepo = strings.TrimSpace(iteration.IssueRepo)
	iteration.IssueURL = strings.TrimSpace(iteration.IssueURL)
	iteration.PullRequestRepo = strings.TrimSpace(iteration.PullRequestRepo)
	iteration.PullRequestURL = strings.TrimSpace(iteration.PullRequestURL)
	iteration.DecisionReason = strings.TrimSpace(iteration.DecisionReason)
	iteration.MetadataJSON = strings.TrimSpace(iteration.MetadataJSON)
	return iteration, nil
}

// DeleteSkillOptTrainSession removes a train session and everything keyed by it
// in one transaction: iterations, each iteration's eval run with its review
// items/options and (ranked) feedback events, review watches, and the session's
// resource locks (matched as a whole colon-delimited key segment). It refuses
// while a non-expired lock exists for the session so an in-flight generation or
// optimizer is never pulled out from under its worker. Interactive prompt
// records carry no session linkage (train-init prompts are workspace-scoped and
// deleted by their own flows), so none are touched here.
func (s *Store) DeleteSkillOptTrainSession(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("train session id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM skillopt_train_sessions WHERE id = ?`, sessionID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("train session %q not found", sessionID)
	}

	// Collect lock keys referencing the session as a whole segment, and refuse
	// while any of them is still live.
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	lockRows, err := tx.QueryContext(ctx, `SELECT resource_key, expires_at FROM resource_locks`)
	if err != nil {
		return err
	}
	sessionLockKeys := []string{}
	for lockRows.Next() {
		var key, expiresAt string
		if err := lockRows.Scan(&key, &expiresAt); err != nil {
			lockRows.Close()
			return err
		}
		matched := false
		for _, segment := range strings.Split(key, ":") {
			if segment == sessionID {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if expiresAt > now {
			lockRows.Close()
			return fmt.Errorf("train session %s has an active resource lock (%s); wait for it to finish or recover it first", sessionID, key)
		}
		sessionLockKeys = append(sessionLockKeys, key)
	}
	if err := lockRows.Err(); err != nil {
		lockRows.Close()
		return err
	}
	lockRows.Close()

	runRows, err := tx.QueryContext(ctx, `SELECT eval_run_id FROM skillopt_train_iterations WHERE session_id = ? AND eval_run_id <> ''`, sessionID)
	if err != nil {
		return err
	}
	runIDs := []string{}
	for runRows.Next() {
		var runID string
		if err := runRows.Scan(&runID); err != nil {
			runRows.Close()
			return err
		}
		runIDs = append(runIDs, runID)
	}
	if err := runRows.Err(); err != nil {
		runRows.Close()
		return err
	}
	runRows.Close()

	for _, runID := range runIDs {
		for _, stmt := range []string{
			`DELETE FROM eval_review_items WHERE run_id = ?`,
			`DELETE FROM eval_review_options WHERE run_id = ?`,
			`DELETE FROM feedback_events WHERE run_id = ?`,
			`DELETE FROM ranked_feedback_events WHERE run_id = ?`,
			`DELETE FROM skillopt_review_watches WHERE run_id = ?`,
			`DELETE FROM eval_runs WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, runID); err != nil {
				return err
			}
		}
	}
	for _, key := range sessionLockKeys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_locks WHERE resource_key = ?`, key); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM skillopt_train_iterations WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM skillopt_train_sessions WHERE id = ?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordCreatedRepo remembers that gitmoot created the repo. A repo can only be
// created once, so on conflict the ORIGINAL creator linkage is preserved (a
// later session re-recording the same name must not steal the cleanup offer).
func (s *Store) RecordCreatedRepo(ctx context.Context, record CreatedRepo) error {
	record.Repo = strings.TrimSpace(record.Repo)
	if record.Repo == "" {
		return errors.New("created repo name is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO created_repos(repo, purpose, session_id, created_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo) DO NOTHING`,
		record.Repo, record.Purpose, record.SessionID)
	return err
}

// ListCreatedReposForSession returns the repos gitmoot created for a session.
// AdoptCreatedRepoRecords links repos recorded before a session existed (a
// setup form creates them with an empty session id) to the session, so the
// session-delete cleanup offer includes them. Rows already owned by another
// session are left alone.
func (s *Store) AdoptCreatedRepoRecords(ctx context.Context, sessionID string, repos []string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session id is required")
	}
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE created_repos SET session_id = ? WHERE repo = ? AND TRIM(COALESCE(session_id,'')) = ''`, sessionID, repo); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListCreatedReposForSession(ctx context.Context, sessionID string) ([]CreatedRepo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, purpose, session_id, created_at FROM created_repos WHERE session_id = ? ORDER BY created_at`, strings.TrimSpace(sessionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []CreatedRepo{}
	for rows.Next() {
		var record CreatedRepo
		if err := rows.Scan(&record.Repo, &record.Purpose, &record.SessionID, &record.CreatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// DeleteCreatedRepoRecord forgets a created-repo record (after the repo itself
// was deleted, or to stop offering cleanup for it).
func (s *Store) DeleteCreatedRepoRecord(ctx context.Context, repo string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM created_repos WHERE repo = ?`, strings.TrimSpace(repo))
	return err
}

func (s *Store) GetSkillOptTrainIteration(ctx context.Context, id string) (SkillOptTrainIteration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, eval_run_id, base_template_version_id,
			candidate_version_id, mode, exploration_level, state, issue_repo, issue_number,
			issue_url, pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json,
			created_at, updated_at
		FROM skillopt_train_iterations WHERE id = ?`, strings.TrimSpace(id))
	return scanSkillOptTrainIteration(row)
}

func (s *Store) ListSkillOptTrainIterations(ctx context.Context, sessionID string) ([]SkillOptTrainIteration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, eval_run_id, base_template_version_id,
			candidate_version_id, mode, exploration_level, state, issue_repo, issue_number,
			issue_url, pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json,
			created_at, updated_at
		FROM skillopt_train_iterations
		WHERE session_id = ?
		ORDER BY rowid`, strings.TrimSpace(sessionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var iterations []SkillOptTrainIteration
	for rows.Next() {
		iteration, err := scanSkillOptTrainIteration(rows)
		if err != nil {
			return nil, err
		}
		iterations = append(iterations, iteration)
	}
	return iterations, rows.Err()
}

func (s *Store) GetLatestSkillOptTrainIteration(ctx context.Context, sessionID string) (SkillOptTrainIteration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, eval_run_id, base_template_version_id,
			candidate_version_id, mode, exploration_level, state, issue_repo, issue_number,
			issue_url, pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json,
			created_at, updated_at
		FROM skillopt_train_iterations
		WHERE session_id = ?
		ORDER BY rowid DESC
		LIMIT 1`, strings.TrimSpace(sessionID))
	return scanSkillOptTrainIteration(row)
}

func (s *Store) GetSkillOptTrainIterationByEvalRun(ctx context.Context, evalRunID string) (SkillOptTrainIteration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, eval_run_id, base_template_version_id,
			candidate_version_id, mode, exploration_level, state, issue_repo, issue_number,
			issue_url, pull_request_repo, pull_request_number, pull_request_url, decision_reason, metadata_json,
			created_at, updated_at
		FROM skillopt_train_iterations
		WHERE eval_run_id = ?
		ORDER BY rowid DESC
		LIMIT 1`, strings.TrimSpace(evalRunID))
	return scanSkillOptTrainIteration(row)
}

func (s *Store) UpsertSkillOptReviewWatch(ctx context.Context, watch SkillOptReviewWatch) error {
	watch, err := normalizeSkillOptReviewWatch(watch)
	if err != nil {
		return err
	}
	return upsertSkillOptReviewWatchExec(ctx, s.db, watch)
}

func upsertSkillOptReviewWatchExec(ctx context.Context, exec sqlExecer, watch SkillOptReviewWatch) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO skillopt_review_watches(
			repo, issue_number, run_id, expected_item_ids_json, status, last_seen_comment_id,
			last_import_error_hash, stale_after, stale_threshold_seconds, stale_notified,
			metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(repo, issue_number) DO UPDATE SET
			run_id = excluded.run_id,
			expected_item_ids_json = excluded.expected_item_ids_json,
			status = excluded.status,
			last_seen_comment_id = excluded.last_seen_comment_id,
			last_import_error_hash = excluded.last_import_error_hash,
			stale_after = excluded.stale_after,
			stale_threshold_seconds = excluded.stale_threshold_seconds,
			stale_notified = excluded.stale_notified,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		watch.Repo, watch.IssueNumber, watch.RunID, watch.ExpectedItemIDsJSON, watch.Status, watch.LastSeenCommentID,
		watch.LastImportErrorHash, watch.StaleAfter, watch.StaleThresholdSeconds, boolInt(watch.StaleNotified), watch.MetadataJSON)
	return err
}

func normalizeSkillOptReviewWatch(watch SkillOptReviewWatch) (SkillOptReviewWatch, error) {
	watch.Repo = strings.TrimSpace(watch.Repo)
	if watch.Repo == "" {
		return SkillOptReviewWatch{}, errors.New("skillopt review watch repo is required")
	}
	if watch.IssueNumber <= 0 {
		return SkillOptReviewWatch{}, errors.New("skillopt review watch issue number is required")
	}
	watch.RunID = strings.TrimSpace(watch.RunID)
	if watch.RunID == "" {
		return SkillOptReviewWatch{}, errors.New("skillopt review watch run id is required")
	}
	watch.ExpectedItemIDsJSON = strings.TrimSpace(watch.ExpectedItemIDsJSON)
	watch.Status = strings.TrimSpace(strings.ToLower(watch.Status))
	if watch.Status == "" {
		watch.Status = SkillOptReviewWatchStatusWatching
	}
	switch watch.Status {
	case SkillOptReviewWatchStatusWatching, SkillOptReviewWatchStatusImported, SkillOptReviewWatchStatusClosed, SkillOptReviewWatchStatusStaleNotified, SkillOptReviewWatchStatusFailed:
	default:
		return SkillOptReviewWatch{}, fmt.Errorf("skillopt review watch status %q is not supported", watch.Status)
	}
	watch.LastImportErrorHash = strings.TrimSpace(watch.LastImportErrorHash)
	watch.StaleAfter = strings.TrimSpace(watch.StaleAfter)
	if watch.StaleThresholdSeconds < 0 {
		return SkillOptReviewWatch{}, errors.New("skillopt review watch stale threshold must not be negative")
	}
	watch.MetadataJSON = strings.TrimSpace(watch.MetadataJSON)
	return watch, nil
}

func (s *Store) GetSkillOptReviewWatch(ctx context.Context, repo string, issueNumber int64) (SkillOptReviewWatch, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo, issue_number, run_id, expected_item_ids_json, status,
			last_seen_comment_id, last_import_error_hash, stale_after, stale_threshold_seconds,
			stale_notified, metadata_json, created_at, updated_at
		FROM skillopt_review_watches
		WHERE repo = ? AND issue_number = ?`, strings.TrimSpace(repo), issueNumber)
	return scanSkillOptReviewWatch(row)
}

func (s *Store) ListSkillOptReviewWatches(ctx context.Context, status string) ([]SkillOptReviewWatch, error) {
	status = strings.TrimSpace(strings.ToLower(status))
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT repo, issue_number, run_id, expected_item_ids_json, status,
				last_seen_comment_id, last_import_error_hash, stale_after, stale_threshold_seconds,
				stale_notified, metadata_json, created_at, updated_at
			FROM skillopt_review_watches
			ORDER BY rowid`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT repo, issue_number, run_id, expected_item_ids_json, status,
				last_seen_comment_id, last_import_error_hash, stale_after, stale_threshold_seconds,
				stale_notified, metadata_json, created_at, updated_at
			FROM skillopt_review_watches
			WHERE status = ?
			ORDER BY rowid`, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	watches := []SkillOptReviewWatch{}
	for rows.Next() {
		watch, err := scanSkillOptReviewWatch(rows)
		if err != nil {
			return nil, err
		}
		watches = append(watches, watch)
	}
	return watches, rows.Err()
}

func (s *Store) UpsertEvalReviewItem(ctx context.Context, item EvalReviewItem) error {
	return upsertEvalReviewItemExec(ctx, s.db, item)
}

func upsertEvalReviewItemExec(ctx context.Context, exec sqlExecer, item EvalReviewItem) error {
	if strings.TrimSpace(item.ID) == "" {
		item.ID = item.RunID + "/" + item.ItemID
	}
	if strings.TrimSpace(item.RunID) == "" {
		return errors.New("eval review item run id is required")
	}
	if strings.TrimSpace(item.ItemID) == "" {
		return errors.New("eval review item id is required")
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO eval_review_items(
			id, run_id, item_id, title, source_artifact_id, baseline_artifact_id, candidate_artifact_id,
			preview_artifact_id, diff_artifact_id, metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			run_id = excluded.run_id,
			item_id = excluded.item_id,
			title = excluded.title,
			source_artifact_id = excluded.source_artifact_id,
			baseline_artifact_id = excluded.baseline_artifact_id,
			candidate_artifact_id = excluded.candidate_artifact_id,
			preview_artifact_id = excluded.preview_artifact_id,
			diff_artifact_id = excluded.diff_artifact_id,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		item.ID, item.RunID, item.ItemID, item.Title, item.SourceArtifactID, item.BaselineArtifactID, item.CandidateArtifactID,
		item.PreviewArtifactID, item.DiffArtifactID, item.MetadataJSON)
	return err
}

// normalizeEvalReviewGenerationWrite validates and canonicalizes a single
// generation write, scoping its review item and options to runID, normalizing
// each artifact/option, and rejecting duplicate option labels for the item.
func normalizeEvalReviewGenerationWrite(runID string, write EvalReviewGenerationWrite) (EvalReviewGenerationWrite, error) {
	itemID := strings.TrimSpace(write.ItemID)
	if itemID == "" && write.ReviewItem != nil {
		itemID = strings.TrimSpace(write.ReviewItem.ItemID)
	}
	if itemID == "" {
		return EvalReviewGenerationWrite{}, errors.New("eval review generation item id is required")
	}
	next := EvalReviewGenerationWrite{ItemID: itemID}
	for _, artifact := range write.Artifacts {
		artifact, err := normalizeEvalArtifact(artifact)
		if err != nil {
			return EvalReviewGenerationWrite{}, err
		}
		next.Artifacts = append(next.Artifacts, artifact)
	}
	if write.ReviewItem != nil {
		item := *write.ReviewItem
		item.RunID = runID
		item.ItemID = itemID
		if strings.TrimSpace(item.ID) == "" {
			item.ID = item.RunID + "/" + item.ItemID
		}
		if strings.TrimSpace(item.RunID) == "" {
			return EvalReviewGenerationWrite{}, errors.New("eval review item run id is required")
		}
		if strings.TrimSpace(item.ItemID) == "" {
			return EvalReviewGenerationWrite{}, errors.New("eval review item id is required")
		}
		next.ReviewItem = &item
	}
	seen := map[string]struct{}{}
	for _, option := range write.Options {
		option.RunID = runID
		option.ItemID = itemID
		option, err := normalizeEvalReviewOption(option)
		if err != nil {
			return EvalReviewGenerationWrite{}, err
		}
		if _, ok := seen[option.Label]; ok {
			return EvalReviewGenerationWrite{}, fmt.Errorf("eval review option label %q is duplicated for item %q", option.Label, itemID)
		}
		seen[option.Label] = struct{}{}
		next.Options = append(next.Options, option)
	}
	return next, nil
}

// writeGeneratedEvalReviewArtifactsTx persists one normalized generation write
// inside tx: upserts artifacts, upserts the review item, and replaces the item's
// options (DELETE-then-INSERT scoped to run_id/item_id). The caller owns the
// transaction lifecycle. The write must already be normalized.
func writeGeneratedEvalReviewArtifactsTx(ctx context.Context, tx *sql.Tx, runID string, write EvalReviewGenerationWrite) error {
	for _, artifact := range write.Artifacts {
		if err := upsertEvalArtifactTx(ctx, tx, artifact); err != nil {
			return err
		}
	}
	if write.ReviewItem != nil {
		item := *write.ReviewItem
		if _, err := tx.ExecContext(ctx, `INSERT INTO eval_review_items(
				id, run_id, item_id, title, source_artifact_id, baseline_artifact_id, candidate_artifact_id,
				preview_artifact_id, diff_artifact_id, metadata_json, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET
				run_id = excluded.run_id,
				item_id = excluded.item_id,
				title = excluded.title,
				source_artifact_id = excluded.source_artifact_id,
				baseline_artifact_id = excluded.baseline_artifact_id,
				candidate_artifact_id = excluded.candidate_artifact_id,
				preview_artifact_id = excluded.preview_artifact_id,
				diff_artifact_id = excluded.diff_artifact_id,
				metadata_json = excluded.metadata_json,
				updated_at = CURRENT_TIMESTAMP`,
			item.ID, item.RunID, item.ItemID, item.Title, item.SourceArtifactID, item.BaselineArtifactID, item.CandidateArtifactID,
			item.PreviewArtifactID, item.DiffArtifactID, item.MetadataJSON); err != nil {
			return err
		}
	}
	if len(write.Options) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM eval_review_options WHERE run_id = ? AND item_id = ?`, runID, write.ItemID); err != nil {
		return err
	}
	for _, option := range write.Options {
		if _, err := tx.ExecContext(ctx, `INSERT INTO eval_review_options(
				id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			option.ID, option.RunID, option.ItemID, option.Label, option.ArtifactID, option.Role, option.MetadataJSON); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ReplaceGeneratedEvalReviewArtifacts(ctx context.Context, runID string, writes []EvalReviewGenerationWrite) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("eval review generation run id is required")
	}
	normalized := make([]EvalReviewGenerationWrite, 0, len(writes))
	for _, write := range writes {
		next, err := normalizeEvalReviewGenerationWrite(runID, write)
		if err != nil {
			return err
		}
		normalized = append(normalized, next)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, write := range normalized {
		if err := writeGeneratedEvalReviewArtifactsTx(ctx, tx, runID, write); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceGeneratedEvalReviewArtifactsForItem atomically persists the generated
// artifacts, review item, and options for a single item in one transaction so a
// completed item is durable on its own (artifacts + item row + options commit
// together). Options are replaced DELETE-then-INSERT scoped to (run_id,item_id),
// so re-writing the same item is idempotent and writing one item leaves others
// untouched.
func (s *Store) ReplaceGeneratedEvalReviewArtifactsForItem(ctx context.Context, runID string, write EvalReviewGenerationWrite) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("eval review generation run id is required")
	}
	normalized, err := normalizeEvalReviewGenerationWrite(runID, write)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeGeneratedEvalReviewArtifactsTx(ctx, tx, runID, normalized); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListEvalReviewItems(ctx context.Context, runID string) ([]EvalReviewItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, item_id, title, source_artifact_id, baseline_artifact_id,
			candidate_artifact_id, preview_artifact_id, diff_artifact_id, metadata_json, created_at, updated_at
		FROM eval_review_items WHERE run_id = ? ORDER BY item_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []EvalReviewItem
	for rows.Next() {
		item, err := scanEvalReviewItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) UpsertEvalReviewOption(ctx context.Context, option EvalReviewOption) error {
	option, err := normalizeEvalReviewOption(option)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO eval_review_options(
			id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(run_id, item_id, label) DO UPDATE SET
			id = excluded.id,
			artifact_id = excluded.artifact_id,
			role = excluded.role,
			metadata_json = excluded.metadata_json,
			updated_at = CURRENT_TIMESTAMP`,
		option.ID, option.RunID, option.ItemID, option.Label, option.ArtifactID, option.Role, option.MetadataJSON)
	return err
}

func (s *Store) ReplaceEvalReviewOptions(ctx context.Context, runID string, itemID string, options []EvalReviewOption) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("eval review option run id is required")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return errors.New("eval review option item id is required")
	}
	normalized := make([]EvalReviewOption, 0, len(options))
	seen := map[string]struct{}{}
	for _, option := range options {
		option.RunID = runID
		option.ItemID = itemID
		option, err := normalizeEvalReviewOption(option)
		if err != nil {
			return err
		}
		if _, ok := seen[option.Label]; ok {
			return fmt.Errorf("eval review option label %q is duplicated for item %q", option.Label, itemID)
		}
		seen[option.Label] = struct{}{}
		normalized = append(normalized, option)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM eval_review_options WHERE run_id = ? AND item_id = ?`, runID, itemID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, option := range normalized {
		if _, err := tx.ExecContext(ctx, `INSERT INTO eval_review_options(
				id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			option.ID, option.RunID, option.ItemID, option.Label, option.ArtifactID, option.Role, option.MetadataJSON); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func normalizeEvalReviewOption(option EvalReviewOption) (EvalReviewOption, error) {
	option.RunID = strings.TrimSpace(option.RunID)
	if option.RunID == "" {
		return EvalReviewOption{}, errors.New("eval review option run id is required")
	}
	option.ItemID = strings.TrimSpace(option.ItemID)
	if option.ItemID == "" {
		return EvalReviewOption{}, errors.New("eval review option item id is required")
	}
	option.Label = normalizeOptionLabel(option.Label)
	if option.Label == "" {
		return EvalReviewOption{}, errors.New("eval review option label is required")
	}
	option.ArtifactID = strings.TrimSpace(option.ArtifactID)
	if option.ArtifactID == "" {
		return EvalReviewOption{}, errors.New("eval review option artifact id is required")
	}
	option.Role = strings.TrimSpace(strings.ToLower(option.Role))
	if option.ID == "" {
		option.ID = option.RunID + "/" + option.ItemID + "/" + option.Label
	}
	option.MetadataJSON = strings.TrimSpace(option.MetadataJSON)
	return option, nil
}

func (s *Store) ListEvalReviewOptions(ctx context.Context, runID string, itemID string) ([]EvalReviewOption, error) {
	runID = strings.TrimSpace(runID)
	itemID = strings.TrimSpace(itemID)
	var (
		rows *sql.Rows
		err  error
	)
	if itemID == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
			FROM eval_review_options WHERE run_id = ? ORDER BY item_id, label`, runID)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT id, run_id, item_id, label, artifact_id, role, metadata_json, created_at, updated_at
			FROM eval_review_options WHERE run_id = ? AND item_id = ? ORDER BY label`, runID, itemID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var options []EvalReviewOption
	for rows.Next() {
		option, err := scanEvalReviewOption(rows)
		if err != nil {
			return nil, err
		}
		options = append(options, option)
	}
	return options, rows.Err()
}

func scanEvalArtifact(row interface{ Scan(dest ...any) error }) (EvalArtifact, error) {
	var artifact EvalArtifact
	if err := row.Scan(&artifact.ID, &artifact.Hash, &artifact.MediaType, &artifact.SizeBytes, &artifact.Driver, &artifact.CreatedAt); err != nil {
		return EvalArtifact{}, err
	}
	return artifact, nil
}

func scanEvalRun(row interface{ Scan(dest ...any) error }) (EvalRun, error) {
	var run EvalRun
	if err := row.Scan(&run.ID, &run.TemplateID, &run.TemplateVersionID, &run.TargetRepo, &run.State, &run.Mode, &run.ExplorationLevel, &run.OptionsCount, &run.MetadataJSON, &run.CreatedAt, &run.UpdatedAt); err != nil {
		return EvalRun{}, err
	}
	return run, nil
}

func scanSkillOptTrainSession(row interface{ Scan(dest ...any) error }) (SkillOptTrainSession, error) {
	var session SkillOptTrainSession
	if err := row.Scan(&session.ID, &session.TemplateID, &session.TemplateVersionID, &session.TargetRepo, &session.WorkspaceRepo, &session.PreviewRepo,
		&session.RequestSummary, &session.TaskKind, &session.State, &session.MetadataJSON, &session.CreatedAt, &session.UpdatedAt); err != nil {
		return SkillOptTrainSession{}, err
	}
	return session, nil
}

func scanSkillOptTrainIteration(row interface{ Scan(dest ...any) error }) (SkillOptTrainIteration, error) {
	var iteration SkillOptTrainIteration
	if err := row.Scan(&iteration.ID, &iteration.SessionID, &iteration.EvalRunID, &iteration.BaseTemplateVersionID,
		&iteration.CandidateVersionID, &iteration.Mode, &iteration.ExplorationLevel, &iteration.State, &iteration.IssueRepo,
		&iteration.IssueNumber, &iteration.IssueURL, &iteration.PullRequestRepo, &iteration.PullRequestNumber,
		&iteration.PullRequestURL, &iteration.DecisionReason, &iteration.MetadataJSON, &iteration.CreatedAt, &iteration.UpdatedAt); err != nil {
		return SkillOptTrainIteration{}, err
	}
	return iteration, nil
}

func scanSkillOptReviewWatch(row interface{ Scan(dest ...any) error }) (SkillOptReviewWatch, error) {
	var watch SkillOptReviewWatch
	var staleNotified int
	if err := row.Scan(&watch.Repo, &watch.IssueNumber, &watch.RunID, &watch.ExpectedItemIDsJSON, &watch.Status,
		&watch.LastSeenCommentID, &watch.LastImportErrorHash, &watch.StaleAfter, &watch.StaleThresholdSeconds,
		&staleNotified, &watch.MetadataJSON, &watch.CreatedAt, &watch.UpdatedAt); err != nil {
		return SkillOptReviewWatch{}, err
	}
	watch.StaleNotified = staleNotified != 0
	return watch, nil
}

func scanEvalReviewItem(row interface{ Scan(dest ...any) error }) (EvalReviewItem, error) {
	var item EvalReviewItem
	if err := row.Scan(&item.ID, &item.RunID, &item.ItemID, &item.Title, &item.SourceArtifactID, &item.BaselineArtifactID,
		&item.CandidateArtifactID, &item.PreviewArtifactID, &item.DiffArtifactID, &item.MetadataJSON, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return EvalReviewItem{}, err
	}
	return item, nil
}

func scanEvalReviewOption(row interface{ Scan(dest ...any) error }) (EvalReviewOption, error) {
	var option EvalReviewOption
	if err := row.Scan(&option.ID, &option.RunID, &option.ItemID, &option.Label, &option.ArtifactID, &option.Role, &option.MetadataJSON, &option.CreatedAt, &option.UpdatedAt); err != nil {
		return EvalReviewOption{}, err
	}
	return option, nil
}

func (s *Store) UpsertFeedbackEvent(ctx context.Context, event FeedbackEvent) error {
	if strings.TrimSpace(event.ID) == "" {
		event.ID = feedbackEventID(event)
	}
	if strings.TrimSpace(event.RunID) == "" {
		return errors.New("feedback event run id is required")
	}
	if strings.TrimSpace(event.ItemID) == "" {
		return errors.New("feedback event item id is required")
	}
	if strings.TrimSpace(event.Choice) == "" {
		return errors.New("feedback event choice is required")
	}
	if strings.TrimSpace(event.Reviewer) == "" {
		return errors.New("feedback event reviewer is required")
	}
	if strings.TrimSpace(event.Source) == "" {
		return errors.New("feedback event source is required")
	}
	if strings.TrimSpace(event.CreatedAt) == "" {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO feedback_events(id, run_id, item_id, choice, reasoning, reviewer, source, source_url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, item_id, reviewer, source, source_url) DO UPDATE SET
			id = excluded.id,
			choice = excluded.choice,
			reasoning = excluded.reasoning,
			reviewer = excluded.reviewer,
			source = excluded.source,
			source_url = excluded.source_url,
			created_at = excluded.created_at`,
		event.ID, event.RunID, event.ItemID, event.Choice, event.Reasoning, event.Reviewer, event.Source, event.SourceURL, event.CreatedAt)
	return err
}

// UpsertBinaryVerdict inserts or replaces one binary verdict keyed by
// (run_id, question_id) so a re-run of the same question set against the same
// run overwrites verdicts in place (stable row count).
func (s *Store) UpsertBinaryVerdict(ctx context.Context, v BinaryVerdict) error {
	if strings.TrimSpace(v.RunID) == "" {
		return errors.New("binary verdict run id is required")
	}
	if strings.TrimSpace(v.QuestionID) == "" {
		return errors.New("binary verdict question id is required")
	}
	if strings.TrimSpace(v.Verdict) == "" {
		v.Verdict = "no"
	}
	// Weights default to 1 so an unweighted caller (and the DB DEFAULT) agree,
	// and so aggregation over the persisted rows reproduces the run's scores.
	if v.QuestionWeight <= 0 {
		v.QuestionWeight = 1
	}
	if v.DimensionWeight <= 0 {
		v.DimensionWeight = 1
	}
	if strings.TrimSpace(v.CreatedAt) == "" {
		v.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO skillopt_binary_verdicts(run_id, question_id, dimension, verdict, explanation, question_weight, dimension_weight, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, question_id) DO UPDATE SET
			dimension = excluded.dimension,
			verdict = excluded.verdict,
			explanation = excluded.explanation,
			question_weight = excluded.question_weight,
			dimension_weight = excluded.dimension_weight,
			created_at = excluded.created_at`,
		v.RunID, v.QuestionID, v.Dimension, v.Verdict, v.Explanation, v.QuestionWeight, v.DimensionWeight, v.CreatedAt)
	return err
}

// ListBinaryVerdicts returns every binary verdict for a run, ordered by
// (dimension, question_id) for a deterministic read.
func (s *Store) ListBinaryVerdicts(ctx context.Context, runID string) ([]BinaryVerdict, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, question_id, dimension, verdict, explanation, question_weight, dimension_weight, created_at
		FROM skillopt_binary_verdicts WHERE run_id = ? ORDER BY dimension, question_id`, strings.TrimSpace(runID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var verdicts []BinaryVerdict
	for rows.Next() {
		var v BinaryVerdict
		if err := rows.Scan(&v.RunID, &v.QuestionID, &v.Dimension, &v.Verdict, &v.Explanation, &v.QuestionWeight, &v.DimensionWeight, &v.CreatedAt); err != nil {
			return nil, err
		}
		verdicts = append(verdicts, v)
	}
	return verdicts, rows.Err()
}

// ListBinaryVerdictsForTemplate returns every binary verdict for every eval run
// of a template, joined with the run's template/version ids, ordered
// deterministically by (run_id, dimension, question_id). It is read-only and
// additive — it exists for the #527 disagreement view and touches no existing
// path. An empty/whitespace templateID returns no rows.
func (s *Store) ListBinaryVerdictsForTemplate(ctx context.Context, templateID string) ([]BinaryVerdictWithRun, error) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT v.run_id, v.question_id, v.dimension, v.verdict, v.explanation, v.question_weight, v.dimension_weight, v.created_at,
			r.template_id, r.template_version_id
		FROM skillopt_binary_verdicts v
		JOIN eval_runs r ON r.id = v.run_id
		WHERE r.template_id = ?
		ORDER BY v.run_id, v.dimension, v.question_id`, templateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BinaryVerdictWithRun
	for rows.Next() {
		var v BinaryVerdictWithRun
		if err := rows.Scan(&v.RunID, &v.QuestionID, &v.Dimension, &v.Verdict, &v.Explanation, &v.QuestionWeight, &v.DimensionWeight, &v.CreatedAt,
			&v.TemplateID, &v.TemplateVersionID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) ListFeedbackEvents(ctx context.Context, runID string) ([]FeedbackEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, item_id, choice, reasoning, reviewer, source, source_url, created_at
		FROM feedback_events WHERE run_id = ? ORDER BY item_id, reviewer, source, source_url`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []FeedbackEvent
	for rows.Next() {
		event, err := scanFeedbackEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func scanFeedbackEvent(row interface{ Scan(dest ...any) error }) (FeedbackEvent, error) {
	var event FeedbackEvent
	if err := row.Scan(&event.ID, &event.RunID, &event.ItemID, &event.Choice, &event.Reasoning, &event.Reviewer, &event.Source, &event.SourceURL, &event.CreatedAt); err != nil {
		return FeedbackEvent{}, err
	}
	return event, nil
}

func (s *Store) UpsertRankedFeedbackEvent(ctx context.Context, event RankedFeedbackEvent) error {
	event, err := normalizeRankedFeedbackEvent(event)
	if err != nil {
		return err
	}
	if err := s.validateRankedFeedbackEventOptions(ctx, event); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO ranked_feedback_events(
			id, run_id, item_id, ranking_json, tie_groups_json, winner, useful_traits_json, rejected_traits_json,
			required_improvements_json, quality, continue_mode, promote, reasoning, reviewer, source, source_url, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, item_id, reviewer, source, source_url) DO UPDATE SET
			id = excluded.id,
			ranking_json = excluded.ranking_json,
			tie_groups_json = excluded.tie_groups_json,
			winner = excluded.winner,
			useful_traits_json = excluded.useful_traits_json,
			rejected_traits_json = excluded.rejected_traits_json,
			required_improvements_json = excluded.required_improvements_json,
			quality = excluded.quality,
			continue_mode = excluded.continue_mode,
			promote = excluded.promote,
			reasoning = excluded.reasoning,
			reviewer = excluded.reviewer,
			source = excluded.source,
			source_url = excluded.source_url,
			created_at = excluded.created_at`,
		event.ID, event.RunID, event.ItemID, event.RankingJSON, event.TieGroupsJSON, event.Winner, event.UsefulTraitsJSON, event.RejectedTraitsJSON, event.RequiredImprovementsJSON,
		event.Quality, event.ContinueMode, event.Promote, event.Reasoning, event.Reviewer, event.Source, event.SourceURL, event.CreatedAt)
	return err
}

// ClearEvalRunFeedback deletes all review items, review options, feedback
// events, and ranked feedback events attached to a run, leaving the eval_runs
// row itself intact. It exists so a synthetic/derived run (e.g. the #527
// binary-lessons run) can be rewritten as an atomic FULL REPLACE rather than an
// accumulating upsert: without it, shrinking the derived lesson set would leave
// stale rows behind and break the documented idempotency guarantee.
func (s *Store) ClearEvalRunFeedback(ctx context.Context, runID string) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("run id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM ranked_feedback_events WHERE run_id = ?`,
		`DELETE FROM feedback_events WHERE run_id = ?`,
		`DELETE FROM eval_review_options WHERE run_id = ?`,
		`DELETE FROM eval_review_items WHERE run_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, runID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) validateRankedFeedbackEventOptions(ctx context.Context, event RankedFeedbackEvent) error {
	run, err := s.GetEvalRun(ctx, event.RunID)
	if err != nil {
		return err
	}
	options, err := s.ListEvalReviewOptions(ctx, event.RunID, event.ItemID)
	if err != nil {
		return err
	}
	if len(options) == 0 {
		return fmt.Errorf("ranked feedback item %s has no registered review options", event.ItemID)
	}
	ranking, err := rankedFeedbackRanking(event)
	if err != nil {
		return err
	}
	tieGroups, err := rankedFeedbackTieGroups(event)
	if err != nil {
		return err
	}
	expectedOptions := len(options)
	if run.OptionsCount > 0 {
		expectedOptions = run.OptionsCount
		if len(options) != expectedOptions {
			return fmt.Errorf("ranked feedback item %s has %d registered options, want %d run options", event.ItemID, len(options), expectedOptions)
		}
	}
	if len(ranking) != expectedOptions {
		return fmt.Errorf("ranked feedback item %s ranking includes %d options, want %d options", event.ItemID, len(ranking), expectedOptions)
	}
	known := make(map[string]struct{}, len(options))
	for _, option := range options {
		known[normalizeOptionLabel(option.Label)] = struct{}{}
	}
	ranked := make(map[string]struct{}, len(ranking))
	for _, label := range ranking {
		if _, ok := known[label]; !ok {
			return fmt.Errorf("ranked feedback item %s references unknown option %q", event.ItemID, label)
		}
		ranked[label] = struct{}{}
	}
	for label := range known {
		if _, ok := ranked[label]; !ok {
			return fmt.Errorf("ranked feedback item %s missing registered option %q", event.ItemID, label)
		}
	}
	if event.Winner != "" {
		if _, ok := known[event.Winner]; !ok {
			return fmt.Errorf("ranked feedback item %s winner references unknown option %q", event.ItemID, event.Winner)
		}
		if len(tieGroups) == 0 || !stringSliceContains(tieGroups[0], event.Winner) {
			return fmt.Errorf("ranked feedback item %s winner %q is not in first ranked group", event.ItemID, event.Winner)
		}
	}
	for _, traits := range []struct {
		name string
		json string
	}{
		{name: "useful_traits_json", json: event.UsefulTraitsJSON},
		{name: "rejected_traits_json", json: event.RejectedTraitsJSON},
	} {
		if strings.TrimSpace(traits.json) == "" {
			continue
		}
		var decoded map[string][]string
		if err := json.Unmarshal([]byte(traits.json), &decoded); err != nil {
			return fmt.Errorf("ranked feedback %s must be a JSON object keyed by option label: %w", traits.name, err)
		}
		for label := range decoded {
			normalized := normalizeOptionLabel(label)
			if normalized == "" {
				return fmt.Errorf("ranked feedback %s contains an empty option label", traits.name)
			}
			if _, ok := known[normalized]; !ok {
				return fmt.Errorf("ranked feedback %s references unknown option %q", traits.name, normalized)
			}
		}
	}
	return nil
}

func normalizeRankedFeedbackEvent(event RankedFeedbackEvent) (RankedFeedbackEvent, error) {
	event.RunID = strings.TrimSpace(event.RunID)
	if event.RunID == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback run id is required")
	}
	event.ItemID = strings.TrimSpace(event.ItemID)
	if event.ItemID == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback item id is required")
	}
	event.Winner = normalizeOptionLabel(event.Winner)
	event.RankingJSON = strings.TrimSpace(event.RankingJSON)
	if event.RankingJSON == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback ranking_json is required")
	}
	if _, err := rankedFeedbackRanking(event); err != nil {
		return RankedFeedbackEvent{}, err
	}
	tieGroups, err := rankedFeedbackTieGroups(event)
	if err != nil {
		return RankedFeedbackEvent{}, err
	}
	if strings.TrimSpace(event.TieGroupsJSON) != "" {
		encoded, err := json.Marshal(tieGroups)
		if err != nil {
			return RankedFeedbackEvent{}, err
		}
		event.TieGroupsJSON = string(encoded)
	}
	event.UsefulTraitsJSON = strings.TrimSpace(event.UsefulTraitsJSON)
	event.RejectedTraitsJSON = strings.TrimSpace(event.RejectedTraitsJSON)
	event.RequiredImprovementsJSON = strings.TrimSpace(event.RequiredImprovementsJSON)
	if event.RequiredImprovementsJSON != "" {
		var decoded any
		if err := json.Unmarshal([]byte(event.RequiredImprovementsJSON), &decoded); err != nil {
			return RankedFeedbackEvent{}, fmt.Errorf("ranked feedback required_improvements_json must be valid JSON: %w", err)
		}
	}
	event.Quality = normalizeRankedFeedbackQuality(event.Quality)
	if event.Quality == "__invalid__" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback quality must be one of poor, acceptable, or strong")
	}
	event.ContinueMode = normalizeRankedFeedbackContinueMode(event.ContinueMode)
	if event.ContinueMode == "__invalid__" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback continue_mode must be one of explore, refine, distill, or validate")
	}
	event.Promote = normalizeRankedFeedbackPromote(event.Promote)
	if event.Promote == "__invalid__" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback promote must be yes or no")
	}
	event.Reasoning = strings.TrimSpace(event.Reasoning)
	event.Reviewer = strings.TrimSpace(event.Reviewer)
	if event.Reviewer == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback reviewer is required")
	}
	event.Source = strings.TrimSpace(event.Source)
	if event.Source == "" {
		return RankedFeedbackEvent{}, errors.New("ranked feedback source is required")
	}
	event.SourceURL = strings.TrimSpace(event.SourceURL)
	if strings.TrimSpace(event.CreatedAt) == "" {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(event.ID) == "" {
		event.ID = rankedFeedbackEventID(event)
	}
	return event, nil
}

func normalizeRankedFeedbackQuality(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "":
		return ""
	case "poor", "acceptable", "strong":
		return strings.TrimSpace(strings.ToLower(value))
	}
	return "__invalid__"
}

func normalizeRankedFeedbackContinueMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "":
		return ""
	case EvalRunModeExplore, EvalRunModeRefine, EvalRunModeDistill, EvalRunModeValidate:
		return strings.TrimSpace(strings.ToLower(value))
	}
	return "__invalid__"
}

func normalizeRankedFeedbackPromote(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "":
		return ""
	case "yes", "y", "true":
		return "yes"
	case "no", "n", "false":
		return "no"
	}
	return "__invalid__"
}

func rankedFeedbackRanking(event RankedFeedbackEvent) ([]string, error) {
	var ranking []string
	if err := json.Unmarshal([]byte(event.RankingJSON), &ranking); err != nil {
		return nil, fmt.Errorf("ranked feedback ranking_json must be a JSON array of option labels: %w", err)
	}
	if len(ranking) < 2 {
		return nil, errors.New("ranked feedback ranking must include at least two options")
	}
	seen := map[string]struct{}{}
	for index, label := range ranking {
		normalized := normalizeOptionLabel(label)
		if normalized == "" {
			return nil, fmt.Errorf("ranked feedback ranking contains empty option label at position %d", index+1)
		}
		if _, ok := seen[normalized]; ok {
			return nil, fmt.Errorf("ranked feedback ranking contains duplicate option label %q", normalized)
		}
		seen[normalized] = struct{}{}
		ranking[index] = normalized
	}
	return ranking, nil
}

func rankedFeedbackTieGroups(event RankedFeedbackEvent) ([][]string, error) {
	ranking, err := rankedFeedbackRanking(event)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(event.TieGroupsJSON) == "" {
		groups := make([][]string, 0, len(ranking))
		for _, label := range ranking {
			groups = append(groups, []string{label})
		}
		return groups, nil
	}
	var groups [][]string
	if err := json.Unmarshal([]byte(event.TieGroupsJSON), &groups); err != nil {
		return nil, fmt.Errorf("ranked feedback tie_groups_json must be a JSON array of option label arrays: %w", err)
	}
	flattened := make([]string, 0, len(ranking))
	seen := map[string]struct{}{}
	for groupIndex, group := range groups {
		if len(group) == 0 {
			return nil, fmt.Errorf("ranked feedback tie group %d is empty", groupIndex+1)
		}
		for labelIndex, label := range group {
			normalized := normalizeOptionLabel(label)
			if normalized == "" {
				return nil, fmt.Errorf("ranked feedback tie group %d contains empty option label at position %d", groupIndex+1, labelIndex+1)
			}
			if _, ok := seen[normalized]; ok {
				return nil, fmt.Errorf("ranked feedback tie groups contain duplicate option label %q", normalized)
			}
			seen[normalized] = struct{}{}
			groups[groupIndex][labelIndex] = normalized
			flattened = append(flattened, normalized)
		}
	}
	if len(flattened) != len(ranking) {
		return nil, fmt.Errorf("ranked feedback tie groups include %d options, want %d ranking options", len(flattened), len(ranking))
	}
	for index, label := range ranking {
		if flattened[index] != label {
			return nil, fmt.Errorf("ranked feedback tie groups do not match ranking order at position %d", index+1)
		}
	}
	return groups, nil
}

func (s *Store) ListRankedFeedbackEvents(ctx context.Context, runID string) ([]RankedFeedbackEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, item_id, ranking_json, tie_groups_json, winner, useful_traits_json, rejected_traits_json,
			required_improvements_json, quality, continue_mode, promote, reasoning, reviewer, source, source_url, created_at
		FROM ranked_feedback_events WHERE run_id = ? ORDER BY item_id, reviewer, source, source_url`, strings.TrimSpace(runID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []RankedFeedbackEvent
	for rows.Next() {
		event, err := scanRankedFeedbackEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func scanRankedFeedbackEvent(row interface{ Scan(dest ...any) error }) (RankedFeedbackEvent, error) {
	var event RankedFeedbackEvent
	if err := row.Scan(&event.ID, &event.RunID, &event.ItemID, &event.RankingJSON, &event.TieGroupsJSON, &event.Winner, &event.UsefulTraitsJSON, &event.RejectedTraitsJSON,
		&event.RequiredImprovementsJSON, &event.Quality, &event.ContinueMode, &event.Promote, &event.Reasoning, &event.Reviewer, &event.Source, &event.SourceURL, &event.CreatedAt); err != nil {
		return RankedFeedbackEvent{}, err
	}
	return event, nil
}

// ListRankedFeedbackEventsAcrossRuns returns every ranked feedback event joined
// with its eval run's template id, ordered deterministically, optionally
// filtered to one template (templateID == "" lists all). It is read-only and
// exists for the judge<->human agreement measurement harness (#344): the
// pairwise judge rows (source=skillopt-ab-judge) and the human rows they must
// be compared against live in the SAME (run_id, item_id) but across MANY runs,
// so the per-run ListRankedFeedbackEvents cannot serve the whole-store join.
func (s *Store) ListRankedFeedbackEventsAcrossRuns(ctx context.Context, templateID string) ([]RankedFeedbackEventWithTemplate, error) {
	query := `SELECT e.id, e.run_id, e.item_id, e.ranking_json, e.tie_groups_json, e.winner, e.useful_traits_json, e.rejected_traits_json,
			e.required_improvements_json, e.quality, e.continue_mode, e.promote, e.reasoning, e.reviewer, e.source, e.source_url, e.created_at,
			r.template_id
		FROM ranked_feedback_events e
		JOIN eval_runs r ON r.id = e.run_id`
	args := []any{}
	if trimmed := strings.TrimSpace(templateID); trimmed != "" {
		query += ` WHERE r.template_id = ?`
		args = append(args, trimmed)
	}
	query += ` ORDER BY e.run_id, e.item_id, e.reviewer, e.source, e.source_url`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []RankedFeedbackEventWithTemplate
	for rows.Next() {
		var event RankedFeedbackEventWithTemplate
		if err := rows.Scan(&event.ID, &event.RunID, &event.ItemID, &event.RankingJSON, &event.TieGroupsJSON, &event.Winner, &event.UsefulTraitsJSON, &event.RejectedTraitsJSON,
			&event.RequiredImprovementsJSON, &event.Quality, &event.ContinueMode, &event.Promote, &event.Reasoning, &event.Reviewer, &event.Source, &event.SourceURL, &event.CreatedAt,
			&event.TemplateID); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// GetBanditArm reads the #473 Mode B bandit arm for a (templateID,
// versionID) variant. A missing row is NOT an error: it returns ok=false with a
// zero arm, and the caller treats that as the uniform Beta(1,1) prior
// (skillopt.NewArm). This keeps the table off-by-default — no row exists until
// the manual A/B records its first pick.
func (s *Store) GetBanditArm(ctx context.Context, templateID, versionID string) (BanditArm, bool, error) {
	templateID = strings.TrimSpace(templateID)
	versionID = strings.TrimSpace(versionID)
	if versionID == "" {
		return BanditArm{}, false, errors.New("bandit arm version id is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT template_id, template_version_id, alpha, beta, pulls, updated_at
		FROM skillopt_bandit_arms WHERE template_id = ? AND template_version_id = ?`, templateID, versionID)
	var arm BanditArm
	if err := row.Scan(&arm.TemplateID, &arm.TemplateVersionID, &arm.Alpha, &arm.Beta, &arm.Pulls, &arm.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BanditArm{}, false, nil
		}
		return BanditArm{}, false, err
	}
	return arm, true, nil
}

// UpsertBanditArm writes (or replaces) a bandit arm's full posterior. The alpha/
// beta/pulls are the caller's authoritative counters; this never increments, it
// stores exactly what it is given.
func (s *Store) UpsertBanditArm(ctx context.Context, arm BanditArm) error {
	arm.TemplateVersionID = strings.TrimSpace(arm.TemplateVersionID)
	if arm.TemplateVersionID == "" {
		return errors.New("bandit arm version id is required")
	}
	arm.TemplateID = strings.TrimSpace(arm.TemplateID)
	if arm.Alpha <= 0 {
		arm.Alpha = 1
	}
	if arm.Beta <= 0 {
		arm.Beta = 1
	}
	if arm.Pulls < 0 {
		arm.Pulls = 0
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO skillopt_bandit_arms(template_id, template_version_id, alpha, beta, pulls, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(template_id, template_version_id) DO UPDATE SET
			alpha = excluded.alpha,
			beta = excluded.beta,
			pulls = excluded.pulls,
			updated_at = CURRENT_TIMESTAMP`,
		arm.TemplateID, arm.TemplateVersionID, arm.Alpha, arm.Beta, arm.Pulls)
	return err
}

// IncrementBanditArm atomically applies ONE pairwise outcome to a (templateID,
// versionID) arm in a single transaction: a win bumps alpha, a loss bumps beta,
// and either way pulls increments. A first-ever pull seeds the row from the
// Beta(1,1) prior. It returns the post-update arm so the caller can recompute
// P(challenger>champion) immediately. The two arms of one A/B are incremented
// with two calls (winner=+win, loser=+loss).
func (s *Store) IncrementBanditArm(ctx context.Context, templateID, versionID string, win bool) (BanditArm, error) {
	templateID = strings.TrimSpace(templateID)
	versionID = strings.TrimSpace(versionID)
	if versionID == "" {
		return BanditArm{}, errors.New("bandit arm version id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BanditArm{}, err
	}
	defer tx.Rollback()

	arm := BanditArm{TemplateID: templateID, TemplateVersionID: versionID, Alpha: 1, Beta: 1, Pulls: 0}
	row := tx.QueryRowContext(ctx, `SELECT template_id, alpha, beta, pulls FROM skillopt_bandit_arms
		WHERE template_id = ? AND template_version_id = ?`, templateID, versionID)
	if err := row.Scan(&arm.TemplateID, &arm.Alpha, &arm.Beta, &arm.Pulls); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return BanditArm{}, err
		}
		// No row yet: keep the Beta(1,1) prior seeded above.
		arm.TemplateID = templateID
	}
	if win {
		arm.Alpha++
	} else {
		arm.Beta++
	}
	arm.Pulls++
	if _, err := tx.ExecContext(ctx, `INSERT INTO skillopt_bandit_arms(template_id, template_version_id, alpha, beta, pulls, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(template_id, template_version_id) DO UPDATE SET
			alpha = excluded.alpha,
			beta = excluded.beta,
			pulls = excluded.pulls,
			updated_at = CURRENT_TIMESTAMP`,
		arm.TemplateID, arm.TemplateVersionID, arm.Alpha, arm.Beta, arm.Pulls); err != nil {
		return BanditArm{}, err
	}
	if err := tx.Commit(); err != nil {
		return BanditArm{}, err
	}
	return arm, nil
}

func (s *Store) ListPairwisePreferences(ctx context.Context, runID string) ([]PairwisePreference, error) {
	events, err := s.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		return nil, err
	}
	preferences := []PairwisePreference{}
	for _, event := range events {
		eventPreferences, err := PairwisePreferencesForRankedFeedback(event)
		if err != nil {
			return nil, err
		}
		preferences = append(preferences, eventPreferences...)
	}
	return preferences, nil
}

func PairwisePreferencesForRankedFeedback(event RankedFeedbackEvent) ([]PairwisePreference, error) {
	tieGroups, err := rankedFeedbackTieGroups(event)
	if err != nil {
		return nil, err
	}
	preferences := []PairwisePreference{}
	for preferredGroupIndex, preferredGroup := range tieGroups {
		for _, rejectedGroup := range tieGroups[preferredGroupIndex+1:] {
			for _, preferred := range preferredGroup {
				for _, rejected := range rejectedGroup {
					preferences = append(preferences, PairwisePreference{
						RunID:         event.RunID,
						ItemID:        event.ItemID,
						Preferred:     preferred,
						Rejected:      rejected,
						RankedEventID: event.ID,
						Reviewer:      event.Reviewer,
						Source:        event.Source,
						SourceURL:     event.SourceURL,
						CreatedAt:     event.CreatedAt,
					})
				}
			}
		}
	}
	return preferences, nil
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func feedbackEventID(event FeedbackEvent) string {
	parts := []string{event.RunID, event.ItemID, event.Reviewer, event.Source, event.SourceURL}
	for index, part := range parts {
		parts[index] = strings.TrimSpace(part)
	}
	content, _ := json.Marshal(parts)
	sum := sha256.Sum256(content)
	return "feedback:" + hex.EncodeToString(sum[:])
}

func rankedFeedbackEventID(event RankedFeedbackEvent) string {
	parts := []string{event.RunID, event.ItemID, event.Reviewer, event.Source, event.SourceURL}
	for index, part := range parts {
		parts[index] = strings.TrimSpace(part)
	}
	content, _ := json.Marshal(parts)
	sum := sha256.Sum256(content)
	return "ranked-feedback:" + hex.EncodeToString(sum[:])
}

func normalizeOptionLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// InsertSkillOptGateRun appends a gate-run audit record (#627). It is additive and
// never mutates a prior run (each gate execution is its own immutable row keyed by a
// fresh id).
func (s *Store) InsertSkillOptGateRun(ctx context.Context, run SkillOptGateRun) error {
	accepted := 0
	if run.Accepted {
		accepted = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO skillopt_gate_runs(
			id, template_id, candidate_version_id, champion_version_id, corpus_path,
			corpus_version, corpus_items, attempts, accepted, champion_mean,
			candidate_mean, reason, deltas_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.TemplateID, run.CandidateVersionID, run.ChampionVersionID, run.CorpusPath,
		run.CorpusVersion, run.CorpusItems, run.Attempts, accepted, run.ChampionMean,
		run.CandidateMean, run.Reason, run.DeltasJSON)
	return err
}

// ListSkillOptGateRuns returns the gate-run audit records for a candidate version,
// newest first (#627).
func (s *Store) ListSkillOptGateRuns(ctx context.Context, candidateVersionID string) ([]SkillOptGateRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_id, candidate_version_id, champion_version_id, corpus_path,
			corpus_version, corpus_items, attempts, accepted, champion_mean, candidate_mean, reason, deltas_json, created_at
		FROM skillopt_gate_runs WHERE candidate_version_id = ? ORDER BY created_at DESC, rowid DESC`,
		strings.TrimSpace(candidateVersionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []SkillOptGateRun{}
	for rows.Next() {
		run, err := scanSkillOptGateRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// HasAcceptedSkillOptGateRun reports whether a candidate version has at least one
// ACCEPTED gate run on record (#627) — the fact the promotion guard consults.
func (s *Store) HasAcceptedSkillOptGateRun(ctx context.Context, candidateVersionID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skillopt_gate_runs WHERE candidate_version_id = ? AND accepted = 1`,
		strings.TrimSpace(candidateVersionID)).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func scanSkillOptGateRun(scanner interface{ Scan(...any) error }) (SkillOptGateRun, error) {
	var run SkillOptGateRun
	var accepted int
	if err := scanner.Scan(&run.ID, &run.TemplateID, &run.CandidateVersionID, &run.ChampionVersionID, &run.CorpusPath,
		&run.CorpusVersion, &run.CorpusItems, &run.Attempts, &accepted, &run.ChampionMean, &run.CandidateMean,
		&run.Reason, &run.DeltasJSON, &run.CreatedAt); err != nil {
		return SkillOptGateRun{}, err
	}
	run.Accepted = accepted == 1
	return run, nil
}

// SkillOpt judge-outcome direction buckets. The four directions are the cells
// of the human-vs-judge confusion matrix.
const (
	// SkillOptJudgeDirectionAgreeAccept: human promoted and judge accepted.
	SkillOptJudgeDirectionAgreeAccept = "agree_accept"
	// SkillOptJudgeDirectionAgreeReject: human rejected and judge rejected.
	SkillOptJudgeDirectionAgreeReject = "agree_reject"
	// SkillOptJudgeDirectionJudgeAcceptHumanReject: judge accepted but the human
	// rejected (a judge false positive relative to the human).
	SkillOptJudgeDirectionJudgeAcceptHumanReject = "judge_accept_human_reject"
	// SkillOptJudgeDirectionJudgeRejectHumanAccept: judge rejected but the human
	// promoted (a judge false negative relative to the human).
	SkillOptJudgeDirectionJudgeRejectHumanAccept = "judge_reject_human_accept"
)

func (s *Store) InsertSkillOptJudgeOutcome(ctx context.Context, outcome SkillOptJudgeOutcome) error {
	id := strings.TrimSpace(outcome.ID)
	if id == "" {
		generated, err := newSkillOptJudgeOutcomeID()
		if err != nil {
			return err
		}
		id = generated
	}
	if strings.TrimSpace(outcome.CandidateVersionID) == "" {
		return errors.New("judge outcome candidate_version_id is required")
	}
	if strings.TrimSpace(outcome.HumanDecision) == "" {
		return errors.New("judge outcome human_decision is required")
	}
	if strings.TrimSpace(outcome.Direction) == "" {
		return errors.New("judge outcome direction is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO skillopt_judge_outcomes(
			id, candidate_version_id, template_id, judge_score_json, judge_prompt_version,
			judge_evaluator_id, judge_prompt_hash, human_decision, direction, reason
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		strings.TrimSpace(outcome.CandidateVersionID),
		strings.TrimSpace(outcome.TemplateID),
		strings.TrimSpace(outcome.JudgeScoreJSON),
		strings.TrimSpace(outcome.JudgePromptVersion),
		strings.TrimSpace(outcome.JudgeEvaluatorID),
		strings.TrimSpace(outcome.JudgePromptHash),
		strings.TrimSpace(outcome.HumanDecision),
		strings.TrimSpace(outcome.Direction),
		strings.TrimSpace(outcome.Reason))
	return err
}

func (s *Store) ListSkillOptJudgeOutcomes(ctx context.Context, templateID string) ([]SkillOptJudgeOutcome, error) {
	query := `SELECT id, candidate_version_id, template_id, judge_score_json, judge_prompt_version,
			judge_evaluator_id, judge_prompt_hash, human_decision, direction, reason, created_at
		FROM skillopt_judge_outcomes`
	var (
		rows *sql.Rows
		err  error
	)
	if templateID = strings.TrimSpace(templateID); templateID != "" {
		query += ` WHERE template_id = ? ORDER BY created_at, id`
		rows, err = s.db.QueryContext(ctx, query, templateID)
	} else {
		query += ` ORDER BY created_at, id`
		rows, err = s.db.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var outcomes []SkillOptJudgeOutcome
	for rows.Next() {
		outcome, err := scanSkillOptJudgeOutcome(rows)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, rows.Err()
}

func (s *Store) GetSkillOptJudgeOutcome(ctx context.Context, id string) (SkillOptJudgeOutcome, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, candidate_version_id, template_id, judge_score_json, judge_prompt_version,
			judge_evaluator_id, judge_prompt_hash, human_decision, direction, reason, created_at
		FROM skillopt_judge_outcomes WHERE id = ?`, strings.TrimSpace(id))
	return scanSkillOptJudgeOutcome(row)
}

func scanSkillOptJudgeOutcome(row interface{ Scan(dest ...any) error }) (SkillOptJudgeOutcome, error) {
	var outcome SkillOptJudgeOutcome
	if err := row.Scan(&outcome.ID, &outcome.CandidateVersionID, &outcome.TemplateID, &outcome.JudgeScoreJSON,
		&outcome.JudgePromptVersion, &outcome.JudgeEvaluatorID, &outcome.JudgePromptHash, &outcome.HumanDecision,
		&outcome.Direction, &outcome.Reason, &outcome.CreatedAt); err != nil {
		return SkillOptJudgeOutcome{}, err
	}
	return outcome, nil
}

func newSkillOptJudgeOutcomeID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "judge-outcome-" + hex.EncodeToString(raw[:]), nil
}

// captureSkillOptJudgeOutcome records one judge-vs-human outcome row for a
// candidate decision. It is best-effort: capture must never fail the decision,
// so callers log and continue on error.
//
// The judge accept/reject signal is read generically from the candidate
// review's eval_report_json (never from new typed struct fields) using
// skillOptJudgeAcceptFromReport, and the four-way Direction is derived from the
// human decision crossed with that signal. The raw eval report is persisted in
// JudgeScoreJSON so Direction can be recomputed later if the heuristic evolves.
func captureSkillOptJudgeOutcome(ctx context.Context, execer sqlExecer, version AgentTemplateVersion, evalReportJSON string, humanDecision string, reason string) error {
	id, err := newSkillOptJudgeOutcomeID()
	if err != nil {
		return err
	}
	judgeAccept, hasSignal, promptVersion, evaluatorID, promptHash := skillOptJudgeAcceptFromReport(evalReportJSON)
	if !hasSignal {
		// No recognizable judge signal in the eval report (missing/empty/
		// unrecognized): skip rather than record a misleading "judge rejected"
		// outcome that would pollute the agreement dataset. Calibration excludes
		// no-data decisions.
		return nil
	}
	humanPromoted := strings.TrimSpace(humanDecision) == "promoted"
	direction := skillOptJudgeDirection(humanPromoted, judgeAccept)
	_, err = execer.ExecContext(ctx, `INSERT INTO skillopt_judge_outcomes(
			id, candidate_version_id, template_id, judge_score_json, judge_prompt_version,
			judge_evaluator_id, judge_prompt_hash, human_decision, direction, reason
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		strings.TrimSpace(version.ID),
		strings.TrimSpace(version.TemplateID),
		strings.TrimSpace(evalReportJSON),
		promptVersion,
		evaluatorID,
		promptHash,
		strings.TrimSpace(humanDecision),
		direction,
		strings.TrimSpace(reason))
	return err
}

func skillOptJudgeDirection(humanPromoted bool, judgeAccept bool) string {
	switch {
	case humanPromoted && judgeAccept:
		return SkillOptJudgeDirectionAgreeAccept
	case !humanPromoted && !judgeAccept:
		return SkillOptJudgeDirectionAgreeReject
	case !humanPromoted && judgeAccept:
		return SkillOptJudgeDirectionJudgeAcceptHumanReject
	default: // humanPromoted && !judgeAccept
		return SkillOptJudgeDirectionJudgeRejectHumanAccept
	}
}

// skillOptJudgeAcceptFromReport derives a judge accept/reject signal plus the
// judge prompt/evaluator identity from a candidate's eval report JSON, reading
// everything generically (map[string]any) so it does not depend on any typed
// struct fields that may be absent on older reports. The eval report may carry
// the judge fields at the top level or nested under "evaluator_score" (the
// EvaluatorScore object in internal/skillopt/contract.go), so both locations
// are inspected.
//
// Judge-accept heuristic (most authoritative field wins):
//  1. explicit boolean "promotable" — true => accept, false => reject;
//  2. a recommendation string ("recommendation"/"recommended_action"):
//     "promote"/"accept"/"approve"/"pass" => accept, "reject"/"decline"/"fail"
//     => reject;
//  3. a quality/contract status string ("quality_status"/"contract_status"):
//     "pass"/"passed"/"promote"/"ok"/"accept"/"approved" => accept,
//     "fail"/"failed"/"reject"/"rejected" => reject (other statuses like
//     "not_run" are inconclusive and skipped);
//  4. fall back to a soft/selection score — "soft", or the landing-page
//     profile's "best_selection_soft"/"best_selection_hard" — first present
//     wins: score >= 0.5 => accept.
//
// hasSignal reports whether any of the above produced a verdict. When it is
// false (missing/empty/unrecognized report), callers should SKIP recording the
// outcome rather than treat the absence of data as a "judge rejected" verdict,
// which would pollute the calibration dataset.
func skillOptJudgeAcceptFromReport(evalReportJSON string) (accept bool, hasSignal bool, promptVersion string, evaluatorID string, promptHash string) {
	evalReportJSON = strings.TrimSpace(evalReportJSON)
	if evalReportJSON == "" {
		return false, false, "", "", ""
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(evalReportJSON), &report); err != nil {
		return false, false, "", "", ""
	}
	// Search the report root and the nested evaluator_score object, preferring
	// whichever supplies the most authoritative signal.
	sources := []map[string]any{report}
	if nested, ok := report["evaluator_score"].(map[string]any); ok {
		sources = append(sources, nested)
	}

	promptVersion = firstSkillOptJudgeString(sources, "judge_prompt_version", "prompt_version")
	evaluatorID = firstSkillOptJudgeString(sources, "judge_evaluator_id", "evaluator_id", "profile_id")
	promptHash = firstSkillOptJudgeString(sources, "judge_prompt_hash", "prompt_hash")

	// 1) explicit promotable boolean.
	for _, source := range sources {
		if value, ok := source["promotable"].(bool); ok {
			return value, true, promptVersion, evaluatorID, promptHash
		}
	}
	// 2) explicit recommendation.
	if recommendation := firstSkillOptJudgeString(sources, "recommendation", "recommended_action"); recommendation != "" {
		switch strings.ToLower(recommendation) {
		case "promote", "accept", "approve", "pass":
			return true, true, promptVersion, evaluatorID, promptHash
		case "reject", "decline", "fail":
			return false, true, promptVersion, evaluatorID, promptHash
		}
	}
	// 3) quality / contract status.
	if status := firstSkillOptJudgeString(sources, "quality_status", "contract_status"); status != "" {
		switch strings.ToLower(status) {
		case "pass", "passed", "promote", "ok", "accept", "approved":
			return true, true, promptVersion, evaluatorID, promptHash
		case "fail", "failed", "reject", "rejected":
			return false, true, promptVersion, evaluatorID, promptHash
		}
	}
	// 4) soft/selection-score fallback. Real optimizer reports vary by evaluator
	// profile: the generic profile sets a top-level "promotable" (handled above);
	// the landing-page profile instead reports the best candidate's selection-gate
	// scores ("best_selection_soft"/"best_selection_hard") with no "promotable".
	// Treat the first such score present as the judge's confidence: >= 0.5 => accept.
	for _, source := range sources {
		for _, key := range []string{"soft", "best_selection_soft", "best_selection_hard"} {
			if score, ok := skillOptJudgeFloat(source[key]); ok {
				return score >= 0.5, true, promptVersion, evaluatorID, promptHash
			}
		}
	}
	return false, false, promptVersion, evaluatorID, promptHash
}

func firstSkillOptJudgeString(sources []map[string]any, keys ...string) string {
	for _, source := range sources {
		for _, key := range keys {
			if value, ok := source[key].(string); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func skillOptJudgeFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
