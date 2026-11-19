package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Workspace states (#2678). opening, ready and closing hold a provider
// sandbox (or may); closed and failed are terminal and hold none.
const (
	WorkspaceStateOpening = "opening"
	WorkspaceStateReady   = "ready"
	WorkspaceStateClosing = "closing"
	WorkspaceStateClosed  = "closed"
	WorkspaceStateFailed  = "failed"
)

// ErrWorkspaceNotFound reports an unknown workspace id.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// WorkspaceAlreadyOpenError reports the one-open-workspace rule: the seat
// already has a non-terminal workspace for this repo and issue or PR.
type WorkspaceAlreadyOpenError struct{ ID string }

func (e *WorkspaceAlreadyOpenError) Error() string {
	return fmt.Sprintf("workspace %s is already open for this seat, repo and issue/PR", e.ID)
}

// WorkspaceCostCapError is a refused reservation. Clause names the cap:
// "total" for max_total_usd, "workspace" for max_workspace_usd.
type WorkspaceCostCapError struct {
	Clause       string
	CommittedUSD float64
	RequestUSD   float64
	LimitUSD     float64
}

func (e *WorkspaceCostCapError) Error() string {
	if e.Clause == "workspace" {
		return fmt.Sprintf("workspace cost cap: reserving $%.2f more would pass this workspace's max_workspace_usd $%.2f (reserved $%.2f)", e.RequestUSD, e.LimitUSD, e.CommittedUSD)
	}
	return fmt.Sprintf("workspace cost cap: reserving $%.2f more would pass [workspaces].max_total_usd $%.2f (committed $%.2f)", e.RequestUSD, e.LimitUSD, e.CommittedUSD)
}

// Workspace is one row of the workspaces table. Times are UTC RFC 3339.
type Workspace struct {
	ID                 string
	Seat               string
	Repo               string
	Issue              int64
	PR                 int64
	Branch             string
	Base               string
	BaseRef            string
	Provider           string
	ProviderInstanceID string
	Root               string
	State              string
	CreatedAt          time.Time
	LastActiveAt       time.Time
	ClosedAt           time.Time
	CloseReason        string
	RateUSDPerHour     float64
	CostReservedUSD    float64
	CostAccruedUSD     float64
	PaidUntil          time.Time
	LinkedPR           int64
}

// Active reports whether the workspace may still hold a provider sandbox.
func (w Workspace) Active() bool {
	return w.State == WorkspaceStateOpening || w.State == WorkspaceStateReady || w.State == WorkspaceStateClosing
}

const workspaceColumns = `id, seat, repo, issue, pr, branch, base, base_ref, provider, provider_instance_id,
	root, state, created_at, last_active_at, closed_at, close_reason, cost_rate_usd_per_hour,
	cost_reserved_usd, cost_accrued_usd, paid_until, linked_pr`

// workspaceCommittedSQL is every workspace's spend that is already certain:
// a terminal workspace's settled cost and an active one's whole reservation.
const workspaceCommittedSQL = `SELECT COALESCE(SUM(CASE WHEN state IN ('closed', 'failed') THEN cost_accrued_usd ELSE cost_reserved_usd END), 0) FROM workspaces`

// workspaceCapEpsilon absorbs float rounding in the cap predicates, so a
// reservation that lands exactly on the cap is admitted.
const workspaceCapEpsilon = 1e-9

// workspaceTimeLayout is fixed-width UTC, so stored times compare correctly
// as text (TouchWorkspace's never-backwards predicate relies on it).
const workspaceTimeLayout = "2006-01-02T15:04:05.000000000Z"

func formatWorkspaceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(workspaceTimeLayout)
}

func parseWorkspaceTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(workspaceTimeLayout, value)
}

