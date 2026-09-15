package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Review router (#2171). A seat names the pull request; Gitmoot decides the
// reviewer, runtime and model, deduplicates concurrent requests on the exact
// head, and wakes the requester through the durable awaited-fact channel when
// the verdict is saved. The reviewing job itself is an ordinary review job, so
// every existing gate, evidence and recovery rule applies unchanged.

const (
	reviewRequestExecutionPath = "review_request"
	defaultReviewRequestTTL    = 12 * time.Hour
	// reviewRequestDispatchWindow bounds how long a claim may hold no readable
	// job row before another requester may take it over. Dispatch does real work
	// (read-only worktree allocation, runtime preflight, GitHub reads) before
	// Mailbox.Enqueue, so a claim younger than this is presumed live. It is a
	// LIVENESS bound, not a timeout: exceeding it only permits takeover once the
	// job row is still absent, which means Enqueue never happened.
	reviewRequestDispatchWindow = 10 * time.Minute
)

// reviewRequestState names what the router did with one request. It is part
// of the CLI surface: a requester reads it to know whether anything was spent.
const (
	reviewRequestDispatched    = "dispatched"
	reviewRequestAttached      = "attached"
	reviewRequestVerdictExists = "verdict_exists"
)

type reviewRequestOutput struct {
	State       string   `json:"state"`
	Repo        string   `json:"repo"`
	PullRequest int      `json:"pull_request"`
	HeadSHA     string   `json:"head_sha"`
	Purpose     string   `json:"purpose"`
	Requester   string   `json:"requester"`
	JobID       string   `json:"job_id"`
	JobState    string   `json:"job_state,omitempty"`
	Reviewer    string   `json:"reviewer,omitempty"`
	Model       string   `json:"model,omitempty"`
	ModelPool   []string `json:"model_pool,omitempty"`
	Verdict     string   `json:"verdict,omitempty"`
	// Baseline is the prior head a delta review was bounded to; BaselineSkipped
	// names why a full review was dispatched instead (#2177). Exactly one is set
	// on a dispatch, and neither on an attach or a reused verdict.
	Baseline        string   `json:"baseline,omitempty"`
	BaselineSkipped string   `json:"baseline_skipped,omitempty"`
	AwaitedFactID   int64    `json:"awaited_fact_id"`
	NotifyBy        string   `json:"notify_by"`
	Holds           []string `json:"holds,omitempty"`
	WatchCommand    string   `json:"watch_command,omitempty"`
}

func runReview(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printReviewUsage(stdout)
		return 0
	}
	switch args[0] {
	case "request":
		return runReviewRequest(args[1:], stdout, stderr)
	case "status":
		return runReviewStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown review subcommand %q\n\n", args[0])
		printReviewUsage(stderr)
		return 2
	}
}

func printReviewUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot review request --pr NUMBER [--repo OWNER/REPO] [--head SHA] [--branch NAME] [--purpose code|security|ui|architecture] [--role ROLE] [--ttl DURATION] [--reviewer AGENT] [--runtime NAME] [--full] [--allow-prompt-head-mismatch] [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot review status --pr NUMBER [--repo OWNER/REPO] [--json] [--home DIR]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "request routes one independent review of the pull request's current (or --head) commit.")
	fmt.Fprintln(w, "Gitmoot picks a review-capable agent, runs it on omp with the [review_router] model pool")
	fmt.Fprintln(w, "for the purpose, falls back to the next model on a provider quota/auth failure, and")
	fmt.Fprintln(w, "wakes --role (default $GITMOOT_ORG_ROLE) when the verdict is saved. A second request for")
	fmt.Fprintln(w, "the same head attaches to the running review or to its saved verdict; it never spends")
	fmt.Fprintln(w, "another reviewer. No verdict ever triggers a model change.")
}

type reviewRequestOptions struct {
	home                    string
	repo                    string
	pr                      int
	head                    string
	branch                  string
	purpose                 string
	role                    string
	ttl                     time.Duration
	reviewer                string
	full                    bool
	allowPromptHeadMismatch bool
	// runtime is the operator escape from the omp pin below (#2180). A pinned
	// runtime with no override is a dead end whenever an unavailability hold is
	// written for that runtime: the caller has no second choice to reach for.
	runtime string
	json    bool
}

func runReviewRequest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review request", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts reviewRequestOptions
	fs.StringVar(&opts.home, "home", "", "home directory to use instead of the current user's home")
	fs.StringVar(&opts.repo, "repo", "", "repository in owner/repo form (defaults to the current checkout)")
	fs.IntVar(&opts.pr, "pr", 0, "pull request number")
	fs.StringVar(&opts.head, "head", "", "exact 40-hex head to review (defaults to the pull request's current head)")
	fs.StringVar(&opts.branch, "branch", "", "pull request head branch (defaults to the pull request's branch)")
	fs.StringVar(&opts.purpose, "purpose", "code", "review purpose: code, security, ui, or architecture")
	fs.StringVar(&opts.role, "role", "", "requesting organization role notified with the verdict (defaults to GITMOOT_ORG_ROLE)")
	fs.DurationVar(&opts.ttl, "ttl", defaultReviewRequestTTL, "how long the requester waits before the wait expires to its parent")
	fs.StringVar(&opts.reviewer, "reviewer", "", "registered review agent to use instead of the router's choice")
	fs.StringVar(&opts.runtime, "runtime", "", "override the runtime this review dispatches on (default omp)")
	fs.BoolVar(&opts.allowPromptHeadMismatch, "allow-prompt-head-mismatch", false, "dispatch even when a carried finding cites a commit outside this pull request's history")
	fs.BoolVar(&opts.full, "full", false, "review the full diff against the PR base even when a prior verdict at an ancestor head could bound the review")
	fs.BoolVar(&opts.json, "json", false, "print the request as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || opts.pr <= 0 {
		fmt.Fprintln(stderr, "review request requires --pr NUMBER")
		return 2
	}
	if opts.ttl <= 0 {
		fmt.Fprintln(stderr, "review request: --ttl must be positive")
		return 2
	}
	opts.purpose = strings.ToLower(strings.TrimSpace(opts.purpose))
	opts.role = strings.ToLower(strings.TrimSpace(opts.role))
	if opts.role == "" {
		opts.role = strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	}
	if opts.role == "" {
		fmt.Fprintln(stderr, "review request: set --role or GITMOOT_ORG_ROLE; the verdict is delivered to that organization role")
		return 2
	}
	var output reviewRequestOutput
	err := withStore(opts.home, func(store *db.Store) error {
		var err error
		output, err = requestReview(context.Background(), store, opts, stderr)
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "review request: %v\n", err)
		return 1
	}
	if opts.json {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "review request: %v\n", err)
			return 1
		}
		return 0
	}
	printReviewRequestOutput(stdout, output)
	return 0
}

// effectiveReviewRuntime is the runtime the review will RUN on, which is not
// always the value written to the request: the router emits no override when
// its choice is the agent's own runtime. Model gating must follow the former.
func effectiveReviewRuntime(override, selected string) string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(selected); trimmed != "" {
		return trimmed
	}
	return runtime.OmpRuntime
}

// reviewRequestRuntime applies the router's omp pin unless the operator named a
// runtime. One site for the pin, so a future caller cannot lose the override.
func reviewRequestRuntime(override, selected, registered string) string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return trimmed
	}
	// NO OVERRIDE WHEN THE CHOICE IS THE AGENT'S OWN RUNTIME (#2189 round 2).
	// Setting one is not merely redundant: an override must be expressible as
	// `--runtime X`, and a runtime whose sessions are commands cannot be, so a
	// shell candidate the router legitimately selected was refused with
	// "--runtime shell requires --session". The agent already runs on it.
	if trimmed := strings.TrimSpace(selected); trimmed != "" {
		if trimmed == strings.TrimSpace(registered) {
			return ""
		}
		return trimmed
	}
	return runtime.OmpRuntime
}

