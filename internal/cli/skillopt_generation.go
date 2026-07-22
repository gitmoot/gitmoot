package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

type skillOptTrainGenerationResult struct {
	GeneratedOptions int
	JobIDs           []string
	AgentName        string
	Runtime          string
	Metadata         map[string]any
}

const skillOptTrainReviewOptionRetryBudget = 1

func estimateSkillOptTrainGenerationLockTTL(ctx context.Context, store *db.Store, request skillOptTrainContinueRequest, iteration db.SkillOptTrainIteration) (time.Duration, error) {
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("eval run %s not found", iteration.EvalRunID)
		}
		return 0, err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return 0, err
	}
	itemCount := len(items)
	if itemCount <= 0 {
		itemCount = 1
	}
	roles := len(skillOptTrainGenerationRoles(run))
	if roles <= 0 {
		roles = 2
	}
	attemptsPerRole := 1
	if strings.TrimSpace(iteration.SessionID) != "" {
		session, err := store.GetSkillOptTrainSession(ctx, iteration.SessionID)
		if err != nil {
			return 0, err
		}
		if skillOptTrainRequiresVuePreviewBundle(session) {
			attemptsPerRole += skillOptTrainReviewOptionRetryBudget
		}
	}
	var dispatch skillOptTrainGenerationDispatch
	if strings.TrimSpace(iteration.SessionID) != "" {
		session, err := store.GetSkillOptTrainSession(ctx, iteration.SessionID)
		if err != nil {
			return 0, err
		}
		dispatch, err = skillOptTrainGeneratorSelection(ctx, store, session, iteration, run, request)
		if err != nil {
			return 0, err
		}
	} else {
		dispatch = skillOptTrainFallbackGeneratorDispatch(request)
	}
	if strings.TrimSpace(dispatch.Mode) == "" {
		dispatch = skillOptTrainFallbackGeneratorDispatch(request)
	}
	if strings.TrimSpace(dispatch.Agent) == "" && strings.TrimSpace(dispatch.Type) == "" && dispatch.Mode != skillOptTrainGenerationModeTargetSkill {
		return 0, errors.New("skillopt train generation dispatch is empty")
	}
	jobTimeout := skillOptTrainGenerationJobTimeoutHint(request, dispatch.Type)
	concurrency := skillOptTrainGenerationConcurrencyHint(request, dispatch.Type)
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > itemCount {
		concurrency = itemCount
	}
	batches := (itemCount + concurrency - 1) / concurrency
	estimated := time.Duration(batches*roles*attemptsPerRole)*jobTimeout + skillOptTrainGenerationLockBuffer
	if estimated < skillOptTrainGenerationLockTTL {
		return skillOptTrainGenerationLockTTL, nil
	}
	return estimated, nil
}

func skillOptTrainGenerationJobTimeoutHint(request skillOptTrainContinueRequest, dispatchType string) time.Duration {
	if strings.TrimSpace(dispatchType) == "" {
		return daemonRunningJobStaleAfter
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return daemonRunningJobStaleAfter
	}
	agentType, ok := types[dispatchType]
	if !ok {
		return daemonRunningJobStaleAfter
	}
	jobTimeout, err := time.ParseDuration(agentType.JobTimeout)
	if err != nil || jobTimeout <= 0 {
		return daemonRunningJobStaleAfter
	}
	return jobTimeout
}

func skillOptTrainGenerationConcurrencyHint(request skillOptTrainContinueRequest, dispatchType string) int {
	if strings.TrimSpace(dispatchType) == "" {
		return 1
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return 1
	}
	agentType, ok := types[dispatchType]
	if !ok || agentType.MaxBackground <= 0 {
		return 1
	}
	return agentType.MaxBackground
}

// skillOptTrainGenerationTaskID is the per-option TaskID stamped on each
// generation child job, uniquely identifying the (run, item, label, attempt)
// option it produced.
func skillOptTrainGenerationTaskID(runID string, itemID string, label string, attempt int) string {
	return fmt.Sprintf("skillopt-train-generation:%s:%s:%s:%d", strings.TrimSpace(runID), strings.TrimSpace(itemID), strings.TrimSpace(label), attempt)
}

// skillOptTrainGenerationProgress emits human-facing, one-line-per-option
// progress to a writer (typically stderr) while option jobs run concurrently.
// A nil receiver or nil writer is a no-op, so automated callers (the review
// watcher passes a nil Progress) stay silent. Writes are mutex-guarded because
// options across items complete on concurrent goroutines.
type skillOptTrainGenerationProgress struct {
	mu     sync.Mutex
	w      io.Writer
	done   int
	total  int
	extend func() error
}

func newSkillOptTrainGenerationProgress(w io.Writer, total int, extend func() error) *skillOptTrainGenerationProgress {
	return &skillOptTrainGenerationProgress{w: w, total: total, extend: extend}
}

func (p *skillOptTrainGenerationProgress) start(items, perItem int, runtime string) {
	if p == nil || p.w == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	suffix := ""
	if strings.TrimSpace(runtime) != "" {
		suffix = " with " + runtime
	}
	fmt.Fprintf(p.w, "generating %d options (%d items x %d)%s...\n", p.total, items, perItem, suffix)
}

