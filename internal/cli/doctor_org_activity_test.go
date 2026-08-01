package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

func TestBuildOrgRoleActivityDoctorCheckClassifiesAbsence(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cfg := loadOrgActivityTestConfig(t)
	tests := []struct {
		name         string
		presence     []db.OrgRolePresence
		wantOK       bool
		wantDetail   []string
		rejectDetail string
	}{
		{
			name: "fresh",
			presence: []db.OrgRolePresence{
				{Role: "owner", LastSeenAt: now.Add(-time.Minute).Format(time.RFC3339)},
				{Role: "review", LastSeenAt: now.Add(-30 * time.Minute).Format(time.RFC3339)},
			},
			wantOK: true, wantDetail: []string{"all 2 monitored org roles"},
		},
		{
			name: "stale target",
			presence: []db.OrgRolePresence{
				{Role: "owner", LastSeenAt: now.Add(-time.Minute).Format(time.RFC3339)},
				{Role: "review", LastSeenAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
			},
			wantDetail: []string{"overdue: review", "idle 2h0m0s", "recycle_after 1h"}, rejectDetail: "owner (",
		},
		{
			name: "never observed",
			presence: []db.OrgRolePresence{
				{Role: "owner", LastSeenAt: now.Add(-time.Minute).Format(time.RFC3339)},
			},
			wantDetail: []string{"never observed: review", "recycle_after 1h"},
		},
		{
			name: "unverified invalid timestamp",
			presence: []db.OrgRolePresence{
				{Role: "owner", LastSeenAt: now.Add(-time.Minute).Format(time.RFC3339)},
				{Role: "review", LastSeenAt: "not-a-time"},
			},
			wantDetail: []string{"unverified: review", "invalid last_seen_at"},
		},
		{
			name: "unverified future timestamp",
			presence: []db.OrgRolePresence{
				{Role: "owner", LastSeenAt: now.Add(-time.Minute).Format(time.RFC3339)},
				{Role: "review", LastSeenAt: now.Add(time.Hour).Format(time.RFC3339)},
			},
			wantDetail: []string{"unverified: review", "future last_seen_at"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := buildOrgRoleActivityDoctorCheck(cfg, test.presence, now)
			if check.OK != test.wantOK {
				t.Fatalf("check.OK = %v, want %v: %#v", check.OK, test.wantOK, check)
			}
			for _, want := range test.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("detail = %q, want %q", check.Detail, want)
				}
			}
			if test.rejectDetail != "" && strings.Contains(check.Detail, test.rejectDetail) {
				t.Fatalf("detail = %q, must not name fresh role %q", check.Detail, test.rejectDetail)
			}
		})
	}
}

func TestDoctorOrgRoleActivityWarnsAtOperatorArtifact(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	appendOrgActivityTestConfig(t, paths)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.TouchOrgRolePresence(context.Background(), "owner", "fresh"); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchOrgRolePresence(context.Background(), "review", "stale"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	seedOrgRolePresenceAt(t, paths, "owner", now.Add(-time.Minute), "fresh")
	seedOrgRolePresenceAt(t, paths, "review", now.Add(-2*time.Hour), "stale")

	checks := runDoctorJSONChecks(t)
	check, ok := doctorJSONCheckByName(checks, orgRoleActivityDoctorCheckName)
	if !ok {
		t.Fatalf("doctor checks = %#v, want %q", checks, orgRoleActivityDoctorCheckName)
	}
	detail, _ := check["detail"].(string)
	if check["status"] != "warn" || !strings.Contains(detail, "overdue: review") || !strings.Contains(detail, "recycle_after 1h") || strings.Contains(detail, "owner (") {
		t.Fatalf("org role activity artifact = %#v, want stale review only with policy", check)
	}
}

func TestOrgRoleActivityDoctorCheckUnreadableIsUnverified(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	appendOrgActivityTestConfig(t, paths)
	check, ok := orgRoleActivityDoctorCheck(paths)
	if !ok || check.OK || !strings.Contains(check.Detail, "org role activity unverified") || !strings.Contains(check.Detail, "open presence store") {
		t.Fatalf("check = %#v, ok=%v; want explicit unverified warning", check, ok)
	}
}

func TestOrgRoleActivityDoctorCheckWithoutPolicyIsInert(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope = [\"*\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if check, ok := orgRoleActivityDoctorCheck(paths); ok {
		t.Fatalf("check = %#v, want no database-backed check without recycle_after", check)
	}
}

func loadOrgActivityTestConfig(t *testing.T) config.OrgConfig {
	t.Helper()
	paths := config.PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	appendOrgActivityTestConfig(t, paths)
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func appendOrgActivityTestConfig(t *testing.T, paths config.Paths) {
	t.Helper()
	body := "[org]\nrecycle_after = \"1h\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n[org.roles.\"review\"]\nparent = \"owner\"\nscope = [\"*\"]\n"
	file, err := os.OpenFile(paths.ConfigFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(body); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
