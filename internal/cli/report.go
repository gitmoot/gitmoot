package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/report"
)

type reportGitHubClient interface {
	Preflight(ctx context.Context, repo github.Repository) error
	SearchOpenIssues(ctx context.Context, repo github.Repository, text string) ([]github.Issue, error)
	CreateIssue(ctx context.Context, input github.CreateIssueInput) (github.Issue, error)
}

var newReportGitHubClient = func() reportGitHubClient {
	return github.NewClient("")
}

var reportIssueRepo = github.Repository{Owner: "gitmoot", Name: "gitmoot"}

func runReport(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printReportUsage(stdout)
		return 0
	}
	switch args[0] {
	case "bug":
		return runReportBug(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown report command %q\n\n", args[0])
		printReportUsage(stderr)
		return 2
	}
}

func printReportUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot report bug --job <job-id> [--preview]")
	fmt.Fprintln(w, "  gitmoot report bug --job <job-id> --create --yes")
	fmt.Fprintln(w, "  gitmoot report bug --source daemon --preview")
	fmt.Fprintln(w, "  gitmoot report bug --source dashboard --preview")
}

type reportBugOptions struct {
	home    string
	jobID   string
	source  string
	preview bool
	create  bool
	yes     bool
}

func runReportBug(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report bug", flag.ContinueOnError)
	fs.SetOutput(stderr)
	options := reportBugOptions{}
	fs.StringVar(&options.home, "home", "", "home directory to use instead of the current user's home")
	fs.StringVar(&options.jobID, "job", "", "job id to build a report from")
	fs.StringVar(&options.source, "source", "", "future report source: daemon or dashboard")
	fs.BoolVar(&options.preview, "preview", false, "print the report without creating a GitHub issue")
	fs.BoolVar(&options.create, "create", false, "create a GitHub issue")
	fs.BoolVar(&options.yes, "yes", false, "confirm non-interactive issue creation")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "report bug does not accept positional arguments")
		return 2
	}
	if err := validateReportBugOptions(options); err != nil {
		fmt.Fprintf(stderr, "report bug: %v\n", err)
		return 2
	}
	if !options.create {
		options.preview = true
	}

	draft, err := buildBugReport(context.Background(), options)
	if err != nil {
		fmt.Fprintf(stderr, "report bug: %v\n", err)
		return 1
	}
	if options.preview {
		printReportPreview(stdout, draft)
		return 0
	}
	return createBugReportIssue(context.Background(), stdout, stderr, draft)
}

func validateReportBugOptions(options reportBugOptions) error {
	if options.preview && options.create {
		return errors.New("--preview and --create are mutually exclusive")
	}
	if options.create && !options.yes {
		return errors.New("--create requires --yes")
	}
	sourceCount := 0
	if strings.TrimSpace(options.jobID) != "" {
		sourceCount++
	}
	if strings.TrimSpace(options.source) != "" {
		sourceCount++
	}
	if sourceCount == 0 {
		return errors.New("a source is required (--job, --source daemon, or --source dashboard)")
	}
	if sourceCount > 1 {
		return errors.New("only one source may be specified")
	}
	return nil
}

func buildBugReport(ctx context.Context, options reportBugOptions) (report.Report, error) {
	if jobID := strings.TrimSpace(options.jobID); jobID != "" {
		var draft report.Report
		err := withStore(options.home, func(store *db.Store) error {
			var err error
			draft, err = report.BuildJobReport(ctx, store, jobID, report.JobOptions{})
			return err
		})
		return draft, err
	}
	if source := strings.TrimSpace(options.source); source != "" {
		switch source {
		case "daemon", "dashboard":
			return report.Report{}, fmt.Errorf("source %s is not supported yet", source)
		default:
			return report.Report{}, fmt.Errorf("source %s is not supported", source)
		}
	}
	return report.Report{}, errors.New("source is required")
}

func printReportPreview(w io.Writer, draft report.Report) {
	fmt.Fprintf(w, "Title: %s\n", draft.Title)
	fmt.Fprintf(w, "Labels: %s\n", strings.Join(draft.Labels, ", "))
	fmt.Fprintf(w, "Fingerprint: %s\n\n", draft.Fingerprint)
	fmt.Fprintln(w, "Body:")
	fmt.Fprint(w, draft.Body)
	if !strings.HasSuffix(draft.Body, "\n") {
		fmt.Fprintln(w)
	}
}

func createBugReportIssue(ctx context.Context, stdout, stderr io.Writer, draft report.Report) int {
	result, err := createBugReportIssueResult(ctx, newReportGitHubClient(), draft, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "report bug: %v\n", err)
		return 1
	}
	if result.Existing {
		fmt.Fprintf(stdout, "existing issue: %s\n", result.URL)
	} else {
		fmt.Fprintf(stdout, "created issue: %s\n", result.URL)
	}
	return 0
}

type bugReportIssueResult struct {
	URL      string
	Existing bool
}

func createBugReportIssueResult(ctx context.Context, gh reportGitHubClient, draft report.Report, warnings io.Writer) (bugReportIssueResult, error) {
	if gh == nil {
		return bugReportIssueResult{}, errors.New("github client is required")
	}
	if strings.TrimSpace(draft.Fingerprint) == "" {
		return bugReportIssueResult{}, errors.New("bug report fingerprint is required")
	}
	if strings.TrimSpace(draft.Body) == "" {
		return bugReportIssueResult{}, errors.New("bug report body is required")
	}
	if err := gh.Preflight(ctx, reportIssueRepo); err != nil {
		return bugReportIssueResult{}, err
	}
	marker := report.FingerprintMarker(draft.Fingerprint)
	matches, err := gh.SearchOpenIssues(ctx, reportIssueRepo, marker)
	if err != nil {
		if warnings != nil {
			fmt.Fprintf(warnings, "warning: duplicate search failed: %v\n", err)
		} else {
			return bugReportIssueResult{}, fmt.Errorf("duplicate search failed: %w", err)
		}
	} else if existing, ok := matchingIssueByMarker(matches, marker); ok {
		return bugReportIssueResult{URL: existing.URL, Existing: true}, nil
	}
	issue, err := gh.CreateIssue(ctx, github.CreateIssueInput{
		Repo:   reportIssueRepo,
		Title:  draft.Title,
		Body:   draft.Body,
		Labels: draft.Labels,
	})
	if err != nil {
		return bugReportIssueResult{}, fmt.Errorf("create issue: %w", err)
	}
	return bugReportIssueResult{URL: issue.URL}, nil
}

func matchingIssueByMarker(issues []github.Issue, marker string) (github.Issue, bool) {
	for _, issue := range issues {
		if strings.TrimSpace(issue.URL) == "" {
			continue
		}
		if issue.Body == "" || strings.Contains(issue.Body, marker) {
			return issue, true
		}
	}
	return github.Issue{}, false
}
