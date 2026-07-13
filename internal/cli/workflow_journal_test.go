package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

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
	notes := []db.WorkflowNote{{ID: 2, CreatedAt: "2026-01-01 00:00:00"}, {ID: 1, CreatedAt: "2026-01-01 00:00:00"}}
	entries := mergeWorkflowTimeline(jobs, notes)
	want := []string{"job:a", "job:b", "note:1", "note:2"}
	for i, entry := range entries {
		if got := entry.Kind + ":" + entry.ID; got != want[i] {
			t.Fatalf("entries[%d] = %q, want %q", i, got, want[i])
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
