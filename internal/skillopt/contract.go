package skillopt

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/db"
)

const (
	ContractVersion = 1

	TrainingPackageKind  = "gitmoot-skillopt-training-package"
	CandidatePackageKind = "gitmoot-skillopt-candidate-package"

	CandidateSourceRepo = "gitmoot-skillopt"
	CandidateSourceRef  = "candidate"
)

var ErrNoCandidate = errors.New("optimizer produced no candidate")

type TemplateSnapshot struct {
	ID             string                 `json:"id"`
	VersionID      string                 `json:"version_id"`
	VersionNumber  int                    `json:"version_number"`
	VersionState   string                 `json:"version_state"`
	ContentHash    string                 `json:"content_hash"`
	SourceRepo     string                 `json:"source_repo"`
	SourceRef      string                 `json:"source_ref"`
	SourcePath     string                 `json:"source_path"`
	ResolvedCommit string                 `json:"resolved_commit"`
	Metadata       agenttemplate.Metadata `json:"metadata"`
	Content        string                 `json:"content"`
}

type ArtifactRef struct {
	ID        string `json:"id"`
	Hash      string `json:"hash"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
	Driver    string `json:"driver"`
}

type EvaluatorProfile struct {
	ProfileID        string                 `json:"profile_id,omitempty"`
	TaskKind         string                 `json:"task_kind,omitempty"`
	ArtifactContract string                 `json:"artifact_contract,omitempty"`
	PreviewAdapter   string                 `json:"preview_adapter,omitempty"`
	Checks           []EvaluatorCheckConfig `json:"checks,omitempty"`
	Judge            *EvaluatorJudgeConfig  `json:"judge,omitempty"`
	Metadata         json.RawMessage        `json:"metadata,omitempty"`
}

type EvaluatorCheckConfig struct {
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	When     string          `json:"when,omitempty"`
	Required bool            `json:"required,omitempty"`
	Config   json.RawMessage `json:"config,omitempty"`
}

type EvaluatorJudgeConfig struct {
	Type   string          `json:"type,omitempty"`
	When   string          `json:"when,omitempty"`
	Model  string          `json:"model,omitempty"`
	Config json.RawMessage `json:"config,omitempty"`
}

type EvaluatorStageStatus struct {
	Stage      string          `json:"stage,omitempty"`
	Status     string          `json:"status,omitempty"`
	StartedAt  string          `json:"started_at,omitempty"`
	FinishedAt string          `json:"finished_at,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
}