func requestReview(ctx context.Context, store *db.Store, opts reviewRequestOptions, stderr io.Writer) (reviewRequestOutput, error) {
	paths, err := pathsFromFlag(opts.home)
	if err != nil {
		return reviewRequestOutput{}, err
	}
	routerSettings, err := config.LoadReviewRouterSettings(paths)
	if err != nil {
		return reviewRequestOutput{}, err
	}
	pool, err := routerSettings.Models(opts.purpose)
	if err != nil {
		return reviewRequestOutput{}, err
	}
	orgConfig, err := config.LoadOrg(paths)
	if err != nil {
		return reviewRequestOutput{}, err
	}
	if _, ok := orgConfig.Role(opts.role); !ok {
		return reviewRequestOutput{}, fmt.Errorf("unknown organization role %q: the requester must be a registered role so the verdict can be delivered", opts.role)
	}
	// One runner for the whole request, resolved from the configured execution
	// backend before anything touches git: repo resolution now reads the remote
	// (#2146), and probing it from this host under a remote backend would answer
	// about the wrong machine.
	execBackend, err := localAgentDispatchExecBackendFor(opts.home)
	if err != nil {
		return reviewRequestOutput{}, err
	}
	runner, err := jobSubprocessRunnerForBackend(execBackend)
	if err != nil {
		return reviewRequestOutput{}, err
	}
	repo, record, _, _, err := resolveLocalAgentRepo(ctx, store, opts.repo, runner)
	if err != nil {
		return reviewRequestOutput{}, err
	}
	head := strings.ToLower(strings.TrimSpace(opts.head))
	branch := strings.TrimSpace(opts.branch)
	if head == "" || branch == "" {
		pr, err := jobGitHubClient(record.CheckoutPath, newAgentDispatchGitHubClient(record.CheckoutPath), runner).GetPullRequest(ctx, repo, int64(opts.pr))
		if err != nil {
			return reviewRequestOutput{}, fmt.Errorf("resolve pull request #%d: %w", opts.pr, err)
		}
		if head == "" {
			head = strings.ToLower(strings.TrimSpace(pr.HeadSHA))
		}
		if branch == "" {
			branch = strings.TrimSpace(pr.HeadRef)
		}
	}
	if err := dispatchHeadSHAError(head); err != nil {
		return reviewRequestOutput{}, err
	}
	subjectKey, err := db.ReviewRequestSubjectKey(repo.FullName(), opts.pr, head, opts.purpose)
	if err != nil {
		return reviewRequestOutput{}, err
	}
	output := reviewRequestOutput{
		Repo:        repo.FullName(),
		PullRequest: opts.pr,
		HeadSHA:     head,
		Purpose:     opts.purpose,
		Requester:   opts.role,
		ModelPool:   pool,
		NotifyBy:    "awaited fact wake to " + opts.role,
		Holds:       reviewRequestHolds(paths, opts.home),
	}
	// The claim is taken on a job id minted here. The claim row — NOT the jobs
	// row — is the mutual exclusion: dispatch takes seconds (worktree allocation,
	// runtime preflight) before Mailbox.Enqueue inserts the job, so during that
	// window the winner's job id is not readable. A loser that treats "job row
	// absent" as "claim is dead" steals the claim and both requesters dispatch,
	// which is the duplicate this table exists to prevent.
	jobID := localAgentJobID("review", "review-router")
	claim, won, err := store.ClaimReviewRequest(ctx, subjectKey, jobID, opts.purpose, opts.role, reviewRequestOwner())
	if err != nil {
		return reviewRequestOutput{}, err
	}
	if !won {
		attach, takeover, err := resolveLostReviewClaim(ctx, store, claim, time.Now().UTC())
		if err != nil {
			return reviewRequestOutput{}, err
		}
		if !takeover {
			return finishReviewAttach(ctx, store, output, attach, claim, opts)
		}
		// The holder is provably gone: its job ended without a verdict, or it
		// never enqueued one and its claim has aged past the dispatch window.
		// Move the claim; a concurrent requester that wins this CAS instead is
		// the one that dispatches, and this caller attaches to its job.
		moved, err := store.ReplaceReviewRequestJob(ctx, subjectKey, claim.JobID, jobID, opts.role, reviewRequestOwner())
		if err != nil {
			return reviewRequestOutput{}, err
		}
		if !moved {
			current, err := store.GetReviewRequest(ctx, subjectKey)
			if err != nil {
				return reviewRequestOutput{}, err
			}
			attach, takeover, err := resolveLostReviewClaim(ctx, store, current, time.Now().UTC())
			if err != nil || takeover {
				return reviewRequestOutput{}, fmt.Errorf("review request for %s is contended: the claim moved to job %s while this request was resolving; re-run it", subjectKey, current.JobID)
			}
			return finishReviewAttach(ctx, store, output, attach, current, opts)
		}
	}
	reviewer, selectedRuntime, err := selectReviewRouterAgent(ctx, store, repo.FullName(), opts.pr, head, opts.purpose, opts.reviewer, opts.role)
	if err != nil {
		releaseUnenqueuedReviewClaim(ctx, store, subjectKey, jobID)
		return reviewRequestOutput{}, err
	}
	// Resolved only once the claim is won and a reviewer exists, so a losing or
	// attaching request never pays for git calls whose answer it will discard.
	var scope *workflow.ReviewScope
	if opts.full {
		output.BaselineSkipped = "flag --full"
	} else {
		scope, output.BaselineSkipped = resolveDeltaReviewScope(ctx, store, jobGitClient(record.CheckoutPath, runner), repo.FullName(), opts.pr, head, opts.purpose)
		if scope != nil {
			output.Baseline = scope.PreviousHeadSHA
		}
	}
	request := localAgentDispatchRequest{
		RepoFlag:     repo.FullName(),
		Agent:        reviewer.Name,
		Action:       "review",
		Instructions: reviewRouterInstructions(opts.purpose, repo.FullName(), opts.pr, head, scope),
		Background:   true,
		// Gated on the EFFECTIVE runtime, not on whether an override was
		// emitted: a review running on the agent's own non-omp runtime must not
		// carry an omp pool model either.
		Model: reviewModelForRuntime(effectiveReviewRuntime(opts.runtime, selectedRuntime), pool),
		// The router SELECTS the reviewer, so it also chooses the runtime: omp
		// unless the operator names another. The escape matters because a pinned
		// runtime with no override is refused outright whenever an availability
		// hold is written for that runtime (#2181), with nothing to fall back to.
		Runtime:              reviewRequestRuntime(opts.runtime, selectedRuntime, reviewer.Runtime),
		ActingOrgRole:        opts.role,
		OperatorOrigin:       true,
		Home:                 opts.home,
		PullRequest:          opts.pr,
		HeadSHA:              head,
		Branch:               branch,
		NoFixTarget:          true,
		SelectedAction:       "review",
		SelectedActionReason: "review router " + opts.purpose,
		ExecutionPath:        reviewRequestExecutionPath,
		JobID:                jobID,
		ReviewPurpose:        opts.purpose,
		ReviewModelPool:      pool,
		ReviewRequester:      opts.role,
		ReviewScope:          scope,
		// A carried finding may legitimately cite a commit outside this PR
		// (#2178 round 1): the citation classifier scans the delta brief, and
		// the refusal it prints names this flag, so the flag must exist here.
		AllowPromptHeadMismatch: opts.allowPromptHeadMismatch,
		DispatchWarning: func(warning string) {
			fmt.Fprintf(stderr, "review request: warning: %s\n", warning)
		},
	}
	dispatched, err := dispatchLocalAgentJobFromCLI(ctx, store, request)
	if err != nil {
		releaseUnenqueuedReviewClaim(ctx, store, subjectKey, jobID)
		return reviewRequestOutput{}, err
	}
	output.State = reviewRequestDispatched
	output.JobID = dispatched.JobID
	output.JobState = dispatched.State
	output.Reviewer = reviewer.Name
	output.Model = pool[0]
	output.WatchCommand = jobWatchCommand(dispatched.JobID, opts.home)
	if err := subscribeReviewRequester(ctx, store, &output, opts); err != nil {
		return reviewRequestOutput{}, err
	}
	return output, nil
}

