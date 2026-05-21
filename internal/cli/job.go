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
	case "run":
		return runJobRun(args[1:], stdout, stderr)
	case "retry":
		return runJobRetry(args[1:], stdout, stderr)
	case "cancel":
		return runJobCancel(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown job command %q\n\n", args[0])
		printJobUsage(stderr)
		return 2
	}
}

func printJobUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot job list [--repo owner/repo] [--state state]")
	fmt.Fprintln(w, "  gitmoot job show <id>")
	fmt.Fprintln(w, "  gitmoot job events <id>")
	fmt.Fprintln(w, "  gitmoot job run <id>")
	fmt.Fprintln(w, "  gitmoot job retry <id>")
	fmt.Fprintln(w, "  gitmoot job cancel <id>")
}

func runJobList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo scope as owner/repo")
	state := fs.String("state", "", "job state filter")
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
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		jobs, err = store.ListJobs(context.Background())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job list: %v\n", err)
		return 1
	}
	for _, job := range filterJobs(jobs, *repo, *state) {
		payload, _ := daemonJobPayload(job)
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t#%d\n", job.ID, job.State, job.Type, job.Agent, payload.Repo, payload.PullRequest)
	}
	return 0
}

func runJobShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jobID, ok := parseSingleJobID(fs, args, stderr, "job show")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	job, payload, err := loadJobWithPayload(*home, jobID)
	if err != nil {
		fmt.Fprintf(stderr, "job show: %v\n", err)
		return 1
	}
	printJob(stdout, job, payload)
	return 0
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
		worker := defaultJobWorker(store, stdout)
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

func loadJobWithPayload(home string, jobID string) (db.Job, workflow.JobPayload, error) {
	var job db.Job
	var payload workflow.JobPayload
	err := withStore(home, func(store *db.Store) error {
		var err error
		job, err = store.GetJob(context.Background(), jobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("job %q not found", jobID)
			}
			return err
		}
		payload, err = daemonJobPayload(job)
		return err
	})
	return job, payload, err
}

func printJob(stdout io.Writer, job db.Job, payload workflow.JobPayload) {
	fmt.Fprintf(stdout, "id: %s\n", job.ID)
	fmt.Fprintf(stdout, "state: %s\n", job.State)
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
