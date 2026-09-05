package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// A task-resolution refusal used to name only the branch (#1909). Three
// reproductions against a real PR still could not say WHICH input mismatched,
// because the operator-visible text carried pull.HeadRef and nothing else - so
// the fork-head guard, an absent task row, and a task belonging to another
// repository were indistinguishable in the field.
//
// These tests pin the DIAGNOSTIC content of each refusal arm: the reason tag and
// the values the decision was actually made on. A refusal that stops naming its
// inputs fails here even though resolution behaviour is unchanged.

func newResolverLogDaemon(t *testing.T) (*db.Store, Daemon, *[]string) {
	t.Helper()
	store := testStore(t)
	logs := &[]string{}
	daemon := Daemon{
		Repo:  github.Repository{Owner: "gitmoot", Name: "gitmoot"},
		Store: store,
		Logf: func(format string, args ...any) {
			*logs = append(*logs, fmt.Sprintf(format, args...))
		},
	}
	return store, daemon, logs
}

// onlyRefusalLog counts reason= TAG OCCURRENCES across every captured line, not
// lines carrying a prose prefix and not reason-bearing lines. Two review rounds
// on #1910 each defeated a weaker rule with a compiling production mutant:
// round 2 renamed the prose ("task resolution rejected") past a prefix match,
// and round 3 merged both reasons onto ONE line
// ("reason=task_repo_mismatch reason=fork_head") past a per-line count. An
// operator reading two reasons for one refusal cannot tell which input decided
// it, and that is true whether they sit on one line or two - so the property is
// one reason tag per refusal, and the tag is what gets counted.
func onlyRefusalLog(t *testing.T, logs []string) string {
	t.Helper()
	reasons := []string{}
	tags := 0
	for _, line := range logs {
		if count := strings.Count(line, "reason="); count > 0 {
			reasons = append(reasons, line)
			tags += count
		}
	}
	if tags != 1 {
		t.Fatalf("reason= tags = %d across %d line(s) (%q), want exactly 1", tags, len(reasons), reasons)
	}
	return reasons[0]
}

