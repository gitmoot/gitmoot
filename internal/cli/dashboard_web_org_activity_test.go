package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestDashboardFleetActivityProjectionDistinguishesLiveNoSessionAndSourceDown(t *testing.T) {
	_, paths := setupOrgHome(t)
	configBody, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(configBody), "display_name = \"Owner\"", "display_name = \"Owner\"\npane = \"owner\"", 1)
	updated = strings.Replace(updated, "parent = \"owner\"", "parent = \"owner\"\npane = \"review\"", 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.AddEventRules(ctx, []db.EventRule{
		{ID: "owner-reply", OnKind: "reply", WakeRole: "owner", Scope: db.EventRuleScopeObserver, Enabled: true},
		{ID: "review-blocked", OnKind: "blocked", WakeRole: "review", Scope: db.EventRuleScopeAddressed, Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	for _, job := range []db.Job{
		{ID: "running", Agent: "worker", Type: "ask", State: "running"},
		{ID: "queued", Agent: "worker", Type: "ask", State: "queued"},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/one", Author: "review",
		Body: workflow.FormatOrgEscalateNote("review", "owner", "release/one", "Need a decision."),
	}); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	completed := observed.Add(-4 * time.Minute)
	turn := int64(12)
	live := org.Snapshot{
		ObservedAt: observed,
		States: map[string]org.RoleLiveState{
			"owner":  {State: org.StateWorking, Activity: &org.RoleActivity{Turn: 11, TurnEpoch: 7, CompletedAt: completed}},
			"review": {State: org.StateUnknown, Detail: "Herdr pane has no agent_status"},
		},
		PaneBindings: map[string]org.PaneBinding{
			"owner":  {PaneID: "w1:p1"},
			"review": {PaneID: "w1:p2"},
		},
		Sessions: map[string]org.SessionActivity{
			"owner": {PaneID: "w1:p1", Agent: "claude", TaskTitle: "Implement dashboard activity", CurrentTurn: &turn},
		},
	}

	tests := []struct {
		name       string
		sourceErr  error
		wantSource string
		wantOwner  string
		wantReview string
		wantLive   int
	}{
		{name: "live source", wantSource: "up", wantOwner: "working", wantReview: "no_session", wantLive: 1},
		{name: "source down", sourceErr: errors.New("socket unavailable"), wantSource: "down", wantOwner: "source_down", wantReview: "source_down"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := buildDashboardFleetActivity(ctx, cfg, live, test.sourceErr, store, observed)
			if err != nil {
				t.Fatal(err)
			}
			if got.Source.State != test.wantSource || got.Summary.Sessions != test.wantLive {
				t.Fatalf("source=%+v summary=%+v", got.Source, got.Summary)
			}
			if got.Summary.JobsRunning != 1 || got.Summary.EscalationsOpen != 1 || got.Summary.Roles != 2 {
				t.Fatalf("store-backed summary = %+v", got.Summary)
			}
			roles := map[string]dashboardFleetActivityRole{}
			for _, role := range got.Roles {
				roles[role.Name] = role
			}
			if roles["owner"].Status != test.wantOwner || roles["review"].Status != test.wantReview {
				t.Fatalf("role states = owner:%q review:%q", roles["owner"].Status, roles["review"].Status)
			}
			if test.sourceErr == nil {
				owner := roles["owner"]
				if owner.TaskTitle != "Implement dashboard activity" || owner.CurrentTurn == nil || *owner.CurrentTurn != 12 ||
					owner.LastCompletedTurn == nil || *owner.LastCompletedTurn != 11 ||
					owner.LastCompletedAt != completed.Format(time.RFC3339) ||
					owner.TurnAgeAt != completed.Format(time.RFC3339) || owner.TurnAgeBasis != "current_inferred" ||
					len(owner.WakeRoutes) != 1 {
					t.Fatalf("owner activity = %+v", owner)
				}
				review := roles["review"]
				if review.TaskTitle != "No active agent session" || !strings.Contains(review.StatusDetail, "No active agent session") ||
					len(review.WakeRoutes) != 1 || review.WakeRoutes[0].Enabled {
					t.Fatalf("review no-session activity = %+v", review)
				}
			}
		})
	}
}

func TestDashboardFleetActivitySnapshotKeepsEngineCountsWithoutOrgRegistry(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, job := range []db.Job{
		{ID: "running", Agent: "worker", Type: "ask", State: "running"},
		{ID: "queued", Agent: "worker", Type: "ask", State: "queued"},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/one", Author: "review",
		Body: workflow.FormatOrgEscalateNote("review", "owner", "release/one", "Need a decision."),
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := (&webDataSource{home: home}).fleetActivitySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.State != "up" || !strings.Contains(got.Source.Detail, "not configured") {
		t.Fatalf("source = %+v", got.Source)
	}
	if got.Summary.JobsRunning != 1 || got.Summary.EscalationsOpen != 1 || got.Summary.Sessions != 0 {
		t.Fatalf("summary = %+v", got.Summary)
	}
	if got.Roles == nil || len(got.Roles) != 0 {
		t.Fatalf("roles = %#v, want []", got.Roles)
	}
}

func TestDashboardFleetActivityHandlerLoudStates(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   dashboardFleetActivity
		err        error
		wantState  string
		wantDetail string
	}{
		{
			name: "healthy no-session",
			snapshot: dashboardFleetActivity{
				Source: dashboardFleetActivitySource{State: "up"},
				Roles:  []dashboardFleetActivityRole{},
			},
			wantState: "up",
		},
		{
			name:      "source failure",
			err:       errors.New("read Herdr socket: refused"),
			wantState: "down", wantDetail: "refused",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ds := &webDataSource{fleetSnapshot: func(context.Context) (dashboardFleetActivity, error) {
				return test.snapshot, test.err
			}}
			recorder := httptest.NewRecorder()
			newDashboardWebHandler(ds).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/fleet/activity", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			var got dashboardFleetActivity
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Source.State != test.wantState || !strings.Contains(got.Source.Detail, test.wantDetail) {
				t.Fatalf("source = %+v, want state=%q detail containing %q", got.Source, test.wantState, test.wantDetail)
			}
			if got.Roles == nil {
				t.Fatal("roles = null, want []")
			}
		})
	}
}

