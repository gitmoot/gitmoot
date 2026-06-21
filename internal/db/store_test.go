package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenMigratesSchema(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	for _, table := range []string{
		"repos",
		"agents",
		"goals",
		"tasks",
		"pull_requests",
		"seen_comments",
		"jobs",
		"job_events",
		"branch_locks",
		"lock_events",
		"resource_locks",
		"merge_gates",
		"agent_repos",
		"agent_templates",
		"agent_template_versions",
		"eval_artifacts",
		"eval_runs",
		"eval_review_items",
		"eval_review_options",
		"feedback_events",
		"ranked_feedback_events",
		"skillopt_train_sessions",
		"skillopt_train_iterations",
		"skillopt_review_watches",
		"skillopt_judge_outcomes",
		"interactive_prompts",
	} {
		ok, err := store.HasTable(ctx, table)
		if err != nil {
			t.Fatalf("HasTable(%s) returned error: %v", table, err)
		}
		if !ok {
			t.Fatalf("expected table %s to exist", table)
		}
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate returned error: %v", err)
	}
}

func TestOpenConfiguresSQLiteContentionPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}

	var busyTimeout int
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout returned error: %v", err)
	}
	if busyTimeout != sqliteBusyTimeoutMillis {
		t.Fatalf("busy_timeout = %d, want %d", busyTimeout, sqliteBusyTimeoutMillis)
	}
	var journalMode string
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode returned error: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly returned error: %v", err)
	}
	defer readOnly.Close()
	if err := readOnly.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read-only PRAGMA busy_timeout returned error: %v", err)
	}
	if busyTimeout != sqliteBusyTimeoutMillis {
		t.Fatalf("read-only busy_timeout = %d, want %d", busyTimeout, sqliteBusyTimeoutMillis)
	}
}