// reviewJobStillAnswers reports whether an earlier claimed job can still answer
// the request: it is queued or running, it is blocked awaiting a human, it
// delegated the answer to children that have not finished, or it saved a
// verdict. A stored result is NOT enough on its own: the daemon's dead-runtime
// recovery writes a synthetic `failed` result, and an agent may report `failed`
// itself, and neither answers "is this head acceptable".
//
// THE FAN-OUT ARM IS LOAD-BEARING ON A STAGED-REVIEW REPO (#2172 review round
// 2, interacting with #2029 which merged the same day). Where
// staged_review_verdict_agent is configured the routed job becomes a PREFLIGHT
// whose own result is a fan-out announcement, not a verdict. Judged on its own
// row it looks finished-without-a-verdict, so the claim became takeover-
// eligible while the verdict child was still running and a second reviewer was
// dispatched. The question is answered by the tree, so the tree is what must be
// consulted.
// reviewClaimSubject is the QUESTION a claim was minted for. #2176: the walk
// was subject-blind, so a review job under a non-review leg answering a
// DIFFERENT pull request or head counted as answering this claim - and because
// a stored verdict never stops answering, the claim pinned forever while the
// awaited fact, keyed to this exact head and purpose, could never be satisfied
// by it. Scoping fix 2 to the subject is what keeps widening the walk safe.
type reviewClaimSubject struct {
	repo        string
	pullRequest int
	headSHA     string
	purpose     string
}

func reviewSubjectFromClaim(claim db.ReviewRequest) (reviewClaimSubject, bool) {
	repo, pullRequest, headSHA, err := db.ParseReviewVerdictSubjectKey(claim.SubjectKey)
	if err != nil {
		return reviewClaimSubject{}, false
	}
	purpose := strings.ToLower(strings.TrimSpace(claim.Purpose))
	if purpose == "" {
		purpose = db.ReviewVerdictKeyPurpose(claim.SubjectKey)
	}
	return reviewClaimSubject{repo: repo, pullRequest: pullRequest, headSHA: headSHA, purpose: purpose}, true
}

// answers reports whether a job's payload addresses this exact question.
//
// An UNKNOWN subject answers NOTHING. A claim key is always written by
// ReviewRequestSubjectKey, so an unparseable one is corruption - and the #2176
// lesson is that construction holds while corruption is undefended. Disabling
// the check there would let any verdict in the tree keep the claim, which is
// the permanent wedge this fix exists to remove. Failing toward release costs
// at most one duplicate reviewer; the claim holder itself is exempt and keeps
// its own claim either way.
func (s reviewClaimSubject) answers(payload workflow.JobPayload) bool {
	if s.repo == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(payload.Repo), s.repo) || payload.PullRequest != s.pullRequest {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(payload.HeadSHA), s.headSHA) {
		return false
	}
	got := strings.ToLower(strings.TrimSpace(payload.ReviewPurpose))
	if got == "" {
		got = db.DefaultReviewPurpose
	}
	want := s.purpose
	if want == "" {
		want = db.DefaultReviewPurpose
	}
	return got == want
}

func reviewJobStillAnswers(ctx context.Context, store *db.Store, job db.Job, subject reviewClaimSubject, now time.Time) bool {
	return reviewJobStillAnswersWithin(ctx, store, job, subject, now, map[string]bool{}, true)
}

// reviewJobStillAnswersWithin carries the visited set. Round 3 established the
// tree is acyclic BY CONSTRUCTION - parent_job_id is insert-only and depth is
// capped - and round 4 refined that: construction holds, corruption is
// undefended, and a hand-edited parent_job_id cycle exhausted the stack. A
// visited job is treated as no answer, which is the same direction the walk
// already takes for a child that cannot answer.
func reviewJobStillAnswersWithin(ctx context.Context, store *db.Store, job db.Job, subject reviewClaimSubject, now time.Time, seen map[string]bool, isClaimHolder bool) bool {
	if seen[job.ID] {
		return false
	}
	seen[job.ID] = true
	payload, payloadErr := workflow.ParseJobPayload(job.Payload)

	// ANSWERING AND TRAVERSING ARE DIFFERENT QUESTIONS, and #2176 round 2 caught
	// me applying that principle to non-review legs while denying it to review
	// legs. A job may answer only if it is a REVIEW of THIS subject; the claim
	// holder is exempt because the claim was minted for it. Everything else is
	// still walked, because any job can be the parent of one that answers - a
	// foreign review leg (another PR, another head) can fan out a child that
	// reviews exactly this head, and pruning its subtree released the claim and
	// dispatched a duplicate while that child worked.
	canAnswer := isClaimHolder || (reviewTypedJob(job) && payloadErr == nil && subject.answers(payload))

	if canAnswer {
		switch job.State {
		case string(workflow.JobQueued), string(workflow.JobRunning), string(workflow.JobBlocked):
			return true
		}
	}
	if payloadErr != nil {
		return false
	}
	if canAnswer && reviewVerdictDecision(job, payload) != "" {
		return true
	}
	// TRAVERSAL IS GATED ON CHILDREN, NOT ON A VERDICT CLASSIFIER (#2176 round
	// 3). This asked ResultIsFanOut, which requires a terminal REVIEW decision
	// (approved / changes_requested) - but dispatchDelegations runs for any
	// decision that is not blocked or failed, and a non-pipeline result keeps
	// its delegations. So an implement or ask leg that succeeded with, say,
	// decision "implemented" has REAL children that were never listed, and a
	// live matching review grandchild under it was invisible. Whether a node
	// has children is a question about the store, so ask the store.
	// NO SKIP HERE, DELIBERATELY. The obvious optimisation - a node with no
	// stored result never dispatched delegations, so do not list its children -
	// is FALSE, and I proved it false on my own fixture before shipping it: a
	// leg with a nil result and real child rows had its whole subtree pruned,
	// which is the same defect class this walk has produced four times. Whether
	// a node has children is a question about the store; every cheaper proxy
	// for it has been wrong so far. The cost is bounded by the delegation cap
	// and the seen-set, and a bounded query cost is worth less than a claim.
	children, err := store.ListJobsByParent(ctx, job.ID)
	if err != nil {
		// Unknown is not proof the tree is finished; failing closed costs a
		// re-run, failing open costs a duplicate reviewer.
		return true
	}
	if resultAwaitsUnbornChildren(payload.Result, len(children)) {
		// Their subject is unknowable until they appear - EXCEPT when this node
		// already declares a different one. #2176 round 3 measured the cost of
		// ignoring that: a continuously active FOREIGN subtree renewed the hold
		// indefinitely, because the bound was foreign activity rather than this
		// claim's clock. A node that names another subject does not get to hold
		// this claim on the strength of children it has not created.
		if subjectIsForeign(subject, payload) {
			return false
		}
		if fanOutChildrenMayStillArrive(job, now) {
			return true
		}
	}
	for _, child := range children {
		if reviewJobStillAnswersWithin(ctx, store, child, subject, now, seen, false) {
			return true
		}
	}
	return false
}

