package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	KeychainModeInjected = "injected"
	KeychainModeProxied  = "proxied"

	KeychainProxyAuthBearer = "bearer"
	KeychainProxyAuthHeader = "header"

	KeychainConsumerPipeline = "pipeline"
)

var (
	ErrKeychainKeyNotFound       = errors.New("keychain key not found")
	ErrKeychainConsumerNotFound  = errors.New("keychain consumer not found")
	ErrKeychainProxyUnconfigured = errors.New("proxied key is not configured")
	ErrKeychainKeyHasGrants      = errors.New("keychain key still has grants")
)

// KeychainKey is value-free registry metadata. Credential bytes never enter
// SQLite; callers load them from the separately validated keychain file.
type KeychainKey struct {
	Name          string `json:"name"`
	Mode          string `json:"mode"`
	ProxyUpstream string `json:"proxy_upstream,omitempty"`
	ProxyAuthKind string `json:"proxy_auth_kind,omitempty"`
	ProxyHeader   string `json:"proxy_header,omitempty"`
	CreatedAt     string `json:"created_at"`
}

func (k KeychainKey) ProxyConfigured() bool {
	if k.Mode != KeychainModeProxied || strings.TrimSpace(k.ProxyUpstream) == "" {
		return false
	}
	if k.ProxyAuthKind == KeychainProxyAuthBearer {
		return strings.TrimSpace(k.ProxyHeader) == ""
	}
	return k.ProxyAuthKind == KeychainProxyAuthHeader && strings.TrimSpace(k.ProxyHeader) != ""
}

// KeychainGrant authorizes one named consumer to use one registered key.
// Pipeline is the only accepted consumer kind in this phase.
type KeychainGrant struct {
	ConsumerKind string `json:"consumer_kind"`
	ConsumerID   string `json:"consumer_id"`
	KeyName      string `json:"key_name"`
	CreatedAt    string `json:"created_at"`
}

func validKeychainMode(mode string) bool {
	return mode == KeychainModeInjected || mode == KeychainModeProxied
}