func TestOpenConfiguresSynchronousNormal(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	// synchronous=NORMAL reads back as 1 on the writable connection.
	var synchronous int
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("PRAGMA synchronous returned error: %v", err)
	}
	if synchronous != 1 {
		t.Fatalf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}

	// wal_autocheckpoint stays at the sane SQLite default so long-lived read
	// connections do not let the WAL grow unbounded.
	var autocheckpoint int
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA wal_autocheckpoint`).Scan(&autocheckpoint); err != nil {
		t.Fatalf("PRAGMA wal_autocheckpoint returned error: %v", err)
	}
	if autocheckpoint <= 0 {
		t.Fatalf("wal_autocheckpoint = %d, want a positive default", autocheckpoint)
	}
}

func TestInteractivePromptStorageAndAnswerValidation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	prompt := InteractivePrompt{
		ID:            "train-init-template",
		Question:      "Which template should Gitmoot train?",
		Choices:       []string{"planner", "writer", "planner"},
		Default:       "planner",
		Required:      true,
		AnswerFormat:  "choice",
		SourceCommand: "gitmoot skillopt train init",
	}
	if err := store.UpsertInteractivePrompt(ctx, prompt); err != nil {
		t.Fatalf("UpsertInteractivePrompt returned error: %v", err)
	}

	pending, err := store.ListInteractivePrompts(ctx, InteractivePromptStatePending)
	if err != nil {
		t.Fatalf("ListInteractivePrompts returned error: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != prompt.ID || len(pending[0].Choices) != 2 {
		t.Fatalf("pending prompts = %+v", pending)
	}

	if _, err := store.AnswerInteractivePrompt(ctx, prompt.ID, "missing", "test"); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("AnswerInteractivePrompt invalid value error = %v", err)
	}
	answered, err := store.AnswerInteractivePrompt(ctx, prompt.ID, "", "agent")
	if err != nil {
		t.Fatalf("AnswerInteractivePrompt default returned error: %v", err)
	}
	if answered.State != InteractivePromptStateResolved || answered.AnswerValue != "planner" || answered.AnswerSource != "agent" || answered.AnsweredAt == "" {
		t.Fatalf("answered prompt = %+v", answered)
	}
	if _, err := store.AnswerInteractivePrompt(ctx, prompt.ID, "writer", "agent"); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("AnswerInteractivePrompt second answer error = %v", err)
	}
	if err := store.UpsertInteractivePrompt(ctx, InteractivePrompt{
		ID:            prompt.ID,
		Question:      "Which template should Gitmoot train now?",
		Choices:       []string{"planner", "writer"},
		Default:       "writer",
		Required:      true,
		AnswerFormat:  "choice",
		SourceCommand: "gitmoot skillopt train init",
	}); err != nil {
		t.Fatalf("UpsertInteractivePrompt retry returned error: %v", err)
	}
	preserved, err := store.GetInteractivePrompt(ctx, prompt.ID)
	if err != nil {
		t.Fatalf("GetInteractivePrompt returned error: %v", err)
	}
	if preserved.State != InteractivePromptStateResolved || preserved.AnswerValue != "planner" || preserved.AnswerSource != "agent" || preserved.AnsweredAt != answered.AnsweredAt {
		t.Fatalf("preserved prompt = %+v, answered_at before %q", preserved, answered.AnsweredAt)
	}
}

func TestDeleteInteractivePrompt(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertInteractivePrompt(ctx, InteractivePrompt{ID: "present", Question: "q", Required: true}); err != nil {
		t.Fatalf("UpsertInteractivePrompt returned error: %v", err)
	}
	if err := store.DeleteInteractivePrompt(ctx, "present"); err != nil {
		t.Fatalf("DeleteInteractivePrompt returned error: %v", err)
	}
	if err := store.DeleteInteractivePrompt(ctx, "present"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("DeleteInteractivePrompt missing error = %v", err)
	}
	if err := store.DeleteInteractivePrompt(ctx, "  "); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("DeleteInteractivePrompt empty id error = %v", err)
	}
}

func TestDeleteInteractivePromptsByState(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	for _, id := range []string{"p1", "p2", "r1"} {
		if err := store.UpsertInteractivePrompt(ctx, InteractivePrompt{ID: id, Question: "q", Required: true}); err != nil {
			t.Fatalf("UpsertInteractivePrompt(%s) returned error: %v", id, err)
		}
	}
	if _, err := store.AnswerInteractivePrompt(ctx, "r1", "done", "test"); err != nil {
		t.Fatalf("AnswerInteractivePrompt returned error: %v", err)
	}

	removed, err := store.DeleteInteractivePromptsByState(ctx, InteractivePromptStateResolved)
	if err != nil {
		t.Fatalf("DeleteInteractivePromptsByState(resolved) returned error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("resolved removed = %d, want 1", removed)
	}

	removedAll, err := store.DeleteInteractivePromptsByState(ctx, "")
	if err != nil {
		t.Fatalf("DeleteInteractivePromptsByState(all) returned error: %v", err)
	}
	if removedAll != 2 {
		t.Fatalf("all removed = %d, want 2", removedAll)
	}

	remaining, err := store.ListInteractivePrompts(ctx, "")
	if err != nil {
		t.Fatalf("ListInteractivePrompts returned error: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining prompts = %+v", remaining)
	}
}

func TestListSkillOptTrainSessions(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	empty, err := store.ListSkillOptTrainSessions(ctx)
	if err != nil {
		t.Fatalf("ListSkillOptTrainSessions empty returned error: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no sessions, got %d", len(empty))
	}
	for _, id := range []string{"session-a", "session-b"} {
		if err := store.UpsertSkillOptTrainSession(ctx, SkillOptTrainSession{ID: id, TemplateID: "planner", State: "items_ready"}); err != nil {
			t.Fatalf("UpsertSkillOptTrainSession(%s) returned error: %v", id, err)
		}
	}
	sessions, err := store.ListSkillOptTrainSessions(ctx)
	if err != nil {
		t.Fatalf("ListSkillOptTrainSessions returned error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	seen := map[string]bool{}
	for _, session := range sessions {
		seen[session.ID] = true
	}
	if !seen["session-a"] || !seen["session-b"] {
		t.Fatalf("missing sessions: %+v", sessions)
	}
}

func TestSkillOptTrainSessionAndIterationStorage(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	session := SkillOptTrainSession{
		ID:                "landing-page-session",
		TemplateID:        "landing-page-designer",
		TemplateVersionID: "landing-page-designer@v3",
		TargetRepo:        "owner/product",
		WorkspaceRepo:     "owner/product-training",
		PreviewRepo:       "owner/product-previews",
		RequestSummary:    "Train a landing page designer with GitHub review previews.",
		TaskKind:          "design",
		State:             "workspace_ready",
		MetadataJSON:      `{"source":"test"}`,
	}
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	storedSession, err := store.GetSkillOptTrainSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	if storedSession.TemplateVersionID != session.TemplateVersionID || storedSession.WorkspaceRepo != session.WorkspaceRepo || storedSession.TaskKind != "design" || storedSession.MetadataJSON != session.MetadataJSON {
		t.Fatalf("stored session = %+v", storedSession)
	}

	iteration := SkillOptTrainIteration{
		ID:                    "landing-page-session-001",
		SessionID:             session.ID,
		EvalRunID:             "landing-page-trial-001",
		BaseTemplateVersionID: "landing-page-designer@v3",
		CandidateVersionID:    "landing-page-designer@v4",
		Mode:                  EvalRunModeExplore,
		ExplorationLevel:      ExplorationLevelHigh,
		State:                 "candidate_created",
		IssueRepo:             "owner/product-training",
		IssueNumber:           67,
		IssueURL:              "https://github.com/owner/product-training/issues/67",
		PullRequestRepo:       "owner/product-training",
		PullRequestNumber:     68,
		PullRequestURL:        "https://github.com/owner/product-training/pull/68",
		DecisionReason:        "candidate accepted for validation",
		MetadataJSON:          `{"step":"candidate"}`,
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration returned error: %v", err)
	}
	storedIteration, err := store.GetSkillOptTrainIteration(ctx, iteration.ID)
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration returned error: %v", err)
	}
	if storedIteration.CandidateVersionID != iteration.CandidateVersionID || storedIteration.IssueNumber != 67 || storedIteration.PullRequestNumber != 68 || storedIteration.DecisionReason != iteration.DecisionReason || storedIteration.MetadataJSON != iteration.MetadataJSON {
		t.Fatalf("stored iteration = %+v", storedIteration)
	}
	iterations, err := store.ListSkillOptTrainIterations(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListSkillOptTrainIterations returned error: %v", err)
	}
	if len(iterations) != 1 || iterations[0].ID != iteration.ID {
		t.Fatalf("iterations = %+v", iterations)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != iteration.ID {
		t.Fatalf("latest = %+v", latest)
	}

	newer := SkillOptTrainIteration{
		ID:        "aaa-lexically-earlier",
		SessionID: session.ID,
		State:     "workspace_ready",
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, newer); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration newer returned error: %v", err)
	}
	iterations, err = store.ListSkillOptTrainIterations(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListSkillOptTrainIterations after newer returned error: %v", err)
	}
	if len(iterations) != 2 || iterations[0].ID != iteration.ID || iterations[1].ID != newer.ID {
		t.Fatalf("iterations after same-second insert = %+v", iterations)
	}
	latest, err = store.GetLatestSkillOptTrainIteration(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration newer returned error: %v", err)
	}
	if latest.ID != newer.ID {
		t.Fatalf("latest after same-second insert = %+v, want %+v", latest, newer)
	}
}

func TestSkillOptTrainStorageDefaultsAndValidation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertSkillOptTrainSession(ctx, SkillOptTrainSession{ID: "session-1", TemplateID: "planner"}); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession defaults returned error: %v", err)
	}
	session, err := store.GetSkillOptTrainSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	if session.State != "request_confirmed" || session.TaskKind != "custom" {
		t.Fatalf("session defaults = %+v", session)
	}

	if err := store.UpsertSkillOptTrainSession(ctx, SkillOptTrainSession{ID: "missing-template"}); err == nil || !strings.Contains(err.Error(), "template id") {
		t.Fatalf("missing template error = %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, SkillOptTrainIteration{ID: "iteration-1"}); err == nil || !strings.Contains(err.Error(), "session id") {
		t.Fatalf("missing session error = %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, SkillOptTrainIteration{ID: "iteration-1", SessionID: "session-1", Mode: "wide"}); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("invalid mode error = %v", err)
	}

	if err := store.UpsertSkillOptTrainIteration(ctx, SkillOptTrainIteration{ID: "iteration-1", SessionID: "session-1"}); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration defaults returned error: %v", err)
	}
	iteration, err := store.GetSkillOptTrainIteration(ctx, "iteration-1")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.Mode != EvalRunModeExplore || iteration.ExplorationLevel != ExplorationLevelHigh || iteration.State != "request_confirmed" {
		t.Fatalf("iteration defaults = %+v", iteration)
	}
}

func TestSkillOptReviewWatchStorage(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	expectedItems, err := json.Marshal([]string{"item-001", "item-002"})
	if err != nil {
		t.Fatalf("marshal expected items: %v", err)
	}
	watch := SkillOptReviewWatch{
		Repo:                  " owner/previews ",
		IssueNumber:           67,
		RunID:                 " landing-page-review-001 ",
		ExpectedItemIDsJSON:   string(expectedItems),
		LastSeenCommentID:     100,
		LastImportErrorHash:   " error-hash ",
		StaleAfter:            "2026-06-05T12:00:00Z",
		StaleThresholdSeconds: 86400,
		StaleNotified:         true,
		MetadataJSON:          `{"source":"test"}`,
	}
	if err := store.UpsertSkillOptReviewWatch(ctx, watch); err != nil {
		t.Fatalf("UpsertSkillOptReviewWatch returned error: %v", err)
	}
	stored, err := store.GetSkillOptReviewWatch(ctx, "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if stored.Repo != "owner/previews" ||
		stored.RunID != "landing-page-review-001" ||
		stored.Status != SkillOptReviewWatchStatusWatching ||
		stored.LastSeenCommentID != 100 ||
		stored.LastImportErrorHash != "error-hash" ||
		stored.StaleAfter != watch.StaleAfter ||
		stored.StaleThresholdSeconds != 86400 ||
		!stored.StaleNotified ||
		stored.ExpectedItemIDsJSON != string(expectedItems) {
		t.Fatalf("stored watch = %+v", stored)
	}
	if err := store.UpsertSkillOptReviewWatch(ctx, SkillOptReviewWatch{
		Repo:        "owner/previews",
		IssueNumber: 67,
		RunID:       "landing-page-review-001",
		Status:      SkillOptReviewWatchStatusImported,
	}); err != nil {
		t.Fatalf("UpsertSkillOptReviewWatch update returned error: %v", err)
	}
	watches, err := store.ListSkillOptReviewWatches(ctx, SkillOptReviewWatchStatusImported)
	if err != nil {
		t.Fatalf("ListSkillOptReviewWatches returned error: %v", err)
	}
	if len(watches) != 1 || watches[0].Status != SkillOptReviewWatchStatusImported || watches[0].IssueNumber != 67 {
		t.Fatalf("imported watches = %+v", watches)
	}
	if err := store.UpsertSkillOptReviewWatch(ctx, SkillOptReviewWatch{Repo: "owner/previews", IssueNumber: 68, RunID: "run", Status: "paused"}); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("invalid status error = %v", err)
	}
}

func TestSkillOptTrainPublicationAndReviewWatchStorageIsTransactional(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	session := SkillOptTrainSession{ID: "session-1", TemplateID: "planner", State: "review_published"}
	iteration := SkillOptTrainIteration{ID: "session-1-001", SessionID: "session-1", EvalRunID: "review-001", State: "review_published", IssueRepo: "owner/review", IssueNumber: 8}
	watch := SkillOptReviewWatch{Repo: "owner/review", IssueNumber: 8, RunID: "review-001", Status: SkillOptReviewWatchStatusWatching}
	if err := store.UpsertSkillOptTrainSessionIterationAndReviewWatch(ctx, session, iteration, watch); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionIterationAndReviewWatch returned error: %v", err)
	}
	storedIteration, err := store.GetSkillOptTrainIteration(ctx, "session-1-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration returned error: %v", err)
	}
	storedWatch, err := store.GetSkillOptReviewWatch(ctx, "owner/review", 8)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if storedIteration.State != "review_published" || storedWatch.RunID != "review-001" {
		t.Fatalf("stored iteration=%+v watch=%+v", storedIteration, storedWatch)
	}

	if err := store.UpsertSkillOptTrainSessionIterationAndReviewWatch(ctx,
		SkillOptTrainSession{ID: "session-2", TemplateID: "planner", State: "review_published"},
		SkillOptTrainIteration{ID: "session-2-001", SessionID: "session-2", EvalRunID: "review-002", State: "review_published", IssueRepo: "owner/review", IssueNumber: 9},
		SkillOptReviewWatch{Repo: "owner/review", IssueNumber: 9, RunID: "review-002", Status: "paused"},
	); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("invalid transactional watch error = %v", err)
	}
	if _, err := store.GetSkillOptTrainSession(ctx, "session-2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invalid transactional upsert stored session err=%v", err)
	}
}

func TestEvalStorageMethods(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	artifact := EvalArtifact{
		ID:        "artifact-a",
		Hash:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MediaType: "text/markdown",
		SizeBytes: 42,
		Driver:    "text",
	}
	if err := store.UpsertEvalArtifact(ctx, artifact); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	storedArtifact, err := store.GetEvalArtifact(ctx, artifact.ID)
	if err != nil {
		t.Fatalf("GetEvalArtifact returned error: %v", err)
	}
	if storedArtifact.Hash != artifact.Hash || storedArtifact.MediaType != "text/markdown" || storedArtifact.Driver != "text" {
		t.Fatalf("stored artifact = %+v", storedArtifact)
	}

	run := EvalRun{
		ID:                "run-1",
		TemplateID:        "planner",
		TemplateVersionID: "planner@v2",
		TargetRepo:        "owner/repo",
		State:             "review",
		Mode:              EvalRunModeExplore,
		ExplorationLevel:  ExplorationLevelHigh,
		OptionsCount:      5,
		MetadataJSON:      `{"seed":1}`,
	}
	if err := store.UpsertEvalRun(ctx, run); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	storedRun, err := store.GetEvalRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if storedRun.TemplateVersionID != "planner@v2" || storedRun.Mode != EvalRunModeExplore || storedRun.ExplorationLevel != ExplorationLevelHigh || storedRun.OptionsCount != 5 || storedRun.MetadataJSON != `{"seed":1}` {
		t.Fatalf("stored run = %+v", storedRun)
	}

	item := EvalReviewItem{
		RunID:               run.ID,
		ItemID:              "item-001",
		Title:               "README plan",
		SourceArtifactID:    artifact.ID,
		BaselineArtifactID:  artifact.ID,
		CandidateArtifactID: artifact.ID,
		PreviewArtifactID:   artifact.ID,
		DiffArtifactID:      artifact.ID,
		MetadataJSON:        `{"path":"README.md"}`,
	}
	if err := store.UpsertEvalReviewItem(ctx, item); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("review items len = %d, want 1", len(items))
	}
	if items[0].ID != "run-1/item-001" || items[0].DiffArtifactID != artifact.ID || items[0].MetadataJSON != `{"path":"README.md"}` {
		t.Fatalf("review item = %+v", items[0])
	}
}

func TestEvalRunDefaultsToValidationMode(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertEvalRun(ctx, EvalRun{ID: "run-1", State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	run, err := store.GetEvalRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.Mode != EvalRunModeValidate || run.ExplorationLevel != ExplorationLevelLow || run.OptionsCount != 2 {
		t.Fatalf("run defaults = %+v", run)
	}
}

func TestEvalRunRejectsInvalidModeAndExplorationLevel(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertEvalRun(ctx, EvalRun{ID: "run-1", Mode: "wide"}); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("invalid mode error = %v", err)
	}
	if err := store.UpsertEvalRun(ctx, EvalRun{ID: "run-1", ExplorationLevel: "huge"}); err == nil || !strings.Contains(err.Error(), "exploration level") {
		t.Fatalf("invalid exploration level error = %v", err)
	}
	if err := store.UpsertEvalRun(ctx, EvalRun{ID: "run-1", OptionsCount: 1}); err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("invalid options count error = %v", err)
	}
}

func TestRankedReviewStorageAndPairwisePreferences(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertEvalRun(ctx, EvalRun{
		ID:               "run-ranked",
		State:            "review",
		Mode:             EvalRunModeExplore,
		ExplorationLevel: ExplorationLevelHigh,
		OptionsCount:     5,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, EvalReviewItem{RunID: "run-ranked", ItemID: "item-001", Title: "Landing page"}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d", "e"} {
		if err := store.UpsertEvalReviewOption(ctx, EvalReviewOption{
			RunID:      "run-ranked",
			ItemID:     "item-001",
			Label:      label,
			ArtifactID: "artifact-" + label,
		}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	options, err := store.ListEvalReviewOptions(ctx, "run-ranked", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 5 || options[0].Label != "a" || options[4].ArtifactID != "artifact-e" {
		t.Fatalf("options = %+v", options)
	}

	ranking, _ := json.Marshal([]string{"c", "a", "d", "b", "e"})
	useful, _ := json.Marshal(map[string][]string{
		"a": {"visual style"},
		"d": {"motion"},
	})
	rejected, _ := json.Marshal(map[string][]string{
		"b": {"generic"},
	})
	required, _ := json.Marshal([]string{"stronger visuals", "better mobile"})
	event := RankedFeedbackEvent{
		RunID:                    "run-ranked",
		ItemID:                   "item-001",
		RankingJSON:              string(ranking),
		Winner:                   "c",
		UsefulTraitsJSON:         string(useful),
		RejectedTraitsJSON:       string(rejected),
		RequiredImprovementsJSON: string(required),
		Quality:                  "Poor",
		ContinueMode:             "Explore",
		Promote:                  "false",
		Reasoning:                "C is clearest.",
		Reviewer:                 "jerry",
		Source:                   "github",
		SourceURL:                "https://github.com/example/repo/pull/1#issuecomment-1",
		CreatedAt:                "2026-06-02T10:00:00Z",
	}
	if err := store.UpsertRankedFeedbackEvent(ctx, event); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent returned error: %v", err)
	}
	stored, err := store.ListRankedFeedbackEvents(ctx, "run-ranked")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 1 || stored[0].Winner != "c" || stored[0].ID == "" {
		t.Fatalf("stored ranked feedback = %+v", stored)
	}
	if stored[0].Quality != "poor" || stored[0].ContinueMode != "explore" || stored[0].Promote != "no" {
		t.Fatalf("stored ranked feedback signals = %+v", stored[0])
	}
	if !strings.Contains(stored[0].RequiredImprovementsJSON, "stronger visuals") || !strings.Contains(stored[0].RequiredImprovementsJSON, "better mobile") {
		t.Fatalf("stored required improvements = %s", stored[0].RequiredImprovementsJSON)
	}
	pairs, err := store.ListPairwisePreferences(ctx, "run-ranked")
	if err != nil {
		t.Fatalf("ListPairwisePreferences returned error: %v", err)
	}
	if len(pairs) != 10 {
		t.Fatalf("pairwise preferences len = %d, want 10: %+v", len(pairs), pairs)
	}
	if pairs[0].Preferred != "c" || pairs[0].Rejected != "a" || pairs[9].Preferred != "b" || pairs[9].Rejected != "e" {
		t.Fatalf("pairwise preferences = %+v", pairs)
	}
}

func TestRankedReviewTieGroupsSkipInGroupPairwisePreferences(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertEvalRun(ctx, EvalRun{ID: "run-tied", State: "review", Mode: EvalRunModeExplore, OptionsCount: 4}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, EvalReviewItem{RunID: "run-tied", ItemID: "item-001", Title: "Tweet"}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		if err := store.UpsertEvalReviewOption(ctx, EvalReviewOption{RunID: "run-tied", ItemID: "item-001", Label: label, ArtifactID: "artifact-" + label}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	if err := store.UpsertRankedFeedbackEvent(ctx, RankedFeedbackEvent{
		RunID:         "run-tied",
		ItemID:        "item-001",
		RankingJSON:   `["a","b","c","d"]`,
		TieGroupsJSON: `[["a","b","c","d"]]`,
		Reviewer:      "jerry",
		Source:        "github",
		SourceURL:     "all-tied",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent all tied returned error: %v", err)
	}
	pairs, err := store.ListPairwisePreferences(ctx, "run-tied")
	if err != nil {
		t.Fatalf("ListPairwisePreferences all tied returned error: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("all-tied pairwise preferences = %+v, want none", pairs)
	}

	if err := store.UpsertRankedFeedbackEvent(ctx, RankedFeedbackEvent{
		RunID:         "run-tied",
		ItemID:        "item-001",
		RankingJSON:   `["a","b","c","d"]`,
		TieGroupsJSON: `[["a"],["b","c"],["d"]]`,
		Winner:        "a",
		Reviewer:      "jerry",
		Source:        "github",
		SourceURL:     "partial-tie",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent partial tie returned error: %v", err)
	}
	events, err := store.ListRankedFeedbackEvents(ctx, "run-tied")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	var partial RankedFeedbackEvent
	for _, event := range events {
		if event.SourceURL == "partial-tie" {
			partial = event
			break
		}
	}
	partialPairs, err := PairwisePreferencesForRankedFeedback(partial)
	if err != nil {
		t.Fatalf("PairwisePreferencesForRankedFeedback partial tie returned error: %v", err)
	}
	if len(partialPairs) != 5 {
		t.Fatalf("partial tie pairwise preference len = %d, want 5: %+v", len(partialPairs), partialPairs)
	}
	for _, pair := range partialPairs {
		if (pair.Preferred == "b" && pair.Rejected == "c") || (pair.Preferred == "c" && pair.Rejected == "b") {
			t.Fatalf("partial tie emitted in-group preference: %+v", partialPairs)
		}
	}
}

func TestRankedReviewStorageRejectsInvalidReferences(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertEvalRun(ctx, EvalRun{ID: "run-ranked", Mode: EvalRunModeExplore, OptionsCount: 3}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, EvalReviewItem{RunID: "run-ranked", ItemID: "item-001"}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c"} {
		if err := store.UpsertEvalReviewOption(ctx, EvalReviewOption{RunID: "run-ranked", ItemID: "item-001", Label: label, ArtifactID: "artifact-" + label}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	if err := store.UpsertEvalReviewOption(ctx, EvalReviewOption{RunID: "run-ranked", ItemID: "item-001", Label: "A", ArtifactID: "artifact-updated"}); err != nil {
		t.Fatalf("duplicate UpsertEvalReviewOption returned error: %v", err)
	}
	options, err := store.ListEvalReviewOptions(ctx, "run-ranked", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions after update returned error: %v", err)
	}
	if len(options) != 3 || options[0].ArtifactID != "artifact-updated" {
		t.Fatalf("updated options = %+v", options)
	}
	if err := store.ReplaceEvalReviewOptions(ctx, "run-ranked", "item-001", []EvalReviewOption{
		{Label: "B", ArtifactID: "artifact-b"},
		{Label: "C", ArtifactID: "artifact-c"},
		{Label: "D", ArtifactID: "artifact-d"},
	}); err != nil {
		t.Fatalf("ReplaceEvalReviewOptions returned error: %v", err)
	}
	options, err = store.ListEvalReviewOptions(ctx, "run-ranked", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions after replace returned error: %v", err)
	}
	if len(options) != 3 || options[0].Label != "b" || options[2].Label != "d" {
		t.Fatalf("replaced options = %+v", options)
	}
	for _, option := range options {
		if option.Label == "a" {
			t.Fatalf("ReplaceEvalReviewOptions left stale option a: %+v", options)
		}
	}

	tests := map[string]RankedFeedbackEvent{
		"duplicate label": {
			RankingJSON: `["a","a"]`,
		},
		"partial ranking": {
			RankingJSON: `["a","b"]`,
		},
		"unknown option": {
			RankingJSON: `["a","b","d"]`,
		},
		"unknown winner": {
			RankingJSON: `["a","b","c"]`,
			Winner:      "d",
		},
		"winner contradicts ranking": {
			RankingJSON: `["a","b","c"]`,
			Winner:      "b",
		},
		"unknown useful trait option": {
			RankingJSON:      `["a","b","c"]`,
			UsefulTraitsJSON: `{"d":["motion"]}`,
		},
		"invalid quality": {
			RankingJSON: `["a","b","c"]`,
			Quality:     "ok",
		},
		"invalid continue mode": {
			RankingJSON:  `["a","b","c"]`,
			ContinueMode: "widen",
		},
		"invalid promote": {
			RankingJSON: `["a","b","c"]`,
			Promote:     "maybe",
		},
		"tie groups mismatch ranking": {
			RankingJSON:   `["a","b","c"]`,
			TieGroupsJSON: `[["a"],["c","b"]]`,
		},
	}
	for name, event := range tests {
		t.Run(name, func(t *testing.T) {
			event.RunID = "run-ranked"
			event.ItemID = "item-001"
			event.Reviewer = "jerry"
			event.Source = "github"
			event.SourceURL = name
			err := store.UpsertRankedFeedbackEvent(ctx, event)
			if err == nil {
				t.Fatal("UpsertRankedFeedbackEvent returned nil error")
			}
		})
	}
}

func TestRankedReviewStorageRejectsOptionCountMismatch(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertEvalRun(ctx, EvalRun{ID: "run-ranked", Mode: EvalRunModeExplore, OptionsCount: 5}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, EvalReviewItem{RunID: "run-ranked", ItemID: "item-001"}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		if err := store.UpsertEvalReviewOption(ctx, EvalReviewOption{RunID: "run-ranked", ItemID: "item-001", Label: label, ArtifactID: "artifact-" + label}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	err = store.UpsertRankedFeedbackEvent(ctx, RankedFeedbackEvent{
		RunID:       "run-ranked",
		ItemID:      "item-001",
		RankingJSON: `["a","b","c","d"]`,
		Reviewer:    "jerry",
		Source:      "github",
		SourceURL:   "mismatch",
	})
	if err == nil || !strings.Contains(err.Error(), "registered options") {
		t.Fatalf("option count mismatch error = %v, want registered options", err)
	}
}

func TestReplaceGeneratedEvalReviewArtifactsRollsBackBatch(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	err = store.ReplaceGeneratedEvalReviewArtifacts(ctx, "run-generated", []EvalReviewGenerationWrite{
		{
			ItemID: "item-001",
			Artifacts: []EvalArtifact{{
				ID:        "run-generated/item-001/option-a",
				Hash:      "sha256:aaa",
				MediaType: "text/markdown",
				SizeBytes: 12,
				Driver:    "text",
			}},
			Options: []EvalReviewOption{{
				Label:      "a",
				ArtifactID: "run-generated/item-001/option-a",
				Role:       "option",
			}},
		},
		{
			ItemID: "item-002",
			Artifacts: []EvalArtifact{{
				ID:        "run-generated/item-002/option-a",
				Hash:      "sha256:bbb",
				MediaType: "text/markdown",
				SizeBytes: 12,
				Driver:    "text",
			}},
			Options: []EvalReviewOption{{
				Label: "a",
				Role:  "option",
			}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "artifact id is required") {
		t.Fatalf("ReplaceGeneratedEvalReviewArtifacts error = %v", err)
	}
	if _, err := store.GetEvalArtifact(ctx, "run-generated/item-001/option-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetEvalArtifact after rollback error = %v, want sql.ErrNoRows", err)
	}
	options, err := store.ListEvalReviewOptions(ctx, "run-generated", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("options after rollback = %+v", options)
	}
}

func TestReplaceGeneratedEvalReviewArtifactsForItemPersists(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	write := EvalReviewGenerationWrite{
		ItemID:     "item-001",
		ReviewItem: &EvalReviewItem{ItemID: "item-001", Title: "Item One"},
		Artifacts: []EvalArtifact{{
			ID:        "run-generated/item-001/option-a",
			Hash:      "sha256:aaa",
			MediaType: "text/markdown",
			SizeBytes: 12,
			Driver:    "text",
		}},
		Options: []EvalReviewOption{{
			Label:      "a",
			ArtifactID: "run-generated/item-001/option-a",
			Role:       "option",
		}},
	}
	if err := store.ReplaceGeneratedEvalReviewArtifactsForItem(ctx, "run-generated", write); err != nil {
		t.Fatalf("ReplaceGeneratedEvalReviewArtifactsForItem error = %v", err)
	}

	if _, err := store.GetEvalArtifact(ctx, "run-generated/item-001/option-a"); err != nil {
		t.Fatalf("GetEvalArtifact error = %v", err)
	}
	items, err := store.ListEvalReviewItems(ctx, "run-generated")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 1 || items[0].ItemID != "item-001" || items[0].Title != "Item One" {
		t.Fatalf("items after persist = %+v", items)
	}
	options, err := store.ListEvalReviewOptions(ctx, "run-generated", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 1 || options[0].Label != "a" {
		t.Fatalf("options after persist = %+v", options)
	}
}

func TestReplaceGeneratedEvalReviewArtifactsForItemRollsBack(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	// A valid artifact alongside an option missing its artifact id: the whole
	// per-item write must fail and leave nothing persisted.
	write := EvalReviewGenerationWrite{
		ItemID:     "item-001",
		ReviewItem: &EvalReviewItem{ItemID: "item-001"},
		Artifacts: []EvalArtifact{{
			ID:        "run-generated/item-001/option-a",
			Hash:      "sha256:aaa",
			MediaType: "text/markdown",
			SizeBytes: 12,
			Driver:    "text",
		}},
		Options: []EvalReviewOption{{
			Label: "a",
			Role:  "option",
		}},
	}
	err = store.ReplaceGeneratedEvalReviewArtifactsForItem(ctx, "run-generated", write)
	if err == nil || !strings.Contains(err.Error(), "artifact id is required") {
		t.Fatalf("ReplaceGeneratedEvalReviewArtifactsForItem error = %v, want artifact id is required", err)
	}
	if _, err := store.GetEvalArtifact(ctx, "run-generated/item-001/option-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetEvalArtifact after rollback error = %v, want sql.ErrNoRows", err)
	}
	items, err := store.ListEvalReviewItems(ctx, "run-generated")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items after rollback = %+v", items)
	}
	options, err := store.ListEvalReviewOptions(ctx, "run-generated", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("options after rollback = %+v", options)
	}
}

func TestReplaceGeneratedEvalReviewArtifactsForItemNoClobber(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	writeItem := func(itemID, hash string) EvalReviewGenerationWrite {
		artifactID := "run-generated/" + itemID + "/option-a"
		return EvalReviewGenerationWrite{
			ItemID:     itemID,
			ReviewItem: &EvalReviewItem{ItemID: itemID},
			Artifacts: []EvalArtifact{{
				ID:        artifactID,
				Hash:      hash,
				MediaType: "text/markdown",
				SizeBytes: 12,
				Driver:    "text",
			}},
			Options: []EvalReviewOption{{
				Label:      "a",
				ArtifactID: artifactID,
				Role:       "option",
			}},
		}
	}

	if err := store.ReplaceGeneratedEvalReviewArtifactsForItem(ctx, "run-generated", writeItem("item-001", "sha256:aaa")); err != nil {
		t.Fatalf("ReplaceGeneratedEvalReviewArtifactsForItem item-001 error = %v", err)
	}
	// Writing item-002 must not clobber item-001.
	if err := store.ReplaceGeneratedEvalReviewArtifactsForItem(ctx, "run-generated", writeItem("item-002", "sha256:bbb")); err != nil {
		t.Fatalf("ReplaceGeneratedEvalReviewArtifactsForItem item-002 error = %v", err)
	}

	items, err := store.ListEvalReviewItems(ctx, "run-generated")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after writing two = %+v", items)
	}
	for _, itemID := range []string{"item-001", "item-002"} {
		opts, err := store.ListEvalReviewOptions(ctx, "run-generated", itemID)
		if err != nil {
			t.Fatalf("ListEvalReviewOptions %s returned error: %v", itemID, err)
		}
		if len(opts) != 1 || opts[0].Label != "a" {
			t.Fatalf("options for %s = %+v", itemID, opts)
		}
	}

	// Re-writing item-001 is idempotent: no duplicate labels, item-002 intact.
	if err := store.ReplaceGeneratedEvalReviewArtifactsForItem(ctx, "run-generated", writeItem("item-001", "sha256:ccc")); err != nil {
		t.Fatalf("ReplaceGeneratedEvalReviewArtifactsForItem rewrite item-001 error = %v", err)
	}
	opts1, err := store.ListEvalReviewOptions(ctx, "run-generated", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions item-001 returned error: %v", err)
	}
	if len(opts1) != 1 || opts1[0].Label != "a" {
		t.Fatalf("options for item-001 after rewrite = %+v", opts1)
	}
	opts2, err := store.ListEvalReviewOptions(ctx, "run-generated", "item-002")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions item-002 returned error: %v", err)
	}
	if len(opts2) != 1 || opts2[0].Label != "a" {
		t.Fatalf("options for item-002 after item-001 rewrite = %+v", opts2)
	}
}

func TestFeedbackEventMethods(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	event := FeedbackEvent{
		RunID:     "run-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reasoning: "More concrete.",
		Reviewer:  "jerry",
		Source:    "markdown",
		SourceURL: "packet",
		CreatedAt: "2026-05-31T10:00:00Z",
	}
	if err := store.UpsertFeedbackEvent(ctx, event); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	event.Choice = "tie"
	event.Reasoning = "Both work."
	if err := store.UpsertFeedbackEvent(ctx, event); err != nil {
		t.Fatalf("second UpsertFeedbackEvent returned error: %v", err)
	}
	events, err := store.ListFeedbackEvents(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	if events[0].Choice != "tie" || events[0].Reasoning != "Both work." || events[0].ID == "" {
		t.Fatalf("event = %+v", events[0])
	}

	collisionA := FeedbackEvent{
		RunID:     "a/b",
		ItemID:    "c",
		Choice:    "a",
		Reviewer:  "reviewer",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}
	collisionB := FeedbackEvent{
		RunID:     "a",
		ItemID:    "b/c",
		Choice:    "b",
		Reviewer:  "reviewer",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}
	if err := store.UpsertFeedbackEvent(ctx, collisionA); err != nil {
		t.Fatalf("collisionA UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(ctx, collisionB); err != nil {
		t.Fatalf("collisionB UpsertFeedbackEvent returned error: %v", err)
	}
	aEvents, err := store.ListFeedbackEvents(ctx, "a/b")
	if err != nil {
		t.Fatalf("ListFeedbackEvents a/b returned error: %v", err)
	}
	bEvents, err := store.ListFeedbackEvents(ctx, "a")
	if err != nil {
		t.Fatalf("ListFeedbackEvents a returned error: %v", err)
	}
	if len(aEvents) != 1 || len(bEvents) != 1 || aEvents[0].ItemID != "c" || bEvents[0].ItemID != "b/c" {
		t.Fatalf("collision events a=%+v b=%+v", aEvents, bEvents)
	}
}

func TestListResourceLocks(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	empty, err := store.ListResourceLocks(ctx)
	if err != nil {
		t.Fatalf("ListResourceLocks empty returned error: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no locks, got %d", len(empty))
	}
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for _, key := range []string{"runtime:codex:a", "generation:session-1"} {
		if _, err := store.AcquireResourceLock(ctx, ResourceLock{
			ResourceKey: key,
			OwnerJobID:  "job-" + key,
			OwnerToken:  "token",
			ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339Nano),
		}, now); err != nil {
			t.Fatalf("AcquireResourceLock(%s) returned error: %v", key, err)
		}
	}
	locks, err := store.ListResourceLocks(ctx)
	if err != nil {
		t.Fatalf("ListResourceLocks returned error: %v", err)
	}
	if len(locks) != 2 {
		t.Fatalf("expected 2 locks, got %d: %+v", len(locks), locks)
	}
	// Ordered by resource_key: "generation:..." < "runtime:...".
	if locks[0].ResourceKey != "generation:session-1" {
		t.Fatalf("locks not ordered by key: %+v", locks)
	}
}

func TestResourceLockMethods(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	lock := ResourceLock{
		ResourceKey:   "runtime:codex:session-a",
		OwnerJobID:    "job-a",
		OwnerToken:    "token-a",
		OwnerPID:      12345,
		OwnerHostname: "host-a",
		CommandHash:   "hash-a",
		ExpiresAt:     now.Add(time.Minute).Format(time.RFC3339Nano),
	}
	acquired, err := store.AcquireResourceLock(ctx, lock, now)
	if err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("first AcquireResourceLock did not acquire")
	}
	acquired, err = store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: lock.ResourceKey,
		OwnerJobID:  "job-b",
		OwnerToken:  "token-b",
		ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("conflicting AcquireResourceLock returned error: %v", err)
	}
	if acquired {
		t.Fatal("conflicting AcquireResourceLock acquired busy resource")
	}
	acquired, err = store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: lock.ResourceKey,
		OwnerJobID:  "job-a",
		OwnerToken:  "token-c",
		ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339Nano),
	}, now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("same-job duplicate AcquireResourceLock returned error: %v", err)
	}
	if acquired {
		t.Fatal("same-job duplicate AcquireResourceLock acquired busy resource")
	}
	stored, err := store.GetResourceLock(ctx, lock.ResourceKey)
	if err != nil {
		t.Fatalf("GetResourceLock returned error: %v", err)
	}
	if stored.OwnerJobID != "job-a" || stored.OwnerToken != "token-a" || stored.OwnerPID != 12345 || stored.OwnerHostname != "host-a" || stored.CommandHash != "hash-a" || stored.AcquiredAt == "" || stored.UpdatedAt == "" || stored.ExpiresAt == "" {
		t.Fatalf("resource lock = %+v", stored)
	}
	if stored.ExpiresAt != formatResourceLockTime(now.Add(time.Minute)) {
		t.Fatalf("resource lock expiry = %q, want fixed-width timestamp", stored.ExpiresAt)
	}
	if updated, err := store.HeartbeatResourceLock(ctx, lock.ResourceKey, "job-a", "wrong-token", now.Add(20*time.Second), now.Add(2*time.Minute)); err != nil || updated {
		t.Fatalf("wrong-token HeartbeatResourceLock returned updated=%v err=%v", updated, err)
	}
	if updated, err := store.HeartbeatResourceLock(ctx, lock.ResourceKey, "job-a", "token-a", now.Add(30*time.Second), now.Add(2*time.Minute)); err != nil || !updated {
		t.Fatalf("HeartbeatResourceLock returned updated=%v err=%v", updated, err)
	}
	stored, err = store.GetResourceLock(ctx, lock.ResourceKey)
	if err != nil {
		t.Fatalf("GetResourceLock after heartbeat returned error: %v", err)
	}
	if stored.UpdatedAt != formatResourceLockTime(now.Add(30*time.Second)) || stored.ExpiresAt != formatResourceLockTime(now.Add(2*time.Minute)) {
		t.Fatalf("heartbeat lock times = updated %q expires %q", stored.UpdatedAt, stored.ExpiresAt)
	}
	released, err := store.ReleaseResourceLock(ctx, lock.ResourceKey, "job-b", "token-a")
	if err != nil {
		t.Fatalf("wrong-owner ReleaseResourceLock returned error: %v", err)
	}
	if released {
		t.Fatal("wrong owner released resource lock")
	}
	released, err = store.ReleaseResourceLock(ctx, lock.ResourceKey, "job-a", "token-c")
	if err != nil {
		t.Fatalf("wrong-token ReleaseResourceLock returned error: %v", err)
	}
	if released {
		t.Fatal("wrong token released resource lock")
	}
	released, err = store.ReleaseResourceLock(ctx, lock.ResourceKey, "job-a", "token-a")
	if err != nil {
		t.Fatalf("ReleaseResourceLock returned error: %v", err)
	}
	if !released {
		t.Fatal("ReleaseResourceLock did not release")
	}
	if _, err := store.GetResourceLock(ctx, lock.ResourceKey); err == nil || err != sql.ErrNoRows {
		t.Fatalf("GetResourceLock after release error = %v, want no rows", err)
	}
}

func TestResourceLockRecoversExpiredLock(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	if acquired, err := store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: "runtime:codex:session-a",
		OwnerJobID:  "job-a",
		OwnerToken:  "token-a",
		ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339Nano),
	}, now); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	acquired, err := store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: "runtime:codex:session-a",
		OwnerJobID:  "job-b",
		OwnerToken:  "token-b",
		ExpiresAt:   now.Add(3 * time.Minute).Format(time.RFC3339Nano),
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("expired AcquireResourceLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expired AcquireResourceLock did not acquire")
	}
	stored, err := store.GetResourceLock(ctx, "runtime:codex:session-a")
	if err != nil {
		t.Fatalf("GetResourceLock returned error: %v", err)
	}
	if stored.OwnerJobID != "job-b" {
		t.Fatalf("resource lock owner = %q, want job-b", stored.OwnerJobID)
	}
	deleted, err := store.DeleteExpiredResourceLocks(ctx, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredResourceLocks returned error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expired locks deleted = %d, want 1", deleted)
	}
}

func TestResourceLockCleanupSkipsPIDBackedLease(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	if acquired, err := store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey:   "skillopt-train:session-a:iteration-a",
		OwnerJobID:    "local-skillopt-train-optimizer-a",
		OwnerToken:    "token-a",
		OwnerPID:      12345,
		OwnerHostname: "host-a",
		CommandHash:   "hash-a",
		ExpiresAt:     now.Add(time.Minute).Format(time.RFC3339Nano),
	}, now); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	deleted, err := store.DeleteExpiredResourceLocks(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredResourceLocks returned error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expired pid-backed locks deleted = %d, want 0", deleted)
	}
	stored, err := store.GetResourceLock(ctx, "skillopt-train:session-a:iteration-a")
	if err != nil {
		t.Fatalf("GetResourceLock returned error: %v", err)
	}
	if stored.OwnerPID != 12345 || stored.CommandHash != "hash-a" {
		t.Fatalf("resource lock metadata = pid %d hash %q, want pid 12345 hash-a", stored.OwnerPID, stored.CommandHash)
	}
}

func TestResourceLockDoesNotRecoverExpiredRunningOwner(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	if err := store.CreateJob(ctx, Job{ID: "job-a", Agent: "audit", Type: "ask", State: "running", Payload: `{"repo":"owner/repo"}`}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	if acquired, err := store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: "runtime:codex:session-a",
		OwnerJobID:  "job-a",
		OwnerToken:  "token-a",
		ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339Nano),
	}, now); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	acquired, err := store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: "runtime:codex:session-a",
		OwnerJobID:  "job-b",
		OwnerToken:  "token-b",
		ExpiresAt:   now.Add(3 * time.Minute).Format(time.RFC3339Nano),
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("expired running-owner AcquireResourceLock returned error: %v", err)
	}
	if acquired {
		t.Fatal("expired running-owner AcquireResourceLock acquired active resource")
	}
	deleted, err := store.DeleteExpiredResourceLocks(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredResourceLocks returned error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expired running-owner locks deleted = %d, want 0", deleted)
	}
	if err := store.UpdateJobState(ctx, "job-a", "queued"); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	deleted, err = store.DeleteExpiredResourceLocks(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredResourceLocks after requeue returned error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expired non-running-owner locks deleted = %d, want 1", deleted)
	}
}

func TestStopAgentInstance(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	mk := func(name, state string) AgentInstance {
		return AgentInstance{
			Name: name, Type: "planner", Runtime: "codex", RuntimeRef: "ref-" + name,
			RepoFullName: "owner/repo", Role: "planner", State: state,
			CreatedAt: formatResourceLockTime(now), LastUsedAt: formatResourceLockTime(now),
			ExpiresAt: formatResourceLockTime(now.Add(time.Minute)),
		}
	}
	if err := store.UpsertAgentInstance(ctx, mk("idle-1", "idle")); err != nil {
		t.Fatalf("seed idle: %v", err)
	}
	if err := store.UpsertAgentInstance(ctx, mk("busy-1", "running")); err != nil {
		t.Fatalf("seed running: %v", err)
	}

	// Idle session stops (row removed).
	if err := store.StopAgentInstance(ctx, "idle-1"); err != nil {
		t.Fatalf("stop idle: %v", err)
	}
	if _, err := store.GetAgentInstance(ctx, "idle-1"); err == nil {
		t.Fatal("idle session should be gone after stop")
	}
	// Running session is refused and left in place.
	if err := store.StopAgentInstance(ctx, "busy-1"); err == nil || !strings.Contains(err.Error(), "running a job") {
		t.Fatalf("expected running-refusal, got %v", err)
	}
	if _, err := store.GetAgentInstance(ctx, "busy-1"); err != nil {
		t.Fatalf("running session must survive a refused stop: %v", err)
	}
	// Missing session errors.
	if err := store.StopAgentInstance(ctx, "ghost"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestAgentInstanceMethods(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	instance := AgentInstance{
		Name:           "planner-bg-1",
		Type:           "planner",
		Runtime:        "codex",
		RuntimeRef:     "550e8400-e29b-41d4-a716-446655440101",
		RepoFullName:   "owner/repo",
		Role:           "planner",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "read-only",
		State:          "idle",
		CreatedAt:      formatResourceLockTime(now),
		LastUsedAt:     formatResourceLockTime(now),
		ExpiresAt:      formatResourceLockTime(now.Add(time.Minute)),
	}
	if err := store.UpsertAgentInstance(ctx, instance); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}
	agent, err := store.GetAgent(ctx, instance.Name)
	if err != nil {
		t.Fatalf("GetAgent fallback returned error: %v", err)
	}
	if agent.Name != instance.Name || agent.RuntimeRef != instance.RuntimeRef || agent.AutonomyPolicy != "read-only" || strings.Join(agent.Capabilities, ",") != "ask" {
		t.Fatalf("fallback agent = %+v", agent)
	}
	allowed, err := store.AgentCanAccessRepo(ctx, instance.Name, "owner/repo")
	if err != nil {
		t.Fatalf("AgentCanAccessRepo returned error: %v", err)
	}
	if !allowed {
		t.Fatal("agent instance was not allowed on its repo")
	}
	reusable, ok, err := store.FindReusableAgentInstance(ctx, "planner", "owner/repo", "read-only", now.Add(30*time.Second))
	if err != nil || !ok {
		t.Fatalf("FindReusableAgentInstance returned instance=%+v ok=%v err=%v", reusable, ok, err)
	}
	if _, ok, err := store.FindReusableAgentInstance(ctx, "planner", "owner/repo", "workspace-write", now.Add(30*time.Second)); err != nil || ok {
		t.Fatalf("FindReusableAgentInstance with mismatched policy ok=%v err=%v, want false nil", ok, err)
	}
	count, err := store.CountActiveAgentInstances(ctx, "planner", "read-only", now.Add(30*time.Second))
	if err != nil || count != 1 {
		t.Fatalf("CountActiveAgentInstances = %d err=%v, want 1 nil", count, err)
	}
	count, err = store.CountActiveAgentInstances(ctx, "planner", "workspace-write", now.Add(30*time.Second))
	if err != nil || count != 0 {
		t.Fatalf("CountActiveAgentInstances with mismatched policy = %d err=%v, want 0 nil", count, err)
	}
	if err := store.MarkAgentInstanceRunning(ctx, instance.Name, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("MarkAgentInstanceRunning returned error: %v", err)
	}
	if _, ok, err := store.FindReusableAgentInstance(ctx, "planner", "owner/repo", "read-only", now.Add(30*time.Second)); err != nil || ok {
		t.Fatalf("running FindReusableAgentInstance ok=%v err=%v, want false nil", ok, err)
	}
	if active, ok, err := store.FindActiveAgentInstance(ctx, "planner", "owner/repo", "read-only", now.Add(30*time.Second)); err != nil || !ok || active.Name != instance.Name {
		t.Fatalf("FindActiveAgentInstance returned instance=%+v ok=%v err=%v", active, ok, err)
	}
	if _, ok, err := store.FindActiveAgentInstance(ctx, "planner", "owner/repo", "workspace-write", now.Add(30*time.Second)); err != nil || ok {
		t.Fatalf("FindActiveAgentInstance with mismatched policy ok=%v err=%v, want false nil", ok, err)
	}
	deleted, err := store.DeleteExpiredAgentInstances(ctx, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("running DeleteExpiredAgentInstances returned error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("running expired instances deleted = %d, want 0", deleted)
	}
	if err := store.TouchAgentInstance(ctx, instance.Name, now.Add(2*time.Minute), time.Minute); err != nil {
		t.Fatalf("TouchAgentInstance returned error: %v", err)
	}
	count, err = store.CountActiveAgentInstances(ctx, "planner", "read-only", now.Add(4*time.Minute))
	if err != nil || count != 0 {
		t.Fatalf("expired idle CountActiveAgentInstances = %d err=%v, want 0 nil", count, err)
	}
	if err := store.CreateJob(ctx, Job{ID: "job-planner-1", Agent: instance.Name, Type: "ask", State: "queued", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	count, err = store.CountActiveAgentInstances(ctx, "planner", "read-only", now.Add(4*time.Minute))
	if err != nil || count != 1 {
		t.Fatalf("expired queued CountActiveAgentInstances = %d err=%v, want 1 nil", count, err)
	}
	if active, ok, err := store.FindActiveAgentInstance(ctx, "planner", "owner/repo", "read-only", now.Add(4*time.Minute)); err != nil || !ok || active.Name != instance.Name {
		t.Fatalf("expired queued FindActiveAgentInstance returned instance=%+v ok=%v err=%v", active, ok, err)
	}
	deleted, err = store.DeleteExpiredAgentInstances(ctx, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredAgentInstances returned error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expired instance with queued job deleted = %d, want 0", deleted)
	}
	if ok, err := store.TransitionJobState(ctx, "job-planner-1", "queued", "done"); err != nil || !ok {
		t.Fatalf("TransitionJobState returned ok=%v err=%v", ok, err)
	}
	if err := store.UpsertAgentInstance(ctx, instance); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "job-planner-retryable", Agent: instance.Name, Type: "ask", State: "failed", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob retryable returned error: %v", err)
	}
	deleted, err = store.DeleteExpiredAgentInstances(ctx, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("retryable DeleteExpiredAgentInstances returned error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expired instance with retryable job deleted = %d, want 0", deleted)
	}
	if ok, err := store.TransitionJobState(ctx, "job-planner-retryable", "failed", "done"); err != nil || !ok {
		t.Fatalf("TransitionJobState retryable returned ok=%v err=%v", ok, err)
	}
	deleted, err = store.DeleteExpiredAgentInstances(ctx, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredAgentInstances returned error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expired instances deleted after queued job completed = %d, want 1", deleted)
	}
}

func TestRepositoryMethods(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertRepo(ctx, Repo{Owner: "jerryfane", Name: "gitmoot", DefaultBranch: "main", RemoteURL: "https://github.com/jerryfane/gitmoot.git", CheckoutPath: "/repo/gitmoot"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	repo, err := store.GetRepo(ctx, "jerryfane/gitmoot")
	if err != nil {
		t.Fatalf("GetRepo returned error: %v", err)
	}
	if repo.FullName() != "jerryfane/gitmoot" || repo.DefaultBranch != "main" || repo.RemoteURL == "" || repo.CheckoutPath != "/repo/gitmoot" || !repo.Enabled || repo.PollInterval != "30s" {
		t.Fatalf("repo = %+v", repo)
	}
	if err := store.UpsertRepo(ctx, Repo{Owner: "jerryfane", Name: "gitmoot", PollInterval: "1m"}); err != nil {
		t.Fatalf("second UpsertRepo returned error: %v", err)
	}
	repo, err = store.GetRepo(ctx, "jerryfane/gitmoot")
	if err != nil {
		t.Fatalf("GetRepo after update returned error: %v", err)
	}
	if repo.DefaultBranch != "main" || repo.RemoteURL == "" || repo.CheckoutPath != "/repo/gitmoot" || repo.PollInterval != "1m" {
		t.Fatalf("updated repo lost existing fields: %+v", repo)
	}
	if err := store.UpsertRepo(ctx, Repo{Owner: "jerryfane", Name: "gitmoot", RemoteURL: "git@github.com:jerryfane/gitmoot.git"}); err != nil {
		t.Fatalf("auto UpsertRepo returned error: %v", err)
	}
	repo, err = store.GetRepo(ctx, "jerryfane/gitmoot")
	if err != nil {
		t.Fatalf("GetRepo after auto update returned error: %v", err)
	}
	if repo.RemoteURL != "git@github.com:jerryfane/gitmoot.git" || repo.PollInterval != "1m" {
		t.Fatalf("auto update did not preserve configured poll interval: %+v", repo)
	}
	if err := store.SetRepoEnabled(ctx, "jerryfane/gitmoot", false); err != nil {
		t.Fatalf("SetRepoEnabled returned error: %v", err)
	}
	if err := store.UpdateRepoPollResult(ctx, "jerryfane/gitmoot", "2026-05-21T12:00:00Z", "rate limited"); err != nil {
		t.Fatalf("UpdateRepoPollResult returned error: %v", err)
	}
	repos, err := store.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos returned error: %v", err)
	}
	if len(repos) != 1 || repos[0].Enabled || repos[0].LastPollAt == "" || repos[0].LastError != "rate limited" {
		t.Fatalf("repos = %+v", repos)
	}
	removed, err := store.RemoveRepo(ctx, "jerryfane/gitmoot")
	if err != nil {
		t.Fatalf("RemoveRepo returned error: %v", err)
	}
	if !removed {
		t.Fatal("RemoveRepo did not remove repo")
	}
	if err := store.UpsertRepo(ctx, repo); err != nil {
		t.Fatalf("restore UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(ctx, AgentTemplate{
		ID:             "thermo",
		Name:           "Thermo",
		Description:    "Strict review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
		MetadataJSON:   `{"id":"thermo","name":"Thermo","description":"Strict review","kind":"agent-template","version":1,"capabilities":["review"],"runtime_compatibility":["codex"],"tags":["review"],"inputs":["repo"],"outputs":["review_findings"]}`,
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	template, err := store.GetAgentTemplate(ctx, "thermo")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if template.ResolvedCommit != "abc123" || template.Content != "Review deeply." || !strings.Contains(template.MetadataJSON, `"kind":"agent-template"`) || template.VersionID != "thermo@v1" || template.VersionNumber != 1 || template.VersionState != "current" || !strings.HasPrefix(template.ContentHash, "sha256:") || template.CreatedAt == "" || template.UpdatedAt == "" {
		t.Fatalf("template = %+v", template)
	}
	if err := store.UpsertAgentTemplate(ctx, AgentTemplate{
		ID:             "thermo",
		Name:           "Thermo",
		Description:    "Strict review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "def456",
		Content:        "Review deeply again.",
		MetadataJSON:   `{"id":"thermo","name":"Thermo","description":"Strict review","kind":"agent-template","version":1,"capabilities":["review"],"runtime_compatibility":["codex"],"tags":["review"],"inputs":["repo"],"outputs":["review_findings"]}`,
	}); err != nil {
		t.Fatalf("second UpsertAgentTemplate returned error: %v", err)
	}
	template, err = store.GetAgentTemplate(ctx, "thermo")
	if err != nil {
		t.Fatalf("GetAgentTemplate second returned error: %v", err)
	}
	if template.VersionID != "thermo@v2" || template.VersionNumber != 2 || template.ResolvedCommit != "def456" {
		t.Fatalf("template second version = %+v", template)
	}
	versions, err := store.ListAgentTemplateVersions(ctx, "thermo")
	if err != nil {
		t.Fatalf("ListAgentTemplateVersions returned error: %v", err)
	}
	if len(versions) != 2 || versions[0].State != "superseded" || versions[1].State != "current" {
		t.Fatalf("versions = %+v", versions)
	}
	pending, err := store.AddPendingAgentTemplateVersion(ctx, AgentTemplate{
		ID:             "thermo",
		Name:           "Thermo Candidate",
		Description:    "Candidate review",
		SourceRepo:     "local",
		SourceRef:      "candidate",
		SourcePath:     "candidate.md",
		ResolvedCommit: "sha256:candidate",
		Content:        "Candidate instructions.",
		MetadataJSON:   `{"id":"thermo","name":"Thermo Candidate","description":"Candidate review","kind":"agent-template","version":1,"capabilities":["review"],"runtime_compatibility":["codex"],"tags":["review"],"inputs":["repo"],"outputs":["review_findings"]}`,
	})
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion returned error: %v", err)
	}
	if pending.State != "pending" || pending.VersionNumber != 3 {
		t.Fatalf("pending version = %+v", pending)
	}
	current, err := store.GetAgentTemplate(ctx, "thermo")
	if err != nil {
		t.Fatalf("GetAgentTemplate current returned error: %v", err)
	}
	if current.VersionID != "thermo@v2" || current.Content != "Review deeply again." {
		t.Fatalf("pending changed current template = %+v", current)
	}
	latest, err := store.GetAgentTemplateReference(ctx, "thermo@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest returned error: %v", err)
	}
	if latest.VersionID != "thermo@v3" || latest.VersionState != "pending" || latest.Content != "Candidate instructions." {
		t.Fatalf("latest template = %+v", latest)
	}
	pinned, err := store.GetAgentTemplateReference(ctx, "thermo@v1")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference returned error: %v", err)
	}
	if pinned.VersionID != "thermo@v1" || pinned.Content != "Review deeply." {
		t.Fatalf("pinned template = %+v", pinned)
	}
	score := 0.82
	if err := store.UpsertAgentTemplateCandidateReview(ctx, AgentTemplateCandidateReview{
		VersionID:         pending.ID,
		TemplateID:        pending.TemplateID,
		BaseVersionID:     current.VersionID,
		DiffArtifactID:    "diff-1",
		Score:             &score,
		PreferenceSummary: "Candidate is more actionable.",
		EvalReportJSON:    `{"score":0.82}`,
		State:             "pending",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview returned error: %v", err)
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, pending.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	if review.BaseVersionID != current.VersionID || review.Score == nil || *review.Score != score || review.PreferenceSummary == "" {
		t.Fatalf("review = %+v", review)
	}
	newerPending, err := store.AddPendingAgentTemplateVersion(ctx, AgentTemplate{
		ID:             "thermo",
		Name:           "Thermo Candidate 2",
		Description:    "Candidate review",
		SourceRepo:     "local",
		SourceRef:      "candidate",
		SourcePath:     "candidate-2.md",
		ResolvedCommit: "sha256:candidate2",
		Content:        "Newer pending instructions.",
		MetadataJSON:   `{"id":"thermo","name":"Thermo Candidate","description":"Candidate review","kind":"agent-template","version":1,"capabilities":["review"],"runtime_compatibility":["codex"],"tags":["review"],"inputs":["repo"],"outputs":["review_findings"]}`,
	})
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion newer candidate returned error: %v", err)
	}
	pendingVersions, err := store.ListPendingAgentTemplateVersions(ctx, "thermo")
	if err != nil {
		t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
	}
	if len(pendingVersions) != 2 || pendingVersions[0].ID != pending.ID || pendingVersions[1].ID != newerPending.ID {
		t.Fatalf("pending versions = %+v", pendingVersions)
	}
	promoted, err := store.PromoteAgentTemplateVersion(ctx, pending.ID)
	if err != nil {
		t.Fatalf("PromoteAgentTemplateVersion returned error: %v", err)
	}
	if promoted.State != "current" || promoted.PromotedAt == "" {
		t.Fatalf("promoted = %+v", promoted)
	}
	currentAfterPromote, err := store.GetAgentTemplate(ctx, "thermo")
	if err != nil {
		t.Fatalf("GetAgentTemplate after promote returned error: %v", err)
	}
	if currentAfterPromote.VersionID != pending.ID || currentAfterPromote.Content != "Candidate instructions." {
		t.Fatalf("current after promote = %+v", currentAfterPromote)
	}
	latestAfterPromote, err := store.GetAgentTemplateReference(ctx, "thermo@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest after promote returned error: %v", err)
	}
	if latestAfterPromote.VersionID != newerPending.ID || latestAfterPromote.VersionState != "pending" {
		t.Fatalf("latest after promote = %+v", latestAfterPromote)
	}
	review, err = store.GetAgentTemplateCandidateReview(ctx, pending.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview after promote returned error: %v", err)
	}
	if review.State != "promoted" || review.DecidedAt == "" {
		t.Fatalf("review after promote = %+v", review)
	}
	rejected, err := store.RejectAgentTemplateVersion(ctx, newerPending.ID, "too verbose")
	if err != nil {
		t.Fatalf("RejectAgentTemplateVersion returned error: %v", err)
	}
	if rejected.State != "rejected" {
		t.Fatalf("rejected = %+v", rejected)
	}
	latestAfterReject, err := store.GetAgentTemplateReference(ctx, "thermo@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest after reject returned error: %v", err)
	}
	if latestAfterReject.VersionID != pending.ID || latestAfterReject.VersionState == "rejected" {
		t.Fatalf("latest selected rejected candidate: %+v", latestAfterReject)
	}
	rejectReview, err := store.GetAgentTemplateCandidateReview(ctx, newerPending.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview reject returned error: %v", err)
	}
	if rejectReview.State != "rejected" || rejectReview.DecisionReason != "too verbose" || rejectReview.DecidedAt == "" {
		t.Fatalf("reject review = %+v", rejectReview)
	}
	if err := store.UpsertAgentTemplate(ctx, AgentTemplate{
		ID:             "outoforder",
		Name:           "Out Of Order",
		Description:    "Promotion ordering",
		SourceRepo:     "local",
		SourceRef:      "main",
		SourcePath:     "template.md",
		ResolvedCommit: "sha256:one",
		Content:        "Current one.",
		MetadataJSON:   `{"id":"outoforder","name":"Out Of Order","description":"Promotion ordering","kind":"agent-template","version":1,"capabilities":["ask"],"runtime_compatibility":["codex"],"tags":["planning"],"inputs":["repo"],"outputs":["plan"]}`,
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate outoforder returned error: %v", err)
	}
	olderPending, err := store.AddPendingAgentTemplateVersion(ctx, AgentTemplate{
		ID:             "outoforder",
		Name:           "Out Of Order Pending",
		Description:    "Promotion ordering",
		SourceRepo:     "local",
		SourceRef:      "candidate",
		SourcePath:     "candidate.md",
		ResolvedCommit: "sha256:pending",
		Content:        "Older pending candidate.",
		MetadataJSON:   `{"id":"outoforder","name":"Out Of Order Pending","description":"Promotion ordering","kind":"agent-template","version":1,"capabilities":["ask"],"runtime_compatibility":["codex"],"tags":["planning"],"inputs":["repo"],"outputs":["plan"]}`,
	})
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion outoforder returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(ctx, AgentTemplate{
		ID:             "outoforder",
		Name:           "Out Of Order New Current",
		Description:    "Promotion ordering",
		SourceRepo:     "local",
		SourceRef:      "main",
		SourcePath:     "template.md",
		ResolvedCommit: "sha256:three",
		Content:        "Newer current version.",
		MetadataJSON:   `{"id":"outoforder","name":"Out Of Order New Current","description":"Promotion ordering","kind":"agent-template","version":1,"capabilities":["ask"],"runtime_compatibility":["codex"],"tags":["planning"],"inputs":["repo"],"outputs":["plan"]}`,
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate outoforder newer returned error: %v", err)
	}
	promotedOutOfOrder, err := store.PromoteAgentTemplateVersion(ctx, olderPending.ID)
	if err != nil {
		t.Fatalf("PromoteAgentTemplateVersion outoforder returned error: %v", err)
	}
	if promotedOutOfOrder.ID != olderPending.ID || promotedOutOfOrder.State != "current" {
		t.Fatalf("promoted outoforder = %+v", promotedOutOfOrder)
	}
	latestOutOfOrder, err := store.GetAgentTemplateReference(ctx, "outoforder@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference outoforder latest returned error: %v", err)
	}
	if latestOutOfOrder.VersionID != olderPending.ID || latestOutOfOrder.VersionState == "superseded" {
		t.Fatalf("outoforder latest selected wrong version: %+v", latestOutOfOrder)
	}
	templates, err := store.ListAgentTemplates(ctx)
	if err != nil {
		t.Fatalf("ListAgentTemplates returned error: %v", err)
	}
	if len(templates) != 2 || templates[0].ID != "outoforder" || templates[1].ID != "thermo" {
		t.Fatalf("templates = %+v", templates)
	}
	if err := store.UpsertAgent(ctx, Agent{Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "session", RepoScope: "jerryfane/gitmoot", TemplateID: "thermo", Capabilities: []string{"review"}, AutonomyPolicy: "auto", HealthStatus: "ok"}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	allowed, err := store.AgentCanAccessRepo(ctx, "audit", "jerryfane/gitmoot")
	if err != nil {
		t.Fatalf("AgentCanAccessRepo returned error: %v", err)
	}
	if !allowed {
		t.Fatal("agent repo scope was not added as allowed repo")
	}
	if err := store.AllowAgentRepo(ctx, "audit", "jerryfane/other"); err != nil {
		t.Fatalf("AllowAgentRepo returned error: %v", err)
	}
	agentRepos, err := store.ListAgentRepos(ctx, "audit")
	if err != nil {
		t.Fatalf("ListAgentRepos returned error: %v", err)
	}
	if len(agentRepos) != 2 || agentRepos[0] != "jerryfane/gitmoot" || agentRepos[1] != "jerryfane/other" {
		t.Fatalf("agent repos = %+v", agentRepos)
	}
	denied, err := store.DenyAgentRepo(ctx, "audit", "jerryfane/other")
	if err != nil {
		t.Fatalf("DenyAgentRepo returned error: %v", err)
	}
	if !denied {
		t.Fatal("DenyAgentRepo did not remove access")
	}
	if err := store.ReplaceAgentRepos(ctx, "audit", []string{"jerryfane/second", "jerryfane/third"}); err != nil {
		t.Fatalf("ReplaceAgentRepos returned error: %v", err)
	}
	agentRepos, err = store.ListAgentRepos(ctx, "audit")
	if err != nil {
		t.Fatalf("ListAgentRepos after replace returned error: %v", err)
	}
	if len(agentRepos) != 2 || agentRepos[0] != "jerryfane/second" || agentRepos[1] != "jerryfane/third" {
		t.Fatalf("agent repos after replace = %+v", agentRepos)
	}
	if err := store.ReplaceAgentRepos(ctx, "audit", nil); err != nil {
		t.Fatalf("empty ReplaceAgentRepos returned error: %v", err)
	}
	allowed, err = store.AgentCanAccessRepo(ctx, "audit", "jerryfane/second")
	if err != nil {
		t.Fatalf("AgentCanAccessRepo after empty replace returned error: %v", err)
	}
	if allowed {
		t.Fatal("empty ReplaceAgentRepos left stale access")
	}
	if err := store.AllowAgentRepo(ctx, "audit", "jerryfane/gitmoot"); err != nil {
		t.Fatalf("restore AllowAgentRepo returned error: %v", err)
	}
	agent, err := store.GetAgent(ctx, "audit")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Name != "audit" || agent.TemplateID != "thermo" || agent.Capabilities[0] != "review" {
		t.Fatalf("agent = %+v", agent)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents returned error: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "audit" {
		t.Fatalf("agents = %+v", agents)
	}
	if err := store.InsertGoal(ctx, Goal{ID: "goal-1", Title: "Build Gitmoot", Source: "GOAL.md", Status: "planned"}); err != nil {
		t.Fatalf("InsertGoal returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-1", GoalID: "goal-1", Title: "Bootstrap", State: "planned"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.InsertGoal(ctx, Goal{ID: "goal-2", Title: "Corrected Goal", Source: "GOAL.md", Status: "planned"}); err != nil {
		t.Fatalf("second InsertGoal returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-1", GoalID: "goal-2", Title: "Bootstrap", State: "planned"}); err != nil {
		t.Fatalf("second UpsertTask returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.GoalID != "goal-2" {
		t.Fatalf("task goal_id = %q, want goal-2", task.GoalID)
	}
	if err := store.UpsertPullRequest(ctx, PullRequest{RepoFullName: "jerryfane/gitmoot", Number: 1, URL: "https://github.com/jerryfane/gitmoot/pull/1", HeadBranch: "task", BaseBranch: "main", HeadSHA: "abc123", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 1)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadSHA != "abc123" {
		t.Fatalf("pull request head sha = %q, want abc123", pr.HeadSHA)
	}
	byBranch, err := store.GetPullRequestByRepoBranch(ctx, "jerryfane/gitmoot", "task")
	if err != nil {
		t.Fatalf("GetPullRequestByRepoBranch returned error: %v", err)
	}
	if byBranch.Number != 1 || byBranch.HeadSHA != "abc123" {
		t.Fatalf("pull request by branch = %+v", byBranch)
	}
	if err := store.MarkCommentSeen(ctx, Comment{RepoFullName: "jerryfane/gitmoot", CommentID: 100, PullRequest: 1, Body: "/gitmoot audit review"}); err != nil {
		t.Fatalf("MarkCommentSeen returned error: %v", err)
	}
	seen, err := store.HasCommentSeen(ctx, "jerryfane/gitmoot", 100)
	if err != nil {
		t.Fatalf("HasCommentSeen returned error: %v", err)
	}
	if !seen {
		t.Fatal("HasCommentSeen did not find marked comment")
	}
	isNew, err := store.MarkCommentSeenIfNew(ctx, Comment{RepoFullName: "jerryfane/gitmoot", CommentID: 101, PullRequest: 1, Body: "/gitmoot audit review again"})
	if err != nil {
		t.Fatalf("MarkCommentSeenIfNew returned error: %v", err)
	}
	if !isNew {
		t.Fatal("MarkCommentSeenIfNew did not report new comment")
	}
	isNew, err = store.MarkCommentSeenIfNew(ctx, Comment{RepoFullName: "jerryfane/gitmoot", CommentID: 101, PullRequest: 1, Body: "/gitmoot audit review again"})
	if err != nil {
		t.Fatalf("duplicate MarkCommentSeenIfNew returned error: %v", err)
	}
	if isNew {
		t.Fatal("MarkCommentSeenIfNew reported duplicate comment as new")
	}
	if err := store.CreateJob(ctx, Job{ID: "job-1", Agent: "audit", Type: "review", State: "queued"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != "queued" {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "job-1" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if err := store.UpdateJobState(ctx, "job-1", "running"); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	transitioned, err := store.TransitionJobState(ctx, "job-1", "queued", "running")
	if err != nil {
		t.Fatalf("TransitionJobState stale returned error: %v", err)
	}
	if transitioned {
		t.Fatal("TransitionJobState unexpectedly changed a non-matching state")
	}
	transitioned, err = store.TransitionJobState(ctx, "job-1", "running", "succeeded")
	if err != nil {
		t.Fatalf("TransitionJobState returned error: %v", err)
	}
	if !transitioned {
		t.Fatal("TransitionJobState did not change matching state")
	}
	if err := store.CreateJob(ctx, Job{ID: "job-2", Agent: "audit", Type: "review", State: "queued"}); err != nil {
		t.Fatalf("second CreateJob returned error: %v", err)
	}
	transitioned, err = store.TransitionJobStateWithEvent(ctx, "job-2", "queued", "running", JobEvent{Kind: "running", Message: "started"})
	if err != nil {
		t.Fatalf("TransitionJobStateWithEvent returned error: %v", err)
	}
	if !transitioned {
		t.Fatal("TransitionJobStateWithEvent did not change matching state")
	}
	jobEvents, err := store.ListJobEvents(ctx, "job-2")
	if err != nil {
		t.Fatalf("ListJobEvents for job-2 returned error: %v", err)
	}
	if len(jobEvents) != 1 || jobEvents[0].Kind != "running" {
		t.Fatalf("job-2 events = %+v", jobEvents)
	}
	if err := store.CreateJobWithEvent(ctx, Job{ID: "job-3", Agent: "audit", Type: "review", State: "queued"}, JobEvent{Kind: "queued", Message: "created"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	jobEvents, err = store.ListJobEvents(ctx, "job-3")
	if err != nil {
		t.Fatalf("ListJobEvents for job-3 returned error: %v", err)
	}
	if len(jobEvents) != 1 || jobEvents[0].Kind != "queued" {
		t.Fatalf("job-3 events = %+v", jobEvents)
	}
	transitioned, err = store.TransitionJobStatePayloadWithEvent(ctx, "job-3", "queued", "succeeded", `{"result":{"summary":"ok"}}`, JobEvent{Kind: "succeeded", Message: "done"})
	if err != nil {
		t.Fatalf("TransitionJobStatePayloadWithEvent returned error: %v", err)
	}
	if !transitioned {
		t.Fatal("TransitionJobStatePayloadWithEvent did not change matching state")
	}
	job, err = store.GetJob(ctx, "job-3")
	if err != nil {
		t.Fatalf("GetJob for job-3 returned error: %v", err)
	}
	if job.State != "succeeded" || job.Payload != `{"result":{"summary":"ok"}}` {
		t.Fatalf("job-3 = %+v", job)
	}
	if err := store.UpdateJobPayload(ctx, "job-1", `{"raw_outputs":["ok"]}`); err != nil {
		t.Fatalf("UpdateJobPayload returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, JobEvent{JobID: "job-1", Kind: "queued", Message: "created"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "queued" {
		t.Fatalf("events = %+v", events)
	}
	acquired, err := store.AcquireLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "lead"})
	if err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("first AcquireLock did not acquire lock")
	}
	acquired, err = store.AcquireLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "lead"})
	if err != nil {
		t.Fatalf("same-owner AcquireLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("same-owner AcquireLock did not return acquired")
	}
	lock, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.Owner != "lead" {
		t.Fatalf("lock owner = %q, want lead", lock.Owner)
	}
	created, err := store.CreateLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "lead"})
	if err != nil {
		t.Fatalf("CreateLock existing returned error: %v", err)
	}
	if created {
		t.Fatal("CreateLock reported existing lock as newly created")
	}
	acquired, err = store.AcquireLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "other"})
	if err != nil {
		t.Fatalf("second AcquireLock returned error: %v", err)
	}
	if acquired {
		t.Fatal("second AcquireLock unexpectedly acquired lock")
	}
	released, err := store.ReleaseLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "other"})
	if err != nil {
		t.Fatalf("wrong-owner ReleaseLock returned error: %v", err)
	}
	if released {
		t.Fatal("wrong-owner ReleaseLock released lock")
	}
	released, err = store.ReleaseLockWithEvent(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "lead"}, BranchLockEvent{Kind: "released", Message: "done"})
	if err != nil {
		t.Fatalf("ReleaseLockWithEvent returned error: %v", err)
	}
	if !released {
		t.Fatal("ReleaseLock did not release owned lock")
	}
	lockEvents, err := store.ListBranchLockEvents(ctx, "jerryfane/gitmoot", "task")
	if err != nil {
		t.Fatalf("ListBranchLockEvents returned error: %v", err)
	}
	if len(lockEvents) != 1 || lockEvents[0].Kind != "released" || lockEvents[0].Owner != "lead" {
		t.Fatalf("lock events = %+v", lockEvents)
	}
	if acquired, err := store.AcquireLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-force", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("force lock AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	releasedLock, released, err := store.ForceReleaseLockWithEvent(ctx, "jerryfane/gitmoot", "task-force", BranchLockEvent{Kind: "force_released", Message: "stale"})
	if err != nil {
		t.Fatalf("ForceReleaseLockWithEvent returned error: %v", err)
	}
	if !released || releasedLock.Owner != "lead" {
		t.Fatalf("force release returned lock=%+v released=%v", releasedLock, released)
	}
	if err := store.UpsertMergeGate(ctx, MergeGate{RepoFullName: "jerryfane/gitmoot", PullRequest: 1, State: "pending", Reason: "waiting"}); err != nil {
		t.Fatalf("UpsertMergeGate returned error: %v", err)
	}
	removed, err = store.RemoveAgent(ctx, "audit")
	if err != nil {
		t.Fatalf("RemoveAgent returned error: %v", err)
	}
	if !removed {
		t.Fatal("RemoveAgent did not remove existing agent")
	}
	agentRepos, err = store.ListAgentRepos(ctx, "audit")
	if err != nil {
		t.Fatalf("ListAgentRepos after RemoveAgent returned error: %v", err)
	}
	if len(agentRepos) != 0 {
		t.Fatalf("agent repos after RemoveAgent = %+v", agentRepos)
	}
	removed, err = store.RemoveAgent(ctx, "audit")
	if err != nil {
		t.Fatalf("second RemoveAgent returned error: %v", err)
	}
	if removed {
		t.Fatal("second RemoveAgent removed missing agent")
	}
}

func TestAddPendingAgentTemplateCandidateRollsBackArtifacts(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := AgentTemplate{
		ID:             "planner",
		Name:           "Planner",
		Description:    "Plans work",
		SourceRepo:     "local",
		SourceRef:      "current",
		SourcePath:     "planner.md",
		ResolvedCommit: "abc123",
		Content:        "Plan carefully.",
		MetadataJSON:   `{"id":"planner","name":"Planner","description":"Plans work","kind":"agent-template","version":1,"capabilities":["ask"],"runtime_compatibility":["codex"],"tags":["planning"],"inputs":["task"],"outputs":["plan"]}`,
	}
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	_, err = store.AddPendingAgentTemplateCandidate(ctx, AgentTemplate{
		ID:             "planner",
		Name:           "Planner Candidate",
		Description:    "Plans work",
		SourceRepo:     "gitmoot-skillopt",
		SourceRef:      "candidate",
		SourcePath:     "candidate.json",
		ResolvedCommit: "def456",
		Content:        "Plan carefully with risks.",
		MetadataJSON:   template.MetadataJSON,
	}, AgentTemplateCandidateReview{
		TemplateID:     "planner",
		BaseVersionID:  current.VersionID,
		DiffArtifactID: "candidate-diff",
		State:          "pending",
	}, []EvalArtifact{
		{ID: "candidate-diff", Hash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", MediaType: "text/markdown", SizeBytes: 10, Driver: "text"},
		{ID: "candidate-diff", Hash: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", MediaType: "text/markdown", SizeBytes: 11, Driver: "text"},
	})
	if err == nil {
		t.Fatal("AddPendingAgentTemplateCandidate returned nil error for duplicate artifact ids")
	}
	if _, err := store.GetEvalArtifact(ctx, "candidate-diff"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetEvalArtifact after rollback error = %v, want sql.ErrNoRows", err)
	}
	pending, err := store.ListPendingAgentTemplateVersions(ctx, "planner")
	if err != nil {
		t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending versions = %+v, want none", pending)
	}
	after, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate after rollback returned error: %v", err)
	}
	if after.VersionID != current.VersionID {
		t.Fatalf("template changed after rollback: before=%+v after=%+v", current, after)
	}
	latest, err := store.GetAgentTemplateReference(ctx, "planner@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest after rollback returned error: %v", err)
	}
	if latest.VersionID != current.VersionID {
		t.Fatalf("latest template changed after rollback: latest=%+v current=%+v", latest, current)
	}
}

func TestMigrateCopiesAgentRepoScopeToAgentRepos(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	store := &Store{db: raw}
	defer store.Close()

	agentReposMigration := len(migrations) - 1
	for i, migration := range migrations {
		if strings.Contains(migration, "CREATE TABLE agent_repos") {
			agentReposMigration = i
			break
		}
	}
	for version, migration := range migrations[:agentReposMigration] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d) returned error: %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, capabilities_json, autonomy_policy, health_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, "audit", "reviewer", "codex", "last", "jerryfane/gitmoot", `["review"]`, "auto", "ok"); err != nil {
		t.Fatalf("insert legacy agent returned error: %v", err)
	}
	if _, err := store.ListAgentRepos(ctx, "audit"); err == nil {
		t.Fatal("ListAgentRepos succeeded before agent_repos migration")
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	repos, err := store.ListAgentRepos(ctx, "audit")
	if err != nil {
		t.Fatalf("ListAgentRepos returned error: %v", err)
	}
	if len(repos) != 1 || repos[0] != "jerryfane/gitmoot" {
		t.Fatalf("repos = %+v", repos)
	}
}

func TestMigrateAppendsAgentInstanceAutonomyPolicy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	store := &Store{db: raw}
	for version, migration := range migrations[:len(migrations)-1] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d) returned error: %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO agent_instances(name, type, runtime, runtime_ref, repo_full_name, role, template_id, capabilities_json, state, created_at, last_used_at, expires_at)
		VALUES ('planner-bg-legacy', 'planner', 'codex', 'session-id', 'owner/repo', 'planner', '', '["ask"]', 'idle', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z')`); err != nil {
		t.Fatalf("insert legacy agent instance returned error: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close returned error: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("Open upgraded DB returned error: %v", err)
	}
	defer upgraded.Close()
	instance, err := upgraded.GetAgentInstance(ctx, "planner-bg-legacy")
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.AutonomyPolicy != "auto" {
		t.Fatalf("autonomy policy = %q, want auto", instance.AutonomyPolicy)
	}
}