func (p *skillOptTrainGenerationProgress) optionDone(itemID, role string, elapsed time.Duration, failed bool) {
	if p == nil {
		return
	}
	// Keep the generation lock fresh while a long run is still producing options.
	if !failed && p.extend != nil {
		_ = p.extend()
	}
	if p.w == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	status := "done"
	if failed {
		status = "failed"
	}
	fmt.Fprintf(p.w, "option %s/%s %s (%d/%d) - %s\n", itemID, role, status, p.done, p.total, formatShortDuration(elapsed))
}

func formatShortDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// skillOptTrainGenerationRuntimeLabel is a best-effort runtime name for the
// generation start line, taken from the resolved target/optimizer backend.
func skillOptTrainGenerationRuntimeLabel(request skillOptTrainContinueRequest) string {
	if backend := strings.TrimSpace(request.Optimizer.TargetBackend); backend != "" {
		return backend
	}
	return strings.TrimSpace(request.Optimizer.Backend)
}

// skillOptTrainGenerationPlan classifies a run's review items against what is
// already persisted: which items are complete (and how many options that adds),
// and which items still need generation. It is the shared detection used by both
// the continue/resume path and generation-lock recovery, so "skip complete,
// regenerate missing" behaves identically in both.
type skillOptTrainGenerationPlan struct {
	ExistingGenerated int
	ToGenerate        []db.EvalReviewItem
	CompleteItemIDs   []string
	MissingItemIDs    []string
}

// classifySkillOptTrainGenerationItems partitions items into already-persisted
// (complete) and still-missing. A complete item contributes its persisted
// options to ExistingGenerated and is skipped; a partially persisted single item
// is an error (per-item commits are atomic, so a half-written item is corruption,
// not a normal resume state).
func classifySkillOptTrainGenerationItems(ctx context.Context, store *db.Store, run db.EvalRun, items []db.EvalReviewItem, roles []string, rankedRun bool) (skillOptTrainGenerationPlan, error) {
	plan := skillOptTrainGenerationPlan{
		ToGenerate: make([]db.EvalReviewItem, 0, len(items)),
	}
	// Count already-persisted options per item in ONE run-scoped query instead of
	// one query per item (the single-conn store would otherwise serialize N
	// round-trips on resume of a large run).
	existingOptionCount := map[string]int{}
	if rankedRun {
		allOptions, err := store.ListEvalReviewOptions(ctx, run.ID, "")
		if err != nil {
			return skillOptTrainGenerationPlan{}, err
		}
		for _, opt := range allOptions {
			existingOptionCount[opt.ItemID]++
		}
	}
	// A mix of complete and incomplete items is the normal resume state (per-item
	// commits), so it is not an error — only a partially generated single item is.
	for _, item := range items {
		if rankedRun {
			existing := existingOptionCount[item.ItemID]
			if existing > 0 {
				if existing == len(roles) {
					plan.ExistingGenerated += existing
					plan.CompleteItemIDs = append(plan.CompleteItemIDs, item.ItemID)
					continue
				}
				return skillOptTrainGenerationPlan{}, fmt.Errorf("item %s has partial generated options; inspect or clear review options before continuing", item.ItemID)
			}
			plan.ToGenerate = append(plan.ToGenerate, item)
			plan.MissingItemIDs = append(plan.MissingItemIDs, item.ItemID)
			continue
		}
		hasBaseline := strings.TrimSpace(item.BaselineArtifactID) != ""
		hasCandidate := strings.TrimSpace(item.CandidateArtifactID) != ""
		if hasBaseline || hasCandidate {
			if hasBaseline && hasCandidate {
				plan.ExistingGenerated += 2
				plan.CompleteItemIDs = append(plan.CompleteItemIDs, item.ItemID)
				continue
			}
			return skillOptTrainGenerationPlan{}, fmt.Errorf("item %s has partial generated A/B artifacts; inspect or clear review item artifacts before continuing", item.ItemID)
		}
		plan.ToGenerate = append(plan.ToGenerate, item)
		plan.MissingItemIDs = append(plan.MissingItemIDs, item.ItemID)
	}
	return plan, nil
}