func (s *Store) AddKeychainKey(ctx context.Context, name, mode string) (KeychainKey, error) {
	name = strings.TrimSpace(name)
	mode = strings.TrimSpace(mode)
	if name == "" {
		return KeychainKey{}, errors.New("keychain key name is required")
	}
	if !validKeychainMode(mode) {
		return KeychainKey{}, fmt.Errorf("invalid keychain mode %q; use injected or proxied", mode)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO keychain_keys(name, mode) VALUES (?, ?)`, name, mode); err != nil {
		return KeychainKey{}, err
	}
	key, found, err := s.GetKeychainKey(ctx, name)
	if err != nil {
		return KeychainKey{}, err
	}
	if !found {
		return KeychainKey{}, errors.New("registered keychain key disappeared")
	}
	return key, nil
}

func (s *Store) GetKeychainKey(ctx context.Context, name string) (KeychainKey, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return KeychainKey{}, false, errors.New("keychain key name is required")
	}
	var key KeychainKey
	err := s.db.QueryRowContext(ctx, `SELECT name, mode,
		COALESCE(proxy_upstream, ''), COALESCE(proxy_auth_kind, ''), COALESCE(proxy_header, ''), created_at
		FROM keychain_keys WHERE name = ?`, name).
		Scan(&key.Name, &key.Mode, &key.ProxyUpstream, &key.ProxyAuthKind, &key.ProxyHeader, &key.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return KeychainKey{}, false, nil
	}
	return key, err == nil, err
}

func (s *Store) ListKeychainKeys(ctx context.Context) ([]KeychainKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, mode,
		COALESCE(proxy_upstream, ''), COALESCE(proxy_auth_kind, ''), COALESCE(proxy_header, ''), created_at
		FROM keychain_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []KeychainKey
	for rows.Next() {
		var key KeychainKey
		if err := rows.Scan(&key.Name, &key.Mode, &key.ProxyUpstream, &key.ProxyAuthKind, &key.ProxyHeader, &key.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) ConfigureKeychainProxy(ctx context.Context, name, upstream, authKind, header string) (KeychainKey, error) {
	name = strings.TrimSpace(name)
	upstream = strings.TrimSpace(upstream)
	authKind = strings.TrimSpace(authKind)
	header = strings.TrimSpace(header)
	if name == "" || upstream == "" {
		return KeychainKey{}, errors.New("keychain proxy configuration requires a key name and upstream")
	}
	if authKind != KeychainProxyAuthBearer && authKind != KeychainProxyAuthHeader {
		return KeychainKey{}, fmt.Errorf("invalid proxy auth kind %q", authKind)
	}
	if authKind == KeychainProxyAuthBearer && header != "" {
		return KeychainKey{}, errors.New("bearer proxy auth cannot set a header name")
	}
	if authKind == KeychainProxyAuthHeader && header == "" {
		return KeychainKey{}, errors.New("header proxy auth requires a header name")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE keychain_keys
		SET proxy_upstream = ?, proxy_auth_kind = ?, proxy_header = ?
		WHERE name = ? AND mode = ?`, upstream, authKind, nullableProxyHeader(header), name, KeychainModeProxied)
	if err != nil {
		return KeychainKey{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return KeychainKey{}, err
	}
	if affected == 0 {
		key, found, getErr := s.GetKeychainKey(ctx, name)
		if getErr != nil {
			return KeychainKey{}, getErr
		}
		if !found {
			return KeychainKey{}, ErrKeychainKeyNotFound
		}
		return KeychainKey{}, fmt.Errorf("key %s uses %s mode; proxy configuration requires proxied mode", name, key.Mode)
	}
	key, found, err := s.GetKeychainKey(ctx, name)
	if err != nil {
		return KeychainKey{}, err
	}
	if !found {
		return KeychainKey{}, ErrKeychainKeyNotFound
	}
	return key, nil
}

func nullableProxyHeader(header string) any {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	return header
}

func (s *Store) ListKeychainGrants(ctx context.Context, keyName string) ([]KeychainGrant, error) {
	query := `SELECT consumer_kind, consumer_id, key_name, created_at FROM keychain_grants`
	var args []any
	if keyName = strings.TrimSpace(keyName); keyName != "" {
		query += ` WHERE key_name = ?`
		args = append(args, keyName)
	}
	query += ` ORDER BY key_name, consumer_kind, consumer_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []KeychainGrant
	for rows.Next() {
		var grant KeychainGrant
		if err := rows.Scan(&grant.ConsumerKind, &grant.ConsumerID, &grant.KeyName, &grant.CreatedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *Store) ListKeychainGrantsForConsumer(ctx context.Context, kind, id string) ([]KeychainGrant, error) {
	kind = strings.TrimSpace(kind)
	id = strings.TrimSpace(id)
	if kind != KeychainConsumerPipeline || id == "" {
		return nil, errors.New("keychain consumer must be a named pipeline")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT consumer_kind, consumer_id, key_name, created_at
		FROM keychain_grants WHERE consumer_kind = ? AND consumer_id = ? ORDER BY key_name`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []KeychainGrant
	for rows.Next() {
		var grant KeychainGrant
		if err := rows.Scan(&grant.ConsumerKind, &grant.ConsumerID, &grant.KeyName, &grant.CreatedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// GetGrantedKey checks grant existence and current registry mode in one query.
// Delivery uses this immediately before loading the value to fail closed after
// a revoke or metadata change.
func (s *Store) GetGrantedKey(ctx context.Context, kind, id, name string) (KeychainKey, bool, error) {
	kind = strings.TrimSpace(kind)
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if kind != KeychainConsumerPipeline || id == "" || name == "" {
		return KeychainKey{}, false, errors.New("keychain grant requires pipeline and key names")
	}
	var key KeychainKey
	err := s.db.QueryRowContext(ctx, `SELECT k.name, k.mode,
		COALESCE(k.proxy_upstream, ''), COALESCE(k.proxy_auth_kind, ''), COALESCE(k.proxy_header, ''), k.created_at
		FROM keychain_keys k
		JOIN keychain_grants g ON g.key_name = k.name
		WHERE g.consumer_kind = ? AND g.consumer_id = ? AND g.key_name = ?`, kind, id, name).
		Scan(&key.Name, &key.Mode, &key.ProxyUpstream, &key.ProxyAuthKind, &key.ProxyHeader, &key.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return KeychainKey{}, false, nil
	}
	return key, err == nil, err
}

func (s *Store) GrantKeychainKey(ctx context.Context, kind, id, name string) (bool, error) {
	kind = strings.TrimSpace(kind)
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if kind != KeychainConsumerPipeline || id == "" || name == "" {
		return false, errors.New("keychain grant requires a named pipeline and key")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipelines WHERE name = ?`, id).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, ErrKeychainConsumerNotFound
	}
	var key KeychainKey
	if err := tx.QueryRowContext(ctx, `SELECT name, mode,
		COALESCE(proxy_upstream, ''), COALESCE(proxy_auth_kind, ''), COALESCE(proxy_header, ''), created_at
		FROM keychain_keys WHERE name = ?`, name).
		Scan(&key.Name, &key.Mode, &key.ProxyUpstream, &key.ProxyAuthKind, &key.ProxyHeader, &key.CreatedAt); errors.Is(err, sql.ErrNoRows) {
		return false, ErrKeychainKeyNotFound
	} else if err != nil {
		return false, err
	}
	if key.Mode == KeychainModeProxied && !key.ProxyConfigured() {
		return false, ErrKeychainProxyUnconfigured
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO keychain_grants(consumer_kind, consumer_id, key_name) VALUES (?, ?, ?)`, kind, id, name)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) RevokeKeychainKey(ctx context.Context, kind, id, name string) (bool, error) {
	kind = strings.TrimSpace(kind)
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if kind != KeychainConsumerPipeline || id == "" || name == "" {
		return false, errors.New("keychain revoke requires a named pipeline and key")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM keychain_grants WHERE consumer_kind = ? AND consumer_id = ? AND key_name = ?`, kind, id, name)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// RemoveKeychainKey removes metadata only. The operator-owned keychain file is
// deliberately untouched. Forced removal deletes grants in the same transaction.
func (s *Store) RemoveKeychainKey(ctx context.Context, name string, force bool) (removed bool, grantsRemoved int, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, 0, errors.New("keychain key name is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM keychain_grants WHERE key_name = ?`, name).Scan(&grantsRemoved); err != nil {
		return false, 0, err
	}
	if grantsRemoved > 0 && !force {
		return false, grantsRemoved, ErrKeychainKeyHasGrants
	}
	if force {
		if _, err := tx.ExecContext(ctx, `DELETE FROM keychain_grants WHERE key_name = ?`, name); err != nil {
			return false, 0, err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM keychain_keys WHERE name = ?`, name)
	if err != nil {
		return false, 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return affected == 1, grantsRemoved, nil
}
