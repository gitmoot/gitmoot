package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// The `review list|show|watch|cancel` commands are the `run` commands limited
// to runs of type review (#2601): the thing a seat looks up after
// `review request`.

const reviewRunType = "review"

func runReviewList(args []string, stdout, stderr io.Writer) int {
	return listRuns(args, stdout, stderr, "review list", reviewRunType)
}

func runReviewShow(args []string, stdout, stderr io.Writer) int {
	if code, ok := requireReviewRun(args, "review show", stderr); !ok {
		return code
	}
	return runJobShow(args, stdout, stderr)
}

func runReviewWatch(args []string, stdout, stderr io.Writer) int {
	if code, ok := requireReviewRun(args, "review watch", stderr); !ok {
		return code
	}
	return runJobWatch(args, stdout, stderr)
}

func runReviewCancel(args []string, stdout, stderr io.Writer) int {
	if code, ok := requireReviewRun(args, "review cancel", stderr); !ok {
		return code
	}
	return runJobCancel(args, stdout, stderr)
}

// retainJobsOfType keeps only the jobs of jobType, preserving input order.
func retainJobsOfType(jobs []db.Job, jobType string) []db.Job {
	kept := jobs[:0:0]
	for _, job := range jobs {
		if job.Type == jobType {
			kept = append(kept, job)
		}
	}
	return kept
}

// requireReviewRun checks that the leading <id> in args names a review run
// before a review command hands args to the matching run command. Help and
// argument errors pass through to the run command, which reports them.
func requireReviewRun(args []string, command string, stderr io.Writer) (int, bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
			return 0, true
		}
		fmt.Fprintf(stderr, "%s requires exactly one review id\n", command)
		return 2, false
	}
	id := args[0]
	home := ""
	for i := 1; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if !strings.HasPrefix(args[i], "-") || name != "home" {
			continue
		}
		if !hasValue && i+1 < len(args) {
			value = args[i+1]
		}
		home = value
	}
	var job db.Job
	if err := withStore(home, func(store *db.Store) error {
		var err error
		job, err = store.GetJob(context.Background(), id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("review %q not found", id)
		}
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return 1, false
	}
	if job.Type != reviewRunType {
		fmt.Fprintf(stderr, "%s: %s is a %s run, not a review; use gitmoot run %s\n", command, id, job.Type, strings.TrimPrefix(command, "review "))
		return 1, false
	}
	return 0, true
}
