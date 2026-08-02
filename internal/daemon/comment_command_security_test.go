package daemon

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

func TestHandleCommentCodeOnlyTokensStaySilent(t *testing.T) {
	ctx := context.Background()
	store, repo, client, daemon := commentCommandFixture(t, ctx)
	pull := github.PullRequest{Number: 10, Title: "Camera design", HeadRef: "camera-state"}
	comment := github.IssueComment{
		ID:     5148920899,
		Author: "alice",
		Body: strings.Join([]string{
			"```swift",
			"@Published private(set) var state = .uninitialized",
			"````",
			"~~~swift",
			"@objc private nonisolated func handleRuntimeError() {}",
			"~~~",
			"> ```swift",
			"> @Published private(set) var quoted = true",
			"> ```",
		}, "\n"),
	}

	if err := daemon.handleComment(ctx, pull, comment); err != nil {
		t.Fatalf("handleComment returned error: %v", err)
	}
	if len(client.posted) != 0 {
		t.Fatalf("code-only comment posted replies = %+v, want none", client.posted)
	}
	if jobs, err := store.ListJobs(ctx); err != nil || len(jobs) != 0 {
		t.Fatalf("code-only comment jobs = %+v, err=%v, want none", jobs, err)
	}
	seen, err := store.HasCommentSeen(ctx, repo.FullName(), comment.ID)
	if err != nil {
		t.Fatalf("HasCommentSeen returned error: %v", err)
	}
	if seen {
		t.Fatal("code-only comment was marked seen; non-command prose must remain outside command handling")
	}
}

func TestHandleCommentAuthorizedCommandStillDispatches(t *testing.T) {
	ctx := context.Background()
	store, repo, client, daemon := commentCommandFixture(t, ctx)
	pull := github.PullRequest{Number: 10, Title: "Camera design", HeadRef: "camera-state"}
	comment := github.IssueComment{
		ID:     5148922000,
		Author: "alice",
		Body:   "/gitmoot ask helper inspect the state model",
	}

	if err := daemon.handleComment(ctx, pull, comment); err != nil {
		t.Fatalf("handleComment returned error: %v", err)
	}
	wantID := jobID(repo, pull.Number, comment.ID, 0, "helper", "ask")
	job, err := store.GetJob(ctx, wantID)
	if err != nil {
		t.Fatalf("GetJob(%q) returned error: %v", wantID, err)
	}
	if job.Agent != "helper" || job.Type != "ask" {
		t.Fatalf("dispatched job = %+v", job)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "queued `ask` job") {
		t.Fatalf("posted acknowledgements = %+v, want one queued-job reply", client.posted)
	}
}

func TestHandleCommentAuthorizedIndentedCodeDoesNotDispatch(t *testing.T) {
	tests := []struct {
		name   string
		indent string
	}{
		{name: "four spaces", indent: "    "},
		{name: "tab", indent: "\t"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, _, client, daemon := commentCommandFixture(t, ctx)
			client.permissions = map[string]string{"alice": "write"}
			var parsed []Command
			daemon.parseCommentCommand = func(line string) (Command, bool) {
				command, ok := ParseCommand(line)
				if ok {
					parsed = append(parsed, command)
				}
				return command, ok
			}
			pull := github.PullRequest{Number: 10, Title: "Camera design", HeadRef: "camera-state"}
			comment := github.IssueComment{
				ID:     int64(5148922250 + index),
				Author: "alice",
				Body: "/gitmoot status\n" + test.indent +
					"@helper ask this indented code must not dispatch",
			}

			if err := daemon.handleComment(ctx, pull, comment); err != nil {
				t.Fatalf("handleComment returned error: %v", err)
			}
			if len(parsed) != 1 || parsed[0].Action != "status" {
				t.Fatalf("parsed commands = %+v, want only the unindented status command", parsed)
			}
			if jobs, err := store.ListJobs(ctx); err != nil || len(jobs) != 0 {
				t.Fatalf("indented code jobs = %+v, err=%v, want none", jobs, err)
			}
		})
	}
}

