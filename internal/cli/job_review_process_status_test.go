package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/evidence"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRunJobListDistinguishesLiveAndDeadDispatchedReviews(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)

	liveWorktree := filepath.Join(home, "worktrees", "live")
	deadWorktree := filepath.Join(home, "worktrees", "dead")
	for _, item := range []struct {
		id       string
		worktree string
	}{
		{id: "local-review-live", worktree: liveWorktree},
		{id: "local-review-dead", worktree: deadWorktree},
	} {
		seedCLIJob(t, store, db.Job{
			ID:    item.id,
			Agent: "reviewer",
			Type:  "review",
			State: string(workflow.JobRunning),
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo:         "owner/repo",
				WorktreePath: item.worktree,
			}),
		}, string(workflow.JobRunning))
	}

	const daemonPID = 4242
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`UPDATE jobs SET runner_pid = ? WHERE id IN (?, ?)`,
		daemonPID, "local-review-live", "local-review-dead"); err != nil {
		t.Fatalf("set daemon runner_pid mutant: %v", err)
	}
	var recordedRunnerPID int
	if err := raw.QueryRow(`SELECT runner_pid FROM jobs WHERE id = ?`, "local-review-dead").Scan(&recordedRunnerPID); err != nil {
		t.Fatalf("read daemon runner_pid mutant: %v", err)
	}
	if recordedRunnerPID != daemonPID {
		t.Fatalf("dead review runner_pid = %d, want daemon pid %d", recordedRunnerPID, daemonPID)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	previousDaemonPID := jobReviewDaemonPID
	previousDescendants := jobReviewDescendantWorktrees
	previousSleep := jobReviewProcessSampleSleep
	jobReviewDaemonPID = func(config.Paths) (int, bool) { return daemonPID, true }
	samples := 0
	jobReviewDescendantWorktrees = func(gotPID int) ([]string, bool) {
		samples++
		if gotPID != daemonPID {
			t.Fatalf("daemon PID = %d, want %d", gotPID, daemonPID)
		}
		if samples == 1 {
			// A single empty observation is not enough to call a live review
			// stalled. The second sample sees its persistent runtime root.
			return nil, true
		}
		// A runtime root between tool calls still owns the worktree cwd; it does
		// not need a transient tool child to remain visibly in progress.
		return []string{liveWorktree}, true
	}
	jobReviewProcessSampleSleep = func(_ time.Duration) {}
	t.Cleanup(func() {
		jobReviewDaemonPID = previousDaemonPID
		jobReviewDescendantWorktrees = previousDescendants
		jobReviewProcessSampleSleep = previousSleep
	})

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var entries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode job list: %v\n%s", err, stdout.String())
	}
	if len(entries) != 2 {
		t.Fatalf("job list returned %d rows, want 2; zero-row success is not evidence", len(entries))
	}
	byID := make(map[string]jobListEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	assertReviewProcessStatus(t, byID["local-review-live"], reviewStatusInProgress)
	assertReviewProcessStatus(t, byID["local-review-dead"], reviewStatusStalled)

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"local-review-live\trunning\treview\treviewer\towner/repo\t#0\tREVIEW (observed; non-authoritative): in_progress",
		"local-review-dead\trunning\treview\treviewer\towner/repo\t#0\tREVIEW (observed; non-authoritative): stalled",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("job list output missing %q:\n%s", want, stdout.String())
		}
	}
	if samples != 2*dispatchedReviewProcessSamples {
		t.Fatalf("process-tree samples = %d, want %d", samples, 2*dispatchedReviewProcessSamples)
	}
}

func TestDispatchedReviewZeroDescendantsIsNotAllHealthy(t *testing.T) {
	job := db.Job{
		ID:    "local-review-dead",
		Type:  "review",
		State: string(workflow.JobRunning),
		Payload: mustJobPayload(t, workflow.JobPayload{
			WorktreePath: "/worktrees/dead",
		}),
	}
	previousDaemonPID := jobReviewDaemonPID
	previousDescendants := jobReviewDescendantWorktrees
	previousSleep := jobReviewProcessSampleSleep
	jobReviewDaemonPID = func(config.Paths) (int, bool) { return 4242, true }
	jobReviewDescendantWorktrees = func(int) ([]string, bool) {
		return nil, true
	}
	jobReviewProcessSampleSleep = func(_ time.Duration) {}
	t.Cleanup(func() {
		jobReviewDaemonPID = previousDaemonPID
		jobReviewDescendantWorktrees = previousDescendants
		jobReviewProcessSampleSleep = previousSleep
	})

	statuses := deriveDispatchedReviewStatuses(config.Paths{}, []db.Job{job})
	if len(statuses) != 1 {
		t.Fatalf("zero-descendant observation produced %d review rows, want 1 stalled row", len(statuses))
	}
	if got := statuses[job.ID].Status; got != reviewStatusStalled {
		t.Fatalf("zero-descendant review status = %q, want %q", got, reviewStatusStalled)
	}
}

