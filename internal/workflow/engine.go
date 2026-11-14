package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/github"
)

type Engine struct {
	Store *db.Store
	// RequireWorkflowPolicy is passed to every mailbox the engine creates so
	// continuations and delegation enqueue share the same home-aware policy.
	RequireWorkflowPolicy func(repo string) RequireWorkflowPolicy
	// OrgPolicy is passed to every mailbox the engine creates. Descendant jobs are
	// exempt from the gate but preserve their initiating role as provenance.
	OrgPolicy         func(repo string) OrgEnforcement
	RequiredReviewers []string
	MergeGate         MergeGate
	JobID             func(JobRequest) string
	// EventSink is the injected, best-effort outbound event seam (#446), mirroring
	// EscalationNotifier: when configured, the engine emits a redacted, versioned
	// events.Event on each terminal transition it owns (job.finished on a
	// succeeded terminal, job.needs_attention on an escalate_human pause) so an
	// off-box consumer (a webhook) can observe the run. It is optional and
	// nil-safe: when nil (the default, no [events] config) NO event is constructed
	// or emitted and behavior is byte-identical. Emit is fire-and-forget and best-
	// effort — a slow/hung/erroring sink never blocks or fails a job (see
	// internal/events). The daemon emits the failure/blocked/awaiting-human
	// terminal cases it owns through the SAME sink so the whole terminal set is
	// covered. #445 (the ask-gate) rides this seam to emit its job.needs_attention.
	EventSink events.Sink
	// BlockerDeferrer is the injected, best-effort, nil-by-default PRE-TERMINAL
	// operational-blocker deferrer (#532 slice E). When set, the engine's Mailbox
	// consults it on a delivery-seam failure BEFORE the terminal transition: if it
	// re-queues the job behind a classified operational blocker (runtime auth/quota/
	// network), Run reports ErrJobDeferred and never emits job.failed, so the
	// [events] stream sees the deferral as a first-class transition instead of a
	// failed→deferred flap. It mirrors EventSink: optional and nil-safe (when nil —
	// foreground/ask paths and every non-daemon construction — Run is byte-identical),
	// and best-effort (a deferrer error is treated as "not deferred" so the job takes
	// its normal terminal path). The concrete impl lives in cli (it classifies with
	// the #602 matcher and writes the payload hold fields), keeping the engine free of
	// the classification coupling; it is wired only on the daemon run path.
	BlockerDeferrer func(ctx context.Context, jobID string, cause error) (bool, error)
	// Home is the resolved GITMOOT_HOME root used to place per-delegation
	// worktrees. DelegationWorktrees is the checkout-bound git client that
	// performs the worktree-add. Both are optional: when either is unset, the
	// dispatcher enqueues implement delegations against the shared checkout
	// (legacy behavior) rather than allocating isolated worktrees.
	Home                   string
	DelegationWorktrees    WorktreeManager
	DelegationCheckout     string
	cleanupTargetValidator func(home, jobID, jobType string, payload JobPayload) error
	// BeforeReadOnlyWorktreeCleanup preserves terminal evidence (such as a
	// read-only diff) independently of deletion eligibility. Missing collectors
	// or preservation failures refuse read-only deletion.
	BeforeReadOnlyWorktreeCleanup func(context.Context, string, string, JobPayload) error
	// CleanupRetention reloads producer retention policy during each inspection
	// and immediately before deletion. Nil is reserved for explicit engine policy.
	CleanupRetention func() (time.Duration, error)
	// CleanupBudget reloads the producer-approved byte cap and sizing bounds for a repository
	// during each budget-gated inspection (#2360 v2). It returns (nil, nil, nil) when no cap is
	// configured (UNKNOWN) and an error when the configuration cannot be read or is invalid; both
	// refuse with size_budget_unknown. Nil means no cap is ever known.
	CleanupBudget func(repo string) (*ApprovedCleanupSizeBudget, *CleanupSizeLimits, error)
	// OwnerPIDLive reports whether a recorded owner PID is a live process on this
	// host. It gates the DESTRUCTIVE implement-delegation worktree/branch cleanup so
	// a worktree still owned by a live runtime worker is never force-removed out from
	// under it (#536): a job whose terminal state was synthesized by stale recovery
	// while its worker was still running keeps an unexpired/live runtime-session
	// lock, and cleanup refuses while that lock is active. Optional and nil-safe:
	// when nil the engine uses the default same-host syscall probe; tests inject a
	// fake. On a healthy terminal the lock is already released, so cleanup is
	// byte-identical to before this field existed.
	OwnerPIDLive func(pid int64) bool
	// WorktreeHasLiveProcess reports whether any live process on this host still has
	// its working directory inside the given worktree path. It is the PID-reuse- and
	// hostname-rename-immune never-clobber gate the DESTRUCTIVE implement-delegation
	// cleanup consults IN ADDITION to the runtime-session lock (#536 finding 1):
	// past lease expiry the lock is reaped, but a daemon-crash-reparented worker can
	// still be writing to the worktree. Removing it then would orphan the live worker
	// onto a deleted cwd — the original #536 corruption shifted to the lease boundary.
	// Optional and nil-safe: when nil the engine uses the default best-effort /proc
	// cwd scan exposed by WorktreeHasLiveProcess; tests inject a fake. On a healthy
	// terminal the worker has already exited, so the probe reports false and cleanup
	// proceeds unchanged.
	WorktreeHasLiveProcess func(path string) bool
	// WorktreeLiveness is the proof-bearing form used by terminal task reclaim.
	// known=false is a hard keep decision because an unreadable process table or
	// cwd link cannot prove that unlinking is safe. WorktreeHasLiveProcess remains
	// the compatibility seam for existing cleanup tests and callers.
	WorktreeLiveness func(path string) (live bool, known bool)
	// Now returns the current time and is the engine's only clock source. It is
	// optional and defaults to time.Now; tests inject it.
	Now func() time.Time
	// RuntimeDefaultModel, when set, resolves a runtime's configured registry
	// default_model (HOME-AWARE) for the runtime named by the argument (#652). It is
	// copied onto the Mailbox in mailbox() and consulted at delivery ONLY as the
	// final model fallback — after the job --model and the agent --model — so an
	// agent/job pin always wins. Nil (the default) forces nothing, so delivery is
	// byte-identical to before #652.
	RuntimeDefaultModel func(runtimeName string) string
	// ReviewModelPool is the engine-side half of #2180: engine-produced reviews
	// pass through the same Mailbox chokepoint, so the resolver must reach it
	// from here too or the fallback stays unreachable on that path.
	ReviewModelPool func(purpose string) []string
	// RuntimeDefaultEffort mirrors RuntimeDefaultModel for the runtime registry's
	// default_effort fallback.
	RuntimeDefaultEffort func(runtimeName string) string
	// ResultCheckMode is the resolved [workflow] result_checks policy (#526): the
	// deterministic binary-checklist audit run on a job's parsed gitmoot_result.
	// It is copied onto every Mailbox the engine builds (mailbox()). The zero
	// value ("") and "off" disable the audit entirely, so an Engine built with a
	// bare struct literal (every test, the ask/foreground path) is byte-identical;
	// the daemon resolves the real mode (default warn) from config and sets it
	// here. "warn" records failures as a job event + job-detail field + feed-
	// forward row; "block" additionally fails the job via the contract-violation
	// path.
	ResultCheckMode ResultCheckMode
	// NativeReviewFanoutEnabled resolves whether native PR events may schedule
	// reviews for a repository. The daemon always wires it from [review] config,
	// whose default is false. Nil preserves the legacy enabled behavior for direct
	// Engine constructions that do not participate in host configuration.
	NativeReviewFanoutEnabled func(repo string) bool
	// ReviewBlockingSeverity resolves the least severe review finding that may
	// restart the fix loop for a repository. Nil preserves block-all behavior.
	ReviewBlockingSeverity func(repo string) string
	// FindingsAdvisory resolves the repository's #1969 findings-consumption
	// declaration. nil, or false, means the repository is undeclared or
	// consuming and unobserved obligations keep holding the merge. It mirrors
	// ReviewBlockingSeverity's shape deliberately: one resolver per repository
	// policy, installed by the same wiring, so the two cannot be configured
	// through different paths and disagree.
	FindingsAdvisory func(repo string) bool
	// PostMergeFollowUpIssue files one GitHub issue for a P2 finding from a
	// post-merge review (#2265) and returns its URL. It is wired only in cli,
	// keeping the engine free of the github client; when nil, the follow-up is
	// recorded as a job event instead.
	PostMergeFollowUpIssue func(ctx context.Context, repo, title, body string, labels []string) (url string, err error)
	// The DesignNotes* hooks are the GitHub seams of the weekly design-notes
	// sweep (#2558, SweepDesignNotes). Like PostMergeFollowUpIssue they are
	// wired only in cli. DesignNotesPullRequest reports whether a pull request
	// merged, closed unmerged, or is still open, with its merge commit and the
	// repository's default branch. DesignNotesFindIssue looks up an existing
	// issue by exact title, labelled or not (labels are applied best effort,
	// so an issue may lack them); when nil the sweep relies on
	// its own record alone. DesignNotesCreateIssue opens the week's issue,
	// DesignNotesListComments returns the bodies of an issue's comments (the
	// sweep reconciles against their finding-uid markers before posting), and
	// DesignNotesComment appends one comment. With any required hook nil the
	// sweep files nothing. DesignNotesCompare returns the base...head compare
	// (GitHub's "compare two commits"); the sweep calls it once per merged pull
	// request, from its merge commit to the default branch, to leave out notes
	// the default branch has since outdated. When nil, every note is posted.
	DesignNotesPullRequest  func(ctx context.Context, repo string, number int) (DesignNotesPullRequestState, error)
	DesignNotesFindIssue    func(ctx context.Context, repo, title string) (issue DesignNotesIssue, found bool, err error)
	DesignNotesCreateIssue  func(ctx context.Context, repo, title, body string, labels []string) (DesignNotesIssue, error)
	DesignNotesListComments func(ctx context.Context, repo string, issue int64) (bodies []string, err error)
	DesignNotesComment      func(ctx context.Context, repo string, issue int64, body string) error
	DesignNotesCompare      func(ctx context.Context, repo, base, head string) (github.CompareResult, error)
	// LedgerResolvers is the #1822 ledger's SHARED resolver value. The merge gate
	// holds the SAME value, assigned from one construction in the daemon, so the
	// review brief and the gate cannot compute different obligation sets. Two
	// review rounds wedged on that divergence while three comments asserted the
	// equality, so it is now one field rather than one promise.
	LedgerResolvers LedgerResolvers
	// ReviewChangedFiles resolves the repository-relative files changed from the
	// exact head a reviewer last saw to the current PR head. A scoped follow-up
	// never degrades SILENTLY: when this seam is unavailable or cannot scope the
	// range, HandlePullRequestOpened records a review_scope_unavailable task event
	// and re-reviews the full PR at that head, which re-anchors the prior head so
	// the next round is scoped again.
	ReviewChangedFiles func(ctx context.Context, repo string, pullRequest int, previousHead string, currentHead string) ([]string, error)
}