func generateSkillOptTrainOptions(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) (skillOptTrainGenerationResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptionsGenerated); err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s not found", iteration.EvalRunID)
		}
		return skillOptTrainGenerationResult{}, err
	}
	rankedRun := skillOptRunUsesRankedOptions(run)
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	if len(items) == 0 {
		return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s has no review items to generate", run.ID)
	}
	roles := skillOptTrainGenerationRoles(run)
	if len(roles) < 2 {
		return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s expects at least 2 options", run.ID)
	}
	plan, err := classifySkillOptTrainGenerationItems(ctx, store, run, items, roles, rankedRun)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	existingGenerated := plan.ExistingGenerated
	toGenerate := plan.ToGenerate
	if len(toGenerate) == 0 {
		metadata := map[string]any{
			"status":            "recovered",
			"generated_options": existingGenerated,
			"strategy":          skillOptTrainGenerationStrategy(run),
			"completed_at":      time.Now().UTC().Format(time.RFC3339Nano),
		}
		return skillOptTrainGenerationResult{
			GeneratedOptions: existingGenerated,
			Metadata:         metadata,
		}, nil
	}
	dispatch, err := skillOptTrainGeneratorSelection(ctx, store, session, iteration, run, request)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	concurrency, err := skillOptTrainGenerationConcurrency(request, dispatch.Type)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	if err := ensureSkillOptTrainGenerationRepoReady(ctx, store, skillOptTrainGenerationRepo(session)); err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	progress := newSkillOptTrainGenerationProgress(request.Progress, len(toGenerate)*len(roles), request.GenerationLockExtend)
	progress.start(len(toGenerate), len(roles), skillOptTrainGenerationRuntimeLabel(request))
	generatedItems, err := generateSkillOptTrainItemOptions(ctx, store, blobStore, session, iteration, run, toGenerate, roles, rankedRun, request, dispatch, concurrency, progress)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	// Each generated item was persisted atomically the moment it completed
	// (see generateSkillOptTrainItemOptions), so there is no end-of-phase batch
	// write here. This loop only aggregates metadata across the items.
	generated := 0
	jobIDs := []string{}
	var generatorAgent string
	var generatorRuntime string
	promptRecords := []map[string]any{}
	for _, item := range generatedItems {
		generated += len(item.Artifacts)
		jobIDs = append(jobIDs, item.JobIDs...)
		if generatorAgent == "" {
			generatorAgent = item.AgentName
		}
		if generatorRuntime == "" {
			generatorRuntime = item.Runtime
		}
		promptRecords = append(promptRecords, item.Prompts...)
	}
	metadata := map[string]any{
		"status":              "succeeded",
		"generated_options":   existingGenerated + generated,
		"jobs":                jobIDs,
		"agent":               generatorAgent,
		"runtime":             generatorRuntime,
		"generation_mode":     dispatch.Mode,
		"template_id":         dispatch.TemplateID,
		"template_version_id": dispatch.TemplateVersionID,
		"concurrency":         concurrency,
		"lock_ttl":            request.GenerationLockTTL.String(),
		"strategy":            skillOptTrainGenerationStrategy(run),
		"prompts":             promptRecords,
		"completed_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	return skillOptTrainGenerationResult{
		GeneratedOptions: existingGenerated + generated,
		JobIDs:           jobIDs,
		AgentName:        generatorAgent,
		Runtime:          generatorRuntime,
		Metadata:         metadata,
	}, nil
}

type skillOptTrainGeneratedItemOptions struct {
	ItemID     string
	ReviewItem *db.EvalReviewItem
	Artifacts  []db.EvalArtifact
	Options    []db.EvalReviewOption
	JobIDs     []string
	AgentName  string
	Runtime    string
	Prompts    []map[string]any
}

// skillOptTrainGenerationWriteForItem projects a generated item onto the store's
// per-item write shape (artifacts + review item + options).
func skillOptTrainGenerationWriteForItem(item skillOptTrainGeneratedItemOptions) db.EvalReviewGenerationWrite {
	return db.EvalReviewGenerationWrite{
		ItemID:     item.ItemID,
		ReviewItem: item.ReviewItem,
		Artifacts:  item.Artifacts,
		Options:    item.Options,
	}
}

type skillOptTrainGeneratedOption struct {
	Output                localAgentJobOutput
	Content               []byte
	MediaType             string
	Driver                string
	GenerationMode        string
	TemplateID            string
	TemplateVersionID     string
	SampleLabel           string
	PreviewBundleMetadata *skillopt.PreviewBundleMetadata
	Prompt                string
	Prompts               []map[string]any
	JobIDs                []string
	RetryAttempts         int
	ValidationErrors      []map[string]any
}

