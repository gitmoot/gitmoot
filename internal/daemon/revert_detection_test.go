package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestRevertedOriginalPRParsesBody covers the pure body-parser (#467): a GitHub
// Revert-button body maps to the ORIGINAL same-repo PR number; no-match,
// cross-repo, and malformed bodies all reject.
func TestRevertedOriginalPRParsesBody(t *testing.T) {
	d := Daemon{Repo: github.Repository{Owner: "jerryfane", Name: "gitmoot"}}
	cases := []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"plain reverts hash", "Reverts #7", 7, true},
		{"owner repo reference", "Reverts jerryfane/gitmoot#42", 42, true},
		{"lowercase reverts", "reverts #11", 11, true},
		{"embedded in prose", "This PR\n\nReverts jerryfane/gitmoot#9\n\ncc @someone", 9, true},
		{"no anchor", "Just a normal PR description.", 0, false},
		{"empty body", "", 0, false},
		{"cross repo owner", "Reverts otherowner/gitmoot#7", 0, false},
		{"cross repo name", "Reverts jerryfane/other#7", 0, false},
		{"malformed no number", "Reverts #", 0, false},
		{"zero pr", "Reverts #0", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := d.revertedOriginalPR(github.PullRequest{Body: tc.body})
			if ok != tc.ok || got != tc.want {
				t.Fatalf("revertedOriginalPR(%q) = (%d, %v), want (%d, %v)", tc.body, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// seedHarvestedPositive installs a template, a completed implement job attributed
// to its version, the original PR row, and writes the ORIGINAL merge's choice=a
// auto-trace feedback row (by running the harvester's merge path), returning the
// version id so a test can inspect the corrective overwrite.
func seedHarvestedPositive(t *testing.T, store *db.Store, repo github.Repository, originalPR int64, branch string) string {
	t.Helper()
	ctx := context.Background()
	template := db.AgentTemplate{
		ID:             "planner",
		Name:           "Planner",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "planner.md",
		ResolvedCommit: "commit-1",
		Content:        "Do the work.",
	}
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, template.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}

	payload := workflow.JobPayload{
		Repo:                   repo.FullName(),
		Branch:                 branch,
		PullRequest:            int(originalPR),
		TaskID:                 branch,
		TemplateID:             template.ID,
		TemplateResolvedCommit: template.ResolvedCommit,
		Result:                 &workflow.AgentResult{Decision: "implemented", Summary: "did the work"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload returned error: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID:      "implement-original",
		Agent:   "lead",
		Type:    "implement",
		State:   string(workflow.JobSucceeded),
		Payload: string(encoded),
	}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	if err := store.UpsertTask(ctx, db.Task{
		ID:           branch,
		RepoFullName: repo.FullName(),
		Title:        "Task 7",
		State:        "merged",
		Branch:       branch,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       originalPR,
		URL:          "https://github.com/" + repo.FullName() + "/pull/7",
		HeadBranch:   branch,
		BaseBranch:   "main",
		HeadSHA:      "orig-head",
		State:        "merged",
	}); err != nil {
		t.Fatalf("UpsertPullRequest original returned error: %v", err)
	}

	// Write the ORIGINAL merge's choice=a positive via the real harvester so the
	// corrective overwrite later targets the exact same UNIQUE row.
	harvester := skillopt.NewOutcomeHarvester(store, nil)
	if err := harvester.Harvest(ctx, db.Job{ID: "implement-original", Type: "implement"}, payload, workflow.Outcome{
		Kind: workflow.OutcomeMerged, Repo: repo.FullName(), PullRequest: int(originalPR), HeadSHA: "orig-head",
	}); err != nil {
		t.Fatalf("seed Harvest (merge) returned error: %v", err)
	}
	return installed.VersionID
}

func revertFeedback(t *testing.T, store *db.Store, versionID string) []db.FeedbackEvent {
	t.Helper()
	events, err := store.ListFeedbackEvents(context.Background(), skillopt.AutoTraceRunID(versionID))
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	return events
}

func revertDaemonEngine(store *db.Store) *workflow.Engine {
	return &workflow.Engine{
		Store:            store,
		OutcomeHarvester: skillopt.NewOutcomeHarvester(store, nil),
		JobID: func(request workflow.JobRequest) string {
			return strings.Join([]string{request.Action, request.Agent, request.TaskID}, "-")
		},
	}
}

// TestPollOnceCorrectsOnMergedRevert proves the wired daemon trigger (#467): with
// RevertDetectionEnabled, a merged revert PR (body `Reverts #7`) in the closed set
// flips the original PR's auto-trace row a -> b in place (count unchanged), and a
// second poll is idempotent.
func TestPollOnceCorrectsOnMergedRevert(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	versionID := seedHarvestedPositive(t, store, repo, 7, "task-7")

	before := revertFeedback(t, store, versionID)
	if len(before) != 1 || before[0].Choice != "a" {
		t.Fatalf("expected one choice=a row before revert, got %+v", before)
	}

	client := &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{
			"open": {},
			"closed": {{
				Number:  20,
				Title:   `Revert "Task 7"`,
				State:   "merged",
				Merged:  true,
				URL:     "https://github.com/jerryfane/gitmoot/pull/20",
				Body:    "Reverts #7",
				HeadRef: "revert-task-7",
				BaseRef: "main",
			}},
		},
		comments: map[int64][]github.IssueComment{},
	}
	d := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: revertDaemonEngine(store), RevertDetectionEnabled: true}

	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	after := revertFeedback(t, store, versionID)
	if len(after) != 1 {
		t.Fatalf("revert must overwrite in place, got %d rows: %+v", len(after), after)
	}
	if after[0].Choice != "b" {
		t.Fatalf("revert must flip choice a -> b, got %q", after[0].Choice)
	}
	if after[0].ItemID != before[0].ItemID {
		t.Fatalf("revert wrote a different item id %q vs %q", after[0].ItemID, before[0].ItemID)
	}

	// A second poll re-fires the same in-place upsert: still one row, still b.
	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce returned error: %v", err)
	}
	idempotent := revertFeedback(t, store, versionID)
	if len(idempotent) != 1 || idempotent[0].Choice != "b" {
		t.Fatalf("re-poll must be idempotent (one choice=b row), got %+v", idempotent)
	}
}

// TestPollOnceSkipsUnmergedRevert proves an OPEN (not-yet-landed) revert PR does
// not correct: detection fires on a merged revert only.
func TestPollOnceSkipsUnmergedRevert(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	versionID := seedHarvestedPositive(t, store, repo, 7, "task-7")

	client := &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{
			"open": {},
			"closed": {{
				Number:  21,
				Title:   `Revert "Task 7"`,
				State:   "closed", // closed but NOT merged
				Merged:  false,
				URL:     "https://github.com/jerryfane/gitmoot/pull/21",
				Body:    "Reverts #7",
				HeadRef: "revert-task-7",
				BaseRef: "main",
			}},
		},
		comments: map[int64][]github.IssueComment{},
	}
	d := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: revertDaemonEngine(store), RevertDetectionEnabled: true}

	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	events := revertFeedback(t, store, versionID)
	if len(events) != 1 || events[0].Choice != "a" {
		t.Fatalf("an unmerged revert must not correct the positive, got %+v", events)
	}
}

