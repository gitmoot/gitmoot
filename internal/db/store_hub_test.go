package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHubMachineLookupHonoursExpiryRevocationAndOneActivePerName(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	fingerprint := func(c byte) string { return strings.Repeat(string(c), 64) }

	mac, err := store.AddHubMachine(ctx, HubMachine{Name: "mac", Fingerprint: fingerprint('a'), Roles: []string{"b-role", "a-role", "b-role"},
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(mac.Roles, ",") != "a-role,b-role" {
		t.Fatalf("roles = %v, want sorted and deduplicated", mac.Roles)
	}
	found, err := store.ActiveHubMachineByFingerprint(ctx, fingerprint('a'), now)
	if err != nil || found.Name != "mac" || strings.Join(found.Roles, ",") != "a-role,b-role" {
		t.Fatalf("lookup = %+v %v", found, err)
	}
	if _, err := store.ActiveHubMachineByFingerprint(ctx, fingerprint('a'), now.Add(24*time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired certificate: %v", err)
	}
	if _, err := store.AddHubMachine(ctx, HubMachine{Name: "mac", Fingerprint: fingerprint('b'), Roles: []string{"a-role"},
		CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, ErrHubMachineExists) {
		t.Fatalf("second active machine with the same name: %v", err)
	}
	if _, err := store.RevokeHubMachine(ctx, "mac", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActiveHubMachineByFingerprint(ctx, fingerprint('a'), now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked certificate: %v", err)
	}
	if _, err := store.RevokeHubMachine(ctx, "mac", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second revoke: %v", err)
	}
	if _, err := store.AddHubMachine(ctx, HubMachine{Name: "mac", Fingerprint: fingerprint('b'), Roles: []string{"a-role"},
		CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("re-add after revoke: %v", err)
	}
	all, err := store.ListHubMachines(ctx, true)
	if err != nil || len(all) != 2 || all[0].RevokedAt.IsZero() || !all[1].RevokedAt.IsZero() {
		t.Fatalf("list --all = %+v %v", all, err)
	}
}