type EvaluatorCheckResult struct {
	Check    string          `json:"check,omitempty"`
	Severity string          `json:"severity,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Evidence []string        `json:"evidence,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

type EvaluatorFailurePacket struct {
	PrimaryReason string                 `json:"primary_reason,omitempty"`
	HumanReason   string                 `json:"human_reason,omitempty"`
	OptimizerHint string                 `json:"optimizer_hint,omitempty"`
	FailedChecks  []EvaluatorCheckResult `json:"failed_checks,omitempty"`
	Evidence      []string               `json:"evidence,omitempty"`
	StageStatus   []EvaluatorStageStatus `json:"stage_status,omitempty"`
}

type EvaluatorScore struct {
	ProfileID       string                  `json:"profile_id,omitempty"`
	TaskKind        string                  `json:"task_kind,omitempty"`
	Hard            *float64                `json:"hard,omitempty"`
	Soft            *float64                `json:"soft,omitempty"`
	DimensionScores map[string]float64      `json:"dimension_scores,omitempty"`
	FailReason      string                  `json:"fail_reason,omitempty"`
	Failure         *EvaluatorFailurePacket `json:"failure,omitempty"`
	StageStatus     []EvaluatorStageStatus  `json:"stage_status,omitempty"`
	Metadata        json.RawMessage         `json:"metadata,omitempty"`
}

type EvalRun struct {
	ID                string          `json:"id"`
	TemplateID        string          `json:"template_id"`
	TemplateVersionID string          `json:"template_version_id"`
	TargetRepo        string          `json:"target_repo"`
	State             string          `json:"state"`
	Mode              string          `json:"mode,omitempty"`
	ExplorationLevel  string          `json:"exploration_level,omitempty"`
	OptionsCount      int             `json:"options_count,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
}

type EvalReviewOption struct {
	Label      string          `json:"label"`
	ArtifactID string          `json:"artifact_id"`
	Role       string          `json:"role,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

type EvalItem struct {
	ID                  string             `json:"id"`
	Title               string             `json:"title,omitempty"`
	SourceArtifactID    string             `json:"source_artifact_id,omitempty"`
	BaselineArtifactID  string             `json:"baseline_artifact_id,omitempty"`
	CandidateArtifactID string             `json:"candidate_artifact_id,omitempty"`
	PreviewArtifactID   string             `json:"preview_artifact_id,omitempty"`
	DiffArtifactID      string             `json:"diff_artifact_id,omitempty"`
	Options             []EvalReviewOption `json:"options,omitempty"`
	Metadata            json.RawMessage    `json:"metadata,omitempty"`
}

type FeedbackEvent struct {
	RunID     string `json:"run_id"`
	ItemID    string `json:"item_id"`
	Choice    string `json:"choice"`
	Reasoning string `json:"reasoning,omitempty"`
	Reviewer  string `json:"reviewer"`
	Source    string `json:"source"`
	SourceURL string `json:"source_url,omitempty"`
	CreatedAt string `json:"created_at"`
}

type RankedFeedbackEvent struct {
	ID             string          `json:"id"`
	RunID          string          `json:"run_id"`
	ItemID         string          `json:"item_id"`
	Ranking        []string        `json:"ranking"`
	Winner         string          `json:"winner,omitempty"`
	UsefulTraits   json.RawMessage `json:"useful_traits,omitempty"`
	RejectedTraits json.RawMessage `json:"rejected_traits,omitempty"`
	Quality        string          `json:"quality,omitempty"`
	ContinueMode   string          `json:"continue_mode,omitempty"`
	Promote        string          `json:"promote,omitempty"`
	Reasoning      string          `json:"reasoning,omitempty"`
	Reviewer       string          `json:"reviewer"`
	Source         string          `json:"source"`
	SourceURL      string          `json:"source_url,omitempty"`
	CreatedAt      string          `json:"created_at"`
}

type PairwisePreference struct {
	RunID         string `json:"run_id"`
	ItemID        string `json:"item_id"`
	Preferred     string `json:"preferred"`
	Rejected      string `json:"rejected"`
	RankedEventID string `json:"ranked_event_id"`
	Reviewer      string `json:"reviewer"`
	Source        string `json:"source"`
	SourceURL     string `json:"source_url,omitempty"`
	CreatedAt     string `json:"created_at"`
}

type TrainingPackage struct {
	Kind                 string                `json:"kind"`
	ContractVersion      int                   `json:"contract_version"`
	Template             TemplateSnapshot      `json:"template"`
	EvalRun              EvalRun               `json:"eval_run"`
	Items                []EvalItem            `json:"items"`
	Artifacts            []ArtifactRef         `json:"artifacts"`
	FeedbackEvents       []FeedbackEvent       `json:"feedback_events"`
	RankedFeedbackEvents []RankedFeedbackEvent `json:"ranked_feedback_events,omitempty"`
	PairwisePreferences  []PairwisePreference  `json:"pairwise_preferences,omitempty"`
	EvaluatorConfig      json.RawMessage       `json:"evaluator_config,omitempty"`
	EvaluatorProfile     *EvaluatorProfile     `json:"evaluator_profile,omitempty"`
}

type CandidateTemplate struct {
	Content  string                 `json:"content"`
	Metadata agenttemplate.Metadata `json:"metadata"`
}

type CandidateSummary struct {
	DiffArtifactID    string                  `json:"diff_artifact_id,omitempty"`
	Score             *float64                `json:"score,omitempty"`
	PreferenceSummary string                  `json:"preference_summary,omitempty"`
	Metadata          json.RawMessage         `json:"metadata,omitempty"`
	EvaluatorScore    *EvaluatorScore         `json:"evaluator_score,omitempty"`
	Failure           *EvaluatorFailurePacket `json:"failure,omitempty"`
}

type CandidateArtifactRef struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Hash      string `json:"hash"`
	MediaType string `json:"media_type"`
	Driver    string `json:"driver"`
	SizeBytes *int64 `json:"size_bytes,omitempty"`
}

type CandidatePackage struct {
	Kind            string                 `json:"kind"`
	ContractVersion int                    `json:"contract_version"`
	TemplateID      string                 `json:"template_id"`
	BaseVersionID   string                 `json:"base_version_id,omitempty"`
	Candidate       CandidateTemplate      `json:"candidate"`
	EvalReport      json.RawMessage        `json:"eval_report,omitempty"`
	Summary         CandidateSummary       `json:"summary,omitempty"`
	Artifacts       []CandidateArtifactRef `json:"artifacts,omitempty"`
}

type CandidateImportOptions struct {
	SourcePath  string
	ArtifactDir string
	BlobStore   artifact.Store
}

func ExportTrainingPackage(ctx context.Context, store *db.Store, runID string) (TrainingPackage, error) {
	if store == nil {
		return TrainingPackage{}, errors.New("store is required")
	}
	run, err := store.GetEvalRun(ctx, strings.TrimSpace(runID))
	if err != nil {
		return TrainingPackage{}, err
	}
	templateRef := run.TemplateID
	if strings.TrimSpace(run.TemplateVersionID) != "" {
		templateRef = run.TemplateVersionID
	}
	template, err := store.GetAgentTemplateReference(ctx, templateRef)
	if err != nil {
		return TrainingPackage{}, fmt.Errorf("load template %q: %w", templateRef, err)
	}
	snapshot, err := templateSnapshot(template)
	if err != nil {
		return TrainingPackage{}, err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return TrainingPackage{}, err
	}
	exportItems := make([]EvalItem, 0, len(items))
	artifactIDs := map[string]struct{}{}
	for _, item := range items {
		options, err := loadEvalReviewOptions(ctx, store, run.ID, item.ItemID)
		if err != nil {
			return TrainingPackage{}, fmt.Errorf("load item %s options: %w", item.ItemID, err)
		}
		exportItem, err := evalItem(item, options)
		if err != nil {
			return TrainingPackage{}, err
		}
		exportItems = append(exportItems, exportItem)
		for _, id := range itemArtifactIDs(item) {
			artifactIDs[id] = struct{}{}
		}
		for _, option := range options {
			artifactIDs[option.ArtifactID] = struct{}{}
		}
	}
	artifacts, err := loadArtifactRefs(ctx, store, artifactIDs)
	if err != nil {
		return TrainingPackage{}, err
	}
	feedbackEvents, err := loadFeedbackEvents(ctx, store, run.ID)
	if err != nil {
		return TrainingPackage{}, err
	}
	rankedFeedbackEvents, err := loadRankedFeedbackEvents(ctx, store, run.ID)
	if err != nil {
		return TrainingPackage{}, err
	}
	pairwisePreferences, err := loadPairwisePreferences(ctx, store, run.ID)
	if err != nil {
		return TrainingPackage{}, err
	}
	metadata, err := rawJSON(run.MetadataJSON)
	if err != nil {
		return TrainingPackage{}, fmt.Errorf("eval run metadata_json: %w", err)
	}
	return TrainingPackage{
		Kind:            TrainingPackageKind,
		ContractVersion: ContractVersion,
		Template:        snapshot,
		EvalRun: EvalRun{
			ID:                run.ID,
			TemplateID:        run.TemplateID,
			TemplateVersionID: run.TemplateVersionID,
			TargetRepo:        run.TargetRepo,
			State:             run.State,
			Mode:              run.Mode,
			ExplorationLevel:  run.ExplorationLevel,
			OptionsCount:      run.OptionsCount,
			Metadata:          metadata,
		},
		Items:                exportItems,
		Artifacts:            artifacts,
		FeedbackEvents:       feedbackEvents,
		RankedFeedbackEvents: rankedFeedbackEvents,
		PairwisePreferences:  pairwisePreferences,
		EvaluatorConfig:      metadata,
	}, nil
}

func ImportCandidatePackage(ctx context.Context, store *db.Store, candidate CandidatePackage, sourcePath string) (db.AgentTemplateVersion, error) {
	return ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{SourcePath: sourcePath})
}

