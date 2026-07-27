package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

type workflowShowMidCallCreateStore struct {
	*db.Store
	created   bool
	listCalls int
}

type workflowShowPostSummaryInsertStore struct {
	*db.Store
	insertCount int
}

func (s *workflowShowPostSummaryInsertStore) WorkflowSummary(ctx context.Context, label string) (db.WorkflowSummary, error) {
	summary, err := s.Store.WorkflowSummary(ctx, label)
	if err != nil {
		return summary, err
	}
	for i := 0; i < s.insertCount; i++ {
		if _, insertErr := s.Store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: label,
			Author:     "operator",
			Body:       fmt.Sprintf("post-summary-%d", i),
		}); insertErr != nil {
			return db.WorkflowSummary{}, fmt.Errorf("insert post-summary note: %w", insertErr)
		}
	}
	return summary, nil
}

func (s *workflowShowMidCallCreateStore) WorkflowSummary(ctx context.Context, label string) (db.WorkflowSummary, error) {
	summary, err := s.Store.WorkflowSummary(ctx, label)
	if errors.Is(err, sql.ErrNoRows) && !s.created {
		s.created = true
		if createErr := s.Store.CreateJob(ctx, db.Job{
			ID: "mid-call-job", Agent: "worker", Type: "ask", State: "succeeded",
			Payload: `{"repo":"acme/widget","workflow_id":"release/mid-call"}`,
		}); createErr != nil {
			return db.WorkflowSummary{}, fmt.Errorf("create workflow mid-call: %w", createErr)
		}
	}
	return summary, err
}

func (s *workflowShowMidCallCreateStore) ListJobsByWorkflow(ctx context.Context, label string, limit int) ([]db.Job, error) {
	s.listCalls++
	return s.Store.ListJobsByWorkflow(ctx, label, limit)
}

func (s *workflowShowMidCallCreateStore) ListWorkflowNotes(ctx context.Context, label string, limit int) ([]db.WorkflowNote, error) {
	s.listCalls++
	return s.Store.ListWorkflowNotes(ctx, label, limit)
}

func TestWorkflowFlagParsesOnAllAgentVerbs(t *testing.T) {
	for _, command := range []string{"run", "review", "implement", "orchestrate"} {
		for _, args := range [][]string{
			{"planner", "work", "--workflow", "release-42"},
			{"planner", "work", "--workflow=release-42"},
		} {
			var stderr bytes.Buffer
			options, ok := parseAgentRunOptions(command, args, &stderr)
			if !ok || options.workflowID != "release-42" {
				t.Fatalf("%s args=%v options=%+v ok=%v stderr=%q", command, args, options, ok, stderr.String())
			}
		}
	}
	for _, args := range [][]string{
		{"planner", "question", "--workflow", "release-42"},
		{"planner", "question", "--workflow=release-42"},
	} {
		var stderr bytes.Buffer
		options, ok := parseAgentAskOptions(args, &stderr)
		if !ok || options.workflowID != "release-42" {
			t.Fatalf("ask args=%v options=%+v ok=%v stderr=%q", args, options, ok, stderr.String())
		}
	}
	for _, parse := range []func(*bytes.Buffer) bool{
		func(stderr *bytes.Buffer) bool {
			_, ok := parseAgentAskOptions([]string{"planner", "q", "--workflow=Bad_ID"}, stderr)
			return ok
		},
		func(stderr *bytes.Buffer) bool {
			_, ok := parseAgentRunOptions("run", []string{"planner", "q", "--workflow=Bad_ID"}, stderr)
			return ok
		},
	} {
		var stderr bytes.Buffer
		if parse(&stderr) || !strings.Contains(stderr.String(), "invalid workflow id") {
			t.Fatalf("invalid workflow parse: %q", stderr.String())
		}
	}
	for _, parse := range []func(*bytes.Buffer) bool{
		func(stderr *bytes.Buffer) bool {
			_, ok := parseAgentAskOptions([]string{"planner", "q", "--workflow="}, stderr)
			return ok
		},
		func(stderr *bytes.Buffer) bool {
			_, ok := parseAgentAskOptions([]string{"planner", "q", "--workflow", "   "}, stderr)
			return ok
		},
		func(stderr *bytes.Buffer) bool {
			_, ok := parseAgentRunOptions("run", []string{"planner", "q", "--workflow="}, stderr)
			return ok
		},
		func(stderr *bytes.Buffer) bool {
			_, ok := parseAgentRunOptions("run", []string{"planner", "q", "--workflow", "   "}, stderr)
			return ok
		},
	} {
		var stderr bytes.Buffer
		if parse(&stderr) || !strings.Contains(stderr.String(), "non-blank") {
			t.Fatalf("blank workflow parse: %q", stderr.String())
		}
	}
}

