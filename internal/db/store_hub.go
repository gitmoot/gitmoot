package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// HubMachine is a machine allowed to call the hub API (#2710): the holder of
// the client certificate whose SHA-256 fingerprint (lowercase hex) is
// Fingerprint, acting as one of Roles.
type HubMachine struct {
	ID          int64
	Name        string
	Fingerprint string
	Roles       []string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   time.Time
}

// ErrHubMachineExists reports an unrevoked machine with the same name.
var ErrHubMachineExists = errors.New("an unrevoked hub machine with this name exists; revoke it first")

const hubMachineColumns = `id, name, fingerprint, created_at, expires_at, revoked_at`

// AddHubMachine records a machine and its roles in one transaction.
func (s *Store) AddHubMachine(ctx context.Context, machine HubMachine) (HubMachine, error) {
	machine.Name = strings.TrimSpace(machine.Name)
	if machine.Name == "" || len(machine.Fingerprint) != 64 || len(machine.Roles) == 0 {
		return HubMachine{}, errors.New("hub machine needs a name, a SHA-256 fingerprint and at least one role")
	}
	roles := slices.Sorted(slices.Values(machine.Roles))
	roles = slices.Compact(roles)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HubMachine{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO hub_machines(name, fingerprint, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		machine.Name, machine.Fingerprint, formatWorkspaceTime(machine.CreatedAt), formatWorkspaceTime(machine.ExpiresAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: hub_machines.name") {
			return HubMachine{}, ErrHubMachineExists
		}
		return HubMachine{}, err
	}
	if machine.ID, err = result.LastInsertId(); err != nil {
		return HubMachine{}, err
	}
	for _, role := range roles {
		if strings.TrimSpace(role) == "" {
			return HubMachine{}, errors.New("hub machine role is blank")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_machine_roles(machine_id, role) VALUES (?, ?)`, machine.ID, role); err != nil {
			return HubMachine{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return HubMachine{}, err
	}
	machine.Roles = roles
	return machine, nil
}

// ActiveHubMachineByFingerprint returns the unrevoked, unexpired machine whose
// certificate has this fingerprint, or sql.ErrNoRows.
func (s *Store) ActiveHubMachineByFingerprint(ctx context.Context, fingerprint string, now time.Time) (HubMachine, error) {
	machines, err := s.queryHubMachines(ctx, `SELECT `+hubMachineColumns+` FROM hub_machines
		WHERE fingerprint = ? AND revoked_at = '' AND expires_at > ?`, fingerprint, formatWorkspaceTime(now))
	if err != nil {
		return HubMachine{}, err
	}
	if len(machines) == 0 {
		return HubMachine{}, sql.ErrNoRows
	}
	return machines[0], nil
}

// ListHubMachines returns every machine, oldest first, revoked ones only when
// includeRevoked is set.
func (s *Store) ListHubMachines(ctx context.Context, includeRevoked bool) ([]HubMachine, error) {
	query := `SELECT ` + hubMachineColumns + ` FROM hub_machines`
	if !includeRevoked {
		query += ` WHERE revoked_at = ''`
	}
	return s.queryHubMachines(ctx, query+` ORDER BY id`)
}

// RevokeHubMachine revokes the unrevoked machine with this name and returns
// it, or sql.ErrNoRows when there is none. Its certificate stops working on
// the next request, including on a connection already open.
func (s *Store) RevokeHubMachine(ctx context.Context, name string, now time.Time) (HubMachine, error) {
	machines, err := s.queryHubMachines(ctx, `UPDATE hub_machines SET revoked_at = ?
		WHERE name = ? AND revoked_at = '' RETURNING `+hubMachineColumns, formatWorkspaceTime(now), strings.TrimSpace(name))
	if err != nil {
		return HubMachine{}, err
	}
	if len(machines) == 0 {
		return HubMachine{}, sql.ErrNoRows
	}
	return machines[0], nil
}

// HubWriteOrigin names the machine and the action of a write made through the
// hub API; the role is the write's own acting role.
type HubWriteOrigin struct {
	MachineID int64
	Action    string
}

// insertHubWrite records a hub write inside the transaction that makes it.
func insertHubWrite(ctx context.Context, tx *sql.Tx, machineID int64, role, action, ref string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO hub_writes(created_at, machine_id, role, action, ref) VALUES (?, ?, ?, ?, ?)`,
		formatWorkspaceTime(now), machineID, role, action, ref)
	return err
}

func (s *Store) queryHubMachines(ctx context.Context, query string, args ...any) ([]HubMachine, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var machines []HubMachine
	for rows.Next() {
		var machine HubMachine
		var created, expires, revoked string
		if err := rows.Scan(&machine.ID, &machine.Name, &machine.Fingerprint, &created, &expires, &revoked); err != nil {
			rows.Close()
			return nil, err
		}
		for target, value := range map[*time.Time]string{&machine.CreatedAt: created, &machine.ExpiresAt: expires, &machine.RevokedAt: revoked} {
			if *target, err = parseWorkspaceTime(value); err != nil {
				rows.Close()
				return nil, fmt.Errorf("hub machine %d: %w", machine.ID, err)
			}
		}
		machines = append(machines, machine)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range machines {
		roles, err := s.hubMachineRoles(ctx, machines[index].ID)
		if err != nil {
			return nil, err
		}
		machines[index].Roles = roles
	}
	return machines, nil
}

func (s *Store) hubMachineRoles(ctx context.Context, machineID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role FROM hub_machine_roles WHERE machine_id = ? ORDER BY role`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

// RecordHubWrite records a hub write that cannot share a transaction with
// its action (a review request, a level decision, a notification outcome).
// Callers record it BEFORE acting and refuse the request if it fails, so a
// remote write never exists without attribution; at worst a row names an
// attempt that then failed.
func (s *Store) RecordHubWrite(ctx context.Context, machineID int64, role, action, ref string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertHubWrite(ctx, tx, machineID, role, action, ref, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrHubAttemptStale reports a hub finish whose attempt is no longer the
// row's current one: the row was finished, deferred or claimed again since.
var ErrHubAttemptStale = errors.New("notification attempt is no longer current")

// HubAttemptToken names one delivery attempt of a wake row: the attempted_at
// stamp its claim wrote. A deferral clears attempted_at and a new claim writes
// a new stamp, so a token never matches a later attempt of the same row
// (attempt_count cannot serve: a deferral decrements it).
func HubAttemptToken(claimedAt time.Time) string {
	return claimedAt.UTC().Format(BlockedEpisodeTimeLayout)
}

// FinishHubWakeAttempt records a remote client's outcome for exactly the
// attempt named by token, on a row addressed to role (#2710). delivered
// finishes it with detail as the receipt; deferred returns it to pending with
// detail as the reason. Anything else, or a stale token, changes nothing.
func (s *Store) FinishHubWakeAttempt(ctx context.Context, role string, id int64, token string, delivered bool, detail string, now time.Time) error {
	stamp := HubAttemptToken(now)
	query := `UPDATE wake_outbox
SET state = 'delivered', coalesced_into = NULL, last_error = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND lower(target_role) = ? AND state = 'attempted' AND attempted_at = ?`
	args := []any{strings.TrimSpace(detail), stamp, stamp, id, strings.ToLower(strings.TrimSpace(role)), token}
	if !delivered {
		query = `UPDATE wake_outbox
SET state = 'pending', coalesced_into = NULL, last_error = ?, attempted_at = NULL, finished_at = NULL,
	attempt_count = attempt_count - 1, updated_at = ?
WHERE id = ? AND lower(target_role) = ? AND state = 'attempted' AND attempt_count > 0 AND attempted_at = ?`
		args = []any{strings.TrimSpace(detail), stamp, id, strings.ToLower(strings.TrimSpace(role)), token}
	}
	writeCtx, cancel := s.durableWriteContext(ctx, 1)
	defer cancel()
	return retryNotificationWrite(writeCtx, func() error {
		result, err := s.db.ExecContext(writeCtx, query, args...)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrHubAttemptStale
		}
		return nil
	})
}

// wakeOutboxCreatedLayout matches strftime('%Y-%m-%dT%H:%M:%fZ'), the form
// wake_outbox.created_at is written in, so the two compare as strings.
const wakeOutboxCreatedLayout = "2006-01-02T15:04:05.000Z"

// RetireWakeBacklogForRole supersedes role's pending notifications created
// before cutoff, recording detail, and reports how many it retired and how
// many pending ones it kept. The messages themselves are untouched and stay
// readable in the role's inbox; only their delivery is retired. Attempted
// rows are left alone: their outcome is in flight (#2721: a role bound to a
// new agent must not have weeks of stale mail pushed into it, and a routine
// re-bind must not drop fresh work, so the caller picks the cutoff).
func (s *Store) RetireWakeBacklogForRole(ctx context.Context, role string, cutoff, now time.Time, detail string) (retired, kept int64, err error) {
	stamp := now.UTC().Format(BlockedEpisodeTimeLayout)
	role = strings.ToLower(strings.TrimSpace(role))
	writeCtx, cancel := s.durableWriteContext(ctx, 2)
	defer cancel()
	err = retryNotificationWrite(writeCtx, func() error {
		tx, err := s.db.BeginTx(writeCtx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(writeCtx, `UPDATE wake_outbox
SET state = ?, coalesced_into = NULL, last_error = ?, finished_at = ?, updated_at = ?
WHERE lower(target_role) = ? AND state = 'pending' AND created_at < ?`,
			WakeOutboxStateSuperseded, strings.TrimSpace(detail), stamp, stamp,
			role, cutoff.UTC().Format(wakeOutboxCreatedLayout))
		if err != nil {
			return err
		}
		if retired, err = result.RowsAffected(); err != nil {
			return err
		}
		if err := tx.QueryRowContext(writeCtx, `SELECT count(*) FROM wake_outbox WHERE lower(target_role) = ? AND state = 'pending'`, role).Scan(&kept); err != nil {
			return err
		}
		return tx.Commit()
	})
	return retired, kept, err
}