func (e Engine) block(ctx context.Context, ref taskRef, reason string) error {
	return e.blockTask(ctx, ref, "workflow_blocked", reason, "workflow")
}

// blockTaskPreWriteHook fires between blockTask's state pre-read and its guarded
// write, and between the dirty-worktree block's read and write. It is the seam a
// concurrent merge occupies in production; nil outside tests.
var blockTaskPreWriteHook func(ctx context.Context, taskID string)

// taskStatePreWriteHook fires between setTaskStateResolved's resolution and its
// write, which is the ONLY window in which a test can prove the write itself is
// ownership-bound: an interleaving placed at the barrier is refused by the barrier's
// renewal and never reaches this write at all. Nil in production.
var taskStatePreWriteHook func(ctx context.Context, taskID string)

// blockTask is the single task-blocking choke point. Every durable block
// atomically writes the state and the event that owns it. A stale state
// observation or failed attribution write commits neither half.
func (e Engine) blockTask(ctx context.Context, ref taskRef, kind string, reason string, label string) error {
	task, err := e.resolveTaskState(ctx, ref, TaskBlocked)
	if err != nil {
		return err
	}
	blockErr := BlockedError{Reason: reason}
	if strings.TrimSpace(task.ID) == "" {
		return blockErr
	}
	fromState := ""
	current, err := e.Store.GetTask(ctx, task.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		fromState = current.State
	}
	// blockTaskPreWriteHook is the only place a test can land a merge in the exact
	// post-read/pre-write seam this classification exists for. Nil in production.
	if blockTaskPreWriteHook != nil {
		blockTaskPreWriteHook(ctx, task.ID)
	}
	blockEvent := db.TaskEvent{
		Kind:      kind,
		FromState: fromState,
		Reason:    reason,
	}
	blocked, err := e.Store.BlockTaskWithEvent(ctx, task, blockEvent)
	if err != nil {
		// #1673: a task whose pull request already MERGED must keep that record, and a
		// dead leg's block_parent must still RELEASE its coordinator. The store's
		// atomic guard refuses the write — which is right — but turning that refusal
		// into a hard error would fail the whole poll and strand the coordinator the
		// sweep exists to free.
		//
		// The classification reads the WINNING state, not the pre-read: the pre-read is
		// exactly the value the guard proved stale, so a merge that lands in the
		// post-read/pre-write seam would be classified from a state that no longer
		// exists and hard-fail the poll. Genuinely incompatible non-merged winners
		// still hard-error.
		if errors.Is(err, db.ErrTaskStateConflict) {
			winner := fromState
			if latest, getErr := e.Store.GetTask(ctx, task.ID); getErr == nil {
				winner = latest.State
			} else if !errors.Is(getErr, sql.ErrNoRows) {
				return fmt.Errorf("record %s block for task %s: %w", label, task.ID, err)
			}
			if winner == string(TaskMerged) {
				_ = e.Store.AddTaskEvent(ctx, db.TaskEvent{
					TaskID: task.ID,
					Kind:   TaskEventMergedRegressionRefused,
					Reason: fmt.Sprintf("refused %s -> %s: the pull request already merged, so the landed-work record is kept; the %s advance that requested it continues",
						TaskMerged, TaskBlocked, label),
				})
				return blockErr
			}
		}
		return fmt.Errorf("record %s block for task %s: %w", label, task.ID, err)
	}
	if !blocked {
		return fmt.Errorf("record %s block for task %s: state transition did not commit", label, task.ID)
	}
	return blockErr
}

