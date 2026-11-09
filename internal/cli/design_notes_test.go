package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/github"
)

type fakeDesignNotesGitHub struct {
	pulls    map[int64]github.PullRequest
	gets     int
	searches []string
	found    []github.Issue
	created  []github.CreateIssueInput
	comments map[int64][]string
}

func (f *fakeDesignNotesGitHub) GetPullRequest(_ context.Context, _ github.Repository, number int64) (github.PullRequest, error) {
	f.gets++
	return f.pulls[number], nil
}

func (f *fakeDesignNotesGitHub) SearchIssuesByTitle(_ context.Context, _ github.Repository, title, label string) ([]github.Issue, error) {
	f.searches = append(f.searches, label+" "+title)
	return f.found, nil
}

func (f *fakeDesignNotesGitHub) CreateIssue(_ context.Context, input github.CreateIssueInput) (github.Issue, error) {
	f.created = append(f.created, input)
	return github.Issue{Number: 900, URL: "https://github.com/owner/repo/issues/900", Title: input.Title}, nil
}

func (f *fakeDesignNotesGitHub) PostIssueComment(_ context.Context, _ github.Repository, number int64, body string) (github.IssueComment, error) {
	if f.comments == nil {
		f.comments = map[int64][]string{}
	}
	f.comments[number] = append(f.comments[number], body)
	return github.IssueComment{Body: body}, nil
}

func openDesignNotesStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func recordCLIDesignNote(t *testing.T, store *db.Store, pr int64, at time.Time) {
	t.Helper()
	if _, err := store.RecordReviewFindingObservation(context.Background(), db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: pr, HeadSHA: strings.Repeat("a", 40), State: db.FindingOpen, Severity: "P3",
		Title: "name the seam", File: "x.go", Line: 3, EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1, ObservedAt: at.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation: %v", err)
	}
}

func TestRunDesignNotesSweepOnceRunsHourlyThroughTheGitHubClient(t *testing.T) {
	ctx := context.Background()
	store := openDesignNotesStore(t)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	fake := &fakeDesignNotesGitHub{pulls: map[int64]github.PullRequest{
		// The list endpoint's shape: merged only through merged_at.
		7: {Number: 7, State: "closed", MergedAt: "2026-11-09T10:00:00Z", HeadSHA: strings.Repeat("c", 40)},
		8: {Number: 8, State: "closed"},
		9: {Number: 9, State: "open"},
	}}
	previous := newDesignNotesGitHubClient
	newDesignNotesGitHubClient = func() designNotesGitHubClient { return fake }
	t.Cleanup(func() { newDesignNotesGitHubClient = previous })
	for _, pr := range []int64{7, 8, 9} {
		recordCLIDesignNote(t, store, pr, now.Add(-time.Hour))
	}

	state := &designNotesSweepState{}
	var out bytes.Buffer
	if err := runDesignNotesSweepOnce(ctx, store, now, state, &out); err != nil {
		t.Fatalf("runDesignNotesSweepOnce: %v", err)
	}
	if fake.gets != 3 || len(fake.created) != 1 || len(fake.comments[900]) != 1 {
		t.Fatalf("gets %d, created %d, comments %v", fake.gets, len(fake.created), fake.comments)
	}
	if got := fake.searches; len(got) != 1 || got[0] != "design-notes [design-notes] owner/repo 2026-W46" {
		t.Fatalf("searches = %q", got)
	}
	if created := fake.created[0]; created.Title != "[design-notes] owner/repo 2026-W46" ||
		strings.Join(created.Labels, ",") != "design-notes,agent:unassigned" {
		t.Fatalf("created = %+v", created)
	}
	if body := fake.comments[900][0]; !strings.Contains(body, "from #7,") || !strings.Contains(body, strings.Repeat("c", 40)) {
		t.Fatalf("comment:\n%s", body)
	}
	if !strings.Contains(out.String(), "opened https://github.com/owner/repo/issues/900") {
		t.Fatalf("log = %q", out.String())
	}

	// Within the hour nothing touches GitHub.
	if err := runDesignNotesSweepOnce(ctx, store, now.Add(59*time.Minute), state, &out); err != nil {
		t.Fatalf("throttled run: %v", err)
	}
	if fake.gets != 3 {
		t.Fatalf("a sweep inside the hour made %d GitHub reads", fake.gets-3)
	}
	// An hour later only the still-open PR is checked again.
	if err := runDesignNotesSweepOnce(ctx, store, now.Add(time.Hour), state, &out); err != nil {
		t.Fatalf("hourly run: %v", err)
	}
	if fake.gets != 4 || len(fake.comments[900]) != 1 {
		t.Fatalf("hourly run: gets %d, comments %v", fake.gets, fake.comments)
	}
}
