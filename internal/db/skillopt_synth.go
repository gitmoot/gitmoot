package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// SkillOpt synth item statuses (#535). A freshly-generated, accepted item is
// ALWAYS created pending_human_approval: the human gate is the load-bearing
// governance boundary. Nothing in the promotion/training path reads this table,
// so a pending row is structurally incapable of affecting a promotion until an
// operator explicitly flips it with `gitmoot skillopt synth approve`/`reject`.
const (
	SynthItemStatusPending  = "pending_human_approval"
	SynthItemStatusApproved = "approved"
	SynthItemStatusRejected = "rejected"
)

// SynthReviewItem is one Autodata-style synthetic SkillOpt review item (#535):
// a Challenger-generated {Context, Question, Rubric} triple that a weak agent
// struggled on, a strong agent solved, and a judge confirmed is well-formed and
// discriminating. Only ACCEPTED items become rows (skipped/rejected candidates
// are logged with their Diagnostic, never persisted). Rows are created
// pending_human_approval and only ever moved to approved/rejected by the human
// gate — they never enter any training/review pool automatically.
type SynthReviewItem struct {
	ID           string
	TemplateID   string
	Repo         string
	Status       string
	Context      string
	Question     string
	Rubric       string
	WeakAgent    string
	StrongAgent  string
	JudgeAgent   string
	WeakAnswer   string
	StrongAnswer string
	WeakScore    float64
	StrongScore  float64
	Gap          float64
	Rounds       int
	Diagnostic   string
	OutPath      string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateSynthReviewItem inserts one accepted synth item. The caller supplies the
// id (a deterministic synth item id); a duplicate id is an error so a
// double-create surfaces rather than silently overwriting. A blank status
// defaults to pending_human_approval — the only status a freshly-generated item
// may hold.
func (s *Store) CreateSynthReviewItem(ctx context.Context, item SynthReviewItem) error {
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		return errors.New("synth review item id is required")
	}
	if strings.TrimSpace(item.Status) == "" {
		item.Status = SynthItemStatusPending
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO skillopt_synth_items(
		id, template_id, repo, status, context, question, rubric,
		weak_agent, strong_agent, judge_agent, weak_answer, strong_answer,
		weak_score, strong_score, gap, rounds, diagnostic, out_path, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, strings.TrimSpace(item.TemplateID), strings.TrimSpace(item.Repo), strings.TrimSpace(item.Status),
		item.Context, item.Question, item.Rubric,
		strings.TrimSpace(item.WeakAgent), strings.TrimSpace(item.StrongAgent), strings.TrimSpace(item.JudgeAgent),
		item.WeakAnswer, item.StrongAnswer, item.WeakScore, item.StrongScore, item.Gap, item.Rounds,
		strings.TrimSpace(item.Diagnostic), strings.TrimSpace(item.OutPath), now, now)
	return err
}

// GetSynthReviewItem returns one synth item by id. A missing row is NOT an error:
// it returns ok=false so callers can print a friendly "not found".
func (s *Store) GetSynthReviewItem(ctx context.Context, id string) (SynthReviewItem, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return SynthReviewItem{}, false, errors.New("synth review item id is required")
	}
	row := s.db.QueryRowContext(ctx, synthReviewItemSelect+` WHERE id = ?`, id)
	item, err := scanSynthReviewItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SynthReviewItem{}, false, nil
	}
	if err != nil {
		return SynthReviewItem{}, false, err
	}
	return item, true, nil
}

// ListSynthReviewItems returns synth items, newest first. When status is
// non-empty only rows with that status are returned (e.g. only pending items
// awaiting the human gate).
func (s *Store) ListSynthReviewItems(ctx context.Context, status string) ([]SynthReviewItem, error) {
	status = strings.TrimSpace(status)
	var (
		rows *sql.Rows
		err  error
	)
	if status == "" {
		rows, err = s.db.QueryContext(ctx, synthReviewItemSelect+` ORDER BY created_at DESC, id DESC`)
	} else {
		rows, err = s.db.QueryContext(ctx, synthReviewItemSelect+` WHERE status = ? ORDER BY created_at DESC, id DESC`, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []SynthReviewItem
	for rows.Next() {
		item, err := scanSynthReviewItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SetSynthReviewItemStatus flips a synth item's status (the human gate:
// approve/reject). It returns an error if no row matched the id so a lost row
// surfaces rather than silently no-op'ing. This is the ONLY mutation on a synth
// item after creation — its content is immutable.
func (s *Store) SetSynthReviewItemStatus(ctx context.Context, id, status string) error {
	id = strings.TrimSpace(id)
	status = strings.TrimSpace(status)
	if id == "" {
		return errors.New("synth review item id is required")
	}
	if status == "" {
		return errors.New("synth review item status is required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE skillopt_synth_items SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("synth review item " + id + " not found")
	}
	return nil
}

const synthReviewItemSelect = `SELECT id, template_id, repo, status, context, question, rubric,
	weak_agent, strong_agent, judge_agent, weak_answer, strong_answer,
	weak_score, strong_score, gap, rounds, diagnostic, out_path, created_at, updated_at
	FROM skillopt_synth_items`

func scanSynthReviewItem(row interface{ Scan(...any) error }) (SynthReviewItem, error) {
	var (
		item               SynthReviewItem
		createdAt, updated string
	)
	if err := row.Scan(&item.ID, &item.TemplateID, &item.Repo, &item.Status, &item.Context, &item.Question, &item.Rubric,
		&item.WeakAgent, &item.StrongAgent, &item.JudgeAgent, &item.WeakAnswer, &item.StrongAnswer,
		&item.WeakScore, &item.StrongScore, &item.Gap, &item.Rounds, &item.Diagnostic, &item.OutPath,
		&createdAt, &updated); err != nil {
		return SynthReviewItem{}, err
	}
	item.CreatedAt = parseSynthTime(createdAt)
	item.UpdatedAt = parseSynthTime(updated)
	return item, nil
}

func parseSynthTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}