// resultAwaitsUnbornChildren reports whether a stored result promised children
// that do not exist yet. It is a STRUCTURAL question and must never be asked of
// a verdict classifier: #2176 round 4 caught ResultIsFanOut - which requires a
// terminal review decision of approved or changes_requested - still gating this
// branch after traversal had been freed from it. The conflation had moved one
// level down rather than left. Two ways to await children:
//
//   - nothing has landed yet, and the result announced a fan-out at all;
//   - some children landed but FEWER than the result declared, which is the
//     deps-deferred shape: a deferred delegation creates no row until its deps
//     succeed, so a sibling's existence must not cancel the grace.
func resultAwaitsUnbornChildren(result *workflow.AgentResult, existing int) bool {
	if result == nil {
		return false
	}
	if declared := len(result.Delegations); declared > existing {
		return true
	}
	return existing == 0 && (result.FanOut || len(result.Delegations) > 0)
}

// subjectIsForeign reports whether a node DECLARES a subject other than the
// claim's. Unset fields are not foreign - a coordinator leg that names no repo
// or pull request tells us nothing, and treating silence as foreign would prune
// the traversal this round's P2 exists to keep open.
func subjectIsForeign(subject reviewClaimSubject, payload workflow.JobPayload) bool {
	if subject.repo == "" {
		return false
	}
	if repo := strings.TrimSpace(payload.Repo); repo != "" && !strings.EqualFold(repo, subject.repo) {
		return true
	}
	if payload.PullRequest > 0 && payload.PullRequest != subject.pullRequest {
		return true
	}
	if head := strings.TrimSpace(payload.HeadSHA); head != "" && !strings.EqualFold(head, subject.headSHA) {
		return true
	}
	return false
}

// fanOutChildrenMayStillArrive bounds the not-yet-enqueued grace (#2176 F1).
// Dispatch writes the parent's announcement before the children rows exist, so
// an empty list can mean "too early to tell" - but granting that unconditionally
// pinned the claim FOREVER for shapes where children never arrive at all: a
// refused staged preflight is rewritten to failed with its fan-out result kept
// verbatim, and a succeeded marked preflight whose advance is permanently
// refused sits in the same state. Two bounds, both required:
//
//   - a fan-out that is not SUCCEEDED will never enqueue children, so it gets no
//     grace at all;
//   - a succeeded one gets the dispatch window, after which the claim releases.
//     Failing open costs one duplicate reviewer; pinning forever costs every
//     future review of that head, which is strictly worse.
func fanOutChildrenMayStillArrive(job db.Job, now time.Time) bool {
	if strings.TrimSpace(job.State) != string(workflow.JobSucceeded) {
		return false
	}
	stamp := strings.TrimSpace(job.UpdatedAt)
	if stamp == "" {
		return false
	}
	updated, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		updated, err = time.Parse("2006-01-02 15:04:05", stamp)
		if err != nil {
			return false
		}
	}
	return now.Sub(updated) <= reviewRequestDispatchWindow
}

// reviewTypedJob reports whether a job answers a REVIEW question. The job type
// carries the delegation's Action verbatim (session_job.go), so this is the
// same boundary the awaited-fact producer uses: anything else cannot satisfy a
// review verdict subscription and must not be read as one.
func reviewTypedJob(job db.Job) bool {
	return strings.EqualFold(strings.TrimSpace(job.Type), "review")
}

// reviewVerdictDecision returns the saved verdict, or "" when the stored result
// is not one (absent, fan-out announcement, failed, blocked, skipped).
//
// It also requires the JOB to have succeeded. A failed or cancelled job can
// carry a stored verdict, and honouring it pinned the claim forever while
// reporting verdict_exists to every later requester — yet the awaited fact is
// only satisfied from a SUCCEEDED transition, so the waiter timed out against a
// verdict the router said existed (#2172 review round 2). The two must agree,
// and the producer's rule is the authority.
func reviewVerdictDecision(job db.Job, payload workflow.JobPayload) string {
	if strings.TrimSpace(job.State) != string(workflow.JobSucceeded) {
		return ""
	}
	if payload.Result == nil || workflow.ResultIsFanOut(payload.Result) {
		return ""
	}
	switch decision := strings.ToLower(strings.TrimSpace(payload.Result.Decision)); decision {
	case "approved", "changes_requested":
		return decision
	}
	return ""
}

// releaseUnenqueuedReviewClaim frees a claim ONLY when its job was never
// enqueued. dispatchLocalAgentJobFromCLI can fail AFTER Mailbox.Enqueue commits
// — a post-enqueue step erroring on an already-queued review — and releasing
// then would leave a live job with no claim, so the next requester dispatches a
// second reviewer for the same head. Absence of the row is the only safe
// evidence; any read error leaves the claim in place, costing at most one
// dispatch-window wait.
func releaseUnenqueuedReviewClaim(ctx context.Context, store *db.Store, subjectKey, jobID string) {
	if _, err := store.GetJob(ctx, jobID); !errors.Is(err, sql.ErrNoRows) {
		return
	}
	_ = store.ReleaseReviewRequest(ctx, subjectKey, jobID)
}

// reviewRequestOwner identifies this process as the claim holder.
func reviewRequestOwner() db.ReviewRequestOwner {
	pid := os.Getpid()
	return db.ReviewRequestOwner{
		PID:          pid,
		PIDStartTime: workflow.RuntimeProcessIdentity(pid),
		BootID:       db.BootID(),
	}
}

// resolveLostReviewClaim answers what a requester that LOST the claim should do.
// It returns the job to attach to (zero when the holder is still dispatching and
// has no job row yet) and whether the claim may be taken over.
//
// Takeover requires PROOF the holder is gone, never an absence a live dispatch
// also produces:
//   - job row readable and still answering  -> attach, no takeover
//   - job row readable and dead without a verdict -> takeover
//   - job row absent, holder process PROVABLY DEAD (same boot, recorded
//     starttime identity gone) -> takeover
//   - job row absent, holder recorded on a DIFFERENT boot -> takeover: the host
//     restarted, so that dispatch cannot still be running
//   - job row absent, holder alive or liveness unknowable -> attach, no takeover
//
// THE AGE BOUND IS A FALLBACK, NOT THE RULE (#2172 review round 2). A clock in
// front of the original race is still that race: a dispatch stalled on a cold
// PR-ref fetch, or stopped by a signal, is SLOW rather than dead, and stealing
// its claim dispatches a second reviewer. The bound now applies only where
// liveness cannot be evaluated at all — a claim written by a binary that
// recorded no owner, or a host with no readable process table.
func resolveLostReviewClaim(ctx context.Context, store *db.Store, claim db.ReviewRequest, now time.Time) (db.Job, bool, error) {
	job, err := store.GetJob(ctx, claim.JobID)
	switch {
	case err == nil:
		subject, _ := reviewSubjectFromClaim(claim)
		return job, !reviewJobStillAnswers(ctx, store, job, subject, now), nil
	case !errors.Is(err, sql.ErrNoRows):
		return db.Job{}, false, err
	}
	if boot, current := strings.TrimSpace(claim.OwnerBootID), db.BootID(); boot != "" && current != "" {
		if boot != current {
			return db.Job{}, true, nil
		}
		if live, known := workflow.RuntimeProcessLiveness(claim.OwnerPID, claim.OwnerPIDStartTime); known {
			return db.Job{}, !live, nil
		}
	}
	claimed, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(claim.UpdatedAt))
	if parseErr != nil {
		// An unreadable timestamp must not authorize a takeover: failing closed
		// costs one re-run, failing open costs a duplicate reviewer.
		return db.Job{}, false, nil
	}
	return db.Job{}, now.Sub(claimed) > reviewRequestDispatchWindow, nil
}

