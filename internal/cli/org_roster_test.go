package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
)

func orgRosterTestConfig(t *testing.T) config.OrgConfig {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[org.roles."owner"]
scope = ["*"]
pane = "Gitmoot"
[org.roles."keeper"]
parent = "owner"
scope = ["gitmoot/*"]
[org.roles."scout"]
parent = "owner"
scope = ["gitmoot/*"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func rosterNames(roles []config.OrgRole) []string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
	}
	return names
}

func assertRosterNames(t *testing.T, label string, got []config.OrgRole, want ...string) {
	t.Helper()
	names := rosterNames(got)
	if len(names) != len(want) {
		t.Fatalf("%s = %v, want %v", label, names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("%s = %v, want %v", label, names, want)
		}
	}
}

// Nil observations must reproduce the pre-#1635 roster exactly: every
// configured role is a full member, nudgeable, standing active. This is the
// deploy-in-either-order contract (missing lifecycle field means ACTIVE).
func TestResolveOrgRosterNilObservationsIsIdentity(t *testing.T) {
	cfg := orgRosterTestConfig(t)
	roster := resolveOrgRoster(cfg, nil, nil)
	assertRosterNames(t, "Members()", roster.Members(), "keeper", "owner", "scout")
	assertRosterNames(t, "Nudgeable()", roster.Nudgeable(), "keeper", "owner", "scout")
	if len(roster.Archived()) != 0 {
		t.Fatalf("Archived() = %v, want empty", roster.Archived())
	}
	for _, name := range []string{"owner", "keeper", "scout"} {
		if s := roster.Standing(name); s != orgSeatActive {
			t.Fatalf("Standing(%q) = %q, want active", name, s)
		}
	}
}

// An archived observation removes the seat from BOTH views (full exclusion) and
// surfaces it, with its observation fields intact, in Archived().
func TestResolveOrgRosterArchivedIsFullExclusion(t *testing.T) {
	cfg := orgRosterTestConfig(t)
	at := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	observed := at.Add(30 * time.Minute)
	roster := resolveOrgRoster(cfg, map[string]orgArchivedObservation{
		"scout": {At: at, By: "jarvis", Reason: "project paused", ObservedAt: observed},
	}, nil)
	assertRosterNames(t, "Members()", roster.Members(), "keeper", "owner")
	assertRosterNames(t, "Nudgeable()", roster.Nudgeable(), "keeper", "owner")
	if got := roster.Archived(); len(got) != 1 ||
		got[0].Role.Name != "scout" || !got[0].At.Equal(at) ||
		got[0].By != "jarvis" || got[0].Reason != "project paused" || !got[0].ObservedAt.Equal(observed) {
		t.Fatalf("Archived() = %+v, want one scout entry with observation fields", got)
	}
	if s := roster.Standing("scout"); s != orgSeatArchived {
		t.Fatalf("Standing(scout) = %q, want archived", s)
	}
}

// A paused seat is a SOFT exclusion: still a member (chart/health/presence/
// dispatch) but not nudgeable (no nudge ladders, alarms, automated wakes).
// Distinct from archived by ruling on #1635 — collapsing the two over-hides a
// live paused seat or under-hides an archived one.
func TestResolveOrgRosterPausedIsSoftExclusion(t *testing.T) {
	cfg := orgRosterTestConfig(t)
	roster := resolveOrgRoster(cfg, nil, map[string]string{"scout": "deliberately idle"})
	assertRosterNames(t, "Members()", roster.Members(), "keeper", "owner", "scout")
	assertRosterNames(t, "Nudgeable()", roster.Nudgeable(), "keeper", "owner")
	if s := roster.Standing("scout"); s != orgSeatPaused {
		t.Fatalf("Standing(scout) = %q, want paused", s)
	}
	if len(roster.Archived()) != 0 {
		t.Fatalf("Archived() = %v, want empty", roster.Archived())
	}
}

// Standing lookups are case-insensitive on the caller side (config guarantees
// lower-case role names; callers may not).
func TestResolveOrgRosterLookupIsCaseInsensitive(t *testing.T) {
	cfg := orgRosterTestConfig(t)
	roster := resolveOrgRoster(cfg, map[string]orgArchivedObservation{"keeper": {ObservedAt: time.Now().UTC()}}, nil)
	assertRosterNames(t, "Members()", roster.Members(), "owner", "scout")
	if s := roster.Standing("KEEPER"); s != orgSeatArchived {
		t.Fatalf("Standing(KEEPER) = %q, want archived", s)
	}
}

// An unconfigured role reads ACTIVE: absence of lifecycle evidence must never
// exclude a seat.
func TestOrgRosterStandingUnknownRoleReadsActive(t *testing.T) {
	cfg := orgRosterTestConfig(t)
	roster := resolveOrgRoster(cfg, nil, nil)
	if s := roster.Standing("nobody"); s != orgSeatActive {
		t.Fatalf("Standing(nobody) = %q, want active", s)
	}
}

// The supplier seam returns no observations today (the #1635 ingest lands
// there), so loadOrgRoster with any store — including nil — is the identity
// roster.
func TestLoadOrgRosterNilStoreIsIdentity(t *testing.T) {
	cfg := orgRosterTestConfig(t)
	roster := loadOrgRoster(context.Background(), nil, cfg)
	assertRosterNames(t, "Members()", roster.Members(), "keeper", "owner", "scout")
	assertRosterNames(t, "Nudgeable()", roster.Nudgeable(), "keeper", "owner", "scout")
}