func ImportCandidatePackageWithOptions(ctx context.Context, store *db.Store, candidate CandidatePackage, options CandidateImportOptions) (db.AgentTemplateVersion, error) {
	if store == nil {
		return db.AgentTemplateVersion{}, errors.New("store is required")
	}
	preparedArtifacts, candidateArtifactIDs, err := prepareCandidateArtifacts(options.ArtifactDir, candidate.Artifacts)
	if err != nil {
		return db.AgentTemplateVersion{}, err
	}
	if len(preparedArtifacts) > 0 && strings.TrimSpace(options.BlobStore.Root) == "" {
		return db.AgentTemplateVersion{}, errors.New("artifact blob store is required")
	}
	if err := validateCandidatePackage(ctx, store, candidate, candidateArtifactIDs); err != nil {
		return db.AgentTemplateVersion{}, err
	}
	if err := validateCandidateArtifactIDsAvailable(ctx, store, candidateArtifactIDs); err != nil {
		return db.AgentTemplateVersion{}, err
	}
	templateID := strings.TrimSpace(candidate.TemplateID)
	baseVersionID, err := candidateBaseVersionID(ctx, store, templateID, candidate.BaseVersionID)
	if err != nil {
		return db.AgentTemplateVersion{}, err
	}
	if err := validateCandidateCreatesPendingVersion(ctx, store, candidate, baseVersionID); err != nil {
		return db.AgentTemplateVersion{}, err
	}
	evalArtifacts, err := storeCandidateArtifactBlobs(options.BlobStore, preparedArtifacts)
	if err != nil {
		return db.AgentTemplateVersion{}, err
	}
	metadataJSON, err := agenttemplate.MarshalMetadata(candidate.Candidate.Metadata)
	if err != nil {
		return db.AgentTemplateVersion{}, err
	}
	sourcePath := strings.TrimSpace(options.SourcePath)
	if sourcePath == "" {
		sourcePath = "candidate-package.json"
	}
	template := db.AgentTemplate{
		ID:             templateID,
		Name:           candidate.Candidate.Metadata.Name,
		Description:    candidate.Candidate.Metadata.Description,
		SourceRepo:     CandidateSourceRepo,
		SourceRef:      CandidateSourceRef,
		SourcePath:     sourcePath,
		ResolvedCommit: agenttemplate.HashContent(candidate.Candidate.Content),
		Content:        candidate.Candidate.Content,
		MetadataJSON:   metadataJSON,
	}
	evalReportJSON, err := rawMessageStorage(candidate.EvalReport)
	if err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("candidate eval_report: %w", err)
	}
	summaryMetadataJSON, err := rawMessageStorage(candidate.Summary.Metadata)
	if err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("candidate summary metadata: %w", err)
	}
	version, err := store.AddPendingAgentTemplateCandidate(ctx, template, db.AgentTemplateCandidateReview{
		TemplateID:          template.ID,
		BaseVersionID:       baseVersionID,
		DiffArtifactID:      strings.TrimSpace(candidate.Summary.DiffArtifactID),
		Score:               candidate.Summary.Score,
		PreferenceSummary:   strings.TrimSpace(candidate.Summary.PreferenceSummary),
		EvalReportJSON:      evalReportJSON,
		SummaryMetadataJSON: summaryMetadataJSON,
		State:               "pending",
	}, evalArtifacts)
	if err != nil {
		return db.AgentTemplateVersion{}, err
	}
	return version, nil
}