func generateSkillOptTrainItemOptions(ctx context.Context, store *db.Store, blobStore artifact.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, items []db.EvalReviewItem, roles []string, rankedRun bool, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch, concurrency int, progress *skillOptTrainGenerationProgress) ([]skillOptTrainGeneratedItemOptions, error) {
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(items) {
		concurrency = len(items)
	}
	results := make([]skillOptTrainGeneratedItemOptions, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for index, item := range items {
		index := index
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[index] = ctx.Err()
				return
			}
			result, err := generateSkillOptTrainSingleItemOptions(ctx, store, blobStore, session, iteration, run, item, roles, rankedRun, request, dispatch, progress)
			if err != nil {
				errs[index] = err
				return
			}
			// Persist the completed item immediately so neither a later item's
			// failure nor a mid-run cancellation can lose it. Artifacts + item row
			// + options commit in one transaction (a partial item is never
			// written), using a non-cancellable context so a fully-generated item
			// is durable even if ctx is cancelled at this instant.
			if err := store.ReplaceGeneratedEvalReviewArtifactsForItem(context.WithoutCancel(ctx), run.ID, skillOptTrainGenerationWriteForItem(result)); err != nil {
				errs[index] = err
				return
			}
			results[index] = result
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func generateSkillOptTrainSingleItemOptions(ctx context.Context, store *db.Store, blobStore artifact.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, roles []string, rankedRun bool, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch, progress *skillOptTrainGenerationProgress) (skillOptTrainGeneratedItemOptions, error) {
	generatedItem := skillOptTrainGeneratedItemOptions{ItemID: item.ItemID}
	replacementOptions := make([]db.EvalReviewOption, 0, len(roles))
	artifactRecords := make([]db.EvalArtifact, 0, len(roles))
	wantsVuePreviewBundle := skillOptTrainWantsVuePreviewBundle(session)
	requiresVuePreviewBundle := skillOptTrainRequiresVuePreviewBundle(session)
	for _, role := range roles {
		optionStart := time.Now()
		generatedOption, err := generateSkillOptTrainSingleOption(ctx, store, session, iteration, run, item, role, rankedRun, request, dispatch, wantsVuePreviewBundle, requiresVuePreviewBundle)
		if err != nil {
			progress.optionDone(item.ItemID, role, time.Since(optionStart), true)
			return skillOptTrainGeneratedItemOptions{}, err
		}
		progress.optionDone(item.ItemID, role, time.Since(optionStart), false)
		artifactRole := role
		if rankedRun {
			artifactRole = "option-" + role
		}
		artifactRecord, err := prepareReviewItemContentArtifact(blobStore, run.ID, item.ItemID, artifactRole, generatedOption.Content, generatedOption.MediaType, generatedOption.Driver)
		if err != nil {
			return skillOptTrainGeneratedItemOptions{}, err
		}
		artifactRecords = append(artifactRecords, artifactRecord)
		optionMetadata := skillOptTrainGeneratedOptionMetadata(generatedOption.Output, generatedOption.Prompt, generatedOption.GenerationMode, generatedOption.TemplateID, generatedOption.TemplateVersionID, generatedOption.SampleLabel, generatedOption.PreviewBundleMetadata, generatedOption.RetryAttempts, generatedOption.ValidationErrors)
		if rankedRun {
			replacementOptions = append(replacementOptions, db.EvalReviewOption{
				RunID:        run.ID,
				ItemID:       item.ItemID,
				Label:        role,
				ArtifactID:   artifactRecord.ID,
				Role:         "option",
				MetadataJSON: optionMetadata,
			})
		} else if role == "baseline" {
			item.BaselineArtifactID = artifactRecord.ID
		} else if role == "candidate" {
			item.CandidateArtifactID = artifactRecord.ID
		}
		generatedItem.JobIDs = append(generatedItem.JobIDs, generatedOption.JobIDs...)
		if generatedItem.AgentName == "" {
			generatedItem.AgentName = generatedOption.Output.Agent
		}
		if generatedItem.Runtime == "" {
			if agent, err := store.GetAgent(ctx, generatedOption.Output.Agent); err == nil {
				generatedItem.Runtime = agent.Runtime
			}
		}
		generatedItem.Prompts = append(generatedItem.Prompts, generatedOption.Prompts...)
	}
	generatedItem.Artifacts = artifactRecords
	generatedItem.Options = replacementOptions
	if !rankedRun {
		generatedItem.ReviewItem = &item
	}
	return generatedItem, nil
}

func generateSkillOptTrainSingleOption(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, rankedRun bool, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch, wantsVuePreviewBundle bool, requiresVuePreviewBundle bool) (skillOptTrainGeneratedOption, error) {
	basePrompt := buildSkillOptTrainGenerationPrompt(session, iteration, run, item, role, rankedRun)
	prompt := basePrompt
	retryBudget := 0
	if wantsVuePreviewBundle && requiresVuePreviewBundle {
		retryBudget = skillOptTrainReviewOptionRetryBudget
	}
	validationErrors := []map[string]any{}
	promptRecords := []map[string]any{}
	jobIDs := []string{}
	for attempt := 0; ; attempt++ {
		output, err := dispatchSkillOptTrainGenerationJob(ctx, store, session, iteration, run, item, role, attempt, request, dispatch, prompt)
		if err != nil {
			return skillOptTrainGeneratedOption{}, fmt.Errorf("generate %s for %s: %w", role, item.ItemID, err)
		}
		promptRecord := map[string]any{
			"item_id": item.ItemID,
			"role":    role,
			"attempt": attempt,
			"job_id":  output.JobID,
			"prompt":  prompt,
		}
		promptRecords = append(promptRecords, promptRecord)
		jobIDs = append(jobIDs, output.JobID)
		if output.Result == nil {
			return skillOptTrainGeneratedOption{}, fmt.Errorf("generate %s for %s: job %s did not return a result", role, item.ItemID, output.JobID)
		}
		if output.Result.Decision != "implemented" {
			return skillOptTrainGeneratedOption{}, fmt.Errorf("generate %s for %s: job %s returned %s, want implemented: %s", role, item.ItemID, output.JobID, output.Result.Decision, output.Result.Summary)
		}
		content := []byte(output.Result.Summary)
		mediaType := "text/markdown"
		driver := "text"
		var previewBundleMetadata *skillopt.PreviewBundleMetadata
		if wantsVuePreviewBundle {
			bundle, err := skillopt.ParsePreviewBundle([]byte(output.Result.Summary))
			if err != nil {
				if requiresVuePreviewBundle {
					validationError := skillOptTrainOptionValidationError(item.ItemID, role, attempt, err)
					validationErrors = append(validationErrors, validationError)
					promptRecord["validation_error"] = validationError
					if attempt < retryBudget {
						prompt = buildSkillOptTrainGenerationRetryPrompt(basePrompt, validationError)
						continue
					}
					return skillOptTrainGeneratedOption{}, fmt.Errorf("generate option validation failed: item=%s option=%s validation_class=preview_bundle retry_count=%d error=%w", item.ItemID, role, attempt, err)
				}
			} else {
				content, err = json.Marshal(bundle)
				if err != nil {
					return skillOptTrainGeneratedOption{}, fmt.Errorf("generate %s for %s: encode preview bundle: %w", role, item.ItemID, err)
				}
				metadata := bundle.Metadata()
				previewBundleMetadata = &metadata
				mediaType = "application/json"
				driver = skillopt.TrainPreviewRendererVueVite
			}
		}
		return skillOptTrainGeneratedOption{
			Output:                output,
			Content:               content,
			MediaType:             mediaType,
			Driver:                driver,
			GenerationMode:        dispatch.Mode,
			TemplateID:            dispatch.TemplateID,
			TemplateVersionID:     dispatch.TemplateVersionID,
			SampleLabel:           skillOptTrainGenerationSampleLabel(item.ItemID, role),
			PreviewBundleMetadata: previewBundleMetadata,
			Prompt:                prompt,
			Prompts:               promptRecords,
			JobIDs:                jobIDs,
			RetryAttempts:         len(validationErrors),
			ValidationErrors:      validationErrors,
		}, nil
	}
}

func dispatchSkillOptTrainGenerationJob(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, attempt int, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch, prompt string) (localAgentJobOutput, error) {
	taskID := skillOptTrainGenerationTaskID(run.ID, item.ItemID, role, attempt)
	if dispatch.Mode != skillOptTrainGenerationModeTargetSkill {
		return dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
			RepoFlag:         skillOptTrainGenerationRepo(session),
			Agent:            dispatch.Agent,
			Action:           "ask",
			Instructions:     prompt,
			Type:             dispatch.Type,
			Home:             request.Home,
			AllowManagedSync: dispatch.Type != "",
			TaskID:           taskID,
		})
	}
	agentName, err := startSkillOptTrainTargetSkillAgent(ctx, store, session, iteration, run, item, role, attempt, request, dispatch)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	return dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag:     skillOptTrainGenerationRepo(session),
		Agent:        agentName,
		Action:       "ask",
		Instructions: prompt,
		Home:         request.Home,
		JobTimeout:   skillOptTrainGenerationJobTimeoutHint(request, dispatch.Type),
		TaskID:       taskID,
	})
}

