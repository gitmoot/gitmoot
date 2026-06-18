package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openCockpitStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedCockpitTrainSession(t *testing.T, store *Store, sessionID string) {
	t.Helper()
	ctx := context.Background()
	runID := sessionID + "-review-001"
	if err := store.UpsertSkillOptTrainSession(ctx, SkillOptTrainSession{ID: sessionID, TemplateID: "planner", TargetRepo: "o/r", State: "items_ready"}); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, SkillOptTrainIteration{ID: sessionID + "-001", SessionID: sessionID, EvalRunID: runID, State: "items_ready"}); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, EvalRun{ID: runID, TemplateID: "planner", TargetRepo: "o/r", State: "draft"}); err != nil {
		t.Fatalf("eval run: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, EvalReviewItem{ID: runID + "-item-1", RunID: runID, ItemID: "item-1", Title: "One"}); err != nil {
		t.Fatalf("item: %v", err)
	}
	if err := store.UpsertSkillOptReviewWatch(ctx, SkillOptReviewWatch{RunID: runID, Repo: "o/r", IssueNumber: 1, Status: "watching"}); err != nil {
		t.Fatalf("watch: %v", err)
	}
}

func TestDeleteSkillOptTrainSessionCascades(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	seedCockpitTrainSession(t, store, "del-sess")
	seedCockpitTrainSession(t, store, "keep-sess")

	// An EXPIRED lock for the session must not block deletion and must be swept.
	expired := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000000000Z")
	if _, err := store.db.ExecContext(ctx, `INSERT INTO resource_locks(resource_key, owner_job_id, owner_token, acquired_at, expires_at, updated_at)
		VALUES ('skillopt-train:del-sess:del-sess-001', 'job-x', 'tok', CURRENT_TIMESTAMP, ?, CURRENT_TIMESTAMP)`, expired); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	if err := store.DeleteSkillOptTrainSession(ctx, "del-sess"); err != nil {
		t.Fatalf("DeleteSkillOptTrainSession: %v", err)
	}

	for _, check := range []struct {
		query string
		arg   string
	}{
		{`SELECT COUNT(*) FROM skillopt_train_sessions WHERE id = ?`, "del-sess"},
		{`SELECT COUNT(*) FROM skillopt_train_iterations WHERE session_id = ?`, "del-sess"},
		{`SELECT COUNT(*) FROM eval_runs WHERE id = ?`, "del-sess-review-001"},
		{`SELECT COUNT(*) FROM eval_review_items WHERE run_id = ?`, "del-sess-review-001"},
		{`SELECT COUNT(*) FROM skillopt_review_watches WHERE run_id = ?`, "del-sess-review-001"},
		{`SELECT COUNT(*) FROM resource_locks WHERE resource_key = ?`, "skillopt-train:del-sess:del-sess-001"},
	} {
		var count int
		if err := store.db.QueryRowContext(ctx, check.query, check.arg).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", check.query, err)
		}
		if count != 0 {
			t.Fatalf("expected 0 rows for %s (%s), got %d", check.query, check.arg, count)
		}
	}
	// The unrelated session is intact.
	if _, err := store.GetSkillOptTrainSession(ctx, "keep-sess"); err != nil {
		t.Fatalf("keep-sess should survive: %v", err)
	}
}

func TestDeleteSkillOptTrainSessionRefusesActiveLock(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	seedCockpitTrainSession(t, store, "busy-sess")
	live := time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05.000000000Z")
	if _, err := store.db.ExecContext(ctx, `INSERT INTO resource_locks(resource_key, owner_job_id, owner_token, acquired_at, expires_at, updated_at)
		VALUES ('skillopt-train:busy-sess:busy-sess-001', 'job-x', 'tok', CURRENT_TIMESTAMP, ?, CURRENT_TIMESTAMP)`, live); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	err := store.DeleteSkillOptTrainSession(ctx, "busy-sess")
	if err == nil || !strings.Contains(err.Error(), "active resource lock") {
		t.Fatalf("expected active-lock refusal, got %v", err)
	}
	if _, err := store.GetSkillOptTrainSession(ctx, "busy-sess"); err != nil {
		t.Fatalf("session must survive a refused delete: %v", err)
	}
}