func TestMigrateAppendsTaskWorktreePath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	store := &Store{db: raw}
	for version, migration := range migrations[:len(migrations)-1] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d) returned error: %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO tasks(id, repo_full_name, goal_id, title, state, branch)
		VALUES ('task-legacy', 'owner/repo', 'goal-1', 'Legacy', 'planned', 'task-legacy')`); err != nil {
		t.Fatalf("insert legacy task returned error: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close returned error: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("Open upgraded DB returned error: %v", err)
	}
	defer upgraded.Close()
	task, err := upgraded.GetTask(ctx, "task-legacy")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.WorktreePath != "" {
		t.Fatalf("worktree path = %q, want empty default", task.WorktreePath)
	}
}

func TestTaskWorktreePathStorage(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertTask(ctx, Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "First", State: "planned", Branch: "task-1", WorktreePath: "/tmp/gitmoot/worktrees/owner--repo/task-1"}); err != nil {
		t.Fatalf("UpsertTask with worktree returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.WorktreePath != "/tmp/gitmoot/worktrees/owner--repo/task-1" {
		t.Fatalf("worktree path = %q", task.WorktreePath)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Updated", State: "implementing", Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask update returned error: %v", err)
	}
	updated, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask updated returned error: %v", err)
	}
	if updated.WorktreePath != task.WorktreePath {
		t.Fatalf("worktree path was not preserved: %q", updated.WorktreePath)
	}
	if updated.State != "implementing" || updated.Title != "Updated" {
		t.Fatalf("task update did not apply: %+v", updated)
	}
	if err := store.ClearTaskWorktreePath(ctx, "task-1"); err != nil {
		t.Fatalf("ClearTaskWorktreePath returned error: %v", err)
	}
	cleared, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask cleared returned error: %v", err)
	}
	if cleared.WorktreePath != "" {
		t.Fatalf("cleared worktree path = %q, want empty", cleared.WorktreePath)
	}
}

func TestTasksRequireUniqueNonEmptyBranches(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertTask(ctx, Task{ID: "task-1", GoalID: "goal-1", Title: "First", State: "planned", Branch: "task-branch"}); err != nil {
		t.Fatalf("UpsertTask first returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-2", GoalID: "goal-1", Title: "Second", State: "planned", Branch: "task-branch"}); err == nil {
		t.Fatal("UpsertTask allowed two tasks to share one branch")
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-empty-1", GoalID: "goal-1", Title: "Empty 1", State: "planned"}); err != nil {
		t.Fatalf("UpsertTask empty first returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-empty-2", GoalID: "goal-1", Title: "Empty 2", State: "planned"}); err != nil {
		t.Fatalf("UpsertTask empty second returned error: %v", err)
	}
}

func TestTasksAllowSameBranchAcrossRepos(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	first := Task{ID: "task-1", RepoFullName: "jerryfane/gitmoot", GoalID: "goal-1", Title: "First", State: "planned", Branch: "task-branch"}
	second := Task{ID: "task-2", RepoFullName: "jerryfane/other", GoalID: "goal-1", Title: "Second", State: "planned", Branch: "task-branch"}
	if err := store.UpsertTask(ctx, first); err != nil {
		t.Fatalf("UpsertTask first returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, second); err != nil {
		t.Fatalf("UpsertTask second repo returned error: %v", err)
	}
	got, err := store.GetTaskByRepoBranch(ctx, "jerryfane/other", "task-branch")
	if err != nil {
		t.Fatalf("GetTaskByRepoBranch returned error: %v", err)
	}
	if got.ID != "task-2" {
		t.Fatalf("repo scoped task = %q, want task-2", got.ID)
	}
}

func TestMigrationDeduplicatesExistingTaskBranches(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations returned error: %v", err)
	}
	for version, migration := range migrations[:2] {
		if _, err := raw.ExecContext(ctx, migration); err != nil {
			t.Fatalf("apply seed migration %d returned error: %v", version+1, err)
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'test')`, version+1); err != nil {
			t.Fatalf("record seed migration %d returned error: %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO tasks(id, goal_id, title, state, branch, updated_at) VALUES
		('task-old', 'goal-1', 'Old', 'planned', 'task-branch', '2026-01-01T00:00:00Z'),
		('task-new', 'goal-1', 'New', 'planned', 'task-branch', '2026-01-02T00:00:00Z')`); err != nil {
		t.Fatalf("insert duplicate tasks returned error: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close returned error: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	kept, err := store.GetTaskByBranch(ctx, "task-branch")
	if err != nil {
		t.Fatalf("GetTaskByBranch returned error: %v", err)
	}
	if kept.ID != "task-new" {
		t.Fatalf("kept task = %q, want latest task-new", kept.ID)
	}
	old, err := store.GetTask(ctx, "task-old")
	if err != nil {
		t.Fatalf("GetTask old returned error: %v", err)
	}
	if old.Branch != "" {
		t.Fatalf("duplicate task branch = %q, want cleared", old.Branch)
	}
}

func TestMigrationCopiesPresetsToAgentTemplates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations returned error: %v", err)
	}
	templateMigration := len(migrations) - 1
	for i, migration := range migrations {
		if strings.Contains(migration, "DROP TABLE presets") {
			templateMigration = i
			break
		}
	}
	for version, migration := range migrations[:templateMigration] {
		if _, err := raw.ExecContext(ctx, migration); err != nil {
			t.Fatalf("apply seed migration %d returned error: %v", version+1, err)
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'test')`, version+1); err != nil {
			t.Fatalf("record seed migration %d returned error: %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO presets(id, name, description, source_repo, source_ref, source_path, resolved_commit, content, created_at, updated_at)
		VALUES ('legacy-template', 'Legacy Template', 'old description', 'owner/repo', 'main', 'path.md', 'abc123', 'legacy instructions', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy preset returned error: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope, preset_id, capabilities_json, autonomy_policy, health_status)
		VALUES ('legacy-agent', 'reviewer', 'codex', 'session-id', 'owner/repo', 'legacy-template', '["review"]', 'auto', 'ok')`); err != nil {
		t.Fatalf("insert legacy agent returned error: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO agent_instances(name, type, runtime, runtime_ref, repo_full_name, role, preset_id, capabilities_json, state, created_at, last_used_at, expires_at)
		VALUES ('legacy-instance', 'reviewer', 'codex', 'session-id', 'owner/repo', 'reviewer', 'legacy-template', '["review"]', 'idle', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z')`); err != nil {
		t.Fatalf("insert legacy agent instance returned error: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close returned error: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	template, err := store.GetAgentTemplate(ctx, "legacy-template")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if template.Content != "legacy instructions" || template.ResolvedCommit != "abc123" || template.MetadataJSON != "" {
		t.Fatalf("template = %+v", template)
	}
	versions, err := store.ListAgentTemplateVersions(ctx, "legacy-template")
	if err != nil {
		t.Fatalf("ListAgentTemplateVersions returned error: %v", err)
	}
	if len(versions) != 1 || versions[0].ID != "legacy-template@v1" || versions[0].State != "current" || versions[0].Content != "legacy instructions" {
		t.Fatalf("legacy versions = %+v", versions)
	}
	agent, err := store.GetAgent(ctx, "legacy-agent")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.TemplateID != "legacy-template" {
		t.Fatalf("agent template id = %q, want legacy-template", agent.TemplateID)
	}
	instance, err := store.GetAgentInstance(ctx, "legacy-instance")
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if instance.TemplateID != "legacy-template" {
		t.Fatalf("agent instance template id = %q, want legacy-template", instance.TemplateID)
	}
	hasPresets, err := store.HasTable(ctx, "presets")
	if err != nil {
		t.Fatalf("HasTable(presets) returned error: %v", err)
	}
	if hasPresets {
		t.Fatal("legacy presets table still exists")
	}
}

func TestListJobsByParent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.CreateJob(ctx, Job{ID: "parent", Agent: "planner", Type: "ask", State: "running"}); err != nil {
		t.Fatalf("CreateJob parent returned error: %v", err)
	}
	// Two delegation children (inserted out of delegation_id order) plus the
	// continuation child (empty delegation id) and one unrelated job.
	if err := store.CreateJob(ctx, Job{ID: "child-ui", Agent: "coder", Type: "implement", State: "running", ParentJobID: "parent", DelegationID: "ui"}); err != nil {
		t.Fatalf("CreateJob child-ui returned error: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "child-api", Agent: "coder", Type: "implement", State: "succeeded", ParentJobID: "parent", DelegationID: "api"}); err != nil {
		t.Fatalf("CreateJob child-api returned error: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "child-cont", Agent: "planner", Type: "ask", State: "queued", ParentJobID: "parent"}); err != nil {
		t.Fatalf("CreateJob child-cont returned error: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "unrelated", Agent: "planner", Type: "ask", State: "queued"}); err != nil {
		t.Fatalf("CreateJob unrelated returned error: %v", err)
	}

	children, err := store.ListJobsByParent(ctx, "parent")
	if err != nil {
		t.Fatalf("ListJobsByParent returned error: %v", err)
	}
	// ORDER BY delegation_id, id: continuation (empty id) first, then api, ui.
	wantIDs := []string{"child-cont", "child-api", "child-ui"}
	if len(children) != len(wantIDs) {
		t.Fatalf("ListJobsByParent returned %d children, want %d: %+v", len(children), len(wantIDs), children)
	}
	for i, want := range wantIDs {
		if children[i].ID != want {
			t.Fatalf("child[%d].ID = %q, want %q (order %+v)", i, children[i].ID, want, children)
		}
	}
	if children[1].UpdatedAt == "" {
		t.Fatalf("ListJobsByParent should populate UpdatedAt for child age: %+v", children[1])
	}

	empty, err := store.ListJobsByParent(ctx, "no-such-parent")
	if err != nil {
		t.Fatalf("ListJobsByParent unknown parent returned error: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("ListJobsByParent for unknown parent = %+v, want empty", empty)
	}
}

func TestUpsertAgentPersistsModel(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	agent := Agent{
		Name:    "modeled",
		Role:    "dev",
		Runtime: "claude",
		Model:   "claude-opus-4-1",
	}
	if err := store.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}

	got, err := store.GetAgent(ctx, "modeled")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if got.Model != "claude-opus-4-1" {
		t.Fatalf("GetAgent model = %q, want %q", got.Model, "claude-opus-4-1")
	}

	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents returned error: %v", err)
	}
	found := false
	for _, a := range agents {
		if a.Name == "modeled" {
			found = true
			if a.Model != "claude-opus-4-1" {
				t.Fatalf("ListAgents model = %q, want %q", a.Model, "claude-opus-4-1")
			}
		}
	}
	if !found {
		t.Fatalf("ListAgents did not return agent %q", "modeled")
	}

	// An empty model upserts and reads back empty (additive, no default).
	plain := Agent{Name: "plain", Role: "dev", Runtime: "codex"}
	if err := store.UpsertAgent(ctx, plain); err != nil {
		t.Fatalf("UpsertAgent (plain) returned error: %v", err)
	}
	gotPlain, err := store.GetAgent(ctx, "plain")
	if err != nil {
		t.Fatalf("GetAgent (plain) returned error: %v", err)
	}
	if gotPlain.Model != "" {
		t.Fatalf("GetAgent (plain) model = %q, want empty", gotPlain.Model)
	}
}

func TestUpsertAgentInstancePersistsModel(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	instance := AgentInstance{
		Name:         "inst-1",
		Type:         "dev",
		Runtime:      "claude",
		RepoFullName: "owner/repo",
		Role:         "dev",
		Model:        "claude-sonnet-4-5",
		State:        "idle",
	}
	if err := store.UpsertAgentInstance(ctx, instance); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}

	got, err := store.GetAgentInstance(ctx, "inst-1")
	if err != nil {
		t.Fatalf("GetAgentInstance returned error: %v", err)
	}
	if got.Model != "claude-sonnet-4-5" {
		t.Fatalf("GetAgentInstance model = %q, want %q", got.Model, "claude-sonnet-4-5")
	}

	// GetAgent falls back to the instance when no registered agent exists,
	// and surfaces the instance's model.
	agent, err := store.GetAgent(ctx, "inst-1")
	if err != nil {
		t.Fatalf("GetAgent (instance fallback) returned error: %v", err)
	}
	if agent.Model != "claude-sonnet-4-5" {
		t.Fatalf("GetAgent (instance fallback) model = %q, want %q", agent.Model, "claude-sonnet-4-5")
	}
}

func TestSkillOptJudgeOutcomeCRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	outcome := SkillOptJudgeOutcome{
		ID:                 "judge-outcome-1",
		CandidateVersionID: "planner@v2",
		TemplateID:         "planner",
		JudgeScoreJSON:     `{"soft":0.81,"quality_status":"pass"}`,
		JudgePromptVersion: "jp-2026-01",
		JudgeEvaluatorID:   "landing_page_v1",
		JudgePromptHash:    "deadbeef",
		HumanDecision:      "promoted",
		Direction:          SkillOptJudgeDirectionAgreeAccept,
		Reason:             "looks good",
	}
	if err := store.InsertSkillOptJudgeOutcome(ctx, outcome); err != nil {
		t.Fatalf("InsertSkillOptJudgeOutcome returned error: %v", err)
	}

	got, err := store.GetSkillOptJudgeOutcome(ctx, "judge-outcome-1")
	if err != nil {
		t.Fatalf("GetSkillOptJudgeOutcome returned error: %v", err)
	}
	if got.CandidateVersionID != outcome.CandidateVersionID ||
		got.TemplateID != outcome.TemplateID ||
		got.JudgeScoreJSON != outcome.JudgeScoreJSON ||
		got.JudgePromptVersion != outcome.JudgePromptVersion ||
		got.JudgeEvaluatorID != outcome.JudgeEvaluatorID ||
		got.JudgePromptHash != outcome.JudgePromptHash ||
		got.HumanDecision != outcome.HumanDecision ||
		got.Direction != outcome.Direction ||
		got.Reason != outcome.Reason {
		t.Fatalf("GetSkillOptJudgeOutcome = %+v, want %+v", got, outcome)
	}
	if strings.TrimSpace(got.CreatedAt) == "" {
		t.Fatalf("expected created_at to be populated, got %+v", got)
	}

	// A second outcome on a different template, plus an auto-generated id.
	other := SkillOptJudgeOutcome{
		CandidateVersionID: "reviewer@v3",
		TemplateID:         "reviewer",
		HumanDecision:      "rejected",
		Direction:          SkillOptJudgeDirectionAgreeReject,
	}
	if err := store.InsertSkillOptJudgeOutcome(ctx, other); err != nil {
		t.Fatalf("InsertSkillOptJudgeOutcome (auto id) returned error: %v", err)
	}

	all, err := store.ListSkillOptJudgeOutcomes(ctx, "")
	if err != nil {
		t.Fatalf("ListSkillOptJudgeOutcomes(all) returned error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListSkillOptJudgeOutcomes(all) = %d rows, want 2", len(all))
	}

	plannerOnly, err := store.ListSkillOptJudgeOutcomes(ctx, "planner")
	if err != nil {
		t.Fatalf("ListSkillOptJudgeOutcomes(planner) returned error: %v", err)
	}
	if len(plannerOnly) != 1 || plannerOnly[0].ID != "judge-outcome-1" {
		t.Fatalf("ListSkillOptJudgeOutcomes(planner) = %+v, want only judge-outcome-1", plannerOnly)
	}

	// Required fields are validated.
	if err := store.InsertSkillOptJudgeOutcome(ctx, SkillOptJudgeOutcome{HumanDecision: "promoted", Direction: SkillOptJudgeDirectionAgreeAccept}); err == nil {
		t.Fatal("InsertSkillOptJudgeOutcome with empty candidate_version_id returned nil error")
	}
}

func TestDecideSkillOptTrainCandidateCapturesJudgeOutcome(t *testing.T) {
	tests := []struct {
		name          string
		evalReport    string
		decision      string
		reason        string
		wantDirection string
	}{
		{
			name:          "promote with judge accept agrees",
			evalReport:    `{"evaluator_score":{"soft":0.9,"quality_status":"pass","judge_prompt_version":"jp-1","evaluator_id":"landing_page_v1","judge_prompt_hash":"abc123"}}`,
			decision:      "promoted",
			wantDirection: SkillOptJudgeDirectionAgreeAccept,
		},
		{
			name:          "promote with judge reject is judge_reject_human_accept",
			evalReport:    `{"soft":0.2,"quality_status":"fail"}`,
			decision:      "promoted",
			wantDirection: SkillOptJudgeDirectionJudgeRejectHumanAccept,
		},
		{
			name:          "reject with judge reject agrees",
			evalReport:    `{"promotable":false,"soft":0.1}`,
			decision:      "rejected",
			reason:        "not actionable",
			wantDirection: SkillOptJudgeDirectionAgreeReject,
		},
		{
			name:          "reject with judge accept is judge_accept_human_reject",
			evalReport:    `{"promotable":true,"soft":0.95}`,
			decision:      "rejected",
			reason:        "style mismatch",
			wantDirection: SkillOptJudgeDirectionJudgeAcceptHumanReject,
		},
		{
			// Real landing-page profile shape (no "promotable"): judge-accept is
			// derived from best_selection_soft. Promote => agree.
			name:          "landing-page promote agrees via best_selection_soft",
			evalReport:    `{"best_selection_hard":1.0,"best_selection_soft":0.88,"best_step":1,"dry_run":false}`,
			decision:      "promoted",
			wantDirection: SkillOptJudgeDirectionAgreeAccept,
		},
		{
			// Real landing-page shape the gate scored well but the human rejected:
			// the false-positive calibration signal we most want to capture.
			name:          "landing-page reject captures judge over-score",
			evalReport:    `{"best_selection_hard":0.9,"best_selection_soft":0.86,"best_step":2}`,
			decision:      "rejected",
			reason:        "off-brand",
			wantDirection: SkillOptJudgeDirectionJudgeAcceptHumanReject,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatalf("Open returned error: %v", err)
			}
			defer store.Close()

			candidate := seedSkillOptJudgeCandidate(t, store, tc.evalReport)
			session := SkillOptTrainSession{ID: "session-1", TemplateID: "planner", State: "review_published"}
			iteration := SkillOptTrainIteration{ID: "session-1-001", SessionID: "session-1", State: "review_published", DecisionReason: tc.reason}

			if _, err := store.DecideSkillOptTrainCandidate(ctx, session, iteration, candidate.ID, tc.decision); err != nil {
				t.Fatalf("DecideSkillOptTrainCandidate returned error: %v", err)
			}

			outcomes, err := store.ListSkillOptJudgeOutcomes(ctx, "planner")
			if err != nil {
				t.Fatalf("ListSkillOptJudgeOutcomes returned error: %v", err)
			}
			if len(outcomes) != 1 {
				t.Fatalf("captured %d outcomes, want 1", len(outcomes))
			}
			outcome := outcomes[0]
			if outcome.CandidateVersionID != candidate.ID {
				t.Fatalf("captured candidate_version_id = %q, want %q", outcome.CandidateVersionID, candidate.ID)
			}
			if outcome.Direction != tc.wantDirection {
				t.Fatalf("captured direction = %q, want %q", outcome.Direction, tc.wantDirection)
			}
			if outcome.HumanDecision != tc.decision {
				t.Fatalf("captured human_decision = %q, want %q", outcome.HumanDecision, tc.decision)
			}
			if outcome.JudgeScoreJSON != tc.evalReport {
				t.Fatalf("captured judge_score_json = %q, want raw eval report %q", outcome.JudgeScoreJSON, tc.evalReport)
			}
			if tc.reason != "" && outcome.Reason != tc.reason {
				t.Fatalf("captured reason = %q, want %q", outcome.Reason, tc.reason)
			}
		})
	}
}

