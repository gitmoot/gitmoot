package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestDashboardOrgRegistryDisabledDegradesGracefully guards the #1097 review fix:
// an org-less deployment (config present, no [org] section) must return an empty
// view / not-found rather than an HTTP 500, so the SPA shows the "Org not
// configured" empty state and org-less hosts (e.g. the public dashboard) keep
// working.
func TestDashboardOrgRegistryDisabledDegradesGracefully(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	ds := &webDataSource{home: home}
	view, err := ds.Org(context.Background())
	if err != nil {
		t.Fatalf("Org() on org-less home should not error, got %v", err)
	}
	if len(view.Roles) != 0 || view.DetectionEnabled || view.DataAsOf != "" {
		t.Fatalf("org-less view = roles:%d detection:%v dataAsOf:%q, want empty", len(view.Roles), view.DetectionEnabled, view.DataAsOf)
	}
	if _, err := ds.OrgRole(context.Background(), "owner"); !errors.Is(err, dashboard.ErrOrgRoleNotFound) {
		t.Fatalf("OrgRole() on org-less home = %v, want ErrOrgRoleNotFound", err)
	}
}

func TestDashboardOrgDataSourceStoreBackedProjection(t *testing.T) {
	home, paths := setupOrgHome(t)
	enableDashboardOrgDetection(t, paths.ConfigFile)

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{ID: "org-signals", OnKind: "blocked", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"owner", "review"} {
		if err := store.TouchOrgRolePresence(ctx, role, "agent dispatch"); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateJob(ctx, db.Job{
			ID: role + "-running", Agent: role, Type: "ask", State: "running",
			Payload: `{"acting_org_role":"` + role + `"}`,
		}); err != nil {
			t.Fatal(err)
		}
	}

	base := time.Now().UTC().Truncate(time.Second)
	sourceMax := base.Add(5 * time.Hour)
	if err := store.UpsertRoleLivePresence(ctx, "owner", "working", base); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRoleLivePresence(ctx, "review", "blocked", base); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlockedEpisode(ctx, "role:review", base.Add(-2*time.Hour), base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBlockedEpisodeEmitted(ctx, "role:review", base.Add(-50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlockedEpisode(ctx, "task:acme/widget:review", base.Add(-3*time.Hour), base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRecycleOverdueEpisode(ctx, "owner", base.Add(-2*time.Hour), sourceMax); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRecycleOverdueEpisodeEmitted(ctx, "owner", base.Add(-40*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.IncrementRoleMissedWake(ctx, "review", base.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	escalationOld := workflow.FormatOrgEscalateNote("review", "owner", "release/one", "Need owner input.")
	escalationNew := workflow.FormatOrgEscalateNote("owner", "review", "release/two", "Please verify.")
	handoffOld := workflow.FormatOrgHandoffNote("owner", "Initial handoff.")
	handoffNew := workflow.FormatOrgHandoffNote("owner", "Latest handoff.")
	handoffReview := workflow.FormatOrgHandoffNote("review", "Review journaled.")
	resolvedEscalation, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{WorkflowID: "release/one", Author: "review", Body: escalationOld})
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range []db.WorkflowNote{
		{WorkflowID: "release/two", Author: "owner", Body: escalationNew},
		{WorkflowID: "org/owner", Author: "owner", Body: handoffOld},
		{WorkflowID: "org/owner", Author: "owner", Body: handoffNew},
		{WorkflowID: "org/review", Author: "review", Body: handoffReview},
	} {
		if _, err := store.InsertWorkflowNote(ctx, note); err != nil {
			t.Fatal(err)
		}
	}
	resolutionBody := workflow.FormatOrgEscalateResolvedNote(resolvedEscalation.ID, "owner", 0)
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: resolvedEscalation.WorkflowID, Author: "owner", Body: resolutionBody,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	conn, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	resolutionMax := base.Add(6 * time.Hour)
	for _, update := range []struct {
		query string
		args  []any
	}{
		{`UPDATE org_role_presence SET last_seen_at = ? WHERE role = 'owner'`, []any{base.Add(-3 * time.Hour).Format("2006-01-02 15:04:05")}},
		{`UPDATE org_role_presence SET last_seen_at = ? WHERE role = 'review'`, []any{base.Add(-10 * time.Minute).Format("2006-01-02 15:04:05")}},
		{`UPDATE org_recycle_overdue_episodes SET updated_at = ? WHERE subject = 'owner'`, []any{sourceMax.Format(db.BlockedEpisodeTimeLayout)}},
		{`UPDATE workflow_notes SET created_at = ? WHERE body = ?`, []any{base.Add(-25 * time.Minute).Format("2006-01-02 15:04:05"), escalationOld}},
		{`UPDATE workflow_notes SET created_at = ? WHERE body = ?`, []any{base.Add(-20 * time.Minute).Format("2006-01-02 15:04:05"), escalationNew}},
		{`UPDATE workflow_notes SET created_at = ? WHERE body = ?`, []any{resolutionMax.Format("2006-01-02 15:04:05"), resolutionBody}},
		{`UPDATE workflow_notes SET created_at = ? WHERE body = ?`, []any{base.Add(-45 * time.Minute).Format("2006-01-02 15:04:05"), handoffOld}},
		{`UPDATE workflow_notes SET created_at = ? WHERE body = ?`, []any{base.Add(-15 * time.Minute).Format("2006-01-02 15:04:05"), handoffNew}},
		{`UPDATE workflow_notes SET created_at = ? WHERE body = ?`, []any{base.Add(-10 * time.Minute).Format("2006-01-02 15:04:05"), handoffReview}},
	} {
		if _, err := conn.ExecContext(ctx, update.query, update.args...); err != nil {
			t.Fatal(err)
		}
	}

	ds := &webDataSource{home: home}
	view, err := ds.Org(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !view.DetectionEnabled || view.DetectionHint != "" {
		t.Fatalf("detection = %v hint %q", view.DetectionEnabled, view.DetectionHint)
	}
	if view.DataAsOf != resolutionMax.Format(time.RFC3339) {
		t.Fatalf("data_as_of = %q, want resolution max %q", view.DataAsOf, resolutionMax.Format(time.RFC3339))
	}
	if len(view.Roles) != 2 || view.Roles[0].Name != "owner" || view.Roles[1].Name != "review" {
		t.Fatalf("roles = %+v", view.Roles)
	}
	if view.Roles[0].DisplayName != "Owner" {
		t.Fatalf("owner display_name = %q", view.Roles[0].DisplayName)
	}
	if view.Roles[0].PresenceState != "working" || view.Roles[0].Badges.Overdue == "" {
		t.Fatalf("owner = %+v", view.Roles[0])
	}
	if view.Roles[1].PresenceState != "blocked" || view.Roles[1].Badges.BlockedSince == "" || view.Roles[1].Badges.MissedWakes != 1 {
		t.Fatalf("review = %+v", view.Roles[1])
	}
	if got := view.Health; got.Roles != 2 || got.Working != 1 || got.Blocked != 1 || got.Overdue != 1 || got.OpenEscalations != 1 || got.StalledWakes != 1 {
		t.Fatalf("health = %+v", got)
	}
	if len(view.Escalations) != 1 || view.Escalations[0].Wf != "release/two" {
		t.Fatalf("escalations = %+v", view.Escalations)
	}
	if len(view.Feed) != 5 {
		t.Fatalf("feed = %+v", view.Feed)
	}
	for _, row := range view.Feed {
		if row.Role == "task:acme/widget:review" {
			t.Fatalf("task episode leaked into org feed: %+v", row)
		}
		if row.Kind == "recycle" && !strings.HasPrefix(row.Detail, "journaled handoff: ") {
			t.Fatalf("handoff is not labeled as journaled: %+v", row)
		}
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "outcome") || strings.Contains(strings.ToLower(string(raw)), "refused") {
		t.Fatalf("feed overclaims delivery outcome: %s", raw)
	}

	role, err := ds.OrgRole(ctx, "OWNER")
	if err != nil {
		t.Fatal(err)
	}
	if role.Identity.Name != "owner" || role.Identity.DisplayName != "Owner" || len(role.Identity.Path) != 1 || role.Presence.State != "working" {
		t.Fatalf("owner role = %+v", role)
	}
	if role.Recycle.LastHandoffText != "Latest handoff." || role.Recycle.LastHandoffAt == "" || role.Recycle.Overdue == "" {
		t.Fatalf("owner recycle = %+v", role.Recycle)
	}
	if role.Activity.JobsToday["running"] != 1 || role.Activity.Notes != 2 {
		t.Fatalf("owner activity = %+v", role.Activity)
	}
	if len(role.Escalations) != 1 || role.Escalations[0].Wf != "release/two" {
		t.Fatalf("owner escalations = %+v", role.Escalations)
	}

	if _, err := ds.OrgRole(ctx, "missing"); !errors.Is(err, dashboard.ErrOrgRoleNotFound) {
		t.Fatalf("missing role error = %v", err)
	}
	handler := newDashboardWebHandler(ds)
	for _, route := range []string{"/api/org", "/api/org/role/owner"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, route, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", route, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/org/role/missing", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing role HTTP status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestDashboardOrgMissedWakeAgeOutMatchesBadgesAndHealth(t *testing.T) {
	_, paths := setupOrgHome(t)
	enableDashboardOrgDetection(t, paths.ConfigFile)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if err := store.IncrementRoleMissedWake(ctx, "owner", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.IncrementRoleMissedWake(ctx, "review", now.Add(-missedWakeStaleAfter-time.Second)); err != nil {
		t.Fatal(err)
	}

	view, err := buildDashboardOrg(ctx, paths, store, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Roles) != 2 {
		t.Fatalf("roles = %+v", view.Roles)
	}
	badges := map[string]int{}
	for _, role := range view.Roles {
		badges[role.Name] = role.Badges.MissedWakes
	}
	if badges["owner"] != 1 || badges["review"] != 0 {
		t.Fatalf("missed-wake badges = %+v", badges)
	}
	if view.Health.StalledWakes != 1 {
		t.Fatalf("stalled wakes = %d, want 1", view.Health.StalledWakes)
	}
}

// TestDashboardOrgResolvedEscalationSurvivesUnrelatedResolutionVolume guards a
// high-review-caught regression: resolved-escalation lookup must not depend on
// a single globally time-windowed LIMIT-200 scan of ALL resolve-marker notes.
// A resolve marker far older than 200 UNRELATED newer resolutions (in other
// workflows) must still be found, because lookup is scoped per-workflow, not by
// a global recency window that could push the marker out while the original
// escalation note (itself capped at 200, in its own small workflow) stays in.
func TestDashboardOrgResolvedEscalationSurvivesUnrelatedResolutionVolume(t *testing.T) {
	_, paths := setupOrgHome(t)
	enableDashboardOrgDetection(t, paths.ConfigFile)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	target, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/one", Author: "review",
		Body: workflow.FormatOrgEscalateNote("review", "owner", "release/one", "Need a decision."),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/one", Author: "owner",
		Body: workflow.FormatOrgEscalateResolvedNote(target.ID, "owner", 0),
	}); err != nil {
		t.Fatal(err)
	}
	// 200+ newer, UNRELATED resolve-marker notes in a different workflow. A
	// single global "most recent 200 resolve markers" scan would push the
	// target's marker out of window; a per-workflow scoped lookup never sees
	// these at all when resolving "release/one"'s escalation.
	for i := 0; i < dashboardOrgNoteLimit+10; i++ {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "filler/other", Author: "owner",
			Body: workflow.FormatOrgEscalateResolvedNote(target.ID+int64(1000+i), "owner", 0),
		}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	view, err := buildDashboardOrg(ctx, paths, store, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Escalations) != 0 || view.Health.OpenEscalations != 0 {
		t.Fatalf("resolved escalation resurfaced as open: escalations=%+v health=%+v", view.Escalations, view.Health)
	}
}

func TestDashboardOrgDetectionDisabledIsHonest(t *testing.T) {
	home, paths := setupOrgHome(t)
	enableDashboardOrgDetection(t, paths.ConfigFile)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	view, err := (&webDataSource{home: home}).Org(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.DetectionEnabled || !strings.Contains(view.DetectionHint, "no enabled org event rules") {
		t.Fatalf("detection = %v hint %q", view.DetectionEnabled, view.DetectionHint)
	}
}

func TestDashboardOrgDetectionUsesLoadedOrchestratePolicy(t *testing.T) {
	tests := []struct {
		name        string
		enableKnob  bool
		seedLive    bool
		wantEnabled bool
		wantHint    string
	}{
		{name: "configured with live presence", enableKnob: true, seedLive: true, wantEnabled: true},
		{name: "configured but no live presence yet", enableKnob: true, wantEnabled: true, wantHint: "waiting for live presence"},
		{name: "zero knob", wantHint: "blocked_role_wake_after is disabled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, paths := setupOrgHome(t)
			if test.enableKnob {
				enableDashboardOrgDetection(t, paths.ConfigFile)
			}
			store, err := db.Open(paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AddEventRule(context.Background(), db.EventRule{
				ID: "org-signals", OnKind: "blocked", WakeRole: "owner", Enabled: true,
			}); err != nil {
				t.Fatal(err)
			}
			if test.seedLive {
				if err := store.UpsertRoleLivePresence(context.Background(), "owner", "working", time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			view, err := (&webDataSource{home: home}).Org(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if view.DetectionEnabled != test.wantEnabled {
				t.Fatalf("DetectionEnabled = %v, want %v (hint %q)", view.DetectionEnabled, test.wantEnabled, view.DetectionHint)
			}
			if test.wantHint == "" && view.DetectionHint != "" {
				t.Fatalf("DetectionHint = %q, want empty", view.DetectionHint)
			}
			if test.wantHint != "" && !strings.Contains(view.DetectionHint, test.wantHint) {
				t.Fatalf("DetectionHint = %q, want substring %q", view.DetectionHint, test.wantHint)
			}
		})
	}
}

func enableDashboardOrgDetection(t *testing.T, configPath string) {
	t.Helper()
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(content), "blocked_role_wake_after = \"0s\"", "blocked_role_wake_after = \"1h\"", 1)
	updated = strings.Replace(updated, "max_consecutive_missed_wakes = 0", "max_consecutive_missed_wakes = 2", 1)
	updated = strings.Replace(updated, "[org]\nenforce = \"warn\"", "[org]\nrecycle_after = \"1h\"\nenforce = \"warn\"", 1)
	if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}
