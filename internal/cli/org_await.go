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
	default:
		fmt.Fprintf(stderr, "unknown org await command %q\n", args[0])
		printOrgAwaitUsage(stderr)
		return 2
	}
}

func printOrgAwaitUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot org await review --repo OWNER/REPO --pr NUMBER --head SHA --ttl DURATION [--role ROLE] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org await list [--role ROLE] [--state waiting|satisfied|expired] [--json] [--home DIR]")
	fmt.Fprintln(w, "Every awaited fact has a required deadline. A matching exact-head review verdict satisfies it; expiry remains queryable and wakes the waiter's parent.")
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
		fact, subscribeErr = store.SubscribeAwaitedFact(context.Background(), db.AwaitedFactSubscription{
			WaiterRole: role, SubjectKind: db.AwaitedFactSubjectReviewVerdict,
			SubjectKey: key, Deadline: time.Now().UTC().Add(*ttl),
		})
		return subscribeErr
	})
	if err != nil {
		fmt.Fprintf(stderr, "org await review: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "awaited fact %d state=%s subject=%s deadline=%s\n", fact.ID, fact.State, fact.SubjectKey, fact.Deadline)
	return 0
}

func runOrgAwaitList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org await list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	roleFlag := fs.String("role", "", "filter by waiter role")
	state := fs.String("state", "", "filter by waiting, satisfied, or expired")
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