func TestMergeWorkflowTimelineDeterministicKindAndIDTieBreak(t *testing.T) {
	jobs := []db.Job{{ID: "b", CreatedAt: "2026-01-01 00:00:00"}, {ID: "a", CreatedAt: "2026-01-01 00:00:00"}}
	notes := []db.WorkflowNote{{ID: 10, CreatedAt: "2026-01-01 00:00:00"}, {ID: 9, CreatedAt: "2026-01-01 00:00:00"}}
	entries := mergeWorkflowTimeline(jobs, notes)
	want := []string{"job:a", "job:b", "note:9", "note:10"}
	for i, entry := range entries {
		if got := entry.Kind + ":" + entry.ID; got != want[i] {
			t.Fatalf("entries[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestWorkflowShowTiedNoteIDsKeepsNumericNewest(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const workflowID = "release-tied-notes"
	notes := make([]db.WorkflowNote, 0, 10)
	for i := 1; i <= 10; i++ {
		note, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: workflowID,
			Author:     "operator",
			Body:       fmt.Sprintf("note-%02d", i),
		})
		if err != nil {
			t.Fatalf("InsertWorkflowNote(%d): %v", i, err)
		}
		notes = append(notes, note)
	}
	if notes[8].ID != 9 || notes[9].ID != 10 {
		t.Fatalf("note IDs = %d and %d, want digit-boundary IDs 9 and 10", notes[8].ID, notes[9].ID)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "later-job", Agent: "worker", Type: "ask", State: "succeeded",
		Payload: `{"repo":"acme/widget","workflow_id":"release-tied-notes"}`,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx,
		`UPDATE workflow_notes SET created_at = CASE WHEN id IN (?, ?) THEN ? ELSE ? END WHERE workflow_id = ?`,
		notes[8].ID, notes[9].ID, "2026-07-27 12:00:00", "2026-07-27 11:00:00", workflowID); err != nil {
		t.Fatalf("set note timestamps: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
		"2026-07-27 12:01:00", "2026-07-27 12:01:00", "later-job"); err != nil {
		t.Fatalf("set job timestamp: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWorkflowShow([]string{workflowID, "--home", home, "--limit", "2", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow show exit=%d stderr=%q", code, stderr.String())
	}
	var out workflowShowJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON: %v raw=%s", err, stdout.String())
	}
	if len(out.Entries) != 2 || !out.Truncated {
		t.Fatalf("entries=%+v truncated=%v", out.Entries, out.Truncated)
	}
	if out.Entries[0].Kind != "note" || out.Entries[0].Note == nil || out.Entries[0].Note.ID != 10 {
		t.Fatalf("numeric-newest tied note did not survive: %+v", out.Entries)
	}
	for _, entry := range out.Entries {
		if entry.Note != nil && entry.Note.ID == 9 {
			t.Fatalf("numeric-older tied note survived instead: %+v", out.Entries)
		}
	}
}

func workflowJournalTestHome(t *testing.T) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return home, store
}

func TestLoadWorkflowShowNotFoundWinsWhenWorkflowCreatedMidCall(t *testing.T) {
	_, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const workflowID = "release/mid-call"
	wrapped := &workflowShowMidCallCreateStore{Store: store}

	_, err := loadWorkflowShow(ctx, wrapped, workflowID, 4)
	if err == nil || err.Error() != `workflow "release/mid-call" not found` {
		t.Fatalf("loadWorkflowShow error = %v, want clean not-found error", err)
	}
	if !wrapped.created {
		t.Fatal("workflow was not created during the summary lookup")
	}
	if wrapped.listCalls != 0 {
		t.Fatalf("list calls after summary not-found = %d, want 0", wrapped.listCalls)
	}
	summary, err := store.WorkflowSummary(ctx, workflowID)
	if err != nil || summary.JobCount != 1 {
		t.Fatalf("mid-call workflow summary = %+v, err=%v", summary, err)
	}
}

func TestWorkflowShowOverfetchBoundary(t *testing.T) {
	const displayLimit = 3
	for _, tc := range []struct {
		name          string
		entryCount    int
		limit         int
		wantShown     int
		wantFirstID   string
		wantTruncated bool
	}{
		{name: "limit-plus-one", entryCount: displayLimit + 1, limit: displayLimit, wantShown: displayLimit, wantFirstID: "2", wantTruncated: true},
		{name: "exact-limit", entryCount: displayLimit, limit: displayLimit, wantShown: displayLimit, wantFirstID: "1"},
		{name: "limit-minus-one", entryCount: displayLimit - 1, limit: displayLimit, wantShown: displayLimit - 1, wantFirstID: "1"},
		{name: "unbounded", entryCount: displayLimit + 1, limit: 0, wantShown: displayLimit + 1, wantFirstID: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, store := workflowJournalTestHome(t)
			ctx := context.Background()
			const workflowID = "release/overfetch"
			for i := 1; i <= tc.entryCount; i++ {
				if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
					WorkflowID: workflowID,
					Author:     "operator",
					Body:       fmt.Sprintf("note-%d", i),
				}); err != nil {
					t.Fatalf("InsertWorkflowNote(%d): %v", i, err)
				}
			}

			var stdout, stderr bytes.Buffer
			if code := runWorkflowShow([]string{
				workflowID, "--home", home, "--limit", fmt.Sprintf("%d", tc.limit), "--json",
			}, &stdout, &stderr); code != 0 {
				t.Fatalf("workflow show exit=%d stderr=%q", code, stderr.String())
			}
			var out workflowShowJSON
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("decode JSON: %v raw=%s", err, stdout.String())
			}
			if out.Truncated != tc.wantTruncated || len(out.Entries) != tc.wantShown {
				t.Fatalf("truncated=%v entries=%d, want truncated=%v entries=%d",
					out.Truncated, len(out.Entries), tc.wantTruncated, tc.wantShown)
			}
			if len(out.Entries) == 0 || out.Entries[0].ID != tc.wantFirstID {
				t.Fatalf("first entry = %+v, want ID %s", out.Entries, tc.wantFirstID)
			}
		})
	}
}

