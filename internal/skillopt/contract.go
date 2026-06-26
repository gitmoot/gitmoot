package skillopt

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/db"
)

const (
	ContractVersion = 1

	TrainingPackageKind       = "gitmoot-skillopt-training-package"
	CandidatePackageKind      = "gitmoot-skillopt-candidate-package"
	JudgeCandidatePackageKind = "gitmoot-skillopt-judge-candidate"

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

// JudgePromptConfig is the additive, per-task_kind judge prompt payload carried
// inside EvaluatorJudgeConfig.Config. It (un)marshals from the existing opaque
// json.RawMessage passthrough, so it adds no new top-level contract fields and
// does not bump ContractVersion. The Python judge reads
// evaluator_profile.judge.config.judge_prompt_templates (keyed by task_kind) and
// evaluator_profile.judge.config.judge_prompt_version.
type JudgePromptConfig struct {
	JudgePromptTemplates map[string]string `json:"judge_prompt_templates,omitempty"`
	JudgePromptVersion   string            `json:"judge_prompt_version,omitempty"`
}

// JudgePromptConfig extracts the per-task_kind judge prompt templates and
// version carried inside the judge Config passthrough, if present and
// well-formed. It returns nil when neither field is present. Malformed shapes
// (templates not an object of string→string, version not a string) are ignored,
// consistent with the file's other best-effort metadata accessors, so a bad
// payload never crashes export or import.
func (c *EvaluatorJudgeConfig) JudgePromptConfig() *JudgePromptConfig {
	if c == nil {
		return nil
	}
	return parseJudgePromptConfig(c.Config)
}

func parseJudgePromptConfig(raw json.RawMessage) *JudgePromptConfig {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	out := &JudgePromptConfig{}
	if rawTemplates, ok := decoded["judge_prompt_templates"]; ok {
		var templates map[string]string
		if err := json.Unmarshal(rawTemplates, &templates); err == nil && len(templates) > 0 {
			out.JudgePromptTemplates = templates
		}
	}
	if rawVersion, ok := decoded["judge_prompt_version"]; ok {
		var version string
		if err := json.Unmarshal(rawVersion, &version); err == nil {
			out.JudgePromptVersion = strings.TrimSpace(version)
		}
	}
	if len(out.JudgePromptTemplates) == 0 && out.JudgePromptVersion == "" {
		return nil
	}
	return out
}

// mergeJudgePromptConfig folds the per-task_kind judge prompt templates and
// version (parsed from the evaluator config) into the judge's opaque Config
// passthrough without disturbing any other keys already present there. It is
// additive: existing Config keys win on conflict only for non-judge-prompt
// fields; the judge-prompt fields are (re)written from the parsed payload so the
// validated shape is what survives export. A nil payload leaves Config
// untouched.
func mergeJudgePromptConfig(existing json.RawMessage, payload *JudgePromptConfig) (json.RawMessage, error) {
	if payload == nil || (len(payload.JudgePromptTemplates) == 0 && payload.JudgePromptVersion == "") {
		return existing, nil
	}
	merged := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(existing); len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &merged); err != nil {
			// Existing Config is not a JSON object we can extend; do not clobber it.
			return existing, nil
		}
	}
	if len(payload.JudgePromptTemplates) > 0 {
		encoded, err := json.Marshal(payload.JudgePromptTemplates)
		if err != nil {
			return existing, err
		}
		merged["judge_prompt_templates"] = encoded
	}
	if payload.JudgePromptVersion != "" {
		encoded, err := json.Marshal(payload.JudgePromptVersion)
		if err != nil {
			return existing, err
		}
		merged["judge_prompt_version"] = encoded
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return existing, err
	}
	return json.RawMessage(out), nil
}

// JudgeCandidateVariant is one per-task_kind result emitted by the judge-prompt
// optimizer (#345 Phase 2). The "_global" key in JudgeCandidatePackage.Variants
// carries the all-items pass (TaskKind is empty there). A variant is only
// eligible for promotion when Accepted is true and BestPrompt is non-empty.
type JudgeCandidateVariant struct {
	TaskKind           string  `json:"task_kind"`
	NItems             int     `json:"n_items"`
	BaselineAgreement  float64 `json:"baseline_agreement"`
	BestAgreement      float64 `json:"best_agreement"`
	BestOrigin         string  `json:"best_origin"`
	JudgePromptVersion string  `json:"judge_prompt_version"`
	Accepted           bool    `json:"accepted"`
	BestPrompt         string  `json:"best_prompt"`
}

