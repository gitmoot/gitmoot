package feedback

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

func TestParseGitHubFeedbackComment(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantOK     bool
		wantRunID  string
		wantItems  int
		wantItem   string
		wantChoice string
		wantReason string
		wantErr    string
	}{
		{
			name:   "fenced yaml",
			body:   "Looks good.\n\n```yaml\nrun_id: run-1\nreviewer: jerry\nitems:\n  - item_id: item-001\n    choice: b\n    reasoning: More concrete.\n```\n",
			wantOK: true, wantRunID: "run-1", wantItems: 1, wantItem: "item-001", wantChoice: "b", wantReason: "More concrete.",
		},
		{
			name:   "short form",
			body:   "run_id: run-1\nitem-001: b - More concrete and easier to execute.\nitem-002: tie\n",
			wantOK: true, wantRunID: "run-1", wantItems: 2, wantItem: "item-001", wantChoice: "b", wantReason: "More concrete and easier to execute.",
		},
		{
			name:   "unrelated",
			body:   "Thanks, I will review this later.",
			wantOK: false,
		},
		{
			name:   "published packet",
			body:   githubFeedbackPacketMarker + "\n# Gitmoot SkillOpt Feedback: run-1\n\n```yaml\nrun_id: run-1\nitems:\n  - item_id: item-001\n    choice: \n```",
			wantOK: false,
		},
		{
			name:   "quoted short form",
			body:   "> run_id: run-1\n> item-001: b - quoted feedback\n\nI agree with this.",
			wantOK: false,
		},
		{
			name:       "raw short form choice",
			body:       "item-001: yes",
			wantOK:     true,
			wantItems:  1,
			wantItem:   "item-001",
			wantChoice: "yes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, ok, err := ParseGitHubFeedbackComment(tt.body)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGitHubFeedbackComment returned error: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if parsed.RunID != tt.wantRunID {
				t.Fatalf("run_id = %q, want %q", parsed.RunID, tt.wantRunID)
			}
			if len(parsed.Items) != tt.wantItems {
				t.Fatalf("items = %+v, want %d", parsed.Items, tt.wantItems)
			}
			if parsed.Items[0].ItemID != tt.wantItem || parsed.Items[0].Choice != tt.wantChoice || parsed.Items[0].Reasoning != tt.wantReason {
				t.Fatalf("first item = %+v", parsed.Items[0])
			}
		})
	}
}

func TestGitHubCollectorPublishesIssueBody(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubFeedbackRun(t, "run-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		issue: github.Issue{Number: 42, URL: "https://github.com/owner/repo/issues/42"},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	result, err := collector.Publish(ctx, store, "run-1", GitHubPublishTarget{
		Repo: github.Repository{Owner: "owner", Name: "repo"},
	})
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if result.Mode != "issue" || result.IssueNumber != 42 || result.URL != fake.issue.URL {
		t.Fatalf("result = %+v", result)
	}
	if fake.createdIssue.Title != "Gitmoot SkillOpt feedback: run-1" {
		t.Fatalf("created issue = %+v", fake.createdIssue)
	}
	body := fake.createdIssue.Body
	for _, want := range []string{
		"Allowed choices: `a`, `b`, `tie`, `neither`, `skip`",
		"run_id: run-1",
		"item-001: <choice> - optional reason",
		"#### Option A",
		"#### Option B",
		"baseline answer",
		"candidate answer",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("issue body missing %q:\n%s", want, body)
		}
	}
}