func finishReviewAttach(ctx context.Context, store *db.Store, output reviewRequestOutput, job db.Job, claim db.ReviewRequest, opts reviewRequestOptions) (reviewRequestOutput, error) {
	output.State = reviewRequestAttached
	output.JobID = firstNonEmpty(job.ID, claim.JobID)
	output.JobState = job.State
	output.Reviewer = job.Agent
	output.Model = job.Model
	output.WatchCommand = jobWatchCommand(output.JobID, opts.home)
	if job.ID == "" {
		// The holder is inside its dispatch window: the job is real and coming,
		// it is simply not insertable-and-readable yet.
		output.JobState = "dispatching"
	}
	if payload, err := workflow.ParseJobPayload(job.Payload); err == nil {
		if decision := reviewVerdictDecision(job, payload); decision != "" {
			output.State = reviewRequestVerdictExists
			output.Verdict = decision
		}
		if len(payload.ReviewModelPool) > 0 {
			output.ModelPool = payload.ReviewModelPool
		}
		if strings.TrimSpace(payload.Model) != "" {
			output.Model = payload.Model
		}
	}
	if err := subscribeReviewRequester(ctx, store, &output, opts); err != nil {
		return reviewRequestOutput{}, err
	}
	return output, nil
}

// subscribeReviewRequester registers the requester's durable interest in the
// exact-head verdict. The subscription rechecks canonical state on insert, so a
// verdict that already exists wakes the requester immediately, and one saved
// later wakes it from the job's own state transition — never from gate
// advancement and never from a process that has to still be alive.
func subscribeReviewRequester(ctx context.Context, store *db.Store, output *reviewRequestOutput, opts reviewRequestOptions) error {
	// The subscription is PURPOSE-SCOPED: a code verdict must not satisfy a
	// security request at the same head, which is exactly what the bare verdict
	// key does. `gitmoot org await review` keeps the bare key and its
	// any-purpose semantics; the producer resolves both.
	verdictKey, err := db.ReviewRequestSubjectKey(output.Repo, output.PullRequest, output.HeadSHA, output.Purpose)
	if err != nil {
		return err
	}
	// One live wait per role and subject: a requester that asks twice keeps its
	// original wait (and deadline) rather than failing on the live-subject index.
	waiting, err := store.ListAwaitedFacts(ctx, opts.role, db.AwaitedFactStateWaiting)
	if err != nil {
		return err
	}
	for _, fact := range waiting {
		if fact.SubjectKind == db.AwaitedFactSubjectReviewVerdict && fact.SubjectKey == verdictKey {
			output.AwaitedFactID = fact.ID
			return nil
		}
	}
	fact, skipped, err := store.SubscribeAwaitedFact(ctx, db.AwaitedFactSubscription{
		WaiterRole:  opts.role,
		SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey:  verdictKey,
		Deadline:    time.Now().UTC().Add(opts.ttl),
	})
	if err != nil {
		return fmt.Errorf("subscribe %s to the verdict: %w", opts.role, err)
	}
	// A head-blind review cannot satisfy an exact-head wait, so a requester that
	// is never told about it waits the full TTL believing a review is coming.
	// Say so at request time instead (#2130 makes this shape common).
	for _, skip := range skipped {
		hold := fmt.Sprintf("review job %s recorded no head, so it cannot satisfy this exact-head wait", skip.JobID)
		if skip.ExternallyDriven {
			hold += " (externally driven: its attribution row is not head-bound)"
		}
		output.Holds = append(output.Holds, hold)
	}
	output.AwaitedFactID = fact.ID
	return nil
}

// reviewRuntimeForCandidate picks the runtime this candidate will actually run
// on, given any availability hold on the REQUESTER's role (#2187).
//
// THE DEFECT THIS CLOSES: selection pinned omp without consulting availability,
// and refuseUnavailableOrgRole then tested that already-made choice at dispatch
// - one layer too late, where the only remaining move is to refuse. On
// 2026-09-14 two seats spent three dispatch attempts and two escalations
// discovering that a review they were OBLIGED to obtain could not be
// dispatched, and the remedy they were given was to type the runtime by hand.
// A router that picks a runtime it could have known was walled has handed its
// own job to the caller.
//
// Order is deliberate: the omp pin first, because the router owns the runtime
// when it selects the reviewer; then the candidate's OWN registered runtime,
// which it is provisioned for. Shell is never auto-selected - its "runtime ref"
// is a command, so a review dispatched onto it would run the agent's script
// instead of a model.
func reviewRuntimeForCandidate(agent db.Agent, incident db.OrgRoleUnavailable, held bool) (string, bool) {
	options := []string{runtime.OmpRuntime}
	if registered := strings.TrimSpace(agent.Runtime); registered != "" && registered != runtime.OmpRuntime {
		options = append(options, registered)
	}
	registered := strings.TrimSpace(agent.Runtime)
	for _, option := range options {
		if held && orgRoleUnavailableRefusesRuntime(incident, option) {
			continue
		}
		// THE TWO VERBS MUST AGREE ON IDENTICAL FLEET STATE (#2189 round 2).
		// A shell-only fleet under an omp hold used to have `review request`
		// REFUSE while `agent review` dispatched the script - same repo, same
		// PR, same holds, different answer depending on which verb was typed.
		// The cause was applying the override-usability property to the
		// candidate's OWN runtime, which needs no override at all.
		if option == registered {
			return option, true
		}
		// EXCLUDED BY PROPERTY, NOT BY NAME (#2189 review). Shell used to be
		// named here, and a name-skip is an exemption that does not look like
		// one - the shape that let Engine.Store survive seven rounds of deleting
		// exemption lists. The real constraint is that the router selects a
		// runtime WITHOUT a session, and a runtime whose sessions are commands
		// refuses exactly that. Ask the resolver instead of hardcoding which
		// runtime that is today.
		if _, _, err := resolveJobRuntimeOverride(option, ""); err != nil {
			continue
		}
		return option, true
	}
	return "", false
}

// reviewRuntimeForOperatorNamedAgent is reviewRuntimeForCandidate with the
// preference reversed: the named agent's OWN runtime first, the omp pin only as
// the escape when that runtime is walled. Same usability property, same hold
// check, opposite order - because here the operator chose the agent and its
// runtime is part of that choice (#2187).
func reviewRuntimeForOperatorNamedAgent(agent db.Agent, incident db.OrgRoleUnavailable, held bool) (string, bool) {
	// The agent's OWN runtime is checked against the hold only. The usability
	// property - "can this runtime be selected without a session?" - applies to
	// a runtime we would SET AS AN OVERRIDE, not to the runtime the agent is
	// already registered on: a shell agent runs its own command and needs no
	// override, so testing it as one rerouted script agents onto omp and would
	// have run a model instead of the operator's script.
	registered := strings.TrimSpace(agent.Runtime)
	if registered != "" && !(held && orgRoleUnavailableRefusesRuntime(incident, registered)) {
		return registered, true
	}
	// A HELD SCRIPT AGENT REFUSES; IT DOES NOT ESCAPE TO omp (#2189 round 2).
	// A shell-scoped hold is reachable - any known runtime name can be walled,
	// and a shell job failing with a quota signature writes one - and taking the
	// omp escape would run a MODEL review under the script agent's identity,
	// which is the exact harm this function exists to prevent. A script has no
	// model fallback, so there is nothing to fall back to.
	if registered == runtime.ShellRuntime {
		return "", false
	}
	for _, option := range []string{runtime.OmpRuntime} {
		if held && orgRoleUnavailableRefusesRuntime(incident, option) {
			continue
		}
		if _, _, err := resolveJobRuntimeOverride(option, ""); err != nil {
			continue
		}
		return option, true
	}
	return "", false
}

