package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// findingsDeclaration renders a repository's #1969 findings-consumption
// declaration. An unset value prints as "undeclared" rather than as its
// behavioural equivalent "consuming", because the whole point of the column is
// that a repository nobody has decided about is visible as one.
func findingsDeclaration(cfg config.ReviewConfig, repo string) string {
	declared := strings.TrimSpace(cfg.For(repo).FindingsConsumption)
	if declared == "" {
		return "undeclared"
	}
	return strings.ToLower(declared)
}

// runFindings reports the #1822 findings ledger per repository.
//
// WHY THIS EXISTS. Two slices of campaign #1966 asked for the same thing from
// opposite ends. #1969 asked for "a report shows answered-versus-open per
// repository, so a silent consumer is visible without a manual query" - it was
// filed because 211 findings were recorded across five repositories and none
// was ever answered, and nothing surfaced that. #1970 asked that "counting open
// findings on a PR yields only findings that apply to its current head" - it
// was filed because 239 of 263 open findings sat at a head the pull request had
// already moved past. Until now there was no findings command at all, so
// "271 open findings" was a number nobody could act on.
//
// WHAT IT DELIBERATELY DOES NOT DO. It does not compute obligations, and it
// does not reclassify anything. #1970 proposed auto-superseding open findings
// when the head moves; measured through LedgerObligationsAtHead, that DROPS a
// live obligation whenever the new push does not touch the finding's file - an
// open finding is an obligation unconditionally, while a superseded one is
// re-armed only by relevance. So both buckets below are still blocking, and the
// report says so rather than implying the stale ones are inert.
func runFindings(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("findings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "findings does not accept positional arguments")
		return 2
	}

	reviewCfg := loadReviewConfig(*home)
	var rows []db.ReviewFindingConsumption
	if err := withStoreAndPaths(*home, func(_ config.Paths, store *db.Store) error {
		var err error
		rows, err = store.ReviewFindingConsumptionByRepo(context.Background())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "findings: %v\n", err)
		return 1
	}

	if *jsonOutput {
		encoded, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "findings: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}

	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no review findings recorded")
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "REPO\tDECLARED\tFINDINGS\tOPEN\tAT HEAD\tAT EARLIER HEAD\tHEAD UNKNOWN\tANSWERED\tWITHDRAWN\tSUPERSEDED")
	for _, row := range rows {
		fmt.Fprintf(writer, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			row.Repo, findingsDeclaration(reviewCfg, row.Repo), row.Findings, row.Open,
			row.OpenAtCurrentHead, row.OpenAtEarlierHead,
			row.OpenHeadUnknown, row.Answered, row.Withdrawn, row.Superseded)
	}
	if err := writer.Flush(); err != nil {
		fmt.Fprintf(stderr, "findings: %v\n", err)
		return 1
	}
	// The two sentences a reader needs to not misread the table. A repository
	// with findings and zero answered is the #1969 shape; a large AT EARLIER HEAD
	// column is the #1970 shape. Neither column is inert.
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "OPEN findings block a merge at every later head until a reviewer answers or withdraws them,")
	fmt.Fprintln(stdout, "so AT EARLIER HEAD is a backlog nobody has looked at, not a set that has expired.")
	fmt.Fprintln(stdout, "A repository with findings and ANSWERED 0 is recording obligations nothing consumes.")
	fmt.Fprintln(stdout, "DECLARED undeclared means nobody has decided whether this repository consumes findings;")
	fmt.Fprintln(stdout, "it behaves as consuming. advisory records and reports findings without holding the merge.")
	return 0
}
