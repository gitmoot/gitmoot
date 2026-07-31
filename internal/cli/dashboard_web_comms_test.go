package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type dashboardCommsSeed struct {
	openEscalationID     int64
	systemID             int64
	resolvedEscalationID int64
	answerID             int64
	resolutionID         int64
}

func seedDashboardComms(t *testing.T, home string) dashboardCommsSeed {
	t.Helper()
	store := openCLIJobStore(t, home)
	defer store.Close()
	ctx := context.Background()
	insert := func(note db.WorkflowNote) db.WorkflowNote {
		t.Helper()
		got, err := store.InsertWorkflowNote(ctx, note)
		if err != nil {
			t.Fatalf("InsertWorkflowNote(%q): %v", note.Body, err)
		}
		return got
	}

	open := insert(db.WorkflowNote{
		WorkflowID: "release/open", Author: "operator", Repo: "gitmoot/gitmoot",
		Body: workflow.FormatOrgEscalateNote("operator", "owner", "release/open", "Can we ship the candidate?"),
	})
	system := insert(db.WorkflowNote{
		WorkflowID: "release/open", Author: db.WorkflowAutoNoteAuthor, Repo: "gitmoot/gitmoot",
		Body: "[auto:pr:1294:ready] PR #1294 checks green",
	})
	resolved := insert(db.WorkflowNote{
		WorkflowID: "release/resolved", Author: "review", Repo: "gitmoot/gitmoot",
		Body: workflow.FormatOrgEscalateNote("review", "owner", "release/resolved", "Which rollout window?"),
	})
	answer := insert(db.WorkflowNote{
		WorkflowID: "release/resolved", Author: "owner", Repo: "gitmoot/gitmoot",
		Body: "Use the first quiet window after the gate.",
	})
	resolution := insert(db.WorkflowNote{
		WorkflowID: "release/resolved", Author: "owner", Repo: "gitmoot/gitmoot",
		Body: workflow.FormatOrgEscalateResolvedNote(resolved.ID, "owner", answer.ID),
	})
	return dashboardCommsSeed{
		openEscalationID: open.ID, systemID: system.ID, resolvedEscalationID: resolved.ID,
		answerID: answer.ID, resolutionID: resolution.ID,
	}
}

func TestDashboardCommsEndpointProjectsThreads(t *testing.T) {
	home := dashboardTestHome(t)
	seed := seedDashboardComms(t, home)
	handler := newDashboardWebHandler(&webDataSource{home: home})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/comms", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/comms status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", got)
	}

	var payload dashboardCommsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /api/comms: %v\n%s", err, recorder.Body.String())
	}
	if len(payload.Threads) != 2 {
		t.Fatalf("threads=%+v, want open and resolved workflows", payload.Threads)
	}
	if payload.Threads[0].WorkflowID != "release/open" || payload.Threads[0].Unresolved != 1 {
		t.Fatalf("first thread=%+v, want unresolved release/open floated first", payload.Threads[0])
	}

	byWorkflow := map[string]dashboardCommsThread{}
	for _, thread := range payload.Threads {
		byWorkflow[thread.WorkflowID] = thread
	}
	open := byWorkflow["release/open"]
	if len(open.Messages) != 2 ||
		open.Messages[0].ID != seed.openEscalationID || open.Messages[0].Kind != "escalation" ||
		open.Messages[0].From != "operator" || open.Messages[0].To != "owner" ||
		open.Messages[1].ID != seed.systemID || open.Messages[1].Kind != "system" {
		t.Fatalf("open messages=%+v, want escalation bubble plus system line", open.Messages)
	}
	resolved := byWorkflow["release/resolved"]
	if resolved.Unresolved != 0 || len(resolved.Messages) != 2 {
		t.Fatalf("resolved thread=%+v, want two bubbles and no unresolved badge", resolved)
	}
	escalation := resolved.Messages[0]
	if escalation.ID != seed.resolvedEscalationID || escalation.Resolution == nil ||
		escalation.Resolution.NoteID != seed.resolutionID ||
		escalation.Resolution.AnswerNoteID != seed.answerID ||
		escalation.Resolution.By != "owner" {
		t.Fatalf("resolved escalation=%+v, want folded resolution marker", escalation)
	}
	reply := resolved.Messages[1]
	if reply.ID != seed.answerID || reply.Kind != "reply" || reply.From != "owner" ||
		reply.To != "review" || reply.Body != "Use the first quiet window after the gate." {
		t.Fatalf("reply=%+v, want linked answer note projected as owner-to-review bubble", reply)
	}
	for _, message := range resolved.Messages {
		if message.ID == seed.resolutionID {
			t.Fatalf("resolution marker leaked as a bubble: %+v", resolved.Messages)
		}
	}
}

