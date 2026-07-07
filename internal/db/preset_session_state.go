package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Preset prompt delivery modes (#33). They are stored verbatim in
// agents.preset_delivery (DEFAULT 'full') and consumed by the workflow decision
// that chooses whether to inline the whole preset or send a short reference.
const (
	// PresetDeliveryFull always inlines the full resolved preset content in the
	// job prompt. It is the default and is byte-identical to pre-#33 behavior.
	PresetDeliveryFull = "full"
	// PresetDeliveryReferenced sends only a short "use your installed preset"
	// reference INSTEAD of the full body, but only when Gitmoot has recorded
	// evidence (a preset_session_state row) that the exact resumed session already
	// received the same preset at the same commit. Any doubt falls back to full.
	PresetDeliveryReferenced = "referenced"
	// PresetDeliveryAuto behaves like referenced but ADDITIONALLY requires the
	// runtime to support persisted sessions (codex/claude); shell/kimi/custom
	// always deliver full under auto.
	PresetDeliveryAuto = "auto"
)

// ValidPresetDeliveryMode reports whether mode is one of the recognized preset
// delivery modes (case-insensitive, trimmed). Empty is NOT valid here; callers
// that accept an unset value should default it to full themselves.
func ValidPresetDeliveryMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case PresetDeliveryFull, PresetDeliveryReferenced, PresetDeliveryAuto:
		return true
	default:
		return false
	}
}

// normalizePresetDeliveryStored coerces any stored/legacy/empty value to a valid
// mode, defaulting to full so an unknown or blank value can never turn the
// optimization on. This is the single normalization chokepoint used on write.
func normalizePresetDeliveryStored(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !ValidPresetDeliveryMode(mode) {
		return PresetDeliveryFull
	}
	return mode
}

// UpdateAgentPresetDelivery re-pins a registered agent's preset delivery mode in
// place, updating only that column (mirrors UpdateAgentRuntimeRef). The mode is
// normalized to a valid value (invalid/blank -> full). It returns an error if no
// agent row matched the name.
func (s *Store) UpdateAgentPresetDelivery(ctx context.Context, name, mode string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE agents SET preset_delivery = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`,
		normalizePresetDeliveryStored(mode), strings.TrimSpace(name))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("agent " + name + " is not registered")
	}
	return nil
}

// RecordPresetSessionState upserts the marker that a given runtime session has
// received a preset at a specific resolved commit (#33). It is called after a
// successful FULL preset delivery so a later referenced/auto delivery on the SAME
// resumed session can send the short reference instead of the whole body.
//
// To enforce the "a preset commit change invalidates state rows for that preset"
// invariant, it first deletes any prior row for the same (runtime, session_id,
// preset_id) tuple regardless of commit, then inserts the current commit — so at
// most one commit is ever recorded per (runtime, session, preset) and a stale
// commit can never satisfy a match. Empty runtime/session/preset are rejected so
// a non-resumable session never records a (meaningless) marker.
func (s *Store) RecordPresetSessionState(ctx context.Context, runtime, sessionID, presetID, presetCommit string) error {
	runtime = strings.TrimSpace(runtime)
	sessionID = strings.TrimSpace(sessionID)
	presetID = strings.TrimSpace(presetID)
	if runtime == "" || sessionID == "" || presetID == "" {
		return errors.New("preset session state requires runtime, session, and preset id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM preset_session_state WHERE runtime = ? AND session_id = ? AND preset_id = ?`,
		runtime, sessionID, presetID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO preset_session_state(runtime, session_id, preset_id, preset_commit, delivered_at)
			VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		runtime, sessionID, presetID, strings.TrimSpace(presetCommit)); err != nil {
		return err
	}
	return tx.Commit()
}

// HasPresetSessionState reports whether an EXACT (runtime, session_id, preset_id,
// preset_commit) marker exists — the precondition the referenced/auto delivery
// decision requires. Any mismatch (different commit, unknown session) returns
// false so the caller falls back to full.
func (s *Store) HasPresetSessionState(ctx context.Context, runtime, sessionID, presetID, presetCommit string) (bool, error) {
	runtime = strings.TrimSpace(runtime)
	sessionID = strings.TrimSpace(sessionID)
	presetID = strings.TrimSpace(presetID)
	if runtime == "" || sessionID == "" || presetID == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM preset_session_state WHERE runtime = ? AND session_id = ? AND preset_id = ? AND preset_commit = ?`,
		runtime, sessionID, presetID, strings.TrimSpace(presetCommit)).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return exists > 0, nil
}

// DeletePresetSessionStateForPreset removes every recorded marker for a preset,
// across all sessions and commits. It exists so a caller that changes a preset's
// resolved content can eagerly invalidate the loaded-state evidence (the
// exact-commit match in HasPresetSessionState already prevents a stale commit
// from matching; this is the eager-cleanup companion). Returns the row count
// removed.
func (s *Store) DeletePresetSessionStateForPreset(ctx context.Context, presetID string) (int64, error) {
	presetID = strings.TrimSpace(presetID)
	if presetID == "" {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM preset_session_state WHERE preset_id = ?`, presetID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