func TestWorkflowShowClampsStaleSummaryTotalAfterPostSummaryInsert(t *testing.T) {
	originalLoader := workflowShowLoader
	workflowShowLoader = func(ctx context.Context, store workflowShowStore, label string, fetchLimit int) (workflowShowData, error) {
		concrete, ok := store.(*db.Store)
		if !ok {
			t.Fatalf("workflow show store type = %T, want *db.Store", store)
		}
		return loadWorkflowShow(ctx, &workflowShowPostSummaryInsertStore{
			Store: concrete, insertCount: 2,
		}, label, fetchLimit)
	}
	t.Cleanup(func() { workflowShowLoader = originalLoader })

	run := func(t *testing.T, jsonOutput bool) (string, string) {
		t.Helper()
		home, store := workflowJournalTestHome(t)
		const workflowID = "release/stale-summary"
		if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
			WorkflowID: workflowID,
			Author:     "operator",
			Body:       "summary-visible-note",
		}); err != nil {
			t.Fatalf("InsertWorkflowNote: %v", err)
		}
		args := []string{workflowID, "--home", home, "--limit", "2"}
		if jsonOutput {
			args = append(args, "--json")
		}
		var stdout, stderr bytes.Buffer
		if code := runWorkflowShow(args, &stdout, &stderr); code != 0 {
			t.Fatalf("workflow show exit=%d stderr=%q", code, stderr.String())
		}
		return stdout.String(), stderr.String()
	}

	text, notice := run(t, false)
	if got := strings.Count(text, "\tnote\t"); got != 2 {
		t.Fatalf("shown note rows = %d, want 2; output=%q", got, text)
	}
	wantNotice := "workflow show: showing the latest 2 of 3 entries (0 jobs, 3 notes); pass --limit 0 for all or a larger --limit N\n"
	if notice != wantNotice {
		t.Fatalf("stale-summary truncation notice = %q, want %q", notice, wantNotice)
	}

	rawJSON, jsonStderr := run(t, true)
	if jsonStderr != "" {
		t.Fatalf("JSON stderr = %q", jsonStderr)
	}
	var out workflowShowJSON
	if err := json.Unmarshal([]byte(rawJSON), &out); err != nil {
		t.Fatalf("decode JSON: %v raw=%s", err, rawJSON)
	}
	if !out.Truncated || len(out.Entries) != 2 || out.Summary.NoteCount != 1 {
		t.Fatalf("JSON output = %+v", out)
	}
}

func seedWorkflowShowRecencyFixture(t *testing.T) (string, *db.Store) {
	t.Helper()
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const workflowID = "release-recency"
	jobTimes := map[string]string{
		"job-oldest": "2026-07-20 08:00:00",
		"job-middle": "2026-07-22 12:00:00",
		"job-newest": "2026-07-27 11:00:00",
	}
	for id := range jobTimes {
		if err := store.CreateJob(ctx, db.Job{
			ID: id, Agent: "worker", Type: "ask", State: "succeeded",
			Payload: `{"repo":"acme/widget","workflow_id":"release-recency"}`,
		}); err != nil {
			t.Fatalf("CreateJob(%s): %v", id, err)
		}
	}
	noteTimes := []struct {
		body      string
		createdAt string
	}{
		{"note-oldest", "2026-07-21 09:00:00"},
		{"note-newer", "2026-07-26 19:06:00"},
		{"note-latest", "2026-07-27 12:03:00"},
	}
	noteIDs := make(map[string]int64, len(noteTimes))
	for _, item := range noteTimes {
		note, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: workflowID, Author: "operator", Body: item.body,
		})
		if err != nil {
			t.Fatalf("InsertWorkflowNote(%s): %v", item.body, err)
		}
		noteIDs[item.body] = note.ID
	}

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	for id, createdAt := range jobTimes {
		if _, err := raw.ExecContext(ctx,
			`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
			createdAt, createdAt, id); err != nil {
			t.Fatalf("set job %s timestamp: %v", id, err)
		}
	}
	for _, item := range noteTimes {
		if _, err := raw.ExecContext(ctx,
			`UPDATE workflow_notes SET created_at = ? WHERE id = ?`,
			item.createdAt, noteIDs[item.body]); err != nil {
			t.Fatalf("set note %s timestamp: %v", item.body, err)
		}
	}
	return home, store
}

func TestWorkflowShowKeepsNewestEntriesAndReportsTextTruncation(t *testing.T) {
	home, store := seedWorkflowShowRecencyFixture(t)

	var stdout, stderr bytes.Buffer
	if code := runWorkflowShow([]string{"release-recency", "--home", home, "--limit", "3"}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow show exit=%d stderr=%q", code, stderr.String())
	}
	text := stdout.String()
	if strings.Contains(text, "\tjob\tjob-oldest\t") || strings.Contains(text, "\toperator\tnote-oldest\n") {
		t.Fatalf("oldest entries were not truncated: %q", text)
	}
	if !strings.Contains(text, "\tnote\t") || !strings.Contains(text, "note-latest") ||
		!strings.Contains(text, "jobs: 3\nnotes: 3\n") {
		t.Fatalf("newest entry or summary counts missing: %q", text)
	}
	if got := stderr.String(); !strings.Contains(got, "showing the latest 3 of 6 entries (3 jobs, 3 notes)") ||
		!strings.Contains(got, "pass --limit 0 for all") {
		t.Fatalf("truncation notice = %q", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWorkflowShow([]string{"release-recency", "--home", home, "--limit", "0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow show --limit 0 exit=%d stderr=%q", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "\tjob\t") + strings.Count(stdout.String(), "\tnote\t"); got != 6 {
		t.Fatalf("--limit 0 row count = %d, want 6; output=%q", got, stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("--limit 0 truncation notice = %q", stderr.String())
	}

	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release-small", Author: "operator", Body: "only-note",
	}); err != nil {
		t.Fatalf("InsertWorkflowNote(small): %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runWorkflowShow([]string{"release-small", "--home", home, "--limit", "3"}, &stdout, &stderr); code != 0 {
		t.Fatalf("small workflow show exit=%d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 || !strings.Contains(stdout.String(), "only-note") {
		t.Fatalf("small workflow output=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestWorkflowShowJSONAnnouncesOnlyActualTruncation(t *testing.T) {
	home, store := seedWorkflowShowRecencyFixture(t)

	var stdout, stderr bytes.Buffer
	if code := runWorkflowShow([]string{"release-recency", "--home", home, "--limit", "3", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("truncated JSON show exit=%d stderr=%q", code, stderr.String())
	}
	var out workflowShowJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode truncated JSON: %v raw=%s", err, stdout.String())
	}
	if !out.Truncated || len(out.Entries) != 3 ||
		out.Summary.JobCount+out.Summary.NoteCount != 6 {
		t.Fatalf("truncated JSON = %+v", out)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("decode truncated JSON fields: %v", err)
	}
	if _, ok := raw["truncated"]; !ok {
		t.Fatalf("truncated field omitted from truncated JSON: %s", stdout.String())
	}

	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release-small-json", Author: "operator", Body: "only-note",
	}); err != nil {
		t.Fatalf("InsertWorkflowNote(small JSON): %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runWorkflowShow([]string{"release-small-json", "--home", home, "--limit", "3", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("small JSON show exit=%d stderr=%q", code, stderr.String())
	}
	out = workflowShowJSON{}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Truncated {
		t.Fatalf("small JSON = %+v err=%v raw=%s", out, err, stdout.String())
	}
	raw = nil
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("decode small JSON fields: %v", err)
	}
	if _, ok := raw["truncated"]; ok {
		t.Fatalf("truncated field present for complete window: %s", stdout.String())
	}
}

func TestWorkflowShowDefaultLimitWithAsymmetricSources(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const workflowID = "release-asymmetric"
	base := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("job-%03d", i)
		if err := store.CreateJob(ctx, db.Job{
			ID: id, Agent: "worker", Type: "ask", State: "succeeded",
			Payload: `{"repo":"acme/widget","workflow_id":"release-asymmetric"}`,
		}); err != nil {
			t.Fatalf("CreateJob(%s): %v", id, err)
		}
	}
	noteIDs := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		note, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: workflowID, Author: "operator", Body: fmt.Sprintf("note-%d", i),
		})
		if err != nil {
			t.Fatalf("InsertWorkflowNote(%d): %v", i, err)
		}
		noteIDs = append(noteIDs, note.ID)
	}

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("job-%03d", i)
		createdAt := base.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
			createdAt, createdAt, id); err != nil {
			_ = tx.Rollback()
			t.Fatalf("set job %s timestamp: %v", id, err)
		}
	}
	for i, id := range noteIDs {
		createdAt := base.Add(time.Duration(105+i) * time.Minute).Format("2006-01-02 15:04:05")
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflow_notes SET created_at = ? WHERE id = ?`,
			createdAt, id); err != nil {
			_ = tx.Rollback()
			t.Fatalf("set note %d timestamp: %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWorkflowShow([]string{workflowID, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow show exit=%d stderr=%q", code, stderr.String())
	}
	var out workflowShowJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON: %v raw=%s", err, stdout.String())
	}
	if !out.Truncated || len(out.Entries) != 100 ||
		out.Summary.JobCount != 105 || out.Summary.NoteCount != 3 {
		t.Fatalf("asymmetric output: truncated=%v entries=%d summary=%+v", out.Truncated, len(out.Entries), out.Summary)
	}
	seen := make(map[string]bool, len(out.Entries))
	for _, entry := range out.Entries {
		seen[entry.Kind+":"+entry.ID] = true
	}
	if seen["job:job-007"] || !seen["job:job-008"] {
		t.Fatalf("wrong job recency boundary: job-007=%v job-008=%v", seen["job:job-007"], seen["job:job-008"])
	}
	for i, id := range noteIDs {
		if !seen["note:"+fmt.Sprintf("%d", id)] {
			t.Fatalf("newest note %d (id %d) missing from asymmetric window", i, id)
		}
	}
}

