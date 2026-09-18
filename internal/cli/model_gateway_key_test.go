package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// seedModelGatewayKey registers a keychain key in the given mode and, when an
// upstream is supplied, configures its proxy settings the way
// `gitmoot key configure` does.
func seedModelGatewayKey(t *testing.T, store *db.Store, name, mode, upstream, authKind, header string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.AddKeychainKey(ctx, name, mode); err != nil {
		t.Fatalf("AddKeychainKey(%q, %q) returned error: %v", name, mode, err)
	}
	if strings.TrimSpace(upstream) == "" {
		return
	}
	if _, err := store.ConfigureKeychainProxy(ctx, name, upstream, authKind, header); err != nil {
		t.Fatalf("ConfigureKeychainProxy(%q) returned error: %v", name, err)
	}
}

// TestModelGatewayKeyRefusesAtPreflight pins that every way an operator can
// misconfigure the key is caught BEFORE a billed sandbox exists.
//
// This is the whole point of resolving at preflight as well as per-request: a
// mistake here should stop the job from starting, not surface as a 401 inside a
// running review that has already cost money.
func TestModelGatewayKeyRefusesAtPreflight(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		seed     func(*testing.T, *db.Store)
		lookup   string
		wantHint string
	}{
		{
			name:     "absent from the keychain",
			seed:     func(*testing.T, *db.Store) {},
			lookup:   "MISSING_KEY",
			wantHint: "gitmoot key add",
		},
		{
			name: "registered but injected rather than proxied",
			seed: func(t *testing.T, store *db.Store) {
				seedModelGatewayKey(t, store, "INJECTED_KEY", db.KeychainModeInjected, "", "", "")
			},
			lookup:   "INJECTED_KEY",
			wantHint: db.KeychainModeProxied,
		},
		{
			name: "proxied but never configured",
			seed: func(t *testing.T, store *db.Store) {
				seedModelGatewayKey(t, store, "UNCONFIGURED_KEY", db.KeychainModeProxied, "", "", "")
			},
			lookup:   "UNCONFIGURED_KEY",
			wantHint: "gitmoot key configure",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := openExecBackendLedgerTestStore(t)
			testCase.seed(t, store)

			worker := jobWorker{Store: store}
			_, err := worker.modelGatewayKeychainKey(testCase.lookup)
			if err == nil {
				t.Fatal("modelGatewayKeychainKey succeeded on a key that cannot serve as an upstream credential")
			}
			if !strings.Contains(err.Error(), testCase.wantHint) {
				t.Fatalf("error = %v, want it to mention %q so the operator knows the remedy", err, testCase.wantHint)
			}
		})
	}
}

// TestModelGatewayKeyAcceptsAConfiguredProxiedKey pins the positive case and,
// more importantly, that the UPSTREAM AND AUTH COME FROM THE KEY rather than
// from config. Config names the key; the key owns its own settings, so there is
// one source of truth and `gitmoot key list` shows it.
func TestModelGatewayKeyAcceptsAConfiguredProxiedKey(t *testing.T) {
	store := openExecBackendLedgerTestStore(t)
	seedModelGatewayKey(t, store, "GOOD_KEY", db.KeychainModeProxied, "https://api.example.com/v1", db.KeychainProxyAuthBearer, "")

	worker := jobWorker{Store: store}
	key, err := worker.modelGatewayKeychainKey("GOOD_KEY")
	if err != nil {
		t.Fatalf("modelGatewayKeychainKey returned error: %v", err)
	}
	if key.ProxyUpstream != "https://api.example.com/v1" {
		t.Fatalf("ProxyUpstream = %q, want the key's own configured upstream", key.ProxyUpstream)
	}
	if key.ProxyAuthKind != db.KeychainProxyAuthBearer {
		t.Fatalf("ProxyAuthKind = %q, want %q", key.ProxyAuthKind, db.KeychainProxyAuthBearer)
	}
}

// TestModelGatewayResolverFailsClosedOnARevokedKey pins the reason the resolver
// re-reads on every request instead of caching.
//
// A key revoked, reconfigured or deleted mid-job must stop working immediately.
// Caching it at preflight would leave a running review authenticating with a
// credential the operator has already withdrawn.
func TestModelGatewayResolverFailsClosedOnARevokedKey(t *testing.T) {
	ctx := context.Background()
	store := openExecBackendLedgerTestStore(t)
	seedModelGatewayKey(t, store, "REVOKED_KEY", db.KeychainModeProxied, "https://api.example.com/v1", db.KeychainProxyAuthBearer, "")

	worker := jobWorker{Store: store}
	resolver := worker.keychainModelGatewayResolver("REVOKED_KEY")

	if _, _, err := store.RemoveKeychainKey(ctx, "REVOKED_KEY", true); err != nil {
		t.Fatalf("RemoveKeychainKey returned error: %v", err)
	}
	if _, err := resolver(ctx); err == nil {
		t.Fatal("resolver returned a credential for a key that has been removed from the keychain")
	}
}