func (e Engine) setTaskState(ctx context.Context, ref taskRef, state TaskState) error {
	_, err := e.setTaskStateResolved(ctx, ref, state)
	return err
}

// setTaskStateResolved returns the task row actually advanced. A branch ref can
// resolve to an existing canonical task.
func (e Engine) setTaskStateResolved(ctx context.Context, ref taskRef, state TaskState) (string, error) {
	task, err := e.resolveTaskState(ctx, ref, state)
	if err != nil || strings.TrimSpace(task.ID) == "" {
		return "", err
	}
	// persistTaskStateOwned, not a plain upsert: `planned` and `awaiting_human` are
	// merged-regression targets too (blocked now goes through the store's atomic
	// BlockTaskWithEvent), and every target must refuse to overwrite a disposed row.
	// Both exclusions live on the write, so a second daemon cannot win the race a
	// pre-read would lose.
	if taskStatePreWriteHook != nil {
		taskStatePreWriteHook(ctx, task.ID)
	}
	if _, err := persistTaskStateOwned(ctx, e.Store, task, state); err != nil {
		return "", err
	}
	return task.ID, nil
}

// resolveTaskState preserves the branch-canonical identity rule without writing,
// allowing all blocked transitions to use Store.BlockTaskWithEvent.
func (e Engine) resolveTaskState(ctx context.Context, ref taskRef, state TaskState) (db.Task, error) {
	if strings.TrimSpace(ref.ID) == "" {
		return db.Task{}, nil
	}
	task := db.Task{
		ID:           ref.ID,
		RepoFullName: ref.Repo,
		GoalID:       ref.GoalID,
		Title:        ref.Title,
		State:        string(state),
		Branch:       ref.Branch,
	}
	existing, err := e.Store.GetTask(ctx, ref.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return db.Task{}, err
	}
	if err == nil {
		if IsDisposedTaskState(existing.State) && existing.State != string(state) {
			return db.Task{}, fmt.Errorf("task %s is %s; workflow advancement cannot move it to %s", existing.ID, existing.State, state)
		}
		// The merged-regression refusal is NOT here: an advisory check on this read
		// loses the race a second daemon can win between the read and the write, so
		// it lives on the write itself (writeTaskState).
		if task.GoalID == "" {
			task.GoalID = existing.GoalID
		}
		if task.RepoFullName == "" {
			task.RepoFullName = existing.RepoFullName
		}
		if task.Title == "" {
			task.Title = existing.Title
		}
		if task.Branch == "" {
			task.Branch = existing.Branch
		}
	}
	// One task per (repo, branch) is enforced by the tasks(repo_full_name, branch)
	// partial-unique index. If this ref carries a non-empty branch already owned by a
	// different task, advance the branch's canonical row instead of inserting a
	// duplicate.
	if task.Branch != "" {
		byBranch, berr := e.Store.GetTaskByRepoBranch(ctx, task.RepoFullName, task.Branch)
		if berr != nil && !errors.Is(berr, sql.ErrNoRows) {
			return db.Task{}, berr
		}
		if berr == nil && byBranch.ID != task.ID {
			if IsDisposedTaskState(byBranch.State) && byBranch.State != string(state) {
				return db.Task{}, fmt.Errorf("task %s is %s; workflow advancement cannot move it to %s", byBranch.ID, byBranch.State, state)
			}
			byBranch.State = string(state)
			return byBranch, nil
		}
	}
	return task, nil
}