type preparedCandidateArtifact struct {
	ref     CandidateArtifactRef
	hash    string
	size    int64
	content []byte
}

func candidateBaseVersionID(ctx context.Context, store *db.Store, templateID string, baseRef string) (string, error) {
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		current, err := store.GetAgentTemplate(ctx, templateID)
		if err != nil {
			return "", fmt.Errorf("load current base version for %q: %w", templateID, err)
		}
		return current.VersionID, nil
	}
	base, err := store.GetAgentTemplateReference(ctx, baseRef)
	if err != nil {
		return "", fmt.Errorf("load base version %q: %w", baseRef, err)
	}
	if base.ID != templateID {
		return "", fmt.Errorf("base version %q belongs to template %q, want %q", baseRef, base.ID, templateID)
	}
	return base.VersionID, nil
}

func validateCandidateCreatesPendingVersion(ctx context.Context, store *db.Store, candidate CandidatePackage, baseVersionID string) error {
	reason := candidateNoCandidateReason(candidate)
	if reason != "" {
		return fmt.Errorf("%w: %s", ErrNoCandidate, reason)
	}
	base, err := store.GetAgentTemplateVersionByID(ctx, strings.TrimSpace(baseVersionID))
	if err != nil {
		return fmt.Errorf("load candidate base version %q: %w", baseVersionID, err)
	}
	candidateHash := agenttemplate.HashContent(candidate.Candidate.Content)
	if strings.TrimSpace(base.ContentHash) == candidateHash || agenttemplate.HashContent(base.Content) == candidateHash {
		return fmt.Errorf("%w: candidate content is unchanged from the base version", ErrNoCandidate)
	}
	return nil
}

