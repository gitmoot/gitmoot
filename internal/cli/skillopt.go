package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/feedback"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

var newSkillOptGitHubClient = func() github.Client {
	return github.NewClient("")
}

func runSkillOpt(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "export":
		return runSkillOptExport(args[1:], stdout, stderr)
	case "import":
		return runSkillOptImport(args[1:], stdout, stderr)
	case "feedback":
		return runSkillOptFeedback(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func printSkillOptUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt export --run <run-id> [--output package.json]")
	fmt.Fprintln(w, "  gitmoot skillopt import --file candidate.json")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id> [--reviewer name]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)")
}

func runSkillOptExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id to export")
	output := fs.String("output", "", "path to write the training package; stdout when omitted")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt export does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt export requires --run")
		return 2
	}
	var pkg skillopt.TrainingPackage
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		pkg, err = skillopt.ExportTrainingPackage(context.Background(), store, *runID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if strings.TrimSpace(*output) == "" {
		_, err = stdout.Write(encoded)
	} else {
		err = writeSkillOptFile(*output, encoded)
		if err == nil {
			writeLine(stdout, "exported %s to %s", pkg.EvalRun.ID, *output)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	return 0
}

func runSkillOptImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	file := fs.String("file", "", "candidate package JSON file to import")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt import does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*file) == "" {
		fmt.Fprintln(stderr, "skillopt import requires --file")
		return 2
	}
	content, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt import: read candidate package: %v\n", err)
		return 1
	}
	var pkg skillopt.CandidatePackage
	if err := json.Unmarshal(content, &pkg); err != nil {
		fmt.Fprintf(stderr, "skillopt import: decode candidate package: %v\n", err)
		return 1
	}
	var versionID string
	if err := withStore(*home, func(store *db.Store) error {
		version, err := skillopt.ImportCandidatePackage(context.Background(), store, pkg, *file)
		if err != nil {
			return err
		}
		versionID = version.ID
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt import: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported pending candidate %s", versionID)
	return 0
}

func writeSkillOptFile(path string, content []byte) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	return os.WriteFile(path, content, 0o644)
}