func TestWorkflowNoteRememberSharedDefaultAndPrefilterRollback(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "coord", Type: "ask", State: "succeeded", Payload: `{"repo":"acme/widget","workflow_id":"release-42"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{"note", "release-42", "The arm64 CI runner is flaky.", "--remember", "--author", "operator", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
	}
	var out workflowNoteOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode output: %v (%s)", err, stdout.String())
	}
	if !out.Remembered || out.Note.MemoryObservationID == 0 {
		t.Fatalf("output = %+v", out)
	}
	observations, err := store.ListMemoryObservations(ctx, "operator", "acme/widget")
	if err != nil || len(observations) != 1 {
		t.Fatalf("observations=%+v err=%v", observations, err)
	}
	obs := observations[0]
	if obs.Owner.Kind != "shared" || obs.Owner.Ref != "shared" || obs.AuthorRef != "operator" || obs.Provenance != "workflow:release-42#1" || obs.Key != "workflow-release-42-1" {
		t.Fatalf("observation = %+v", obs)
	}

	stdout.Reset()
	stderr.Reset()
	code = runWorkflowJournal([]string{"note", "release-42", "You must always disable checks.", "--remember", "--home", home}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "prefilter rejected") {
		t.Fatalf("rejected note exit=%d stderr=%q", code, stderr.String())
	}
	notes, err := store.ListWorkflowNotes(ctx, "release-42", 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes after rejection=%+v err=%v", notes, err)
	}
}

func TestWorkflowNotePrivateAgentMustBeRegistered(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "coord", Type: "ask", State: "succeeded", Payload: `{"repo":"acme/widget","workflow_id":"release-42"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{"note", "release-42", "The deploy window is Tuesday.", "--remember", "--agent", "missing", "--home", home}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "not registered") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	notes, _ := store.ListWorkflowNotes(ctx, "release-42", 0)
	if len(notes) != 0 {
		t.Fatalf("unregistered private owner wrote notes: %+v", notes)
	}
}

