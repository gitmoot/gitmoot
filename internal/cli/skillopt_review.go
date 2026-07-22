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

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

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
	mode := fs.String("mode", "", "review mode: validate, explore, refine, or distill")
	explorationLevel := fs.String("exploration-level", "", "exploration level: high, medium, or low")
	optionsCount := fs.Int("options", 0, "expected number of review options")
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
			Mode:              strings.TrimSpace(*mode),
			ExplorationLevel:  strings.TrimSpace(*explorationLevel),
			OptionsCount:      *optionsCount,
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

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type skillOptOptionSpec struct {
	Label string
	Path  string
}

type preparedSkillOptOption struct {
	Spec     skillOptOptionSpec
	Artifact db.EvalArtifact
	Metadata string
}

func parseSkillOptOptionFlags(values []string) ([]skillOptOptionSpec, error) {
	specs := make([]skillOptOptionSpec, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		label, path, ok := strings.Cut(value, "=")
		if !ok {
			return nil, fmt.Errorf("--option must use label=path form")
		}
		label = strings.ToLower(strings.TrimSpace(label))
		path = strings.TrimSpace(path)
		if err := validateSkillOptOptionLabel(label); err != nil {
			return nil, err
		}
		if path == "" {
			return nil, fmt.Errorf("option %s path is required", label)
		}
		if _, ok := seen[label]; ok {
			return nil, fmt.Errorf("duplicate option label %q", label)
		}
		seen[label] = struct{}{}
		specs = append(specs, skillOptOptionSpec{Label: label, Path: path})
	}
	return specs, nil
}

