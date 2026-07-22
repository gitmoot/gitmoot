package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestWebDataSourceBrainEventsPagingEmptyAndChangeCursor(t *testing.T) {
	home, store := memoryTestHome(t)
	ds := &webDataSource{home: home}
	ctx := context.Background()
	empty, err := ds.BrainEvents(ctx, 0, 2)
	if err != nil || empty.Events == nil || len(empty.Events) != 0 || empty.NextCursor != 0 || empty.Total != 0 {
		t.Fatalf("empty brain events = %+v err=%v", empty, err)
	}
	before, err := ds.ChangeCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range []string{"one", "two", "three"} {
		if _, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{Owner: db.MemoryOwner{Kind: "agent", Ref: "builder"},
			Repo: "acme/widget", Scope: "repo", Key: key, Content: "fact " + key, SourceJob: "job-" + key}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	after, err := ds.ChangeCursor(ctx)
	if err != nil || before == after || !strings.HasSuffix(after, ".3") {
		t.Fatalf("cursor before=%q after=%q err=%v", before, after, err)
	}
	first, err := ds.BrainEvents(ctx, 0, 2)
	if err != nil || len(first.Events) != 2 || first.Events[0].Key != "three" || first.Events[1].Key != "two" || first.NextCursor != first.Events[1].ID || first.Total != 3 {
		t.Fatalf("first page = %+v err=%v", first, err)
	}
	second, err := ds.BrainEvents(ctx, first.NextCursor, 2)
	if err != nil || len(second.Events) != 1 || second.Events[0].Key != "one" || second.NextCursor != 0 || second.Total != 3 {
		t.Fatalf("second page = %+v err=%v", second, err)
	}
}

func TestBrainEventsHTTPAPIShape(t *testing.T) {
	home, store := memoryTestHome(t)
	if _, err := store.UpsertConfirmedMemory(context.Background(), db.ConfirmedMemory{
		Owner: db.MemoryOwner{Kind: "agent", Ref: "builder"}, Repo: "acme/widget", Scope: "repo",
		Key: "api", Content: "api fact", SourceJob: "job-api"}); err != nil {
		t.Fatal(err)
	}
	handler := newDashboardWebHandler(&webDataSource{home: home})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/brain/events?limit=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Events []map[string]any `json:"events"`
		Next   int64            `json:"nextCursor"`
		Total  int64            `json:"total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Events == nil || len(body.Events) != 1 || body.Events[0]["memoryId"] == nil || body.Events[0]["ownerKind"] != "agent" || body.Total != 1 {
		t.Fatalf("body=%s", recorder.Body.String())
	}
}

func TestBrainFactHTTPAPIIncludesHistoricalRowsAndErrors(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	activeID, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "active", Content: "Active fact. More detail."})
	if err != nil {
		t.Fatal(err)
	}
	retiredID, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: owner, Scope: "general", Key: "retired", Content: "retired fact"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RetireConfirmedMemory(ctx, retiredID, "outdated runbook"); err != nil {
		t.Fatal(err)
	}
	liveID, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "edition", Content: "before"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "edition", Content: "after"}, db.PreserveSupersededEdition()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListConfirmedMemoriesByOwnerKind(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var supersededID int64
	for _, row := range rows {
		if row.SupersededBy == liveID {
			supersededID = row.ID
		}
	}
	if supersededID == 0 {
		t.Fatal("superseded archive not found")
	}
	degenerateID, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "code-only", Content: "```go\nfmt.Println(\"hello\")\n```"})
	if err != nil {
		t.Fatal(err)
	}

	handler := newDashboardWebHandler(&webDataSource{home: home})
	tests := []struct {
		name, status, title string
		id                  int64
	}{
		{name: "active", id: activeID, status: "active", title: "Active fact"},
		{name: "retired", id: retiredID, status: "retired", title: "retired fact"},
		{name: "superseded", id: supersededID, status: "superseded", title: "before"},
		{name: "degenerate title fallback", id: degenerateID, status: "active", title: "code-only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/brain/fact?id="+strconv.FormatInt(tc.id, 10), nil))
			if recorder.Code != http.StatusOK || recorder.Header().Get(dashboardCacheHeader) != "miss" {
				t.Fatalf("status=%d cache=%q body=%s", recorder.Code, recorder.Header().Get(dashboardCacheHeader), recorder.Body.String())
			}
			var fact map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &fact); err != nil {
				t.Fatal(err)
			}
			if fact["status"] != tc.status || fact["title"] != tc.title || fact["ownerKind"] != "agent" || fact["ownerRef"] != "builder" || fact["content"] == "" || fact["firstConfirmedAt"] == "" || fact["updatedAt"] == "" {
				t.Fatalf("fact=%s", recorder.Body.String())
			}
			switch tc.status {
			case "active":
				if _, ok := fact["retiredAt"]; ok {
					t.Fatalf("active fact carries retiredAt: %s", recorder.Body.String())
				}
			case "retired":
				if fact["retiredReason"] != "outdated runbook" || fact["retiredAt"] == "" {
					t.Fatalf("retired fact=%s", recorder.Body.String())
				}
			case "superseded":
				if fact["supersededBy"] != float64(liveID) {
					t.Fatalf("superseded fact=%s", recorder.Body.String())
				}
			}
		})
	}

	for _, path := range []string{"/api/brain/fact", "/api/brain/fact?id=nope", "/api/brain/fact?id=-1"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusBadRequest || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("path=%s status=%d type=%q body=%s", path, recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/brain/fact?id=999999", nil))
	if recorder.Code != http.StatusNotFound || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("missing status=%d type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	var apiErr map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &apiErr); err != nil || apiErr["error"] == "" {
		t.Fatalf("missing error=%v body=%s", err, recorder.Body.String())
	}
}
