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
			name:   "ranked short form",
			body:   "run_id: ranked-1\nitem-001 ranking: C > A > D > B\nbest traits:\n- C: clearest product explanation\nreject:\n- B: too generic\n",
			wantOK: true, wantRunID: "ranked-1", wantItems: 1, wantItem: "item-001",
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
			if tt.name == "ranked short form" {
				if strings.Join(parsed.Items[0].Ranking, ",") != "C,A,D,B" ||
					!strings.Contains(strings.Join(parsed.Items[0].UsefulTraits["C"], ","), "clearest product explanation") ||
					!strings.Contains(strings.Join(parsed.Items[0].RejectedTraits["B"], ","), "too generic") {
					t.Fatalf("ranked first item = %+v", parsed.Items[0])
				}
			}
		})
	}
}

func TestParseGitHubFeedbackCommentScopesRankedTraitsToItem(t *testing.T) {
	body := "run_id: ranked-1\n" +
		"item-001 ranking: C > A > D > B\n" +
		"best traits:\n" +
		"- C: clearest product explanation\n" +
		"item-002 ranking: A > B > C > D\n" +
		"reject:\n" +
		"- D: too generic\n"
	parsed, ok, err := ParseGitHubFeedbackComment(body)
	if err != nil {
		t.Fatalf("ParseGitHubFeedbackComment returned error: %v", err)
	}
	if !ok || len(parsed.Items) != 2 {
		t.Fatalf("parsed ok=%t items=%+v", ok, parsed.Items)
	}
	if !strings.Contains(strings.Join(parsed.Items[0].UsefulTraits["C"], ","), "clearest product explanation") {
		t.Fatalf("first item traits = %+v", parsed.Items[0])
	}
	if len(parsed.Items[0].RejectedTraits) != 0 {
		t.Fatalf("first item received second item rejected traits: %+v", parsed.Items[0])
	}
	if !strings.Contains(strings.Join(parsed.Items[1].RejectedTraits["D"], ","), "too generic") {
		t.Fatalf("second item traits = %+v", parsed.Items[1])
	}
	if len(parsed.Items[1].UsefulTraits) != 0 {
		t.Fatalf("second item received first item useful traits: %+v", parsed.Items[1])
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
		"Reply by copying the fenced `yaml` block below",
		"Allowed choices: `a`, `b`, `tie`, `neither`, `skip`",
		"## Phase Recommendation",
		"recommend continue validate",
		"run_id: run-1",
		"choice:",
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

func TestGitHubCollectorPublishesRankedIssueBody(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubRankedFeedbackRun(t, "ranked-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		issue: github.Issue{Number: 42, URL: "https://github.com/owner/repo/issues/42"},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	result, err := collector.Publish(ctx, store, "ranked-1", GitHubPublishTarget{
		Repo: github.Repository{Owner: "owner", Name: "repo"},
	})
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if result.Mode != "issue" || result.IssueNumber != 42 {
		t.Fatalf("result = %+v", result)
	}
	body := fake.createdIssue.Body
	for _, want := range []string{
		"Rank every option",
		"Reply by copying the fenced `yaml` block below",
		"Valid `quality` values: `poor`, `acceptable`, `strong`",
		"Valid `continue_mode` values: `explore`, `refine`, `distill`, `validate`",
		"Valid `promote` values: `yes`, `no`",
		"## Review Table",
		"## Phase Recommendation",
		"recommend continue explore",
		"| Item | What to compare | Options |",
		"Option C: [open](https://example.com/c)",
		"## Inline Options Without Public Links",
		"#### Option A",
		"option a answer",
		"```yaml",
		"run_id: ranked-1",
		"item_id: item-001",
		"<replace with ranked option labels, e.g. A > B > C > D>",
		"quality: \"\"",
		"continue_mode: \"\"",
		"promote: \"\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("ranked issue body missing %q:\n%s", want, body)
		}
	}
	for _, leaked := range []string{"/tmp/gitmoot-option-a.md", "#### Option C", "option c answer"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("ranked issue body leaked %q:\n%s", leaked, body)
		}
	}
}