// JudgeCandidatePackage is the structured output of the judge-prompt optimizer,
// emitted by the Python side as kind=JudgeCandidatePackageKind. It carries one
// JudgeCandidateVariant per task_kind keyed in Variants (plus a "_global" pass).
// `gitmoot skillopt judge promote` reads an accepted variant out of this package
// and writes it into a template so the next skill-opt run judges with it.
type JudgeCandidatePackage struct {
	Kind                   string                           `json:"kind"`
	ContractVersion        int                              `json:"contract_version"`
	JudgePromptVersionBase string                           `json:"judge_prompt_version_base,omitempty"`
	NLabeled               int                              `json:"n_labeled,omitempty"`
	Variants               map[string]JudgeCandidateVariant `json:"variants"`
}

// ParseJudgeCandidatePackage decodes and validates a judge-candidate package
// blob, enforcing the kind tag and ContractVersion so a stale or foreign payload
// is rejected before any promotion is attempted.
func ParseJudgeCandidatePackage(data []byte) (JudgeCandidatePackage, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return JudgeCandidatePackage{}, errors.New("judge candidate package is empty")
	}
	var pkg JudgeCandidatePackage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return JudgeCandidatePackage{}, fmt.Errorf("decode judge candidate package: %w", err)
	}
	if pkg.Kind != JudgeCandidatePackageKind {
		return JudgeCandidatePackage{}, fmt.Errorf("judge candidate package kind must be %q, got %q", JudgeCandidatePackageKind, pkg.Kind)
	}
	if pkg.ContractVersion != ContractVersion {
		return JudgeCandidatePackage{}, fmt.Errorf("judge candidate package contract_version must be %d, got %d", ContractVersion, pkg.ContractVersion)
	}
	if len(pkg.Variants) == 0 {
		return JudgeCandidatePackage{}, errors.New("judge candidate package has no variants")
	}
	return pkg, nil
}

// EvaluationConfigForReader expands a template's flat Evaluation map
// (map[string]string, where judge_prompt_templates is stored as a JSON-encoded
// object string per the write contract of `judge promote`) into the nested
// evaluator config that judgePromptConfigFromConfig / EvaluatorProfileFromConfig
// consume. It mirrors how the eval-run start path nests an "evaluation" object:
// the JSON-string value of judge_prompt_templates is re-inlined as a real object
// so the reader's map[string]string decode succeeds. This is the bridge that
// proves the round-trip without changing the reader's contract. It returns nil
// for an empty map so callers can fall back to existing config sources.
func EvaluationConfigForReader(evaluation map[string]string) json.RawMessage {
	if len(evaluation) == 0 {
		return nil
	}
	nested := make(map[string]json.RawMessage, len(evaluation))
	for key, value := range evaluation {
		switch key {
		case "judge_prompt_templates":
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				continue
			}
			// Stored as a JSON-encoded object string; re-inline as raw JSON when
			// it parses as the map[string]string the reader expects, otherwise
			// fall back to a JSON string so the value is never silently dropped.
			var probe map[string]string
			if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
				nested[key] = json.RawMessage(trimmed)
				continue
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				continue
			}
			nested[key] = encoded
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				continue
			}
			nested[key] = encoded
		}
	}
	if len(nested) == 0 {
		return nil
	}
	nestedRaw, err := json.Marshal(nested)
	if err != nil {
		return nil
	}
	wrapper, err := json.Marshal(map[string]json.RawMessage{"evaluation": nestedRaw})
	if err != nil {
		return nil
	}
	return json.RawMessage(wrapper)
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

type GateRejectionScores struct {
	Hard      *float64 `json:"hard,omitempty"`
	Soft      *float64 `json:"soft,omitempty"`
	GateScore *float64 `json:"gate_score,omitempty"`
}