// reviewModelForRuntime enforces the invariant this work has now broken THREE
// times from three directions (#2189 review P1):
//
//		MODEL AND RUNTIME MUST BE CHOSEN TOGETHER.
//
//	 1. #2186 round 4: runtime parity forced a claude reviewer onto omp, stripping
//	    its auth profile.
//	 2. Same round: the pool head was pinned as payload.Model regardless of the
//	    effective runtime, handing an omp-qualified model to a claude reviewer.
//	 3. Here: the RUNTIME was demoted away from omp for availability while the
//	    omp-qualified pool model stayed - producing a job that runs and dies at
//	    delivery instead of a review. Demoting turned a clear refusal into a
//	    silent late failure, which is worse than what it replaced.
//
// THE GENERAL FORM, after a FOURTH arrival walked around the narrow one
// (#2189 round 2): ANY model reaching a dispatch must be resolvable by the
// runtime that dispatch will use, WHATEVER ITS SOURCE. This function binds the
// POOL model; an operator-supplied --model is bound by routerChosenReviewRuntime,
// which refuses to reroute underneath one. Stating the rule about pool models
// only was a correct rule written narrowly enough that the next arrival - an
// operator flag - walked straight past it.
//
// The pool is an OMP provider/model chain, so it may only be used when the
// review actually runs on omp. Off omp the agent's own configured model is the
// only valid choice, and the router states nothing.
func reviewModelForRuntime(selectedRuntime string, pool []string) string {
	if strings.TrimSpace(selectedRuntime) != runtime.OmpRuntime || len(pool) == 0 {
		return ""
	}
	return pool[0]
}

// selectReviewRouterAgent picks the registered agent that will carry the
// review. The agent supplies identity and repo access; runtime and model come
// from the router, so any review-capable agent scoped to the repository will
// do, and an omp-native one is preferred so the override changes nothing.
func selectReviewRouterAgent(ctx context.Context, store *db.Store, repo string, pullRequest int, headSHA, purpose, explicit, role string) (db.Agent, string, error) {
	incident, held, err := store.GetActiveOrgRoleUnavailable(ctx, role, time.Now().UTC())
	if err != nil {
		return db.Agent{}, "", err
	}
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		agent, err := store.GetAgent(ctx, explicit)
		if err != nil {
			return db.Agent{}, "", fmt.Errorf("reviewer %q: %w", explicit, err)
		}
		if !agentHasCapability(agent.Capabilities, "review") {
			return db.Agent{}, "", fmt.Errorf("reviewer %q is not registered with the review capability", explicit)
		}
		// A NAMED reviewer is routed own-first on BOTH verbs (#2189 round 2).
		// The auth-profile rationale - rerouting an unwalled agent strips the
		// profile ApplyJobRuntimeOverride clears - does not depend on which verb
		// the caller typed, and a router whose answer changes with the verb is a
		// weaker guarantee than one answer. Only a reviewer the ROUTER picked is
		// routed omp-first, because there the router chose the agent too.
		selected, ok := reviewRuntimeForOperatorNamedAgent(agent, incident, held)
		if !ok {
			return db.Agent{}, "", unavailableReviewRuntimeError(role, incident)
		}
		return agent, selected, nil
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return db.Agent{}, "", err
	}
	candidates := make([]db.Agent, 0, len(agents))
	for _, agent := range agents {
		if !agentHasCapability(agent.Capabilities, "review") || agentHasCapability(agent.Capabilities, "implement") {
			continue
		}
		allowed, err := store.AgentCanAccessRepo(ctx, agent.Name, repo)
		if err != nil {
			return db.Agent{}, "", err
		}
		if allowed {
			candidates = append(candidates, agent)
		}
	}
	if len(candidates) == 0 {
		return db.Agent{}, "", fmt.Errorf("no review-only agent is registered for %s; register one with `gitmoot agent subscribe <name> --runtime omp --session fresh:<suffix> --role reviewer --repo %s --capability review` or pass --reviewer", repo, repo)
	}
	// A SECOND PURPOSE AT THE SAME HEAD MUST NOT BE REFUSED AS A LOOP (#2172
	// review rounds 2-4). "Answered" is scoped to the PURPOSE being requested,
	// exactly as DetectReviewLoop now is: an agent holding a code verdict here
	// has not answered a security request, so it is neither deprioritized nor
	// grounds for refusal. Round 4 caught this map still purpose-blind while
	// the guard behind it had been fixed - the refusal below fired for every
	// candidate whenever a same-head verdict of ANY purpose existed, which is
	// the documented contract failing on the selection path.
	answered := map[string]bool{}
	wantPurpose := strings.ToLower(strings.TrimSpace(purpose))
	if wantPurpose == "" {
		wantPurpose = db.DefaultReviewPurpose
	}
	if verdicts, err := store.SucceededReviewVerdicts(ctx, repo, pullRequest); err == nil {
		for _, verdict := range verdicts {
			if !strings.EqualFold(verdict.HeadSHA, headSHA) {
				continue
			}
			got := strings.ToLower(strings.TrimSpace(verdict.ReviewPurpose))
			if got == "" {
				got = db.DefaultReviewPurpose
			}
			if got != wantPurpose {
				continue
			}
			answered[strings.ToLower(strings.TrimSpace(verdict.Agent))] = true
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		leftFree, rightFree := !answered[strings.ToLower(candidates[i].Name)], !answered[strings.ToLower(candidates[j].Name)]
		if leftFree != rightFree {
			return leftFree
		}
		left, right := candidates[i].Runtime == runtime.OmpRuntime, candidates[j].Runtime == runtime.OmpRuntime
		if left != right {
			return left
		}
		return candidates[i].Name < candidates[j].Name
	})
	if answered[strings.ToLower(candidates[0].Name)] {
		return db.Agent{}, "", fmt.Errorf("every review-only agent for %s already holds a %s verdict at head %s, so a repeat of the same purpose would be refused as a review loop; register another review-only agent, ask a different purpose, or pass --reviewer", repo, wantPurpose, headSHA)
	}
	// ROUTE AROUND THE HOLD RATHER THAN REFUSE AT DISPATCH. Candidates are
	// already ordered by the loop guard and the omp preference, so a hold
	// DEMOTES a candidate instead of failing the request.
	for _, candidate := range candidates {
		if selected, ok := reviewRuntimeForCandidate(candidate, incident, held); ok {
			return candidate, selected, nil
		}
	}
	return db.Agent{}, "", unavailableReviewRuntimeError(role, incident)
}

// unavailableReviewRuntimeError names the runtime the hold is scoped to and
// when it lifts. The refusal two seats hit on 2026-09-14 named neither, which
// is why they escalated rather than routing elsewhere: a refusal that does not
// say what it is scoped to is indistinguishable from a total outage (#2181).
func unavailableReviewRuntimeError(role string, incident db.OrgRoleUnavailable) error {
	scope := strings.TrimSpace(incident.Runtime)
	if scope == "" {
		scope = "every runtime"
	}
	return fmt.Errorf("no available runtime for role %q: its %s hold (%s) lasts until %s, and no review-capable agent offers another runtime; wait, pass --runtime, or clear the hold",
		role, scope, strings.TrimSpace(incident.Reason), strings.TrimSpace(incident.Until))
}