// A fork head that merely collides with a local branch name must log the head
// repository it was rejected for - the value that distinguishes this arm from an
// absent task row.
func TestLookupPolledPullRequestTaskLogsForkHeadRefusalInputs(t *testing.T) {
	ctx := context.Background()
	store, daemon, logs := newResolverLogDaemon(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-local",
		RepoFullName: "gitmoot/gitmoot",
		GoalID:       "goal-1",
		Title:        "Local task",
		State:        "pr_open",
		Branch:       "shared-branch-name",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	_, err := daemon.lookupPolledPullRequestTask(ctx, github.PullRequest{
		Number:           99,
		HeadRef:          "shared-branch-name",
		HeadRepoFullName: "outsider/gitmoot",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error = %v, want sql.ErrNoRows for a fork head", err)
	}

	line := onlyRefusalLog(t, *logs)
	for _, want := range []string{
		"reason=fork_head",
		`head_repo="outsider/gitmoot"`,
		`repo="gitmoot/gitmoot"`,
		`head_ref="shared-branch-name"`,
		"gitmoot/gitmoot#99",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("fork-head refusal log = %q, want it to contain %q", line, want)
		}
	}
}

// A local head with no task row must log a DIFFERENT reason than the fork arm,
// so an operator can tell "not mine" from "not registered" without reading code.
func TestLookupPolledPullRequestTaskLogsMissingTaskRefusalInputs(t *testing.T) {
	ctx := context.Background()
	_, daemon, logs := newResolverLogDaemon(t)

	_, err := daemon.lookupPolledPullRequestTask(ctx, github.PullRequest{
		Number:           7,
		HeadRef:          "feat/unregistered",
		HeadRepoFullName: "gitmoot/gitmoot",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error = %v, want sql.ErrNoRows for an unregistered branch", err)
	}

	line := onlyRefusalLog(t, *logs)
	for _, want := range []string{
		"reason=no_task_for_branch",
		`repo="gitmoot/gitmoot"`,
		`head_repo="gitmoot/gitmoot"`,
		`head_ref="feat/unregistered"`,
		"gitmoot/gitmoot#7",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing-task refusal log = %q, want it to contain %q", line, want)
		}
	}
	if strings.Contains(line, "reason=fork_head") {
		t.Fatalf("missing-task refusal log = %q, want a reason distinct from the fork-head arm", line)
	}
}

// The third arm: the branch string resolves a task BY ID, but that task belongs
// to another repository. Both repositories must appear, since the mismatch is
// the whole content of the refusal.
func TestLookupPullRequestTaskLogsTaskRepoMismatchInputs(t *testing.T) {
	ctx := context.Background()
	store, daemon, logs := newResolverLogDaemon(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-elsewhere",
		RepoFullName: "other/repo",
		GoalID:       "goal-1",
		Title:        "Task in another repository",
		State:        "pr_open",
		Branch:       "some-branch",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	_, err := daemon.lookupPullRequestTask(ctx, "gitmoot/gitmoot", "task-elsewhere")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error = %v, want sql.ErrNoRows when the task belongs to another repository", err)
	}

	line := onlyRefusalLog(t, *logs)
	for _, want := range []string{
		"reason=task_repo_mismatch",
		`lookup_repo="gitmoot/gitmoot"`,
		`task="task-elsewhere"`,
		`task_repo="other/repo"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("task-repo-mismatch refusal log = %q, want it to contain %q", line, want)
		}
	}
}

// The composed path, which the direct-call test above cannot reach: a polled PR
// whose HeadRef happens to equal ANOTHER repository's task ID. lookupPullRequestTask's
// id fallback (db.Store.GetTask is a global lookup, not repo-scoped) refuses with
// the repo-mismatch reason, and the polled resolver used to add no_task_for_branch
// on top - one event, two contradictory reason tags, which is the ambiguity #1909
// exists to remove. Exactly one refusal line must survive, and it must be the arm
// that actually decided.
func TestLookupPolledPullRequestTaskLogsOneReasonWhenTaskBelongsToAnotherRepo(t *testing.T) {
	ctx := context.Background()
	store, daemon, logs := newResolverLogDaemon(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-elsewhere",
		RepoFullName: "other/repo",
		GoalID:       "goal-1",
		Title:        "Task in another repository",
		State:        "pr_open",
		Branch:       "some-branch",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	_, err := daemon.lookupPolledPullRequestTask(ctx, github.PullRequest{
		Number:           42,
		HeadRef:          "task-elsewhere",
		HeadRepoFullName: "gitmoot/gitmoot",
	})
	// Resolution behaviour is unchanged: every caller reads this as "no managed
	// task", so the reason marker must not become visible as a different error.
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error = %v, want sql.ErrNoRows through the polled resolver", err)
	}

	line := onlyRefusalLog(t, *logs)
	for _, want := range []string{
		"reason=task_repo_mismatch",
		`lookup_repo="gitmoot/gitmoot"`,
		`task="task-elsewhere"`,
		`task_repo="other/repo"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("composed refusal log = %q, want it to contain %q", line, want)
		}
	}
	if strings.Contains(line, "reason=no_task_for_branch") {
		t.Fatalf("composed refusal log = %q, want the deciding arm's reason only", line)
	}
}

// Successful resolution must stay silent: a log line per resolved PR per poll
// would bury the refusals this change exists to surface.
func TestLookupPolledPullRequestTaskLogsNothingOnSuccess(t *testing.T) {
	ctx := context.Background()
	store, daemon, logs := newResolverLogDaemon(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-ok",
		RepoFullName: "gitmoot/gitmoot",
		GoalID:       "goal-1",
		Title:        "Resolvable task",
		State:        "pr_open",
		Branch:       "feat/registered",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	task, err := daemon.lookupPolledPullRequestTask(ctx, github.PullRequest{
		Number:           11,
		HeadRef:          "feat/registered",
		HeadRepoFullName: "gitmoot/gitmoot",
	})
	if err != nil {
		t.Fatalf("lookupPolledPullRequestTask returned error: %v", err)
	}
	if task.ID != "task-ok" {
		t.Fatalf("task = %+v, want task-ok", task)
	}
	// Same prose-independence as onlyRefusalLog: a renamed refusal line must
	// still fail this test.
	for _, line := range *logs {
		if strings.Contains(line, "reason=") {
			t.Fatalf("logs = %q, want no reason-bearing line on successful resolution", *logs)
		}
	}
}
