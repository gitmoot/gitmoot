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
	"github.com/gitmoot/gitmoot/internal/workflow"
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
	// #2077 f3: THE BRIEF POINTS HERE AND THIS COULD NOT ANSWER.
	//
	// ledgerObligationBrief is byte-bounded, so once the budget is exhausted it
	// omits mandatory UIDs and tells the reviewer to consult the findings ledger.
	// This command was the ledger's only supported reader and it printed
	// per-repository COUNTS - no rows, no UIDs - while a read-only review seat
	// has no database access at all. The instruction was correct and the
	// destination could not satisfy it.
	//
	// That is why the finding survived three rounds of bounding work: every fix
	// was in the writer, and the missing half was here.
	repo := fs.String("repo", "", "with --pr, list individual findings for this owner/repo instead of the per-repository summary")
	pullRequest := fs.Int("pr", 0, "with --repo, the pull request whose findings to list")
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
	// BOTH OR NEITHER. A UID is unique only within a repository, and the store's
	// reader is keyed on (repo, pull_request) - so a repo without a PR would have
	// silently listed the pull_request=0 rows and reported "no findings" for a
	// repository full of them. Measured: that is exactly what the first version
	// of this command did. Refusing beats a listing that is empty for the wrong
	// reason, which is the failure this whole finding is about.
	if (strings.TrimSpace(*repo) == "") != (*pullRequest == 0) {
		fmt.Fprintln(stderr, "findings: --repo and --pr must be given together; a finding UID is unique only within one repository's pull request")
		return 2
	}
	if strings.TrimSpace(*repo) != "" {
		return runFindingsRows(strings.TrimSpace(*repo), *pullRequest, *home, *jsonOutput, stdout, stderr)
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

// runFindingsRows lists individual ledger rows, which is what the obligation
// brief tells a reviewer to consult (#2077 f3).
//
// It prints the UID first because that is the only thing a reviewer can act on:
// continuing a prior finding requires citing its uid exactly, and typing its old
// label mints a new finding instead. A budgeted brief omits UIDs; this is where
// they are recoverable.
func runFindingsRows(repo string, pullRequest int, home string, jsonOutput bool, stdout, stderr io.Writer) int {
	var rows []db.ReviewFindingObservation
	if err := withStoreAndPaths(home, func(_ config.Paths, store *db.Store) error {
		var err error
		rows, err = store.ListReviewFindingObservations(context.Background(), repo, int64(pullRequest))
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "findings: %v\n", err)
		return 1
	}

	// FOLD BEFORE EVERY OUTPUT PATH (#2086 f1, and its comment lived twelve lines
	// too low until #2086 f4).
	//
	// The store is APPEND-ONLY and its doc comment says so: it returns every
	// observation and does not fold, so a caller folds. The first version printed
	// the raw log, one line per observation, showing a finding as "open" at an old
	// head beside its own "answered" row at a newer one - four lines for one
	// finding on PR #2077. A reviewer sent here by a budgeted brief is asking
	// WHICH OBLIGATIONS ARE OPEN, and a stale open row answers that wrongly using
	// the ledger's own data.
	//
	// The second version folded AFTER the JSON branch, so --json still returned
	// the raw log - the worse half, because JSON is what a dispatch path consumes
	// and a human at least sees the duplicate UIDs and can wonder.
	//
	// The rule is the engine's, not a new one: workflow.LatestObservationsInOrder,
	// including its QUOTED guard.
	rows = workflow.LatestObservationsInOrder(rows)

	if jsonOutput {
		encoded, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "findings: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}

	if len(rows) == 0 {
		// NAME BOTH REASONS. A reviewer sent here by a brief needs to know whether
		// the ledger is empty or their filter missed, and those are different
		// problems with different next steps.
		if pullRequest > 0 {
			fmt.Fprintf(stdout, "no findings recorded for %s#%d\n", repo, pullRequest)
		} else {
			fmt.Fprintf(stdout, "no findings recorded for %s\n", repo)
		}
		return 0
	}

	writer := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "UID\tSEV\tSTATE\tPR\tHEAD\tTITLE")
	for _, row := range rows {
		head := row.HeadSHA
		if len(head) > 8 {
			head = head[:8]
		}
		title := strings.TrimSpace(row.Title)
		if title == "" {
			// A row with no title is the #2072 shape and the reviewer needs to see
			// that it exists rather than a blank line they cannot cite.
			title = "(no title recorded)"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\t%s\n",
			row.FindingUID, row.Severity, row.State, row.PullRequest, head, title)
	}
	if err := writer.Flush(); err != nil {
		fmt.Fprintf(stderr, "findings: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Cite a UID verbatim as \"continues_uid\" to continue a finding; typing its label mints a new one.")
	// #2086 f2: STATE IS NOT THE SAME QUESTION AS "DOES THIS STILL BLOCK".
	// LedgerObligationsAtHead can treat an ANSWERED finding as still mandatory
	// when the files its relevance keys name were touched again after the answer.
	// No column over the raw ledger can show that - it is a function of the head
	// under review, not of the row - so the limit is printed rather than left for
	// a reader to discover by trusting STATE and being wrong.
	fmt.Fprintln(stdout, "STATE is the last recorded observation. An answered finding can still be MANDATORY at a")
	fmt.Fprintln(stdout, "later head if the files it names changed again, which only the merge gate can decide.")
	return 0
}