// CreateWorkspace inserts an opening workspace together with its first
// reservation. THE GLOBAL CAP IS ENFORCED IN THIS STATEMENT: the predicate
// rides the INSERT, so two concurrent opens cannot both see room. Zero rows
// is the refusal; a unique-index violation is the one-open-workspace rule.
func (s *Store) CreateWorkspace(ctx context.Context, w Workspace, maxTotalUSD float64) error {
	if strings.TrimSpace(w.ID) == "" || strings.TrimSpace(w.Seat) == "" || strings.TrimSpace(w.Repo) == "" {
		return errors.New("workspace id, seat and repo are required")
	}
	if (w.Issue > 0) == (w.PR > 0) {
		return errors.New("workspace needs exactly one of issue or pr")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO workspaces(`+workspaceColumns+`)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, 0, ?, 0
		WHERE (`+workspaceCommittedSQL+`) + ? <= ? + ?`,
		w.ID, w.Seat, w.Repo, w.Issue, w.PR, w.Branch, w.Base, w.BaseRef, w.Provider, w.ProviderInstanceID,
		w.Root, WorkspaceStateOpening, formatWorkspaceTime(w.CreatedAt), formatWorkspaceTime(w.LastActiveAt),
		w.RateUSDPerHour, w.CostReservedUSD, formatWorkspaceTime(w.PaidUntil),
		w.CostReservedUSD, maxTotalUSD, workspaceCapEpsilon)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: workspaces.seat") {
			existing, findErr := s.FindActiveWorkspace(ctx, w.Seat, w.Repo, w.Issue, w.PR)
			if findErr != nil {
				return errors.Join(err, findErr)
			}
			return &WorkspaceAlreadyOpenError{ID: existing.ID}
		}
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	committed, err := s.WorkspaceCommittedUSD(ctx)
	if err != nil {
		return err
	}
	return &WorkspaceCostCapError{Clause: "total", CommittedUSD: committed, RequestUSD: w.CostReservedUSD, LimitUSD: maxTotalUSD}
}

// WorkspaceCommittedUSD is the spend max_total_usd is measured against.
func (s *Store) WorkspaceCommittedUSD(ctx context.Context) (float64, error) {
	var committed float64
	err := s.db.QueryRowContext(ctx, workspaceCommittedSQL).Scan(&committed)
	return committed, err
}

// GetWorkspace returns one workspace or ErrWorkspaceNotFound.
func (s *Store) GetWorkspace(ctx context.Context, id string) (Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE id = ?`, strings.TrimSpace(id))
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrWorkspaceNotFound
	}
	return w, err
}

// FindActiveWorkspace returns the seat's non-terminal workspace for repo and
// issue or PR, or ErrWorkspaceNotFound.
func (s *Store) FindActiveWorkspace(ctx context.Context, seat, repo string, issue, pr int64) (Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces
		WHERE seat = ? AND repo = ? AND issue = ? AND pr = ? AND state IN ('opening', 'ready', 'closing')`, seat, repo, issue, pr)
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrWorkspaceNotFound
	}
	return w, err
}

// ListWorkspaces returns workspaces newest first; terminal ones only when
// includeTerminal is set.
func (s *Store) ListWorkspaces(ctx context.Context, includeTerminal bool) ([]Workspace, error) {
	query := `SELECT ` + workspaceColumns + ` FROM workspaces`
	if !includeTerminal {
		query += ` WHERE state IN ('opening', 'ready', 'closing')`
	}
	query += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workspaces []Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

// SetWorkspaceInstance records the provider sandbox of an opening workspace
// as soon as the provider returns it, so a crash after create still leaves
// the daemon a handle to destroy.
func (s *Store) SetWorkspaceInstance(ctx context.Context, id, instanceID, root string) error {
	return s.updateOneWorkspace(ctx, `UPDATE workspaces SET provider_instance_id = ?, root = ? WHERE id = ? AND state = 'opening'`, instanceID, root, id)
}

// MarkWorkspaceReady finishes open: the branch is checked out at base.
func (s *Store) MarkWorkspaceReady(ctx context.Context, id, branch, base, baseRef string, now time.Time) error {
	return s.updateOneWorkspace(ctx, `UPDATE workspaces SET state = 'ready', branch = ?, base = ?, base_ref = ?, last_active_at = ?
		WHERE id = ? AND state = 'opening'`, branch, base, baseRef, formatWorkspaceTime(now), id)
}

// TouchWorkspace moves last_active_at forward (never back) on a ready
// workspace; the idle timeout is measured from it.
func (s *Store) TouchWorkspace(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspaces SET last_active_at = ?
		WHERE id = ? AND state = 'ready' AND last_active_at < ?`, formatWorkspaceTime(now), id, formatWorkspaceTime(now))
	return err
}

