package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// The `review list|show|watch|cancel` commands are the `run` commands limited
// to runs of type review (#2601): the thing a seat looks up after
// `review request`. Each one runs the shared run handler with a type
// restriction, so the flags are parsed once and the type is checked on the
// same run, in the same home, that the handler prints or cancels.

const reviewRunType = "review"

func runReviewList(args []string, stdout, stderr io.Writer) int {
	return listRuns(args, stdout, stderr, "review list", reviewRunType)
}

func runReviewShow(args []string, stdout, stderr io.Writer) int {
	return showRun(args, stdout, stderr, "review show", reviewRunType)
}

func runReviewWatch(args []string, stdout, stderr io.Writer) int {
	return watchRun(args, stdout, stderr, "review watch", reviewRunType)
}

func runReviewCancel(args []string, stdout, stderr io.Writer) int {
	return cancelRun(args, stdout, stderr, "review cancel", reviewRunType)
}

// checkRunType refuses a run whose type is not onlyType; an empty onlyType
// accepts every type.
func checkRunType(job db.Job, onlyType, command string) error {
	if onlyType == "" || job.Type == onlyType {
		return nil
	}
	verb := command[strings.LastIndexByte(command, ' ')+1:]
	return fmt.Errorf("%s is a %s run, not a %s; use gitmoot run %s", job.ID, job.Type, onlyType, verb)
}
