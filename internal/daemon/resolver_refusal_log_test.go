package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
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

type resolverLogEntry struct {
	format   string
	rendered string
}

type resolverLogCapture struct {
	entries     []resolverLogEntry
	processLogs bytes.Buffer
}

func newResolverLogDaemon(t *testing.T) (*db.Store, Daemon, *resolverLogCapture) {
	t.Helper()
	store := testStore(t)
	logs := &resolverLogCapture{}
	previousLogWriter := log.Writer()
	log.SetOutput(&logs.processLogs)
	t.Cleanup(func() {
		log.SetOutput(previousLogWriter)
	})
	daemon := Daemon{
		Repo:  github.Repository{Owner: "gitmoot", Name: "gitmoot"},
		Store: store,
		Logf: func(format string, args ...any) {
			logs.entries = append(logs.entries, resolverLogEntry{
				format:   format,
				rendered: fmt.Sprintf(format, args...),
			})
		},
	}
	return store, daemon, logs
}

func (logs *resolverLogCapture) assertSingleSink(t *testing.T) {
	t.Helper()
	if logs.processLogs.Len() != 0 {
		t.Fatalf("resolver wrote outside Daemon.Logf: %q", logs.processLogs.String())
	}
}

// onlyRefusalLog counts reason= tags in the Logf FORMAT, not the rendered
// diagnostic. Four review rounds on #1910 each defeated a weaker rule: prose
// renaming bypassed a prefix match; two reasons on one rendered line bypassed a
// per-line count; and valid input containing "reason=" looked like a second tag
// when rendered data was counted. Capturing the format separately distinguishes
// schema from data. Capturing the process logger as well prevents a second
// operator-visible sink from bypassing the assertion.
func onlyRefusalLog(t *testing.T, logs *resolverLogCapture) string {
	t.Helper()
	logs.assertSingleSink(t)
	reasons := []string{}
	tags := 0
	for _, entry := range logs.entries {
		if count := strings.Count(entry.format, "reason="); count > 0 {
			reasons = append(reasons, entry.rendered)
			tags += count
		}
	}
	if tags != 1 {
		t.Fatalf("reason= tags = %d across %d Logf call(s) (%q), want exactly 1", tags, len(reasons), reasons)
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

	line := onlyRefusalLog(t, logs)
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
		HeadRef:          "feat/reason=embedded",
		HeadRepoFullName: "gitmoot/gitmoot",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error = %v, want sql.ErrNoRows for an unregistered branch", err)
	}

	line := onlyRefusalLog(t, logs)
	for _, want := range []string{
		"reason=no_task_for_branch",
		`repo="gitmoot/gitmoot"`,
		`head_repo="gitmoot/gitmoot"`,
		`head_ref="feat/reason=embedded"`,
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

	line := onlyRefusalLog(t, logs)
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

	line := onlyRefusalLog(t, logs)
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
	// Same metadata independence as onlyRefusalLog: a renamed refusal line
	// must still fail this test, while "reason=" inside rendered input data must
	// not. Resolver diagnostics must also stay behind the injected sink.
	logs.assertSingleSink(t)
	for _, entry := range logs.entries {
		if strings.Contains(entry.format, "reason=") {
			t.Fatalf("logs = %q, want no reason-bearing Logf call on successful resolution", logs.entries)
		}
	}
}
