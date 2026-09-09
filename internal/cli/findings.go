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
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/subprocess"
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
	// #2097: STATE IS NOT "DOES THIS STILL BLOCK", AND THE DIFFERENCE IS A HEAD.
	//
	// The rows this command already prints carry STATE, which is the last recorded
	// observation. The question a merge asks is different: at THIS head, what does
	// the gate still demand? LedgerObligationsAtHead answers it, and its answer is
	// not derivable from any state column - an ANSWERED finding is mandatory again
	// when the diff since the answer touches its relevance keys.
	//
	// So this flag does not filter the rows below. It asks the gate's own
	// predicate, through the same LedgerResolvers the daemon and the merge gate
	// hold, so the CLI cannot answer differently from the thing that blocks.
	atHead := fs.String("at-head", "", "with --repo and --pr, list the obligations the merge gate would still demand at this head")
	// #1971: THE CLASS GREW FROM 198 FINDINGS TO 288 IN TWO DAYS WITH NOTHING
	// REPORTING IT. Merged pull requests carrying unresolved obligations are
	// invisible: the merge is done, the PR is closed, and the ledger rows sit
	// there. Of 28 such pull requests measured on this box, 23 had NO merge_gates
	// row at all - gitmoot's gate was never asked, because main carries no branch
	// protection - and zero of the 288 findings were ever refused.
	//
	// This is the only clause of #1971 that does not depend on the merge path: a
	// stricter gate would have stopped none of them, and a report would have
	// surfaced all of them.
	mergedUnresolved := fs.Bool("merged-unresolved", false, "list merged pull requests that still carried unresolved findings at their branch head (gate obligations plus findings open at that exact head)")
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
	if !*mergedUnresolved && (strings.TrimSpace(*repo) == "") != (*pullRequest == 0) {
		fmt.Fprintln(stderr, "findings: --repo and --pr must be given together; a finding UID is unique only within one repository's pull request")
		return 2
	}
	// NAME THE ACCEPTED COMBINATION, NOT THE INVALID ONE (#2086 f3's lesson,
	// applied to the flag this change adds). A refusal that says only "invalid"
	// leaves the caller to guess which of four flags to move.
	if strings.TrimSpace(*atHead) != "" && strings.TrimSpace(*repo) == "" {
		fmt.Fprintln(stderr, "findings: --at-head requires --repo and --pr; obligations are computed for one pull request at one head")
		return 2
	}
	if *mergedUnresolved {
		if strings.TrimSpace(*atHead) != "" || *pullRequest != 0 {
			fmt.Fprintln(stderr, "findings: --merged-unresolved scans every merged pull request; it accepts --repo alone to narrow the scan, and neither --pr nor --at-head")
			return 2
		}
		return runFindingsMergedUnresolved(strings.TrimSpace(*repo), *home, *jsonOutput, stdout, stderr)
	}
	if strings.TrimSpace(*atHead) != "" {
		return runFindingsObligations(strings.TrimSpace(*repo), *pullRequest, strings.TrimSpace(*atHead), *home, *jsonOutput, stdout, stderr)
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

// findingsObligation is the printable form of one obligation the merge gate
// would still demand. It exists so --json has a stable shape that is NOT
// db.ReviewFindingObservation: an obligation is a question about a head, and
// serialising the row would invite a consumer to read STATE off it and reach the
// opposite conclusion.
type findingsObligation struct {
	FindingUID string `json:"finding_uid"`
	RoundLabel string `json:"round_label,omitempty"`
	Severity   string `json:"severity"`
	Reason     string `json:"reason"`
}

// findingsObligationsReport carries the obligations plus what could not be
// resolved while computing them.
//
// DEGRADATIONS ARE PART OF THE ANSWER, NOT A LOG LINE. LedgerScope degrades
// rather than rejecting: with no changed-file resolver, answered findings stay
// ADVISORY and simply do not appear. That under-reports, and a caller reading an
// empty list has no way to tell "nothing is demanded" from "the instrument could
// not look". The engine already emits these as task events; a CLI has no task,
// so they are collected and printed.
type findingsObligationsReport struct {
	Repo         string               `json:"repo"`
	PullRequest  int                  `json:"pull_request"`
	Head         string               `json:"head"`
	Obligations  []findingsObligation `json:"obligations"`
	Degradations []string             `json:"degradations,omitempty"`
	// Advisory carries the repository's #1969 findings-consumption declaration.
	// When true the obligations below are RECORDED AND NOT HELD: the merge gate
	// waives them. A consumer that ignores this field will report an advisory
	// repository as blocked.
	Advisory bool `json:"advisory"`
}

// runFindingsObligations answers "what would the merge gate still demand at this
// head" for one pull request (#2097).
//
// IT CALLS THE GATE'S OWN PREDICATE THROUGH THE GATE'S OWN CONSTRUCTOR.
// workflow.LedgerResolvers.ScopeFor is documented as the only production path to
// a LedgerScope, and daemonLedgerResolvers is the single place this binary builds
// one - the same value the daemon hands to both the review brief and the merge
// gate. Reimplementing the predicate here, or hand-building a scope, would
// create the second convention #2097 exists to prevent: the CLI would answer a
// question the gate does not ask.
func runFindingsObligations(repo string, pullRequest int, head string, home string, jsonOutput bool, stdout, stderr io.Writer) int {
	ctx := context.Background()
	report := findingsObligationsReport{Repo: repo, PullRequest: pullRequest, Head: head, Obligations: []findingsObligation{}}

	var rows []db.ReviewFindingObservation
	var checkout string
	if err := withStoreAndPaths(home, func(_ config.Paths, store *db.Store) error {
		var err error
		rows, err = store.ListReviewFindingObservations(ctx, repo, int64(pullRequest))
		if err != nil {
			return err
		}
		// A missing checkout is a DEGRADATION, not a failure: the predicate still
		// answers for open findings, which are mandatory unconditionally.
		//
		// #2099 f2: THE NOTE STATES THE FACT, NOT ITS ASSUMED CONSEQUENCE. The
		// first version said "answered findings are advisory" here, which is a
		// claim about the RESOLVERS - and it was often false, because
		// github.NewClient("") is non-nil and daemonLedgerChangedFiles still
		// installs the compare-API fallback. An answered finding could then be
		// correctly re-armed and printed while the note said it could not be.
		//
		// So this reports only what it knows: the checkout is missing. What that
		// costs is derived below, from the resolvers that actually exist.
		checkout, err = mergeGateCheckout(ctx, store, repo, "")
		if err != nil {
			report.Degradations = append(report.Degradations,
				fmt.Sprintf("no registered checkout for %s: %v", repo, err))
			checkout = ""
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "findings: %v\n", err)
		return 1
	}

	resolvers := daemonLedgerResolvers(github.NewClient(checkout), checkout, subprocess.ExecRunner{})
	scope := resolvers.ScopeFor(repo, pullRequest, "")
	// #2099 f1: CARRY THE REPOSITORY'S DECLARATION, BECAUSE THE GATE DOES.
	//
	// The production path resolves findings_consumption into
	// MergeRequest.FindingsAdvisory, copies it onto the LedgerScope, and
	// EnsureLedgerObligationsObserved then returns nil for exactly these pending
	// obligations when the repository is advisory. Printing them without the
	// declaration reports an advisory repository as blocked by obligations its
	// own gate waives - which is the CLI answering differently from the thing
	// that blocks, the one property this command exists to guarantee.
	//
	// The obligations are still LISTED, because advisory means recorded and not
	// held, never invisible (#1969). What changes is the sentence beside them.
	// DERIVED FROM THE RESOLVERS THAT EXIST, not from what produced them. Each
	// missing half has a different, specific consequence, and naming the wrong
	// one is what #2099 f2 caught.
	if resolvers.ChangedSince == nil {
		report.Degradations = append(report.Degradations,
			"no changed-file resolver, so an ANSWERED finding cannot be re-armed by relevance and will not appear here")
	}
	if resolvers.PathExistsAtHead == nil {
		report.Degradations = append(report.Degradations,
			"no locator resolver, so a STATIC answer citing a deleted path still counts as an answer")
	}
	advisory := loadReviewConfig(home).For(repo).FindingsAreAdvisory()
	report.Advisory = advisory
	scope.FindingsAdvisory = advisory
	scope.Repo = repo
	scope.Degraded = func(note string) {
		report.Degradations = append(report.Degradations, note)
	}
	for _, obligation := range workflow.LedgerObligationsAtHead(ctx, rows, head, scope) {
		report.Obligations = append(report.Obligations, findingsObligation{
			FindingUID: obligation.FindingUID,
			RoundLabel: obligation.RoundLabel,
			Severity:   obligation.Severity,
			Reason:     obligation.Reason,
		})
	}

	if jsonOutput {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "findings: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}

	if len(report.Obligations) == 0 {
		fmt.Fprintf(stdout, "no obligations for %s#%d at %s\n", repo, pullRequest, shortFindingsHead(head))
	} else if advisory {
		fmt.Fprintf(stdout, "%s declares findings_consumption = advisory: the merge gate WAIVES these and does not hold the merge.\n", repo)
		fmt.Fprintln(stdout, "They are listed because advisory means recorded, not invisible.")
	}
	if len(report.Obligations) > 0 {
		writer := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(writer, "UID\tSEV\tROUND\tREASON")
		for _, obligation := range report.Obligations {
			round := obligation.RoundLabel
			if strings.TrimSpace(round) == "" {
				round = "(unlabelled)"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", obligation.FindingUID, obligation.Severity, round, obligation.Reason)
		}
		if err := writer.Flush(); err != nil {
			fmt.Fprintf(stderr, "findings: %v\n", err)
			return 1
		}
	}
	// PRINT WHAT COULD NOT BE RESOLVED, ALWAYS, INCLUDING BESIDE AN EMPTY LIST.
	// An empty obligation list and an unresolvable instrument read identically
	// otherwise, and the empty one reads as good news.
	for _, note := range report.Degradations {
		fmt.Fprintf(stdout, "degraded: %s\n", note)
	}
	return 0
}

// shortFindingsHead abbreviates a head for human output without hiding a value
// that is not a sha at all.
func shortFindingsHead(head string) string {
	head = strings.TrimSpace(head)
	if len(head) > 8 {
		return head[:8]
	}
	return head
}

// findingsMergedUnresolved is one merged pull request that still carried
// unresolved findings at its branch head. Its Obligations are the UNION of the
// gate's own LedgerObligationsAtHead set and findings whose latest observation
// at that exact head is still open, which the gate's dischargedAtHead step
// removes.
//
// It is a SUPERSET of what the gate would demand, and MAY BE A PROPER ONE. It
// is NOT always proper: the exact-head addend is empty whenever no finding's
// latest observation sits at the merged head, and then the union equals the
// gate set exactly - TestMergedUnresolvedKeysOnTheBranchHeadNotTheMergeCommit
// is that case, its only finding being at an earlier head (#2106 f4 corrected
// an earlier version of this comment that claimed equality was impossible).
//
// What must NOT come back is the reduction to "obligations the merge gate would
// have demanded" (#2106 f3 was exactly that sentence, twice): the report can
// exceed the gate, even though it does not always.
type findingsMergedUnresolved struct {
	Repo        string `json:"repo"`
	PullRequest int64  `json:"pull_request"`
	HeadSHA     string `json:"head_sha"`
	MergedAt    string `json:"merged_at,omitempty"`
	// Advisory is the repository's CURRENT findings_consumption setting, read at
	// report time. It is NOT a record of what was true when the PR merged.
	Advisory    bool                 `json:"advisory"`
	Obligations []findingsObligation `json:"obligations"`
}

// findingsMergedUnresolvedReport carries the rows plus what the scan covered and
// what it could not resolve.
//
// SCANNED IS PART OF THE ANSWER. An empty list from a scan of seven
// repositories and an empty list from a scan of zero read identically, and the
// second is an instrument failure wearing a success message. That defect
// shipped once in this command already (#2086 f3), so the counts are reported
// rather than left for a reader to assume.
type findingsMergedUnresolvedReport struct {
	ScannedRepos        int                        `json:"scanned_repos"`
	ScannedPullRequests int                        `json:"scanned_pull_requests"`
	Merged              int                        `json:"merged_pull_requests"`
	Unresolved          []findingsMergedUnresolved `json:"unresolved"`
	Degradations        []string                   `json:"degradations,omitempty"`
}

// runFindingsMergedUnresolved reports merged pull requests that still carried
// unresolved findings at their branch head (#1971 clause 3). The reported set is
// the UNION described on findingsMergedUnresolved: gate obligations plus
// findings still open at that exact head.
//
// IT KEYS ON THE BRANCH HEAD, NEVER THE MERGE COMMIT, and that is not a
// preference. Every merge in this repository is a SQUASH, so the merge commit is
// a commit no reviewer ever observed: measured across 28 merged pull requests
// carrying findings, the ledger holds 19 with an observation at the branch head
// and ZERO at the merge commit, with the two SHAs never equal.
//
// The failure is silent in both directions depending on how the caller assembles
// its input. Filter observations to the merge sha first and the predicate sees
// an empty input and reports a clean list forever. Pass the merge sha as the
// head instead, and dischargedAtHead - which discharges only on an exact head
// match - never discharges anything, so every answered row reappears as an
// obligation. Empty or inflated, and neither announces itself.
func runFindingsMergedUnresolved(repoFilter string, home string, jsonOutput bool, stdout, stderr io.Writer) int {
	ctx := context.Background()
	report := findingsMergedUnresolvedReport{Unresolved: []findingsMergedUnresolved{}}

	var pairs []db.ReviewFindingPullRequest
	observations := map[string][]db.ReviewFindingObservation{}
	checkouts := map[string]string{}
	if err := withStoreAndPaths(home, func(_ config.Paths, store *db.Store) error {
		var err error
		pairs, err = store.ListReviewFindingPullRequests(ctx)
		if err != nil {
			return err
		}
		repos := map[string]bool{}
		for _, pair := range pairs {
			if repoFilter != "" && !strings.EqualFold(pair.Repo, repoFilter) {
				continue
			}
			repos[pair.Repo] = true
			rows, listErr := store.ListReviewFindingObservations(ctx, pair.Repo, pair.PullRequest)
			if listErr != nil {
				return listErr
			}
			observations[findingsPairKey(pair)] = rows
		}
		for repo := range repos {
			checkout, checkoutErr := mergeGateCheckout(ctx, store, repo, "")
			if checkoutErr != nil {
				report.Degradations = append(report.Degradations,
					fmt.Sprintf("no registered checkout for %s: %v", repo, checkoutErr))
				continue
			}
			checkouts[repo] = checkout
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "findings: %v\n", err)
		return 1
	}

	reviewCfg := loadReviewConfig(home)
	seenRepos := map[string]bool{}
	for _, pair := range pairs {
		if repoFilter != "" && !strings.EqualFold(pair.Repo, repoFilter) {
			continue
		}
		seenRepos[pair.Repo] = true
		report.ScannedPullRequests++

		owner, name, ok := strings.Cut(pair.Repo, "/")
		if !ok {
			report.Degradations = append(report.Degradations, fmt.Sprintf("unparseable repo %q, skipped", pair.Repo))
			continue
		}
		checkout := checkouts[pair.Repo]
		pull, err := newFindingsGitHubClient(checkout).GetPullRequest(ctx, github.Repository{Owner: owner, Name: name}, pair.PullRequest)
		if err != nil {
			// A pull request the forge cannot answer for is UNKNOWN, not clean.
			report.Degradations = append(report.Degradations,
				fmt.Sprintf("%s#%d could not be read from the forge, so it is neither included nor cleared: %v", pair.Repo, pair.PullRequest, err))
			continue
		}
		if !pull.Merged && strings.TrimSpace(pull.MergedAt) == "" {
			continue
		}
		report.Merged++

		head := strings.TrimSpace(pull.HeadSHA)
		if head == "" {
			report.Degradations = append(report.Degradations,
				fmt.Sprintf("%s#%d reports no head sha, so its obligations cannot be evaluated", pair.Repo, pair.PullRequest))
			continue
		}

		resolvers := daemonLedgerResolvers(github.NewClient(checkout), checkout, subprocess.ExecRunner{})
		scope := resolvers.ScopeFor(pair.Repo, int(pair.PullRequest), "")
		scope.Repo = pair.Repo
		scope.FindingsAdvisory = reviewCfg.For(pair.Repo).FindingsAreAdvisory()
		scope.Degraded = func(note string) {
			report.Degradations = append(report.Degradations, fmt.Sprintf("%s#%d: %s", pair.Repo, pair.PullRequest, note))
		}

		rows := observations[findingsPairKey(pair)]
		var obligations []findingsObligation
		seen := map[string]bool{}
		for _, obligation := range workflow.LedgerObligationsAtHead(ctx, rows, head, scope) {
			seen[obligation.FindingUID] = true
			obligations = append(obligations, findingsObligation{
				FindingUID: obligation.FindingUID,
				RoundLabel: obligation.RoundLabel,
				Severity:   obligation.Severity,
				Reason:     obligation.Reason,
			})
		}
		// #2106 f1: A FINDING OPEN AT THE MERGE HEAD IS THE MOST DAMNING CASE AND
		// THE GATE'S PREDICATE DISCHARGES IT.
		//
		// dischargedAtHead removes any finding observed AT the head being judged,
		// because the gate asks "what must a NEW review at this head still
		// observe" - and a row already recorded there has been observed. That is
		// correct for the gate and WRONG FOR THIS REPORT, which asks the opposite
		// question: "what was still unresolved when this merged". A P1 recorded at
		// the exact head that merged produced Merged:1 and Unresolved:[].
		//
		// THIS IS NOT A SECOND CONVENTION. The obligation predicate stays the sole
		// authority on obligations; this adds the disjoint set it deliberately
		// excludes - findings whose LATEST observation at this head is still OPEN -
		// and labels them distinctly so a reader can tell the two apart.
		for _, row := range workflow.LatestObservationsInOrder(rows) {
			if seen[row.FindingUID] || row.State != db.FindingOpen {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(row.HeadSHA), head) {
				continue
			}
			obligations = append(obligations, findingsObligation{
				FindingUID: row.FindingUID,
				RoundLabel: row.RoundLabel,
				Severity:   row.Severity,
				Reason:     "still open at the merged head",
			})
		}
		if len(obligations) == 0 {
			continue
		}
		report.Unresolved = append(report.Unresolved, findingsMergedUnresolved{
			Repo: pair.Repo, PullRequest: pair.PullRequest, HeadSHA: head,
			MergedAt: strings.TrimSpace(pull.MergedAt),
			Advisory: scope.FindingsAdvisory, Obligations: obligations,
		})
	}
	report.ScannedRepos = len(seenRepos)

	if jsonOutput {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "findings: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}

	fmt.Fprintf(stdout, "scanned %d repositor%s, %d pull request(s) with findings, %d merged\n",
		report.ScannedRepos, map[bool]string{true: "y", false: "ies"}[report.ScannedRepos == 1],
		report.ScannedPullRequests, report.Merged)
	if len(report.Unresolved) == 0 {
		fmt.Fprintln(stdout, "no merged pull request carries an unresolved obligation")
	} else {
		writer := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(writer, "PR\tHEAD\tMERGED\tWAIVED\tUID\tSEV\tREASON")
		for _, row := range report.Unresolved {
			for _, obligation := range row.Obligations {
				fmt.Fprintf(writer, "%s#%d\t%s\t%s\t%t\t%s\t%s\t%s\n",
					row.Repo, row.PullRequest, shortFindingsHead(row.HeadSHA), row.MergedAt,
					row.Advisory, obligation.FindingUID, obligation.Severity, obligation.Reason)
			}
		}
		if err := writer.Flush(); err != nil {
			fmt.Fprintf(stderr, "findings: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout)
		// #2106 f3: THE REPORT IS A UNION AND SAYING OTHERWISE IS NOW FALSE.
		// The f1 fix deliberately adds rows LedgerObligationsAtHead excludes, so
		// this line can no longer claim every item is an obligation the gate
		// would demand. The disagreement is one-directional and by design: the
		// report is a superset of what the gate asks for and never a subset. It
		// is not always a PROPER superset: with no exact-head-open findings the
		// two sets coincide (#2106 f4).
		fmt.Fprintln(stdout, "These merged while still carrying unresolved findings at their branch head.")
		fmt.Fprintln(stdout, "Rows are the UNION of two sets: obligations the gate would have demanded, and")
		fmt.Fprintln(stdout, "findings still open at that exact head, which the gate discharges. The second")
		fmt.Fprintln(stdout, "kind carries the reason \"still open at the merged head\", so this report is a")
		fmt.Fprintln(stdout, "superset of the gate rather than the same predicate. With no findings of the")
		fmt.Fprintln(stdout, "second kind the two coincide.")
		// #2106 f2: THIS COLUMN IS TODAY'S POLICY, NOT A HISTORICAL AUTHORISATION.
		// It is read from the CURRENT review configuration, so saying it proves the
		// gate let a past merge through by declaration reverses history whenever
		// findings_consumption has changed since: an accidental bypass reads as
		// authorized, or an authorized one as accidental. Stated as current policy,
		// with no causal claim about the merge that already happened.
		fmt.Fprintln(stdout, "WAIVED reflects the repository's CURRENT findings_consumption setting, read now.")
		fmt.Fprintln(stdout, "It is not evidence about the merge: the setting may have changed since, and no")
		fmt.Fprintln(stdout, "durable merge-time record of it exists.")
	}
	for _, note := range report.Degradations {
		fmt.Fprintf(stdout, "degraded: %s\n", note)
	}
	return 0
}

func findingsPairKey(pair db.ReviewFindingPullRequest) string {
	return fmt.Sprintf("%s#%d", pair.Repo, pair.PullRequest)
}

// newFindingsGitHubClient is the forge seam for the merged-unresolved scan,
// following the convention agent_dispatch.go:26 already uses in this package
// rather than introducing a second one.
var newFindingsGitHubClient = func(checkout string) github.Client {
	return github.NewClient(checkout)
}
