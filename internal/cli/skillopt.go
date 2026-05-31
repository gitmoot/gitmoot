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
	"unicode/utf8"

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
	case "review":
		return runSkillOptReview(args[1:], stdout, stderr)
	case "candidate":
		return runSkillOptCandidate(args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "  gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]")
	fmt.Fprintln(w, "  gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt review item add --run <run-id> --item <item-id> --baseline baseline.md --candidate candidate.md [--title text]")
	fmt.Fprintln(w, "  gitmoot skillopt review status --run <run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate list [--template id]")
	fmt.Fprintln(w, "  gitmoot skillopt candidate show <version-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate promote <version-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate reject <version-id> [--reason text]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id> [--reviewer name]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)")
}

func runSkillOptReview(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "create":
		return runSkillOptReviewCreate(args[1:], stdout, stderr)
	case "item":
		return runSkillOptReviewItem(args[1:], stdout, stderr)
	case "status":
		return runSkillOptReviewStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt review command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptReviewCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "agent template id or version to review")
	repoFlag := fs.String("repo", "", "target repository in owner/repo form")
	runID := fs.String("run", "", "review run id")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review create does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*templateID) == "" || strings.TrimSpace(*repoFlag) == "" || strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt review create requires --template, --repo, and --run")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review create: %v\n", err)
		return 2
	}
	var run db.EvalRun
	if err := withStore(*home, func(store *db.Store) error {
		template, err := loadInstalledTemplate(context.Background(), store, *templateID)
		if err != nil {
			return err
		}
		run = db.EvalRun{
			ID:                strings.TrimSpace(*runID),
			TemplateID:        template.ID,
			TemplateVersionID: template.VersionID,
			TargetRepo:        repo.FullName(),
			State:             "review",
			MetadataJSON:      `{"driver":"manual-review"}`,
		}
		return store.UpsertEvalRun(context.Background(), run)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review create: %v\n", err)
		return 1
	}
	writeLine(stdout, "created review %s for %s", run.ID, run.TemplateVersionID)
	return 0
}

func runSkillOptReviewItem(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runSkillOptReviewItemAdd(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt review item command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptReviewItemAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review item add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "review run id")
	itemID := fs.String("item", "", "review item id")
	title := fs.String("title", "", "review item title")
	baselinePath := fs.String("baseline", "", "baseline output file")
	candidatePath := fs.String("candidate", "", "candidate output file")
	metadataJSON := fs.String("metadata-json", "", "JSON metadata to attach to the review item")
	mediaType := fs.String("media-type", "", "media type override for stored artifacts")
	driver := fs.String("driver", "text", "artifact driver")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review item add does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" || strings.TrimSpace(*itemID) == "" || strings.TrimSpace(*baselinePath) == "" || strings.TrimSpace(*candidatePath) == "" {
		fmt.Fprintln(stderr, "skillopt review item add requires --run, --item, --baseline, and --candidate")
		return 2
	}
	metadata, err := normalizeSkillOptMetadataJSON(*metadataJSON)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
		return 2
	}
	var item db.EvalReviewItem
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		run, err := store.GetEvalRun(ctx, strings.TrimSpace(*runID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("review run %s not found", strings.TrimSpace(*runID))
			}
			return err
		}
		blobStore := artifact.NewStore(paths.ArtifactBlobs)
		baseline, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "baseline", *baselinePath, *mediaType, *driver)
		if err != nil {
			return err
		}
		candidate, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "candidate", *candidatePath, *mediaType, *driver)
		if err != nil {
			return err
		}
		if baseline.ID == candidate.ID {
			return errors.New("baseline and candidate artifact ids must be different")
		}
		if err := store.UpsertEvalArtifact(ctx, baseline); err != nil {
			return fmt.Errorf("register baseline artifact: %w", err)
		}
		if err := store.UpsertEvalArtifact(ctx, candidate); err != nil {
			return fmt.Errorf("register candidate artifact: %w", err)
		}
		item = db.EvalReviewItem{
			RunID:               run.ID,
			ItemID:              strings.TrimSpace(*itemID),
			Title:               strings.TrimSpace(*title),
			BaselineArtifactID:  baseline.ID,
			CandidateArtifactID: candidate.ID,
			MetadataJSON:        metadata,
		}
		return store.UpsertEvalReviewItem(ctx, item)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
		return 1
	}
	writeLine(stdout, "added review item %s to %s", item.ItemID, item.RunID)
	return 0
}

func runSkillOptReviewStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "review run id")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review status does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt review status requires --run")
		return 2
	}
	var status skillOptReviewStatus
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var err error
		status, err = loadSkillOptReviewStatus(context.Background(), store, artifact.NewStore(paths.ArtifactBlobs), *runID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review status: %v\n", err)
		return 1
	}
	itemCount := len(status.Items)
	feedbackCount := len(status.Feedback)
	fmt.Fprintf(stdout, "run: %s\n", status.Run.ID)
	fmt.Fprintf(stdout, "template: %s\n", status.Run.TemplateID)
	fmt.Fprintf(stdout, "template_version: %s\n", status.Run.TemplateVersionID)
	fmt.Fprintf(stdout, "repo: %s\n", status.Run.TargetRepo)
	fmt.Fprintf(stdout, "state: %s\n", status.Run.State)
	fmt.Fprintf(stdout, "items: %d\n", itemCount)
	fmt.Fprintf(stdout, "feedback: %d\n", feedbackCount)
	fmt.Fprintf(stdout, "packet_blockers: %d\n", len(status.PacketBlockers))
	fmt.Fprintf(stdout, "training_blockers: %d\n", len(status.TrainingBlockers))
	fmt.Fprintf(stdout, "ready_for_packet: %t\n", status.PacketReady)
	fmt.Fprintf(stdout, "ready_for_training: %t\n", status.TrainingReady)
	for _, blocker := range status.PacketBlockers {
		fmt.Fprintf(stdout, "packet_blocker: %s\n", blocker)
	}
	for _, blocker := range status.TrainingBlockers {
		fmt.Fprintf(stdout, "training_blocker: %s\n", blocker)
	}
	return 0
}

type skillOptReviewStatus struct {
	Run              db.EvalRun
	Items            []db.EvalReviewItem
	Feedback         []db.FeedbackEvent
	PacketBlockers   []string
	TrainingBlockers []string
	PacketReady      bool
	TrainingReady    bool
}

func loadSkillOptReviewStatus(ctx context.Context, store *db.Store, blobStore artifact.Store, runID string) (skillOptReviewStatus, error) {
	run, err := store.GetEvalRun(ctx, strings.TrimSpace(runID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillOptReviewStatus{}, fmt.Errorf("review run %s not found", strings.TrimSpace(runID))
		}
		return skillOptReviewStatus{}, err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	events, err := store.ListFeedbackEvents(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	packetBlockers := reviewPacketBlockers(ctx, store, blobStore, items)
	trainingBlockers := reviewTrainingBlockers(ctx, store, run.ID, items, events)
	return skillOptReviewStatus{
		Run:              run,
		Items:            items,
		Feedback:         events,
		PacketBlockers:   packetBlockers,
		TrainingBlockers: trainingBlockers,
		PacketReady:      len(packetBlockers) == 0,
		TrainingReady:    len(packetBlockers) == 0 && len(trainingBlockers) == 0,
	}, nil
}

func reviewPacketBlockers(ctx context.Context, store *db.Store, blobStore artifact.Store, items []db.EvalReviewItem) []string {
	if len(items) == 0 {
		return []string{"run has no review items"}
	}
	var blockers []string
	validated := map[string]struct{}{}
	for _, item := range items {
		itemID := strings.TrimSpace(item.ItemID)
		if itemID == "" {
			itemID = item.ID
		}
		baseline := strings.TrimSpace(item.BaselineArtifactID)
		candidate := strings.TrimSpace(item.CandidateArtifactID)
		if baseline == "" || candidate == "" {
			blockers = append(blockers, fmt.Sprintf("item %s is missing a baseline or candidate artifact", itemID))
			continue
		}
		if baseline == candidate {
			blockers = append(blockers, fmt.Sprintf("item %s uses the same artifact for baseline and candidate", itemID))
			continue
		}
		blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "baseline", baseline, validated)...)
		blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "candidate", candidate, validated)...)
	}
	return blockers
}

func reviewTrainingBlockers(ctx context.Context, store *db.Store, runID string, items []db.EvalReviewItem, events []db.FeedbackEvent) []string {
	if len(items) == 0 {
		return []string{"run has no review items"}
	}
	var blockers []string
	feedbackByItem := map[string]int{}
	for _, event := range events {
		feedbackByItem[strings.TrimSpace(event.ItemID)]++
	}
	for _, item := range items {
		itemID := strings.TrimSpace(item.ItemID)
		if itemID == "" {
			itemID = item.ID
		}
		if feedbackByItem[itemID] == 0 {
			blockers = append(blockers, fmt.Sprintf("item %s has no imported feedback", itemID))
		}
	}
	if _, err := skillopt.ExportTrainingPackage(ctx, store, runID); err != nil {
		blockers = append(blockers, fmt.Sprintf("training export failed: %v", err))
	}
	return blockers
}

