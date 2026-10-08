package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// TestStaleNotificationsSurfaceEverywhereUntilDelivered pins #2303 end to end:
// the configured [org].notification_stale_after decides what is stale, every
// operator surface ("Needs a human", Comms, doctor, daemon status) reports the
// same per-role count, oldest age and last reason, and delivering the rows
// clears all of them. Nothing here may change a delivery state.
func TestStaleNotificationsSurfaceEverywhereUntilDelivered(t *testing.T) {
	home := dashboardTestHome(t)
	paths := config.PathsForHome(home)
	appendFile(t, paths.ConfigFile, "\n[org]\nnotification_stale_after = \"20m\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n"+
		"[org.roles.\"reviewer\"]\nparent = \"owner\"\nscope = [\"*\"]\n[org.roles.\"builder\"]\nparent = \"owner\"\nscope = [\"*\"]\n"+
		"[org.roles.\"tester\"]\nparent = \"owner\"\nscope = [\"*\"]\n")

	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	addressed := func(role string, age time.Duration) int64 {
		t.Helper()
		note, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "stale/" + role, Author: "owner", Body: "please look", AddressedTarget: role,
		})
		if err != nil {
			t.Fatal(err)
		}
		setWakeOutboxCreatedAt(t, paths.Database, strconv.FormatInt(note.ID, 10), now.Add(-age))
		return note.ID
	}
	addressed("reviewer", 2*time.Hour+5*time.Minute)
	addressed("reviewer", 40*time.Minute)
	// 25m is stale only because the configured threshold (20m) is honored; the
	// 30m default would hide it.
	addressed("builder", 25*time.Minute)
	addressed("tester", 5*time.Minute)
	ids := wakeOutboxIDsForRole(t, paths.Database, "reviewer")
	if err := store.DeferMessageNotifications(ctx, ids, "runtime notification capability unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	wantRoles := []dashboardStaleNotificationRole{
		{Role: "reviewer", Count: 2, OldestAge: "2h5m", Reason: "runtime notification capability unavailable"},
		{Role: "builder", Count: 1, OldestAge: "25m", Reason: staleNotificationNoReason},
	}
	normalize := func(in []dashboardStaleNotificationRole) []dashboardStaleNotificationRole {
		out := make([]dashboardStaleNotificationRole, 0, len(in))
		for _, r := range in {
			out = append(out, dashboardStaleNotificationRole{Role: r.Role, Count: r.Count, OldestAge: r.OldestAge, Reason: r.Reason})
		}
		return out
	}
	statesBefore := wakeOutboxStates(t, paths.Database)

	var stale dashboardStaleNotifications
	getDashboardJSON(t, home, "/api/stale-notifications", &stale)
	if got := normalize(stale.Roles); !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("stale roles = %+v, want %+v", got, wantRoles)
	}
	if stale.Total != 3 || stale.Threshold != "20m" {
		t.Fatalf("stale = %+v, want total 3 over 20m", stale)
	}
	var attention dashboard.Attention
	getDashboardJSON(t, home, "/api/attention", &attention)
	if attention.Total != 3 {
		t.Fatalf("attention total = %d, want the 3 stale notifications counted as waiting", attention.Total)
	}
	var comms dashboardCommsResponse
	getDashboardJSON(t, home, "/api/comms", &comms)
	if got := normalize(comms.StaleNotifications.Roles); !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("comms stale roles = %+v, want %+v", got, wantRoles)
	}

	check, ok := staleNotificationsDoctorCheck(paths)
	wantDetail := "3 notifications waiting longer than 20m (reviewer: 2 waiting, oldest 2h5m, last reason: runtime notification capability unavailable; builder: 1 waiting, oldest 25m, last reason: no delivery attempt recorded yet)"
	if !ok || check.OK || check.Detail != wantDetail {
		t.Fatalf("doctor check = %+v (ok %v), want warning %q", check, ok, wantDetail)
	}
	if line := daemonStaleNotificationsLine(paths); line != "WARNING: stale notifications: "+wantDetail {
		t.Fatalf("daemon status line = %q", line)
	}
	// Reading must not have changed any delivery state.
	if after := wakeOutboxStates(t, paths.Database); after != statesBefore {
		t.Fatalf("flagging changed delivery states: before %v after %v", statesBefore, after)
	}

	// Deliver every row; every surface must clear. A fresh source skips the
	// attention response cache.
	execRaw(t, paths.Database, `UPDATE wake_outbox SET state = 'delivered', finished_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE state = 'pending'`)
	stale = dashboardStaleNotifications{}
	getDashboardJSON(t, home, "/api/stale-notifications", &stale)
	attention = dashboard.Attention{}
	getDashboardJSON(t, home, "/api/attention", &attention)
	if stale.Total != 0 || len(stale.Roles) != 0 || attention.Total != 0 {
		t.Fatalf("after delivery stale = %+v attention total %d, want nothing waiting", stale, attention.Total)
	}
	check, _ = staleNotificationsDoctorCheck(paths)
	if !check.OK || check.Detail != "none waiting longer than 20m" {
		t.Fatalf("after delivery doctor check = %+v", check)
	}
	if line := daemonStaleNotificationsLine(paths); line != "stale notifications: none waiting longer than 20m" {
		t.Fatalf("after delivery daemon status line = %q", line)
	}
}