func TestDashboardCommsResolutionAdvancesThreadActivity(t *testing.T) {
	escalationAt := "2026-07-30 10:00:00"
	resolutionAt := "2026-07-30 12:00:00"
	escalation := db.WorkflowNote{
		ID: 41, WorkflowID: "release/resolved-without-answer", Author: "review",
		CreatedAt: escalationAt,
		Body:      workflow.FormatOrgEscalateNote("review", "owner", "release/resolved-without-answer", "Ship now?"),
	}
	resolution := db.WorkflowNote{
		ID: 42, WorkflowID: escalation.WorkflowID, Author: "owner",
		CreatedAt: resolutionAt,
		Body:      workflow.FormatOrgEscalateResolvedNote(escalation.ID, "owner", 0),
	}

	thread, ok := dashboardCommsProjectThread(escalation.WorkflowID, []db.WorkflowNote{escalation, resolution})
	if !ok {
		t.Fatal("dashboardCommsProjectThread returned no thread")
	}
	if want := "2026-07-30T12:00:00Z"; thread.UpdatedAt != want {
		t.Fatalf("thread updated_at=%q, want resolution marker time %q", thread.UpdatedAt, want)
	}
	if len(thread.Messages) != 1 || thread.Messages[0].Resolution == nil {
		t.Fatalf("messages=%+v, want one escalation with a folded resolution", thread.Messages)
	}
}

func TestDashboardCommsHTTPStates(t *testing.T) {
	emptyHome := dashboardTestHome(t)
	badHome := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(badHome, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		home       string
		path       string
		wantStatus int
		want       string
	}{
		{name: "empty endpoint is an explicit empty array", home: emptyHome, path: "/api/comms", wantStatus: http.StatusOK, want: `"threads":[]`},
		{name: "source down is service unavailable", home: badHome, path: "/api/comms", wantStatus: http.StatusServiceUnavailable, want: "comms source unavailable"},
		{name: "page route is self contained", home: emptyHome, path: "/comms", wantStatus: http.StatusOK, want: "Read-only org traffic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			newDashboardWebHandler(&webDataSource{home: test.home}).ServeHTTP(
				recorder, httptest.NewRequest(http.MethodGet, test.path, nil),
			)
			if recorder.Code != test.wantStatus || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("GET %s status=%d body=%q, want status=%d containing %q",
					test.path, recorder.Code, recorder.Body.String(), test.wantStatus, test.want)
			}
		})
	}
}

func TestDashboardCommsPageContract(t *testing.T) {
	home := dashboardTestHome(t)
	recorder := httptest.NewRecorder()
	newDashboardWebHandler(&webDataSource{home: home}).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/comms?note=42#note-42", nil),
	)
	body := recorder.Body.String()
	for _, test := range []struct {
		name string
		want string
	}{
		{name: "thread rail open filter", want: `id="open"`},
		{name: "thread rail all filter", want: `id="all"`},
		{name: "search", want: `id="search"`},
		{name: "workflow search keeps matching thread messages", want: `matchesMessage(m,workflowMatch?'':q`},
		{name: "from filter", want: `id="from"`},
		{name: "to filter", want: `id="to"`},
		{name: "resolution filter", want: `id="resolution"`},
		{name: "date filter", want: `id="date"`},
		{name: "engine toggle", want: `id="systems"`},
		{name: "expand toggle", want: `class="expand"`},
		{name: "deep link anchor", want: `id="note-`},
		{name: "resolution footer", want: `Resolved by`},
		{name: "open duration", want: `m open`},
		{name: "source down state", want: `Comms source unavailable`},
		{name: "empty filter state", want: `No matching comms`},
		{name: "dark theme", want: `data-theme="dark"`},
		{name: "light theme", want: `[data-theme="light"]`},
		{name: "no external assets", want: `<script>`},
		{name: "read only footer", want: `gitmoot org escalate resolve`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(body, test.want) {
				t.Fatalf("Comms page missing %q (%s)", test.want, test.name)
			}
		})
	}
	for _, forbidden := range []string{"https://", "http://", "<link rel=\"stylesheet\"", "cdn"} {
		t.Run("forbids "+fmt.Sprintf("%q", forbidden), func(t *testing.T) {
			if strings.Contains(body, forbidden) {
				t.Fatalf("Comms page contains forbidden external asset marker %q", forbidden)
			}
		})
	}
}
