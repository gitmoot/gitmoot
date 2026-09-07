package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1525: the detached-HEAD and wrong-branch refusals ended at "refusing to run
// or deliver from the wrong checkout" with no recovery path, leaving the remedy
// contract enforced on four of six before-run refusals. Both cross-family
// reviewers on PR #1521 raised it independently.
//
// THE ASSERTION THAT MATTERS IS EXECUTION, NOT PRESENCE. PR #1521 produced three
// separate defects that were all variants of a remedy that could not clear its
// own refusal, and every one of them would have passed a "does it print
// something" check: a remedy that moved the worktree to the PR tip when the gate
// wanted the frozen dispatch head, an unfetched head that returned a raw
// `fatal: Not a valid commit name`, and a universal recovery that could not
// clear a force-pushed-away head. So these tests run the printed command
// verbatim and re-drive the refusal.
func TestWrongBranchRefusalPrintsARemedyThatClearsIt(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := createDaemonWorkerGitCheckout(t, "feature/task-branch")
	head := remedyGitOutput(t, worktree, "rev-parse", "HEAD")
	task := db.Task{ID: "task-1525", RepoFullName: "owner/repo", Branch: "feature/payload-branch", WorktreePath: worktree}
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	payload := workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/payload-branch", TaskID: "task-1525",
		PullRequest: 42, HeadSHA: head, FixWorktree: true, WorktreePath: worktree,
	}
	job := db.Job{ID: "job-1525", Agent: "lead", Type: "implement"}

	_, err := implementationFinalizationTargetForRunner(ctx, store, job, payload, implementationFinalizationAfterRun, subprocess.ExecRunner{})
	var blocked workflow.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want a result-delivery BlockedError", err)
	}
	if !strings.Contains(err.Error(), "refusing to run or deliver from the wrong checkout") {
		t.Fatalf("error = %q, want the wrong-checkout refusal", err)
	}

	runPrintedRemedy(t, err.Error())

	if _, err := implementationFinalizationTargetForRunner(ctx, store, job, payload, implementationFinalizationAfterRun, subprocess.ExecRunner{}); err != nil {
		t.Fatalf("the printed remedy did not clear the refusal it printed; retry still fails: %v", err)
	}
}

// THE ARM THAT CAUGHT ME. With no dispatch head there is no command that can be
// verified to clear this refusal, and my first version printed
// `git checkout <branch>` anyway. The acceptance test executed it verbatim and
// it exited 1, because the branch does not exist locally: a remedy that cannot
// clear its own refusal, committed inside the fix for exactly that defect class.
//
// So this arm asserts the OTHER property the issue allows, and asserts the
// absence of a command rather than only the presence of prose. The tempting
// repair, `checkout -B <branch> HEAD`, would have cleared the refusal by
// rebranding whatever commit was sitting there as the expected branch, which
// defeats the guard instead of satisfying it.
func TestWrongBranchWithoutADispatchHeadStatesNoInPlaceRecovery(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := createDaemonWorkerGitCheckout(t, "feature/task-branch")
	task := db.Task{ID: "task-1525n", RepoFullName: "owner/repo", Branch: "feature/payload-branch", WorktreePath: worktree}
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	payload := workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/payload-branch", TaskID: "task-1525n",
		FixWorktree: true, WorktreePath: worktree,
	}
	_, err := implementationFinalizationTargetForRunner(ctx, store,
		db.Job{ID: "job-1525n", Agent: "lead", Type: "implement"}, payload,
		implementationFinalizationAfterRun, subprocess.ExecRunner{})
	if err == nil {
		t.Fatal("a wrong-branch worktree was accepted")
	}
	if !strings.Contains(err.Error(), "no in-place checkout can be verified") ||
		!strings.Contains(err.Error(), "dispatch a new implement job") {
		t.Fatalf("error = %q, want an explicit statement that no in-place recovery exists", err)
	}
	if strings.Contains(err.Error(), "`") {
		t.Fatalf("a refusal with no verifiable remedy still printed a command, which is the defect this issue is about:\n  %s", err)
	}
}

// The detached-HEAD arm. `CurrentBranch` cannot name a branch, so the refusal is
// the "no usable current branch" one, and its remedy must equally clear it.
func TestDetachedHeadRefusalPrintsARemedyThatClearsIt(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := createDaemonWorkerGitCheckout(t, "feature/payload-branch")
	head := remedyGitOutput(t, worktree, "rev-parse", "HEAD")
	runDaemonWorkerGit(t, worktree, "checkout", "--detach", head)

	task := db.Task{ID: "task-1525d", RepoFullName: "owner/repo", Branch: "feature/payload-branch", WorktreePath: worktree}
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	payload := workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/payload-branch", TaskID: "task-1525d",
		PullRequest: 42, HeadSHA: head, FixWorktree: true, WorktreePath: worktree,
	}
	job := db.Job{ID: "job-1525d", Agent: "lead", Type: "implement"}

	_, err := implementationFinalizationTargetForRunner(ctx, store, job, payload, implementationFinalizationBeforeRun, subprocess.ExecRunner{})
	if err == nil {
		t.Fatal("a detached fix worktree was accepted before the model ran")
	}
	if !strings.Contains(err.Error(), "no usable current branch") {
		t.Fatalf("error = %q, want the unverifiable-checkout refusal", err)
	}

	runPrintedRemedy(t, err.Error())

	if _, err := implementationFinalizationTargetForRunner(ctx, store, job, payload, implementationFinalizationBeforeRun, subprocess.ExecRunner{}); err != nil {
		t.Fatalf("the printed remedy did not clear the refusal it printed; retry still fails: %v", err)
	}
}