func startSkillOptTrainTargetSkillAgent(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, attempt int, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch) (string, error) {
	repo, record, err := resolveLocalAgentRepo(ctx, store, skillOptTrainGenerationRepo(session))
	if err != nil {
		return "", err
	}
	if err := store.UpsertRepo(ctx, record); err != nil {
		return "", err
	}
	template, err := loadInstalledTemplate(ctx, store, dispatch.TemplateVersionID)
	if err != nil {
		return "", err
	}
	agent := runtime.Agent{
		Name:           skillOptTrainTargetSkillAgentName(run.ID, item.ItemID, role, attempt),
		Role:           "generator",
		Runtime:        firstNonEmpty(strings.TrimSpace(dispatch.Runtime), runtime.CodexRuntime),
		RepoScope:      repo.FullName(),
		TemplateID:     dispatch.TemplateVersionID,
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "idle",
	}
	adapter, err := runtimeAdapterFor(request.Home, agent.Runtime, record.CheckoutPath)
	if err != nil {
		return "", err
	}
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: agent, Prompt: agentStartupPrompt(agent, template)})
	if err != nil {
		return "", err
	}
	agent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(agent); err != nil {
		return "", err
	}
	if err := persistAgentSubscription(ctx, store, agent, []string{repo.FullName()}); err != nil {
		return "", err
	}
	return agent.Name, nil
}

func skillOptTrainTargetSkillAgentName(runID string, itemID string, role string, attempt int) string {
	parts := []string{"skillopt-target", runID, itemID, role}
	if attempt > 0 {
		parts = append(parts, fmt.Sprintf("retry-%d", attempt))
	}
	parts = append(parts, fmt.Sprintf("%x", time.Now().UTC().UnixNano()))
	return skillOptSafeAgentName(strings.Join(parts, "-"))
}

func skillOptSafeAgentName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			ok = r == '-' || r == '_' || r == '.' || r == '@'
		}
		if ok {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		return "skillopt-target"
	}
	return name
}

