package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// A hold is a journal note, so who set it and why stays on record:
//
//	[pr:hold repo=owner/repo pr=N head=<sha> by=<role>] reason
//	[pr:hold-clear repo=owner/repo pr=N by=<role>]
//
// The newest note of a pull request decides; a hold silences the pull request
// only while its head is the held head.
const (
	prHoldNotePrefix      = "[pr:hold"
	prHoldNoteMarker      = "[pr:hold "
	prHoldClearNoteMarker = "[pr:hold-clear "
)

type prHold struct {
	Head   string
	By     string
	Reason string
	At     string
}

func formatPRHoldNote(repo string, pr int, head, by, reason string) string {
	return fmt.Sprintf("%srepo=%s pr=%d head=%s by=%s] %s", prHoldNoteMarker, repo, pr, strings.ToLower(head), by, strings.TrimSpace(reason))
}

func formatPRHoldClearNote(repo string, pr int, by string) string {
	return fmt.Sprintf("%srepo=%s pr=%d by=%s]", prHoldClearNoteMarker, repo, pr, by)
}

// parsePRHoldNote reads a hold or clear note; head is "" for a clear.
func parsePRHoldNote(body string) (repo string, pr int, hold prHold, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(body, prHoldNoteMarker):
		rest = strings.TrimPrefix(body, prHoldNoteMarker)
	case strings.HasPrefix(body, prHoldClearNoteMarker):
		rest = strings.TrimPrefix(body, prHoldClearNoteMarker)
	default:
		return "", 0, prHold{}, false
	}
	header, reason, found := strings.Cut(rest, "]")
	if !found {
		return "", 0, prHold{}, false
	}
	for _, field := range strings.Fields(header) {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "repo":
			repo = value
		case "pr":
			pr, _ = strconv.Atoi(value)
		case "head":
			hold.Head = value
		case "by":
			hold.By = value
		}
	}
	hold.Reason = strings.TrimSpace(reason)
	return repo, pr, hold, repo != "" && pr > 0
}

// listPRHolds returns repo's active hold notes by pull request: the newest
// hold or clear note of each pull request, when it is a hold. Every hold note
// of the repo is read, with no window: a busy pull request must not push an
// older pull request's decision out of view.
func listPRHolds(ctx context.Context, store *db.Store, repo string) (map[int]prHold, error) {
	notes, err := store.ListAllRepoWorkflowNotesByBodyPrefix(ctx, prHoldNotePrefix, repo)
	if err != nil {
		return nil, err
	}
	holds := map[int]prHold{}
	decided := map[int]bool{}
	for _, note := range notes { // newest first
		noteRepo, pr, hold, ok := parsePRHoldNote(note.Body)
		if !ok || !strings.EqualFold(noteRepo, repo) || decided[pr] {
			continue
		}
		decided[pr] = true
		if hold.Head != "" {
			hold.At = note.CreatedAt
			holds[pr] = hold
		}
	}
	return holds, nil
}

func runPRStalled(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr stalled", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "only this repository (owner/repo); default every enabled repository [pr_stalled] covers")
	jsonOutput := fs.Bool("json", false, "print the reports as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pr stalled takes no arguments")
		return 2
	}
	repoFilter := strings.TrimSpace(*repoFlag)
	if repoFilter != "" {
		if _, err := github.ParseRepository(repoFilter); err != nil {
			fmt.Fprintf(stderr, "pr stalled: --repo: %v\n", err)
			return 2
		}
	}
	var reports []prStalledReport
	var failures []string
	err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		policy, err := resolvePRStalledPolicy(paths.Home)
		if err != nil {
			return err
		}
		repos := []string{repoFilter}
		if repoFilter == "" {
			repos = nil
			registered, err := store.ListRepos(ctx)
			if err != nil {
				return err
			}
			for _, repo := range registered {
				if repo.Enabled && policy.Covers(repo.FullName()) {
					repos = append(repos, repo.FullName())
				}
			}
		}
		env := newPRStalledEnv(ctx, store, paths.Home, prStalledGitHubClient())
		now := time.Now().UTC()
		for _, repo := range repos {
			report, err := evaluateStalledPullRequests(ctx, store, env, repo, policy, now)
			if err != nil {
				failures = append(failures, err.Error())
				continue
			}
			reports = append(reports, report)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "pr stalled: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if reports == nil {
			reports = []prStalledReport{}
		}
		if err := writeJSON(stdout, reports); err != nil {
			fmt.Fprintf(stderr, "pr stalled: %v\n", err)
			return 1
		}
	} else {
		printPRStalled(stdout, reports)
	}
	for _, failure := range failures {
		fmt.Fprintf(stderr, "pr stalled: %s\n", failure)
	}
	if len(failures) > 0 {
		return 1
	}
	return 0
}

