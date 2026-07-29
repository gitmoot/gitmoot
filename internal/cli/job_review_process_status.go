package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/evidence"
	"github.com/gitmoot/gitmoot/internal/presence"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	dispatchedReviewProcessSamples = 2
	dispatchedReviewProcessGap     = 100 * time.Millisecond
)

type daemonPIDProbe func(paths config.Paths) (pid int, known bool)
type daemonDescendantWorktreeProbe func(daemonPID int) (worktrees []string, known bool)

var (
	jobReviewDaemonPID           daemonPIDProbe                = observedDaemonPID
	jobReviewDescendantWorktrees daemonDescendantWorktreeProbe = daemonDescendantWorktrees
	jobReviewProcessSampleSleep                                = time.Sleep
)

// deriveDispatchedReviewStatuses produces a display-only liveness observation
// for engine-run reviews. A positive sample is enough to preserve in_progress;
// stalled requires every sample to be conclusive and empty for that job. The
// externally-driven guard is the deliberate boundary around in-session reviews.
func deriveDispatchedReviewStatuses(paths config.Paths, jobs []db.Job) map[string]reviewStatusDisplay {
	statuses := make(map[string]reviewStatusDisplay)
	worktrees := make(map[string]string)
	for _, job := range jobs {
		if job.Type != "review" ||
			job.State != string(workflow.JobRunning) ||
			job.ExternallyDriven {
			continue
		}
		payload, err := jobListPayload(job)
		if err != nil {
			continue
		}
		if worktree := normalizedProcessWorktree(payload.WorktreePath); worktree != "" {
			worktrees[job.ID] = worktree
		}
	}
	if len(worktrees) == 0 ||
		jobReviewDaemonPID == nil ||
		jobReviewDescendantWorktrees == nil {
		return statuses
	}

	daemonPID, known := jobReviewDaemonPID(paths)
	if !known || daemonPID <= 0 {
		return statuses
	}

	live := make(map[string]bool, len(worktrees))
	allSamplesKnown := true
	for sample := 0; sample < dispatchedReviewProcessSamples; sample++ {
		observed, sampleKnown := jobReviewDescendantWorktrees(daemonPID)
		if !sampleKnown {
			allSamplesKnown = false
		} else {
			for jobID, worktree := range worktrees {
				if descendantOwnsWorktree(observed, worktree) {
					live[jobID] = true
				}
			}
		}
		if sample+1 < dispatchedReviewProcessSamples && jobReviewProcessSampleSleep != nil {
			jobReviewProcessSampleSleep(dispatchedReviewProcessGap)
		}
	}

	for jobID := range worktrees {
		switch {
		case live[jobID]:
			statuses[jobID] = observedReviewStatus(reviewStatusInProgress)
		case allSamplesKnown:
			statuses[jobID] = observedReviewStatus(reviewStatusStalled)
		}
	}
	return statuses
}

// observedDaemonPID verifies the advertised daemon without calling
// currentDaemonPID, whose stale-file cleanup would turn `job list` into a write.
func observedDaemonPID(paths config.Paths) (int, bool) {
	state := daemonProcessState(paths)
	contents, err := os.ReadFile(state.PIDFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	probeState, err := presence.ProbeDaemonProcess(pid, state.MetaFile)
	if err != nil || probeState != presence.DaemonRunning {
		return 0, false
	}
	return pid, true
}

func daemonDescendantWorktrees(daemonPID int) ([]string, bool) {
	return daemonDescendantWorktreesAtRoot(daemonPID, "/proc")
}

// daemonDescendantWorktreesAtRoot walks the transitive process tree below the
// daemon and returns the cwd of every readable descendant. The daemon itself is
// never evidence: jobs.runner_pid deliberately records that long-lived process,
// while a live runtime is a distinct descendant whose cwd binds it to one
// isolated review worktree.
func daemonDescendantWorktreesAtRoot(daemonPID int, procRoot string) ([]string, bool) {
	if daemonPID <= 0 {
		return nil, false
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}
	daemonStat, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(daemonPID), "stat"))
	if err != nil {
		return nil, false
	}
	if _, ok := procParentPID(daemonStat); !ok {
		return nil, false
	}

	parents := make(map[int]int, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "stat"))
		if err != nil {
			continue
		}
		if ppid, ok := procParentPID(stat); ok {
			parents[pid] = ppid
		}
	}

	observed := make(map[string]struct{})
	for pid := range parents {
		if pid == daemonPID || !processDescendsFrom(pid, daemonPID, parents) {
			continue
		}
		cwd, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "cwd"))
		if err != nil {
			continue
		}
		if worktree := normalizedProcessWorktree(strings.TrimSuffix(strings.TrimSpace(cwd), " (deleted)")); worktree != "" {
			observed[worktree] = struct{}{}
		}
	}
	worktrees := make([]string, 0, len(observed))
	for worktree := range observed {
		worktrees = append(worktrees, worktree)
	}
	sort.Strings(worktrees)
	return worktrees, true
}

func observedReviewStatus(status string) reviewStatusDisplay {
	if strings.TrimSpace(status) == "" {
		return reviewStatusDisplay{}
	}
	return reviewStatusDisplay{
		Status:    status,
		Grade:     evidence.GradeObserved,
		Authority: reviewStatusAuthorityNonAuthoritative,
	}
}

func procParentPID(stat []byte) (int, bool) {
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return 0, false
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil && ppid >= 0
}

func processDescendsFrom(pid, ancestor int, parents map[int]int) bool {
	seen := make(map[int]struct{})
	for pid > 1 {
		if _, duplicate := seen[pid]; duplicate {
			return false
		}
		seen[pid] = struct{}{}
		parent, ok := parents[pid]
		if !ok {
			return false
		}
		if parent == ancestor {
			return true
		}
		pid = parent
	}
	return false
}

func normalizedProcessWorktree(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

func descendantOwnsWorktree(observed []string, worktree string) bool {
	for _, cwd := range observed {
		cwd = normalizedProcessWorktree(cwd)
		if cwd == worktree || strings.HasPrefix(cwd, worktree+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