func candidateNoCandidateReason(candidate CandidatePackage) string {
	for _, source := range []json.RawMessage{candidate.EvalReport, candidate.Summary.Metadata} {
		reason := rawNoCandidateReason(source)
		if reason != "" {
			return reason
		}
	}
	return ""
}

func rawNoCandidateReason(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	if promotable, ok := data["promotable"].(bool); ok && promotable {
		return ""
	}
	reason, _ := data["no_candidate_reason"].(string)
	return strings.TrimSpace(reason)
}

func validateCandidatePackage(ctx context.Context, store *db.Store, candidate CandidatePackage, candidateArtifactIDs map[string]struct{}) error {
	if candidate.Kind != CandidatePackageKind {
		return fmt.Errorf("candidate package kind must be %q", CandidatePackageKind)
	}
	if candidate.ContractVersion != ContractVersion {
		return fmt.Errorf("candidate package contract_version must be %d", ContractVersion)
	}
	templateID := strings.TrimSpace(candidate.TemplateID)
	if err := agenttemplate.ValidateID(templateID); err != nil {
		return err
	}
	if _, err := store.GetAgentTemplate(ctx, templateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("agent template %s is not installed", templateID)
		}
		return err
	}
	parsed, err := agenttemplate.ParseTemplateContent(candidate.Candidate.Content)
	if err != nil {
		return fmt.Errorf("validate candidate template: %w", err)
	}
	if parsed.Metadata.ID != templateID {
		return fmt.Errorf("candidate template id %q does not match package template_id %q", parsed.Metadata.ID, templateID)
	}
	if !sameMetadata(parsed.Metadata, candidate.Candidate.Metadata) {
		return errors.New("candidate metadata does not match candidate template frontmatter")
	}
	if _, err := candidateBaseVersionID(ctx, store, templateID, candidate.BaseVersionID); err != nil {
		return err
	}
	if strings.TrimSpace(candidate.Summary.DiffArtifactID) != "" {
		diffArtifactID := strings.TrimSpace(candidate.Summary.DiffArtifactID)
		if _, ok := candidateArtifactIDs[diffArtifactID]; !ok {
			if _, err := store.GetEvalArtifact(ctx, diffArtifactID); err != nil {
				return fmt.Errorf("load summary diff artifact %q: %w", candidate.Summary.DiffArtifactID, err)
			}
		}
	}
	if _, err := rawMessageStorage(candidate.EvalReport); err != nil {
		return fmt.Errorf("candidate eval_report: %w", err)
	}
	if _, err := rawMessageStorage(candidate.Summary.Metadata); err != nil {
		return fmt.Errorf("candidate summary metadata: %w", err)
	}
	return nil
}