func validateSkillOptOptionLabel(label string) error {
	if label == "" {
		return errors.New("option label is required")
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("option label %q must use only letters, digits, dots, dashes, or underscores", label)
		}
	}
	return nil
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
	var optionFlags repeatedStringFlag
	fs.Var(&optionFlags, "option", "N-way option in label=path form; repeat once per option")
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
	if strings.TrimSpace(*runID) == "" || strings.TrimSpace(*itemID) == "" {
		fmt.Fprintln(stderr, "skillopt review item add requires --run and --item")
		return 2
	}
	hasAB := strings.TrimSpace(*baselinePath) != "" || strings.TrimSpace(*candidatePath) != ""
	hasOptions := len(optionFlags) > 0
	if hasAB && hasOptions {
		fmt.Fprintln(stderr, "skillopt review item add accepts either --baseline/--candidate or repeated --option flags, not both")
		return 2
	}
	if !hasOptions && (strings.TrimSpace(*baselinePath) == "" || strings.TrimSpace(*candidatePath) == "") {
		fmt.Fprintln(stderr, "skillopt review item add requires --baseline and --candidate, or repeated --option label=path flags")
		return 2
	}
	optionSpecs, err := parseSkillOptOptionFlags(optionFlags)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
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
		rankedRun := skillOptRunUsesRankedOptions(run)
		if hasOptions && !rankedRun {
			return fmt.Errorf("review run %s is validate/A/B mode; use --baseline and --candidate", run.ID)
		}
		if !hasOptions && rankedRun {
			return fmt.Errorf("review run %s is ranked mode; use repeated --option label=path flags", run.ID)
		}
		if hasOptions {
			if run.OptionsCount > 0 && len(optionSpecs) != run.OptionsCount {
				return fmt.Errorf("review run %s expects %d options, got %d", run.ID, run.OptionsCount, len(optionSpecs))
			}
			preparedOptions := make([]preparedSkillOptOption, 0, len(optionSpecs))
			for _, spec := range optionSpecs {
				optionArtifact, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "option-"+spec.Label, spec.Path, *mediaType, *driver)
				if err != nil {
					return err
				}
				optionMetadata, err := reviewOptionMetadataJSON(spec.Path)
				if err != nil {
					return err
				}
				preparedOptions = append(preparedOptions, preparedSkillOptOption{
					Spec:     spec,
					Artifact: optionArtifact,
					Metadata: optionMetadata,
				})
			}
			item = db.EvalReviewItem{
				RunID:        run.ID,
				ItemID:       strings.TrimSpace(*itemID),
				Title:        strings.TrimSpace(*title),
				MetadataJSON: metadata,
			}
			if err := preserveExistingSkillOptReviewItemDetails(ctx, store, &item); err != nil {
				return err
			}
			if err := store.UpsertEvalReviewItem(ctx, item); err != nil {
				return err
			}
			replacementOptions := make([]db.EvalReviewOption, 0, len(preparedOptions))
			for _, prepared := range preparedOptions {
				if err := store.UpsertEvalArtifact(ctx, prepared.Artifact); err != nil {
					return fmt.Errorf("register option %s artifact: %w", prepared.Spec.Label, err)
				}
				replacementOptions = append(replacementOptions, db.EvalReviewOption{
					RunID:        run.ID,
					ItemID:       strings.TrimSpace(*itemID),
					Label:        prepared.Spec.Label,
					ArtifactID:   prepared.Artifact.ID,
					Role:         "option",
					MetadataJSON: prepared.Metadata,
				})
			}
			if err := store.ReplaceEvalReviewOptions(ctx, run.ID, strings.TrimSpace(*itemID), replacementOptions); err != nil {
				return err
			}
			return nil
		}
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
		if err := preserveExistingSkillOptReviewItemDetails(ctx, store, &item); err != nil {
			return err
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
	feedbackCount := len(status.Feedback) + len(status.RankedFeedback)
	fmt.Fprintf(stdout, "run: %s\n", status.Run.ID)
	fmt.Fprintf(stdout, "template: %s\n", status.Run.TemplateID)
	fmt.Fprintf(stdout, "template_version: %s\n", status.Run.TemplateVersionID)
	fmt.Fprintf(stdout, "repo: %s\n", status.Run.TargetRepo)
	fmt.Fprintf(stdout, "state: %s\n", status.Run.State)
	fmt.Fprintf(stdout, "mode: %s\n", status.Recommendation.CurrentMode)
	fmt.Fprintf(stdout, "exploration_level: %s\n", status.Recommendation.ExplorationLevel)
	fmt.Fprintf(stdout, "items: %d\n", itemCount)
	fmt.Fprintf(stdout, "feedback: %d\n", feedbackCount)
	fmt.Fprintf(stdout, "pairwise_preferences: %d\n", len(status.PairwisePreferences))
	fmt.Fprintf(stdout, "ranking_stability: %s\n", status.Recommendation.RankingStability)
	fmt.Fprintf(stdout, "recommended_next_mode: %s\n", status.Recommendation.RecommendedMode)
	fmt.Fprintf(stdout, "recommendation: %s\n", status.Recommendation.Summary())
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

func preserveExistingSkillOptReviewItemDetails(ctx context.Context, store *db.Store, item *db.EvalReviewItem) error {
	if store == nil || item == nil {
		return nil
	}
	if strings.TrimSpace(item.Title) != "" && strings.TrimSpace(item.MetadataJSON) != "" {
		return nil
	}
	items, err := store.ListEvalReviewItems(ctx, item.RunID)
	if err != nil {
		return err
	}
	for _, existing := range items {
		if strings.TrimSpace(existing.ItemID) != strings.TrimSpace(item.ItemID) {
			continue
		}
		if strings.TrimSpace(item.Title) == "" {
			item.Title = existing.Title
		}
		if strings.TrimSpace(item.MetadataJSON) == "" {
			item.MetadataJSON = existing.MetadataJSON
		}
		return nil
	}
	return nil
}

type skillOptReviewStatus struct {
	Run                 db.EvalRun
	Items               []db.EvalReviewItem
	Feedback            []db.FeedbackEvent
	RankedFeedback      []db.RankedFeedbackEvent
	PairwisePreferences []db.PairwisePreference
	Recommendation      skillopt.PhaseRecommendation
	PacketBlockers      []string
	TrainingBlockers    []string
	PacketReady         bool
	TrainingReady       bool
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
	rankedEvents, err := store.ListRankedFeedbackEvents(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	packetBlockers := reviewPacketBlockers(ctx, store, blobStore, run, items)
	trainingBlockers := reviewTrainingBlockers(ctx, store, run, items, events, rankedEvents)
	recommendation := skillopt.RecommendPhaseForItems(run, items, events, rankedEvents, pairwisePreferences)
	return skillOptReviewStatus{
		Run:                 run,
		Items:               items,
		Feedback:            events,
		RankedFeedback:      rankedEvents,
		PairwisePreferences: pairwisePreferences,
		Recommendation:      recommendation,
		PacketBlockers:      packetBlockers,
		TrainingBlockers:    trainingBlockers,
		PacketReady:         len(packetBlockers) == 0,
		TrainingReady:       len(packetBlockers) == 0 && len(trainingBlockers) == 0,
	}, nil
}

func reviewPacketBlockers(ctx context.Context, store *db.Store, blobStore artifact.Store, run db.EvalRun, items []db.EvalReviewItem) []string {
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
		if skillOptRunUsesRankedOptions(run) {
			options, err := store.ListEvalReviewOptions(ctx, run.ID, item.ItemID)
			if err != nil {
				blockers = append(blockers, fmt.Sprintf("item %s options are not readable: %v", itemID, err))
				continue
			}
			if len(options) == 0 {
				blockers = append(blockers, fmt.Sprintf("item %s has no registered options", itemID))
				continue
			}
			if run.OptionsCount > 0 && len(options) != run.OptionsCount {
				blockers = append(blockers, fmt.Sprintf("item %s has %d options, want %d", itemID, len(options), run.OptionsCount))
				continue
			}
			for _, option := range options {
				blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "option "+option.Label, option.ArtifactID, validated)...)
			}
			continue
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

func skillOptRunUsesRankedOptions(run db.EvalRun) bool {
	return run.Mode != db.EvalRunModeValidate || run.OptionsCount > 2
}

func reviewTrainingBlockers(ctx context.Context, store *db.Store, run db.EvalRun, items []db.EvalReviewItem, events []db.FeedbackEvent, rankedEvents []db.RankedFeedbackEvent) []string {
	if len(items) == 0 {
		return []string{"run has no review items"}
	}
	var blockers []string
	feedbackByItem := map[string]int{}
	for _, event := range events {
		feedbackByItem[strings.TrimSpace(event.ItemID)]++
	}
	for _, event := range rankedEvents {
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
	if _, err := skillopt.ExportTrainingPackage(ctx, store, run.ID); err != nil {
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

func reviewOptionMetadataJSON(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	encoded, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		return "", fmt.Errorf("option metadata: %w", err)
	}
	return string(encoded), nil
}