func TestHandleCommentMixedProseAndAddressedCommandPostsOnlyAck(t *testing.T) {
	ctx := context.Background()
	store, repo, client, daemon := commentCommandFixture(t, ctx)
	pull := github.PullRequest{Number: 10, Title: "Camera design", HeadRef: "camera-state"}
	comment := github.IssueComment{
		ID:     5148922500,
		Author: "alice",
		Body: strings.Join([]string{
			"Here is my reasoning about the state model.",
			"@helper ask inspect the state model",
			"Thanks, that should cover it.",
		}, "\n"),
	}

	if err := daemon.handleComment(ctx, pull, comment); err != nil {
		t.Fatalf("handleComment returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "queued `ask` job") {
		t.Fatalf("posted comments = %+v, want exactly one queued-job acknowledgement", client.posted)
	}
	wantID := jobID(repo, pull.Number, comment.ID, 0, "helper", "ask")
	if _, err := store.GetJob(ctx, wantID); err != nil {
		t.Fatalf("GetJob(%q) returned error: %v", wantID, err)
	}
}

func TestHandleCommentUnauthorizedRejectedBeforeParse(t *testing.T) {
	ctx := context.Background()
	store, repo, client, daemon := commentCommandFixture(t, ctx)
	client.permissions = map[string]string{"mallory": "read"}
	parseCalls := 0
	daemon.parseCommentCommand = func(line string) (Command, bool) {
		parseCalls++
		return ParseCommand(line)
	}
	pull := github.PullRequest{Number: 10, Title: "Camera design", HeadRef: "camera-state"}
	comment := github.IssueComment{ID: 5148923000, Author: "mallory", Body: "@helper ask inspect the state model"}

	if err := daemon.handleComment(ctx, pull, comment); err != nil {
		t.Fatalf("handleComment returned error: %v", err)
	}
	if parseCalls != 0 {
		t.Fatalf("unauthorized command reached parser %d time(s), want 0", parseCalls)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "ignored comment 5148923000") {
		t.Fatalf("posted acknowledgements = %+v, want permission rejection", client.posted)
	}
	if _, err := store.GetJob(ctx, jobID(repo, pull.Number, comment.ID, 0, "helper", "ask")); !isNoRows(err) {
		t.Fatalf("unauthorized comment created a job or returned unexpected error: %v", err)
	}
}

func TestHandleCommentDistinguishesProseFromMalformedCommand(t *testing.T) {
	ctx := context.Background()
	store, repo, client, daemon := commentCommandFixture(t, ctx)
	pull := github.PullRequest{Number: 10, Title: "Camera design", HeadRef: "camera-state"}

	prose := github.IssueComment{ID: 5148924000, Author: "alice", Body: "A human writeup is not a malformed command and needs no engine reply."}
	if err := daemon.handleComment(ctx, pull, prose); err != nil {
		t.Fatalf("handleComment(prose) returned error: %v", err)
	}
	if len(client.posted) != 0 {
		t.Fatalf("non-command prose posted replies = %+v, want none", client.posted)
	}
	if seen, err := store.HasCommentSeen(ctx, repo.FullName(), prose.ID); err != nil || seen {
		t.Fatalf("non-command prose seen=%v err=%v, want false", seen, err)
	}

	malformed := github.IssueComment{ID: 5148924001, Author: "alice", Body: "@helper"}
	if err := daemon.handleComment(ctx, pull, malformed); err != nil {
		t.Fatalf("handleComment(malformed) returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "malformed agent mention command") {
		t.Fatalf("malformed command replies = %+v, want one visible parse error", client.posted)
	}
	if seen, err := store.HasCommentSeen(ctx, repo.FullName(), malformed.ID); err != nil || !seen {
		t.Fatalf("malformed command seen=%v err=%v, want true", seen, err)
	}
}

func TestParseCommandsMarkdownCodeEdges(t *testing.T) {
	body := strings.Join([]string{
		"````markdown",
		"```",
		"/gitmoot ask helper nested shorter fence is content",
		"```",
		"````",
		"> ~~~go",
		"> /gitmoot ask helper quoted fence",
		"> ~~~",
		"   ```go",
		"/gitmoot ask helper three-space fence",
		"   ```",
		"```text",
		"/gitmoot ask helper unclosed fence",
	}, "\n")
	if commands := ParseCommandsWithoutAuthorization(body); len(commands) != 0 {
		t.Fatalf("fenced edge cases parsed commands = %+v, want none", commands)
	}

	commands := ParseCommandsWithoutAuthorization("Use `/gitmoot ask helper inline` as documentation.\n/gitmoot status")
	if len(commands) != 1 || commands[0].Action != "status" {
		t.Fatalf("inline stripping commands = %+v, want only status", commands)
	}
}

// TestHandleCommentUnrecognizedActionStaysSilent pins #1355: on the PR path, a
// line that is addressed by shape but names no known action must produce no
// outbound comment. The body is the real incident — a Swift property wrapper
// outside any code fence, which parses as the mention form `@<agent> <action>`
// with action "private(set)" — so before the fix Gitmoot replied "unsupported
// command action" onto an unrelated repository.
//
// The second leg is the discriminator: a *recognized* action with a bad argument
// still gets a reply, so the fix suppresses parser noise rather than silencing
// command feedback wholesale.
//
// There is deliberately no issue-path leg. handleIssueComment filters to
// Action == "ask" before dispatching, so an unrecognized action is dropped
// there and never reaches handleIssueAsk; asserting silence on that path would
// certify the pre-existing filter, not this change.
func TestHandleCommentUnrecognizedActionStaysSilent(t *testing.T) {
	ctx := context.Background()
	store, _, client, daemon := commentCommandFixture(t, ctx)
	pull := github.PullRequest{Number: 10, Title: "Camera design", HeadRef: "camera-state"}

	comment := github.IssueComment{
		ID:     5148925000,
		Author: "alice",
		Body:   "@Published private(set) var state = .uninitialized",
	}
	if err := daemon.handleComment(ctx, pull, comment); err != nil {
		t.Fatalf("handleComment returned error: %v", err)
	}
	if len(client.posted) != 0 {
		t.Fatalf("unrecognized action on a PR posted replies = %+v, want none", client.posted)
	}
	if jobs, err := store.ListJobs(ctx); err != nil || len(jobs) != 0 {
		t.Fatalf("unrecognized action jobs = %+v, err=%v, want none", jobs, err)
	}

	badArgument := github.IssueComment{ID: 5148925002, Author: "alice", Body: "@helper retry"}
	if err := daemon.handleComment(ctx, pull, badArgument); err != nil {
		t.Fatalf("handleComment(bad argument) returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "requires a job id") {
		t.Fatalf("recognized action with a bad argument replies = %+v, want one job-id error", client.posted)
	}
}

// TestHandleCommentInlineSpanCannotRedirectCommand pins inline-span stripping
// (#1365) with a fixture that actually exercises it. The first field is
// `/gitmoot`, so isCommandAddressedLine accepts the line and it reaches the
// parser either way — which is what the existing assertion in
// TestParseCommandsMarkdownCodeEdges does not do, because its fixture line
// begins with "Use" and so is never addressed at all.
//
// In the `/gitmoot ask <agent> …` form the agent is the third field, so a
// quoted span sitting before the real agent name shifts it. Stripped, the
// command dispatches to `helper`; unstripped, the agent resolves to the
// backticked token and no job is created. The span therefore cannot change
// which command runs.
func TestHandleCommentInlineSpanCannotRedirectCommand(t *testing.T) {
	ctx := context.Background()
	store, repo, client, daemon := commentCommandFixture(t, ctx)
	pull := github.PullRequest{Number: 10, Title: "Camera design", HeadRef: "camera-state"}
	comment := github.IssueComment{
		ID:     5148926000,
		Author: "alice",
		Body:   "/gitmoot ask `not-an-agent` helper inspect the state model",
	}

	if err := daemon.handleComment(ctx, pull, comment); err != nil {
		t.Fatalf("handleComment returned error: %v", err)
	}
	wantID := jobID(repo, pull.Number, comment.ID, 0, "helper", "ask")
	job, err := store.GetJob(ctx, wantID)
	if err != nil {
		t.Fatalf("GetJob(%q) returned error: %v — the inline span redirected the command", wantID, err)
	}
	if job.Agent != "helper" || job.Type != "ask" {
		t.Fatalf("dispatched job = %+v, want ask/helper", job)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "queued `ask` job") {
		t.Fatalf("posted acknowledgements = %+v, want one queued-job reply", client.posted)
	}
}

func commentCommandFixture(t *testing.T, ctx context.Context) (*db.Store, github.Repository, *fakeGitHub, Daemon) {
	t.Helper()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "helper",
		Role:           "helper",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{}
	return store, repo, client, Daemon{Repo: repo, Store: store, GitHub: client}
}

func isNoRows(err error) bool {
	return err == sql.ErrNoRows
}
