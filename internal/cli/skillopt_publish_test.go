package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func TestSkillOptGitHubPagesURLHandlesProjectAndUserPages(t *testing.T) {
	project := githubPagesURL(db.Repo{Owner: "owner", Name: "previews"}, "runs/run-1/item/a/")
	if project != "https://owner.github.io/previews/runs/run-1/item/a/" {
		t.Fatalf("project pages URL = %q", project)
	}
	user := githubPagesURL(db.Repo{Owner: "owner", Name: "owner.github.io"}, "runs/run-1/item/a/")
	if user != "https://owner.github.io/runs/run-1/item/a/" {
		t.Fatalf("user pages URL = %q", user)
	}
}

func TestSkillOptPreviewRouteSlugsUnsafeSegments(t *testing.T) {
	route, err := skillOptPreviewRoute("", "run 1", "hero#main", "A/B?")
	if err != nil {
		t.Fatalf("skillOptPreviewRoute returned error: %v", err)
	}
	want := "runs/run-1-" + shortHash("run 1") + "/hero-main-" + shortHash("hero#main") + "/a-b-" + shortHash("A/B?") + "/"
	if route != want {
		t.Fatalf("route = %q, want %q", route, want)
	}
}

func TestTrustedVueViteScaffoldUsesRelativeBase(t *testing.T) {
	workDir := t.TempDir()
	if err := writeTrustedVueViteScaffold(workDir); err != nil {
		t.Fatalf("writeTrustedVueViteScaffold returned error: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(workDir, "vite.config.js"))
	if err != nil {
		t.Fatalf("read vite config: %v", err)
	}
	if !strings.Contains(string(config), "base: './'") {
		t.Fatalf("vite config missing relative base:\n%s", string(config))
	}
}

func TestPublishGitHubPagesPreviewRestoresCheckoutOnCommitFailure(t *testing.T) {
	previewDir := t.TempDir()
	runGit(t, previewDir, "init")
	runGit(t, previewDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, previewDir, "config", "user.name", "Gitmoot")
	runGit(t, previewDir, "branch", "-m", "main")
	if err := os.WriteFile(filepath.Join(previewDir, "README.md"), []byte("previews\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, previewDir, "add", "README.md")
	runGit(t, previewDir, "commit", "-m", "init")
	distDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte("<main>preview</main>\n"), 0o644); err != nil {
		t.Fatalf("write dist index: %v", err)
	}
	previewRunner := &skillOptTrainFakePreviewRunner{failGitCommit: true}
	oldPreviewRunner := skillOptTrainPreviewRunner
	skillOptTrainPreviewRunner = previewRunner
	defer func() {
		skillOptTrainPreviewRunner = oldPreviewRunner
	}()

	_, err := publishGitHubPagesPreview(context.Background(), db.Repo{Owner: "owner", Name: "previews", CheckoutPath: previewDir}, "runs/run-1/item/a/", distDir)
	if err == nil || !strings.Contains(err.Error(), "git commit") {
		t.Fatalf("publishGitHubPagesPreview error = %v, want git commit", err)
	}
	status := runGitOutput(t, previewDir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Fatalf("preview repo left dirty after commit failure:\n%s", status)
	}
	if _, err := os.Stat(filepath.Join(previewDir, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview route was not cleaned, stat err=%v", err)
	}
}

func TestPublishGitHubPagesPreviewReportsPagesStatus(t *testing.T) {
	for _, tt := range []struct {
		name       string
		pages      string
		pagesError string
		wantStatus string
		wantReason string
	}{
		{name: "ready", pages: "built", wantStatus: "ready"},
		{name: "pending", pages: "queued", wantStatus: "pending"},
		{name: "failed", pages: "errored", pagesError: "Pages build failed", wantStatus: "failed", wantReason: "Pages build failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			previewDir := t.TempDir()
			runGit(t, previewDir, "init")
			runGit(t, previewDir, "config", "user.email", "gitmoot@example.com")
			runGit(t, previewDir, "config", "user.name", "Gitmoot")
			runGit(t, previewDir, "branch", "-m", "main")
			if err := os.WriteFile(filepath.Join(previewDir, "README.md"), []byte("previews\n"), 0o644); err != nil {
				t.Fatalf("write README: %v", err)
			}
			runGit(t, previewDir, "add", "README.md")
			runGit(t, previewDir, "commit", "-m", "init")
			distDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte("<main>preview</main>\n"), 0o644); err != nil {
				t.Fatalf("write dist index: %v", err)
			}
			previewRunner := &skillOptTrainFakePreviewRunner{pagesStatus: tt.pages, pagesError: tt.pagesError}
			oldPreviewRunner := skillOptTrainPreviewRunner
			skillOptTrainPreviewRunner = previewRunner
			defer func() {
				skillOptTrainPreviewRunner = oldPreviewRunner
			}()

			result, err := publishGitHubPagesPreview(context.Background(), db.Repo{Owner: "owner", Name: "previews", CheckoutPath: previewDir}, "runs/run-1/item/a/", distDir)
			if err != nil {
				t.Fatalf("publishGitHubPagesPreview returned error: %v", err)
			}
			if result.PagesStatus != tt.wantStatus || !strings.Contains(result.StatusReason, tt.wantReason) {
				t.Fatalf("publication = %+v, want status=%s reason=%q", result, tt.wantStatus, tt.wantReason)
			}
			if result.CommitSHA == "" || result.URL != "https://owner.github.io/previews/runs/run-1/item/a/" {
				t.Fatalf("publication missing commit/url: %+v", result)
			}
		})
	}
}

func TestObserveGitHubPagesBuildStatusWaitsForPublishedCommit(t *testing.T) {
	previewRunner := &skillOptTrainFakePreviewRunner{
		pagesStatus:  "built",
		pagesCommits: []string{"old-commit", "new-commit"},
	}
	oldPreviewRunner := skillOptTrainPreviewRunner
	skillOptTrainPreviewRunner = previewRunner
	defer func() {
		skillOptTrainPreviewRunner = oldPreviewRunner
	}()

	status, reason := observeGitHubPagesBuildStatusWithPoll(
		context.Background(),
		db.Repo{Owner: "owner", Name: "previews", CheckoutPath: t.TempDir()},
		"new-commit",
		50*time.Millisecond,
		time.Millisecond,
	)
	if status != "ready" || reason != "" {
		t.Fatalf("status=%q reason=%q, want ready with no reason", status, reason)
	}
	ghCalls := 0
	for _, call := range previewRunner.calls {
		if call.command == "gh" {
			ghCalls++
		}
	}
	if ghCalls < 2 {
		t.Fatalf("gh api calls = %d, want at least 2", ghCalls)
	}
}

func TestObserveGitHubPagesBuildStatusPollsMatchingPendingBuild(t *testing.T) {
	previewRunner := &skillOptTrainFakePreviewRunner{
		pagesStatuses: []string{"queued", "building", "built"},
		pagesCommits:  []string{"new-commit"},
	}
	oldPreviewRunner := skillOptTrainPreviewRunner
	skillOptTrainPreviewRunner = previewRunner
	defer func() {
		skillOptTrainPreviewRunner = oldPreviewRunner
	}()

	status, reason := observeGitHubPagesBuildStatusWithPoll(
		context.Background(),
		db.Repo{Owner: "owner", Name: "previews", CheckoutPath: t.TempDir()},
		"new-commit",
		50*time.Millisecond,
		time.Millisecond,
	)
	if status != "ready" || reason != "" {
		t.Fatalf("status=%q reason=%q, want ready with no reason", status, reason)
	}
	ghCalls := 0
	for _, call := range previewRunner.calls {
		if call.command == "gh" {
			ghCalls++
		}
	}
	if ghCalls < 3 {
		t.Fatalf("gh api calls = %d, want at least 3", ghCalls)
	}
}

func TestObserveGitHubPagesBuildStatusMarksStaleAfterTimeout(t *testing.T) {
	previewRunner := &skillOptTrainFakePreviewRunner{
		pagesStatus:  "built",
		pagesCommits: []string{"old-commit"},
	}
	oldPreviewRunner := skillOptTrainPreviewRunner
	skillOptTrainPreviewRunner = previewRunner
	defer func() {
		skillOptTrainPreviewRunner = oldPreviewRunner
	}()

	status, reason := observeGitHubPagesBuildStatusWithPoll(
		context.Background(),
		db.Repo{Owner: "owner", Name: "previews", CheckoutPath: t.TempDir()},
		"new-commit",
		0,
		time.Millisecond,
	)
	if status != "stale" || !strings.Contains(reason, "old-commit") || !strings.Contains(reason, "new-commit") {
		t.Fatalf("status=%q reason=%q, want stale reason with commit mismatch", status, reason)
	}
}

func TestSkillOptTrainContinueMarksReviewPublishedFeedbackSynced(t *testing.T) {
	home, _ := seedSkillOptTrainFeedbackSynced(t)
	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: feedback_synced",
		"blocked_step: training_package_created",
		"feedback: 1",
		"continue_ready: true",
		"feedback_events: 1",
		"pairwise_preferences: 0",
		"next: export the training package before running the optimizer",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue stdout missing %q:\n%s", want, stdout.String())
		}
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	session, err = store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession after continue returned error: %v", err)
	}
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after continue returned error: %v", err)
	}
	if session.State != skillopt.TrainStateFeedbackSynced || iteration.State != skillopt.TrainStateFeedbackSynced {
		t.Fatalf("states = session %s iteration %s, want feedback_synced", session.State, iteration.State)
	}
	if !strings.Contains(iteration.MetadataJSON, `"feedback_sync"`) {
		t.Fatalf("iteration metadata missing feedback_sync: %s", iteration.MetadataJSON)
	}
}