type GateRejectionPacket struct {
	RejectionType        string              `json:"rejection_type,omitempty"`
	Retryable            bool                `json:"retryable,omitempty"`
	Baseline             GateRejectionScores `json:"baseline,omitempty"`
	Candidate            GateRejectionScores `json:"candidate,omitempty"`
	PrimaryReason        string              `json:"primary_reason,omitempty"`
	HumanReason          string              `json:"human_reason,omitempty"`
	OptimizerHint        string              `json:"optimizer_hint,omitempty"`
	FailedDimensions     []string            `json:"failed_dimensions,omitempty"`
	Evidence             []string            `json:"evidence,omitempty"`
	HumanFeedbackContext json.RawMessage     `json:"human_feedback_context,omitempty"`
	AttemptedPatch       string              `json:"attempted_patch,omitempty"`
	RetryAttempts        string              `json:"retry_attempts,omitempty"`
	NextAction           string              `json:"next_action,omitempty"`
}

type EvaluatorScore struct {
	ProfileID              string                  `json:"profile_id,omitempty"`
	TaskKind               string                  `json:"task_kind,omitempty"`
	ContractStatus         string                  `json:"contract_status,omitempty"`
	QualityStatus          string                  `json:"quality_status,omitempty"`
	HumanFeedbackAlignment json.RawMessage         `json:"human_feedback_alignment,omitempty"`
	Hard                   *float64                `json:"hard,omitempty"`
	Soft                   *float64                `json:"soft,omitempty"`
	DimensionScores        map[string]float64      `json:"dimension_scores,omitempty"`
	FailReason             string                  `json:"fail_reason,omitempty"`
	Failure                *EvaluatorFailurePacket `json:"failure,omitempty"`
	GateRejection          *GateRejectionPacket    `json:"gate_rejection,omitempty"`
	StageStatus            []EvaluatorStageStatus  `json:"stage_status,omitempty"`
	Metadata               json.RawMessage         `json:"metadata,omitempty"`
	JudgePromptVersion     string                  `json:"judge_prompt_version,omitempty"`
	JudgeEvaluatorID       string                  `json:"judge_evaluator_id,omitempty"`
	JudgePromptHash        string                  `json:"judge_prompt_hash,omitempty"`
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
	ID                   string          `json:"id"`
	RunID                string          `json:"run_id"`
	ItemID               string          `json:"item_id"`
	Ranking              []string        `json:"ranking"`
	TieGroups            [][]string      `json:"tie_groups,omitempty"`
	Winner               string          `json:"winner,omitempty"`
	UsefulTraits         json.RawMessage `json:"useful_traits,omitempty"`
	RejectedTraits       json.RawMessage `json:"rejected_traits,omitempty"`
	RequiredImprovements json.RawMessage `json:"required_improvements,omitempty"`
	Quality              string          `json:"quality,omitempty"`
	ContinueMode         string          `json:"continue_mode,omitempty"`
	Promote              string          `json:"promote,omitempty"`
	Reasoning            string          `json:"reasoning,omitempty"`
	Reviewer             string          `json:"reviewer"`
	Source               string          `json:"source"`
	SourceURL            string          `json:"source_url,omitempty"`
	CreatedAt            string          `json:"created_at"`
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
	TrainingMode         string                `json:"training_mode,omitempty"`
	Template             TemplateSnapshot      `json:"template"`
	EvalRun              EvalRun               `json:"eval_run"`
	Items                []EvalItem            `json:"items"`
	Artifacts            []ArtifactRef         `json:"artifacts"`
	FeedbackEvents       []FeedbackEvent       `json:"feedback_events"`
	RankedFeedbackEvents []RankedFeedbackEvent `json:"ranked_feedback_events,omitempty"`
	PairwisePreferences  []PairwisePreference  `json:"pairwise_preferences,omitempty"`
	FeedbackContext      json.RawMessage       `json:"feedback_context,omitempty"`
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
	GateRejection     *GateRejectionPacket    `json:"gate_rejection,omitempty"`
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

// feedbackByteCap bounds the assembled NormalizedSignal.Feedback string so a
// read-side projection of very verbose trait/reasoning fields stays human- and
// optimizer-readable. The cap is documented in the SkillOpt exchange contract
// docs (8 KiB). When the assembled text exceeds the cap it is truncated and a
// trailing marker is appended.
const feedbackByteCap = 8 * 1024

// feedbackTruncationMarker is appended when Feedback is truncated at the byte
// cap so consumers can tell the tail was clipped.
const feedbackTruncationMarker = "… (truncated)"

// NormalizedSignal is a pure, read-side projection of the scalar quality signal
// and the textual feedback that the SkillOpt contract already carries across
// several optional fields. It is a view/return type only: it is NOT embedded in
// any wire package (TrainingPackage/CandidatePackage), adds no field to any
// existing contract struct, and does not change ContractVersion or the bytes the
// optimizer subprocess reads.
//
// Score is in the range [0,1] when HasScore is true. When HasScore is false the
// scalar signal was genuinely absent and Score is meaningless (do not treat it
// as a neutral midpoint). Feedback is a single bounded string assembled in a
// fixed section order; it is empty when no textual signal was present.
type NormalizedSignal struct {
	Score    float64 `json:"score"`
	HasScore bool    `json:"has_score"`
	Feedback string  `json:"feedback,omitempty"`
}

// ProjectSignal projects the already-present scalar and textual fields into one
// uniform {Score, Feedback} view so consumers read a single signal instead of N
// optional fields. All inputs are pointers and nil-tolerant, so callers pass
// whatever subset they hold. It is pure and total: it never panics and has no
// side effects.
//
// Scalar precedence (see normalizedScore): a present Hard == 0 is an
// authoritative gate-fail 0 (HasScore=true); otherwise the quality component is
// Soft, else the mean of DimensionScores, else Hard > 0; genuinely-absent data
// yields HasScore=false. Textual assembly (see normalizedFeedback) concatenates
// the non-empty parts in a fixed, deterministic order, bounded by a byte cap.
func ProjectSignal(score *EvaluatorScore, ranked *RankedFeedbackEvent, failure *EvaluatorFailurePacket) NormalizedSignal {
	value, ok := normalizedScore(score)
	return NormalizedSignal{
		Score:    value,
		HasScore: ok,
		Feedback: normalizedFeedback(ranked, failure),
	}
}

// normalizedScore fuses EvaluatorScore.Hard/Soft/DimensionScores into a single
// score in [0,1] and reports whether any usable scalar field existed.
//
// Rules:
//  1. Hard present and == 0 (gate failed) → (0, true): a hard-fail dominates and
//     is a real, informative 0, not "missing".
//  2. Otherwise the quality component is Soft if present, else the arithmetic
//     mean of DimensionScores if the map is non-empty, else Hard if Hard > 0,
//     else unknown. Hard > 0 is a gate, not a weight, so it does not scale the
//     quality component.
//  3. A usable quality component → (clamp(quality), true).
//  4. No usable field (Hard nil, Soft nil, DimensionScores empty) → (0, false):
//     absent, not a fabricated neutral 0.5.
//
// Every returned score is clamped to [0,1].
func normalizedScore(score *EvaluatorScore) (float64, bool) {
	if score == nil {
		return 0, false
	}
	// A hard-fail gate (Hard == 0) is an authoritative, informative 0.
	if score.Hard != nil && *score.Hard == 0 {
		return 0, true
	}
	// Quality component precedence: Soft > mean(DimensionScores) > Hard.
	if score.Soft != nil {
		return clampUnit(*score.Soft), true
	}
	if len(score.DimensionScores) > 0 {
		var sum float64
		for _, value := range score.DimensionScores {
			sum += value
		}
		return clampUnit(sum / float64(len(score.DimensionScores))), true
	}
	if score.Hard != nil && *score.Hard > 0 {
		return clampUnit(*score.Hard), true
	}
	return 0, false
}

func clampUnit(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// normalizedFeedback deterministically concatenates the non-empty textual parts
// into one bounded string, in a fixed section order:
//
//	OptimizerHint, RequiredImprovements, UsefulTraits, RejectedTraits, Reasoning
//
// Each section gets a stable header. json.RawMessage trait fields are best-effort
// decoded into the known shapes (map[string][]string keyed by option label, or
// []string); map keys are sorted for determinism, and an undecodable section is
// skipped rather than dumped raw (the function never panics and never emits raw
// braces). The whole string is truncated to feedbackByteCap with a trailing
// marker. Empty inputs yield "".
func normalizedFeedback(ranked *RankedFeedbackEvent, failure *EvaluatorFailurePacket) string {
	var sections []string
	if failure != nil {
		if hint := strings.TrimSpace(failure.OptimizerHint); hint != "" {
			sections = append(sections, "Optimizer hint:\n"+hint)
		}
	}
	if ranked != nil {
		if section := feedbackListSection("Required improvements:", ranked.RequiredImprovements); section != "" {
			sections = append(sections, section)
		}
		if section := feedbackTraitsSection("Useful traits:", ranked.UsefulTraits); section != "" {
			sections = append(sections, section)
		}
		if section := feedbackTraitsSection("Rejected traits:", ranked.RejectedTraits); section != "" {
			sections = append(sections, section)
		}
		if reasoning := strings.TrimSpace(ranked.Reasoning); reasoning != "" {
			sections = append(sections, "Reasoning:\n"+reasoning)
		}
	}
	return truncateFeedback(strings.Join(sections, "\n\n"))
}

// feedbackListSection renders a RequiredImprovements-shaped json.RawMessage
// ([]string) as a bulleted section, or "" when empty/undecodable.
func feedbackListSection(header string, raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			lines = append(lines, "- "+item)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return header + "\n" + strings.Join(lines, "\n")
}

// feedbackTraitsSection renders a trait-shaped json.RawMessage
// (map[string][]string keyed by option label) as deterministic "label: trait"
// lines with sorted keys, or "" when empty/undecodable.
func feedbackTraitsSection(header string, raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var traits map[string][]string
	if err := json.Unmarshal(raw, &traits); err != nil {
		return ""
	}
	labels := make([]string, 0, len(traits))
	for label := range traits {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	lines := make([]string, 0, len(labels))
	for _, label := range labels {
		for _, trait := range traits[label] {
			trait = strings.TrimSpace(trait)
			if trait != "" {
				lines = append(lines, label+": "+trait)
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return header + "\n" + strings.Join(lines, "\n")
}

// truncateFeedback bounds text to feedbackByteCap, appending a marker when it
// clips. It cuts on a UTF-8 rune boundary so the output stays valid UTF-8.
func truncateFeedback(text string) string {
	if len(text) <= feedbackByteCap {
		return text
	}
	limit := feedbackByteCap - len(feedbackTruncationMarker)
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit] + feedbackTruncationMarker
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
	feedbackContext, err := buildTrainingFeedbackContext(run, feedbackEvents, rankedFeedbackEvents)
	if err != nil {
		return TrainingPackage{}, err
	}
	metadata, err := rawJSON(run.MetadataJSON)
	if err != nil {
		return TrainingPackage{}, fmt.Errorf("eval run metadata_json: %w", err)
	}
	evaluatorConfig, err := evaluatorConfigFromRunMetadata(metadata)
	if err != nil {
		return TrainingPackage{}, fmt.Errorf("eval run evaluator_config: %w", err)
	}
	evaluatorProfile := EvaluatorProfileFromConfig(evaluatorConfig)
	return TrainingPackage{
		Kind:            TrainingPackageKind,
		ContractVersion: ContractVersion,
		TrainingMode:    run.Mode,
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
		FeedbackContext:      feedbackContext,
		EvaluatorConfig:      evaluatorConfig,
		EvaluatorProfile:     evaluatorProfile,
	}, nil
}

func evaluatorConfigFromRunMetadata(metadata json.RawMessage) (json.RawMessage, error) {
	metadata = bytes.TrimSpace(metadata)
	if len(metadata) == 0 {
		return nil, nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		return nil, err
	}
	if rawMode, ok := decoded["mode"]; ok {
		var mode string
		if err := json.Unmarshal(rawMode, &mode); err == nil && isTrainingMode(mode) {
			delete(decoded, "mode")
		}
	}
	if len(decoded) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func isTrainingMode(value string) bool {
	switch strings.TrimSpace(value) {
	case db.EvalRunModeExplore, db.EvalRunModeRefine, db.EvalRunModeDistill, db.EvalRunModeValidate:
		return true
	default:
		return false
	}
}

func buildTrainingFeedbackContext(run db.EvalRun, feedback []FeedbackEvent, ranked []RankedFeedbackEvent) (json.RawMessage, error) {
	if len(feedback) == 0 && len(ranked) == 0 {
		return nil, nil
	}
	context := map[string]any{
		"feedback_source":        "imported_human_review",
		"feedback_target":        "baseline_review_outputs",
		"review_run_id":          run.ID,
		"reviewed_skill_version": run.TemplateVersionID,
	}
	if issue := reviewIssueFromFeedback(feedback, ranked); issue != "" {
		context["review_issue"] = issue
	}
	if run.TargetRepo != "" {
		context["target_repo"] = run.TargetRepo
	}
	raw, err := json.Marshal(context)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func reviewIssueFromFeedback(feedback []FeedbackEvent, ranked []RankedFeedbackEvent) string {
	for _, event := range ranked {
		if issue := reviewIssueFromSourceURL(event.SourceURL); issue != "" {
			return issue
		}
	}
	for _, event := range feedback {
		if issue := reviewIssueFromSourceURL(event.SourceURL); issue != "" {
			return issue
		}
	}
	return ""
}

func reviewIssueFromSourceURL(sourceURL string) string {
	value := strings.TrimSpace(sourceURL)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(parsed.Host)
	if host != "github.com" && !strings.HasSuffix(host, ".github.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return ""
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSpace(parts[1])
	number := strings.TrimSpace(parts[3])
	if owner == "" || repo == "" || number == "" {
		return ""
	}
	return owner + "/" + repo + "#" + number
}

func EvaluatorProfileFromConfig(config json.RawMessage) *EvaluatorProfile {
	var metadata map[string]any
	if len(bytes.TrimSpace(config)) == 0 {
		return nil
	}
	if err := json.Unmarshal(config, &metadata); err != nil {
		return nil
	}
	evaluatorID := evaluatorProfileMetadataString(metadata, "evaluator_id")
	if evaluatorID == "" {
		evaluatorID = evaluatorProfileMetadataString(metadata, "evaluation", "evaluator_id")
	}
	evaluatorModel := evaluatorProfileMetadataString(metadata, "evaluator_model")
	if evaluatorModel == "" {
		evaluatorModel = evaluatorProfileMetadataString(metadata, "evaluation", "evaluator_model")
	}
	return BuildEvaluatorProfile(evaluatorID, evaluatorModel, config)
}

// judgePromptConfigFromConfig pulls the per-task_kind judge prompt templates and
// version out of the evaluator config, accepting them either at the top level or
// nested under "evaluation" (mirroring how evaluator_id/evaluator_model are
// read). It is best-effort: malformed shapes yield nil rather than an error.
func judgePromptConfigFromConfig(config json.RawMessage) *JudgePromptConfig {
	if len(bytes.TrimSpace(config)) == 0 {
		return nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(config, &decoded); err != nil {
		return nil
	}
	if payload := parseJudgePromptConfig(config); payload != nil {
		return payload
	}
	if nested, ok := decoded["evaluation"]; ok {
		return parseJudgePromptConfig(nested)
	}
	return nil
}

func BuildEvaluatorProfile(evaluatorID string, evaluatorModel string, metadata json.RawMessage) *EvaluatorProfile {
	profileID := strings.TrimSpace(evaluatorID)
	evaluatorID = strings.ToLower(profileID)
	if evaluatorID == "" {
		return nil
	}
	judge := &EvaluatorJudgeConfig{Type: "llm_judge", When: "checks_pass"}
	if model := strings.TrimSpace(evaluatorModel); model != "" {
		judge.Model = model
	}
	if payload := judgePromptConfigFromConfig(metadata); payload != nil {
		if merged, err := mergeJudgePromptConfig(judge.Config, payload); err == nil {
			judge.Config = merged
		}
	}
	switch evaluatorID {
	case "landing_page_v1", "vue_landing_page_v1":
		return &EvaluatorProfile{
			ProfileID:        "landing_page_v1",
			TaskKind:         "vue_landing_page",
			ArtifactContract: "vue_vite_bundle",
			PreviewAdapter:   "vue_vite",
			Checks: []EvaluatorCheckConfig{
				{ID: "required_files", Type: "artifact_contract", Required: true},
				{ID: "render_smoke", Type: "playwright", When: "checks_pass"},
			},
			Judge:    judge,
			Metadata: metadata,
		}
	default:
		return &EvaluatorProfile{
			ProfileID: profileID,
			TaskKind:  "generic",
			Judge:     judge,
			Metadata:  metadata,
		}
	}
}

func evaluatorProfileMetadataString(metadata map[string]any, path ...string) string {
	var current any = metadata
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	if value, ok := current.(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
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
	metadata, err := templateMetadataOrSynthesized(template)
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

// templateMetadataOrSynthesized parses the template's stored metadata, or —
// for legacy rows installed before frontmatter became mandatory at install
// time and therefore carrying an empty metadata_json — synthesizes a minimal
// valid record from the row itself so training does not dead-end. Non-empty
// but invalid metadata still fails loudly.
func templateMetadataOrSynthesized(template db.AgentTemplate) (agenttemplate.Metadata, error) {
	if strings.TrimSpace(template.MetadataJSON) != "" {
		return agenttemplate.UnmarshalMetadata(template.MetadataJSON)
	}
	// Prefer frontmatter embedded in the content (mirrors the cached-template
	// fallbacks in agent_template.go and train_init_templates.go), so the
	// exported metadata cannot contradict the content's own frontmatter.
	if parsed, err := agenttemplate.ParseTemplateContent(template.Content); err == nil {
		return parsed.Metadata, nil
	}
	name := strings.TrimSpace(template.Name)
	if name == "" {
		name = template.ID
	}
	description := strings.TrimSpace(template.Description)
	if description == "" {
		description = "Agent template " + template.ID
	}
	minimal := agenttemplate.Metadata{
		ID:                   template.ID,
		Name:                 name,
		Description:          description,
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask"},
		RuntimeCompatibility: []string{"codex", "claude"},
		Tags:                 []string{"agent-template"},
		Inputs:               []string{"repo", "task"},
		Outputs:              []string{"response"},
	}
	// Round-trip through the canonical encoder so the synthesized record is
	// validated and normalized exactly like stored metadata.
	encoded, err := agenttemplate.MarshalMetadata(minimal)
	if err != nil {
		return agenttemplate.Metadata{}, fmt.Errorf("synthesize metadata for template %s: %w", template.ID, err)
	}
	return agenttemplate.UnmarshalMetadata(encoded)
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
		tieGroups, err := rankedFeedbackTieGroups(event, ranking)
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
		requiredImprovements, err := rawJSON(event.RequiredImprovementsJSON)
		if err != nil {
			return nil, fmt.Errorf("ranked feedback %s required_improvements_json: %w", event.ID, err)
		}
		output = append(output, RankedFeedbackEvent{
			ID:                   event.ID,
			RunID:                event.RunID,
			ItemID:               event.ItemID,
			Ranking:              ranking,
			TieGroups:            tieGroups,
			Winner:               event.Winner,
			UsefulTraits:         usefulTraits,
			RejectedTraits:       rejectedTraits,
			RequiredImprovements: requiredImprovements,
			Quality:              event.Quality,
			ContinueMode:         event.ContinueMode,
			Promote:              event.Promote,
			Reasoning:            event.Reasoning,
			Reviewer:             event.Reviewer,
			Source:               event.Source,
			SourceURL:            event.SourceURL,
			CreatedAt:            event.CreatedAt,
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

func rankedFeedbackTieGroups(event db.RankedFeedbackEvent, ranking []string) ([][]string, error) {
	if strings.TrimSpace(event.TieGroupsJSON) == "" {
		return nil, nil
	}
	var groups [][]string
	if err := json.Unmarshal([]byte(event.TieGroupsJSON), &groups); err != nil {
		return nil, fmt.Errorf("ranked feedback %s tie_groups_json: %w", event.ID, err)
	}
	flattened := make([]string, 0, len(ranking))
	for _, group := range groups {
		for _, label := range group {
			flattened = append(flattened, strings.TrimSpace(strings.ToLower(label)))
		}
	}
	if len(flattened) != len(ranking) {
		return nil, fmt.Errorf("ranked feedback %s tie_groups_json does not match ranking length", event.ID)
	}
	for index, label := range ranking {
		if flattened[index] != strings.TrimSpace(strings.ToLower(label)) {
			return nil, fmt.Errorf("ranked feedback %s tie_groups_json does not match ranking order", event.ID)
		}
	}
	return groups, nil
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