func validateCandidateArtifactIDsAvailable(ctx context.Context, store *db.Store, ids map[string]struct{}) error {
	for id := range ids {
		if _, err := store.GetEvalArtifact(ctx, id); err == nil {
			return fmt.Errorf("candidate artifact %q already exists", id)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load candidate artifact %q: %w", id, err)
		}
	}
	return nil
}

func prepareCandidateArtifacts(artifactDir string, refs []CandidateArtifactRef) ([]preparedCandidateArtifact, map[string]struct{}, error) {
	ids := make(map[string]struct{}, len(refs))
	if len(refs) == 0 {
		return nil, ids, nil
	}
	if strings.TrimSpace(artifactDir) == "" {
		return nil, nil, errors.New("candidate artifacts require --artifact-dir")
	}
	prepared := make([]preparedCandidateArtifact, 0, len(refs))
	for index, ref := range refs {
		ref.ID = strings.TrimSpace(ref.ID)
		ref.Path = strings.TrimSpace(ref.Path)
		ref.Hash = strings.TrimSpace(ref.Hash)
		ref.MediaType = strings.TrimSpace(ref.MediaType)
		ref.Driver = strings.TrimSpace(ref.Driver)
		if ref.ID == "" {
			return nil, nil, fmt.Errorf("candidate artifact %d id is required", index+1)
		}
		if _, exists := ids[ref.ID]; exists {
			return nil, nil, fmt.Errorf("candidate artifact %q is duplicated", ref.ID)
		}
		ids[ref.ID] = struct{}{}
		if ref.MediaType == "" {
			return nil, nil, fmt.Errorf("candidate artifact %q media_type is required", ref.ID)
		}
		if ref.Driver == "" {
			return nil, nil, fmt.Errorf("candidate artifact %q driver is required", ref.ID)
		}
		if ref.SizeBytes != nil && *ref.SizeBytes < 0 {
			return nil, nil, fmt.Errorf("candidate artifact %q size_bytes cannot be negative", ref.ID)
		}
		expectedHash, err := artifact.NormalizeHash(ref.Hash)
		if err != nil {
			return nil, nil, fmt.Errorf("candidate artifact %q hash: %w", ref.ID, err)
		}
		path, err := resolveCandidateArtifactPath(artifactDir, ref.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("candidate artifact %q path: %w", ref.ID, err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read candidate artifact %q: %w", ref.ID, err)
		}
		size := int64(len(content))
		if ref.SizeBytes != nil && *ref.SizeBytes != size {
			return nil, nil, fmt.Errorf("candidate artifact %q size_bytes is %d, want %d", ref.ID, *ref.SizeBytes, size)
		}
		actualHash := artifact.ContentHash(content)
		if actualHash != expectedHash {
			return nil, nil, fmt.Errorf("candidate artifact %q hash is %s, want %s", ref.ID, actualHash, expectedHash)
		}
		ref.Hash = expectedHash
		prepared = append(prepared, preparedCandidateArtifact{ref: ref, hash: actualHash, size: size, content: content})
	}
	return prepared, ids, nil
}

func resolveCandidateArtifactPath(artifactDir string, artifactPath string) (string, error) {
	artifactPath = strings.TrimSpace(artifactPath)
	if artifactPath == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(artifactPath) || !filepath.IsLocal(artifactPath) {
		return "", fmt.Errorf("%q must be a relative path inside artifact-dir", artifactPath)
	}
	root, err := filepath.Abs(strings.TrimSpace(artifactDir))
	if err != nil {
		return "", fmt.Errorf("resolve artifact-dir: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve artifact-dir symlinks: %w", err)
	}
	candidatePath := filepath.Join(root, filepath.Clean(artifactPath))
	candidatePath, err = filepath.EvalSymlinks(candidatePath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, candidatePath)
	if err != nil {
		return "", fmt.Errorf("verify artifact path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q resolves outside artifact-dir", artifactPath)
	}
	return candidatePath, nil
}

func storeCandidateArtifactBlobs(blobStore artifact.Store, preparedArtifacts []preparedCandidateArtifact) ([]db.EvalArtifact, error) {
	evalArtifacts := make([]db.EvalArtifact, 0, len(preparedArtifacts))
	for _, prepared := range preparedArtifacts {
		blob, err := blobStore.Put(prepared.content)
		if err != nil {
			return nil, fmt.Errorf("store candidate artifact %q blob: %w", prepared.ref.ID, err)
		}
		if blob.Hash != prepared.hash {
			return nil, fmt.Errorf("store candidate artifact %q blob hash is %s, want %s", prepared.ref.ID, blob.Hash, prepared.hash)
		}
		evalArtifacts = append(evalArtifacts, db.EvalArtifact{
			ID:        prepared.ref.ID,
			Hash:      prepared.hash,
			MediaType: prepared.ref.MediaType,
			SizeBytes: prepared.size,
			Driver:    prepared.ref.Driver,
		})
	}
	return evalArtifacts, nil
}

func templateSnapshot(template db.AgentTemplate) (TemplateSnapshot, error) {
	metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON)
	if err != nil {
		return TemplateSnapshot{}, err
	}
	return TemplateSnapshot{
		ID:             template.ID,
		VersionID:      template.VersionID,
		VersionNumber:  template.VersionNumber,
		VersionState:   template.VersionState,
		ContentHash:    template.ContentHash,
		SourceRepo:     template.SourceRepo,
		SourceRef:      template.SourceRef,
		SourcePath:     template.SourcePath,
		ResolvedCommit: template.ResolvedCommit,
		Metadata:       metadata,
		Content:        template.Content,
	}, nil
}

