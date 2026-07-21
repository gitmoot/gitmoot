package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

// runPipelineResume is `gitmoot pipeline resume <run> [--from <stage>]`: it re-runs a
// parked run from its halted stage (or the --from stage) via pipeline.ResumePipelineRun; the
// next daemon scan re-enqueues the reset stages.
func runPipelineResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	from := fs.String("from", "", "stage id to resume from (defaults to the halted stage)")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline resume requires a run id")
			return 2
		}
		return 0
	}
	runID := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline resume accepts exactly one run id")
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		_, err := pipeline.ResumePipelineRun(context.Background(), store, runID, *from)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline resume: %v\n", err)
		return 1
	}
	writeLine(stdout, "resumed run %s", runID)
	return 0
}

// runPipelineCancel is `gitmoot pipeline cancel <run>`: it abandons a run, cancelling
// its in-flight stage jobs through the shared CancelJob path and marking the run and
// its non-terminal stages cancelled.
func runPipelineCancel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline cancel requires a run id")
			return 2
		}
		return 0
	}
	runID := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline cancel accepts exactly one run id")
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		_, err := pipeline.CancelPipelineRun(context.Background(), store, runID, time.Now().UTC())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline cancel: %v\n", err)
		return 1
	}
	writeLine(stdout, "cancelled run %s", runID)
	return 0
}
