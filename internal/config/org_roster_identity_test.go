package config

import (
	"testing"
	"time"
)

// The REAL identity witness (JARVIS's #1637 merge-review correction, directive
// 85732): the cli-side identity test asserts hardcoded fixture names and never
// consults the raw accessor — a reimplemented expectation. This test lives in
// package config, where sortedRoles() is reachable, and compares
// ResolveOrgRoster's nil-observation output element-wise against it — so a
// divergence between the classified roster and the raw enumeration fails HERE
// regardless of what any fixture expects.
func TestResolveOrgRosterNilObservationsMatchesSortedRoles(t *testing.T) {
	cfg := OrgConfig{roles: map[string]OrgRole{
		"owner":  {Name: "owner", Scope: []string{"*"}},
		"keeper": {Name: "keeper", Parent: "owner", Scope: []string{"*"}},
		"scout":  {Name: "scout", Parent: "owner", Scope: []string{"*"}, RecycleAfter: time.Hour},
	}}
	raw := cfg.sortedRoles()
	roster := ResolveOrgRoster(cfg, nil, nil)
	for _, view := range []struct {
		name string
		got  []OrgRole
	}{
		{"Members", roster.Members()},
		{"Nudgeable", roster.Nudgeable()},
	} {
		if len(view.got) != len(raw) {
			t.Fatalf("%s() has %d roles, sortedRoles() has %d", view.name, len(view.got), len(raw))
		}
		for i := range raw {
			if view.got[i].Name != raw[i].Name {
				t.Fatalf("%s()[%d] = %q, sortedRoles()[%d] = %q — nil observations must be the raw enumeration in the raw order", view.name, i, view.got[i].Name, i, raw[i].Name)
			}
		}
	}
	if len(roster.Archived()) != 0 {
		t.Fatalf("Archived() = %v with nil observations", roster.Archived())
	}
}