// reviewRequestHolds names admission states that keep a dispatched review from
// starting. They are reported on the request itself so a requester can tell a
// held review from a slow one instead of pinging its coordinator.
func reviewRequestHolds(paths config.Paths, home string) []string {
	var holds []string
	if running, err := daemonIsRunning(home); err != nil {
		holds = append(holds, "daemon state unknown: "+err.Error())
	} else if !running {
		holds = append(holds, "daemon not running: the review stays queued until `gitmoot daemon run` starts")
	}
	if evaluation := evaluateConfiguredDiskGuard(paths); !evaluation.allowsDispatch() {
		holds = append(holds, "disk guard paused dispatch: "+evaluation.detail())
	}
	return holds
}

// resolveDeltaReviewScope picks the most recent terminal verdict for this repo,
// pull request and purpose whose head is an ancestor of head, and bounds the
// review to that range (#2177). A nil scope with a non-empty reason means a
// full review; the reason is REPORTED, never acted on, because every failure
// here costs one full read and must never refuse the request. Refusing would
// block the commonest shape of all - a branch rebased onto main.
// undecodableBlocks reports why a delta must be refused because an invisible
// verdict would have been SELECTED instead of the chosen baseline.
//
// Selection orders by updated_at and takes the newest ancestor matching the
// purpose, so "the reviewer last saw head X" is a RECENCY claim, not merely a
// positional one (#2178 round 3). Testing position alone missed a newer
// invisible verdict at a head that happens to be an ancestor OF the baseline:
// it would have sorted first, so the brief named the wrong head while the
// positional check saw nothing between the two commits.
//
// Each arm below mirrors one reason selection itself would have passed the row
// over, so a row that could never have been chosen never refuses:
//   - not valid JSON at all: nothing about its position is knowable, and that
//     is the ONLY shape that disqualifies unconditionally;
//   - no head: selection skips an empty baseline, so it is unselectable
//     (Mailbox.OpenExternalJob writes exactly this shape, and one such row used
//     to disable delta review for its whole pull request permanently);
//   - another purpose: a different question;
//   - not newer than the baseline: selection had already passed it;
//   - not an ancestor of the dispatch head: off this line of history.
func undecodableBlocks(ctx context.Context, git gitutil.Client, undecodable []db.UndecodableReviewVerdict, baseline, baselineUpdatedAt, baselineJobID, head, wantPurpose string) string {
	for _, invisible := range undecodable {
		if !invisible.Readable {
			return "verdict history has an unreadable row; its position cannot be established, so the baseline cannot be trusted"
		}
		if invisible.HeadSHA == "" {
			continue
		}
		got := invisible.ReviewPurpose
		if got == "" {
			got = db.DefaultReviewPurpose
		}
		if got != wantPurpose {
			continue
		}
		if invisible.HeadSHA == baseline || invisible.HeadSHA == head {
			continue
		}
		// String comparison, because SucceededReviewVerdicts orders by the same
		// column as a string: the two agree by construction.
		if invisible.UpdatedAt < baselineUpdatedAt ||
			(invisible.UpdatedAt == baselineUpdatedAt && invisible.JobID <= baselineJobID) {
			continue
		}
		reachable, err := git.IsAncestor(ctx, invisible.HeadSHA, head)
		if err != nil || !reachable {
			continue
		}
		return fmt.Sprintf("an undecodable verdict at %s is newer than the baseline and reachable from this head, so it would have been selected; baseline cannot be trusted", invisible.HeadSHA)
	}
	return ""
}

func resolveDeltaReviewScope(ctx context.Context, store *db.Store, git gitutil.Client, repo string, pullRequest int, head, purpose string) (*workflow.ReviewScope, string) {
	// AN UNDECODABLE VERDICT IS NOT AN ABSENCE (#2179, #2178 round 1). A row
	// SucceededReviewVerdicts cannot decode is dropped silently, so the loop
	// below would walk straight past a real terminal verdict to an OLDER
	// ancestor - bounding the review against a stale baseline and telling the
	// reviewer "you last saw head X" when it did not. A false statement in the
	// brief is worse than a full re-read, so lossy history disables the delta
	// entirely rather than selecting from what survived.
	undecodable, err := store.UndecodableReviewVerdicts(ctx, repo, pullRequest)
	if err != nil {
		return nil, "scope unavailable: " + err.Error()
	}
	verdicts, err := store.SucceededReviewVerdicts(ctx, repo, pullRequest)
	if err != nil {
		return nil, "scope unavailable: " + err.Error()
	}
	want := strings.ToLower(strings.TrimSpace(purpose))
	if want == "" {
		want = db.DefaultReviewPurpose
	}
	head = strings.ToLower(strings.TrimSpace(head))
	// SucceededReviewVerdicts is already ordered updated_at DESC, id DESC and
	// already limited to succeeded jobs holding approved or changes_requested,
	// which is exactly the terminal-verdict set a baseline may come from.
	var lastUnavailable string
	candidates := 0
	for _, verdict := range verdicts {
		got := strings.ToLower(strings.TrimSpace(verdict.ReviewPurpose))
		if got == "" {
			got = db.DefaultReviewPurpose
		}
		if got != want {
			continue
		}
		baseline := strings.ToLower(strings.TrimSpace(verdict.HeadSHA))
		if baseline == "" || baseline == head {
			// The same head is the verdict-reuse path, already handled before
			// any dispatch; it is not a baseline for a delta against itself.
			continue
		}
		candidates++
		files, err := localReviewChangedFiles(ctx, git, pullRequest, baseline, head)
		if err != nil {
			var unavailable workflow.ReviewScopeUnavailableError
			if errors.As(err, &unavailable) {
				// PROOF this head is not an ancestor. A rebase usually orphans
				// every older head too, so keep looking rather than concluding.
				lastUnavailable = unavailable.Reason
				continue
			}
			// The instrument could not RUN. Do not keep hammering git.
			return nil, "scope unavailable: " + err.Error()
		}
		job, err := store.GetJob(ctx, verdict.JobID)
		if err != nil {
			return nil, "scope unavailable: " + err.Error()
		}
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			return nil, "scope unavailable: " + err.Error()
		}
		var findings []string
		if payload.Result != nil {
			findings = workflow.NamedReviewFindings(*payload.Result)
		}
		// ANCESTRY-SCOPED, NOT PR-SCOPED (#2178 round 2). Refusing on ANY
		// undecodable row over-refuses: a row orphaned by a rebase, or one for
		// another purpose, could never have been selected as this baseline. The
		// question is narrower - is an invisible verdict sitting BETWEEN the
		// baseline and the dispatch head, where it would have been the real
		// baseline? Only then is the "you last saw X" sentence false.
		if reason := undecodableBlocks(ctx, git, undecodable, baseline, job.UpdatedAt, job.ID, head, want); reason != "" {
			return nil, reason
		}
		return &workflow.ReviewScope{
			PreviousHeadSHA: baseline,
			Findings:        findings,
			ChangedFiles:    files,
		}, ""
	}
	if candidates == 0 {
		return nil, "no prior verdict for this purpose"
	}
	return nil, "not a direct follow-up: " + lastUnavailable
}