func TestDecideSkillOptTrainCandidateSkipsCaptureWithoutJudgeSignal(t *testing.T) {
	// A decision whose eval report carries no recognizable judge signal must not
	// record a misleading "judge rejected" outcome — it is skipped entirely.
	for _, report := range []string{"", "{}", `{"summary":"no judge fields here"}`} {
		ctx := context.Background()
		store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
		if err != nil {
			t.Fatalf("Open returned error: %v", err)
		}
		candidate := seedSkillOptJudgeCandidate(t, store, report)
		session := SkillOptTrainSession{ID: "session-1", TemplateID: "planner", State: "review_published"}
		iteration := SkillOptTrainIteration{ID: "session-1-001", SessionID: "session-1", State: "review_published"}
		if _, err := store.DecideSkillOptTrainCandidate(ctx, session, iteration, candidate.ID, "promoted"); err != nil {
			t.Fatalf("DecideSkillOptTrainCandidate returned error: %v", err)
		}
		outcomes, err := store.ListSkillOptJudgeOutcomes(ctx, "planner")
		if err != nil {
			t.Fatalf("ListSkillOptJudgeOutcomes returned error: %v", err)
		}
		if len(outcomes) != 0 {
			t.Fatalf("report %q: captured %d outcomes, want 0 (no judge signal)", report, len(outcomes))
		}
		store.Close()
	}
}