func TestGitHubCollectorYAMLTemplateHandlesSpecialIDs(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubFeedbackRunWithItem(t, "run: 1 # x", "- item: 001", "owner/repo")
	collector := GitHubCollector{BlobStore: blobs, GitHub: &fakeFeedbackGitHub{}}

	body, err := collector.Body(ctx, store, "run: 1 # x")
	if err != nil {
		t.Fatalf("Body returned error: %v", err)
	}
	blocks := fencedBlocks(body)
	if len(blocks) == 0 {
		t.Fatalf("body has no yaml block:\n%s", body)
	}
	parsed, ok, err := parseFullYAMLFeedback(blocks[0])
	if err != nil {
		t.Fatalf("parseFullYAMLFeedback returned error: %v\n%s", err, blocks[0])
	}
	if !ok {
		t.Fatalf("yaml block was not parsed as feedback:\n%s", blocks[0])
	}
	if parsed.RunID != "run: 1 # x" || len(parsed.Items) != 1 || parsed.Items[0].ItemID != "- item: 001" {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestGitHubCollectorPublishesPRComment(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubFeedbackRun(t, "run-1", "owner/repo")
	fake := &fakeFeedbackGitHub{}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	result, err := collector.Publish(ctx, store, "run-1", GitHubPublishTarget{
		Repo:        github.Repository{Owner: "owner", Name: "repo"},
		PullRequest: 7,
	})
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if result.Mode != "pr-comment" || result.IssueNumber != 7 {
		t.Fatalf("result = %+v", result)
	}
	if fake.createdIssue.Title != "" {
		t.Fatalf("CreateIssue was called in PR mode: %+v", fake.createdIssue)
	}
	if len(fake.postedComments) != 1 || fake.postedComments[0].IssueNumber != 7 {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if !strings.Contains(fake.postedComments[0].Body, "Short-Form Reply") {
		t.Fatalf("PR comment body missing feedback instructions:\n%s", fake.postedComments[0].Body)
	}
}

func TestGitHubCollectorSyncImportsValidCommentsAndIgnoresUnrelated(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubFeedbackRun(t, "run-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "LGTM", URL: "https://github.com/owner/repo/issues/42#issuecomment-1", Author: "alice", CreatedAt: "2026-05-31T10:00:00Z"},
				{ID: 4, Body: "status: done", URL: "https://github.com/owner/repo/issues/42#issuecomment-4", Author: "alice", CreatedAt: "2026-05-31T10:30:00Z"},
				{ID: 5, Body: "```yaml\nitems:\n  - name: docs\n```", URL: "https://github.com/owner/repo/issues/42#issuecomment-5", Author: "alice", CreatedAt: "2026-05-31T10:45:00Z"},
				{ID: 6, Body: "item-001: b - stale unscoped reply", URL: "https://github.com/owner/repo/issues/42#issuecomment-6", Author: "dan", CreatedAt: "2026-05-31T10:50:00Z"},
				{ID: 7, Body: "```yaml\nitems:\n  - item_id: item-001\n    choice: b\n```", URL: "https://github.com/owner/repo/issues/42#issuecomment-7", Author: "erin", CreatedAt: "2026-05-31T10:55:00Z"},
				{ID: 2, Body: "run_id: run-1\nitem-001: b - More concrete.", URL: "https://github.com/owner/repo/issues/42#issuecomment-2", Author: "bob", CreatedAt: "2026-05-31T11:00:00Z"},
				{ID: 3, Body: "```yaml\nrun_id: other-run\nitems:\n  - item_id: item-001\n    choice: a\n```", URL: "https://github.com/owner/repo/issues/42#issuecomment-3", Author: "carol", CreatedAt: "2026-05-31T12:00:00Z"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake, Now: func() time.Time {
		return time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	}}

	events, err := collector.Sync(ctx, store, "run-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err != nil {
		t.Fatalf("Sync returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1 imported event", events)
	}
	if events[0].RunID != "run-1" || events[0].ItemID != "item-001" || events[0].Reviewer != "bob" || events[0].Source != SourceGitHub || events[0].SourceURL != "https://github.com/owner/repo/issues/42#issuecomment-2" {
		t.Fatalf("event = %+v", events[0])
	}
	stored, err := store.ListFeedbackEvents(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored events = %+v", stored)
	}
	if _, err := collector.Sync(ctx, store, "run-1", github.Repository{Owner: "owner", Name: "repo"}, 42); err != nil {
		t.Fatalf("second Sync returned error: %v", err)
	}
	stored, err = store.ListFeedbackEvents(ctx, "run-1")
	if err != nil {
		t.Fatalf("second ListFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("duplicate sync persisted duplicate events: %+v", stored)
	}
}

func TestGitHubCollectorSyncRejectsInvalidChoiceForKnownShortFormItem(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubFeedbackRun(t, "run-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "status: done", URL: "https://github.com/owner/repo/issues/42#issuecomment-1", Author: "alice", CreatedAt: "2026-05-31T10:00:00Z"},
				{ID: 2, Body: "run_id: run-1\nitem-001: yes", URL: "https://github.com/owner/repo/issues/42#issuecomment-2", Author: "bob", CreatedAt: "2026-05-31T11:00:00Z"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	_, err := collector.Sync(ctx, store, "run-1", github.Repository{Owner: "owner", Name: "repo"}, 42)

	if err == nil || !strings.Contains(err.Error(), `comment 2 item item-001: invalid feedback choice "yes"`) {
		t.Fatalf("Sync error = %v", err)
	}
	stored, err := store.ListFeedbackEvents(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("Sync persisted partial events after invalid known item choice: %+v", stored)
	}
}

func setupGitHubFeedbackRun(t *testing.T, runID string, targetRepo string) (*db.Store, artifact.Store) {
	return setupGitHubFeedbackRunWithItem(t, runID, "item-001", targetRepo)
}

func setupGitHubFeedbackRunWithItem(t *testing.T, runID string, itemID string, targetRepo string) (*db.Store, artifact.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	blobStore := artifact.NewStore(filepath.Join(t.TempDir(), "blobs"))
	baseline, err := blobStore.Put([]byte("baseline answer"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidate, err := blobStore.Put([]byte("candidate answer"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "baseline", Hash: baseline.Hash, MediaType: "text/markdown", SizeBytes: baseline.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "candidate", Hash: candidate.Hash, MediaType: "text/markdown", SizeBytes: candidate.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{ID: runID, TemplateID: "planner", TargetRepo: targetRepo, State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:               runID,
		ItemID:              itemID,
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	return store, blobStore
}

type fakeFeedbackGitHub struct {
	github.NoopClient

	issue          github.Issue
	createdIssue   github.CreateIssueInput
	postedComments []postedFeedbackComment
	comments       map[int64][]github.IssueComment
}

type postedFeedbackComment struct {
	IssueNumber int64
	Body        string
}

func (f *fakeFeedbackGitHub) CreateIssue(_ context.Context, input github.CreateIssueInput) (github.Issue, error) {
	f.createdIssue = input
	if f.issue.Number == 0 {
		f.issue = github.Issue{Number: 1, URL: "https://github.com/" + input.Repo.FullName() + "/issues/1"}
	}
	return f.issue, nil
}

func (f *fakeFeedbackGitHub) PostIssueComment(_ context.Context, repo github.Repository, issueNumber int64, body string) (github.IssueComment, error) {
	f.postedComments = append(f.postedComments, postedFeedbackComment{IssueNumber: issueNumber, Body: body})
	return github.IssueComment{ID: int64(len(f.postedComments)), Body: body, URL: "https://github.com/" + repo.FullName() + "/issues/" + "7#issuecomment-1"}, nil
}

func (f *fakeFeedbackGitHub) ListIssueComments(_ context.Context, _ github.Repository, issueNumber int64) ([]github.IssueComment, error) {
	return append([]github.IssueComment(nil), f.comments[issueNumber]...), nil
}