func reviewRouterInstructions(purpose, repo string, pullRequest int, head string, scope *workflow.ReviewScope) string {
	var focus string
	switch purpose {
	case "security":
		focus = "Prioritize authentication, authorization, secrets, injection, unsafe deserialization, path traversal, and trust-boundary changes; treat every new external input as hostile."
	case "ui":
		focus = "Prioritize user-visible behavior: rendering, state transitions, accessibility, error and empty states, and regressions in existing flows."
	case "architecture":
		focus = "Prioritize module boundaries, data ownership, concurrency and failure semantics, migration safety, and whether the change makes future failures easier or harder to detect."
	default:
		focus = "Prioritize correctness of the changed production paths, their failure boundaries, and valid-input behavior."
	}
	// A bounded delta review replaces ONLY the diff-scope sentence; every
	// obligation after it - execute checks, declare evidence, answer prior
	// findings, do not edit - is identical on both paths (#2177).
	diffScope := "Read the full diff against its base."
	if scope != nil {
		diffScope = workflow.ReviewScopeInstructions(head, scope)
	}
	return fmt.Sprintf(`Independent review-only assessment of %s pull request #%d at exact head %s.
%s
%s Independently execute substantive focused checks that exercise the changed production code (build, vet, and the tests covering the changed paths); do not approve on static reading alone. Return evidence=executed with a nonempty tests_run naming the exact commands and outcomes, and an evidence locator for every finding. Answer every prior finding recorded for this pull request. Do not edit files, implement or apply fixes, merge, deploy, or perform live-service actions.`, repo, pullRequest, head, focus, diffScope)
}

func printReviewRequestOutput(w io.Writer, output reviewRequestOutput) {
	fmt.Fprintf(w, "state: %s\n", output.State)
	fmt.Fprintf(w, "repo: %s#%d head: %s purpose: %s\n", output.Repo, output.PullRequest, output.HeadSHA, output.Purpose)
	fmt.Fprintf(w, "job: %s", output.JobID)
	if output.JobState != "" {
		fmt.Fprintf(w, " (%s)", output.JobState)
	}
	fmt.Fprintln(w)
	if output.Reviewer != "" {
		fmt.Fprintf(w, "reviewer: %s on omp model %s\n", output.Reviewer, output.Model)
	}
	if output.Verdict != "" {
		fmt.Fprintf(w, "verdict: %s (already saved; no new review spent)\n", output.Verdict)
	}
	if output.Baseline != "" {
		fmt.Fprintf(w, "baseline: %s (delta review; prior findings carried)\n", output.Baseline)
	}
	if output.BaselineSkipped != "" {
		fmt.Fprintf(w, "baseline: none (%s)\n", output.BaselineSkipped)
	}
	if len(output.ModelPool) > 1 {
		fmt.Fprintf(w, "fallback models: %s\n", strings.Join(output.ModelPool[1:], ", "))
	}
	fmt.Fprintf(w, "notify: %s (awaited fact %d)\n", output.NotifyBy, output.AwaitedFactID)
	for _, hold := range output.Holds {
		fmt.Fprintf(w, "hold: %s\n", hold)
	}
	if output.WatchCommand != "" {
		fmt.Fprintf(w, "watch: %s\n", output.WatchCommand)
	}
}

// reviewGateState renders the gate's own capability for this repo. It answers
// the question a router surface must not leave to inference: if a merge does
// not happen, is that the gate refusing, or the gate declining to decide?
func reviewGateState(home, repo string) string {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return "unknown: " + err.Error()
	}
	gate, err := config.LoadMergeGatePolicy(paths)
	if err != nil {
		return "unknown: " + err.Error()
	}
	if gate.For(repo).AutoMerge {
		return "native auto-merge enabled: an exact-head approval plus green CI can merge without a human"
	}
	return "native auto-merge disabled by operator kill switch: the gate publishes status but merges nothing, and a merge is a human decision"
}

type reviewStatusOutput struct {
	Repo        string `json:"repo"`
	PullRequest int    `json:"pull_request"`
	// Gate states what the native merge gate can do for this repository, in
	// words. A requester must never read an absent or kill-switched gate as an
	// approval, and "not applied to this head" alone does not say which it is.
	Gate     string              `json:"gate"`
	Requests []reviewStatusEntry `json:"requests"`
}

type reviewStatusEntry struct {
	JobID   string `json:"job_id"`
	State   string `json:"state"`
	HeadSHA string `json:"head_sha"`
	Purpose string `json:"purpose,omitempty"`
	// Baseline is the prior head this review was bounded to, empty for a full
	// review (#2177). It tells a reader which kind of review produced a verdict
	// without reading the reviewer's prompt.
	Baseline  string `json:"baseline,omitempty"`
	Requester string `json:"requester,omitempty"`
	Reviewer  string `json:"reviewer"`
	Model     string `json:"model,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
	Findings  int    `json:"findings"`
	Hold      string `json:"hold,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

func runReviewStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "repository in owner/repo form (defaults to the current checkout)")
	pr := fs.Int("pr", 0, "pull request number")
	jsonOutput := fs.Bool("json", false, "print the status as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *pr <= 0 {
		fmt.Fprintln(stderr, "review status requires --pr NUMBER")
		return 2
	}
	var output reviewStatusOutput
	err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		execBackend, err := localAgentDispatchExecBackendFor(*home)
		if err != nil {
			return err
		}
		runner, err := jobSubprocessRunnerForBackend(execBackend)
		if err != nil {
			return err
		}
		repo, _, _, _, err := resolveLocalAgentRepo(ctx, store, *repoFlag, runner)
		if err != nil {
			return err
		}
		jobs, err := store.ListReviewJobsForPullRequest(ctx, repo.FullName(), *pr)
		if err != nil {
			return err
		}
		output = reviewStatusOutput{Repo: repo.FullName(), PullRequest: *pr, Gate: reviewGateState(*home, repo.FullName()), Requests: make([]reviewStatusEntry, 0, len(jobs))}
		for _, job := range jobs {
			entry := reviewStatusEntry{JobID: job.ID, State: job.State, Reviewer: job.Agent, Model: job.Model, UpdatedAt: job.UpdatedAt}
			if payload, err := workflow.ParseJobPayload(job.Payload); err == nil {
				entry.HeadSHA = payload.HeadSHA
				entry.Purpose = payload.ReviewPurpose
				entry.Requester = payload.ReviewRequester
				if payload.ReviewScope != nil {
					entry.Baseline = payload.ReviewScope.PreviousHeadSHA
				}
				if payload.Model != "" {
					entry.Model = payload.Model
				}
				// #1685: a fan-out announcement is NOT a verdict, and every
				// consumer must refuse to render it as one. A staged preflight
				// announces its children with decision="approved"; printing that
				// as verdict=approved tells a requester the head was approved
				// while the verdict child is still running. reviewVerdictDecision
				// is the single place that distinguishes them.
				entry.Verdict = reviewVerdictDecision(job, payload)
				if entry.Verdict != "" {
					entry.Evidence = payload.Result.Evidence
					entry.Findings = len(payload.Result.Findings)
				}
			}
			if reason := loadStuckReason(store, job); reason.Reason != "" {
				entry.Hold = reason.Reason
			}
			output.Requests = append(output.Requests, entry)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "review status: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "review status: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s#%d: %d review job(s)\ngate: %s\n", output.Repo, output.PullRequest, len(output.Requests), output.Gate)
	for _, entry := range output.Requests {
		line := fmt.Sprintf("  %s %s head=%s reviewer=%s", entry.JobID, entry.State, shortReviewHead(entry.HeadSHA), entry.Reviewer)
		if entry.Model != "" {
			line += " model=" + entry.Model
		}
		if entry.Baseline != "" {
			line += " baseline=" + shortReviewHead(entry.Baseline)
		}
		if entry.Verdict != "" {
			line += fmt.Sprintf(" verdict=%s evidence=%s findings=%d", entry.Verdict, firstNonEmpty(entry.Evidence, "undeclared"), entry.Findings)
		}
		if entry.Hold != "" {
			line += " hold=" + entry.Hold
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}

func shortReviewHead(head string) string {
	if len(head) > 12 {
		return head[:12]
	}
	return head
}