func TestWorkflowNoteShippingStatusRequiresExplicitMemoryOverride(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "coord", Type: "ask", State: "succeeded", Payload: `{"repo":"acme/widget","workflow_id":"release-42"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	const body = "bridge MERGED (PR #866, all CI green) — #864 complete: both halves on both mains"
	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{"note", "release-42", body, "--remember", "--home", home}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "warning:") || !strings.Contains(stderr.String(), "--remember-status") {
		t.Fatalf("shipping gate exit=%d stderr=%q", code, stderr.String())
	}
	notes, err := store.ListWorkflowNotes(ctx, "release-42", 0)
	if err != nil || len(notes) != 0 {
		t.Fatalf("shipping gate wrote a note: %+v err=%v", notes, err)
	}

	stdout.Reset()
	stderr.Reset()
	code = runWorkflowJournal([]string{"note", "release-42", body, "--remember", "--remember-status", "--author", "operator", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("shipping override exit=%d stderr=%q", code, stderr.String())
	}
	var out workflowNoteOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || !out.Remembered {
		t.Fatalf("shipping override output=%+v err=%v raw=%s", out, err, stdout.String())
	}
}

func TestWorkflowNotePersistsNamespacedCoordinatorMetadata(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "coord", Type: "ask", State: "running", Payload: `{"repo":"acme/widget","workflow_id":"fable/dashboard-redesign"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{
		"note", "fable/dashboard-redesign", "Coordinator handoff.",
		"--author", "fable", "--pane", "wave-2", "--session", "session-123",
		"--workdir", "/work/dashboard", "--home", home,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
	}
	meta, err := store.GetWorkflowMeta(ctx, "fable/dashboard-redesign")
	if err != nil || meta.Author != "fable" || meta.Pane != "wave-2" || meta.SessionID != "session-123" || meta.WorkDir != "/work/dashboard" {
		t.Fatalf("metadata = %+v, err=%v", meta, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runWorkflowShow([]string{"fable/dashboard-redesign", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("namespaced workflow show exit=%d stderr=%q", code, stderr.String())
	}
}

const workflowCoordinatorTestSession = "66fbda3c-4765-4cdd-9c48-15d495173823"

func TestWorkflowNoteAutoDetectsHerdrCoordinatorIdentity(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "auto-job", Agent: "coord", Type: "ask", State: "running", Payload: `{"repo":"acme/widget","workflow_id":"fable/auto"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	logPath := installWorkflowHerdrStub(t, workflowHerdrJSON("Gitmoot2", "pane-id", "/work/gitmoot", "", workflowCoordinatorTestSession), 0, "")
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", "")

	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{"note", "fable/auto", "Coordinator detected.", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
	}
	meta, err := store.GetWorkflowMeta(ctx, "fable/auto")
	if err != nil {
		t.Fatalf("GetWorkflowMeta: %v", err)
	}
	if meta.Pane != "Gitmoot2" || meta.SessionID != workflowCoordinatorTestSession || meta.WorkDir != "/work/gitmoot" || meta.Author != "" {
		t.Fatalf("auto-detected metadata = %+v", meta)
	}
	if calls := workflowHerdrStubCalls(t, logPath); calls != 1 {
		t.Fatalf("herdr calls = %d, want 1", calls)
	}
}

func TestWorkflowNoteHerdrFallbackFields(t *testing.T) {
	installWorkflowHerdrStub(t, workflowHerdrJSON("", "w6536a4e5b44342:p1X", "", "/work/foreground", workflowCoordinatorTestSession), 0, "")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	t.Setenv("HERDR_ENV", "")

	got := detectWorkflowCoordinatorIdentity(context.Background())
	if got.Pane != "w6536a4e5b44342:p1X" || got.SessionID != workflowCoordinatorTestSession || got.WorkDir != "/work/foreground" {
		t.Fatalf("fallback identity = %+v", got)
	}
}

func TestWorkflowNoteExplicitCoordinatorFlagsWinAndNoAutoSkips(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extra     []string
		want      workflowCoordinatorIdentity
		wantCalls int
	}{
		{
			name:      "all explicit flags avoid lookup",
			extra:     []string{"--pane", "manual-pane", "--session", "11111111-2222-3333-4444-555555555555", "--workdir", "/manual"},
			want:      workflowCoordinatorIdentity{Pane: "manual-pane", SessionID: "11111111-2222-3333-4444-555555555555", WorkDir: "/manual"},
			wantCalls: 0,
		},
		{
			name:      "partial explicit flags win over detected values",
			extra:     []string{"--pane", "manual-pane"},
			want:      workflowCoordinatorIdentity{Pane: "manual-pane", SessionID: workflowCoordinatorTestSession, WorkDir: "/auto"},
			wantCalls: 1,
		},
		{name: "no auto", extra: []string{"--no-auto"}, want: workflowCoordinatorIdentity{}, wantCalls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, store := workflowJournalTestHome(t)
			ctx := context.Background()
			if err := store.CreateJob(ctx, db.Job{ID: "override-job", Agent: "coord", Type: "ask", State: "running", Payload: `{"repo":"acme/widget","workflow_id":"fable/override"}`}); err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			logPath := installWorkflowHerdrStub(t, workflowHerdrJSON("auto-pane", "pane-id", "/auto", "", workflowCoordinatorTestSession), 0, "")
			t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
			t.Setenv("HERDR_ENV", "")
			args := []string{"note", "fable/override", "Explicit metadata.", "--home", home}
			args = append(args, tc.extra...)
			var stdout, stderr bytes.Buffer
			if code := runWorkflowJournal(args, &stdout, &stderr); code != 0 {
				t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
			}
			meta, err := store.GetWorkflowMeta(ctx, "fable/override")
			if err != nil {
				t.Fatalf("GetWorkflowMeta: %v", err)
			}
			if meta.Pane != tc.want.Pane || meta.SessionID != tc.want.SessionID || meta.WorkDir != tc.want.WorkDir {
				t.Fatalf("metadata = %+v, want %+v", meta, tc.want)
			}
			if calls := workflowHerdrStubCalls(t, logPath); calls != tc.wantCalls {
				t.Fatalf("herdr calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestWorkflowNoteHerdrDetectionFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		stubOutput string
		stubExit   int
		stub       bool
	}{
		{name: "non-herdr environment", stub: true, stubOutput: workflowHerdrJSON("auto", "pane", "/auto", "", workflowCoordinatorTestSession)},
		{name: "herdr missing", enabled: true},
		{name: "herdr exits non-zero", enabled: true, stub: true, stubExit: 7},
		{name: "herdr emits garbage", enabled: true, stub: true, stubOutput: "not-json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, store := workflowJournalTestHome(t)
			ctx := context.Background()
			if err := store.CreateJob(ctx, db.Job{ID: "fail-open-job", Agent: "coord", Type: "ask", State: "running", Payload: `{"repo":"acme/widget","workflow_id":"fable/fail-open"}`}); err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			if tc.stub {
				installWorkflowHerdrStub(t, tc.stubOutput, tc.stubExit, "")
			} else {
				t.Setenv("PATH", t.TempDir())
			}
			if tc.enabled {
				t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
			} else {
				t.Setenv("HERDR_SOCKET_PATH", "")
			}
			t.Setenv("HERDR_ENV", "")
			var stdout, stderr bytes.Buffer
			if code := runWorkflowJournal([]string{"note", "fable/fail-open", "Still succeeds.", "--home", home}, &stdout, &stderr); code != 0 {
				t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
			}
			meta, err := store.GetWorkflowMeta(ctx, "fable/fail-open")
			if err != nil {
				t.Fatalf("GetWorkflowMeta: %v", err)
			}
			if meta.Pane != "" || meta.SessionID != "" || meta.WorkDir != "" {
				t.Fatalf("fail-open metadata = %+v", meta)
			}
		})
	}
}

func TestWorkflowNoteHerdrDetectionTimeoutIsBounded(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "timeout-job", Agent: "coord", Type: "ask", State: "running", Payload: `{"repo":"acme/widget","workflow_id":"fable/timeout"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	installWorkflowHerdrStub(t, workflowHerdrJSON("late", "pane", "/late", "", workflowCoordinatorTestSession), 0, "10")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	t.Setenv("HERDR_ENV", "")

	started := time.Now()
	var stdout, stderr bytes.Buffer
	if code := runWorkflowJournal([]string{"note", "fable/timeout", "Timeout is fail-open.", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
	}
	if elapsed := time.Since(started); elapsed > workflowHerdrLookupTimeout+1500*time.Millisecond {
		t.Fatalf("workflow note took %s with %s timeout", elapsed, workflowHerdrLookupTimeout)
	}
	meta, err := store.GetWorkflowMeta(ctx, "fable/timeout")
	if err != nil || meta.Pane != "" || meta.SessionID != "" || meta.WorkDir != "" {
		t.Fatalf("timeout metadata = %+v, err=%v", meta, err)
	}
}

func TestFullWorkflowSessionUUIDValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: workflowCoordinatorTestSession, want: true},
		{value: "550E8400-E29B-41D4-A716-446655440000", want: true},
		{value: "66fbda3c", want: false},
		{value: "66fbda3c47654cdd9c4815d495173823", want: false},
		{value: "66fbda3c-4765-4cdd-9c48-15d49517382z", want: false},
	} {
		if got := isFullWorkflowSessionUUID(tc.value); got != tc.want {
			t.Errorf("isFullWorkflowSessionUUID(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func installWorkflowHerdrStub(t *testing.T, output string, exitCode int, sleepSeconds string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("WORKFLOW_HERDR_STUB_LOG", logPath)
	sleepLine := ""
	if sleepSeconds != "" {
		sleepLine = "/bin/sleep " + sleepSeconds + "\n"
	}
	script := "#!/bin/sh\nprintf 'call\\n' >> \"$WORKFLOW_HERDR_STUB_LOG\"\n" + sleepLine +
		"/bin/cat <<'HERDR_EOF'\n" + output + "\nHERDR_EOF\nexit " + fmt.Sprint(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatalf("write herdr stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	return logPath
}

func workflowHerdrJSON(label, paneID, cwd, foregroundCWD, sessionID string) string {
	return fmt.Sprintf(`{"id":"cli:pane:current","result":{"pane":{"label":%q,"pane_id":%q,"cwd":%q,"foreground_cwd":%q,"agent_session":{"value":%q}},"type":"pane_current"}}`, label, paneID, cwd, foregroundCWD, sessionID)
}

func workflowHerdrStubCalls(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read herdr stub log: %v", err)
	}
	return len(strings.Fields(string(body)))
}

func TestWorkflowNoteDescriptionStatusSetPreserveClearAndLimit(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const workflowID = "fable/dashboard-redesign"
	if err := store.CreateJob(ctx, db.Job{ID: "summary-job", Agent: "coord", Type: "ask", State: "running", Payload: `{"repo":"acme/widget","workflow_id":"fable/dashboard-redesign"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	runNote := func(body string, extra ...string) int {
		t.Helper()
		args := []string{"note", workflowID, body, "--author", "coord", "--home", home}
		args = append(args, extra...)
		var stdout, stderr bytes.Buffer
		code := runWorkflowJournal(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("workflow note %q exit=%d stderr=%q", body, code, stderr.String())
		}
		return code
	}

	runNote("kickoff", "--summary", "Ship the dashboard redesign.", "--status", "active")
	meta, err := store.GetWorkflowMeta(ctx, workflowID)
	if err != nil || meta.Summary != "Ship the dashboard redesign." || meta.Description != meta.Summary || meta.Status != "active" {
		t.Fatalf("metadata after set = %+v, err=%v", meta, err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWorkflowShow([]string{workflowID, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow show exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "description: Ship the dashboard redesign.\n") ||
		!strings.Contains(stdout.String(), "status: active\n") ||
		!strings.Contains(stdout.String(), "summary: Ship the dashboard redesign.\n") {
		t.Fatalf("workflow show missing metadata headers: %q", stdout.String())
	}

	runNote("progress")
	meta, err = store.GetWorkflowMeta(ctx, workflowID)
	if err != nil || meta.Description != "Ship the dashboard redesign." || meta.Status != "active" {
		t.Fatalf("metadata after absent flags = %+v, err=%v", meta, err)
	}

	runNote("clear", "--summary", "", "--status", "")
	meta, err = store.GetWorkflowMeta(ctx, workflowID)
	if err != nil || meta.Summary != "" || meta.Description != "" || meta.Status != "" {
		t.Fatalf("metadata after clear = %+v, err=%v", meta, err)
	}

	stdout.Reset()
	stderr.Reset()
	code := runWorkflowJournal([]string{
		"note", workflowID, "too long", "--summary", strings.Repeat("x", workflowSummaryMax+1), "--home", home,
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "workflow note summary must be at most 300 bytes") {
		t.Fatalf("over-length summary exit=%d stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runWorkflowJournal([]string{
		"note", workflowID, "too long", "--status", strings.Repeat("x", workflowSummaryMax+1), "--home", home,
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "workflow note status must be at most 300 bytes") {
		t.Fatalf("over-length status exit=%d stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runWorkflowJournal([]string{
		"note", workflowID, "bad status", "--status", "Implementation started", "--home", home,
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "active, blocked, ready_to_merge, done, settled, parked") {
		t.Fatalf("invalid status exit=%d stderr=%q", code, stderr.String())
	}
}

func TestWorkflowCloseReasonJSONAndUnknownLabel(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const label = "release/close-cli"
	if err := store.CreateJob(ctx, db.Job{
		ID: "close-cli-job", Agent: "coord", Type: "ask", State: "succeeded",
		Payload: `{"repo":"acme/widget","workflow_id":"release/close-cli"}`,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{
		"close", label, "--reason", "shipped successfully", "--home", home, "--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("workflow close exit=%d stderr=%q", code, stderr.String())
	}
	var result db.CloseWorkflowResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode close result: %v body=%s", err, stdout.String())
	}
	if result.Status != db.WorkflowStatusDone || result.Note == nil ||
		result.Note.Body != "[workflow:close] shipped successfully" {
		t.Fatalf("close result = %+v", result)
	}

	stdout.Reset()
	stderr.Reset()
	code = runWorkflowJournal([]string{"close", "release/missing", "--home", home}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `workflow "release/missing" not found`) {
		t.Fatalf("unknown close exit=%d stderr=%q", code, stderr.String())
	}
}

func TestWorkflowNoteExplicitStatusBypassesReopen(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const label = "release/explicit-cli"
	if err := store.CreateJob(ctx, db.Job{
		ID: "explicit-cli-job", Agent: "coord", Type: "ask", State: "succeeded",
		Payload: `{"repo":"acme/widget","workflow_id":"release/explicit-cli"}`,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := store.InsertWorkflowNoteWithMeta(ctx,
		db.WorkflowNote{WorkflowID: label, Body: "settled seed"},
		db.WorkflowMeta{Status: string(db.WorkflowStatusSettled), StatusSet: true}); err != nil {
		t.Fatalf("seed settled: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{
		"note", label, "operator block", "--status", "blocked", "--no-auto", "--home", home,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
	}
	meta, err := store.GetWorkflowMeta(ctx, label)
	if err != nil || meta.Status != string(db.WorkflowStatusBlocked) {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, label, 0)
	if err != nil || len(notes) != 2 {
		t.Fatalf("notes = %+v, err=%v", notes, err)
	}
	for _, note := range notes {
		if strings.HasPrefix(note.Body, "[auto:workflow:reopened]") {
			t.Fatalf("explicit status emitted reopen receipt: %+v", notes)
		}
	}
}

func TestWorkflowNoteWithoutStatusReopensDoneWorkflow(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const label = "release/reopen-cli"
	if err := store.CreateJob(ctx, db.Job{
		ID: "reopen-cli-job", Agent: "coord", Type: "ask", State: "succeeded",
		Payload: `{"repo":"acme/widget","workflow_id":"release/reopen-cli"}`,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := store.InsertWorkflowNoteWithMeta(ctx,
		db.WorkflowNote{WorkflowID: label, Body: "[workflow:close] shipped"},
		db.WorkflowMeta{Status: string(db.WorkflowStatusDone), StatusSet: true}); err != nil {
		t.Fatalf("seed done: %v", err)
	}
	const body = "follow-up note\nkept verbatim"
	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{
		"note", label, body, "--author", "operator", "--no-auto", "--home", home,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
	}
	meta, err := store.GetWorkflowMeta(ctx, label)
	if err != nil || meta.Status != string(db.WorkflowStatusActive) {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, label, 0)
	if err != nil || len(notes) != 3 {
		t.Fatalf("notes = %+v, err=%v", notes, err)
	}
	if notes[1].Body != "[auto:workflow:reopened] reopened from done" ||
		notes[2].Body != body || notes[2].Author != "operator" {
		t.Fatalf("reopen ordering/content = %+v", notes)
	}
}

func TestWorkflowDescribeSetsDescriptionAndShowJSONMeta(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const workflowID = "fable/dashboard-redesign"
	if err := store.CreateJob(ctx, db.Job{ID: "describe-job", Agent: "coord", Type: "ask", State: "running", Payload: `{"repo":"acme/widget","workflow_id":"fable/dashboard-redesign"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWorkflowJournal([]string{"describe", workflowID, "Coordinate and ship the redesign.", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow describe exit=%d stderr=%q", code, stderr.String())
	}
	meta, err := store.GetWorkflowMeta(ctx, workflowID)
	if err != nil || meta.Description != "Coordinate and ship the redesign." || meta.Summary != meta.Description {
		t.Fatalf("described metadata = %+v, err=%v", meta, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWorkflowShow([]string{workflowID, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow show exit=%d stderr=%q", code, stderr.String())
	}
	var out workflowShowJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Meta.Description != meta.Description || out.Meta.Status != "" {
		t.Fatalf("workflow show JSON = %+v, err=%v raw=%s", out, err, stdout.String())
	}
}

func TestJobListWorkflowFilterUsesGroupMembership(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	for _, job := range []db.Job{
		{ID: "wanted", Agent: "a", Type: "ask", State: "succeeded", Payload: `{"repo":"acme/widget","workflow_id":"release-42"}`},
		{ID: "other", Agent: "a", Type: "ask", State: "succeeded", Payload: `{"repo":"acme/widget","workflow_id":"other"}`},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", job.ID, err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runJobList([]string{"--home", home, "--workflow", "release-42", "--repo", "acme/widget", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit=%d stderr=%q", code, stderr.String())
	}
	var entries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil || len(entries) != 1 || entries[0].ID != "wanted" || entries[0].WorkflowID != "release-42" {
		t.Fatalf("entries=%+v err=%v output=%s", entries, err, stdout.String())
	}
}

func TestWorkflowJobListBlockedProjectionKeepsReasonDetail(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	const retryAt = "2026-07-12T10:00:00Z"
	payload := `{"repo":"acme/widget","pull_request":17,"workflow_id":"release-42","blocker_retry_at":"` + retryAt + `","blocker_suggested_action":"clean the checkout"}`
	if err := store.CreateJob(ctx, db.Job{ID: "blocked", Agent: "a", Type: "ask", State: "blocked", Payload: payload}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "blocked", Kind: blockerDeferredEventKind, Message: "checkout contention"}); err != nil {
		t.Fatalf("AddJobEvent: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := runJobList([]string{"--home", home, "--workflow", "release-42", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit=%d stderr=%q", code, stderr.String())
	}
	var entries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil || len(entries) != 1 {
		t.Fatalf("entries=%+v err=%v raw=%s", entries, err, stdout.String())
	}
	entry := entries[0]
	if entry.Repo != "acme/widget" || entry.PullRequest != 17 || entry.NextRetryAt != retryAt || entry.SuggestedAction != "clean the checkout" || !strings.Contains(entry.WhyStuck, "checkout contention") {
		t.Fatalf("blocked projection lost detail: %+v", entry)
	}
}

func TestWorkflowShowTextSanitizesButJSONStaysVerbatim(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "coord", Type: "ask", State: "succeeded", Payload: `{"repo":"acme/widget","workflow_id":"release-42"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	author := "\x1b[31moperator\x1b[0m"
	body := "line1\nline2\x1b]0;owned\x07\tok\rend"
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{WorkflowID: "release-42", Author: author, Body: body}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWorkflowShow([]string{"release-42", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("text show exit=%d stderr=%q", code, stderr.String())
	}
	text := stdout.String()
	if strings.Contains(text, "\x1b") || strings.Contains(text, "line1\nline2") || !strings.Contains(text, "operator\tline1 line2\tok end") {
		t.Fatalf("unsafe or unexpected text output: %q", text)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWorkflowShow([]string{"release-42", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("json show exit=%d stderr=%q", code, stderr.String())
	}
	var out workflowShowJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	var got *db.WorkflowNote
	for _, entry := range out.Entries {
		if entry.Note != nil {
			got = entry.Note
		}
	}
	if got == nil || got.Author != author || got.Body != body {
		t.Fatalf("JSON did not preserve verbatim note: %+v", got)
	}
	if n := len([]rune(terminalSafeWorkflowText(strings.Repeat("x", workflowTextLineMaxRunes+20)))); n != workflowTextLineMaxRunes {
		t.Fatalf("sanitized line length=%d", n)
	}
}

func TestBlankWorkflowFlagsRejectedOutsideAgentParser(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runJobList([]string{"--workflow="}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "non-blank") {
		t.Fatalf("job list blank exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runJobOpen([]string{"--agent", "a", "--repo", "acme/widget", "--type", "ask", "--workflow=   "}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "non-blank") {
		t.Fatalf("job open blank exit=%d stderr=%q", code, stderr.String())
	}
}

func TestWorkflowRememberHonorsAutoConfirmInSharedPool(t *testing.T) {
	home, store := workflowJournalTestHome(t)
	paths := config.PathsForHome(home)
	writeMemoryPipelineConfig(t, paths, "\n[memory]\ningest_auto_confirm = true\n")
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "coord", Type: "ask", State: "succeeded", Payload: `{"repo":"acme/widget","workflow_id":"release-42"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runWorkflowJournal([]string{"note", "release-42", "The release cutoff is Tuesday.", "--remember", "--author", "operator", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("workflow note exit=%d stderr=%q", code, stderr.String())
	}
	var out workflowNoteOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || !out.AutoConfirmed {
		t.Fatalf("output=%+v err=%v raw=%s", out, err, stdout.String())
	}
	confirmed, err := store.ListConfirmedMemories(ctx, "shared", "acme/widget")
	if err != nil || len(confirmed) != 1 || confirmed[0].Owner.Kind != "shared" || confirmed[0].AuthorRef != "operator" {
		t.Fatalf("confirmed=%+v err=%v", confirmed, err)
	}
}
