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
	State         string   `json:"state"`
	Repo          string   `json:"repo"`
	PullRequest   int      `json:"pull_request"`
	HeadSHA       string   `json:"head_sha"`
	Purpose       string   `json:"purpose"`
	Requester     string   `json:"requester"`
	JobID         string   `json:"job_id"`
	JobState      string   `json:"job_state,omitempty"`
	Reviewer      string   `json:"reviewer,omitempty"`
	Model         string   `json:"model,omitempty"`
	ModelPool     []string `json:"model_pool,omitempty"`
	Verdict       string   `json:"verdict,omitempty"`
	AwaitedFactID int64    `json:"awaited_fact_id"`
	NotifyBy      string   `json:"notify_by"`
	Holds         []string `json:"holds,omitempty"`
	WatchCommand  string   `json:"watch_command,omitempty"`
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
	fmt.Fprintln(w, "  gitmoot review request --pr NUMBER [--repo OWNER/REPO] [--head SHA] [--branch NAME] [--purpose code|security|ui|architecture] [--role ROLE] [--ttl DURATION] [--reviewer AGENT] [--json] [--home DIR]")
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
	home     string
	repo     string
	pr       int
	head     string
	branch   string
	purpose  string
	role     string
	ttl      time.Duration
	reviewer string
	json     bool
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
	reviewer, err := selectReviewRouterAgent(ctx, store, repo.FullName(), opts.pr, head, opts.reviewer)
	if err != nil {
		releaseUnenqueuedReviewClaim(ctx, store, subjectKey, jobID)
		return reviewRequestOutput{}, err
	}
	request := localAgentDispatchRequest{
		RepoFlag:             repo.FullName(),
		Agent:                reviewer.Name,
		Action:               "review",
		Instructions:         reviewRouterInstructions(opts.purpose, repo.FullName(), opts.pr, head),
		Background:           true,
		Model:                pool[0],
		Runtime:              runtime.OmpRuntime,
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
func reviewJobStillAnswers(ctx context.Context, store *db.Store, job db.Job) bool {
	switch job.State {
	case string(workflow.JobQueued), string(workflow.JobRunning), string(workflow.JobBlocked):
		return true
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		return false
	}
	if reviewVerdictDecision(job, payload) != "" {
		return true
	}
	if payload.Result == nil || !workflow.ResultIsFanOut(payload.Result) {
		return false
	}
	children, err := store.ListJobsByParent(ctx, job.ID)
	if err != nil {
		// Unknown is not proof the tree is finished; failing closed costs a
		// re-run, failing open costs a duplicate reviewer.
		return true
	}
	for _, child := range children {
		// #2172 round 3: only a REVIEW child can answer a review question. A
		// staged preflight may also delegate a non-review leg (implement, ask)
		// whose result legitimately carries decision="approved"; counting it
		// pinned the claim forever while the awaited fact - which only a review
		// verdict can satisfy - stayed unsatisfiable. A non-review child is not
		// evidence in either direction, so it is skipped rather than trusted.
		if !reviewTypedJob(child) {
			continue
		}
		if reviewJobStillAnswers(ctx, store, child) {
			return true
		}
	}
	return false
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
		return job, !reviewJobStillAnswers(ctx, store, job), nil
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

// selectReviewRouterAgent picks the registered agent that will carry the
// review. The agent supplies identity and repo access; runtime and model come
// from the router, so any review-capable agent scoped to the repository will
// do, and an omp-native one is preferred so the override changes nothing.
func selectReviewRouterAgent(ctx context.Context, store *db.Store, repo string, pullRequest int, headSHA, explicit string) (db.Agent, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		agent, err := store.GetAgent(ctx, explicit)
		if err != nil {
			return db.Agent{}, fmt.Errorf("reviewer %q: %w", explicit, err)
		}
		if !agentHasCapability(agent.Capabilities, "review") {
			return db.Agent{}, fmt.Errorf("reviewer %q is not registered with the review capability", explicit)
		}
		return agent, nil
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return db.Agent{}, err
	}
	candidates := make([]db.Agent, 0, len(agents))
	for _, agent := range agents {
		if !agentHasCapability(agent.Capabilities, "review") || agentHasCapability(agent.Capabilities, "implement") {
			continue
		}
		allowed, err := store.AgentCanAccessRepo(ctx, agent.Name, repo)
		if err != nil {
			return db.Agent{}, err
		}
		if allowed {
			candidates = append(candidates, agent)
		}
	}
	if len(candidates) == 0 {
		return db.Agent{}, fmt.Errorf("no review-only agent is registered for %s; register one with `gitmoot agent subscribe <name> --runtime omp --session fresh:<suffix> --role reviewer --repo %s --capability review` or pass --reviewer", repo, repo)
	}
	// A SECOND PURPOSE AT THE SAME HEAD MUST NOT BE REFUSED AS A LOOP (#2172
	// review round 2). DetectReviewLoop keys on agent identity and head, and it
	// is purpose-blind by design because it protects the engine's own re-review
	// path. If the router hands it an agent that already holds a verdict here,
	// a security request is refused because a code review happened — flatly
	// contradicting the documented parallel-purposes contract. So prefer an
	// agent that has not already answered at this head, and let the shared
	// guard keep its meaning instead of weakening it for everyone.
	answered := map[string]bool{}
	if verdicts, err := store.SucceededReviewVerdicts(ctx, repo, pullRequest); err == nil {
		for _, verdict := range verdicts {
			if strings.EqualFold(verdict.HeadSHA, headSHA) {
				answered[strings.ToLower(strings.TrimSpace(verdict.Agent))] = true
			}
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
		return db.Agent{}, fmt.Errorf("every review-only agent for %s already holds a verdict at head %s, so a second purpose would be refused as a review loop; register another review-only agent or pass --reviewer", repo, headSHA)
	}
	return candidates[0], nil
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

func reviewRouterInstructions(purpose, repo string, pullRequest int, head string) string {
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
	return fmt.Sprintf(`Independent review-only assessment of %s pull request #%d at exact head %s.
%s
Read the full diff against its base. Independently execute substantive focused checks that exercise the changed production code (build, vet, and the tests covering the changed paths); do not approve on static reading alone. Return evidence=executed with a nonempty tests_run naming the exact commands and outcomes, and an evidence locator for every finding. Answer every prior finding recorded for this pull request. Do not edit files, implement or apply fixes, merge, deploy, or perform live-service actions.`, repo, pullRequest, head, focus)
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
	Repo        string              `json:"repo"`
	PullRequest int                 `json:"pull_request"`
	// Gate states what the native merge gate can do for this repository, in
	// words. A requester must never read an absent or kill-switched gate as an
	// approval, and "not applied to this head" alone does not say which it is.
	Gate        string              `json:"gate"`
	Requests    []reviewStatusEntry `json:"requests"`
}

type reviewStatusEntry struct {
	JobID     string `json:"job_id"`
	State     string `json:"state"`
	HeadSHA   string `json:"head_sha"`
	Purpose   string `json:"purpose,omitempty"`
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