// TestStaleNotificationsLeaveOutRowsPendingOnPurpose covers #2309: a row the
// daemon leaves pending on purpose is not "waiting too long". A route the
// operator muted (a disabled addressed rule, health "suppressed") and a row no
// delivery rule routes (health "inert", like the owner's blocked wakes in
// production) stay visible with their real label, but only the normal row past
// the threshold is counted and warned about everywhere.
func TestStaleNotificationsLeaveOutRowsPendingOnPurpose(t *testing.T) {
	home := dashboardTestHome(t)
	paths := config.PathsForHome(home)
	appendFile(t, paths.ConfigFile, "\n[org]\nnotification_stale_after = \"20m\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n"+
		"[org.roles.\"quiet\"]\nparent = \"owner\"\nscope = [\"*\"]\n[org.roles.\"normal\"]\nparent = \"owner\"\nscope = [\"*\"]\n"+
		"[org.roles.\"unrouted\"]\nparent = \"owner\"\nscope = [\"*\"]\n")

	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "mute-quiet", OnKind: db.WakeOutboxKindReply, WakeRole: "quiet",
		Scope: db.EventRuleScopeAddressed, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	for role, age := range map[string]time.Duration{"quiet": 2 * time.Hour, "normal": 45 * time.Minute} {
		note, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "stale/" + role, Author: "owner", Body: "please look", AddressedTarget: role,
		})
		if err != nil {
			t.Fatal(err)
		}
		setWakeOutboxCreatedAt(t, paths.Database, strconv.FormatInt(note.ID, 10), now.Add(-age))
	}
	insertOptionalBlockedWake(t, store, "owner")
	insertOptionalBlockedWake(t, store, "unrouted")
	execRaw(t, paths.Database, `UPDATE wake_outbox SET created_at = ? WHERE target_role IN ('owner', 'unrouted')`,
		now.Add(-3*time.Hour).Format(db.BlockedEpisodeTimeLayout))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	statesBefore := wakeOutboxStates(t, paths.Database)

	type wireRole struct {
		Role   string `json:"role"`
		Count  int    `json:"count"`
		Reason string `json:"reason"`
	}
	type wire struct {
		Total            int        `json:"total"`
		Roles            []wireRole `json:"roles"`
		PendingOnPurpose []wireRole `json:"pendingOnPurpose"`
	}
	// A rule-less owner wake is NOT pending on purpose: the daemon delivers
	// owner wakes as Herdr Grams without a rule (owner_gram.go), so an old one
	// is overdue and counted.
	wantRoles := []wireRole{
		{Role: "owner", Count: 1, Reason: "no delivery attempt recorded yet"},
		{Role: "normal", Count: 1, Reason: "no delivery attempt recorded yet"},
	}
	wantOnPurpose := []wireRole{
		{Role: "unrouted", Count: 1, Reason: "no delivery rule"},
		{Role: "quiet", Count: 1, Reason: "muted"},
	}
	var stale wire
	getDashboardJSON(t, home, "/api/stale-notifications", &stale)
	if stale.Total != 2 || !reflect.DeepEqual(stale.Roles, wantRoles) || !reflect.DeepEqual(stale.PendingOnPurpose, wantOnPurpose) {
		t.Fatalf("stale = %+v, want total 2, roles %+v, pending on purpose %+v", stale, wantRoles, wantOnPurpose)
	}
	var attention dashboard.Attention
	getDashboardJSON(t, home, "/api/attention", &attention)
	if attention.Total != 2 {
		t.Fatalf("attention total = %d, want the owner and normal rows counted as waiting", attention.Total)
	}
	var comms struct {
		Stale wire `json:"stale_notifications"`
	}
	getDashboardJSON(t, home, "/api/comms", &comms)
	if !reflect.DeepEqual(comms.Stale, stale) {
		t.Fatalf("comms stale = %+v, want %+v", comms.Stale, stale)
	}

	wantDetail := "2 notifications waiting longer than 20m (owner: 1 waiting, oldest 3h, last reason: no delivery attempt recorded yet; " +
		"normal: 1 waiting, oldest 45m, last reason: no delivery attempt recorded yet); " +
		"2 left pending on purpose, not counted (unrouted: 1 no delivery rule; quiet: 1 muted)"
	check, ok := staleNotificationsDoctorCheck(paths)
	if !ok || check.OK || check.Detail != wantDetail {
		t.Fatalf("doctor check = %+v (ok %v), want warning %q", check, ok, wantDetail)
	}
	if line := daemonStaleNotificationsLine(paths); line != "WARNING: stale notifications: "+wantDetail {
		t.Fatalf("daemon status line = %q", line)
	}
	// Reading must not have changed any delivery state.
	if after := wakeOutboxStates(t, paths.Database); after != statesBefore {
		t.Fatalf("flagging changed delivery states: before %v after %v", statesBefore, after)
	}

	// With the normal and owner rows delivered, only rows pending on purpose
	// remain: no warning anywhere, but the labels stay.
	execRaw(t, paths.Database, `UPDATE wake_outbox SET state = 'delivered', finished_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE target_role IN ('normal', 'owner')`)
	statesBefore = wakeOutboxStates(t, paths.Database)
	stale = wire{}
	getDashboardJSON(t, home, "/api/stale-notifications", &stale)
	attention = dashboard.Attention{}
	getDashboardJSON(t, home, "/api/attention", &attention)
	if stale.Total != 0 || len(stale.Roles) != 0 || attention.Total != 0 || !reflect.DeepEqual(stale.PendingOnPurpose, wantOnPurpose) {
		t.Fatalf("muted/unroutable only: stale = %+v attention total %d, want nothing waiting and labels kept", stale, attention.Total)
	}
	quietDetail := "none waiting longer than 20m; 2 left pending on purpose, not counted (unrouted: 1 no delivery rule; quiet: 1 muted)"
	check, _ = staleNotificationsDoctorCheck(paths)
	if !check.OK || check.Detail != quietDetail {
		t.Fatalf("muted/unroutable only doctor check = %+v, want OK %q", check, quietDetail)
	}
	if line := daemonStaleNotificationsLine(paths); line != "stale notifications: "+quietDetail {
		t.Fatalf("muted/unroutable only daemon status line = %q", line)
	}
	// Reading must not have changed any delivery state.
	if after := wakeOutboxStates(t, paths.Database); after != statesBefore {
		t.Fatalf("flagging changed delivery states: before %v after %v", statesBefore, after)
	}
}

func getDashboardJSON(t *testing.T, home, path string, out any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	newDashboardWebHandler(&webDataSource{home: home}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: decode %v: %s", path, err, recorder.Body.String())
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func execRaw(t *testing.T, databasePath, query string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func wakeOutboxIDsForRole(t *testing.T, databasePath, role string) []int64 {
	t.Helper()
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT id FROM wake_outbox WHERE target_role = ? ORDER BY id`, role)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func wakeOutboxStates(t *testing.T, databasePath string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT id || ':' || state || ':' || attempt_count FROM wake_outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return strings.Join(out, ",")
}