func TestGitHubCollectorRejectsPreviewBundleWithoutURL(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubRankedFeedbackRun(t, "ranked-1", "owner/repo")
	bundle, err := blobs.Put([]byte(`{"renderer":"vue-vite"}`))
	if err != nil {
		t.Fatalf("Put preview bundle returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: "option-a", Hash: bundle.Hash, MediaType: "application/json", SizeBytes: bundle.Size, Driver: "vue-vite"}); err != nil {
		t.Fatalf("UpsertEvalArtifact option-a returned error: %v", err)
	}
	if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{
		RunID:        "ranked-1",
		ItemID:       "item-001",
		Label:        "a",
		ArtifactID:   "option-a",
		Role:         "option",
		MetadataJSON: `{"preview_bundle":{"renderer":"vue-vite","files":["package.json"]}}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewOption option-a returned error: %v", err)
	}
	fake := &fakeFeedbackGitHub{
		issue: github.Issue{Number: 42, URL: "https://github.com/owner/repo/issues/42"},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	_, err = collector.Publish(ctx, store, "ranked-1", GitHubPublishTarget{
		Repo: github.Repository{Owner: "owner", Name: "repo"},
	})
	if err == nil || !strings.Contains(err.Error(), "preview_url") {
		t.Fatalf("Publish error = %v, want preview_url", err)
	}
	if fake.createdIssue.Body != "" {
		t.Fatalf("Publish created issue body despite missing preview URL:\n%s", fake.createdIssue.Body)
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

	result, err := collector.Sync(ctx, store, "run-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err != nil {
		t.Fatalf("Sync returned error: %v", err)
	}
	events := result.FeedbackEvents
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1 imported event", events)
	}
	if len(result.Diagnostics) == 0 {
		t.Fatalf("diagnostics = %+v, want ignored-comment diagnostics", result.Diagnostics)
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

func TestGitHubCollectorSyncReportsNoComments(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubFeedbackRun(t, "run-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{42: nil},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	_, err := collector.Sync(ctx, store, "run-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err == nil || !strings.Contains(err.Error(), "no comments found") {
		t.Fatalf("Sync err = %v, want no comments diagnostic", err)
	}
}

func TestGitHubCollectorSyncReportsWrongRunIDWhenNoValidFeedback(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubFeedbackRun(t, "run-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "```yaml\nrun_id: other-run\nitems:\n  - item_id: item-001\n    choice: b\n```", Author: "alice"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	_, err := collector.Sync(ctx, store, "run-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err == nil || !strings.Contains(err.Error(), `wrong run_id "other-run", want "run-1"`) {
		t.Fatalf("Sync err = %v, want wrong run_id diagnostic", err)
	}
}

func TestGitHubCollectorSyncReportsMissingItemFeedback(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubFeedbackRun(t, "run-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "```yaml\nrun_id: run-1\nitems:\n  - choice: b\n```", Author: "alice"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	_, err := collector.Sync(ctx, store, "run-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err == nil || !strings.Contains(err.Error(), `missing item feedback for run "run-1"`) {
		t.Fatalf("Sync err = %v, want missing item feedback diagnostic", err)
	}
}

func TestGitHubCollectorSyncImportsRankedShortFormComment(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubRankedFeedbackRun(t, "ranked-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "run_id: ranked-1\nitem-001 ranking: C > A > D > B\nbest traits:\n- C: clearest product explanation\n- A: best visual style\nreject:\n- B: too generic\n", URL: "https://github.com/owner/repo/issues/42#issuecomment-1", Author: "alice", CreatedAt: "2026-06-02T10:00:00Z"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	result, err := collector.Sync(ctx, store, "ranked-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err != nil {
		t.Fatalf("Sync returned error: %v", err)
	}
	if result.Count() != 1 || len(result.RankedFeedbackEvents) != 1 {
		t.Fatalf("result = %+v", result)
	}
	stored, err := store.ListRankedFeedbackEvents(ctx, "ranked-1")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 1 || stored[0].Winner != "c" || stored[0].Reviewer != "alice" || stored[0].Source != SourceGitHub {
		t.Fatalf("stored ranked feedback = %+v", stored)
	}
	if !strings.Contains(stored[0].UsefulTraitsJSON, `"c":["clearest product explanation"]`) || !strings.Contains(stored[0].RejectedTraitsJSON, `"b":["too generic"]`) {
		t.Fatalf("stored traits useful=%s rejected=%s", stored[0].UsefulTraitsJSON, stored[0].RejectedTraitsJSON)
	}
	pairs, err := store.ListPairwisePreferences(ctx, "ranked-1")
	if err != nil {
		t.Fatalf("ListPairwisePreferences returned error: %v", err)
	}
	if len(pairs) != 6 || pairs[0].Preferred != "c" || pairs[0].Rejected != "a" {
		t.Fatalf("pairwise preferences = %+v", pairs)
	}
}

func TestGitHubCollectorSyncImportsRankedYAMLCommentWithColonItemID(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubRankedFeedbackRunWithItem(t, "ranked-1", "scenario:landing", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "```yaml\nrun_id: ranked-1\nitems:\n  - item_id: scenario:landing\n    ranking:\n      - C > A > D > B\n    quality: poor\n    continue_mode: explore\n    promote: no\n    choice: option c is strongest, but improve mobile and visuals\n    required_improvements:\n      - better mobile layout\n      - stronger product visuals\n```\n", URL: "https://github.com/owner/repo/issues/42#issuecomment-1", Author: "alice", CreatedAt: "2026-06-02T10:00:00Z"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	result, err := collector.Sync(ctx, store, "ranked-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err != nil {
		t.Fatalf("Sync returned error: %v", err)
	}
	if result.Count() != 1 || len(result.RankedFeedbackEvents) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.RankedFeedbackEvents[0].ItemID != "scenario:landing" || result.RankedFeedbackEvents[0].Winner != "c" {
		t.Fatalf("ranked event = %+v", result.RankedFeedbackEvents[0])
	}
	if result.RankedFeedbackEvents[0].Quality != "poor" || result.RankedFeedbackEvents[0].ContinueMode != "explore" || result.RankedFeedbackEvents[0].Promote != "no" {
		t.Fatalf("ranked event signals = %+v", result.RankedFeedbackEvents[0])
	}
	if !strings.Contains(result.RankedFeedbackEvents[0].Reasoning, "improve mobile") || !strings.Contains(result.RankedFeedbackEvents[0].RequiredImprovementsJSON, "stronger product visuals") {
		t.Fatalf("ranked event reasoning/improvements = %+v", result.RankedFeedbackEvents[0])
	}
}

func TestGitHubCollectorSyncImportsRankedDirectShortFormWithSignals(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubRankedFeedbackRun(t, "ranked-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "run_id: ranked-1\nitem-001: C > A > D > B - all options are still weak\nitem-001 quality: poor\nitem-001 continue_mode: explore\nitem-001 promote: no\n", URL: "https://github.com/owner/repo/issues/42#issuecomment-1", Author: "alice", CreatedAt: "2026-06-02T10:00:00Z"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	result, err := collector.Sync(ctx, store, "ranked-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err != nil {
		t.Fatalf("Sync returned error: %v", err)
	}
	if result.Count() != 1 || len(result.RankedFeedbackEvents) != 1 {
		t.Fatalf("result = %+v", result)
	}
	event := result.RankedFeedbackEvents[0]
	if event.Winner != "c" || event.Quality != "poor" || event.ContinueMode != "explore" || event.Promote != "no" || !strings.Contains(event.Reasoning, "weak") {
		t.Fatalf("ranked event = %+v", event)
	}
}

func TestGitHubCollectorSyncRejectsInvalidRankedSignalWithoutPartialImport(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubRankedFeedbackRun(t, "ranked-1", "owner/repo")
	if err := addGitHubRankedFeedbackItem(ctx, store, blobs, "ranked-1", "item-002"); err != nil {
		t.Fatalf("addGitHubRankedFeedbackItem returned error: %v", err)
	}
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "```yaml\nrun_id: ranked-1\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n    quality: poor\n  - item_id: item-002\n    ranking:\n      - C > A > D > B\n    quality: ok\n```\n", URL: "https://github.com/owner/repo/issues/42#issuecomment-1", Author: "alice", CreatedAt: "2026-06-02T10:00:00Z"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	_, err := collector.Sync(ctx, store, "ranked-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err == nil || !strings.Contains(err.Error(), "quality") {
		t.Fatalf("Sync error = %v", err)
	}
	stored, err := store.ListRankedFeedbackEvents(ctx, "ranked-1")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("Sync persisted partial ranked events: %+v", stored)
	}
}

func TestGitHubCollectorSyncRejectsUnchangedRankedTemplate(t *testing.T) {
	ctx := context.Background()
	store, blobs := setupGitHubRankedFeedbackRun(t, "ranked-1", "owner/repo")
	fake := &fakeFeedbackGitHub{
		comments: map[int64][]github.IssueComment{
			42: {
				{ID: 1, Body: "```yaml\nrun_id: ranked-1\nitems:\n  - item_id: item-001\n    ranking:\n      - <replace with ranked option labels, e.g. A > B > C > D>\n```\n", URL: "https://github.com/owner/repo/issues/42#issuecomment-1", Author: "alice", CreatedAt: "2026-06-02T10:00:00Z"},
			},
		},
	}
	collector := GitHubCollector{BlobStore: blobs, GitHub: fake}

	_, err := collector.Sync(ctx, store, "ranked-1", github.Repository{Owner: "owner", Name: "repo"}, 42)
	if err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("Sync error = %v", err)
	}
	stored, err := store.ListRankedFeedbackEvents(ctx, "ranked-1")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("Sync persisted unchanged template feedback: %+v", stored)
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

func setupGitHubRankedFeedbackRun(t *testing.T, runID string, targetRepo string) (*db.Store, artifact.Store) {
	return setupGitHubRankedFeedbackRunWithItem(t, runID, "item-001", targetRepo)
}

func setupGitHubRankedFeedbackRunWithItem(t *testing.T, runID string, itemID string, targetRepo string) (*db.Store, artifact.Store) {
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
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:           runID,
		TemplateID:   "planner",
		TargetRepo:   targetRepo,
		State:        "review",
		Mode:         db.EvalRunModeExplore,
		OptionsCount: 4,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:  runID,
		ItemID: itemID,
		Title:  "Ranked Item",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		content := []byte("option " + label + " answer")
		blob, err := blobStore.Put(content)
		if err != nil {
			t.Fatalf("Put option %s returned error: %v", label, err)
		}
		artifactID := "option-" + label
		if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: artifactID, Hash: blob.Hash, MediaType: "text/markdown", SizeBytes: blob.Size, Driver: "text"}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", label, err)
		}
		metadata := `{"path":"/tmp/gitmoot-option-` + label + `.md"}`
		if label == "c" {
			metadata = `{"preview_url":"https://example.com/c","preview_bundle":{"renderer":"vue-vite","files":["package.json"]}}`
		}
		if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{RunID: runID, ItemID: itemID, Label: label, ArtifactID: artifactID, Role: "option", MetadataJSON: metadata}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	return store, blobStore
}

func addGitHubRankedFeedbackItem(ctx context.Context, store *db.Store, blobStore artifact.Store, runID string, itemID string) error {
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{RunID: runID, ItemID: itemID, Title: "Ranked Item"}); err != nil {
		return err
	}
	for _, label := range []string{"a", "b", "c", "d"} {
		content := []byte("option " + label + " answer")
		blob, err := blobStore.Put(content)
		if err != nil {
			return err
		}
		artifactID := itemID + "-option-" + label
		if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{ID: artifactID, Hash: blob.Hash, MediaType: "text/markdown", SizeBytes: blob.Size, Driver: "text"}); err != nil {
			return err
		}
		if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{RunID: runID, ItemID: itemID, Label: label, ArtifactID: artifactID, Role: "option"}); err != nil {
			return err
		}
	}
	return nil
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