// TestPollOnceDisabledIsByteIdentical is the off-by-default regression (#467):
// with RevertDetectionEnabled=false the daemon issues NO closed-PR list, parses NO
// body, and writes NOTHING — the original positive is untouched and the closed-PR
// list is never requested.
func TestPollOnceDisabledIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	versionID := seedHarvestedPositive(t, store, repo, 7, "task-7")

	client := &countingClosedListGitHub{fakeGitHub: &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{
			"open": {},
			"closed": {{
				Number: 20, State: "merged", Merged: true, Body: "Reverts #7",
				HeadRef: "revert-task-7", BaseRef: "main",
			}},
		},
		comments: map[int64][]github.IssueComment{},
	}}
	// RevertDetectionEnabled defaults false.
	d := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: revertDaemonEngine(store)}

	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	events := revertFeedback(t, store, versionID)
	if len(events) != 1 || events[0].Choice != "a" {
		t.Fatalf("disabled revert detection must be byte-identical (positive untouched), got %+v", events)
	}
	if client.closedListCalls != 0 {
		t.Fatalf("disabled revert detection must request NO closed-PR list, got %d calls", client.closedListCalls)
	}
}

// countingClosedListGitHub counts "closed"-state ListPullRequests calls so the
// off-path byte-identical test can prove no extra GitHub read happens when revert
// detection is disabled.
type countingClosedListGitHub struct {
	*fakeGitHub
	closedListCalls int
}

func (c *countingClosedListGitHub) ListPullRequests(ctx context.Context, repo github.Repository, state string) ([]github.PullRequest, error) {
	if state == "closed" {
		c.closedListCalls++
	}
	return c.fakeGitHub.ListPullRequests(ctx, repo, state)
}
