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

// WakeOutboxRowsTargetRole reports whether every id names an existing
// wake_outbox row addressed to role. target_role never changes, so the answer
// stays true for the later finish, whose own WHERE clause checks the state.
func (s *Store) WakeOutboxRowsTargetRole(ctx context.Context, role string, ids []int64) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, strings.ToLower(strings.TrimSpace(role)))
	for _, id := range ids {
		args = append(args, id)
	}
	var matched int
	if err := s.db.QueryRowContext(ctx, `SELECT count(DISTINCT id) FROM wake_outbox WHERE lower(target_role) = ? AND id IN (`+placeholders+`)`, args...).Scan(&matched); err != nil {
		return false, err
	}
	return matched == len(slices.Compact(slices.Sorted(slices.Values(ids)))), nil
}