func evalItem(item db.EvalReviewItem, options []EvalReviewOption) (EvalItem, error) {
	metadata, err := rawJSON(item.MetadataJSON)
	if err != nil {
		return EvalItem{}, fmt.Errorf("eval item %s metadata_json: %w", item.ItemID, err)
	}
	return EvalItem{
		ID:                  item.ItemID,
		Title:               item.Title,
		SourceArtifactID:    item.SourceArtifactID,
		BaselineArtifactID:  item.BaselineArtifactID,
		CandidateArtifactID: item.CandidateArtifactID,
		PreviewArtifactID:   item.PreviewArtifactID,
		DiffArtifactID:      item.DiffArtifactID,
		Options:             options,
		Metadata:            metadata,
	}, nil
}

func loadEvalReviewOptions(ctx context.Context, store *db.Store, runID string, itemID string) ([]EvalReviewOption, error) {
	options, err := store.ListEvalReviewOptions(ctx, runID, itemID)
	if err != nil {
		return nil, err
	}
	output := make([]EvalReviewOption, 0, len(options))
	for _, option := range options {
		metadata, err := rawJSON(option.MetadataJSON)
		if err != nil {
			return nil, fmt.Errorf("review option %s metadata_json: %w", option.Label, err)
		}
		output = append(output, EvalReviewOption{
			Label:      option.Label,
			ArtifactID: option.ArtifactID,
			Role:       option.Role,
			Metadata:   metadata,
		})
	}
	return output, nil
}

func itemArtifactIDs(item db.EvalReviewItem) []string {
	values := []string{
		item.SourceArtifactID,
		item.BaselineArtifactID,
		item.CandidateArtifactID,
		item.PreviewArtifactID,
		item.DiffArtifactID,
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			ids = append(ids, value)
		}
	}
	return ids
}

func loadArtifactRefs(ctx context.Context, store *db.Store, ids map[string]struct{}) ([]ArtifactRef, error) {
	sortedIDs := make([]string, 0, len(ids))
	for id := range ids {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)
	refs := make([]ArtifactRef, 0, len(sortedIDs))
	for _, id := range sortedIDs {
		artifact, err := store.GetEvalArtifact(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load artifact %q: %w", id, err)
		}
		refs = append(refs, ArtifactRef{
			ID:        artifact.ID,
			Hash:      artifact.Hash,
			MediaType: artifact.MediaType,
			SizeBytes: artifact.SizeBytes,
			Driver:    artifact.Driver,
		})
	}
	return refs, nil
}