func runSkillOptFeedback(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	if args[0] != "markdown" && args[0] != "github" {
		fmt.Fprintf(stderr, "unknown skillopt feedback collector %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
	if len(args) < 2 {
		fmt.Fprintf(stderr, "skillopt feedback %s requires a subcommand\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
	if args[0] == "markdown" {
		switch args[1] {
		case "export":
			return runSkillOptFeedbackMarkdownExport(args[2:], stdout, stderr)
		case "import":
			return runSkillOptFeedbackMarkdownImport(args[2:], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "unknown skillopt feedback markdown command %q\n\n", args[1])
			printSkillOptUsage(stderr)
			return 2
		}
	}
	switch args[1] {
	case "publish":
		return runSkillOptFeedbackGitHubPublish(args[2:], stdout, stderr)
	case "sync":
		return runSkillOptFeedbackGitHubSync(args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt feedback github command %q\n\n", args[1])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptFeedbackMarkdownExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback markdown export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	output := fs.String("output", "", "packet output directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback markdown export does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" || strings.TrimSpace(*output) == "" {
		fmt.Fprintln(stderr, "skillopt feedback markdown export requires --run and --output")
		return 2
	}
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		collector := feedback.MarkdownCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
		return collector.WritePacket(context.Background(), store, *runID, *output)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback markdown export: %v\n", err)
		return 1
	}
	writeLine(stdout, "wrote markdown feedback packet for %s to %s", *runID, *output)
	return 0
}

func runSkillOptFeedbackMarkdownImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback markdown import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	packet := fs.String("packet", "", "packet directory containing feedback.yml")
	reviewer := fs.String("reviewer", "", "reviewer name override")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback markdown import does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*packet) == "" {
		fmt.Fprintln(stderr, "skillopt feedback markdown import requires --packet")
		return 2
	}
	var count int
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		collector := feedback.MarkdownCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
		events, err := collector.ImportPacket(context.Background(), store, *packet, *reviewer)
		if err != nil {
			return err
		}
		count = len(events)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback markdown import: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported %d feedback events", count)
	return 0
}

func runSkillOptFeedbackGitHubPublish(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback github publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	repoFlag := fs.String("repo", "", "GitHub repository owner/repo")
	pullRequest := fs.Int64("pr", 0, "existing pull request number to comment on instead of creating an issue")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback github publish does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt feedback github publish requires --run")
		return 2
	}
	var result feedback.GitHubPublishResult
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		run, err := store.GetEvalRun(context.Background(), strings.TrimSpace(*runID))
		if err != nil {
			return err
		}
		repo, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, *repoFlag)
		if err != nil {
			return err
		}
		collector := feedback.GitHubCollector{
			BlobStore: artifact.NewStore(paths.ArtifactBlobs),
			GitHub:    newSkillOptGitHubClient(),
		}
		result, err = collector.Publish(context.Background(), store, run.ID, feedback.GitHubPublishTarget{
			Repo:        repo,
			PullRequest: *pullRequest,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback github publish: %v\n", err)
		return 1
	}
	writeLine(stdout, "published github feedback %s for %s to %s#%d: %s", result.Mode, strings.TrimSpace(*runID), result.Repo.FullName(), result.IssueNumber, result.URL)
	return 0
}

func runSkillOptFeedbackGitHubSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback github sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	repoFlag := fs.String("repo", "", "GitHub repository owner/repo")
	issueNumber := fs.Int64("issue", 0, "GitHub issue number containing feedback comments")
	pullRequest := fs.Int64("pr", 0, "GitHub pull request number containing feedback comments")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt feedback github sync requires --run")
		return 2
	}
	if *issueNumber > 0 && *pullRequest > 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync accepts only one of --issue or --pr")
		return 2
	}
	targetNumber := *issueNumber
	if targetNumber == 0 {
		targetNumber = *pullRequest
	}
	if targetNumber <= 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync requires --issue or --pr")
		return 2
	}
	var count int
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		run, err := store.GetEvalRun(context.Background(), strings.TrimSpace(*runID))
		if err != nil {
			return err
		}
		repo, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, *repoFlag)
		if err != nil {
			return err
		}
		collector := feedback.GitHubCollector{
			BlobStore: artifact.NewStore(paths.ArtifactBlobs),
			GitHub:    newSkillOptGitHubClient(),
		}
		events, err := collector.Sync(context.Background(), store, run.ID, repo, targetNumber)
		if err != nil {
			return err
		}
		count = len(events)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback github sync: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported %d github feedback events", count)
	return 0
}

func resolveSkillOptFeedbackRepo(ctx context.Context, paths config.Paths, store *db.Store, run db.EvalRun, repoFlag string) (github.Repository, error) {
	if strings.TrimSpace(repoFlag) != "" {
		return daemon.ParseRepository(repoFlag)
	}
	if strings.TrimSpace(run.TargetRepo) != "" {
		if repo, err := daemon.ParseRepository(run.TargetRepo); err == nil {
			return repo, nil
		}
	}
	templateRef := strings.TrimSpace(run.TemplateVersionID)
	if templateRef == "" {
		templateRef = strings.TrimSpace(run.TemplateID)
	}
	if templateRef != "" {
		template, err := store.GetAgentTemplateReference(ctx, templateRef)
		if err == nil && strings.TrimSpace(template.SourceRepo) != "" {
			if repo, err := daemon.ParseRepository(template.SourceRepo); err == nil {
				return repo, nil
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return github.Repository{}, err
		}
	}
	defaultRepo, err := config.LoadDefaultFeedbackRepo(paths)
	if err != nil {
		return github.Repository{}, err
	}
	if strings.TrimSpace(defaultRepo) != "" {
		return daemon.ParseRepository(defaultRepo)
	}
	return github.Repository{}, errors.New("skillopt feedback github requires --repo because no target repo, template source repo, or [feedback].repo default is configured")
}

func withSkillOptStore(home string, fn func(config.Paths, *db.Store) error) error {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	if err := config.Initialize(paths); err != nil {
		return err
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	return fn(paths, store)
}
