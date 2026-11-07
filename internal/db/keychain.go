package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// KeychainModeProxied is the only mode a new key can take: the value is served
// to an upstream through the credential gateway and never enters a job's
// environment. Rows written before #2542 may still carry mode "injected"; they
// list, show and remove like any other row.
const KeychainModeProxied = "proxied"

const (
	KeychainProxyAuthBearer = "bearer"
	KeychainProxyAuthHeader = "header"
)

var ErrKeychainKeyNotFound = errors.New("keychain key not found")

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

func (s *Store) AddKeychainKey(ctx context.Context, name, mode string) (KeychainKey, error) {
	name = strings.TrimSpace(name)
	mode = strings.TrimSpace(mode)
	if name == "" {
		return KeychainKey{}, errors.New("keychain key name is required")
	}
	if mode != KeychainModeProxied {
		return KeychainKey{}, fmt.Errorf("invalid keychain mode %q; use proxied", mode)
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

// RemoveKeychainKey removes metadata only. The operator-owned keychain file is
// deliberately untouched.
func (s *Store) RemoveKeychainKey(ctx context.Context, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errors.New("keychain key name is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM keychain_keys WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}