// persistTaskStateOwned is the guarded write point for automated task-state
// advancement (#1673). Every exclusion it enforces lives on the conflict UPDATE,
// so the advisory pre-reads in resolveTaskState cannot be invalidated between the
// read and the write by another daemon — or by the same daemon earlier in the same
// poll.
//
// Two exclusion families, refused differently because they mean different things:
//
//	DISPOSED (dismissed/superseded/stranded) is an explicit operator or audit
//	disposition. Automation never moves a task out of one, so a refusal is an
//	ERROR naming the state — the same answer resolveTaskState's pre-read gives,
//	now unwinnable by a race. Writing the SAME disposed state is permitted, so a
//	disposal stays idempotent.
//
//	MERGED, for a target that would claim the work is not done
//	(IsMergedWorkRegressionTarget), is refused SILENTLY with a durable
//	TaskEventMergedRegressionRefused trace: the failure policy that requested it
//	still has to release its coordinator, so this must not become an error.
//
// `blocked` reaches the store through BlockTaskWithEvent, which carries its own
// terminal-state conflict check; this covers the remaining regression targets
// (`planned`, `awaiting_human`) and every ordinary advancement.
func persistTaskStateOwned(ctx context.Context, store *db.Store, task db.Task, state TaskState) (bool, error) {
	task.State = string(state)
	forbidden, mergedGuarded := taskStateWriteExclusions(state)
	written, err := store.UpsertTaskUnlessStates(ctx, task, forbidden)
	if err != nil {
		return false, err
	}
	if written {
		return true, nil
	}
	return false, classifyRefusedTaskStateWrite(ctx, store, task, state, mergedGuarded)
}