// ACCEPTANCE 3, and the one that converts the invariant from a convention into
// something enforced: EVERY refusal this function can emit must either print a
// runnable command or state explicitly that no in-place recovery exists.
//
// It is a source census rather than a behavioural sweep because the alternative
// is unreachable: several arms need a git failure, a missing object or a
// diverged head to fire, and a test that drove only the reachable ones would
// certify the family while silently skipping members. A new refusal added
// without either property fails this immediately, which is the property the
// issue asked for.
func TestEveryFinalizationRefusalIsActionable(t *testing.T) {
	source, err := os.ReadFile("daemon_workflow.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	body := functionSource(t, string(source), "func implementationFinalizationTargetForRunner(")

	refusals := regexp.MustCompile(`blockedResultDelivery\(fmt\.Sprintf\(\s*\n\s*"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(body, -1)
	if len(refusals) < 9 {
		t.Fatalf("found %d refusal literals, want at least the 9 this family had when the guard was written; if the count DROPPED the census is matching less than it should", len(refusals))
	}
	// "No in-place recovery exists" is the honest second option the issue allows,
	// and one arm already uses it: the missing-dispatch-head case refuses to guess
	// a fetch/reset remedy and says to dispatch a new job instead.
	noInPlaceRecovery := regexp.MustCompile(`dispatch a new|rerun through|refresh pull request`)

	// A refusal may DELEGATE its remedy to a variable and end in "; %s". That is
	// legitimate and two arms do it, but accepting a bare %s would gut this check,
	// so every remedy builder in the function is held to the same bar. My first
	// version accepted the literals and would have passed the broken
	// `git checkout <branch>` remedy the acceptance test caught.
	builders := regexp.MustCompile(`(?:checkoutRemedy|recovery)\s*(?::?=)\s*fmt\.Sprintf\(\s*\n?\s*"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(body, -1)
	if len(builders) < 3 {
		t.Fatalf("found %d remedy builders, want at least 3; a builder renamed out of this pattern would silently stop being checked", len(builders))
	}
	for _, builder := range builders {
		remedy := builder[1]
		if strings.Contains(remedy, "`") || noInPlaceRecovery.MatchString(remedy) {
			continue
		}
		t.Errorf("remedy builder is neither a runnable command nor a statement that none exists:\n  %s", remedy)
	}

	for _, refusal := range refusals {
		message := refusal[1]
		if strings.Contains(message, "`") || noInPlaceRecovery.MatchString(message) {
			continue
		}
		if strings.HasSuffix(message, "; %s") {
			// Delegated to a builder, and every builder was just checked.
			continue
		}
		t.Errorf("refusal prints no runnable remedy and does not say that none exists:\n  %s", message)
	}
}

// runPrintedRemedy extracts the FIRST backticked command from a refusal and runs
// it verbatim. Verbatim is the point: a remedy that only works after a human
// adjusts it has not cleared anything.
func runPrintedRemedy(t *testing.T, message string) {
	t.Helper()
	match := regexp.MustCompile("`([^`]+)`").FindStringSubmatch(message)
	if match == nil {
		t.Fatalf("refusal printed no command to run:\n  %s", message)
	}
	command := match[1]
	t.Logf("executing printed remedy: %s", command)
	fields, err := shellFieldsForRemedy(command)
	if err != nil {
		t.Fatalf("cannot parse printed remedy %q: %v", command, err)
	}
	if fields[0] != "git" {
		t.Fatalf("printed remedy is not a git command: %q", command)
	}
	output, runErr := exec.Command(fields[0], fields[1:]...).CombinedOutput()
	if runErr != nil {
		t.Fatalf("printed remedy %q failed: %v\n%s", command, runErr, output)
	}
}

// shellFieldsForRemedy splits a printed command, honouring the %q quoting the
// refusals use for worktree paths.
func shellFieldsForRemedy(command string) ([]string, error) {
	var fields []string
	var current strings.Builder
	inQuotes := false
	for _, r := range command {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case r == ' ' && !inQuotes:
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	if inQuotes {
		return nil, errors.New("unbalanced quotes")
	}
	if len(fields) == 0 {
		return nil, errors.New("empty command")
	}
	return fields, nil
}

// functionSource returns one top-level function's body text.
func functionSource(t *testing.T, source string, signature string) string {
	t.Helper()
	start := strings.Index(source, signature)
	if start < 0 {
		t.Fatalf("function %q not found; the census is matching nothing", signature)
	}
	rest := source[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("function %q has no closing brace", signature)
	}
	return rest[:end]
}

func remedyGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}