func printPRStalled(w io.Writer, reports []prStalledReport) {
	var stalled []prStalledRow
	var held []prHeldRow
	open := 0
	for _, report := range reports {
		stalled = append(stalled, report.Stalled...)
		held = append(held, report.Held...)
		open += report.Open
		if report.Truncated {
			fmt.Fprintf(w, "%s: %d open pull requests; only the newest %d were evaluated\n", report.Repo, report.Open, len(report.open))
		}
		if len(report.ChecksTruncated) > 0 {
			numbers := make([]string, len(report.ChecksTruncated))
			for i, number := range report.ChecksTruncated {
				numbers[i] = "#" + strconv.Itoa(number)
			}
			fmt.Fprintf(w, "%s: checks truncated (more than 100 contexts), not judged green: %s\n", report.Repo, strings.Join(numbers, ", "))
		}
		if len(report.NoCIConfigured) > 0 {
			numbers := make([]string, len(report.NoCIConfigured))
			for i, number := range report.NoCIConfigured {
				numbers[i] = "#" + strconv.Itoa(number)
			}
			fmt.Fprintf(w, "%s: no CI configured (no workflow files, no required checks), not flagged no_ci_on_head: %s\n", report.Repo, strings.Join(numbers, ", "))
		}
	}
	fmt.Fprintf(w, "%d stalled of %d open pull requests in %d repositories\n", len(stalled), open, len(reports))
	if len(stalled) > 0 {
		table := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(table, "PR\tSTATE\tAGE\tOWNER\tCAUSE\tNEXT")
		for _, row := range stalled {
			owner := row.Owner
			switch {
			case owner == "":
				owner = "(unowned)"
			case row.OwnerSource == ownerSourceRepoOwner:
				owner += " (repo owner)"
			case row.OwnerSource == ownerSourceCoordinator:
				owner += " (coordinator)"
			}
			if row.OwnerGone != "" {
				owner += " (owner gone: " + row.OwnerGone + ")"
			}
			fmt.Fprintf(table, "%s#%d\t%s\t%s\t%s\t%s\t%s\n", row.Repo, row.PullRequest, row.State, row.Age, owner, row.Cause, row.Next)
		}
		_ = table.Flush()
	}
	if len(held) > 0 {
		fmt.Fprintln(w, "\nheld (silenced until the head moves):")
		for _, row := range held {
			fmt.Fprintf(w, "  %s#%d head=%s by %s at %s: %s\n", row.Repo, row.PullRequest, shortReviewHead(row.Head), firstNonEmpty(row.By, "unknown"), row.At, row.Reason)
		}
	}
}

func runPRHold(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr hold", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	reason := fs.String("reason", "", "why the pull request is held (required unless --clear)")
	clear := fs.Bool("clear", false, "remove the hold")
	roleFlag := fs.String("role", "", "acting organization role recorded on the hold (defaults to GITMOOT_ORG_ROLE, then the calling seat's pane)")
	jsonOutput := fs.Bool("json", false, "print the hold as JSON")
	// Accept the PR before or after the flags.
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "pr hold requires exactly one owner/repo#N")
		return 2
	}
	repo, number, err := parseRepoPRRef(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "pr hold: %v\n", err)
		return 2
	}
	if !*clear && strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "pr hold requires --reason TEXT (or --clear)")
		return 2
	}
	if *clear && strings.TrimSpace(*reason) != "" {
		fmt.Fprintln(stderr, "pr hold --clear takes no --reason")
		return 2
	}
	if refuseFlagLikeText("pr hold", *reason, stderr) {
		return 2
	}
	ctx := context.Background()
	role, err := resolveDispatchOrgRole(ctx, *home, *roleFlag)
	if err != nil {
		fmt.Fprintf(stderr, "pr hold: %v\n", err)
		return 1
	}
	if role == "" {
		fmt.Fprintln(stderr, "pr hold: set --role or GITMOOT_ORG_ROLE, or run from a registered seat's pane; the hold records who set it")
		return 2
	}
	if cfg, ok := loadMergeGateOrgConfig(*home); ok && cfg.Enabled() {
		if _, known := cfg.Role(role); !known {
			fmt.Fprintf(stderr, "pr hold: unknown role %q\n", role)
			return 2
		}
	}
	output := struct {
		Repo        string `json:"repo"`
		PullRequest int    `json:"pr"`
		Head        string `json:"head_sha,omitempty"`
		By          string `json:"by"`
		Reason      string `json:"reason,omitempty"`
		Cleared     bool   `json:"cleared"`
	}{Repo: repo.FullName(), PullRequest: number, By: role, Reason: strings.TrimSpace(*reason), Cleared: *clear}
	err = withStore(*home, func(store *db.Store) error {
		body := formatPRHoldClearNote(repo.FullName(), number, role)
		if !*clear {
			pull, err := prStalledGitHubClient().GetPullRequest(ctx, repo, int64(number))
			if err != nil {
				return fmt.Errorf("read %s#%d: %w", repo.FullName(), number, err)
			}
			if !strings.EqualFold(strings.TrimSpace(pull.State), "open") {
				return fmt.Errorf("%s#%d is %s, not open", repo.FullName(), number, strings.ToLower(pull.State))
			}
			output.Head = strings.ToLower(strings.TrimSpace(pull.HeadSHA))
			body = formatPRHoldNote(repo.FullName(), number, output.Head, role, output.Reason)
		}
		_, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{Author: role, Body: body, Repo: repo.FullName()})
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "pr hold: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "pr hold: %v\n", err)
			return 1
		}
		return 0
	}
	if *clear {
		fmt.Fprintf(stdout, "hold cleared on %s#%d by %s\n", output.Repo, number, role)
		return 0
	}
	fmt.Fprintf(stdout, "%s#%d held at head %s by %s until the head moves: %s\n", output.Repo, number, shortReviewHead(output.Head), role, output.Reason)
	return 0
}

// parseRepoPRRef parses owner/repo#N.
func parseRepoPRRef(value string) (github.Repository, int, error) {
	repoPart, numberPart, ok := strings.Cut(strings.TrimSpace(value), "#")
	if !ok {
		return github.Repository{}, 0, fmt.Errorf("%q is not owner/repo#N", value)
	}
	repo, err := github.ParseRepository(repoPart)
	if err != nil {
		return github.Repository{}, 0, fmt.Errorf("%q is not owner/repo#N", value)
	}
	number, err := strconv.Atoi(numberPart)
	if err != nil || number <= 0 {
		return github.Repository{}, 0, fmt.Errorf("%q is not owner/repo#N", value)
	}
	return repo, number, nil
}