func TestSkillOptJudgeAcceptFromRealReportShapes(t *testing.T) {
	// Shapes taken from real optimizer eval reports in the wild (not synthetic):
	// the generic profile sets top-level "promotable"; the landing-page profile
	// instead reports "best_selection_soft"/"best_selection_hard" with no
	// "promotable". Both must yield a signal so the decision is captured.
	cases := []struct {
		name       string
		report     string
		wantAccept bool
		wantSignal bool
	}{
		{"generic promotable true", `{"promotable":true,"best_selection_soft":0.88}`, true, true},
		{"generic promotable false", `{"promotable":false,"best_selection_soft":0.88}`, false, true},
		{"landing-page best_selection_soft high", `{"best_selection_hard":1.0,"best_selection_soft":0.88,"best_step":1,"dry_run":false}`, true, true},
		{"landing-page best_selection_soft low", `{"best_selection_hard":0.0,"best_selection_soft":0.2,"best_step":0}`, false, true},
		{"landing-page hard only", `{"best_selection_hard":0.9,"best_step":1}`, true, true},
		{"no signal", `{"best_step":0,"dry_run":true}`, false, false},
	}
	for _, tc := range cases {
		accept, hasSignal, _, _, _ := skillOptJudgeAcceptFromReport(tc.report)
		if hasSignal != tc.wantSignal {
			t.Fatalf("%s: hasSignal=%v want %v (report=%s)", tc.name, hasSignal, tc.wantSignal, tc.report)
		}
		if hasSignal && accept != tc.wantAccept {
			t.Fatalf("%s: accept=%v want %v", tc.name, accept, tc.wantAccept)
		}
	}
}

