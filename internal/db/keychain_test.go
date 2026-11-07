package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeychainMigrationAppliesToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	legacy, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	version := 0
	proxyVersion := 0
	for i, migration := range migrations {
		if strings.Contains(migration, "CREATE TABLE keychain_keys") {
			version = i + 1
		}
		if strings.Contains(migration, "ADD COLUMN proxy_upstream") {
			proxyVersion = i + 1
		}
	}
	if version == 0 || proxyVersion == 0 {
		t.Fatalf("keychain migrations not found: create=%d proxy=%d", version, proxyVersion)
	}
	if _, err := legacy.db.Exec(`DROP TABLE IF EXISTS keychain_grants; DROP TABLE keychain_keys;
		DELETE FROM schema_migrations WHERE version IN (?, ?)`, version, proxyVersion); err != nil {
		t.Fatalf("rewind keychain migration: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("Open existing database: %v", err)
	}
	defer store.Close()
	for _, table := range []string{"keychain_keys"} {
		if ok, err := store.HasTable(context.Background(), table); err != nil || !ok {
			t.Fatalf("table %s ok=%v err=%v", table, ok, err)
		}
	}
	columns, err := store.db.QueryContext(context.Background(), `PRAGMA table_info(keychain_keys)`)
	if err != nil {
		t.Fatal(err)
	}
	defer columns.Close()
	found := map[string]bool{}
	for columns.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := columns.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	for _, name := range []string{"proxy_upstream", "proxy_auth_kind", "proxy_header"} {
		if !found[name] {
			t.Fatalf("proxy migration did not add %s", name)
		}
	}
}

func TestKeychainCRUDProxiedOnly(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if _, err := store.AddKeychainKey(ctx, "BAD", "other"); err == nil {
		t.Fatal("invalid mode was accepted")
	}
	if _, err := store.AddKeychainKey(ctx, "API_TOKEN", "injected"); err == nil {
		t.Fatal("injected mode was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO keychain_keys(name, mode) VALUES ('RAW_BAD', 'other')`); err == nil {
		t.Fatal("migration CHECK accepted an invalid mode")
	}
	added, err := store.AddKeychainKey(ctx, "MODEL_TOKEN", KeychainModeProxied)
	if err != nil || added.Name != "MODEL_TOKEN" || added.Mode != KeychainModeProxied || added.CreatedAt == "" || added.ProxyConfigured() {
		t.Fatalf("AddKeychainKey proxied = %+v err=%v", added, err)
	}
	if _, err := store.AddKeychainKey(ctx, "MODEL_TOKEN", KeychainModeProxied); err == nil {
		t.Fatal("duplicate key name was accepted")
	}
	configured, err := store.ConfigureKeychainProxy(ctx, "MODEL_TOKEN", "https://api.example.test/v1", KeychainProxyAuthHeader, "X-Model-Key")
	if err != nil || configured.ProxyUpstream != "https://api.example.test/v1" || configured.ProxyAuthKind != KeychainProxyAuthHeader || configured.ProxyHeader != "X-Model-Key" || !configured.ProxyConfigured() {
		t.Fatalf("ConfigureKeychainProxy = %+v err=%v", configured, err)
	}
	shown, found, err := store.GetKeychainKey(ctx, "MODEL_TOKEN")
	if err != nil || !found || shown.ProxyHeader != "X-Model-Key" || !shown.ProxyConfigured() {
		t.Fatalf("GetKeychainKey = %+v found=%v err=%v", shown, found, err)
	}
	if _, found, err := store.GetKeychainKey(ctx, "MISSING"); err != nil || found {
		t.Fatalf("GetKeychainKey missing = found %v err %v", found, err)
	}
	keys, err := store.ListKeychainKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Name != "MODEL_TOKEN" || keys[0].ProxyHeader != "X-Model-Key" {
		t.Fatalf("keys = %+v err=%v", keys, err)
	}
	if removed, err := store.RemoveKeychainKey(ctx, "MODEL_TOKEN"); err != nil || !removed {
		t.Fatalf("RemoveKeychainKey = removed %v err %v", removed, err)
	}
	if removed, err := store.RemoveKeychainKey(ctx, "MODEL_TOKEN"); err != nil || removed {
		t.Fatalf("second RemoveKeychainKey = removed %v err %v", removed, err)
	}
	if keys, err := store.ListKeychainKeys(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("keys after remove = %+v err=%v", keys, err)
	}
}

// Rows written before #2542 may carry mode "injected"; AddKeychainKey no longer
// creates them, but they must still list, show and remove.
func TestKeychainLegacyInjectedRowListsShowsAndRemoves(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO keychain_keys(name, mode) VALUES ('LEGACY_TOKEN', 'injected')`); err != nil {
		t.Fatalf("insert legacy injected row: %v", err)
	}
	keys, err := store.ListKeychainKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Name != "LEGACY_TOKEN" || keys[0].Mode != "injected" || keys[0].ProxyConfigured() {
		t.Fatalf("keys = %+v err=%v", keys, err)
	}
	shown, found, err := store.GetKeychainKey(ctx, "LEGACY_TOKEN")
	if err != nil || !found || shown.Mode != "injected" {
		t.Fatalf("GetKeychainKey = %+v found=%v err=%v", shown, found, err)
	}
	if removed, err := store.RemoveKeychainKey(ctx, "LEGACY_TOKEN"); err != nil || !removed {
		t.Fatalf("RemoveKeychainKey = removed %v err %v", removed, err)
	}
	if _, found, err := store.GetKeychainKey(ctx, "LEGACY_TOKEN"); err != nil || found {
		t.Fatalf("GetKeychainKey after remove = found %v err %v", found, err)
	}
}