func TestDispatchedReviewUnknownTreeAndSessionReviewStayNeutral(t *testing.T) {
	dispatched := db.Job{
		ID:    "local-review-unknown",
		Type:  "review",
		State: string(workflow.JobRunning),
		Payload: mustJobPayload(t, workflow.JobPayload{
			WorktreePath: "/worktrees/unknown",
		}),
	}
	session := dispatched
	session.ID = "session-review"
	session.ExternallyDriven = true

	previousDaemonPID := jobReviewDaemonPID
	previousDescendants := jobReviewDescendantWorktrees
	previousSleep := jobReviewProcessSampleSleep
	jobReviewDaemonPID = func(config.Paths) (int, bool) { return 4242, true }
	jobReviewDescendantWorktrees = func(int) ([]string, bool) {
		return nil, false
	}
	jobReviewProcessSampleSleep = func(_ time.Duration) {}
	t.Cleanup(func() {
		jobReviewDaemonPID = previousDaemonPID
		jobReviewDescendantWorktrees = previousDescendants
		jobReviewProcessSampleSleep = previousSleep
	})

	statuses := deriveDispatchedReviewStatuses(config.Paths{}, []db.Job{dispatched, session})
	if len(statuses) != 0 {
		t.Fatalf("unknown process tree or in-session review produced statuses: %+v", statuses)
	}
}

func TestDaemonDescendantWorktreesTracksRuntimeRootWhileIdle(t *testing.T) {
	procRoot := t.TempDir()
	liveWorktree := t.TempDir()
	deadWorktree := t.TempDir()

	writeFakeProcProcess(t, procRoot, 4242, 1, "")
	// The only descendant is the runtime root itself. No tool subprocess is
	// present, modeling a healthy seat idle between commands.
	writeFakeProcProcess(t, procRoot, 4243, 4242, liveWorktree)
	// A process in another worktree does not prove this daemon owns that work.
	writeFakeProcProcess(t, procRoot, 5000, 1, deadWorktree)

	worktrees, known := daemonDescendantWorktreesAtRoot(4242, procRoot)
	if !known {
		t.Fatal("daemon descendant snapshot is unknown, want known")
	}
	if len(worktrees) != 1 || worktrees[0] != liveWorktree {
		t.Fatalf("daemon descendant worktrees = %v, want [%s]", worktrees, liveWorktree)
	}

	emptyRoot := t.TempDir()
	writeFakeProcProcess(t, emptyRoot, 4242, 1, "")
	worktrees, known = daemonDescendantWorktreesAtRoot(4242, emptyRoot)
	if !known || len(worktrees) != 0 {
		t.Fatalf("daemon-only snapshot = (%v, %v), want ([], true)", worktrees, known)
	}
}

func assertReviewProcessStatus(t *testing.T, entry jobListEntry, want string) {
	t.Helper()
	if entry.ReviewStatus != want ||
		entry.ReviewStatusGrade != evidence.GradeObserved ||
		entry.ReviewStatusAuthority != reviewStatusAuthorityNonAuthoritative {
		t.Fatalf("review process status = (%q, %q, %q), want (%q, observed, non_authoritative)",
			entry.ReviewStatus, entry.ReviewStatusGrade, entry.ReviewStatusAuthority, want)
	}
}

func writeFakeProcProcess(t *testing.T, procRoot string, pid, ppid int, cwd string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fake proc process: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"),
		[]byte(strconv.Itoa(pid)+" (runtime worker) S "+strconv.Itoa(ppid)+"\n"), 0o600); err != nil {
		t.Fatalf("write fake proc stat: %v", err)
	}
	if cwd != "" {
		if err := os.Symlink(cwd, filepath.Join(dir, "cwd")); err != nil {
			t.Fatalf("symlink fake proc cwd: %v", err)
		}
	}
}