func seedSkillOptJudgeCandidate(t *testing.T, store *Store, evalReportJSON string) AgentTemplateVersion {
	t.Helper()
	ctx := context.Background()
	template := AgentTemplate{
		ID:             "planner",
		Name:           "Planner",
		Description:    "Plans work",
		SourceRepo:     "local",
		SourceRef:      "current",
		SourcePath:     "planner.md",
		ResolvedCommit: "abc123",
		Content:        "Plan carefully.",
		MetadataJSON:   `{"id":"planner","name":"Planner","description":"Plans work","kind":"agent-template","version":1,"capabilities":["ask"],"runtime_compatibility":["codex"],"tags":["planning"],"inputs":["task"],"outputs":["plan"]}`,
	}
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	candidate, err := store.AddPendingAgentTemplateCandidate(ctx, AgentTemplate{
		ID:             "planner",
		Name:           "Planner Candidate",
		Description:    "Plans work",
		SourceRepo:     "gitmoot-skillopt",
		SourceRef:      "candidate",
		SourcePath:     "candidate.json",
		ResolvedCommit: "def456",
		Content:        "Plan carefully with risks.",
		MetadataJSON:   template.MetadataJSON,
	}, AgentTemplateCandidateReview{
		TemplateID:     "planner",
		BaseVersionID:  current.VersionID,
		EvalReportJSON: evalReportJSON,
		State:          "pending",
	}, nil)
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateCandidate returned error: %v", err)
	}
	return candidate
}

func TestCockpitPaneCRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	pane := CockpitPane{
		ID:          "cockpit-pane-1",
		JobID:       "job-child-1",
		PaneKey:     "job:job-child-1",
		RootJobID:   "job-root",
		PaneID:      "pane-aaa",
		WorkspaceID: "ws-1",
		Source:      "custom:gitmoot",
	}
	if err := store.InsertCockpitPane(ctx, pane); err != nil {
		t.Fatalf("InsertCockpitPane returned error: %v", err)
	}

	got, err := store.GetCockpitPaneByJob(ctx, "job-child-1")
	if err != nil {
		t.Fatalf("GetCockpitPaneByJob returned error: %v", err)
	}
	if got.ID != pane.ID ||
		got.JobID != pane.JobID ||
		got.PaneKey != pane.PaneKey ||
		got.RootJobID != pane.RootJobID ||
		got.PaneID != pane.PaneID ||
		got.WorkspaceID != pane.WorkspaceID ||
		got.Source != pane.Source {
		t.Fatalf("GetCockpitPaneByJob = %+v, want %+v", got, pane)
	}
	if strings.TrimSpace(got.CreatedAt) == "" {
		t.Fatalf("expected created_at to be populated, got %+v", got)
	}

	// A second pane under the same root, with an auto-generated id and a
	// distinct pane_key (so the UNIQUE(workspace_id, pane_key) holds).
	second := CockpitPane{
		JobID:       "job-child-2",
		PaneKey:     "job:job-child-2",
		RootJobID:   "job-root",
		PaneID:      "pane-bbb",
		WorkspaceID: "ws-1",
		Source:      "custom:gitmoot",
	}
	if err := store.InsertCockpitPane(ctx, second); err != nil {
		t.Fatalf("InsertCockpitPane (auto id) returned error: %v", err)
	}

	// A pane under a different root must not leak into the root listing.
	other := CockpitPane{
		JobID:       "job-other",
		PaneKey:     "job:job-other",
		RootJobID:   "job-root-2",
		PaneID:      "pane-ccc",
		WorkspaceID: "ws-2",
		Source:      "custom:gitmoot",
	}
	if err := store.InsertCockpitPane(ctx, other); err != nil {
		t.Fatalf("InsertCockpitPane (other root) returned error: %v", err)
	}

	rootPanes, err := store.ListCockpitPanesByRoot(ctx, "job-root")
	if err != nil {
		t.Fatalf("ListCockpitPanesByRoot returned error: %v", err)
	}
	if len(rootPanes) != 2 {
		t.Fatalf("ListCockpitPanesByRoot(job-root) = %d rows, want 2", len(rootPanes))
	}
	if rootPanes[0].JobID != "job-child-1" || rootPanes[1].JobID != "job-child-2" {
		t.Fatalf("ListCockpitPanesByRoot ordering = %s, %s; want job-child-1, job-child-2",
			rootPanes[0].JobID, rootPanes[1].JobID)
	}

	// Delete the first pane; the listing shrinks and the by-job lookup misses.
	if err := store.DeleteCockpitPane(ctx, "cockpit-pane-1"); err != nil {
		t.Fatalf("DeleteCockpitPane returned error: %v", err)
	}
	if _, err := store.GetCockpitPaneByJob(ctx, "job-child-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetCockpitPaneByJob after delete = %v, want sql.ErrNoRows", err)
	}
	rootPanes, err = store.ListCockpitPanesByRoot(ctx, "job-root")
	if err != nil {
		t.Fatalf("ListCockpitPanesByRoot after delete returned error: %v", err)
	}
	if len(rootPanes) != 1 || rootPanes[0].JobID != "job-child-2" {
		t.Fatalf("ListCockpitPanesByRoot after delete = %+v, want only job-child-2", rootPanes)
	}

	// Deleting a missing row is a no-op, not an error.
	if err := store.DeleteCockpitPane(ctx, "cockpit-pane-missing"); err != nil {
		t.Fatalf("DeleteCockpitPane(missing) returned error: %v", err)
	}

	// job_id is required.
	if err := store.InsertCockpitPane(ctx, CockpitPane{PaneKey: "k", WorkspaceID: "ws-x"}); err == nil {
		t.Fatal("InsertCockpitPane with empty job_id returned nil error")
	}
}