func TestDeleteSkillOptTrainSessionNotFound(t *testing.T) {
	store := openCockpitStore(t)
	if err := store.DeleteSkillOptTrainSession(context.Background(), "ghost"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestCreatedRepoRecords(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	if err := store.RecordCreatedRepo(ctx, CreatedRepo{Repo: "o/ws", Purpose: "train", SessionID: "s1"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := store.RecordCreatedRepo(ctx, CreatedRepo{Repo: "o/other", Purpose: "train", SessionID: "s2"}); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	records, err := store.ListCreatedReposForSession(ctx, "s1")
	if err != nil || len(records) != 1 || records[0].Repo != "o/ws" {
		t.Fatalf("list = %+v err=%v", records, err)
	}
	if err := store.DeleteCreatedRepoRecord(ctx, "o/ws"); err != nil {
		t.Fatalf("delete record: %v", err)
	}
	records, err = store.ListCreatedReposForSession(ctx, "s1")
	if err != nil || len(records) != 0 {
		t.Fatalf("after delete = %+v err=%v", records, err)
	}
}

func TestRevertAgentTemplateVersion(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	base := AgentTemplate{ID: "planner", Name: "Planner", SourceRepo: "o/r", SourceRef: "main", SourcePath: "p.md", ResolvedCommit: "abc", Content: "v1 content"}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	v1, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("latest v1: %v", err)
	}
	// Add and promote a v2 so v1 becomes superseded.
	v2Template := base
	v2Template.Content = "v2 content"
	v2Template.ResolvedCommit = "def"
	v2, err := store.AddPendingAgentTemplateVersion(ctx, v2Template)
	if err != nil {
		t.Fatalf("add v2: %v", err)
	}
	if _, err := store.PromoteAgentTemplateVersion(ctx, v2.ID); err != nil {
		t.Fatalf("promote v2: %v", err)
	}

	// Revert to v1.
	reverted, err := store.RevertAgentTemplateVersion(ctx, "planner", v1.VersionID)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.State != "current" {
		t.Fatalf("reverted state = %q", reverted.State)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if current.Content != "v1 content" {
		t.Fatalf("template content after revert = %q, want v1 content", current.Content)
	}
	// v2 is now superseded.
	v2After, err := store.GetAgentTemplateVersionByID(ctx, v2.ID)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if v2After.State != "superseded" {
		t.Fatalf("v2 state after revert = %q", v2After.State)
	}
	// Reverting a non-superseded version is refused.
	if _, err := store.RevertAgentTemplateVersion(ctx, "planner", v2.ID); err != nil {
		t.Fatalf("revert back to v2 (superseded now) should work: %v", err)
	}
	if _, err := store.RevertAgentTemplateVersion(ctx, "planner", v2.ID); err == nil {
		t.Fatal("reverting the CURRENT version should be refused")
	}
}

func TestDeleteAgentChecked(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{Name: "worker", Runtime: "codex"}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "j1", Agent: "worker", Type: "ask", State: "queued"}); err != nil {
		t.Fatalf("job: %v", err)
	}
	// Refused while a queued job references the agent — wraps the sentinel so
	// callers can classify with errors.Is, not message text.
	if err := store.DeleteAgentChecked(ctx, "worker"); err == nil || !errors.Is(err, ErrAgentHasActiveJobs) {
		t.Fatalf("expected job-reference refusal (ErrAgentHasActiveJobs), got %v", err)
	}
	if err := store.UpdateJobState(ctx, "j1", "succeeded"); err != nil {
		t.Fatalf("settle job: %v", err)
	}
	if err := store.DeleteAgentChecked(ctx, "worker"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteAgentChecked(ctx, "worker"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found on second delete, got %v", err)
	}
}

func TestUpdateAgentRuntime(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	original := Agent{
		Name:           "worker",
		Role:           "implement",
		Runtime:        "codex",
		RuntimeRef:     "sess-abc",
		RepoScope:      "owner/repo",
		TemplateID:     "worker-tpl",
		Capabilities:   []string{"implement", "review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}
	if err := store.UpsertAgent(ctx, original); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}

	// Unknown runtime is rejected, leaving the row untouched.
	if err := store.UpdateAgentRuntime(ctx, "worker", "gpt"); err == nil || !strings.Contains(err.Error(), "unknown runtime") {
		t.Fatalf("expected unknown-runtime error, got %v", err)
	}
	// Missing agent errors.
	if err := store.UpdateAgentRuntime(ctx, "ghost", "claude"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected not-registered error, got %v", err)
	}

	if err := store.UpdateAgentRuntime(ctx, "worker", "claude"); err != nil {
		t.Fatalf("switch runtime: %v", err)
	}
	got, err := store.GetAgent(ctx, "worker")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Runtime != "claude" {
		t.Fatalf("runtime = %q, want claude", got.Runtime)
	}
	if got.RuntimeRef != "" {
		t.Fatalf("runtime_ref = %q, want cleared", got.RuntimeRef)
	}
	// Everything else preserved.
	if got.Role != "implement" || got.RepoScope != "owner/repo" || got.TemplateID != "worker-tpl" ||
		got.AutonomyPolicy != "auto" || strings.Join(got.Capabilities, ",") != "implement,review" {
		t.Fatalf("switch runtime altered preserved fields: %+v", got)
	}
}

func TestLatestJobEvents(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, Job{ID: "job-a", Agent: "planner", Type: "ask", State: "failed"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "job-b", Agent: "planner", Type: "review", State: "queued"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, event := range []JobEvent{
		{JobID: "job-a", Kind: "queued", Message: "created"},
		{JobID: "job-a", Kind: "failed", Message: "boom"},
		{JobID: "job-b", Kind: "queued", Message: "created"},
	} {
		if err := store.AddJobEvent(ctx, event); err != nil {
			t.Fatalf("AddJobEvent: %v", err)
		}
	}
	latest, err := store.LatestJobEvents(ctx)
	if err != nil {
		t.Fatalf("LatestJobEvents: %v", err)
	}
	if got := latest["job-a"]; got.Kind != "failed" || got.Message != "boom" {
		t.Fatalf("job-a latest = %+v", got)
	}
	if got := latest["job-b"]; got.Kind != "queued" || got.Message != "created" {
		t.Fatalf("job-b latest = %+v", got)
	}
	if _, ok := latest["job-missing"]; ok {
		t.Fatal("jobs without events must be absent from the map")
	}
}

func TestAdoptCreatedRepoRecords(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	// Pending (form-created, no session yet) and owned rows.
	if err := store.RecordCreatedRepo(ctx, CreatedRepo{Repo: "o/pending", Purpose: "train"}); err != nil {
		t.Fatalf("record pending: %v", err)
	}
	if err := store.RecordCreatedRepo(ctx, CreatedRepo{Repo: "o/owned", Purpose: "train", SessionID: "other-session"}); err != nil {
		t.Fatalf("record owned: %v", err)
	}
	if err := store.AdoptCreatedRepoRecords(ctx, "session-1", []string{"o/pending", "o/owned", "o/missing"}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	adopted, err := store.ListCreatedReposForSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(adopted) != 1 || adopted[0].Repo != "o/pending" {
		t.Fatalf("adopted = %+v", adopted)
	}
	// The owned row keeps its original session.
	owned, err := store.ListCreatedReposForSession(ctx, "other-session")
	if err != nil || len(owned) != 1 {
		t.Fatalf("owned rows disturbed: %v %+v", err, owned)
	}
	if err := store.AdoptCreatedRepoRecords(ctx, "", nil); err == nil {
		t.Fatal("empty session id must error")
	}
}