func TestDashboardFleetActivitySSEHandlerStreamsLoudStates(t *testing.T) {
	tests := []struct {
		name      string
		snapshot  dashboardFleetActivity
		err       error
		wantState string
	}{
		{
			name: "up",
			snapshot: dashboardFleetActivity{
				Source: dashboardFleetActivitySource{State: "up"},
				Roles:  []dashboardFleetActivityRole{},
			},
			wantState: "up",
		},
		{name: "down", err: errors.New("Herdr unavailable"), wantState: "down"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ds := &webDataSource{
				fleetInterval: time.Hour,
				fleetSnapshot: func(context.Context) (dashboardFleetActivity, error) {
					return test.snapshot, test.err
				},
			}
			server := httptest.NewServer(newDashboardWebHandler(ds))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/fleet/activity/events", nil)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
				response.Body.Close()
				cancel()
				t.Fatalf("response status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
			}
			scanner := bufio.NewScanner(response.Body)
			var got dashboardFleetActivity
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
					response.Body.Close()
					cancel()
					t.Fatal(err)
				}
				break
			}
			response.Body.Close()
			cancel()
			if got.Source.State != test.wantState {
				t.Fatalf("streamed source = %+v, want %q", got.Source, test.wantState)
			}
		})
	}
}

func TestDashboardFleetActivityAssetsAreSelfContainedAndInjected(t *testing.T) {
	handler := newDashboardWebHandler(&webDataSource{})
	tests := []struct {
		path        string
		contentType string
		contains    []string
	}{
		{
			path: "/", contentType: "text/html",
			contains: []string{"/assets/gitmoot-fleet-activity.css", "/assets/gitmoot-fleet-activity.js"},
		},
		{
			path: "/org", contentType: "text/html",
			contains: []string{"/assets/gitmoot-fleet-activity.css", "/assets/gitmoot-fleet-activity.js"},
		},
		{
			path: "/assets/gitmoot-fleet-activity.css", contentType: "text/css",
			contains: []string{".gmfa-strip", "prefers-color-scheme:light", ".gmfa-filter-empty"},
		},
		{
			path: "/assets/gitmoot-fleet-activity.js", contentType: "application/javascript",
			contains: []string{
				"new EventSource('/api/fleet/activity/events')", "[data-org-node]",
				"decorateDrawer", "Current-turn age", "turn_age_basis",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, test.contentType) {
				t.Fatalf("content type = %q, want %q", got, test.contentType)
			}
			for _, needle := range test.contains {
				if !strings.Contains(recorder.Body.String(), needle) {
					t.Fatalf("body missing %q", needle)
				}
			}
		})
	}
}

func TestDashboardFleetActivitySubscribersShareOnePoller(t *testing.T) {
	var calls atomic.Int64
	ds := &webDataSource{
		fleetInterval: time.Hour,
		fleetSnapshot: func(context.Context) (dashboardFleetActivity, error) {
			calls.Add(1)
			return dashboardFleetActivity{
				ObservedAt: "2026-07-31T09:00:00Z",
				Source:     dashboardFleetActivitySource{State: "up"},
				Roles:      []dashboardFleetActivityRole{},
			}, nil
		},
	}
	first, cancelFirst, err := ds.subscribeFleetActivity()
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveFleetActivity(t, first)
	second, cancelSecond, err := ds.subscribeFleetActivity()
	if err != nil {
		cancelFirst()
		t.Fatal(err)
	}
	_ = receiveFleetActivity(t, second)
	if got := calls.Load(); got != 1 {
		t.Fatalf("snapshot calls = %d, want one shared poll", got)
	}
	ds.fleetMu.Lock()
	poller := ds.fleetPoller
	ds.fleetMu.Unlock()
	if poller == nil {
		t.Fatal("shared poller is absent")
	}
	poller.mu.Lock()
	refs := poller.refs
	poller.mu.Unlock()
	if refs != 2 {
		t.Fatalf("shared poller refs = %d, want 2", refs)
	}
	cancelFirst()
	cancelSecond()
	select {
	case <-poller.done:
	case <-time.After(time.Second):
		t.Fatal("shared poller did not stop after its last viewer")
	}
}

func receiveFleetActivity(t *testing.T, events <-chan []byte) dashboardFleetActivity {
	t.Helper()
	select {
	case body, open := <-events:
		if !open {
			t.Fatal("fleet activity channel closed")
		}
		var got dashboardFleetActivity
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		return got
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fleet activity")
		return dashboardFleetActivity{}
	}
}