func skillOptTrainGenerationSampleLabel(itemID string, role string) string {
	return strings.TrimSpace(itemID) + "/" + strings.TrimSpace(role)
}

func skillOptTrainWantsVuePreviewBundle(session db.SkillOptTrainSession) bool {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	return policy.Mode != skillopt.TrainPreviewModeNone && policy.Renderer == skillopt.TrainPreviewRendererVueVite
}

func skillOptTrainRequiresVuePreviewBundle(session db.SkillOptTrainSession) bool {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	return policy.Mode == skillopt.TrainPreviewModeRequired && policy.Renderer == skillopt.TrainPreviewRendererVueVite
}

func recordSkillOptTrainGenerationFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest, failure error) error {
	metadata := map[string]any{
		"status":       "failed",
		"agent":        strings.TrimSpace(request.GeneratorAgent),
		"agent_type":   strings.TrimSpace(request.GeneratorType),
		"error":        failure.Error(),
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "generation", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "generation", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func skillOptTrainGenerationRepo(session db.SkillOptTrainSession) string {
	if repo := strings.TrimSpace(session.WorkspaceRepo); repo != "" {
		return repo
	}
	return strings.TrimSpace(session.TargetRepo)
}

func ensureSkillOptTrainGenerationRepoReady(ctx context.Context, store *db.Store, repoName string) error {
	repoName = strings.TrimSpace(repoName)
	if repoName == "" {
		return errors.New("skillopt train generation repo is required")
	}
	repo, err := daemon.ParseRepository(repoName)
	if err != nil {
		return err
	}
	record, err := resolveRepoRecord(ctx, store, repo, ".")
	if err != nil {
		return fmt.Errorf("generation repo %s is not registered with a checkout path; run `gitmoot repo add %s --path /path/to/checkout` before train continue: %w", repo.FullName(), repo.FullName(), err)
	}
	return store.UpsertRepo(ctx, record)
}

const (
	skillOptTrainGenerationModeTargetSkill       = "target_skill"
	skillOptTrainGenerationModeSkillOptGenerator = "skillopt_generator"
	skillOptTrainGenerationModeCustomAgent       = "custom_agent"
)

type skillOptTrainGenerationDispatch struct {
	Mode              string
	Agent             string
	Type              string
	Runtime           string
	TemplateID        string
	TemplateVersionID string
}

func skillOptTrainGeneratorSelection(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, request skillOptTrainContinueRequest) (skillOptTrainGenerationDispatch, error) {
	agent := strings.TrimSpace(request.GeneratorAgent)
	agentType := strings.TrimSpace(request.GeneratorType)
	if agent != "" && agentType != "" {
		return skillOptTrainGenerationDispatch{}, errors.New("use only one of --generator-agent or --generator-type")
	}
	if agent != "" {
		return skillOptTrainGenerationDispatch{Mode: skillOptTrainGenerationModeCustomAgent, Agent: agent}, nil
	}
	if agentType != "" {
		return skillOptTrainGenerationDispatch{Mode: skillOptTrainGenerationModeSkillOptGenerator, Agent: agentType, Type: agentType}, nil
	}
	templateVersionID := skillOptTrainGenerationTemplateVersion(session, iteration, run)
	if templateVersionID == "" {
		return skillOptTrainFallbackGeneratorDispatch(request), nil
	}
	template, err := loadInstalledTemplate(ctx, store, templateVersionID)
	if err != nil {
		return skillOptTrainGenerationDispatch{}, err
	}
	return skillOptTrainGenerationDispatch{
		Mode:              skillOptTrainGenerationModeTargetSkill,
		Runtime:           skillOptTrainTargetSkillGenerationRuntime(request),
		TemplateID:        template.ID,
		TemplateVersionID: template.VersionID,
	}, nil
}

func skillOptTrainFallbackGeneratorDispatch(request skillOptTrainContinueRequest) skillOptTrainGenerationDispatch {
	agentType := strings.TrimSpace(request.GeneratorType)
	if agentType == "" {
		agentType = "skillopt-generator"
	}
	return skillOptTrainGenerationDispatch{Mode: skillOptTrainGenerationModeSkillOptGenerator, Agent: agentType, Type: agentType}
}

func skillOptTrainGenerationTemplateVersion(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun) string {
	return firstNonEmpty(
		strings.TrimSpace(iteration.BaseTemplateVersionID),
		strings.TrimSpace(run.TemplateVersionID),
		strings.TrimSpace(session.TemplateVersionID),
	)
}

func skillOptTrainTargetSkillGenerationRuntime(request skillOptTrainContinueRequest) string {
	for _, value := range []string{
		request.Optimizer.Backend,
		request.Optimizer.TargetBackend,
		request.Optimizer.OptimizerBackend,
	} {
		switch strings.TrimSpace(strings.ToLower(value)) {
		case runtime.ClaudeRuntime, "claude-code":
			return runtime.ClaudeRuntime
		case runtime.CodexRuntime, "codex_exec":
			return runtime.CodexRuntime
		}
	}
	return runtime.CodexRuntime
}

func skillOptTrainGenerationConcurrency(request skillOptTrainContinueRequest, dispatchType string) (int, error) {
	if strings.TrimSpace(dispatchType) == "" {
		return 1, nil
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return 0, err
	}
	agentType, ok := types[dispatchType]
	if !ok {
		return 0, fmt.Errorf("agent %q not found", dispatchType)
	}
	if agentType.MaxBackground <= 0 {
		return 1, nil
	}
	return agentType.MaxBackground, nil
}

func skillOptTrainOptionLabels(count int) []string {
	if count <= 0 {
		count = 2
	}
	labels := make([]string, 0, count)
	for index := 0; index < count; index++ {
		if index < 26 {
			labels = append(labels, string(rune('a'+index)))
			continue
		}
		labels = append(labels, fmt.Sprintf("option-%d", index+1))
	}
	return labels
}

func skillOptTrainGenerationRoles(run db.EvalRun) []string {
	if !skillOptRunUsesRankedOptions(run) {
		return []string{"baseline", "candidate"}
	}
	return skillOptTrainOptionLabels(run.OptionsCount)
}

func buildSkillOptTrainGenerationPrompt(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, rankedRun bool) string {
	itemMetadata := decodedSkillOptMetadata(item.MetadataJSON)
	sessionMetadata := decodedSkillOptMetadata(session.MetadataJSON)
	requestText := metadataString(sessionMetadata, "request")
	if requestText == "" {
		requestText = session.RequestSummary
	}
	var builder strings.Builder
	builder.WriteString("Generate one review option for a Gitmoot SkillOpt training run.\n")
	builder.WriteString("Return the generated artifact content in gitmoot_result.summary with decision implemented. Do not include commentary outside the artifact.\n\n")
	builder.WriteString("Training request:\n")
	builder.WriteString(requestText)
	builder.WriteString("\n\n")
	builder.WriteString("Session: ")
	builder.WriteString(session.ID)
	builder.WriteString("\nIteration: ")
	builder.WriteString(iteration.ID)
	builder.WriteString("\nEval run: ")
	builder.WriteString(run.ID)
	builder.WriteString("\nMode: ")
	builder.WriteString(run.Mode)
	builder.WriteString("\nExploration level: ")
	builder.WriteString(run.ExplorationLevel)
	if rankedRun {
		builder.WriteString("\nOption label: ")
		builder.WriteString(strings.ToUpper(role))
	} else {
		builder.WriteString("\nA/B artifact role: ")
		builder.WriteString(role)
	}
	builder.WriteString("\nItem id: ")
	builder.WriteString(item.ItemID)
	builder.WriteString("\nTitle: ")
	builder.WriteString(item.Title)
	if brief := metadataString(itemMetadata, "brief"); brief != "" {
		builder.WriteString("\nBrief: ")
		builder.WriteString(brief)
	}
	if audience := metadataString(itemMetadata, "target_audience"); audience != "" {
		builder.WriteString("\nTarget audience: ")
		builder.WriteString(audience)
	}
	if outputType := metadataString(itemMetadata, "output_type"); outputType != "" {
		builder.WriteString("\nOutput type: ")
		builder.WriteString(outputType)
	}
	if hints, ok := itemMetadata["artifact_hints"].([]any); ok && len(hints) > 0 {
		builder.WriteString("\nArtifact hints:")
		for _, hint := range hints {
			if text := strings.TrimSpace(fmt.Sprint(hint)); text != "" {
				builder.WriteString("\n- ")
				builder.WriteString(text)
			}
		}
	}
	builder.WriteString("\n\nGeneration rules:\n")
	if rankedRun {
		builder.WriteString("- Make this option meaningfully different from the other labels in layout, content strategy, and visual/interaction direction.\n")
	} else if role == "baseline" {
		builder.WriteString("- Generate the baseline artifact: a solid, conventional answer that satisfies the item brief.\n")
	} else {
		builder.WriteString("- Generate the candidate artifact: a meaningfully different improved answer intended to be compared against the baseline.\n")
	}
	switch run.ExplorationLevel {
	case db.ExplorationLevelHigh:
		builder.WriteString("- Use high exploration: vary the product explanation, proof/content structure, and visual direction substantially.\n")
	case db.ExplorationLevelMedium:
		builder.WriteString("- Use medium exploration: combine promising directions while keeping alternatives visibly different.\n")
	case db.ExplorationLevelLow:
		builder.WriteString("- Use low exploration: make narrow refinements and avoid broad direction changes.\n")
	}
	builder.WriteString("- Keep the artifact self-contained and directly reviewable.\n")
	builder.WriteString("- Preserve the requested output type.\n")
	if skillOptTrainWantsVuePreviewBundle(session) {
		requiresVuePreviewBundle := skillOptTrainRequiresVuePreviewBundle(session)
		builder.WriteString("\nPreview bundle contract:\n")
		if requiresVuePreviewBundle {
			builder.WriteString("- This train session requires a Vue/Vite preview bundle for every generated option.\n")
			builder.WriteString("- Keep gitmoot_result.summary as a string value. The string content must be exactly one serialized JSON object, with no markdown, code fences, or prose.\n")
			builder.WriteString("- Do not set gitmoot_result.summary to a nested object; encode the bundle JSON as the summary string.\n")
		} else {
			builder.WriteString("- This train session is configured for optional Vue/Vite previews. Prefer a Vue/Vite preview bundle so Gitmoot can publish preview URLs; plain text or markdown is accepted only as inline fallback.\n")
			builder.WriteString("- If you return a preview bundle, keep gitmoot_result.summary as a string containing exactly one serialized JSON object, with no markdown, code fences, or prose.\n")
			builder.WriteString("- If you return plain text or markdown fallback, use the normal summary string and do not include the bundle JSON shape.\n")
		}
		builder.WriteString("- Use renderer \"vue-vite\".\n")
		builder.WriteString("- Include build_command exactly \"npm run build\" and dist_dir \"dist\".\n")
		builder.WriteString("- Include files with these required relative paths: package.json, index.html, src/main.js, src/App.vue.\n")
		builder.WriteString("- package.json scripts must include only \"build\": \"vite build\". Do not include dependencies or devDependencies; Gitmoot supplies trusted build dependencies.\n")
		builder.WriteString("- index.html and src/main.js may use a simple Vue mount placeholder; Gitmoot canonicalizes and overwrites them with trusted scaffold files before build.\n")
		builder.WriteString("- src/App.vue must be scriptless template/style Vue only. Do not include script blocks, imports, require, import.meta, @import, or CSS url().\n")
		builder.WriteString("- Each file entry must have path and non-empty content. Use slash-separated relative paths only.\n")
		builder.WriteString("- Do not include local absolute paths, path traversal, secrets, .env files, node_modules, dependency caches, dist, built outputs, vite.config.js, or files outside the required paths.\n")
		builder.WriteString("- The JSON object shape is: renderer string, files array of {path, content}, build_command string, dist_dir string.\n")
	}
	return builder.String()
}

func buildSkillOptTrainGenerationRetryPrompt(basePrompt string, validationError map[string]any) string {
	var builder strings.Builder
	builder.WriteString(basePrompt)
	builder.WriteString("\n\nRetry instruction:\n")
	builder.WriteString("- Retry this same review option only; do not change the item id or option label.\n")
	builder.WriteString("- The previous generated artifact failed validation. Fix the concrete validation error below and return a fresh artifact.\n")
	builder.WriteString("- Do not repeat the same invalid output.\n")
	builder.WriteString("\nValidation error:\n")
	fmt.Fprintf(&builder, "- class: %s\n", validationError["class"])
	fmt.Fprintf(&builder, "- message: %s\n", validationError["message"])
	return builder.String()
}

func skillOptTrainOptionValidationError(itemID string, role string, attempt int, err error) map[string]any {
	return map[string]any{
		"class":   "preview_bundle",
		"item_id": itemID,
		"role":    role,
		"attempt": attempt,
		"message": strings.TrimSpace(err.Error()),
	}
}

func prepareReviewItemContentArtifact(blobStore artifact.Store, runID string, itemID string, role string, content []byte, mediaType string, driver string) (db.EvalArtifact, error) {
	if len(content) == 0 || strings.TrimSpace(string(content)) == "" {
		return db.EvalArtifact{}, fmt.Errorf("%s content is required", role)
	}
	blob, err := blobStore.Put(content)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("store %s artifact blob: %w", role, err)
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = "text/plain"
	}
	if strings.TrimSpace(driver) == "" {
		driver = "text"
	}
	return db.EvalArtifact{
		ID:        reviewItemArtifactID(runID, itemID, role),
		Hash:      blob.Hash,
		MediaType: mediaType,
		SizeBytes: blob.Size,
		Driver:    driver,
	}, nil
}

func skillOptTrainGeneratedOptionMetadata(output localAgentJobOutput, prompt string, generationMode string, templateID string, templateVersionID string, sampleLabel string, previewBundleMetadata *skillopt.PreviewBundleMetadata, retryAttempts int, validationErrors []map[string]any) string {
	metadata := map[string]any{
		"source":              "gitmoot skillopt train continue",
		"job_id":              output.JobID,
		"agent":               output.Agent,
		"prompt":              prompt,
		"raw_output_count":    output.RawOutputCount,
		"generation_mode":     generationMode,
		"template_id":         templateID,
		"template_version_id": templateVersionID,
		"sample_label":        sampleLabel,
	}
	if previewBundleMetadata != nil {
		metadata["preview_bundle"] = *previewBundleMetadata
	}
	if retryAttempts > 0 {
		metadata["retry_attempts"] = retryAttempts
		metadata["validation_errors"] = validationErrors
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func skillOptTrainGenerationStrategy(run db.EvalRun) map[string]any {
	return map[string]any{
		"mode":              run.Mode,
		"exploration_level": run.ExplorationLevel,
		"options_count":     run.OptionsCount,
	}
}
