package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func runJob(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printJobUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runJobList(args[1:], stdout, stderr)
	case "show":
		return runJobShow(args[1:], stdout, stderr)
	case "events":
		return runJobEvents(args[1:], stdout, stderr)
	case "watch":
		return runJobWatch(args[1:], stdout, stderr)
	case "run":
		return runJobRun(args[1:], stdout, stderr)
	case "retry":
		return runJobRetry(args[1:], stdout, stderr)
	case "cancel":
		return runJobCancel(args[1:], stdout, stderr)
	case "kill":
		return runJobKill(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown job command %q\n\n", args[0])
		printJobUsage(stderr)
		return 2
	}
}

func printJobUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot job list [--repo owner/repo] [--state state] [--json]")
	fmt.Fprintln(w, "  gitmoot job show <id> [--json]")
	fmt.Fprintln(w, "  gitmoot job events <id>")
	fmt.Fprintln(w, "  gitmoot job watch <id> [--poll 1s] [--json]")
	fmt.Fprintln(w, "  gitmoot job run <id>")
	fmt.Fprintln(w, "  gitmoot job retry <id>")
	fmt.Fprintln(w, "  gitmoot job cancel <id>")
	fmt.Fprintln(w, "  gitmoot job kill <root-job-id>")
}

func runJobList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo scope as owner/repo")
	state := fs.String("state", "", "job state filter")
	jsonOutput := fs.Bool("json", false, "print jobs (with why-stuck detail) as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "job list does not accept positional arguments")
		return 2
	}
	var jobs []db.Job
	// preflightFailed maps a coordinator job id -> the reason its delegation
	// fan-out could not be routed (#451). A delegation preflight failure no longer
	// terminal-blocks the coordinator, so its job state and overall-latest event do
	// not reveal it; this surfaces the reason as a trailing column so a zero-child
	// fan-out is not invisible. Best-effort: a lookup error leaves the column off.
	var preflightFailed map[string]string
	// reasonEvents / locks feed the why-stuck surface (#552): the latest
	// reason-bearing event per job and the live resource locks, both fetched in a
	// single query so the derived reason costs no per-job lookup. Best-effort: a
	// lookup error leaves the reason off rather than failing the listing.
	var reasonEvents map[string]db.JobEvent
	var locks []db.ResourceLock
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		jobs, err = store.ListJobs(context.Background())
		if err != nil {
			return err
		}
		preflightFailed, _ = store.JobIDsWithEventKind(context.Background(), "delegation_preflight_failed")
		reasonEvents, _ = store.LatestJobEventsOfKinds(context.Background(), stuckReasonEventKinds)
		locks, _ = store.ListResourceLocks(context.Background())
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "job list: %v\n", err)
		return 1
	}
	filtered := filterJobs(jobs, *repo, *state)
	if *jsonOutput {
		entries := make([]jobListEntry, 0, len(filtered))
		for _, job := range filtered {
			payload, _ := daemonJobPayload(job)
			ev, ok := reasonEvents[job.ID]
			reason := deriveStuckReason(job, ev, ok, locks)
			entries = append(entries, jobListEntry{
				ID:              job.ID,
				State:           job.State,
				Type:            job.Type,
				Agent:           job.Agent,
				Repo:            payload.Repo,
				PullRequest:     payload.PullRequest,
				PreflightFailed: strings.TrimSpace(preflightFailed[job.ID]),
				WhyStuck:        reason.Reason,
				NextRetryAt:     reason.NextRetryAt,
				SuggestedAction: reason.SuggestedAction,
			})
		}
		if err := writeJSON(stdout, entries); err != nil {
			fmt.Fprintf(stderr, "job list: %v\n", err)
			return 1
		}
		return 0
	}
	for _, job := range filtered {
		payload, _ := daemonJobPayload(job)
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t#%d", job.ID, job.State, job.Type, job.Agent, payload.Repo, payload.PullRequest)
		if reason, ok := preflightFailed[job.ID]; ok && strings.TrimSpace(reason) != "" {
			fmt.Fprintf(stdout, "\tPREFLIGHT_FAILED: %s", reason)
		}
		ev, ok := reasonEvents[job.ID]
		if reason := deriveStuckReason(job, ev, ok, locks); !reason.empty() {
			fmt.Fprintf(stdout, "\tWHY: %s", reason.Reason)
			if reason.NextRetryAt != "" {
				fmt.Fprintf(stdout, " (next retry %s)", reason.NextRetryAt)
			}
			if reason.SuggestedAction != "" {
				fmt.Fprintf(stdout, " [action: %s]", reason.SuggestedAction)
			}
		}
		fmt.Fprintln(stdout)
	}
	return 0
}