// SetWorkspaceLinkedPR records the pull request an issue workspace's branch
// was opened as.
func (s *Store) SetWorkspaceLinkedPR(ctx context.Context, id string, pr int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspaces SET linked_pr = ? WHERE id = ? AND issue > 0`, pr, id)
	return err
}

// ExtendWorkspaceReservation settles a ready workspace's accrued cost and
// reserves one more period of addUSD ending at paidUntil. Both caps ride the
// UPDATE, so the refusal and the reservation are one decision.
func (s *Store) ExtendWorkspaceReservation(ctx context.Context, id string, addUSD float64, paidUntil time.Time, accruedUSD, maxTotalUSD, maxWorkspaceUSD float64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE workspaces
		SET cost_reserved_usd = cost_reserved_usd + ?, paid_until = ?, cost_accrued_usd = ?
		WHERE id = ? AND state = 'ready'
		AND cost_reserved_usd + ? <= ? + ?
		AND (`+workspaceCommittedSQL+`) + ? <= ? + ?`,
		addUSD, formatWorkspaceTime(paidUntil), accruedUSD,
		id, addUSD, maxWorkspaceUSD, workspaceCapEpsilon,
		addUSD, maxTotalUSD, workspaceCapEpsilon)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 1 {
		return err
	}
	w, err := s.GetWorkspace(ctx, id)
	if err != nil {
		return err
	}
	if w.State != WorkspaceStateReady {
		return fmt.Errorf("workspace %s is %s, not ready", id, w.State)
	}
	if w.CostReservedUSD+addUSD > maxWorkspaceUSD+workspaceCapEpsilon {
		return &WorkspaceCostCapError{Clause: "workspace", CommittedUSD: w.CostReservedUSD, RequestUSD: addUSD, LimitUSD: maxWorkspaceUSD}
	}
	committed, err := s.WorkspaceCommittedUSD(ctx)
	if err != nil {
		return err
	}
	return &WorkspaceCostCapError{Clause: "total", CommittedUSD: committed, RequestUSD: addUSD, LimitUSD: maxTotalUSD}
}

// BeginWorkspaceClose moves an opening or ready workspace to closing and
// records why. It reports false when the workspace was already closing or
// terminal; the first recorded reason wins.
func (s *Store) BeginWorkspaceClose(ctx context.Context, id, reason string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE workspaces SET state = 'closing', close_reason = ?
		WHERE id = ? AND state IN ('opening', 'ready')`, reason, id)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// FinishWorkspaceClose settles a closing workspace into closed or failed with
// its final accrued cost. Only a closing workspace can finish.
func (s *Store) FinishWorkspaceClose(ctx context.Context, id, state string, accruedUSD float64, closedAt time.Time) error {
	if state != WorkspaceStateClosed && state != WorkspaceStateFailed {
		return fmt.Errorf("workspace close state %q is not terminal", state)
	}
	return s.updateOneWorkspace(ctx, `UPDATE workspaces SET state = ?, cost_accrued_usd = ?, closed_at = ?
		WHERE id = ? AND state = 'closing'`, state, accruedUSD, formatWorkspaceTime(closedAt), id)
}

func (s *Store) updateOneWorkspace(ctx context.Context, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("workspace %v was not in the expected state", args[len(args)-1])
	}
	return nil
}

func scanWorkspace(row interface{ Scan(...any) error }) (Workspace, error) {
	var w Workspace
	var created, lastActive, closed, paidUntil string
	if err := row.Scan(&w.ID, &w.Seat, &w.Repo, &w.Issue, &w.PR, &w.Branch, &w.Base, &w.BaseRef, &w.Provider,
		&w.ProviderInstanceID, &w.Root, &w.State, &created, &lastActive, &closed, &w.CloseReason,
		&w.RateUSDPerHour, &w.CostReservedUSD, &w.CostAccruedUSD, &paidUntil, &w.LinkedPR); err != nil {
		return Workspace{}, err
	}
	var errs []error
	var err error
	if w.CreatedAt, err = parseWorkspaceTime(created); err != nil {
		errs = append(errs, err)
	}
	if w.LastActiveAt, err = parseWorkspaceTime(lastActive); err != nil {
		errs = append(errs, err)
	}
	if w.ClosedAt, err = parseWorkspaceTime(closed); err != nil {
		errs = append(errs, err)
	}
	if w.PaidUntil, err = parseWorkspaceTime(paidUntil); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return Workspace{}, fmt.Errorf("workspace %s has an unreadable time: %w", w.ID, err)
	}
	return w, nil
}