// taskStateWriteExclusions is the shared exclusion set every automated task-state
// write carries: never overwrite a disposed row, and never regress landed work.
func taskStateWriteExclusions(state TaskState) (forbidden []string, mergedGuarded bool) {
	forbidden = make([]string, 0, 4)
	for _, disposed := range []TaskState{TaskDismissed, TaskSuperseded, TaskStranded} {
		if disposed != state {
			forbidden = append(forbidden, string(disposed))
		}
	}
	mergedGuarded = IsMergedWorkRegressionTarget(string(state))
	if mergedGuarded {
		forbidden = append(forbidden, string(TaskMerged))
	}
	return forbidden, mergedGuarded
}

// classifyRefusedTaskStateWrite reads the state that actually WON and answers the
// two families differently: a disposed row is a hard error, landed work is a silent
// refusal with a durable trace. Only the row can say which one refused.
func classifyRefusedTaskStateWrite(ctx context.Context, store *db.Store, task db.Task, state TaskState, mergedGuarded bool) error {
	existing, getErr := store.GetTask(ctx, task.ID)
	if getErr != nil {
		return getErr
	}
	if IsDisposedTaskState(existing.State) && existing.State != string(state) {
		return fmt.Errorf("task %s is %s; automated advancement cannot move it to %s", task.ID, existing.State, state)
	}
	if mergedGuarded {
		_ = store.AddTaskEvent(ctx, db.TaskEvent{
			TaskID: task.ID,
			Kind:   TaskEventMergedRegressionRefused,
			Reason: fmt.Sprintf("refused %s -> %s: the pull request already merged, so the landed-work record is kept; the operation that requested it continues",
				TaskMerged, state),
		})
	}
	return nil
}

func (e Engine) jobID(request JobRequest) string {
	if e.JobID != nil {
		return e.JobID(request)
	}
	hash := fnv.New64a()
	for _, value := range []string{
		request.Repo,
		request.Branch,
		strconv.Itoa(request.PullRequest),
		request.TaskID,
		request.Agent,
		request.Action,
		request.ReviewRound,
		request.Instructions,
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return "workflow-" + strconv.FormatUint(hash.Sum64(), 36)
}

type taskRef struct {
	ID     string
	Repo   string
	GoalID string
	Title  string
	Branch string
}

func taskRefFromPullRequest(event PullRequestEvent) taskRef {
	return taskRef{
		ID:     event.TaskID,
		Repo:   event.Repo,
		GoalID: event.GoalID,
		Title:  event.TaskTitle,
		Branch: event.Branch,
	}
}

func taskRefFromPayload(payload JobPayload) taskRef {
	return taskRef{
		ID:     payload.TaskID,
		Repo:   payload.Repo,
		GoalID: payload.GoalID,
		Title:  payload.TaskTitle,
		Branch: payload.Branch,
	}
}

// firstNonEmptyString returns the first non-blank value, or "".
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