func validateReviewArtifactBlob(ctx context.Context, store *db.Store, blobStore artifact.Store, itemID string, role string, artifactID string, validated map[string]struct{}) []string {
	if _, ok := validated[artifactID]; ok {
		return nil
	}
	validated[artifactID] = struct{}{}
	record, err := store.GetEvalArtifact(ctx, artifactID)
	if err != nil {
		return []string{fmt.Sprintf("item %s %s artifact %s is not registered: %v", itemID, role, artifactID, err)}
	}
	if _, err := blobStore.Read(record.Hash); err != nil {
		return []string{fmt.Sprintf("item %s %s artifact %s blob is not readable: %v", itemID, role, artifactID, err)}
	}
	return nil
}

func prepareReviewItemArtifact(blobStore artifact.Store, runID string, itemID string, role string, path string, mediaTypeOverride string, driver string) (db.EvalArtifact, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return db.EvalArtifact{}, fmt.Errorf("%s path is required", role)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("read %s file: %w", role, err)
	}
	mediaType, err := reviewArtifactMediaType(path, content, mediaTypeOverride)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("%s file: %w", role, err)
	}
	blob, err := blobStore.Put(content)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("store %s artifact blob: %w", role, err)
	}
	artifactRecord := db.EvalArtifact{
		ID:        reviewItemArtifactID(runID, itemID, role),
		Hash:      blob.Hash,
		MediaType: mediaType,
		SizeBytes: blob.Size,
		Driver:    strings.TrimSpace(driver),
	}
	if artifactRecord.Driver == "" {
		artifactRecord.Driver = "text"
	}
	return artifactRecord, nil
}

func reviewItemArtifactID(runID string, itemID string, role string) string {
	return strings.TrimSpace(runID) + "/" + strings.TrimSpace(itemID) + "/" + strings.TrimSpace(role)
}

func reviewArtifactMediaType(path string, content []byte, override string) (string, error) {
	if mediaType := strings.TrimSpace(override); mediaType != "" {
		return mediaType, nil
	}
	if !utf8.Valid(content) {
		return "", errors.New("binary content requires --media-type")
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown", nil
	case ".txt", ".text", ".diff", ".patch":
		return "text/plain", nil
	case ".csv":
		return "text/csv", nil
	case ".json":
		return "application/json", nil
	}
	return "text/plain", nil
}

func normalizeSkillOptMetadataJSON(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return "", fmt.Errorf("metadata-json: %w", err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", fmt.Errorf("metadata-json: %w", err)
	}
	return string(encoded), nil
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
	artifactDir := fs.String("artifact-dir", "", "directory containing candidate package artifacts")
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
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		version, err := skillopt.ImportCandidatePackageWithOptions(context.Background(), store, pkg, skillopt.CandidateImportOptions{
			SourcePath:  *file,
			ArtifactDir: *artifactDir,
			BlobStore:   artifact.NewStore(paths.ArtifactBlobs),
		})
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

func runSkillOptCandidate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runSkillOptCandidateList(args[1:], stdout, stderr)
	case "show":
		return runSkillOptCandidateShow(args[1:], stdout, stderr)
	case "promote":
		return runSkillOptCandidatePromote(args[1:], stdout, stderr)
	case "reject":
		return runSkillOptCandidateReject(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt candidate command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptCandidateList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "template id to filter")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt candidate list does not accept positional arguments")
		return 2
	}
	var versions []db.AgentTemplateVersion
	var reviews map[string]db.AgentTemplateCandidateReview
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		versions, err = store.ListPendingAgentTemplateVersions(context.Background(), *templateID)
		if err != nil {
			return err
		}
		reviews = make(map[string]db.AgentTemplateCandidateReview, len(versions))
		for _, version := range versions {
			review, err := store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
			if err == nil {
				reviews[version.ID] = review
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate list: %v\n", err)
		return 1
	}
	if len(versions) == 0 {
		writeLine(stdout, "no pending candidates")
		return 0
	}
	fmt.Fprintf(stdout, "%-18s %-14s %-9s %-8s %s\n", "VERSION", "TEMPLATE", "STATE", "SCORE", "SUMMARY")
	for _, version := range versions {
		review := reviews[version.ID]
		fmt.Fprintf(stdout, "%-18s %-14s %-9s %-8s %s\n", version.ID, version.TemplateID, version.State, scoreText(review.Score), firstLine(review.PreferenceSummary))
	}
	return 0
}

func runSkillOptCandidateShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "skillopt candidate show requires exactly one version id")
		return 2
	}
	versionID := fs.Arg(0)
	var version db.AgentTemplateVersion
	var review db.AgentTemplateCandidateReview
	var hasReview bool
	var base db.AgentTemplate
	var hasBase bool
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		version, err = store.GetAgentTemplateVersionByID(context.Background(), versionID)
		if err != nil {
			return err
		}
		review, err = store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
		if err == nil {
			hasReview = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		baseRef := strings.TrimSpace(review.BaseVersionID)
		if baseRef == "" {
			current, err := store.GetAgentTemplate(context.Background(), version.TemplateID)
			if err != nil {
				return err
			}
			baseRef = current.VersionID
		}
		if baseRef != "" && baseRef != version.ID {
			base, err = store.GetAgentTemplateReference(context.Background(), baseRef)
			if err == nil {
				hasBase = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate show: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "version: %s\n", version.ID)
	fmt.Fprintf(stdout, "template: %s\n", version.TemplateID)
	fmt.Fprintf(stdout, "state: %s\n", version.State)
	fmt.Fprintf(stdout, "source: %s@%s:%s\n", version.SourceRepo, version.SourceRef, version.SourcePath)
	fmt.Fprintf(stdout, "content_hash: %s\n", version.ContentHash)
	if hasReview {
		fmt.Fprintf(stdout, "base_version: %s\n", emptyText(review.BaseVersionID))
		fmt.Fprintf(stdout, "score: %s\n", scoreText(review.Score))
		fmt.Fprintf(stdout, "preference_summary: %s\n", emptyText(review.PreferenceSummary))
		fmt.Fprintf(stdout, "diff_artifact: %s\n", emptyText(review.DiffArtifactID))
		if strings.TrimSpace(review.EvalReportJSON) != "" {
			fmt.Fprintf(stdout, "eval_report:\n%s\n", indentJSON(review.EvalReportJSON))
		}
		if strings.TrimSpace(review.DecisionReason) != "" {
			fmt.Fprintf(stdout, "decision_reason: %s\n", review.DecisionReason)
		}
	}
	if hasBase {
		diff := artifact.TextDriver{}.Diff(base.VersionID+".md", version.ID+".md", []byte(base.Content), []byte(version.Content))
		fmt.Fprintf(stdout, "content_diff:\n%s", diff)
	}
	return 0
}

func runSkillOptCandidatePromote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "skillopt candidate promote requires exactly one version id")
		return 2
	}
	var promoted db.AgentTemplateVersion
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		promoted, err = store.PromoteAgentTemplateVersion(context.Background(), fs.Arg(0))
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate promote: %v\n", err)
		return 1
	}
	writeLine(stdout, "promoted candidate %s", promoted.ID)
	return 0
}

func runSkillOptCandidateReject(args []string, stdout, stderr io.Writer) int {
	parsed, help, ok := parseSkillOptCandidateRejectArgs(args, stderr)
	if help {
		printSkillOptUsage(stdout)
		return 0
	}
	if !ok {
		return 2
	}
	if parsed.versionID == "" {
		fmt.Fprintln(stderr, "skillopt candidate reject requires exactly one version id")
		return 2
	}
	if parsed.extraVersion {
		fmt.Fprintln(stderr, "skillopt candidate reject requires exactly one version id")
		return 2
	}
	var rejected db.AgentTemplateVersion
	if err := withStore(parsed.home, func(store *db.Store) error {
		var err error
		rejected, err = store.RejectAgentTemplateVersion(context.Background(), parsed.versionID, parsed.reason)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate reject: %v\n", err)
		return 1
	}
	writeLine(stdout, "rejected candidate %s", rejected.ID)
	return 0
}

type skillOptCandidateRejectArgs struct {
	home         string
	reason       string
	versionID    string
	extraVersion bool
}

func parseSkillOptCandidateRejectArgs(args []string, stderr io.Writer) (skillOptCandidateRejectArgs, bool, bool) {
	var parsed skillOptCandidateRejectArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			return parsed, true, true
		case arg == "--home" || arg == "--reason":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "skillopt candidate reject: %s requires a value\n", arg)
				return parsed, false, false
			}
			i++
			if arg == "--home" {
				parsed.home = args[i]
			} else {
				parsed.reason = args[i]
			}
		case strings.HasPrefix(arg, "--home="):
			parsed.home = strings.TrimPrefix(arg, "--home=")
		case strings.HasPrefix(arg, "--reason="):
			parsed.reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "skillopt candidate reject: unknown flag %s\n", arg)
			return parsed, false, false
		case parsed.versionID == "":
			parsed.versionID = arg
		default:
			parsed.extraVersion = true
		}
	}
	return parsed, false, true
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

func scoreText(score *float64) string {
	if score == nil {
		return "-"
	}
	return fmt.Sprintf("%.4g", *score)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	if before, _, ok := strings.Cut(value, "\n"); ok {
		return strings.TrimSpace(before)
	}
	return value
}

func emptyText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func indentJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return value
	}
	encoded, err := json.MarshalIndent(decoded, "  ", "  ")
	if err != nil {
		return value
	}
	return string(encoded)
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
