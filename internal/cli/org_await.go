package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func runOrgAwait(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printOrgAwaitUsage(stdout)
		return 0
	}
	switch args[0] {
	case "review":
		return runOrgAwaitReview(args[1:], stdout, stderr)
	case "list":
		return runOrgAwaitList(args[1:], stdout, stderr)
	case "withdraw":
		return runOrgAwaitWithdraw(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown org await command %q\n", args[0])
		printOrgAwaitUsage(stderr)
		return 2
	}
}

func printOrgAwaitUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot org await review --repo OWNER/REPO --pr NUMBER --head SHA --ttl DURATION [--role ROLE] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org await list [--role ROLE] [--state waiting|satisfied|expired|superseded] [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org await withdraw [--role ROLE] --reason TEXT [--home DIR] ID")
	fmt.Fprintln(w, "Every awaited fact has a required deadline. A matching exact-head review verdict satisfies it. At the deadline a review wait the PR has moved past (a later wait on another head, merged, closed, or a new head) ends superseded with its reason and wakes nobody; any other wait expires, remains queryable, and wakes the waiter (an archived waiter's nearest live ancestor). The waiter may withdraw a waiting fact it no longer needs (for example after cancelling the review on purpose): it ends superseded as \"withdrawn by ROLE: REASON\" and wakes nobody.")
}

func runOrgAwaitReview(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org await review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	roleFlag := fs.String("role", "", "organization role waiting for the fact")
	repo := fs.String("repo", "", "repository in owner/repo form")
	pullRequest := fs.Int("pr", 0, "pull request number")
	head := fs.String("head", "", "exact pull request head SHA")
	ttl := fs.Duration("ttl", 0, "maximum wait duration (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *ttl <= 0 {
		fmt.Fprintln(stderr, "org await review requires --repo, --pr, --head, and a positive --ttl")
		return 2
	}
	role := strings.ToLower(strings.TrimSpace(*roleFlag))
	if role == "" {
		role = strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org await review: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org await review: %v\n", err)
		return 1
	}
	if _, ok := cfg.Role(role); !ok {
		fmt.Fprintf(stderr, "org await review: unknown waiter role %q\n", role)
		return 2
	}
	key, err := db.ReviewVerdictSubjectKey(*repo, *pullRequest, *head)
	if err != nil {
		fmt.Fprintf(stderr, "org await review: %v\n", err)
		return 2
	}
	var fact db.AwaitedFact
	err = withStore(*home, func(store *db.Store) error {
		var subscribeErr error
		var skipped []db.HeadlessReviewSkip
		fact, skipped, subscribeErr = store.SubscribeAwaitedFact(context.Background(), db.AwaitedFactSubscription{
			WaiterRole: role, SubjectKind: db.AwaitedFactSubjectReviewVerdict,
			SubjectKey: key, Deadline: time.Now().UTC().Add(*ttl),
		})
		if subscribeErr != nil {
			return subscribeErr
		}
		// BEHAVIOUR UNCHANGED and no row gains a head. A headless review still
		// cannot satisfy a review_verdict awaited fact, because it names no head
		// to satisfy it at. Saying so is the only change (#2008).
		//
		// The decision happens HERE rather than in db: the store reports the
		// skipped rows raw, and the Agent-first/reason vocabulary lives in
		// workflow, which db cannot import.
		for _, skip := range skipped {
			reason, excluded := workflow.HeadBoundExclusion(skip.ExternallyDriven, "")
			if !excluded {
				continue
			}
			if err := workflow.RecordHeadBoundExclusion(context.Background(), store, skip.JobID,
				"awaited_facts.reviewVerdict", reason); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "org await review: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "awaited fact %d state=%s subject=%s deadline=%s\n", fact.ID, fact.State, fact.SubjectKey, fact.Deadline)
	return 0
}

// runOrgAwaitWithdraw lets the waiter end a wait it no longer needs (#2484).
// Without it a deliberate review cancel left the wait open until its deadline,
// and every review_cancelled notice told the waiter to retry the review.
func runOrgAwaitWithdraw(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org await withdraw", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	roleFlag := fs.String("role", "", "organization role that is waiting (defaults to GITMOOT_ORG_ROLE)")
	reason := fs.String("reason", "", "why the wait is no longer needed (required, recorded on the fact)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "org await withdraw requires one awaited fact ID and --reason")
		return 2
	}
	id, err := strconv.ParseInt(strings.TrimSpace(fs.Arg(0)), 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(stderr, "org await withdraw: invalid awaited fact ID %q\n", fs.Arg(0))
		return 2
	}
	role := strings.ToLower(strings.TrimSpace(*roleFlag))
	if role == "" {
		role = strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	}
	if role == "" {
		fmt.Fprintln(stderr, "org await withdraw: --role or GITMOOT_ORG_ROLE is required")
		return 2
	}
	var fact db.AwaitedFact
	err = withStore(*home, func(store *db.Store) error {
		var withdrawErr error
		fact, withdrawErr = store.WithdrawAwaitedFact(context.Background(), id, role, *reason, time.Now().UTC())
		return withdrawErr
	})
	if err != nil {
		fmt.Fprintf(stderr, "org await withdraw: %v\n", err)
		if errors.Is(err, db.ErrAwaitedFactNotWithdrawable) {
			return 2
		}
		return 1
	}
	fmt.Fprintf(stdout, "awaited fact %d state=%s subject=%s detail=%s\n", fact.ID, fact.State, fact.SubjectKey, fact.ResolutionDetail)
	return 0
}

func runOrgAwaitList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org await list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	roleFlag := fs.String("role", "", "filter by waiter role")
	state := fs.String("state", "", "filter by waiting, satisfied, expired, or superseded")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org await list does not accept positional arguments")
		return 2
	}
	role := strings.ToLower(strings.TrimSpace(*roleFlag))
	var facts []db.AwaitedFact
	if err := withStore(*home, func(store *db.Store) error {
		var listErr error
		facts, listErr = store.ListAwaitedFacts(context.Background(), role, *state)
		return listErr
	}); err != nil {
		fmt.Fprintf(stderr, "org await list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoded, err := json.MarshalIndent(facts, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "org await list: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}
	for _, fact := range facts {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", strconv.FormatInt(fact.ID, 10), fact.State, fact.WaiterRole, fact.SubjectKey)
	}
	return 0
}