// jobListEntry is the JSON shape for `job list --json`: the existing table
// columns plus the additive why-stuck fields (#552). The stuck fields are omitted
// when empty so a healthy job's JSON is not bloated.
type jobListEntry struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	Type            string `json:"type"`
	Agent           string `json:"agent"`
	Repo            string `json:"repo"`
	PullRequest     int    `json:"pull_request"`
	PreflightFailed string `json:"preflight_failed,omitempty"`
	WhyStuck        string `json:"why_stuck,omitempty"`
	NextRetryAt     string `json:"next_retry_at,omitempty"`
	SuggestedAction string `json:"suggested_action,omitempty"`
}

func runJobShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the job (with why-stuck detail) as JSON")
	jobID, ok := parseSingleJobID(fs, args, stderr, "job show")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	var job db.Job
	var payload workflow.JobPayload
	var reason stuckReason
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		job, err = store.GetJob(context.Background(), jobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("job %q not found", jobID)
			}
			return err
		}
		payload, err = daemonJobPayload(job)
		if err != nil {
			return err
		}
		reason = loadStuckReason(store, job)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "job show: %v\n", err)
		return 1
	}
	if *jsonOutput {
		out := jobShowOutput{Job: job, Payload: payload, WhyStuck: reason.Reason, NextRetryAt: reason.NextRetryAt, SuggestedAction: reason.SuggestedAction}
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "job show: %v\n", err)
			return 1
		}
		return 0
	}
	printJob(stdout, job, payload, reason)
	return 0
}

// jobShowOutput is the JSON shape for `job show --json`: the job, its decoded
// payload, and the additive why-stuck detail (#552). The stuck fields are omitted
// when empty so a healthy job's JSON is unchanged in spirit.
type jobShowOutput struct {
	Job             db.Job              `json:"job"`
	Payload         workflow.JobPayload `json:"payload"`
	WhyStuck        string              `json:"why_stuck,omitempty"`
	NextRetryAt     string              `json:"next_retry_at,omitempty"`
	SuggestedAction string              `json:"suggested_action,omitempty"`
}

// loadStuckReason derives a queued/blocked job's why-stuck reason from its full
// event history (scanning for the LATEST reason-bearing event, which is more
// authoritative than the overall-latest event) and the live resource locks. It is
// best-effort: a lookup error yields the zero reason rather than failing `job
// show`. Non-queued/blocked jobs short-circuit without any query.
func loadStuckReason(store *db.Store, job db.Job) stuckReason {
	state := strings.TrimSpace(job.State)
	if state != string(workflow.JobQueued) && state != string(workflow.JobBlocked) {
		return stuckReason{}
	}
	events, err := store.ListJobEvents(context.Background(), job.ID)
	if err != nil {
		return stuckReason{}
	}
	ev, ok := latestReasonEvent(events)
	locks, _ := store.ListResourceLocks(context.Background())
	return deriveStuckReason(job, ev, ok, locks)
}

// latestReasonEvent returns the last event whose kind is a stuck-reason kind.
func latestReasonEvent(events []db.JobEvent) (db.JobEvent, bool) {
	var found db.JobEvent
	var ok bool
	for _, event := range events {
		for _, kind := range stuckReasonEventKinds {
			if event.Kind == kind {
				found, ok = event, true
				break
			}
		}
	}
	return found, ok
}

func runJobEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jobID, ok := parseSingleJobID(fs, args, stderr, "job events")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	var events []db.JobEvent
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		if _, err = store.GetJob(context.Background(), jobID); err != nil {
			return err
		}
		events, err = store.ListJobEvents(context.Background(), jobID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job events: %v\n", err)
		return 1
	}
	for _, event := range events {
		fmt.Fprintf(stdout, "%s\t%s\n", event.Kind, event.Message)
	}
	return 0
}

type jobWatchOutput struct {
	Job    db.Job        `json:"job"`
	Events []db.JobEvent `json:"events"`
}

func runJobWatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	poll := fs.Duration("poll", time.Second, "poll interval")
	jsonOutput := fs.Bool("json", false, "print final job and events as JSON")
	jobID, ok := parseSingleJobID(fs, args, stderr, "job watch")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	if *poll <= 0 {
		fmt.Fprintln(stderr, "job watch poll interval must be positive")
		return 2
	}
	var output jobWatchOutput
	if err := withStore(*home, func(store *db.Store) error {
		nextEvent := 0
		for {
			job, err := store.GetJob(context.Background(), jobID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("job %q not found", jobID)
				}
				return err
			}
			events, err := store.ListJobEvents(context.Background(), jobID)
			if err != nil {
				return err
			}
			if !*jsonOutput {
				for nextEvent < len(events) {
					event := events[nextEvent]
					fmt.Fprintf(stdout, "%s\t%s\n", event.Kind, event.Message)
					nextEvent++
				}
			}
			if jobStateIsTerminal(job.State) {
				output = jobWatchOutput{Job: job, Events: events}
				return nil
			}
			time.Sleep(*poll)
		}
	}); err != nil {
		fmt.Fprintf(stderr, "job watch: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "job watch: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "state: %s\n", output.Job.State)
	return 0
}

func runJobRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jobID, ok := parseSingleJobID(fs, args, stderr, "job run")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	var latest db.Job
	if err := withStore(*home, func(store *db.Store) error {
		job, err := store.GetJob(context.Background(), jobID)
		if err != nil {
			return err
		}
		if job.State != string(workflow.JobQueued) {
			return fmt.Errorf("job %s is %s; run requires queued", job.ID, job.State)
		}
		worker := defaultJobWorker(store, stdout, *home)
		worker.CommenterFactory = worker.defaultCommenter
		if err := worker.run(context.Background(), job); err != nil {
			return err
		}
		latest, err = store.GetJob(context.Background(), jobID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job run: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "ran job %s: %s\n", latest.ID, latest.State)
	return 0
}

func runJobRetry(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job retry", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jobID, ok := parseSingleJobID(fs, args, stderr, "job retry")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	var job db.Job
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		job, err = workflow.RetryJob(context.Background(), store, jobID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job retry: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "queued retry for job %s\n", job.ID)
	return 0
}

func runJobCancel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jobID, ok := parseSingleJobID(fs, args, stderr, "job cancel")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	var job db.Job
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		job, err = workflow.CancelJob(context.Background(), store, jobID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job cancel: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "cancelled job %s\n", job.ID)
	return 0
}

// runJobKill is the operator kill switch (#341): it marks a runaway delegation
// tree (identified by its root job id) as killed so the engine routes the
// coordinator's next continuation through the graceful finalize path and the
// daemon stops starting queued children. In-flight jobs finish normally.
func runJobKill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job kill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	rootJobID, ok := parseSingleJobID(fs, args, stderr, "job kill")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	var job db.Job
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		job, err = workflow.KillDelegationTree(context.Background(), store, rootJobID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job kill: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "killed delegation tree rooted at %s\n", job.ID)
	return 0
}

func parseSingleJobID(fs *flag.FlagSet, args []string, stderr io.Writer, command string) (string, bool) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintf(stderr, "%s requires exactly one id\n", command)
			return "", false
		}
		return "", false
	}
	jobID := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return "", false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s requires exactly one id\n", command)
		return "", false
	}
	return jobID, true
}

func parseSingleJobIDExitCode(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		return 0
	}
	return 2
}

func jobStateIsTerminal(state string) bool {
	switch state {
	case string(workflow.JobSucceeded), string(workflow.JobFailed), string(workflow.JobBlocked), string(workflow.JobCancelled):
		return true
	default:
		return false
	}
}

func filterJobs(jobs []db.Job, repoFilter string, stateFilter string) []db.Job {
	repoFilter = strings.TrimSpace(repoFilter)
	stateFilter = strings.TrimSpace(stateFilter)
	filtered := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if stateFilter != "" && job.State != stateFilter {
			continue
		}
		if repoFilter != "" {
			payload, err := daemonJobPayload(job)
			if err != nil || payload.Repo != repoFilter {
				continue
			}
		}
		filtered = append(filtered, job)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].ID < filtered[j].ID
	})
	return filtered
}

func printJob(stdout io.Writer, job db.Job, payload workflow.JobPayload, reason stuckReason) {
	fmt.Fprintf(stdout, "id: %s\n", job.ID)
	fmt.Fprintf(stdout, "state: %s\n", job.State)
	if !reason.empty() {
		fmt.Fprintf(stdout, "why_stuck: %s\n", reason.Reason)
		if reason.NextRetryAt != "" {
			fmt.Fprintf(stdout, "next_retry_at: %s\n", reason.NextRetryAt)
		}
		if reason.SuggestedAction != "" {
			fmt.Fprintf(stdout, "suggested_action: %s\n", reason.SuggestedAction)
		}
	}
	fmt.Fprintf(stdout, "type: %s\n", job.Type)
	fmt.Fprintf(stdout, "agent: %s\n", job.Agent)
	fmt.Fprintf(stdout, "repo: %s\n", payload.Repo)
	fmt.Fprintf(stdout, "branch: %s\n", payload.Branch)
	fmt.Fprintf(stdout, "pull_request: %d\n", payload.PullRequest)
	if payload.Result != nil {
		fmt.Fprintf(stdout, "decision: %s\n", payload.Result.Decision)
		fmt.Fprintf(stdout, "summary: %s\n", payload.Result.Summary)
	}
	if len(payload.RawOutputs) > 0 {
		fmt.Fprintf(stdout, "raw_outputs: %d retained locally\n", len(payload.RawOutputs))
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err == nil {
		fmt.Fprintf(stdout, "payload: %s\n", string(encoded))
	}
}