func loadFeedbackEvents(ctx context.Context, store *db.Store, runID string) ([]FeedbackEvent, error) {
	events, err := store.ListFeedbackEvents(ctx, runID)
	if err != nil {
		return nil, err
	}
	output := make([]FeedbackEvent, 0, len(events))
	for _, event := range events {
		output = append(output, FeedbackEvent{
			RunID:     event.RunID,
			ItemID:    event.ItemID,
			Choice:    event.Choice,
			Reasoning: event.Reasoning,
			Reviewer:  event.Reviewer,
			Source:    event.Source,
			SourceURL: event.SourceURL,
			CreatedAt: event.CreatedAt,
		})
	}
	return output, nil
}

func loadRankedFeedbackEvents(ctx context.Context, store *db.Store, runID string) ([]RankedFeedbackEvent, error) {
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		return nil, err
	}
	output := make([]RankedFeedbackEvent, 0, len(events))
	for _, event := range events {
		ranking, err := rankedFeedbackRanking(event)
		if err != nil {
			return nil, err
		}
		usefulTraits, err := rawJSON(event.UsefulTraitsJSON)
		if err != nil {
			return nil, fmt.Errorf("ranked feedback %s useful_traits_json: %w", event.ID, err)
		}
		rejectedTraits, err := rawJSON(event.RejectedTraitsJSON)
		if err != nil {
			return nil, fmt.Errorf("ranked feedback %s rejected_traits_json: %w", event.ID, err)
		}
		output = append(output, RankedFeedbackEvent{
			ID:             event.ID,
			RunID:          event.RunID,
			ItemID:         event.ItemID,
			Ranking:        ranking,
			Winner:         event.Winner,
			UsefulTraits:   usefulTraits,
			RejectedTraits: rejectedTraits,
			Quality:        event.Quality,
			ContinueMode:   event.ContinueMode,
			Promote:        event.Promote,
			Reasoning:      event.Reasoning,
			Reviewer:       event.Reviewer,
			Source:         event.Source,
			SourceURL:      event.SourceURL,
			CreatedAt:      event.CreatedAt,
		})
	}
	return output, nil
}

func rankedFeedbackRanking(event db.RankedFeedbackEvent) ([]string, error) {
	var ranking []string
	if err := json.Unmarshal([]byte(event.RankingJSON), &ranking); err != nil {
		return nil, fmt.Errorf("ranked feedback %s ranking_json: %w", event.ID, err)
	}
	return ranking, nil
}

func loadPairwisePreferences(ctx context.Context, store *db.Store, runID string) ([]PairwisePreference, error) {
	preferences, err := store.ListPairwisePreferences(ctx, runID)
	if err != nil {
		return nil, err
	}
	output := make([]PairwisePreference, 0, len(preferences))
	for _, preference := range preferences {
		output = append(output, PairwisePreference{
			RunID:         preference.RunID,
			ItemID:        preference.ItemID,
			Preferred:     preference.Preferred,
			Rejected:      preference.Rejected,
			RankedEventID: preference.RankedEventID,
			Reviewer:      preference.Reviewer,
			Source:        preference.Source,
			SourceURL:     preference.SourceURL,
			CreatedAt:     preference.CreatedAt,
		})
	}
	return output, nil
}

func rawJSON(value string) (json.RawMessage, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, err
	}
	return json.RawMessage(value), nil
}

func rawMessageStorage(value json.RawMessage) (string, error) {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return "", nil
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, value); err != nil {
		return "", err
	}
	return compacted.String(), nil
}

func sameMetadata(a agenttemplate.Metadata, b agenttemplate.Metadata) bool {
	encodedA, err := json.Marshal(a)
	if err != nil {
		return false
	}
	encodedB, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(encodedA) == string(encodedB)
}