func TestCockpitPaneUniquePerWorkspaceSeat(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	first := CockpitPane{
		JobID:       "job-a",
		PaneKey:     "seat:planner",
		RootJobID:   "root",
		PaneID:      "pane-1",
		WorkspaceID: "ws-1",
	}
	if err := store.InsertCockpitPane(ctx, first); err != nil {
		t.Fatalf("InsertCockpitPane(first) returned error: %v", err)
	}

	// Same (workspace_id, pane_key) for a different job must be rejected.
	dup := CockpitPane{
		JobID:       "job-b",
		PaneKey:     "seat:planner",
		RootJobID:   "root",
		PaneID:      "pane-2",
		WorkspaceID: "ws-1",
	}
	if err := store.InsertCockpitPane(ctx, dup); err == nil {
		t.Fatal("InsertCockpitPane with duplicate (workspace_id, pane_key) returned nil error")
	}

	// The same pane_key in a different workspace is allowed.
	otherWS := CockpitPane{
		JobID:       "job-c",
		PaneKey:     "seat:planner",
		RootJobID:   "root",
		PaneID:      "pane-3",
		WorkspaceID: "ws-2",
	}
	if err := store.InsertCockpitPane(ctx, otherWS); err != nil {
		t.Fatalf("InsertCockpitPane(other workspace) returned error: %v", err)
	}
}

func TestDeleteCockpitPaneByJob(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	pane := CockpitPane{
		JobID:       "job-x",
		PaneKey:     "job:job-x",
		RootJobID:   "root",
		PaneID:      "pane-1",
		WorkspaceID: "ws-1",
		Source:      "custom:gitmoot",
	}
	if err := store.InsertCockpitPane(ctx, pane); err != nil {
		t.Fatalf("InsertCockpitPane returned error: %v", err)
	}

	// Delete by job id (the cockpit teardown path) removes the row without
	// knowing its generated primary key.
	if err := store.DeleteCockpitPaneByJob(ctx, "job-x"); err != nil {
		t.Fatalf("DeleteCockpitPaneByJob returned error: %v", err)
	}
	if _, err := store.GetCockpitPaneByJob(ctx, "job-x"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetCockpitPaneByJob after delete-by-job = %v, want sql.ErrNoRows", err)
	}

	// The slot is reclaimable: re-inserting the same (workspace_id, pane_key)
	// after a delete-by-job must not hit the UNIQUE constraint.
	if err := store.InsertCockpitPane(ctx, pane); err != nil {
		t.Fatalf("re-insert after DeleteCockpitPaneByJob returned error: %v", err)
	}

	// Deleting a missing job is a no-op, not an error.
	if err := store.DeleteCockpitPaneByJob(ctx, "job-missing"); err != nil {
		t.Fatalf("DeleteCockpitPaneByJob(missing) returned error: %v", err)
	}
}

// TestGetCockpitPaneByKey: the seat-reuse lookup finds the pane for a
// (workspace_id, pane_key) seat and reports a clean miss (false, nil) — never
// sql.ErrNoRows — when none exists.
func TestGetCockpitPaneByKey(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	pane := CockpitPane{
		JobID:       "job-seat",
		PaneKey:     "seat:builder",
		RootJobID:   "root",
		PaneID:      "pane-seat",
		WorkspaceID: "ws-1",
		Source:      "custom:gitmoot",
	}
	if err := store.InsertCockpitPane(ctx, pane); err != nil {
		t.Fatalf("InsertCockpitPane returned error: %v", err)
	}

	got, found, err := store.GetCockpitPaneByKey(ctx, "ws-1", "seat:builder")
	if err != nil {
		t.Fatalf("GetCockpitPaneByKey returned error: %v", err)
	}
	if !found {
		t.Fatal("GetCockpitPaneByKey did not find the seat pane")
	}
	if got.PaneID != "pane-seat" || got.JobID != "job-seat" {
		t.Fatalf("GetCockpitPaneByKey = %+v, want pane-seat/job-seat", got)
	}

	// A miss is a clean (false, nil), not sql.ErrNoRows.
	_, found, err = store.GetCockpitPaneByKey(ctx, "ws-1", "seat:nobody")
	if err != nil {
		t.Fatalf("GetCockpitPaneByKey(miss) returned error: %v", err)
	}
	if found {
		t.Fatal("GetCockpitPaneByKey(miss) reported found")
	}
	_, found, err = store.GetCockpitPaneByKey(ctx, "ws-other", "seat:builder")
	if err != nil || found {
		t.Fatalf("GetCockpitPaneByKey(other workspace) = (found=%v, err=%v), want (false, nil)", found, err)
	}
}

// TestListAllCockpitPanes: the reconcile sweep can enumerate every pane across all
// roots, oldest first.
func TestListAllCockpitPanes(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if all, err := store.ListAllCockpitPanes(ctx); err != nil || len(all) != 0 {
		t.Fatalf("ListAllCockpitPanes(empty) = (%d rows, %v), want (0, nil)", len(all), err)
	}

	panes := []CockpitPane{
		{JobID: "job-1", PaneKey: "seat:a", RootJobID: "root-1", PaneID: "p1", WorkspaceID: "ws-1"},
		{JobID: "job-2", PaneKey: "seat:b", RootJobID: "root-1", PaneID: "p2", WorkspaceID: "ws-1"},
		{JobID: "job-3", PaneKey: "seat:c", RootJobID: "root-2", PaneID: "p3", WorkspaceID: "ws-2"},
	}
	for _, p := range panes {
		if err := store.InsertCockpitPane(ctx, p); err != nil {
			t.Fatalf("InsertCockpitPane(%s) returned error: %v", p.JobID, err)
		}
	}
	all, err := store.ListAllCockpitPanes(ctx)
	if err != nil {
		t.Fatalf("ListAllCockpitPanes returned error: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListAllCockpitPanes = %d rows, want 3", len(all))
	}
	seen := map[string]bool{}
	for _, p := range all {
		seen[p.JobID] = true
	}
	for _, want := range []string{"job-1", "job-2", "job-3"} {
		if !seen[want] {
			t.Fatalf("ListAllCockpitPanes missing %s; got %+v", want, all)
		}
	}
}

func TestGetOrCreateWorkspaceForRoot(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	var calls atomic.Int64
	create := func() (string, error) {
		calls.Add(1)
		return "ws-created", nil
	}

	// First call creates and binds the workspace.
	ws, err := store.GetOrCreateWorkspaceForRoot(ctx, "root-1", create)
	if err != nil {
		t.Fatalf("GetOrCreateWorkspaceForRoot returned error: %v", err)
	}
	if ws != "ws-created" {
		t.Fatalf("workspace id = %q, want ws-created", ws)
	}

	// Repeated calls for the same root return the bound id without calling create.
	for i := 0; i < 3; i++ {
		ws, err := store.GetOrCreateWorkspaceForRoot(ctx, "root-1", create)
		if err != nil {
			t.Fatalf("GetOrCreateWorkspaceForRoot (repeat) returned error: %v", err)
		}
		if ws != "ws-created" {
			t.Fatalf("workspace id (repeat) = %q, want ws-created", ws)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("create called %d times, want exactly 1", got)
	}

	// A different root creates its own workspace.
	ws2, err := store.GetOrCreateWorkspaceForRoot(ctx, "root-2", func() (string, error) {
		return "ws-other", nil
	})
	if err != nil {
		t.Fatalf("GetOrCreateWorkspaceForRoot(root-2) returned error: %v", err)
	}
	if ws2 != "ws-other" {
		t.Fatalf("workspace id (root-2) = %q, want ws-other", ws2)
	}

	// Validation: empty root and nil create are rejected.
	if _, err := store.GetOrCreateWorkspaceForRoot(ctx, "  ", create); err == nil {
		t.Fatal("GetOrCreateWorkspaceForRoot with empty root returned nil error")
	}
	if _, err := store.GetOrCreateWorkspaceForRoot(ctx, "root-3", nil); err == nil {
		t.Fatal("GetOrCreateWorkspaceForRoot with nil create returned nil error")
	}
}

// TestGetAndDeleteWorkspaceForRoot covers the registry lookup + delete the
// job-mode finalize relies on to close the per-root workspace once the pane rows
// are gone: a registered root reports found; an unknown/empty root reports a clean
// miss (never sql.ErrNoRows); delete drops the row and is idempotent.
func TestGetAndDeleteWorkspaceForRoot(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	// Unknown root is a clean miss, not an error.
	if ws, found, err := store.GetWorkspaceForRoot(ctx, "root-x"); err != nil || found || ws != "" {
		t.Fatalf("GetWorkspaceForRoot(unknown) = (%q,%v,%v), want (\"\",false,nil)", ws, found, err)
	}
	// An empty root id is also a clean miss.
	if _, found, err := store.GetWorkspaceForRoot(ctx, "  "); err != nil || found {
		t.Fatalf("GetWorkspaceForRoot(empty) = (found=%v,err=%v), want (false,nil)", found, err)
	}

	if _, err := store.GetOrCreateWorkspaceForRoot(ctx, "root-1", func() (string, error) {
		return "ws-1", nil
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}

	ws, found, err := store.GetWorkspaceForRoot(ctx, "root-1")
	if err != nil || !found || ws != "ws-1" {
		t.Fatalf("GetWorkspaceForRoot(root-1) = (%q,%v,%v), want (ws-1,true,nil)", ws, found, err)
	}

	// Delete drops the row; a subsequent lookup is a clean miss.
	if err := store.DeleteWorkspaceForRoot(ctx, "root-1"); err != nil {
		t.Fatalf("DeleteWorkspaceForRoot returned error: %v", err)
	}
	if _, found, err := store.GetWorkspaceForRoot(ctx, "root-1"); err != nil || found {
		t.Fatalf("GetWorkspaceForRoot after delete = (found=%v,err=%v), want (false,nil)", found, err)
	}
	// Idempotent: deleting a missing row is a no-op, not an error.
	if err := store.DeleteWorkspaceForRoot(ctx, "root-1"); err != nil {
		t.Fatalf("DeleteWorkspaceForRoot (repeat) returned error: %v", err)
	}
}

func TestGetOrCreateWorkspaceForRootConcurrent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	const goroutines = 16
	var calls atomic.Int64
	create := func() (string, error) {
		// Each create returns a distinct id so we can verify exactly one wins.
		n := calls.Add(1)
		return "ws-" + string(rune('a'+int(n-1))), nil
	}

	var wg sync.WaitGroup
	results := make([]string, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			ws, err := store.GetOrCreateWorkspaceForRoot(ctx, "root-race", create)
			if err != nil {
				t.Errorf("GetOrCreateWorkspaceForRoot(concurrent) returned error: %v", err)
				return
			}
			results[idx] = ws
		}(i)
	}
	close(start)
	wg.Wait()

	// All callers must observe the same single bound workspace id.
	winner := results[0]
	if winner == "" {
		t.Fatal("first concurrent caller got an empty workspace id")
	}
	for i, got := range results {
		if got != winner {
			t.Fatalf("concurrent caller %d got %q, want the single winner %q", i, got, winner)
		}
	}
	// create may run more than once under contention (it shells out before the
	// insert), but the bound id is stable and matches what every caller sees.
	final, err := store.GetOrCreateWorkspaceForRoot(ctx, "root-race", func() (string, error) {
		t.Fatal("create must not run once the root is bound")
		return "", nil
	})
	if err != nil {
		t.Fatalf("GetOrCreateWorkspaceForRoot(after race) returned error: %v", err)
	}
	if final != winner {
		t.Fatalf("post-race bound id = %q, want %q", final, winner)
	}
}
